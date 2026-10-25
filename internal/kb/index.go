package kb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"bb-graph-assistant/internal/project"
	_ "modernc.org/sqlite"
)

const (
	DefaultSearchTopK          = 5
	MaximumSearchTopK          = 100
	maximumSourceFilters       = 32
	maximumSourceNameBytes     = 128
	maximumSourceIDBytes       = 1024
	maximumSourceReadIDBytes   = 512
	maximumTitleBytes          = 2048
	maximumLocatorBytes        = 2048
	maximumURLBytes            = 2048
	maximumRevisionBytes       = 256
	maximumLicenseBytes        = 256
	maximumAttributionBytes    = 4096
	maximumMetadataJSONBytes   = 16 << 10
	maximumCoverageStateBytes  = 256
	maximumSourceCoverageBytes = 512 << 10
	maximumSourceCoverageRows  = 100
	maximumExcerptRunes        = 1200
	defaultSourceChars         = 12000
	maximumSourceChars         = 30000
	maximumSearchTerms         = 32
	maximumSearchResultBytes   = 4 << 20
	defaultRerankPoolSize      = 30
)

var snapshotDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Index opens an existing Python-compatible FTS index in SQLite read-only mode.
// Snapshot construction and source fetching remain separate operations.
type Index struct {
	db     *sql.DB
	models RetrievalModels
}

// RetrievalModels supplies the pinned E5 query embedding and BGE cross-encoder.
// The Go index remains usable with lexical-only search when models are absent.
type RetrievalModels interface {
	EmbedQuery(context.Context, string) ([]float32, error)
	Rerank(context.Context, string, []string) ([]float64, error)
}

func (i *Index) SetRetrievalModels(models RetrievalModels) {
	if i != nil {
		i.models = models
	}
}

// SearchOptions bounds one lexical query. SourceNames, when provided, are
// exact source-family names and are combined with OR semantics.
type SearchOptions struct {
	TopK        int
	SourceNames []string
}

// SearchResult includes snapshot coverage so a partial corpus is visible to
// the caller alongside its citations.
type SearchResult struct {
	SnapshotID         string `json:"snapshot_id"`
	CoverageState      string `json:"coverage_state"`
	CoverageReportHash string `json:"coverage_report_sha256,omitempty"`
	RetrievalMode      string `json:"retrieval_mode"`
	Hits               []Hit  `json:"hits"`
}

// StatsResult reports bounded counts and source coverage for the active KB
// snapshot. Source coverage is retained as structured reference metadata.
type StatsResult struct {
	SnapshotID                   string           `json:"snapshot_id"`
	SourceFamilies               int64            `json:"source_families"`
	SourceDocuments              int64            `json:"source_documents"`
	Chunks                       int64            `json:"chunks"`
	EmbeddedChunks               int64            `json:"embedded_chunks"`
	EmbeddingModel               *string          `json:"embedding_model,omitempty"`
	VectorReady                  bool             `json:"vector_ready"`
	CoverageState                string           `json:"coverage_state"`
	CoverageReportHash           *string          `json:"coverage_report_sha256"`
	SourceCoverage               []map[string]any `json:"source_coverage"`
	ReferenceMaterialIsUntrusted bool             `json:"reference_material_is_untrusted"`
}

// SourceResult returns one exact KB document with a bounded text prefix and
// its index provenance. The source text remains untrusted reference material.
type SourceResult struct {
	SnapshotID                   string         `json:"snapshot_id"`
	CoverageState                string         `json:"coverage_state"`
	CoverageReportHash           *string        `json:"coverage_report_sha256"`
	SourceName                   string         `json:"source_name"`
	SourceID                     string         `json:"source_id"`
	Title                        string         `json:"title"`
	SourceURL                    *string        `json:"source_url"`
	SourceLocator                *string        `json:"source_locator"`
	SourceRevision               *string        `json:"source_revision"`
	LicenseID                    *string        `json:"license_id"`
	Attribution                  *string        `json:"attribution"`
	ContentSHA256                string         `json:"content_sha256"`
	Content                      string         `json:"content"`
	ContentChars                 int            `json:"content_chars"`
	TotalChars                   int64          `json:"total_chars"`
	Truncated                    bool           `json:"truncated"`
	ContentHashVerified          bool           `json:"content_hash_verified"`
	Metadata                     map[string]any `json:"metadata"`
	ReferenceMaterialIsUntrusted bool           `json:"reference_material_is_untrusted"`
}

