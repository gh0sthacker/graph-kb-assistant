// Package neo4jstore persists the sanitized BehaviorGraph projection in one
// local Neo4j Community database. Project isolation is enforced on every node
// lookup and relationship write through the mandatory project_key property.
package neo4jstore

import (
	"context"
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
	maxSnapshotRecords = 200_000
	maxSnapshotBytes   = 256 << 20
	writeBatchSize     = 500
)

var projectKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

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

type preparedRecord struct {
	ID             string
	Type           string
	Payload        string
	PayloadHash    string
	Kind           string
	Predicate      string
	CardEndpointID string
	CardCaptureID  string
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
	Records    []preparedRecord
	Refs       []preparedRef
	Assertions []preparedAssertion
	Digest     string
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
				    n.predicate = row.predicate,
				    n.card_endpoint_id = row.card_endpoint_id,
				    n.card_capture_id = row.card_capture_id`,
				map[string]any{"project_key": projectKey, "rows": recordRows(prepared.Records[start:end])},
			)
			if runErr != nil {
				return nil, errors.New("Neo4j graph records could not be written")
			}
			if _, runErr = result.Consume(ctx); runErr != nil {
				return nil, errors.New("Neo4j graph records could not be written")
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
			SET m.snapshot_sha256 = $snapshot_sha256,
			    m.generation = coalesce(m.generation, 0) + 1,
			    m.schema_version = 2,
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
		"assertions": len(prepared.Assertions), "snapshot_bytes": lenMustMarshal(snapshot),
	}, nil
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
			RETURN i.payload_sha256 AS payload_sha256`,
			map[string]any{"project_key": projectKey, "bundle_id": bundleID, "payload_sha256": hex.EncodeToString(digest[:])},
		)
		if runErr != nil || !marker.Next(ctx) {
			return nil, errors.New("Neo4j outbox checkpoint could not be opened")
		}
		markerRecord := marker.Record()
		storedDigestValue, _ := markerRecord.Get("payload_sha256")
		storedDigest, exists := storedDigestValue.(string)
		if _, runErr = marker.Consume(ctx); runErr != nil {
			return nil, errors.New("Neo4j outbox checkpoint could not be read")
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
		projectMarker, runErr := tx.Run(ctx, `
			MERGE (m:GraphProject {project_key: $project_key})
			SET m.schema_version = 2,
			    m.snapshot_sha256 = null,
			    m.generation = coalesce(m.generation, 0) + 1,
			    m.record_count = null,
			    m.reference_count = null,
			    m.assertion_count = null,
			    m.projection_state = 'incremental'
			RETURN m.project_key AS project_key`,
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
		return s.readProjectSnapshotTx(ctx, tx, projectKey)
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

func (s *Store) readProjectSnapshotTx(ctx context.Context, tx neo4j.ManagedTransaction, projectKey string) (behaviorgraph.Record, error) {
	metadata, err := tx.Run(ctx, `
		MATCH (m:GraphProject {project_key: $project_key})
		RETURN m.snapshot_sha256 AS snapshot_sha256, m.record_count AS record_count,
		       m.reference_count AS reference_count, m.assertion_count AS assertion_count,
	       m.schema_version AS schema_version, m.generation AS generation`,
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
	wantDigest, digestOK := snapshotDigestValue.(string)
	wantRecords, recordsOK := recordCountValue.(int64)
	wantRefs, refsOK := refCountValue.(int64)
	wantAssertions, assertionsOK := assertionCountValue.(int64)
	wantSchema, schemaOK := schemaVersionValue.(int64)
	wantGeneration, generationOK := generationValue.(int64)
	if _, err := metadata.Consume(ctx); err != nil || !schemaOK || wantSchema != 2 || !generationOK || wantGeneration < 1 ||
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
		RETURN m.generation AS generation`,
		map[string]any{"project_key": projectKey},
	)
	if err != nil || !finalMetadata.Next(ctx) {
		return nil, errors.New("Neo4j graph snapshot generation could not be rechecked")
	}
	finalGenerationValue, _ := finalMetadata.Record().Get("generation")
	finalGeneration, finalGenerationOK := finalGenerationValue.(int64)
	if _, err := finalMetadata.Consume(ctx); err != nil || !finalGenerationOK || finalGeneration != wantGeneration {
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
			    n.predicate = row.predicate,
			    n.card_endpoint_id = row.card_endpoint_id,
			    n.card_capture_id = row.card_capture_id`,
			map[string]any{"project_key": projectKey, "rows": recordRows(prepared.Records[start:end])},
		)
		if runErr != nil {
			return errors.New("Neo4j graph records could not be written")
		}
		if _, runErr = result.Consume(ctx); runErr != nil {
			return errors.New("Neo4j graph records could not be written")
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
	if len(output.Records) == 0 || len(output.Records) > maxSnapshotRecords || len(output.Refs) > maxSnapshotRecords {
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
			"card_endpoint_id": record.CardEndpointID, "card_capture_id": record.CardCaptureID,
		})
	}
	return rows
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
