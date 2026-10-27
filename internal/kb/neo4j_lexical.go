package kb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"bb-graph-assistant/internal/neo4jstore"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	LexicalEngineSQLite = "sqlite-fts5"
	LexicalEngineNeo4j  = "neo4j-lucene"
)

var luceneIndexSnapshotPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// LuceneCandidate is a ranked Neo4j fulltext result. SnapshotID is returned
// from the node so callers can fail closed if a generation index is misbound.
type LuceneCandidate struct {
	ChunkID    string
	SnapshotID string
	SourceName string
	Score      float64
}

// LuceneCandidateProvider supplies lexical candidates from an immutable KB
// generation. Source filters are applied after the global limited pool.
type LuceneCandidateProvider interface {
	Search(ctx context.Context, query, snapshotID string, limit int, sources []string, includeSupplemental bool) ([]LuceneCandidate, error)
	Close() error
}

type luceneGeneration struct {
	SnapshotID    string
	State         string
	SchemaVersion int64
	CoreCount     int64
	TotalCount    int64
	CoreSHA256    string
	TotalSHA256   string
}

// luceneExecutor keeps provider behavior testable without starting Neo4j.
type luceneExecutor interface {
	ReadGeneration(context.Context, string) (luceneGeneration, error)
	QueryFulltext(context.Context, string, string, int, []string) ([]LuceneCandidate, error)
	Close() error
}

type neo4jLuceneProvider struct{ executor luceneExecutor }

// NewNeo4jLuceneProvider creates a driver using the application's existing
// private Neo4j environment configuration. Driver connection is lazy.
func NewNeo4jLuceneProvider() (LuceneCandidateProvider, error) {
	cfg, err := neo4jstore.ConfigFromEnv()
	if err != nil {
		return nil, errors.New("Neo4j KB credentials are unavailable")
	}
	driver, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.Username, cfg.Password, ""))
	if err != nil {
		return nil, errors.New("Neo4j KB driver could not be initialized")
	}
	return &neo4jLuceneProvider{executor: &driverLuceneExecutor{driver: driver, database: cfg.Database}}, nil
}

func newNeo4jLuceneProviderWithExecutor(executor luceneExecutor) LuceneCandidateProvider {
	return &neo4jLuceneProvider{executor: executor}
}

func (p *neo4jLuceneProvider) Search(ctx context.Context, query, snapshotID string, limit int, sources []string, includeSupplemental bool) ([]LuceneCandidate, error) {
	if p == nil || p.executor == nil {
		return nil, errors.New("Neo4j KB lexical provider is unavailable")
	}
	if !luceneIndexSnapshotPattern.MatchString(snapshotID) || limit < 1 || limit > 600 {
		return nil, errors.New("Neo4j KB lexical request is invalid")
	}
	cleaned := sanitizeRedAmonLuceneQuery(query)
	if cleaned == "" {
		return []LuceneCandidate{}, nil
	}
	generation, err := p.executor.ReadGeneration(ctx, snapshotID)
	if err != nil {
		return nil, errors.New("Neo4j KB generation marker is unavailable")
	}
	if generation.SnapshotID != snapshotID || generation.State != "ready" || generation.SchemaVersion != 1 ||
		generation.CoreCount <= 0 || generation.TotalCount < generation.CoreCount ||
		!luceneIndexSnapshotPattern.MatchString(generation.CoreSHA256) || !luceneIndexSnapshotPattern.MatchString(generation.TotalSHA256) {
		return nil, errors.New("Neo4j KB generation marker does not match the requested snapshot")
	}
	if includeSupplemental && generation.TotalCount == 0 {
		return []LuceneCandidate{}, nil
	}
	indexName := "kb_" + snapshotID
	if includeSupplemental {
		indexName += "_all"
	}
	candidates, err := p.executor.QueryFulltext(ctx, indexName, cleaned, limit, sources)
	if err != nil {
		return nil, errors.New("Neo4j KB Lucene index query failed")
	}
	if len(candidates) > limit {
		return nil, errors.New("Neo4j KB Lucene result exceeds its requested limit")
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !chunkIDPattern.MatchString(candidate.ChunkID) || candidate.SnapshotID != snapshotID || candidate.SourceName == "" || math.IsNaN(candidate.Score) || math.IsInf(candidate.Score, 0) || candidate.Score < 0 {
			return nil, errors.New("Neo4j KB Lucene result has invalid snapshot or score")
		}
		if _, duplicate := seen[candidate.ChunkID]; duplicate {
			return nil, errors.New("Neo4j KB Lucene result contains duplicate chunk identities")
		}
		seen[candidate.ChunkID] = struct{}{}
	}
	return candidates, nil
}