// Hit contains a short excerpt and enough provenance to cite and verify the
// matched source chunk. It does not expose full source documents.
type Hit struct {
	ChunkID        string         `json:"chunk_id"`
	SourceName     string         `json:"source_name"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Excerpt        string         `json:"excerpt"`
	SourceLocator  *string        `json:"source_locator"`
	SourceURL      *string        `json:"source_url"`
	SourceRevision *string        `json:"source_revision"`
	LicenseID      *string        `json:"license_id"`
	Attribution    *string        `json:"attribution"`
	SourceSHA256   string         `json:"source_sha256"`
	ChunkSHA256    string         `json:"chunk_sha256"`
	Metadata       map[string]any `json:"metadata"`
	StartChar      int            `json:"start_char"`
	EndChar        int            `json:"end_char"`
	BM25           float64        `json:"bm25"`
	RankScore      float64        `json:"rank_score"`
	VectorScore    *float64       `json:"vector_score,omitempty"`
	RerankScore    *float64       `json:"rerank_score,omitempty"`
}

// OpenIndex validates the path and required Python index schema without
// creating or modifying files. An empty path uses the configured private
// data-root index or BB_ASSISTANT_KB_INDEX.
func OpenIndex(path string) (*Index, error) {
	if path == "" {
		path = strings.TrimSpace(os.Getenv("BB_ASSISTANT_KB_INDEX"))
	}
	if path == "" {
		root, err := project.DataRoot()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(root, "kb", "index.sqlite3")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, errors.New("KB index path is invalid")
	}
	requestedInfo, err := os.Lstat(absolute)
	if err != nil || requestedInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("KB index must be an existing regular file, not a symbolic link")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, errors.New("KB index path could not be resolved")
	}
	if err := rejectSourceCheckoutIndex(resolved); err != nil {
		return nil, err
	}
	info, err := os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("KB index must be an existing regular file, not a symbolic link")
	}
	fileURL := (&url.URL{Scheme: "file", Path: resolved}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		return nil, errors.New("KB index could not be opened read-only")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	index := &Index{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := index.validate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

// Stats returns counts and source coverage from one consistent, read-only
// snapshot. Coverage metadata is capped before it is decoded.
func (i *Index) Stats(ctx context.Context) (StatsResult, error) {
	if i == nil || i.db == nil {
		return StatsResult{}, errors.New("KB index is unavailable")
	}
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StatsResult{}, errors.New("KB read transaction could not start")
	}
	defer tx.Rollback()
	info, err := readSnapshotInfo(ctx, tx)
	if err != nil {
		return StatsResult{}, err
	}
	if info.CoverageState != "unverified" && info.CoverageReportHash == "" {
		return StatsResult{}, errors.New("KB coverage report provenance is missing")
	}
	result := StatsResult{
		SnapshotID: info.SnapshotID, CoverageState: info.CoverageState,
		CoverageReportHash:           optionalString(info.CoverageReportHash),
		SourceCoverage:               []map[string]any{},
		ReferenceMaterialIsUntrusted: true,
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(DISTINCT source_name), count(*) FROM source_documents").Scan(&result.SourceFamilies, &result.SourceDocuments); err != nil {
		return StatsResult{}, errors.New("KB source counts could not be read")
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM chunks").Scan(&result.Chunks); err != nil {
		return StatsResult{}, errors.New("KB chunk count could not be read")
	}
	var embeddingTable int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='chunk_embeddings'").Scan(&embeddingTable); err != nil {
		return StatsResult{}, errors.New("KB embedding table state could not be read")
	}
	if embeddingTable == 1 {
		var vectorModel, vectorSnapshot string
		var dimensions int
		if err := tx.QueryRowContext(ctx, `SELECT
			coalesce(max(CASE WHEN key='vector_model' THEN substr(value,1,256) END),''),
			coalesce(max(CASE WHEN key='vector_snapshot_id' THEN substr(value,1,65) END),''),
			coalesce(max(CASE WHEN key='vector_dimensions' THEN CAST(value AS INTEGER) END),0)
			FROM kb_meta`).Scan(&vectorModel, &vectorSnapshot, &dimensions); err != nil {
			return StatsResult{}, errors.New("KB embedding model metadata could not be read")
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM chunk_embeddings").Scan(&result.EmbeddedChunks); err != nil {
			return StatsResult{}, errors.New("KB embedding count could not be read")
		}
		result.EmbeddingModel = optionalString(vectorModel)
		result.VectorReady = result.EmbeddedChunks > 0 && vectorSnapshot == info.SnapshotID && dimensions == 1024 && vectorModel == "intfloat/e5-large-v2"
	}
	var sourceCoverage []byte
	err = tx.QueryRowContext(ctx,
		"SELECT substr(CAST(value AS BLOB),1,?) FROM kb_meta WHERE key='source_coverage'", maximumSourceCoverageBytes+1,
	).Scan(&sourceCoverage)
	if errors.Is(err, sql.ErrNoRows) && info.CoverageState != "unverified" {
		return StatsResult{}, errors.New("KB source coverage is missing")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return StatsResult{}, errors.New("KB source coverage could not be read")
	}
	if err == nil {
		if len(sourceCoverage) > maximumSourceCoverageBytes {
			return StatsResult{}, errors.New("KB source coverage exceeds the output limit")
		}
		decoder := json.NewDecoder(bytes.NewReader(sourceCoverage))
		decoder.UseNumber()
		if err := decoder.Decode(&result.SourceCoverage); err != nil || result.SourceCoverage == nil {
			return StatsResult{}, errors.New("KB source coverage is invalid")
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return StatsResult{}, errors.New("KB source coverage has trailing data")
		}
		if len(result.SourceCoverage) > maximumSourceCoverageRows {
			return StatsResult{}, errors.New("KB source coverage contains too many entries")
		}
		for _, row := range result.SourceCoverage {
			if row == nil {
				return StatsResult{}, errors.New("KB source coverage has an invalid entry")
			}
			sourceName, nameOK := row["source_name"].(string)
			state, stateOK := row["state"].(string)
			if !nameOK || sourceName == "" || len(sourceName) > maximumSourceNameBytes || !stateOK ||
				(state != "done" && state != "partial" && state != "blocked") {
				return StatsResult{}, errors.New("KB source coverage has an invalid entry")
			}
			if result.CoverageState == "DONE" && state != "done" {
				return StatsResult{}, errors.New("KB source coverage contradicts its DONE state")
			}
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maximumSourceCoverageBytes+4096 {
		return StatsResult{}, errors.New("KB statistics exceed the output limit")
	}
	if err := tx.Commit(); err != nil {
		return StatsResult{}, errors.New("KB read transaction could not finish")
	}
	return result, nil
}

// GetSource reads one exact source document and a bounded prefix of its text.
// A zero maxChars uses the documented default; full content is never required.
func (i *Index) GetSource(ctx context.Context, sourceName, sourceID string, maxChars int) (SourceResult, error) {
	if i == nil || i.db == nil {
		return SourceResult{}, errors.New("KB index is unavailable")
	}
	if sourceName == "" || sourceName != strings.TrimSpace(sourceName) || len(sourceName) > maximumSourceNameBytes ||
		sourceID == "" || sourceID != strings.TrimSpace(sourceID) || len(sourceID) > maximumSourceReadIDBytes {
		return SourceResult{}, errors.New("KB source identity is invalid")
	}
	if maxChars == 0 {
		maxChars = defaultSourceChars
	}
	if maxChars < 256 || maxChars > maximumSourceChars {
		return SourceResult{}, errors.New("KB source character limit must be between 256 and 30000")
	}
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SourceResult{}, errors.New("KB read transaction could not start")
	}
	defer tx.Rollback()
	info, err := readSnapshotInfo(ctx, tx)
	if err != nil {
		return SourceResult{}, err
	}
	result := SourceResult{
		SnapshotID: info.SnapshotID, CoverageState: info.CoverageState,
		CoverageReportHash:           optionalString(info.CoverageReportHash),
		ReferenceMaterialIsUntrusted: true,
	}
	var content string
	var contentLength int64
	var metadataValue sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT substr(source_name,1,129),substr(source_id,1,513),substr(title,1,2049),
		substr(source_url,1,2049),substr(source_locator,1,2049),substr(source_revision,1,257),
		substr(license_id,1,257),substr(attribution,1,4097),
		CASE WHEN length(CAST(metadata_json AS BLOB))<=? THEN metadata_json END,
		substr(content_sha256,1,65),length(text),substr(text,1,?)
		FROM source_documents WHERE source_name=? AND source_id=?`, maximumMetadataJSONBytes, maxChars+1, sourceName, sourceID).Scan(
		&result.SourceName, &result.SourceID, &result.Title,
		&result.SourceURL, &result.SourceLocator, &result.SourceRevision,
		&result.LicenseID, &result.Attribution, &metadataValue,
		&result.ContentSHA256, &contentLength, &content,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceResult{}, errors.New("KB source was not found")
	}
	if err != nil {
		return SourceResult{}, errors.New("KB source could not be read")
	}
	if !snapshotDigestPattern.MatchString(result.ContentSHA256) || contentLength < 0 || !utf8.ValidString(content) ||
		len(result.SourceName) > maximumSourceNameBytes || len(result.SourceID) > maximumSourceReadIDBytes || len(result.Title) > maximumTitleBytes ||
		pointerTooLong(result.SourceURL, maximumURLBytes) || pointerTooLong(result.SourceLocator, maximumLocatorBytes) ||
		pointerTooLong(result.SourceRevision, maximumRevisionBytes) || pointerTooLong(result.LicenseID, maximumLicenseBytes) ||
		pointerTooLong(result.Attribution, maximumAttributionBytes) {
		return SourceResult{}, errors.New("KB source provenance exceeds a validation limit")
	}
	if !metadataValue.Valid || len(metadataValue.String) > maximumMetadataJSONBytes {
		return SourceResult{}, errors.New("KB source metadata exceeds the size limit")
	}
	decoder := json.NewDecoder(strings.NewReader(metadataValue.String))
	decoder.UseNumber()
	if err := decoder.Decode(&result.Metadata); err != nil || result.Metadata == nil {
		return SourceResult{}, errors.New("KB source metadata is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return SourceResult{}, errors.New("KB source metadata has trailing data")
	}
	runes := []rune(content)
	result.Truncated = contentLength > int64(maxChars)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	result.Content = string(runes)
	result.ContentChars = len(runes)
	result.TotalChars = contentLength
	if !result.Truncated {
		if int64(result.ContentChars) != contentLength {
			return SourceResult{}, errors.New("KB source length is inconsistent")
		}
		digest := sha256.Sum256([]byte(result.Content))
		if hex.EncodeToString(digest[:]) != result.ContentSHA256 {
			return SourceResult{}, errors.New("KB source content hash does not match its provenance")
		}
		result.ContentHashVerified = true
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maximumSourceChars*4+maximumMetadataJSONBytes+(16<<10) {
		return SourceResult{}, errors.New("KB source exceeds the output limit")
	}
	if err := tx.Commit(); err != nil {
		return SourceResult{}, errors.New("KB read transaction could not finish")
	}
	return result, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// Search executes a bounded FTS5 query against the compatible local index.
func (i *Index) Search(ctx context.Context, query string, options SearchOptions) (SearchResult, error) {
	if i == nil || i.db == nil {
		return SearchResult{}, errors.New("KB index is unavailable")
	}
	if options.TopK == 0 {
		options.TopK = DefaultSearchTopK
	}
	if options.TopK < 1 || options.TopK > MaximumSearchTopK {
		return SearchResult{}, errors.New("KB top-k must be between 1 and 100")
	}
	if utf8.RuneCountInString(query) > 4096 {
		return SearchResult{}, errors.New("KB query exceeds the 4096-character limit")
	}
	terms := redAmonFullTextTerms(query)
	if len(options.SourceNames) > maximumSourceFilters {
		return SearchResult{}, errors.New("select no more than 32 KB source families")
	}
	sources := make([]string, 0, len(options.SourceNames))
	seenSources := make(map[string]struct{}, len(options.SourceNames))
	for _, name := range options.SourceNames {
		if name == "" || name != strings.TrimSpace(name) || len(name) > maximumSourceNameBytes {
			return SearchResult{}, errors.New("KB source family name is empty or too long")
		}
		if _, exists := seenSources[name]; exists {
			return SearchResult{}, errors.New("KB source family filters must be unique")
		}
		seenSources[name] = struct{}{}
		sources = append(sources, name)
	}
	sort.Strings(sources)

	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SearchResult{}, errors.New("KB read transaction could not start")
	}
	defer tx.Rollback()
	result, err := readSnapshotInfo(ctx, tx)
	if err != nil {
		return SearchResult{}, err
	}
	result.Hits = []Hit{}
	result.RetrievalMode = "fulltext"
	if len(terms) == 0 {
		if err := tx.Commit(); err != nil {
			return SearchResult{}, errors.New("KB read transaction could not finish")
		}
		return result, nil
	}
	match := make([]string, 0, len(terms))
	for _, term := range terms {
		// RedAmon's Neo4j/Lucene full-text query matches complete analyzed
		// terms. Prefix expansion here (for example auth*) returns candidates
		// RedAmon would not, changing the hybrid RRF pool and final results.
		match = append(match, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	statement := `SELECT substr(c.chunk_id,1,65),substr(c.source_name,1,129),
		substr(c.source_id,1,1025),substr(c.title,1,2049),substr(c.text,1,?),
		substr(c.source_locator,1,2049),substr(c.source_url,1,2049),
		substr(c.source_revision,1,257),substr(c.license_id,1,257),substr(c.attribution,1,4097),
		substr(c.source_sha256,1,65),substr(c.chunk_sha256,1,65),
		CASE WHEN length(CAST(c.metadata_json AS BLOB))<=? THEN c.metadata_json END,c.start_char,c.end_char,
		bm25(chunk_search,0.0,0.0,4.0,1.0)
		FROM chunk_search JOIN chunks c ON c.chunk_id=chunk_search.chunk_id
		WHERE chunk_search MATCH ?
		AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content'`
	args := []any{maximumExcerptRunes, maximumMetadataJSONBytes, strings.Join(match, " OR ")}
	candidateLimit := max(options.TopK*6, 30)
	statement += " ORDER BY 16 LIMIT ?"
	args = append(args, candidateLimit)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return SearchResult{}, errors.New("KB full-text search failed")
	}
	fulltextHits, err := scanSearchHits(rows)
	if err != nil {
		return SearchResult{}, err
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, errors.New("KB full-text search could not finish")
	}
	if err := rows.Close(); err != nil {
		return SearchResult{}, errors.New("KB full-text search could not finish")
	}
	// RedAmon applies include_sources after Neo4j's global Lucene top-k query.
	// Applying this predicate inside SQL before LIMIT would fill the pool with
	// lower-ranked source-specific hits that RedAmon excludes.
	fulltextHits = filterHitsBySources(fulltextHits, sources)
	fulltextIDs := hitIDs(fulltextHits)
	result.Hits = fulltextHits
	vectorIDs, vectorScores, vectorMode := i.vectorCandidates(ctx, tx, query, sources, candidateLimit)
	if vectorMode != "" {
		result.RetrievalMode = vectorMode
	}
	if len(vectorIDs) > 0 {
		seen := make(map[string]struct{}, len(result.Hits))
		for _, hit := range result.Hits {
			seen[hit.ChunkID] = struct{}{}
		}
		missingIDs := make([]string, 0, len(vectorIDs))
		for _, id := range vectorIDs {
			if _, exists := seen[id]; !exists {
				missingIDs = append(missingIDs, id)
			}
		}
		if len(missingIDs) > 0 {
			vectorHits, err := readHitsByID(ctx, tx, missingIDs)
			if err != nil {
				return SearchResult{}, err
			}
			result.Hits = append(result.Hits, vectorHits...)
		}
	}
	preRanked := rankHybridHits(result.Hits, vectorIDs, vectorScores, fulltextIDs, nil, candidateLimit)
	if i.models != nil && len(preRanked) > 1 {
		pool := preRanked[:min(len(preRanked), defaultRerankPoolSize)]
		texts, textErr := loadRerankTexts(ctx, tx, pool)
		if textErr == nil {
			scores, err := i.models.Rerank(ctx, query, texts)
			if err == nil && len(scores) == len(pool) {
				rerankScores := make(map[string]float64, len(pool))
				for n, hit := range pool {
					if !math.IsNaN(scores[n]) && !math.IsInf(scores[n], 0) {
						rerankScores[hit.ChunkID] = scores[n]
					}
				}
				if len(rerankScores) > 0 {
					preRanked = rankHybridHits(preRanked, vectorIDs, vectorScores, fulltextIDs, rerankScores, candidateLimit)
				}
			}
		}
	}
	result.Hits = diversifyHits(preRanked, options.TopK)
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maximumSearchResultBytes {
		return SearchResult{}, errors.New("KB search result exceeds the output limit")
	}
	if err := tx.Commit(); err != nil {
		return SearchResult{}, errors.New("KB read transaction could not finish")
	}
	return result, nil
}

func scanSearchHits(rows *sql.Rows) ([]Hit, error) {
	hits := make([]Hit, 0)
	for rows.Next() {
		var hit Hit
		var excerpt string
		var metadataJSON sql.NullString
		if err := rows.Scan(
			&hit.ChunkID, &hit.SourceName, &hit.SourceID, &hit.Title, &excerpt,
			&hit.SourceLocator, &hit.SourceURL, &hit.SourceRevision, &hit.LicenseID, &hit.Attribution,
			&hit.SourceSHA256, &hit.ChunkSHA256, &metadataJSON, &hit.StartChar, &hit.EndChar, &hit.BM25,
		); err != nil {
			return nil, errors.New("KB search result could not be read")
		}
		if !snapshotDigestPattern.MatchString(hit.ChunkID) || !snapshotDigestPattern.MatchString(hit.SourceSHA256) || !snapshotDigestPattern.MatchString(hit.ChunkSHA256) || hit.StartChar < 0 || hit.EndChar < hit.StartChar {
			return nil, errors.New("KB search result has invalid provenance")
		}
		if len(hit.SourceName) > maximumSourceNameBytes || len(hit.SourceID) > maximumSourceIDBytes || len(hit.Title) > maximumTitleBytes ||
			pointerTooLong(hit.SourceLocator, maximumLocatorBytes) || pointerTooLong(hit.SourceURL, maximumURLBytes) ||
			pointerTooLong(hit.SourceRevision, maximumRevisionBytes) || pointerTooLong(hit.LicenseID, maximumLicenseBytes) ||
			pointerTooLong(hit.Attribution, maximumAttributionBytes) {
			return nil, errors.New("KB search provenance exceeds a field size limit")
		}
		if !metadataJSON.Valid || len(metadataJSON.String) > maximumMetadataJSONBytes {
			return nil, errors.New("KB search result metadata exceeds the size limit")
		}
		decoder := json.NewDecoder(strings.NewReader(metadataJSON.String))
		decoder.UseNumber()
		if err := decoder.Decode(&hit.Metadata); err != nil || hit.Metadata == nil {
			return nil, errors.New("KB search result metadata is invalid")
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return nil, errors.New("KB search result metadata has trailing data")
		}
		hit.Excerpt = excerpt
		hits = append(hits, hit)
	}
	return hits, nil
}

func readHitsByID(ctx context.Context, tx *sql.Tx, ids []string) ([]Hit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	statement := `SELECT substr(c.chunk_id,1,65),substr(c.source_name,1,129),
		substr(c.source_id,1,1025),substr(c.title,1,2049),substr(c.text,1,?),
		substr(c.source_locator,1,2049),substr(c.source_url,1,2049),
		substr(c.source_revision,1,257),substr(c.license_id,1,257),substr(c.attribution,1,4097),
		substr(c.source_sha256,1,65),substr(c.chunk_sha256,1,65),
		CASE WHEN length(CAST(c.metadata_json AS BLOB))<=? THEN c.metadata_json END,c.start_char,c.end_char,0.0
		FROM chunks c WHERE c.chunk_id IN (` + placeholders + `)
		AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content'`
	args := []any{maximumExcerptRunes, maximumMetadataJSONBytes}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, errors.New("KB vector candidate metadata could not be read")
	}
	defer rows.Close()
	hits, err := scanSearchHits(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("KB vector candidate metadata could not be read")
	}
	return hits, nil
}

func loadRerankTexts(ctx context.Context, tx *sql.Tx, hits []Hit) ([]string, error) {
	ids := hitIDs(hits)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := tx.QueryContext(ctx,
		"SELECT chunk_id,substr(text,1,1920) FROM chunks WHERE chunk_id IN ("+placeholders+")", stringArgs(ids)...)
	if err != nil {
		return nil, errors.New("KB reranker candidate text could not be read")
	}
	textsByID := make(map[string]string, len(ids))
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			_ = rows.Close()
			return nil, errors.New("KB reranker candidate text could not be read")
		}
		textsByID[id] = text
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, errors.New("KB reranker candidate text could not be read")
	}
	if err := rows.Close(); err != nil {
		return nil, errors.New("KB reranker candidate text could not be read")
	}
	texts := make([]string, len(ids))
	for n, id := range ids {
		text, ok := textsByID[id]
		if !ok {
			return nil, errors.New("KB reranker candidate text is missing")
		}
		texts[n] = text
	}
	return texts, nil
}

