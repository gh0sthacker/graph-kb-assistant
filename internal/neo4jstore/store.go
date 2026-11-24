// Package neo4jstore persists the sanitized BehaviorGraph projection in one
// local Neo4j Community database. Project isolation is enforced on every node
// lookup and relationship write through the mandatory project_key property.
package neo4jstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	// The full-data occurrence projection measured 227,626 records. Keep
	// bounded headroom for that verified local shape; the 256 MiB aggregate
	// payload limit remains the primary size bound.
	maxSnapshotRecords          = 300_000
	maxSnapshotBytes            = 256 << 20
	writeBatchSize              = 500
	rebuildDeleteBatchSize      = 5_000
	maxSignatureObservations    = behaviorgraph.MaxVariantObservations
	maxFieldValueTraversalPaths = behaviorgraph.MaxFieldValueTraversalPaths
)

var projectKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var signatureDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var neo4jErrorCodePattern = regexp.MustCompile(`^Neo[.](ClientError|TransientError|DatabaseError)[.][A-Za-z0-9_.]+$`)

var requiredConstraintNames = []string{
	"graph_record_identity",
	"graph_import_identity",
	"graph_project_identity",
}
var requiredIndexNames = []string{
	"graph_record_project_type",
	"graph_record_project_kind",
	"graph_record_project_card_endpoint",
	"graph_record_project_card_capture",
	"graph_record_project_request_signature",
	"graph_record_project_response_signature",
	"graph_record_project_field_equality",
}

var ErrSignatureIndexUnavailable = errors.New("Neo4j project signature index is unavailable")
var ErrFieldValueIndexUnavailable = errors.New("Neo4j project field-value index is unavailable")
var ErrProjectRebuildInProgress = errors.New("Neo4j project graph rebuild is in progress")

// Config contains connection settings only. Passwords are never included in
// formatted errors or status reports.
type Config struct {
	URI      string
	Username string
	Password string
	Database string
}

// ConfigFromEnv reads the private local Neo4j connection settings.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		URI:      strings.TrimSpace(os.Getenv("BB_ASSISTANT_NEO4J_URI")),
		Username: strings.TrimSpace(os.Getenv("BB_ASSISTANT_NEO4J_USERNAME")),
		Password: os.Getenv("BB_ASSISTANT_NEO4J_PASSWORD"),
		Database: strings.TrimSpace(os.Getenv("BB_ASSISTANT_NEO4J_DATABASE")),
	}
	if cfg.URI == "" {
		cfg.URI = "bolt://127.0.0.1:7687"
	}
	if cfg.Username == "" {
		cfg.Username = "neo4j"
	}
	if cfg.Database == "" {
		cfg.Database = "neo4j"
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ConfigFromRequiredEnvPrefix reads an explicitly configured Neo4j endpoint
// from a caller-owned environment namespace. It intentionally applies no
// localhost or credential defaults so an optional store cannot silently bind
// to the application's primary graph database.
func ConfigFromRequiredEnvPrefix(prefix string) (Config, error) {
	if !regexp.MustCompile(`^BB_ASSISTANT_[A-Z0-9_]+$`).MatchString(prefix) {
		return Config{}, errors.New("Neo4j environment prefix is invalid")
	}
	cfg := Config{
		URI:      strings.TrimSpace(os.Getenv(prefix + "_URI")),
		Username: strings.TrimSpace(os.Getenv(prefix + "_USERNAME")),
		Password: os.Getenv(prefix + "_PASSWORD"),
		Database: strings.TrimSpace(os.Getenv(prefix + "_DATABASE")),
	}
	if cfg.URI == "" || cfg.Username == "" || cfg.Password == "" || cfg.Database == "" {
		return Config{}, errors.New("Neo4j endpoint settings are required")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	parsed, err := url.Parse(c.URI)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Neo4j URI must be a host-only bolt URI without embedded credentials")
	}
	switch parsed.Scheme {
	case "bolt", "bolt+s", "bolt+ssc", "neo4j", "neo4j+s", "neo4j+ssc":
	default:
		return errors.New("Neo4j URI scheme is unsupported")
	}
	if c.Username == "" || len(c.Username) > 128 || strings.ContainsAny(c.Username, "\r\n\x00") ||
		c.Password == "" || len(c.Password) > 4096 || strings.ContainsAny(c.Password, "\r\n\x00") {
		return errors.New("Neo4j credentials are missing or invalid")
	}
	if c.Database == "" || len(c.Database) > 63 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(c.Database) {
		return errors.New("Neo4j database name is invalid")
	}
	return nil
}

type Store struct {
	driver   neo4j.DriverWithContext
	database string
}

// New opens and verifies a Neo4j connection without exposing connection or
// authentication details in returned errors.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	driver, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.Username, cfg.Password, ""))
	if err != nil {
		return nil, fmt.Errorf("Neo4j driver could not be initialized: %s", safeDriverError(err, cfg))
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		_ = driver.Close(ctx)
		return nil, fmt.Errorf("Neo4j is unavailable or authentication failed: %s", safeDriverError(err, cfg))
	}
	return &Store{driver: driver, database: cfg.Database}, nil
}

func safeDriverError(err error, cfg Config) string {
	message := err.Error()
	for _, secret := range []string{cfg.Password, cfg.Username, cfg.URI} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func (s *Store) Close(ctx context.Context) error {
	if s == nil || s.driver == nil {
		return nil
	}
	return s.driver.Close(ctx)
}

// EnsureSchema creates only the identity constraint and lookup indexes needed
// by the projection. Community edition stores all projects in the same `neo4j`
// database, so callers must scope every operation by project_key.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if s == nil || s.driver == nil {
		return errors.New("Neo4j store is unavailable")
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database})
	defer session.Close(ctx)
	for _, query := range []string{
		"CREATE CONSTRAINT graph_record_identity IF NOT EXISTS FOR (n:GraphRecord) REQUIRE (n.project_key, n.record_id) IS UNIQUE",
		"CREATE CONSTRAINT graph_import_identity IF NOT EXISTS FOR (i:GraphImport) REQUIRE (i.project_key, i.bundle_id) IS UNIQUE",
		"CREATE CONSTRAINT graph_project_identity IF NOT EXISTS FOR (p:GraphProject) REQUIRE p.project_key IS UNIQUE",
		"CREATE INDEX graph_record_project_type IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.record_type)",
		"CREATE INDEX graph_record_project_kind IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.kind)",
		"CREATE INDEX graph_record_project_card_endpoint IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.card_endpoint_id)",
		"CREATE INDEX graph_record_project_card_capture IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.card_capture_id)",
		"CREATE INDEX graph_record_project_request_signature IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.kind, n.request_signature)",
		"CREATE INDEX graph_record_project_response_signature IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.kind, n.response_signature)",
		"CREATE INDEX graph_record_project_field_equality IF NOT EXISTS FOR (n:GraphRecord) ON (n.project_key, n.kind, n.field_equality_signature)",
	} {
		result, err := session.Run(ctx, query, nil)
		if err != nil {
			return errors.New("Neo4j graph schema could not be initialized")
		}
		if _, err := result.Consume(ctx); err != nil {
			return errors.New("Neo4j graph schema could not be initialized")
		}
	}
	return nil
}

// CheckSchema verifies the projection's required schema without modifying it.
func (s *Store) CheckSchema(ctx context.Context) error {
	if s == nil || s.driver == nil {
		return errors.New("Neo4j store is unavailable")
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	return checkSchemaWithQueries(ctx, func(ctx context.Context, query string) ([]string, error) {
		result, err := session.Run(ctx, query, nil)
		if err != nil {
			return nil, err
		}
		records, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(records))
		for _, record := range records {
			value, ok := record.Get("name")
			if !ok {
				return nil, errors.New("Neo4j schema catalog omitted a name")
			}
			name, ok := value.(string)
			if !ok || name == "" {
				return nil, errors.New("Neo4j schema catalog returned an invalid name")
			}
			names = append(names, name)
		}
		return names, nil
	})
}