func (p *neo4jLuceneProvider) Close() error {
	if p == nil || p.executor == nil {
		return nil
	}
	return p.executor.Close()
}

// sanitizeRedAmonLuceneQuery intentionally mirrors RedAmon's raw Lucene
// sanitization; it does not quote tokens or insert boolean operators.
func sanitizeRedAmonLuceneQuery(query string) string {
	cleaned := redAmonLuceneSpecialChars.ReplaceAllString(query, " ")
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(cleaned, "&&", " "), "||", " "))
}

var redAmonLuceneSpecialChars = regexp.MustCompile(`[+\-!(){}\[\]^"~*?:\\/]`)

type driverLuceneExecutor struct {
	driver   neo4j.DriverWithContext
	database string
}

func (e *driverLuceneExecutor) ReadGeneration(ctx context.Context, snapshotID string) (luceneGeneration, error) {
	if e == nil || e.driver == nil {
		return luceneGeneration{}, errors.New("Neo4j driver is unavailable")
	}
	session := e.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: e.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	result, err := session.Run(ctx, `MATCH (g:AssistantKBGeneration {snapshot_id:$snapshot_id})
RETURN g.snapshot_id AS snapshot_id,g.state AS state,g.schema_version AS schema_version,
g.core_count AS core_count,g.total_count AS total_count,g.core_sha256 AS core_sha256,
g.total_sha256 AS total_sha256 LIMIT 2`, map[string]any{"snapshot_id": snapshotID})
	if err != nil {
		return luceneGeneration{}, err
	}
	records, err := result.Collect(ctx)
	if err != nil || len(records) != 1 {
		return luceneGeneration{}, errors.New("generation marker must be unique")
	}
	readString := func(key string) string { value, _ := records[0].Get(key); text, _ := value.(string); return text }
	readInt := func(key string) int64 {
		value, _ := records[0].Get(key)
		switch n := value.(type) {
		case int64:
			return n
		case int:
			return int64(n)
		default:
			return -1
		}
	}
	return luceneGeneration{
		SnapshotID: readString("snapshot_id"), State: readString("state"), SchemaVersion: readInt("schema_version"),
		CoreCount: readInt("core_count"), TotalCount: readInt("total_count"),
		CoreSHA256: readString("core_sha256"), TotalSHA256: readString("total_sha256"),
	}, nil
}

func (e *driverLuceneExecutor) QueryFulltext(ctx context.Context, indexName, query string, limit int, sources []string) ([]LuceneCandidate, error) {
	if e == nil || e.driver == nil || !regexp.MustCompile(`^kb_[a-f0-9]{64}(?:_all)?$`).MatchString(indexName) {
		return nil, errors.New("Neo4j fulltext index name is invalid")
	}
	session := e.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: e.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	statement := `CALL db.index.fulltext.queryNodes($index_name,$q_text,{limit:$limit}) YIELD node,score`
	params := map[string]any{"index_name": indexName, "q_text": query, "limit": limit}
	if len(sources) > 0 {
		statement += " WHERE node.source IN $sources"
		params["sources"] = sources
	}
	statement += " RETURN node.chunk_id AS chunk_id,node.snapshot_id AS snapshot_id,node.source AS source,score ORDER BY score DESC"
	result, err := session.Run(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	rows, err := result.Collect(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]LuceneCandidate, 0, len(rows))
	for _, row := range rows {
		idValue, _ := row.Get("chunk_id")
		snapshotValue, _ := row.Get("snapshot_id")
		sourceValue, _ := row.Get("source")
		scoreValue, _ := row.Get("score")
		id, _ := idValue.(string)
		snapshot, _ := snapshotValue.(string)
		source, _ := sourceValue.(string)
		score, ok := neo4jNumber(scoreValue)
		if !ok {
			return nil, fmt.Errorf("Lucene score has unsupported type %T", scoreValue)
		}
		candidates = append(candidates, LuceneCandidate{ChunkID: id, SnapshotID: snapshot, SourceName: source, Score: score})
	}
	return candidates, nil
}

func neo4jNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int64:
		return float64(number), true
	case int:
		return float64(number), true
	default:
		return 0, false
	}
}

func (e *driverLuceneExecutor) Close() error {
	if e == nil || e.driver == nil {
		return nil
	}
	return e.driver.Close(context.Background())
}

func luceneCandidateIDs(candidates []LuceneCandidate) []string {
	ids := make([]string, len(candidates))
	for i, candidate := range candidates {
		ids[i] = candidate.ChunkID
	}
	return ids
}