func stringArgs(values []string) []any {
	args := make([]any, len(values))
	for n, value := range values {
		args[n] = value
	}
	return args
}

func (i *Index) vectorCandidates(
	ctx context.Context, tx *sql.Tx, query string, sources []string, limit int,
) ([]string, map[string]float64, string) {
	if i.models == nil {
		return nil, nil, "lexical_model_unavailable"
	}
	var tableExists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='chunk_embeddings'").Scan(&tableExists); err != nil || tableExists != 1 {
		return nil, nil, "lexical_only"
	}
	var modelName, vectorSnapshot, snapshotID string
	var dimensions int
	err := tx.QueryRowContext(ctx, `SELECT
		max(CASE WHEN key='vector_model' THEN substr(value,1,256) END),
		max(CASE WHEN key='vector_snapshot_id' THEN substr(value,1,65) END),
		max(CASE WHEN key='snapshot_id' THEN substr(value,1,65) END),
		max(CASE WHEN key='vector_dimensions' THEN CAST(value AS INTEGER) END) FROM kb_meta`).
		Scan(&modelName, &vectorSnapshot, &snapshotID, &dimensions)
	if err != nil || modelName != "intfloat/e5-large-v2" || vectorSnapshot != snapshotID || dimensions != 1024 {
		return nil, nil, "lexical_only"
	}
	queryVector, err := i.models.EmbedQuery(ctx, query)
	if err != nil || len(queryVector) != dimensions || len(queryVector) == 0 {
		return nil, nil, "lexical_fallback"
	}
	queryNorm := 0.0
	for _, value := range queryVector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, nil, "lexical_fallback"
		}
		queryNorm += float64(value) * float64(value)
	}
	queryNorm = math.Sqrt(queryNorm)
	if queryNorm == 0 {
		return nil, nil, "lexical_fallback"
	}
	statement := `SELECT e.chunk_id,e.vector,c.source_name FROM chunk_embeddings e
		JOIN chunks c ON c.chunk_id=e.chunk_id
		WHERE e.model_name=? AND e.dimensions=?
		AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content'`
	args := []any{modelName, dimensions}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, nil, "lexical_fallback"
	}
	type scored struct {
		id     string
		source string
		score  float64
	}
	all := make([]scored, 0)
	for rows.Next() {
		var id, source string
		var blob []byte
		if err := rows.Scan(&id, &blob, &source); err != nil || len(blob) != dimensions*4 {
			_ = rows.Close()
			return nil, nil, "lexical_fallback"
		}
		dot, norm := 0.0, 0.0
		for n, queryValue := range queryVector {
			value := math.Float32frombits(binary.LittleEndian.Uint32(blob[n*4 : n*4+4]))
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				_ = rows.Close()
				return nil, nil, "lexical_fallback"
			}
			dot += float64(queryValue) * float64(value)
			norm += float64(value) * float64(value)
		}
		if norm > 0 {
			all = append(all, scored{id: id, source: source, score: dot / (queryNorm * math.Sqrt(norm))})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, "lexical_fallback"
	}
	if err := rows.Close(); err != nil {
		return nil, nil, "lexical_fallback"
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].score > all[b].score })
	if len(all) > limit {
		all = all[:limit]
	}
	// RedAmon asks FAISS for one global candidate pool, then applies the
	// include_sources filter while fetching the fused candidates from Neo4j.
	// Filtering before top-k changes which vectors can enter RRF and can fill
	// the pool with lower-ranked source-specific rows that RedAmon would omit.
	if len(sources) > 0 {
		allowed := make(map[string]struct{}, len(sources))
		for _, source := range sources {
			allowed[source] = struct{}{}
		}
		filtered := all[:0]
		for _, item := range all {
			if _, ok := allowed[item.source]; ok {
				filtered = append(filtered, item)
			}
		}
		all = filtered
	}
	ids, scores := make([]string, 0, len(all)), make(map[string]float64, len(all))
	for _, item := range all {
		ids = append(ids, item.id)
		scores[item.id] = item.score
	}
	return ids, scores, "hybrid"
}

func hitIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for n, hit := range hits {
		ids[n] = hit.ChunkID
	}
	return ids
}

func filterHitsBySources(hits []Hit, sources []string) []Hit {
	if len(sources) == 0 {
		return hits
	}
	allowed := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		allowed[source] = struct{}{}
	}
	filtered := make([]Hit, 0, len(hits))
	for _, hit := range hits {
		if _, ok := allowed[hit.SourceName]; ok {
			filtered = append(filtered, hit)
		}
	}
	return filtered
}

func rankHybridHits(hits []Hit, vectorIDs []string, vectorScores map[string]float64, fulltextIDs []string, rerankScores map[string]float64, limit int) []Hit {
	if limit < 1 || len(hits) == 0 {
		return []Hit{}
	}
	rrf := make(map[string]float64, len(vectorIDs)+len(fulltextIDs))
	for _, list := range [][]string{vectorIDs, fulltextIDs} {
		seen := make(map[string]struct{}, len(list))
		for rank, id := range list {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			rrf[id] += 1 / float64(60+rank+1)
		}
	}
	for n := range hits {
		hit := &hits[n]
		base, exists := rrf[hit.ChunkID]
		if !exists {
			continue
		}
		if score, ok := vectorScores[hit.ChunkID]; ok {
			value := score
			hit.VectorScore = &value
		}
		boost := redAmonSourceBoosts[hit.SourceName]
		if boost == 0 {
			boost = 1
		}
		if score, ok := rerankScores[hit.ChunkID]; ok {
			value := score
			hit.RerankScore = &value
			base = 1 / (1 + math.Exp(-max(-60, min(60, score))))
		}
		hit.RankScore = base * boost
	}
	// Match RedAmon: reranked top-pool items sort by sigmoid(logit)*source boost,
	// then the untouched tail remains in its original RRF order.
	if len(rerankScores) > 0 {
		scored := make([]Hit, 0, len(hits))
		tail := make([]Hit, 0, len(hits))
		for _, hit := range hits {
			if _, reranked := rerankScores[hit.ChunkID]; reranked {
				scored = append(scored, hit)
			} else {
				tail = append(tail, hit)
			}
		}
		sort.SliceStable(scored, func(a, b int) bool { return scored[a].RankScore > scored[b].RankScore })
		sort.SliceStable(tail, func(a, b int) bool { return tail[a].RankScore > tail[b].RankScore })
		hits = append(scored, tail...)
	} else {
		sort.SliceStable(hits, func(a, b int) bool { return hits[a].RankScore > hits[b].RankScore })
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func diversifyHits(hits []Hit, topK int) []Hit {
	if topK < 1 || len(hits) == 0 {
		return []Hit{}
	}
	if len(hits) <= topK {
		return hits
	}
	selected := []Hit{hits[0]}
	remaining := append([]Hit(nil), hits[1:]...)
	for len(selected) < topK && len(remaining) > 0 {
		bestIndex, bestScore := 0, math.Inf(-1)
		for index, candidate := range remaining {
			maxSimilarity := 0.0
			for _, chosen := range selected {
				maxSimilarity = math.Max(maxSimilarity, kbChunkSimilarity(candidate, chosen))
			}
			score := 0.65*candidate.RankScore - 0.35*maxSimilarity
			if score > bestScore {
				bestIndex, bestScore = index, score
			}
		}
		selected = append(selected, remaining[bestIndex])
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	return selected
}

var redAmonSourceBoosts = map[string]float64{
	"tool_docs": 1.20, "gtfobins": 1.15, "lolbas": 1.15,
	"owasp": 1.05, "nuclei": 1.00, "nvd": 0.90, "exploitdb": 0.85,
}

func rankFullTextHits(hits []Hit, topK int) []Hit {
	if topK < 1 || len(hits) == 0 {
		return []Hit{}
	}
	for rank := range hits {
		boost := redAmonSourceBoosts[hits[rank].SourceName]
		if boost == 0 {
			boost = 1
		}
		hits[rank].RankScore = boost / float64(61+rank)
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].RankScore > hits[b].RankScore })
	if len(hits) <= topK {
		return hits
	}
	selected := make([]Hit, 0, topK)
	selected = append(selected, hits[0])
	remaining := append([]Hit(nil), hits[1:]...)
	for len(selected) < topK && len(remaining) > 0 {
		bestIndex := 0
		bestScore := math.Inf(-1)
		for index, candidate := range remaining {
			maxSimilarity := 0.0
			for _, chosen := range selected {
				if similarity := kbChunkSimilarity(candidate, chosen); similarity > maxSimilarity {
					maxSimilarity = similarity
				}
			}
			mmr := 0.65*candidate.RankScore - 0.35*maxSimilarity
			if mmr > bestScore {
				bestIndex, bestScore = index, mmr
			}
		}
		selected = append(selected, remaining[bestIndex])
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	return selected
}

func kbChunkSimilarity(a, b Hit) float64 {
	score := 0.0
	if a.SourceName != "" && a.SourceName == b.SourceName {
		score += 0.4
	}
	tokensA, tokensB := strings.Fields(strings.ToLower(a.Title)), strings.Fields(strings.ToLower(b.Title))
	setA, setB := make(map[string]struct{}, len(tokensA)), make(map[string]struct{}, len(tokensB))
	for _, token := range tokensA {
		setA[token] = struct{}{}
	}
	for _, token := range tokensB {
		setB[token] = struct{}{}
	}
	if len(setA)+len(setB) > 0 {
		intersection := 0
		for token := range setA {
			if _, exists := setB[token]; exists {
				intersection++
			}
		}
		union := len(setA) + len(setB) - intersection
		if union > 0 {
			score += 0.6 * float64(intersection) / float64(union)
		}
	}
	return math.Min(score, 1.0)
}

func (i *Index) validate(ctx context.Context) error {
	for _, table := range []string{"kb_meta", "source_documents", "chunks", "chunk_search"} {
		var found int
		if err := i.db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table,
		).Scan(&found); err != nil || found != 1 {
			return errors.New("KB index is missing a required table")
		}
	}
	var snapshotID, coverage string
	err := i.db.QueryRowContext(ctx,
		"SELECT max(CASE WHEN key='snapshot_id' THEN substr(value,1,65) END), max(CASE WHEN key='coverage_state' THEN substr(value,1,257) END) FROM kb_meta",
	).Scan(&snapshotID, &coverage)
	if err != nil || !snapshotDigestPattern.MatchString(snapshotID) || len(coverage) > maximumCoverageStateBytes || (coverage != "DONE" && coverage != "PARTIAL" && coverage != "BLOCKED" && coverage != "unverified") {
		return errors.New("KB index is empty or has unsupported snapshot metadata")
	}
	var count int
	if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM chunks").Scan(&count); err != nil || count < 1 {
		return errors.New("KB index contains no searchable chunks")
	}
	return nil
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSnapshotInfo(ctx context.Context, queryer rowQueryer) (SearchResult, error) {
	result := SearchResult{}
	err := queryer.QueryRowContext(ctx,
		"SELECT max(CASE WHEN key='snapshot_id' THEN substr(value,1,65) END), max(CASE WHEN key='coverage_state' THEN substr(value,1,257) END), max(CASE WHEN key='coverage_report_sha256' THEN substr(value,1,65) END) FROM kb_meta",
	).Scan(&result.SnapshotID, &result.CoverageState, &result.CoverageReportHash)
	if err != nil || !snapshotDigestPattern.MatchString(result.SnapshotID) || len(result.CoverageState) > maximumCoverageStateBytes ||
		(result.CoverageState != "DONE" && result.CoverageState != "PARTIAL" && result.CoverageState != "BLOCKED" && result.CoverageState != "unverified") {
		return SearchResult{}, errors.New("KB snapshot metadata could not be read")
	}
	if result.CoverageReportHash != "" && !snapshotDigestPattern.MatchString(result.CoverageReportHash) {
		return SearchResult{}, errors.New("KB coverage report provenance is invalid")
	}
	return result, nil
}

func queryTerms(input string) []string {
	terms := make([]string, 0, maximumSearchTerms)
	current := make([]rune, 0, 64)
	flush := func() {
		if len(current) >= 2 && len(terms) < maximumSearchTerms {
			terms = append(terms, string(current))
		}
		current = current[:0]
	}
	for _, r := range input {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			current = append(current, r)
			if len(current) == 64 {
				flush()
			}
		} else {
			flush()
			if len(terms) == maximumSearchTerms {
				break
			}
		}
	}
	if len(terms) < maximumSearchTerms {
		flush()
	}
	return terms
}