func checkSchemaWithQueries(ctx context.Context, query func(context.Context, string) ([]string, error)) error {
	if ctx == nil || query == nil {
		return errors.New("Neo4j schema check is unavailable")
	}
	for _, check := range []struct {
		statement string
		required  []string
	}{
		{statement: "SHOW CONSTRAINTS YIELD name RETURN name", required: requiredConstraintNames},
		{statement: "SHOW INDEXES YIELD name RETURN name", required: requiredIndexNames},
	} {
		names, err := query(ctx, check.statement)
		if err != nil {
			return errors.New("Neo4j graph schema could not be inspected")
		}
		present := make(map[string]struct{}, len(names))
		for _, name := range names {
			present[name] = struct{}{}
		}
		var missing []string
		for _, name := range check.required {
			if _, ok := present[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("Neo4j graph schema is incomplete; missing: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

// DeleteProject removes every graph projection row for one exact project key.
// Callers must hold the project's exclusive maintenance lease and unregister
// it only after this succeeds. Deletes are committed in bounded batches so a
// large capture history does not require one unbounded transaction.
func (s *Store) DeleteProject(ctx context.Context, projectKey string) error {
	if s == nil || s.driver == nil {
		return errors.New("Neo4j store is unavailable")
	}
	if !projectKeyPattern.MatchString(projectKey) {
		return errors.New("Neo4j project key is invalid")
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database})
	defer session.Close(ctx)
	deleteBatches := func(query, errorMessage string) error {
		for {
			result, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
				result, runErr := tx.Run(ctx, query, map[string]any{
					"project_key": projectKey, "limit": writeBatchSize,
				})
				if runErr != nil {
					return nil, errors.New(errorMessage)
				}
				if !result.Next(ctx) {
					if result.Err() != nil {
						return nil, errors.New(errorMessage)
					}
					return nil, errors.New(errorMessage)
				}
				deleted, valueOK := result.Record().Get("deleted")
				if !valueOK {
					return nil, errors.New(errorMessage)
				}
				if _, consumeErr := result.Consume(ctx); consumeErr != nil {
					return nil, errors.New(errorMessage)
				}
				count, ok := deleted.(int64)
				if !ok || count < 0 || count > writeBatchSize {
					return nil, errors.New(errorMessage)
				}
				return count, nil
			})
			if err != nil {
				return err
			}
			deleted, ok := result.(int64)
			if !ok {
				return errors.New(errorMessage)
			}
			if deleted == 0 {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	if err := deleteBatches(`
		MATCH (n:GraphRecord {project_key: $project_key})
		WITH n LIMIT $limit
		DETACH DELETE n
		RETURN count(n) AS deleted`, "Neo4j project graph could not be purged"); err != nil {
		return err
	}
	if err := deleteBatches(`
		MATCH (n:GraphImport {project_key: $project_key})
		WITH n LIMIT $limit
		DELETE n
		RETURN count(n) AS deleted`, "Neo4j project import markers could not be purged"); err != nil {
		return err
	}
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, runErr := tx.Run(ctx, "MATCH (n:GraphProject {project_key: $project_key}) DELETE n", map[string]any{"project_key": projectKey})
		if runErr != nil {
			return nil, errors.New("Neo4j project metadata could not be purged")
		}
		if _, runErr = result.Consume(ctx); runErr != nil {
			return nil, errors.New("Neo4j project metadata could not be purged")
		}
		return nil, nil
	})
	return err
}

type preparedRecord struct {
	ID                     string
	Type                   string
	Payload                string
	PayloadHash            string
	Kind                   string
	RequestSignature       string
	ResponseSignature      string
	FieldEqualitySignature string
	Predicate              string
	CardEndpointID         string
	CardCaptureID          string
}

type preparedRef struct {
	OwnerID  string
	SourceID string
}

type preparedAssertion struct {
	ID        string
	SubjectID string
	ObjectID  string
}

type preparedSnapshot struct {
	Records       []preparedRecord
	Refs          []preparedRef
	Assertions    []preparedAssertion
	Digest        string
	SnapshotBytes int
}

// ReplaceProjectSnapshot atomically replaces exactly one project's graph
// projection. The SQLite BehaviorGraph remains the rollback source until the
// caller verifies parity. Other project namespaces are never matched/deleted.
func (s *Store) ReplaceProjectSnapshot(ctx context.Context, projectKey string, snapshot behaviorgraph.Record) (map[string]int, error) {
	if s == nil || s.driver == nil {
		return nil, errors.New("Neo4j store is unavailable")
	}
	prepared, err := prepareSnapshot(projectKey, snapshot)
	if err != nil {
		return nil, err
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database})
	defer session.Close(ctx)
	_, err = session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if err := lockProjectForWrite(ctx, tx, projectKey); err != nil {
			return nil, err
		}
		if err := requireProjectNotRebuilding(ctx, tx, projectKey); err != nil {
			return nil, err
		}
		deleteResult, runErr := tx.Run(ctx,
			"MATCH (n:GraphRecord {project_key: $project_key}) DETACH DELETE n",
			map[string]any{"project_key": projectKey},
		)
		if runErr != nil {
			return nil, errors.New("selected Neo4j project could not be cleared for snapshot replacement")
		}
		if _, runErr = deleteResult.Consume(ctx); runErr != nil {
			return nil, errors.New("selected Neo4j project could not be cleared for snapshot replacement")
		}
		for start := 0; start < len(prepared.Records); start += writeBatchSize {
			end := min(start+writeBatchSize, len(prepared.Records))
			result, runErr := tx.Run(ctx, `
				UNWIND $rows AS row
				MERGE (n:GraphRecord {project_key: $project_key, record_id: row.record_id})
				SET n.record_type = row.record_type,
				    n.payload_json = row.payload_json,
				    n.payload_sha256 = row.payload_sha256,
				    n.kind = row.kind,
				    n.request_signature = row.request_signature,
				    n.response_signature = row.response_signature,
				    n.field_equality_signature = row.field_equality_signature,
				    n.predicate = row.predicate,
				    n.card_endpoint_id = row.card_endpoint_id,
				    n.card_capture_id = row.card_capture_id`,
				map[string]any{"project_key": projectKey, "rows": recordRows(prepared.Records[start:end])},
			)
			if runErr != nil {
				return nil, fmt.Errorf("Neo4j graph records batch %d could not be written (%s)", start/writeBatchSize, safeNeo4jErrorCode(runErr))
			}
			if _, runErr = result.Consume(ctx); runErr != nil {
				return nil, fmt.Errorf("Neo4j graph records batch %d could not be written (%s)", start/writeBatchSize, safeNeo4jErrorCode(runErr))
			}
		}
		for start := 0; start < len(prepared.Refs); start += writeBatchSize {
			end := min(start+writeBatchSize, len(prepared.Refs))
			result, runErr := tx.Run(ctx, `
				UNWIND $rows AS row
				MATCH (owner:GraphRecord {project_key: $project_key, record_id: row.owner_id})
				MATCH (source:GraphRecord {project_key: $project_key, record_id: row.source_id, record_type: 'source'})
				MERGE (owner)-[r:EVIDENCED_BY {project_key: $project_key}]->(source)`,
				map[string]any{"project_key": projectKey, "rows": refRows(prepared.Refs[start:end])},
			)
			if runErr != nil {
				return nil, errors.New("Neo4j graph provenance could not be written")
			}
			if _, runErr = result.Consume(ctx); runErr != nil {
				return nil, errors.New("Neo4j graph provenance could not be written")
			}
		}
		for start := 0; start < len(prepared.Assertions); start += writeBatchSize {
			end := min(start+writeBatchSize, len(prepared.Assertions))
			result, runErr := tx.Run(ctx, `
				UNWIND $rows AS row
				MATCH (assertion:GraphRecord {project_key: $project_key, record_id: row.record_id, record_type: 'assertion'})
				MATCH (subject:GraphRecord {project_key: $project_key, record_id: row.subject_id})
				MATCH (object:GraphRecord {project_key: $project_key, record_id: row.object_id})
				MERGE (assertion)-[s:SUBJECT {project_key: $project_key}]->(subject)
				MERGE (assertion)-[o:OBJECT {project_key: $project_key}]->(object)`,
				map[string]any{"project_key": projectKey, "rows": assertionRows(prepared.Assertions[start:end])},
			)
			if runErr != nil {
				return nil, errors.New("Neo4j assertion relationships could not be written")
			}
			if _, runErr = result.Consume(ctx); runErr != nil {
				return nil, errors.New("Neo4j assertion relationships could not be written")
			}
		}
		meta, runErr := tx.Run(ctx, `
			MERGE (m:GraphProject {project_key: $project_key})
			WITH m, coalesce(m.generation, 0) + 1 AS next_generation
			SET m.snapshot_sha256 = $snapshot_sha256,
			    m.generation = next_generation,
			    m.schema_version = 2,
			    m.signature_index_version = 1,
			    m.signature_index_generation = next_generation,
			    m.field_value_index_version = 1,
			    m.field_value_index_generation = next_generation,
			    m.projection_state = 'snapshot',
			    m.record_count = $record_count,
			    m.reference_count = $reference_count,
			    m.assertion_count = $assertion_count
			RETURN m.project_key AS project_key`,
			map[string]any{
				"project_key": projectKey, "snapshot_sha256": prepared.Digest,
				"record_count": len(prepared.Records), "reference_count": len(prepared.Refs),
				"assertion_count": len(prepared.Assertions),
			},
		)
		if runErr != nil || !meta.Next(ctx) {
			return nil, errors.New("Neo4j graph snapshot metadata could not be committed")
		}
		if _, runErr = meta.Consume(ctx); runErr != nil {
			return nil, errors.New("Neo4j graph snapshot metadata could not be committed")
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]int{
		"records": len(prepared.Records), "references": len(prepared.Refs),
		"assertions": len(prepared.Assertions), "snapshot_bytes": prepared.SnapshotBytes,
	}, nil
}

// RebuildProjectSnapshot replaces one project's projection using short,
// replayable transactions. The project remains marked "rebuilding" until a
// full read-back digest matches the validated source snapshot; graph readers
// fail closed during that maintenance window. The caller must hold the same
// cross-process project maintenance lock used by intake and projection jobs.
func (s *Store) RebuildProjectSnapshot(ctx context.Context, projectKey string, snapshot behaviorgraph.Record) (map[string]int, error) {
	if s == nil || s.driver == nil {
		return nil, errors.New("Neo4j store is unavailable")
	}
	prepared, err := prepareSnapshot(projectKey, snapshot)
	if err != nil {
		return nil, err
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	var tokenBytes [24]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, errors.New("Neo4j graph rebuild token could not be created")
	}
	token := hex.EncodeToString(tokenBytes[:])
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database})
	defer session.Close(ctx)

	var baseGeneration int64
	_, err = session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if err := lockProjectForWrite(ctx, tx, projectKey); err != nil {
			return nil, err
		}
		result, runErr := tx.Run(ctx, `
			MERGE (m:GraphProject {project_key: $project_key})
			WITH m, CASE WHEN m.projection_state = 'rebuilding'
				THEN coalesce(m.rebuild_base_generation, m.generation, 0)
				ELSE coalesce(m.generation, 0) END AS base_generation
			SET m.projection_state = 'rebuilding',
			    m.rebuild_token = $rebuild_token,
			    m.rebuild_base_generation = base_generation,
			    m.rebuild_target_sha256 = $snapshot_sha256,
			    m.signature_index_generation = null,
			    m.field_value_index_generation = null
			RETURN base_generation`,
			map[string]any{"project_key": projectKey, "rebuild_token": token, "snapshot_sha256": prepared.Digest},
		)
		if runErr != nil || !result.Next(ctx) {
			return nil, errors.New("Neo4j project rebuild could not be started")
		}
		value, _ := result.Record().Get("base_generation")
		generation, ok := value.(int64)
		if _, runErr = result.Consume(ctx); runErr != nil || !ok || generation < 0 {
			return nil, errors.New("Neo4j project rebuild metadata is invalid")
		}
		baseGeneration = generation
		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	for {
		deleted := int64(0)
		err := executeProjectRebuildWrite(ctx, session, projectKey, token, func(tx neo4j.ManagedTransaction) error {
			result, runErr := tx.Run(ctx, `
				MATCH (n:GraphRecord {project_key: $project_key})
				WITH n LIMIT $limit
				DETACH DELETE n
				RETURN count(n) AS matched_count`,
				map[string]any{"project_key": projectKey, "limit": rebuildDeleteBatchSize},
			)
			if runErr != nil {
				return errors.New("Neo4j graph rebuild cleanup failed")
			}
			count, countErr := resultCount(ctx, result)
			if countErr != nil {
				return errors.New("Neo4j graph rebuild cleanup failed")
			}
			deleted = count
			return nil
		})
		if err != nil {
			return nil, err
		}
		if deleted == 0 {
			break
		}
	}

	for start := 0; start < len(prepared.Records); start += writeBatchSize {
		end := min(start+writeBatchSize, len(prepared.Records))
		err := executeProjectRebuildWrite(ctx, session, projectKey, token, func(tx neo4j.ManagedTransaction) error {
			result, runErr := tx.Run(ctx, `
				UNWIND $rows AS row
				MERGE (n:GraphRecord {project_key: $project_key, record_id: row.record_id})
				SET n.record_type = row.record_type,
				    n.payload_json = row.payload_json,
				    n.payload_sha256 = row.payload_sha256,
				    n.kind = row.kind,
				    n.request_signature = row.request_signature,
				    n.response_signature = row.response_signature,
				    n.field_equality_signature = row.field_equality_signature,
				    n.predicate = row.predicate,
				    n.card_endpoint_id = row.card_endpoint_id,
				    n.card_capture_id = row.card_capture_id
				RETURN count(*) AS matched_count`,
				map[string]any{"project_key": projectKey, "rows": recordRows(prepared.Records[start:end])},
			)
			if runErr != nil {
				return fmt.Errorf("Neo4j graph rebuild record batch %d failed (%s)", start/writeBatchSize, safeNeo4jErrorCode(runErr))
			}
			count, countErr := resultCount(ctx, result)
			if countErr != nil || count != int64(end-start) {
				return fmt.Errorf("Neo4j graph rebuild record batch %d was incomplete", start/writeBatchSize)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for start := 0; start < len(prepared.Refs); start += writeBatchSize {
		end := min(start+writeBatchSize, len(prepared.Refs))
		err := executeProjectRebuildWrite(ctx, session, projectKey, token, func(tx neo4j.ManagedTransaction) error {
			result, runErr := tx.Run(ctx, `
				UNWIND $rows AS row
				MATCH (owner:GraphRecord {project_key: $project_key, record_id: row.owner_id})
				MATCH (source:GraphRecord {project_key: $project_key, record_id: row.source_id, record_type: 'source'})
				MERGE (owner)-[r:EVIDENCED_BY {project_key: $project_key}]->(source)
				RETURN count(*) AS matched_count`,
				map[string]any{"project_key": projectKey, "rows": refRows(prepared.Refs[start:end])},
			)
			if runErr != nil {
				return errors.New("Neo4j graph rebuild provenance failed")
			}
			count, countErr := resultCount(ctx, result)
			if countErr != nil || count != int64(end-start) {
				return errors.New("Neo4j graph rebuild provenance is incomplete")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for start := 0; start < len(prepared.Assertions); start += writeBatchSize {
		end := min(start+writeBatchSize, len(prepared.Assertions))
		err := executeProjectRebuildWrite(ctx, session, projectKey, token, func(tx neo4j.ManagedTransaction) error {
			result, runErr := tx.Run(ctx, `
				UNWIND $rows AS row
				MATCH (assertion:GraphRecord {project_key: $project_key, record_id: row.record_id, record_type: 'assertion'})
				MATCH (subject:GraphRecord {project_key: $project_key, record_id: row.subject_id})
				MATCH (object:GraphRecord {project_key: $project_key, record_id: row.object_id})
				MERGE (assertion)-[s:SUBJECT {project_key: $project_key}]->(subject)
				MERGE (assertion)-[o:OBJECT {project_key: $project_key}]->(object)
				RETURN count(*) AS matched_count`,
				map[string]any{"project_key": projectKey, "rows": assertionRows(prepared.Assertions[start:end])},
			)
			if runErr != nil {
				return errors.New("Neo4j graph rebuild assertion links failed")
			}
			count, countErr := resultCount(ctx, result)
			if countErr != nil || count != int64(end-start) {
				return errors.New("Neo4j graph rebuild assertion links are incomplete")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	targetGeneration := baseGeneration + 1
	err = executeProjectRebuildWrite(ctx, session, projectKey, token, func(tx neo4j.ManagedTransaction) error {
		result, runErr := tx.Run(ctx, `
			MATCH (m:GraphProject {project_key: $project_key})
			WHERE m.rebuild_token = $rebuild_token
			SET m.snapshot_sha256 = $snapshot_sha256,
			    m.generation = $generation,
			    m.schema_version = 2,
			    m.signature_index_version = 1,
			    m.signature_index_generation = $generation,
			    m.field_value_index_version = 1,
			    m.field_value_index_generation = $generation,
			    m.projection_state = 'rebuilding',
			    m.record_count = $record_count,
			    m.reference_count = $reference_count,
			    m.assertion_count = $assertion_count
			RETURN m.project_key AS project_key`,
			map[string]any{
				"project_key": projectKey, "rebuild_token": token, "snapshot_sha256": prepared.Digest,
				"generation": targetGeneration, "record_count": len(prepared.Records),
				"reference_count": len(prepared.Refs), "assertion_count": len(prepared.Assertions),
			},
		)
		if runErr != nil || !result.Next(ctx) {
			return errors.New("Neo4j graph rebuild metadata could not be prepared")
		}
		if _, runErr = result.Consume(ctx); runErr != nil {
			return errors.New("Neo4j graph rebuild metadata could not be prepared")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return s.readProjectSnapshotTx(ctx, tx, projectKey, true, token)
	})
	if err != nil {
		return nil, err
	}
	readback, ok := value.(behaviorgraph.Record)
	if !ok {
		return nil, errors.New("Neo4j graph rebuild read-back is invalid")
	}
	readbackDigest, err := behaviorgraph.SnapshotDigest(readback)
	if err != nil || readbackDigest != prepared.Digest {
		return nil, errors.New("Neo4j graph rebuild did not match the source snapshot")
	}

	err = executeProjectRebuildWrite(ctx, session, projectKey, token, func(tx neo4j.ManagedTransaction) error {
		result, runErr := tx.Run(ctx, `
			MATCH (m:GraphProject {project_key: $project_key})
			WHERE m.rebuild_token = $rebuild_token AND m.generation = $generation
			SET m.projection_state = 'snapshot'
			REMOVE m.rebuild_token, m.rebuild_base_generation, m.rebuild_target_sha256
			RETURN m.project_key AS project_key`,
			map[string]any{"project_key": projectKey, "rebuild_token": token, "generation": targetGeneration},
		)
		if runErr != nil || !result.Next(ctx) {
			return errors.New("Neo4j graph rebuild could not be published")
		}
		if _, runErr = result.Consume(ctx); runErr != nil {
			return errors.New("Neo4j graph rebuild could not be published")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]int{
		"records": len(prepared.Records), "references": len(prepared.Refs),
		"assertions": len(prepared.Assertions), "snapshot_bytes": prepared.SnapshotBytes,
	}, nil
}

func executeProjectRebuildWrite(
	ctx context.Context,
	session neo4j.SessionWithContext,
	projectKey, token string,
	write func(neo4j.ManagedTransaction) error,
) error {
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if err := lockProjectForWrite(ctx, tx, projectKey); err != nil {
			return nil, err
		}
		if err := requireProjectRebuildOwner(ctx, tx, projectKey, token); err != nil {
			return nil, err
		}
		if err := write(tx); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("Neo4j graph rebuild transaction failed: %w", err)
	}
	return nil
}

// ApplyOutboxBundle applies one checksummed, sanitized graph import event in an
// idempotent Neo4j transaction. The event payload is the normalized bundle
// already committed to the project's SQLite outbox.
func (s *Store) ApplyOutboxBundle(ctx context.Context, projectKey, bundleID string, bundleJSON []byte) (bool, error) {
	if s == nil || s.driver == nil || !projectKeyPattern.MatchString(projectKey) || len(bundleJSON) == 0 || len(bundleJSON) > behaviorgraph.MaxBundleJSONBytes {
		return false, errors.New("Neo4j outbox event is invalid")
	}
	digest := sha256.Sum256(bundleJSON)
	if bundleID != "bundle-"+hex.EncodeToString(digest[:]) {
		return false, errors.New("Neo4j outbox checksum does not match its bundle identity")
	}
	bundle, err := behaviorgraph.DecodeBundleJSON(bundleJSON)
	if err != nil || bundle["program"] != projectKey {
		return false, errors.New("Neo4j outbox bundle is malformed or belongs to another project")
	}
	if err := behaviorgraph.ValidatePublicData(bundle); err != nil {
		return false, errors.New("Neo4j outbox bundle contains unsafe graph data")
	}
	snapshot := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"sources": bundle["sources"], "entities": bundle["entities"],
		"assertions": bundle["assertions"], "cards": bundle["cards"],
	}
	prepared, err := prepareGraphSnapshot(projectKey, snapshot, false)
	if err != nil {
		return false, err
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return false, err
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database})
	defer session.Close(ctx)
	value, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if err := lockProjectForWrite(ctx, tx, projectKey); err != nil {
			return nil, err
		}
		marker, runErr := tx.Run(ctx, `
			OPTIONAL MATCH (i:GraphImport {project_key: $project_key, bundle_id: $bundle_id})
			OPTIONAL MATCH (p:GraphProject {project_key: $project_key})
			RETURN i.payload_sha256 AS payload_sha256, p.projection_state AS projection_state`,
			map[string]any{"project_key": projectKey, "bundle_id": bundleID, "payload_sha256": hex.EncodeToString(digest[:])},
		)
		if runErr != nil || !marker.Next(ctx) {
			return nil, errors.New("Neo4j outbox checkpoint could not be opened")
		}
		markerRecord := marker.Record()
		storedDigestValue, _ := markerRecord.Get("payload_sha256")
		projectionStateValue, _ := markerRecord.Get("projection_state")
		storedDigest, exists := storedDigestValue.(string)
		if _, runErr = marker.Consume(ctx); runErr != nil {
			return nil, errors.New("Neo4j outbox checkpoint could not be read")
		}
		if !projectionStateUsable(projectionStateValue) {
			if projectionState, _ := projectionStateValue.(string); projectionState == "rebuilding" {
				return nil, ErrProjectRebuildInProgress
			}
			return nil, errors.New("Neo4j project projection state is invalid")
		}
		if exists {
			if storedDigest != hex.EncodeToString(digest[:]) {
				return nil, errors.New("Neo4j outbox checkpoint has a conflicting payload")
			}
			return false, nil
		}
		if err := writePreparedGraph(ctx, tx, projectKey, prepared); err != nil {
			return nil, err
		}
		checkpoint, runErr := tx.Run(ctx, `
			MERGE (i:GraphImport {project_key: $project_key, bundle_id: $bundle_id})
			ON CREATE SET i.payload_sha256 = $payload_sha256
			RETURN i.payload_sha256 AS payload_sha256`,
			map[string]any{"project_key": projectKey, "bundle_id": bundleID, "payload_sha256": hex.EncodeToString(digest[:])},
		)
		if runErr != nil || !checkpoint.Next(ctx) {
			return nil, errors.New("Neo4j outbox checkpoint could not be committed")
		}
		checkpointRecord := checkpoint.Record()
		checkpointValue, _ := checkpointRecord.Get("payload_sha256")
		checkpointDigest, valid := checkpointValue.(string)
		if _, runErr = checkpoint.Consume(ctx); runErr != nil || !valid || checkpointDigest != hex.EncodeToString(digest[:]) {
			return nil, errors.New("Neo4j outbox checkpoint has a conflicting payload")
		}
		projectMarker, runErr := tx.Run(ctx, incrementalProjectionMarkerQuery,
			map[string]any{"project_key": projectKey},
		)
		if runErr != nil || !projectMarker.Next(ctx) {
			return nil, errors.New("Neo4j project projection state could not be updated")
		}
		if _, runErr = projectMarker.Consume(ctx); runErr != nil {
			return nil, errors.New("Neo4j project projection state could not be updated")
		}
		return true, nil
	})
	if err != nil {
		return false, err
	}
	applied, ok := value.(bool)
	if !ok {
		return false, errors.New("Neo4j outbox transaction returned an invalid result")
	}
	return applied, nil
}

// ReadProjectSnapshot returns only records and relationships from one selected
// project namespace, reconstructing provenance arrays for the shared validator.
func (s *Store) ReadProjectSnapshot(ctx context.Context, projectKey string) (behaviorgraph.Record, error) {
	if s == nil || s.driver == nil || !projectKeyPattern.MatchString(projectKey) {
		return nil, errors.New("selected Neo4j project is invalid")
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return s.readProjectSnapshotTx(ctx, tx, projectKey, false, "")
	})
	if err != nil {
		return nil, err
	}
	snapshot, ok := value.(behaviorgraph.Record)
	if !ok {
		return nil, errors.New("Neo4j graph snapshot result is invalid")
	}
	return snapshot, nil
}

// ReadProjectObservationIDsByVariantSignatures performs an exact indexed
// lookup over one project's signature-bearing variant nodes and follows only
// their assertion relationships. It never loads the full graph snapshot.
func (s *Store) ReadProjectObservationIDsByVariantSignatures(
	ctx context.Context,
	projectKey, requestSignature, responseSignature string,
	limit int,
) ([]string, bool, error) {
	if s == nil || s.driver == nil || !projectKeyPattern.MatchString(projectKey) ||
		(requestSignature == "" && responseSignature == "") ||
		(requestSignature != "" && !signatureDigestPattern.MatchString(requestSignature)) ||
		(responseSignature != "" && !signatureDigestPattern.MatchString(responseSignature)) ||
		limit < 1 || limit > maxSignatureObservations {
		return nil, false, errors.New("Neo4j variant signature lookup is invalid")
	}

	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	type lookupResult struct {
		ids       []string
		truncated bool
	}
	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		metadata, runErr := tx.Run(ctx, `
			MATCH (project:GraphProject {project_key: $project_key})
			RETURN project.signature_index_version AS signature_index_version,
			       project.generation AS generation,
			       project.signature_index_generation AS signature_index_generation,
			       project.projection_state AS projection_state`,
			map[string]any{"project_key": projectKey},
		)
		if runErr != nil {
			return nil, errors.New("Neo4j project signature index is unavailable")
		}
		if !metadata.Next(ctx) {
			if metadata.Err() != nil {
				return nil, errors.New("Neo4j project signature index is unavailable")
			}
			return nil, fmt.Errorf("%w; reproject the selected graph", ErrSignatureIndexUnavailable)
		}
		metadataRecord := metadata.Record()
		versionValue, _ := metadataRecord.Get("signature_index_version")
		generationValue, _ := metadataRecord.Get("generation")
		indexedGenerationValue, _ := metadataRecord.Get("signature_index_generation")
		projectionStateValue, _ := metadataRecord.Get("projection_state")
		current := signatureIndexMarkerCurrent(versionValue, generationValue, indexedGenerationValue) && projectionStateUsable(projectionStateValue)
		generation, generationOK := generationValue.(int64)
		if projectionState, _ := projectionStateValue.(string); projectionState == "rebuilding" {
			return nil, ErrProjectRebuildInProgress
		}
		if _, runErr = metadata.Consume(ctx); runErr != nil || !current {
			return nil, fmt.Errorf("%w; reproject the selected graph", ErrSignatureIndexUnavailable)
		}

		parameters := map[string]any{"project_key": projectKey, "limit_plus_one": limit + 1}
		if requestSignature != "" {
			parameters["request_signature"] = requestSignature
		}
		if responseSignature != "" {
			parameters["response_signature"] = responseSignature
		}
		result, queryErr := tx.Run(ctx, observationIDsByVariantSignaturesQuery(requestSignature != "", responseSignature != ""), parameters)
		if queryErr != nil {
			return nil, errors.New("Neo4j project signature query failed")
		}
		ids := make([]string, 0, min(limit+1, 256))
		for result.Next(ctx) {
			idValue, _ := result.Record().Get("observation_id")
			id, ok := idValue.(string)
			if !ok || id == "" || len(id) > 512 {
				return nil, errors.New("Neo4j project signature query returned an invalid observation")
			}
			ids = append(ids, id)
		}
		if queryErr := result.Err(); queryErr != nil {
			return nil, errors.New("Neo4j project signature query could not finish")
		}
		if _, queryErr := result.Consume(ctx); queryErr != nil {
			return nil, errors.New("Neo4j project signature query could not finish")
		}
		finalGeneration, generationErr := neighborhoodGeneration(ctx, tx, projectKey)
		if generationErr != nil || !generationOK || finalGeneration != generation {
			return nil, errors.New("Neo4j project signature query changed while it was being read")
		}
		truncated := len(ids) > limit
		if truncated {
			ids = ids[:limit]
		}
		return lookupResult{ids: ids, truncated: truncated}, nil
	})
	if err != nil {
		return nil, false, err
	}
	result, ok := value.(lookupResult)
	if !ok {
		return nil, false, errors.New("Neo4j project signature query result is invalid")
	}
	return result.ids, result.truncated, nil
}

func signatureIndexMarkerCurrent(versionValue, generationValue, indexedGenerationValue any) bool {
	version, versionOK := versionValue.(int64)
	generation, generationOK := generationValue.(int64)
	indexedGeneration, indexedGenerationOK := indexedGenerationValue.(int64)
	return versionOK && version == 1 && generationOK && generation >= 1 &&
		indexedGenerationOK && indexedGeneration == generation
}

const incrementalProjectionMarkerQuery = `
	MERGE (m:GraphProject {project_key: $project_key})
	WITH m, coalesce(m.generation, 0) AS previous_generation
	WITH m, previous_generation, previous_generation + 1 AS next_generation
	SET m.schema_version = 2,
	    m.snapshot_sha256 = null,
	    m.generation = next_generation,
	    m.signature_index_generation = CASE
	        WHEN m.signature_index_version = 1
	            AND m.signature_index_generation = previous_generation
	            AND previous_generation >= 1
	        THEN next_generation
	        ELSE null
	    END,
	    m.field_value_index_generation = CASE
	        WHEN m.field_value_index_version = 1
	            AND m.field_value_index_generation = previous_generation
	            AND previous_generation >= 1
	        THEN next_generation
	        ELSE null
	    END,
	    m.record_count = null,
	    m.reference_count = null,
	    m.assertion_count = null,
	    m.projection_state = 'incremental'
	RETURN m.project_key AS project_key`

func observationIDsByVariantSignaturesQuery(requestSignature, responseSignature bool) string {
	requestPattern := `
		MATCH (requestVariant:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'request_variant', request_signature: $request_signature})
		MATCH (requestAssertion:GraphRecord {project_key: $project_key, record_type: 'assertion', predicate: 'uses_request_variant'})
		MATCH (requestAssertion)-[:OBJECT {project_key: $project_key}]->(requestVariant)
		MATCH (requestAssertion)-[:SUBJECT {project_key: $project_key}]->(observation:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'observation'})`
	responsePattern := `
		MATCH (responseVariant:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'response_variant', response_signature: $response_signature})
		MATCH (responseAssertion:GraphRecord {project_key: $project_key, record_type: 'assertion', predicate: 'received_response'})
		MATCH (responseAssertion)-[:OBJECT {project_key: $project_key}]->(responseVariant)
		MATCH (responseAssertion)-[:SUBJECT {project_key: $project_key}]->(observation:GraphRecord {project_key: $project_key, record_type: 'entity', kind: 'observation'})`
	query := ""
	switch {
	case requestSignature && responseSignature:
		query = requestPattern + responsePattern
	case requestSignature:
		query = requestPattern
	case responseSignature:
		query = responsePattern
	default:
		return ""
	}
	return query + `
		RETURN DISTINCT observation.record_id AS observation_id
		ORDER BY observation_id
		LIMIT $limit_plus_one`
}

func (s *Store) readProjectSnapshotTx(ctx context.Context, tx neo4j.ManagedTransaction, projectKey string, allowRebuilding bool, rebuildToken string) (behaviorgraph.Record, error) {
	metadata, err := tx.Run(ctx, `
		MATCH (m:GraphProject {project_key: $project_key})
		RETURN m.snapshot_sha256 AS snapshot_sha256, m.record_count AS record_count,
		       m.reference_count AS reference_count, m.assertion_count AS assertion_count,
	       m.schema_version AS schema_version, m.generation AS generation,
	       m.projection_state AS projection_state, m.rebuild_token AS rebuild_token`,
		map[string]any{"project_key": projectKey},
	)
	if err != nil || !metadata.Next(ctx) {
		return nil, errors.New("Neo4j graph snapshot metadata is unavailable")
	}
	meta := metadata.Record()
	snapshotDigestValue, _ := meta.Get("snapshot_sha256")
	recordCountValue, _ := meta.Get("record_count")
	refCountValue, _ := meta.Get("reference_count")
	assertionCountValue, _ := meta.Get("assertion_count")
	schemaVersionValue, _ := meta.Get("schema_version")
	generationValue, _ := meta.Get("generation")
	projectionStateValue, _ := meta.Get("projection_state")
	rebuildTokenValue, _ := meta.Get("rebuild_token")
	wantDigest, digestOK := snapshotDigestValue.(string)
	wantRecords, recordsOK := recordCountValue.(int64)
	wantRefs, refsOK := refCountValue.(int64)
	wantAssertions, assertionsOK := assertionCountValue.(int64)
	wantSchema, schemaOK := schemaVersionValue.(int64)
	wantGeneration, generationOK := generationValue.(int64)
	projectionState, _ := projectionStateValue.(string)
	storedRebuildToken, _ := rebuildTokenValue.(string)
	stateValid := projectionStateUsable(projectionStateValue)
	if allowRebuilding {
		stateValid = projectionState == "rebuilding" && rebuildToken != "" && storedRebuildToken == rebuildToken
	}
	if !allowRebuilding && projectionState == "rebuilding" {
		return nil, ErrProjectRebuildInProgress
	}
	if _, err := metadata.Consume(ctx); err != nil || !schemaOK || wantSchema != 2 || !generationOK || wantGeneration < 1 ||
		!stateValid ||
		(snapshotDigestValue != nil && (!digestOK || !projectKeyPattern.MatchString(wantDigest))) ||
		(recordCountValue != nil && (!recordsOK || wantRecords < 0)) ||
		(refCountValue != nil && (!refsOK || wantRefs < 0)) ||
		(assertionCountValue != nil && (!assertionsOK || wantAssertions < 0)) {
		return nil, errors.New("Neo4j graph snapshot metadata is malformed")
	}
	result, err := tx.Run(ctx, `
		MATCH (n:GraphRecord {project_key: $project_key})
		RETURN n.record_id AS record_id, n.record_type AS record_type, n.payload_json AS payload_json,
		       n.payload_sha256 AS payload_sha256
		ORDER BY n.record_type, n.record_id LIMIT $limit`,
		map[string]any{"project_key": projectKey, "limit": maxSnapshotRecords + 1},
	)
	if err != nil {
		return nil, errors.New("Neo4j graph records are unavailable")
	}
	entities, assertions, cards, sources := make([]behaviorgraph.Record, 0), make([]behaviorgraph.Record, 0), make([]behaviorgraph.Record, 0), make([]behaviorgraph.Record, 0)
	owners := make(map[string]string)
	var totalBytes int
	for result.Next(ctx) {
		record := result.Record()
		id, _ := record.Get("record_id")
		kind, _ := record.Get("record_type")
		payload, _ := record.Get("payload_json")
		payloadHashValue, _ := record.Get("payload_sha256")
		idText, idOK := id.(string)
		kindText, kindOK := kind.(string)
		payloadText, payloadOK := payload.(string)
		payloadHash, hashOK := payloadHashValue.(string)
		if !idOK || !kindOK || !payloadOK || !hashOK || len(payloadText) > 1<<20 {
			return nil, errors.New("Neo4j graph record exceeds its safe read limit")
		}
		actualPayloadHash := sha256.Sum256([]byte(payloadText))
		if payloadHash != hex.EncodeToString(actualPayloadHash[:]) {
			return nil, errors.New("Neo4j graph record checksum does not match")
		}
		totalBytes += len(payloadText)
		if totalBytes > maxSnapshotBytes {
			return nil, errors.New("Neo4j graph snapshot exceeds its safe read limit")
		}
		decoded := behaviorgraph.Record{}
		if err := json.Unmarshal([]byte(payloadText), &decoded); err != nil {
			return nil, errors.New("Neo4j graph record contains invalid JSON")
		}
		switch kindText {
		case "entity":
			if decoded["id"] != idText {
				return nil, errors.New("Neo4j graph entity identity is inconsistent")
			}
			entities = append(entities, decoded)
			owners[idText] = kindText
		case "assertion":
			if decoded["id"] != idText {
				return nil, errors.New("Neo4j graph assertion identity is inconsistent")
			}
			assertions = append(assertions, decoded)
			owners[idText] = kindText
		case "card":
			if decoded["id"] != idText {
				return nil, errors.New("Neo4j graph card identity is inconsistent")
			}
			cards = append(cards, decoded)
			owners[idText] = kindText
		case "source":
			sources = append(sources, decoded)
			owners[idText] = kindText
		default:
			return nil, errors.New("Neo4j graph record type is unsupported")
		}
		if len(owners) > maxSnapshotRecords {
			return nil, errors.New("Neo4j graph has too many records")
		}
	}
	if err := result.Err(); err != nil {
		return nil, errors.New("Neo4j graph records could not be read")
	}
	refResult, err := tx.Run(ctx, `
		MATCH (owner:GraphRecord {project_key: $project_key})-[:EVIDENCED_BY {project_key: $project_key}]->(source:GraphRecord {project_key: $project_key, record_type: 'source'})
		RETURN owner.record_id AS owner_id, source.payload_json AS payload_json
		ORDER BY owner.record_id, source.record_id LIMIT $limit`,
		map[string]any{"project_key": projectKey, "limit": maxSnapshotRecords + 1},
	)
	if err != nil {
		return nil, errors.New("Neo4j graph provenance is unavailable")
	}
	refsByOwner := make(map[string][]behaviorgraph.Record)
	refCount := 0
	for refResult.Next(ctx) {
		row := refResult.Record()
		ownerValue, _ := row.Get("owner_id")
		payloadValue, _ := row.Get("payload_json")
		ownerID, ownerOK := ownerValue.(string)
		payload, payloadOK := payloadValue.(string)
		if !ownerOK || !payloadOK || owners[ownerID] == "" || len(payload) > 1<<20 {
			return nil, errors.New("Neo4j graph provenance is malformed")
		}
		var budgetErr error
		totalBytes, budgetErr = addSnapshotBytes(totalBytes, len(payload))
		if budgetErr != nil {
			return nil, errors.New("Neo4j graph snapshot exceeds its safe read limit")
		}
		ref := behaviorgraph.Record{}
		if err := json.Unmarshal([]byte(payload), &ref); err != nil {
			return nil, errors.New("Neo4j graph provenance is malformed")
		}
		refsByOwner[ownerID] = append(refsByOwner[ownerID], ref)
		refCount++
		if refCount > maxSnapshotRecords {
			return nil, errors.New("Neo4j graph has too many provenance records")
		}
	}
	if err := refResult.Err(); err != nil {
		return nil, errors.New("Neo4j graph provenance could not be read")
	}
	for _, group := range [][]behaviorgraph.Record{entities, assertions, cards} {
		for _, record := range group {
			id, _ := record["id"].(string)
			record["source_refs"] = refsByOwner[id]
		}
	}
	if len(owners) == 0 {
		return nil, errors.New("Neo4j selected project graph snapshot is missing")
	}
	for _, values := range [][]behaviorgraph.Record{entities, assertions, cards, sources} {
		sort.Slice(values, func(i, j int) bool {
			return recordSortKey(values[i]) < recordSortKey(values[j])
		})
	}
	snapshot := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"sources": sources, "entities": entities, "assertions": assertions, "cards": cards,
	}
	if err := behaviorgraph.ValidatePublicData(snapshot); err != nil {
		return nil, errors.New("Neo4j graph snapshot contains unsafe metadata")
	}
	actualDigest, err := behaviorgraph.SnapshotDigest(snapshot)
	if err != nil || (digestOK && actualDigest != wantDigest) ||
		(recordsOK && int64(len(owners)) != wantRecords) ||
		(refsOK && int64(refCount) != wantRefs) ||
		(assertionsOK && int64(len(assertions)) != wantAssertions) {
		return nil, errors.New("Neo4j graph snapshot failed its integrity check")
	}
	finalMetadata, err := tx.Run(ctx, `
		MATCH (m:GraphProject {project_key: $project_key})
		RETURN m.generation AS generation, m.projection_state AS projection_state,
		       m.rebuild_token AS rebuild_token`,
		map[string]any{"project_key": projectKey},
	)
	if err != nil || !finalMetadata.Next(ctx) {
		return nil, errors.New("Neo4j graph snapshot generation could not be rechecked")
	}
	finalGenerationValue, _ := finalMetadata.Record().Get("generation")
	finalGeneration, finalGenerationOK := finalGenerationValue.(int64)
	finalStateValue, _ := finalMetadata.Record().Get("projection_state")
	finalTokenValue, _ := finalMetadata.Record().Get("rebuild_token")
	finalState, _ := finalStateValue.(string)
	finalToken, _ := finalTokenValue.(string)
	stateStillCurrent := projectionStateUsable(finalStateValue)
	if allowRebuilding {
		stateStillCurrent = finalState == "rebuilding" && finalToken == rebuildToken
	} else {
		stateStillCurrent = stateStillCurrent && finalState == projectionState
	}
	if _, err := finalMetadata.Consume(ctx); err != nil || !finalGenerationOK || finalGeneration != wantGeneration || !stateStillCurrent {
		return nil, errors.New("Neo4j graph snapshot changed while it was being read")
	}
	return snapshot, nil
}

func addSnapshotBytes(current, addition int) (int, error) {
	if current < 0 || addition < 0 || addition > maxSnapshotBytes || current > maxSnapshotBytes-addition {
		return 0, errors.New("Neo4j graph snapshot exceeds its safe read limit")
	}
	return current + addition, nil
}

func lockProjectForWrite(ctx context.Context, tx neo4j.ManagedTransaction, projectKey string) error {
	result, err := tx.Run(ctx, `
		MERGE (m:GraphProject {project_key: $project_key})
		SET m.write_lock = coalesce(m.write_lock, 0) + 1
		RETURN m.project_key AS project_key`,
		map[string]any{"project_key": projectKey},
	)
	if err != nil || !result.Next(ctx) {
		return errors.New("Neo4j project graph could not be locked for a write")
	}
	projectValue, _ := result.Record().Get("project_key")
	project, ok := projectValue.(string)
	if _, err := result.Consume(ctx); err != nil || !ok || project != projectKey {
		return errors.New("Neo4j project graph could not be locked for a write")
	}
	return nil
}

func projectionStateUsable(value any) bool {
	if value == nil {
		return true // tolerate pre-marker snapshots while keeping rebuild fail-closed
	}
	state, ok := value.(string)
	if !ok {
		return false
	}
	switch state {
	case "", "snapshot", "incremental":
		return true
	default:
		return false
	}
}

func requireProjectNotRebuilding(ctx context.Context, tx neo4j.ManagedTransaction, projectKey string) error {
	result, err := tx.Run(ctx, `
		MATCH (m:GraphProject {project_key: $project_key})
		RETURN m.projection_state AS projection_state`, map[string]any{"project_key": projectKey})
	if err != nil || !result.Next(ctx) {
		return errors.New("Neo4j project projection state is unavailable")
	}
	value, _ := result.Record().Get("projection_state")
	state, _ := value.(string)
	if _, err := result.Consume(ctx); err != nil {
		return errors.New("Neo4j project projection state is unavailable")
	}
	if state == "rebuilding" {
		return ErrProjectRebuildInProgress
	}
	if !projectionStateUsable(value) {
		return errors.New("Neo4j project projection state is invalid")
	}
	return nil
}

func requireProjectRebuildOwner(ctx context.Context, tx neo4j.ManagedTransaction, projectKey, token string) error {
	result, err := tx.Run(ctx, `
		MATCH (m:GraphProject {project_key: $project_key})
		WHERE m.projection_state = 'rebuilding' AND m.rebuild_token = $rebuild_token
		RETURN m.project_key AS project_key`,
		map[string]any{"project_key": projectKey, "rebuild_token": token})
	if err != nil || !result.Next(ctx) {
		return ErrProjectRebuildInProgress
	}
	value, _ := result.Record().Get("project_key")
	owner, ok := value.(string)
	if _, err := result.Consume(ctx); err != nil || !ok || owner != projectKey {
		return ErrProjectRebuildInProgress
	}
	return nil
}

func prepareSnapshot(projectKey string, snapshot behaviorgraph.Record) (preparedSnapshot, error) {
	return prepareGraphSnapshot(projectKey, snapshot, true)
}

func writePreparedGraph(ctx context.Context, tx neo4j.ManagedTransaction, projectKey string, prepared preparedSnapshot) error {
	conflicts, err := tx.Run(ctx, `
		UNWIND $rows AS row
		OPTIONAL MATCH (n:GraphRecord {project_key: $project_key, record_id: row.record_id})
		WITH row, n
		WHERE n IS NOT NULL AND (n.record_type <> row.record_type OR (row.record_type <> 'entity' AND n.payload_sha256 <> row.payload_sha256))
		RETURN count(*) AS conflict_count`,
		map[string]any{"project_key": projectKey, "rows": recordRows(prepared.Records)},
	)
	if err != nil {
		return errors.New("Neo4j graph identity check failed")
	}
	conflictCount, err := resultCount(ctx, conflicts)
	if err != nil || conflictCount != 0 {
		return errors.New("Neo4j graph contains an immutable record identity conflict")
	}
	for start := 0; start < len(prepared.Records); start += writeBatchSize {
		end := min(start+writeBatchSize, len(prepared.Records))
		result, runErr := tx.Run(ctx, `
			UNWIND $rows AS row
			MERGE (n:GraphRecord {project_key: $project_key, record_id: row.record_id})
			SET n.record_type = row.record_type,
			    n.payload_json = CASE WHEN row.record_type = 'entity' AND n.payload_json IS NOT NULL THEN n.payload_json ELSE row.payload_json END,
			    n.payload_sha256 = CASE WHEN row.record_type = 'entity' AND n.payload_sha256 IS NOT NULL THEN n.payload_sha256 ELSE row.payload_sha256 END,
			    n.kind = row.kind,
			    n.request_signature = row.request_signature,
			    n.response_signature = row.response_signature,
			    n.field_equality_signature = row.field_equality_signature,
			    n.predicate = row.predicate,
			    n.card_endpoint_id = row.card_endpoint_id,
			    n.card_capture_id = row.card_capture_id`,
			map[string]any{"project_key": projectKey, "rows": recordRows(prepared.Records[start:end])},
		)
		if runErr != nil {
			return fmt.Errorf("Neo4j graph records batch %d could not be written (%s)", start/writeBatchSize, safeNeo4jErrorCode(runErr))
		}
		if _, runErr = result.Consume(ctx); runErr != nil {
			return fmt.Errorf("Neo4j graph records batch %d could not be written (%s)", start/writeBatchSize, safeNeo4jErrorCode(runErr))
		}
	}
	for start := 0; start < len(prepared.Refs); start += writeBatchSize {
		end := min(start+writeBatchSize, len(prepared.Refs))
		query := `
			UNWIND $rows AS row
			MATCH (owner:GraphRecord {project_key: $project_key, record_id: row.owner_id})
			MATCH (source:GraphRecord {project_key: $project_key, record_id: row.source_id, record_type: 'source'})
			MERGE (owner)-[r:EVIDENCED_BY {project_key: $project_key}]->(source)
			RETURN count(*) AS matched_count`
		result, runErr := tx.Run(ctx, query, map[string]any{"project_key": projectKey, "rows": refRows(prepared.Refs[start:end])})
		if runErr != nil {
			return errors.New("Neo4j graph provenance could not be written")
		}
		count, countErr := resultCount(ctx, result)
		if countErr != nil || count != int64(end-start) {
			return errors.New("Neo4j graph provenance endpoints are incomplete")
		}
	}
	for start := 0; start < len(prepared.Assertions); start += writeBatchSize {
		end := min(start+writeBatchSize, len(prepared.Assertions))
		query := `
			UNWIND $rows AS row
			MATCH (assertion:GraphRecord {project_key: $project_key, record_id: row.record_id, record_type: 'assertion'})
			MATCH (subject:GraphRecord {project_key: $project_key, record_id: row.subject_id})
			MATCH (object:GraphRecord {project_key: $project_key, record_id: row.object_id})
			MERGE (assertion)-[s:SUBJECT {project_key: $project_key}]->(subject)
			MERGE (assertion)-[o:OBJECT {project_key: $project_key}]->(object)
			RETURN count(*) AS matched_count`
		result, runErr := tx.Run(ctx, query, map[string]any{"project_key": projectKey, "rows": assertionRows(prepared.Assertions[start:end])})
		if runErr != nil {
			return errors.New("Neo4j assertion relationships could not be written")
		}
		count, countErr := resultCount(ctx, result)
		if countErr != nil || count != int64(end-start) {
			return errors.New("Neo4j assertion endpoints are incomplete")
		}
	}
	return nil
}

// safeNeo4jErrorCode preserves actionable server diagnostics without
// exposing messages that may contain query parameters or local values.
func safeNeo4jErrorCode(err error) string {
	var neo4jErr *neo4j.Neo4jError
	if errors.As(err, &neo4jErr) && neo4jErr != nil && neo4jErrorCodePattern.MatchString(neo4jErr.Code) {
		return neo4jErr.Code
	}
	return "Neo4j.DriverError.Unknown"
}

func resultCount(ctx context.Context, result neo4j.ResultWithContext) (int64, error) {
	if !result.Next(ctx) {
		if err := result.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("Neo4j count query returned no row")
	}
	value, _ := result.Record().Get("matched_count")
	if value == nil {
		value, _ = result.Record().Get("conflict_count")
	}
	count, ok := value.(int64)
	if !ok {
		return 0, errors.New("Neo4j count query result is invalid")
	}
	if _, err := result.Consume(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func cloneGraphRecord(record behaviorgraph.Record) behaviorgraph.Record {
	copyRecord := make(behaviorgraph.Record, len(record))
	for key, value := range record {
		copyRecord[key] = value
	}
	return copyRecord
}

func prepareGraphSnapshot(projectKey string, snapshot behaviorgraph.Record, requireAllEndpoints bool) (preparedSnapshot, error) {
	if !projectKeyPattern.MatchString(projectKey) || snapshot["program"] != projectKey || !graphIntegerIsTwo(snapshot["schema_version"]) {
		return preparedSnapshot{}, errors.New("graph snapshot namespace or schema is invalid")
	}
	if err := behaviorgraph.ValidatePublicData(snapshot); err != nil {
		return preparedSnapshot{}, errors.New("graph snapshot contains unsafe or private data")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) == 0 || len(encoded) > maxSnapshotBytes {
		return preparedSnapshot{}, errors.New("graph snapshot exceeds its safe size limit")
	}
	var output preparedSnapshot
	output.SnapshotBytes = len(encoded)
	seen := make(map[string]struct{})
	sources := make(map[string]struct{})
	getList := func(key string) ([]behaviorgraph.Record, error) {
		value := snapshot[key]
		switch records := value.(type) {
		case []behaviorgraph.Record:
			return records, nil
		case []any:
			converted := make([]behaviorgraph.Record, 0, len(records))
			for _, item := range records {
				row, ok := item.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("graph snapshot %s are malformed", key)
				}
				converted = append(converted, behaviorgraph.Record(row))
			}
			return converted, nil
		default:
			return nil, fmt.Errorf("graph snapshot %s are malformed", key)
		}
	}
	sourceRecords, err := getList("sources")
	if err != nil {
		return preparedSnapshot{}, err
	}
	entities, err := getList("entities")
	if err != nil {
		return preparedSnapshot{}, err
	}
	assertions, err := getList("assertions")
	if err != nil {
		return preparedSnapshot{}, err
	}
	cards, err := getList("cards")
	if err != nil {
		return preparedSnapshot{}, err
	}
	for _, group := range [][]behaviorgraph.Record{entities, assertions, cards} {
		for _, record := range group {
			refs, ok := record["source_refs"].([]behaviorgraph.Record)
			if !ok {
				if raw, exists := record["source_refs"].([]any); exists {
					refs = make([]behaviorgraph.Record, 0, len(raw))
					for _, item := range raw {
						ref, isRecord := item.(map[string]any)
						if !isRecord {
							return preparedSnapshot{}, errors.New("graph snapshot provenance is malformed")
						}
						refs = append(refs, behaviorgraph.Record(ref))
					}
				} else {
					return preparedSnapshot{}, errors.New("graph snapshot provenance is malformed")
				}
			}
			record["source_refs"] = refs
		}
	}
	add := func(record behaviorgraph.Record, recordType string, sourceID string) (string, error) {
		id := sourceID
		if id == "" {
			id, _ = record["id"].(string)
		}
		if id == "" || len(id) > 256 {
			return "", errors.New("graph snapshot record identity is invalid")
		}
		if _, exists := seen[id]; exists {
			return "", errors.New("graph snapshot contains duplicate record identities")
		}
		seen[id] = struct{}{}
		payloadRecord := cloneGraphRecord(record)
		if recordType != "source" {
			delete(payloadRecord, "source_refs")
		}
		payloadBytes, marshalErr := json.Marshal(payloadRecord)
		if marshalErr != nil || len(payloadBytes) == 0 || len(payloadBytes) > 1<<20 {
			return "", errors.New("graph snapshot record exceeds its safe size limit")
		}
		digest := sha256.Sum256(payloadBytes)
		item := preparedRecord{ID: id, Type: recordType, Payload: string(payloadBytes), PayloadHash: hex.EncodeToString(digest[:])}
		if value, ok := record["kind"].(string); ok {
			item.Kind = value
		}
		var variantData map[string]any
		switch data := record["data"].(type) {
		case map[string]any:
			variantData = data
		case behaviorgraph.Record:
			variantData = map[string]any(data)
		}
		if variantData != nil {
			if item.Kind == "request_variant" {
				if value, ok := variantData["request_signature"].(string); ok && signatureDigestPattern.MatchString(value) {
					item.RequestSignature = value
				}
			}
			if item.Kind == "response_variant" {
				if value, ok := variantData["response_signature"].(string); ok && signatureDigestPattern.MatchString(value) {
					item.ResponseSignature = value
				}
			}
			if item.Kind == "value_reference" {
				if value, ok := variantData["equality_signature"].(string); ok && signatureDigestPattern.MatchString(value) {
					item.FieldEqualitySignature = value
				}
			}
		}
		if value, ok := record["predicate"].(string); ok {
			item.Predicate = value
		}
		if recordType == "card" {
			if endpoint, ok := record["endpoint"].(map[string]any); ok {
				item.CardEndpointID, _ = endpoint["id"].(string)
			}
			item.CardCaptureID, _ = record["capture_id"].(string)
		}
		output.Records = append(output.Records, item)
		return id, nil
	}
	for _, source := range sourceRecords {
		payload, marshalErr := json.Marshal(source)
		if marshalErr != nil {
			return preparedSnapshot{}, errors.New("graph source could not be encoded")
		}
		digest := sha256.Sum256(payload)
		sourceID := "source-" + hex.EncodeToString(digest[:])
		if _, addErr := add(source, "source", sourceID); addErr != nil {
			return preparedSnapshot{}, addErr
		}
		sources[sourceID] = struct{}{}
	}
	addRefs := func(ownerID string, record behaviorgraph.Record) error {
		refs, ok := record["source_refs"].([]behaviorgraph.Record)
		if !ok {
			if raw, exists := record["source_refs"].([]any); exists {
				refs = make([]behaviorgraph.Record, 0, len(raw))
				for _, item := range raw {
					ref, isRecord := item.(map[string]any)
					if !isRecord {
						return errors.New("graph record provenance is malformed")
					}
					refs = append(refs, behaviorgraph.Record(ref))
				}
			} else {
				return errors.New("graph record provenance is malformed")
			}
		}
		for _, ref := range refs {
			payload, marshalErr := json.Marshal(ref)
			if marshalErr != nil {
				return errors.New("graph source reference could not be encoded")
			}
			digest := sha256.Sum256(payload)
			sourceID := "source-" + hex.EncodeToString(digest[:])
			if _, exists := sources[sourceID]; !exists {
				return errors.New("graph record references an unknown source")
			}
			output.Refs = append(output.Refs, preparedRef{OwnerID: ownerID, SourceID: sourceID})
		}
		return nil
	}
	for _, entity := range entities {
		id, addErr := add(entity, "entity", "")
		if addErr != nil {
			return preparedSnapshot{}, addErr
		}
		if err := addRefs(id, entity); err != nil {
			return preparedSnapshot{}, err
		}
	}
	for _, assertion := range assertions {
		id, addErr := add(assertion, "assertion", "")
		if addErr != nil {
			return preparedSnapshot{}, addErr
		}
		subject, subjectOK := assertion["subject"].(string)
		object, objectOK := assertion["object"].(string)
		if !subjectOK || !objectOK {
			return preparedSnapshot{}, errors.New("graph assertion endpoints are malformed")
		}
		output.Assertions = append(output.Assertions, preparedAssertion{ID: id, SubjectID: subject, ObjectID: object})
		if err := addRefs(id, assertion); err != nil {
			return preparedSnapshot{}, err
		}
	}
	for _, card := range cards {
		id, addErr := add(card, "card", "")
		if addErr != nil {
			return preparedSnapshot{}, addErr
		}
		if err := addRefs(id, card); err != nil {
			return preparedSnapshot{}, err
		}
	}
	if !withinSnapshotRecordBudget(len(output.Records), len(output.Refs)) {
		return preparedSnapshot{}, errors.New("graph snapshot record count is outside the safe limit")
	}
	if requireAllEndpoints {
		for _, assertion := range output.Assertions {
			if _, subjectOK := seen[assertion.SubjectID]; !subjectOK {
				return preparedSnapshot{}, errors.New("graph assertion subject is missing from its project")
			}
			if _, objectOK := seen[assertion.ObjectID]; !objectOK {
				return preparedSnapshot{}, errors.New("graph assertion object is missing from its project")
			}
		}
	}
	normalizedSnapshot := behaviorgraph.Record{
		"schema_version": snapshot["schema_version"], "program": snapshot["program"],
		"sources": sourceRecords, "entities": entities, "assertions": assertions, "cards": cards,
	}
	output.Digest, err = behaviorgraph.SnapshotDigest(normalizedSnapshot)
	if err != nil {
		return preparedSnapshot{}, errors.New("graph snapshot digest could not be computed")
	}
	sort.Slice(output.Records, func(i, j int) bool { return output.Records[i].ID < output.Records[j].ID })
	sort.Slice(output.Refs, func(i, j int) bool {
		if output.Refs[i].OwnerID == output.Refs[j].OwnerID {
			return output.Refs[i].SourceID < output.Refs[j].SourceID
		}
		return output.Refs[i].OwnerID < output.Refs[j].OwnerID
	})
	return output, nil
}

func withinSnapshotRecordBudget(records, refs int) bool {
	return records > 0 && records <= maxSnapshotRecords && refs >= 0 && refs <= maxSnapshotRecords
}

func graphIntegerIsTwo(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		return typed.String() == "2"
	case int:
		return typed == 2
	default:
		return false
	}
}

func recordRows(records []preparedRecord) []map[string]any {
	rows := make([]map[string]any, 0, len(records))
	for _, record := range records {
		rows = append(rows, map[string]any{
			"record_id": record.ID, "record_type": record.Type, "payload_json": record.Payload,
			"payload_sha256": record.PayloadHash, "kind": record.Kind, "predicate": record.Predicate,
			"request_signature":        optionalSignatureProperty(record.RequestSignature),
			"response_signature":       optionalSignatureProperty(record.ResponseSignature),
			"field_equality_signature": optionalSignatureProperty(record.FieldEqualitySignature),
			"card_endpoint_id":         record.CardEndpointID, "card_capture_id": record.CardCaptureID,
		})
	}
	return rows
}

func optionalSignatureProperty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func refRows(refs []preparedRef) []map[string]any {
	rows := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		rows = append(rows, map[string]any{"owner_id": ref.OwnerID, "source_id": ref.SourceID})
	}
	return rows
}

func assertionRows(assertions []preparedAssertion) []map[string]any {
	rows := make([]map[string]any, 0, len(assertions))
	for _, assertion := range assertions {
		rows = append(rows, map[string]any{
			"record_id": assertion.ID, "subject_id": assertion.SubjectID, "object_id": assertion.ObjectID,
		})
	}
	return rows
}

func recordSortKey(record behaviorgraph.Record) string {
	if id, ok := record["id"].(string); ok {
		return id
	}
	encoded, _ := json.Marshal(record)
	return string(encoded)
}

func lenMustMarshal(value any) int {
	encoded, _ := json.Marshal(value)
	return len(encoded)
}