// redAmonFullTextTerms mirrors RedAmon's Lucene query sanitization before
// producing exact FTS5 terms. In particular, punctuation in identifiers such
// as CVE-2024-12345 must separate terms rather than turn the whole value into
// a phrase that Lucene would not require.
func redAmonFullTextTerms(input string) []string {
	var sanitized strings.Builder
	for _, character := range input {
		switch character {
		case '+', '-', '!', '(', ')', '{', '}', '[', ']', '^', '"', '~', '*', '?', ':', '\\', '/':
			sanitized.WriteByte(' ')
		default:
			sanitized.WriteRune(character)
		}
	}
	query := strings.ReplaceAll(sanitized.String(), "&&", " ")
	query = strings.ReplaceAll(query, "||", " ")
	return queryTerms(query)
}

func pointerTooLong(value *string, maximum int) bool {
	return value != nil && len(*value) > maximum
}

func rejectSourceCheckoutIndex(path string) error {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return errors.New("could not validate KB index location")
	}
	root, err := filepath.Abs(filepath.Dir(filepath.Dir(filepath.Dir(sourceFile))))
	if err != nil {
		return errors.New("could not validate KB index location")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return errors.New("could not validate KB index location")
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return errors.New("KB index must remain outside the source checkout")
	}
	return nil
}
