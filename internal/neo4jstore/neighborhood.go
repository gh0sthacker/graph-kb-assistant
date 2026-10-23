package neo4jstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	maxNeighborhoodRecords    = 10_000
	maxNeighborhoodBytes      = 16 << 20
	maxNeighborhoodRefs       = 20_000
	maxNeighborhoodEntityRefs = 64
	maxEntityProvenanceBatch  = 100
)

var neighborhoodIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*-[a-f0-9]{64}$`)
var ErrNeighborhoodCursorStale = errors.New("Neo4j neighborhood cursor generation is stale")

type NeighborhoodCursor struct {
	Generation       int64
	Priority         int
	AfterAssertionID string
}

// Neighborhood is an integrity-checked, bounded one-hop projection. It does not
// establish the integrity or completeness of the entire project snapshot.
type Neighborhood struct {
	Snapshot   behaviorgraph.Record
	Truncated  bool
	Generation int64
	// CandidateCount is the observed candidate count, capped at budget + 1.
	CandidateCount int
	// ProvenanceTruncated reports sampled entity provenance, independently of
	// the candidate-edge Truncated flag. Assertion provenance is always complete.
	ProvenanceTruncated bool
	// TruncatedEntityIDs is sorted and identifies every entity whose source_refs
	// are a bounded sample. An unlisted entity had all of its refs read.
	TruncatedEntityIDs []string
	// EntitySourceRefLimit is the per-entity ceiling; shared byte/record/ref
	// budgets can reduce an entity's sample further.
	EntitySourceRefLimit int
	NextCursor           *NeighborhoodCursor
}

// ReadProjectNeighborhood reads seeds and their incident assertions using
// project-scoped identity lookups. Only assertion endpoint dependencies are
// followed recursively; an encountered host never expands into other traffic.
// All records, provenance and assertion topology are checked in one read
// transaction. A dependency closure that exceeds the edge or safety budget
// fails rather than returning dangling assertions.
func (s *Store) ReadProjectNeighborhood(ctx context.Context, projectKey string, seedIDs []string, maxAssertions int) (Neighborhood, error) {
	return s.ReadProjectNeighborhoodPage(ctx, projectKey, seedIDs, maxAssertions, nil)
}

// ReadProjectNeighborhoodPage reads one deterministic candidate page. A
// continuation cursor is bound to the Neo4j project generation and must be
// checked inside the same read transaction as the page query.
func (s *Store) ReadProjectNeighborhoodPage(ctx context.Context, projectKey string, seedIDs []string, maxAssertions int, cursor *NeighborhoodCursor) (Neighborhood, error) {
	seeds, err := neighborhoodSeeds(projectKey, seedIDs, maxAssertions)
	if err != nil {
		return Neighborhood{}, err
	}
	if err := validateNeighborhoodCursor(cursor); err != nil {
		return Neighborhood{}, err
	}
	if s == nil || s.driver == nil {
		return Neighborhood{}, errors.New("Neo4j store is unavailable")
	}
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return readProjectNeighborhoodTx(ctx, tx, projectKey, seeds, maxAssertions, cursor)
	})
	if err != nil {
		return Neighborhood{}, err
	}
	result, ok := value.(Neighborhood)
	if !ok {
		return Neighborhood{}, errors.New("Neo4j neighborhood result is invalid")
	}
	return result, nil
}

func validateNeighborhoodCursor(cursor *NeighborhoodCursor) error {
	if cursor == nil {
		return nil
	}
	if cursor.Generation < 1 || (cursor.Priority != 0 && cursor.Priority != 1) ||
		!neighborhoodIDPattern.MatchString(cursor.AfterAssertionID) || !strings.HasPrefix(cursor.AfterAssertionID, "assertion-") {
		return errors.New("Neo4j neighborhood continuation cursor is malformed")
	}
	return nil
}

func cursorPriority(cursor *NeighborhoodCursor) int {
	if cursor == nil {
		return -1
	}
	return cursor.Priority
}

func cursorAfterID(cursor *NeighborhoodCursor) string {
	if cursor == nil {
		return ""
	}
	return cursor.AfterAssertionID
}

func neighborhoodSeeds(projectKey string, ids []string, budget int) ([]string, error) {
	if !projectKeyPattern.MatchString(projectKey) || len(ids) < 1 || len(ids) > 20 || budget < 1 || budget > 300 {
		return nil, errors.New("Neo4j neighborhood selection is invalid")
	}
	seen := make(map[string]bool, len(ids))
	seeds := make([]string, 0, len(ids))
	for _, id := range ids {
		if !neighborhoodIDPattern.MatchString(id) || strings.HasPrefix(id, "source-") || strings.HasPrefix(id, "card-") || strings.HasPrefix(id, "assertion-") {
			return nil, errors.New("Neo4j neighborhood seed identity is invalid")
		}
		if !seen[id] {
			seen[id] = true
			seeds = append(seeds, id)
		}
	}
	sort.Strings(seeds)
	return seeds, nil
}

type neighborhoodRecord struct {
	id    string
	kind  string
	value behaviorgraph.Record
	// Retaining the checksum makes repeated records detectably immutable.
	hash string
}

type neighborhoodRead struct {
	project           string
	records           map[string]neighborhoodRecord
	bytes             int
	provenanceRows    int
	truncatedEntities map[string]bool
}

func decodeNeighborhoodRecord(id, kind, payload, checksum string) (neighborhoodRecord, error) {
	if !neighborhoodIDPattern.MatchString(id) || len(payload) < 2 || len(payload) > 1<<20 || !projectKeyPattern.MatchString(checksum) {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood record is malformed or exceeds its safe read limit")
	}
	digest := sha256.Sum256([]byte(payload))
	if checksum != hex.EncodeToString(digest[:]) {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood record checksum does not match")
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var decoded behaviorgraph.Record
	if decoder.Decode(&decoded) != nil || decoded == nil {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood record contains invalid JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood record contains trailing JSON")
	}
	switch kind {
	case "entity":
		entityKind, _ := decoded["kind"].(string)
		_, dataOK := decoded["data"].(map[string]any)
		if len(decoded) != 3 || decoded["id"] != id || entityKind == "" || !strings.HasPrefix(id, entityKind+"-") || !dataOK {
			return neighborhoodRecord{}, errors.New("Neo4j neighborhood entity identity or type is inconsistent")
		}
	case "assertion":
		subject, subjectOK := decoded["subject"].(string)
		object, objectOK := decoded["object"].(string)
		_, predicateOK := decoded["predicate"].(string)
		_, contextOK := decoded["context"].(map[string]any)
		_, originOK := decoded["origin_kind"].(string)
		_, evidenceOK := decoded["evidence_state"].(string)
		if len(decoded) != 7 || decoded["id"] != id || !strings.HasPrefix(id, "assertion-") || !subjectOK || !objectOK ||
			!neighborhoodIDPattern.MatchString(subject) || !neighborhoodIDPattern.MatchString(object) || !predicateOK || !contextOK || !originOK || !evidenceOK {
			return neighborhoodRecord{}, errors.New("Neo4j neighborhood assertion identity or type is inconsistent")
		}
	case "card":
		if decoded["id"] != id || !strings.HasPrefix(id, "card-") {
			return neighborhoodRecord{}, errors.New("Neo4j neighborhood analysis card identity is inconsistent")
		}
	case "source":
		path, pathOK := decoded["path"].(string)
		hash, hashOK := decoded["sha256"].(string)
		locator, locatorOK := decoded["record_locator"].(string)
		canonical, err := json.Marshal(decoded)
		sourceDigest := sha256.Sum256(canonical)
		if len(decoded) != 3 || !pathOK || path == "" || !hashOK || !projectKeyPattern.MatchString(hash) || !locatorOK || locator == "" ||
			err != nil || id != "source-"+hex.EncodeToString(sourceDigest[:]) {
			return neighborhoodRecord{}, errors.New("Neo4j neighborhood source identity is inconsistent")
		}
	default:
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood record type is unsupported")
	}
	if behaviorgraph.ValidatePublicData(decoded) != nil {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood record contains unsafe metadata")
	}
	return neighborhoodRecord{id: id, kind: kind, value: decoded, hash: checksum}, nil
}

func (r *neighborhoodRead) add(id, kind, payload, checksum string) (neighborhoodRecord, error) {
	if len(payload) > maxNeighborhoodBytes-r.bytes {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood exceeds its safe byte limit")
	}
	r.bytes += len(payload)
	decoded, err := decodeNeighborhoodRecord(id, kind, payload, checksum)
	if err != nil {
		return neighborhoodRecord{}, err
	}
	if existing, ok := r.records[id]; ok {
		if existing.kind != decoded.kind || existing.hash != decoded.hash {
			return neighborhoodRecord{}, errors.New("Neo4j neighborhood record changed while it was being read")
		}
		return existing, nil
	}
	if len(r.records) >= maxNeighborhoodRecords {
		return neighborhoodRecord{}, errors.New("Neo4j neighborhood exceeds its safe record limit")
	}
	r.records[id] = decoded
	return decoded, nil
}

// neighborhoodPayloadUpperBound converts Neo4j's Unicode-character count to
// a conservative UTF-8 byte bound before payload_json is transferred. Graph
// records are capped at 1 MiB by the writer, so this multiplication is safe.
func neighborhoodPayloadUpperBound(characterCount int64, remainingBytes int) (int, error) {
	if characterCount < 2 || characterCount > 1<<20 || remainingBytes < 0 || characterCount > int64(remainingBytes/4) {
		return 0, errors.New("Neo4j neighborhood exceeds its safe byte limit")
	}
	return int(characterCount) * 4, nil
}

func neighborhoodGeneration(ctx context.Context, tx neo4j.ManagedTransaction, project string) (int64, error) {
	result, err := tx.Run(ctx, `
		MATCH (m:GraphProject {project_key: $project_key})
		RETURN m.schema_version AS schema_version, m.generation AS generation`, map[string]any{"project_key": project})
	if err != nil || !result.Next(ctx) {
		return 0, errors.New("Neo4j neighborhood project metadata is unavailable")
	}
	row := result.Record()
	schemaValue, _ := row.Get("schema_version")
	generationValue, _ := row.Get("generation")
	schema, schemaOK := schemaValue.(int64)
	generation, generationOK := generationValue.(int64)
	if result.Next(ctx) || !schemaOK || schema != 2 || !generationOK || generation < 1 {
		return 0, errors.New("Neo4j neighborhood project metadata is malformed")
	}
	if _, err := result.Consume(ctx); err != nil {
		return 0, errors.New("Neo4j neighborhood project metadata could not be read")
	}
	return generation, nil
}

func (r *neighborhoodRead) load(ctx context.Context, tx neo4j.ManagedTransaction, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	metadata, err := tx.Run(ctx, `
		UNWIND $record_ids AS record_id
		MATCH (n:GraphRecord {project_key: $project_key, record_id: record_id})
		RETURN n.record_id AS record_id, n.record_type AS record_type,
		       size(n.payload_json) AS payload_char_count, n.payload_sha256 AS payload_sha256
		LIMIT $limit`, map[string]any{"project_key": r.project, "record_ids": ids, "limit": len(ids) + 1})
	if err != nil {
		return errors.New("Neo4j neighborhood records are unavailable")
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if wanted[id] {
			return errors.New("Neo4j neighborhood record selection contains duplicates")
		}
		wanted[id] = true
	}
	type recordMeta struct {
		kind, checksum string
		characterCount int64
	}
	selected := make(map[string]recordMeta, len(ids))
	reservedBytes := 0
	for metadata.Next(ctx) {
		row := metadata.Record()
		id, _ := row.Get("record_id")
		kind, _ := row.Get("record_type")
		characters, _ := row.Get("payload_char_count")
		checksum, _ := row.Get("payload_sha256")
		idText, idOK := id.(string)
		kindText, kindOK := kind.(string)
		characterCount, sizeOK := characters.(int64)
		checksumText, checksumOK := checksum.(string)
		if !idOK || !wanted[idText] || selected[idText].kind != "" || !kindOK ||
			(kindText != "entity" && kindText != "assertion" && kindText != "card") || !sizeOK || !checksumOK || !projectKeyPattern.MatchString(checksumText) {
			return errors.New("Neo4j neighborhood record identity or type is inconsistent")
		}
		upperBound, err := neighborhoodPayloadUpperBound(characterCount, maxNeighborhoodBytes-r.bytes-reservedBytes)
		if err != nil {
			return err
		}
		reservedBytes += upperBound
		selected[idText] = recordMeta{kind: kindText, checksum: checksumText, characterCount: characterCount}
		delete(wanted, idText)
	}
	if metadata.Err() != nil {
		return errors.New("Neo4j neighborhood records could not be read")
	}
	if len(wanted) != 0 {
		return errors.New("Neo4j neighborhood seed or assertion endpoint is missing")
	}
	if len(r.records)+len(selected) > maxNeighborhoodRecords {
		return errors.New("Neo4j neighborhood exceeds its safe record limit")
	}
	result, err := tx.Run(ctx, `
		UNWIND $record_ids AS record_id
		MATCH (n:GraphRecord {project_key: $project_key, record_id: record_id})
		RETURN n.record_id AS record_id, n.record_type AS record_type,
		       n.payload_json AS payload_json, n.payload_sha256 AS payload_sha256
		LIMIT $limit`, map[string]any{"project_key": r.project, "record_ids": ids, "limit": len(ids) + 1})
	if err != nil {
		return errors.New("Neo4j neighborhood record payloads are unavailable")
	}
	wanted = make(map[string]bool, len(selected))
	for id := range selected {
		wanted[id] = true
	}
	for result.Next(ctx) {
		row := result.Record()
		id, _ := row.Get("record_id")
		kind, _ := row.Get("record_type")
		payload, _ := row.Get("payload_json")
		checksum, _ := row.Get("payload_sha256")
		idText, idOK := id.(string)
		kindText, kindOK := kind.(string)
		payloadText, payloadOK := payload.(string)
		checksumText, checksumOK := checksum.(string)
		meta, exists := selected[idText]
		if !idOK || !wanted[idText] || !exists || !kindOK || meta.kind != kindText || !payloadOK || !checksumOK || meta.checksum != checksumText || int64(len([]rune(payloadText))) != meta.characterCount {
			return errors.New("Neo4j neighborhood record changed after its payload budget check")
		}
		if _, err := r.add(idText, kindText, payloadText, checksumText); err != nil {
			return err
		}
		delete(wanted, idText)
	}
	if result.Err() != nil || len(wanted) != 0 {
		return errors.New("Neo4j neighborhood record payloads are incomplete")
	}
	return nil
}

func readProjectNeighborhoodTx(ctx context.Context, tx neo4j.ManagedTransaction, project string, seeds []string, budget int, cursor *NeighborhoodCursor) (Neighborhood, error) {
	generation, err := neighborhoodGeneration(ctx, tx, project)
	if err != nil {
		return Neighborhood{}, err
	}
	if cursor != nil && cursor.Generation != generation {
		return Neighborhood{}, ErrNeighborhoodCursorStale
	}
	read := &neighborhoodRead{project: project, records: make(map[string]neighborhoodRecord)}
	if err := read.load(ctx, tx, seeds); err != nil {
		return Neighborhood{}, err
	}
	result, err := tx.Run(ctx, `
		UNWIND $seed_ids AS seed_id
		MATCH (seed:GraphRecord {project_key: $project_key, record_id: seed_id})
		CALL {
			WITH seed
			WITH seed WHERE seed.record_type = 'assertion'
			RETURN seed.record_id AS assertion_id
			UNION
			WITH seed
			MATCH (seed)<-[:SUBJECT|OBJECT {project_key: $project_key}]-(a:GraphRecord {project_key: $project_key, record_type: 'assertion'})
			RETURN a.record_id AS assertion_id
		}
		WITH DISTINCT assertion_id
		WITH assertion_id, CASE WHEN assertion_id IN $seed_ids THEN 0 ELSE 1 END AS page_priority
		WHERE NOT $has_cursor OR page_priority > $after_priority OR
		      (page_priority = $after_priority AND assertion_id > $after_assertion_id)
		RETURN assertion_id, page_priority
		ORDER BY page_priority, assertion_id
		LIMIT $limit`, map[string]any{
		"project_key": project, "seed_ids": seeds, "limit": budget + 1,
		"has_cursor": cursor != nil, "after_priority": cursorPriority(cursor), "after_assertion_id": cursorAfterID(cursor),
	})
	if err != nil {
		return Neighborhood{}, errors.New("Neo4j neighborhood assertions are unavailable")
	}
	candidates := make([]neighborhoodCandidate, 0, budget+1)
	seen := make(map[string]bool)
	for result.Next(ctx) {
		row := result.Record()
		value, _ := row.Get("assertion_id")
		id, ok := value.(string)
		priorityValue, _ := row.Get("page_priority")
		priority, priorityOK := priorityValue.(int64)
		if !ok || !priorityOK || (priority != 0 && priority != 1) || !neighborhoodIDPattern.MatchString(id) || !strings.HasPrefix(id, "assertion-") || seen[id] || len(candidates) >= budget+1 {
			return Neighborhood{}, errors.New("Neo4j neighborhood candidate identity is malformed")
		}
		seen[id] = true
		candidates = append(candidates, neighborhoodCandidate{id: id, priority: int(priority)})
	}
	if result.Err() != nil {
		return Neighborhood{}, errors.New("Neo4j neighborhood assertions could not be read")
	}
	output := Neighborhood{Generation: generation, CandidateCount: len(candidates), Truncated: len(candidates) > budget}
	if output.Truncated {
		candidates = candidates[:budget]
		last := candidates[len(candidates)-1]
		output.NextCursor = &NeighborhoodCursor{Generation: generation, Priority: last.priority, AfterAssertionID: last.id}
	}
	missing := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		id := candidate.id
		if _, exists := read.records[id]; !exists {
			missing = append(missing, id)
		}
	}
	if err := read.load(ctx, tx, missing); err != nil {
		return Neighborhood{}, err
	}
	// Close assertion-to-assertion evidence dependencies without following any
	// incident relationships from newly discovered entities.
	for {
		missingSet := make(map[string]bool)
		assertions := 0
		for _, record := range read.records {
			if record.kind != "assertion" {
				continue
			}
			assertions++
			for _, key := range []string{"subject", "object"} {
				id := record.value[key].(string)
				if _, exists := read.records[id]; !exists {
					missingSet[id] = true
				}
			}
		}
		if assertions > budget {
			return Neighborhood{}, errors.New("Neo4j neighborhood assertion dependencies exceed the edge budget")
		}
		if len(missingSet) == 0 {
			break
		}
		missing = make([]string, 0, len(missingSet))
		for id := range missingSet {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		if err := read.load(ctx, tx, missing); err != nil {
			return Neighborhood{}, err
		}
	}
	assertionIDs, ownerIDs := make([]string, 0), make([]string, 0, len(read.records))
	for id, record := range read.records {
		ownerIDs = append(ownerIDs, id)
		if record.kind == "assertion" {
			assertionIDs = append(assertionIDs, id)
		}
	}
	sort.Strings(assertionIDs)
	sort.Strings(ownerIDs)
	if err := read.checkTopology(ctx, tx, assertionIDs); err != nil {
		return Neighborhood{}, err
	}
	if err := read.loadProvenance(ctx, tx, ownerIDs); err != nil {
		return Neighborhood{}, err
	}
	output.TruncatedEntityIDs = read.truncatedEntityIDs()
	output.ProvenanceTruncated = len(output.TruncatedEntityIDs) != 0
	output.EntitySourceRefLimit = maxNeighborhoodEntityRefs
	entities, assertions, sources := make([]behaviorgraph.Record, 0), make([]behaviorgraph.Record, 0), make([]behaviorgraph.Record, 0)
	for _, record := range read.records {
		switch record.kind {
		case "entity":
			entities = append(entities, record.value)
		case "assertion":
			assertions = append(assertions, record.value)
		case "source":
			sources = append(sources, record.value)
		}
	}
	for _, values := range [][]behaviorgraph.Record{entities, assertions, sources} {
		sort.Slice(values, func(i, j int) bool { return recordSortKey(values[i]) < recordSortKey(values[j]) })
	}
	output.Snapshot = behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"sources": sources, "entities": entities, "assertions": assertions, "cards": []behaviorgraph.Record{},
	}
	if behaviorgraph.ValidatePublicData(output.Snapshot) != nil {
		return Neighborhood{}, errors.New("Neo4j neighborhood contains unsafe metadata")
	}
	finalGeneration, err := neighborhoodGeneration(ctx, tx, project)
	if err != nil || finalGeneration != generation {
		return Neighborhood{}, errors.New("Neo4j neighborhood project changed while it was being read")
	}
	return output, nil
}

func (r *neighborhoodRead) closeAssertionDependencies(ctx context.Context, tx neo4j.ManagedTransaction, budget int) error {
	for {
		missingSet := make(map[string]bool)
		assertions := 0
		for _, record := range r.records {
			if record.kind != "assertion" {
				continue
			}
			assertions++
			for _, key := range []string{"subject", "object"} {
				id, ok := record.value[key].(string)
				if !ok || !neighborhoodIDPattern.MatchString(id) {
					return errors.New("Neo4j analytical assertion dependency is malformed")
				}
				if _, exists := r.records[id]; !exists {
					missingSet[id] = true
				}
			}
		}
		if assertions > budget {
			return errors.New("Neo4j analytical assertion dependencies exceed the edge budget")
		}
		if len(missingSet) == 0 {
			return nil
		}
		missing := make([]string, 0, len(missingSet))
		for id := range missingSet {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		if err := r.load(ctx, tx, missing); err != nil {
			return err
		}
	}
}

type neighborhoodCandidate struct {
	id       string
	priority int
}

func (r *neighborhoodRead) checkTopology(ctx context.Context, tx neo4j.ManagedTransaction, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	result, err := tx.Run(ctx, `
		UNWIND $assertion_ids AS assertion_id
		MATCH (a:GraphRecord {project_key: $project_key, record_id: assertion_id, record_type: 'assertion'})
		OPTIONAL MATCH (a)-[r:SUBJECT|OBJECT {project_key: $project_key}]->(endpoint:GraphRecord {project_key: $project_key})
		RETURN a.record_id AS assertion_id, type(r) AS role, endpoint.record_id AS endpoint_id,
		       EXISTS {
		           MATCH (a)-[other:SUBJECT|OBJECT]->(foreign)
		           WHERE other.project_key IS NULL OR other.project_key <> $project_key
		              OR foreign.project_key IS NULL OR foreign.project_key <> $project_key OR NOT foreign:GraphRecord
		       } AS invalid_links
		LIMIT $limit`, map[string]any{"project_key": r.project, "assertion_ids": ids, "limit": 2*len(ids) + 1})
	if err != nil {
		return errors.New("Neo4j neighborhood assertion topology is unavailable")
	}
	seen := make(map[string]map[string]string, len(ids))
	for result.Next(ctx) {
		row := result.Record()
		id, _ := row.Get("assertion_id")
		role, _ := row.Get("role")
		endpoint, _ := row.Get("endpoint_id")
		invalid, _ := row.Get("invalid_links")
		idText, _ := id.(string)
		roleText, _ := role.(string)
		endpointText, _ := endpoint.(string)
		invalidBool, validBool := invalid.(bool)
		if !validBool || invalidBool || !validNeighborhoodTopology(r.records, seen, idText, roleText, endpointText) {
			return errors.New("Neo4j neighborhood assertion topology is inconsistent")
		}
	}
	if result.Err() != nil || len(seen) != len(ids) {
		return errors.New("Neo4j neighborhood assertion topology is incomplete")
	}
	for _, id := range ids {
		if len(seen[id]) != 2 {
			return errors.New("Neo4j neighborhood assertion topology is incomplete")
		}
	}
	return nil
}

func validNeighborhoodTopology(records map[string]neighborhoodRecord, seen map[string]map[string]string, id, role, endpoint string) bool {
	assertion, exists := records[id]
	if !exists || assertion.kind != "assertion" || (role != "SUBJECT" && role != "OBJECT") {
		return false
	}
	if _, exists := records[endpoint]; !exists || assertion.value[strings.ToLower(role)] != endpoint {
		return false
	}
	if seen[id] == nil {
		seen[id] = make(map[string]string, 2)
	}
	if _, duplicate := seen[id][role]; duplicate {
		return false
	}
	seen[id][role] = endpoint
	return true
}

func (r *neighborhoodRead) loadProvenance(ctx context.Context, tx neo4j.ManagedTransaction, ids []string) error {
	assertions, entities := make([]string, 0), make([]string, 0)
	for _, id := range ids {
		switch r.records[id].kind {
		case "assertion", "card":
			assertions = append(assertions, id)
		case "entity":
			entities = append(entities, id)
		default:
			return errors.New("Neo4j neighborhood provenance owner is invalid")
		}
	}
	sort.Strings(assertions)
	sort.Strings(entities)
	// Assertions and cards keep complete source_refs. Reserve those before
	// sampling entity refs, which do not participate in entity identity.
	if err := r.loadAssertionProvenance(ctx, tx, assertions); err != nil {
		return err
	}
	for offset := 0; offset < len(entities); offset += maxEntityProvenanceBatch {
		end := min(offset+maxEntityProvenanceBatch, len(entities))
		remaining := len(entities) - offset
		limit := min(maxNeighborhoodEntityRefs, (maxNeighborhoodRefs-r.provenanceRows)/remaining-1)
		if limit < 1 {
			return errors.New("Neo4j neighborhood has no safe provenance budget for its entities")
		}
		byteBudget := (maxNeighborhoodBytes - r.bytes) / remaining
		recordBudget := max(1, (maxNeighborhoodRecords-len(r.records))/remaining)
		if err := r.loadEntityProvenanceBatch(ctx, tx, entities[offset:end], min(limit, recordBudget), byteBudget); err != nil {
			return err
		}
	}
	return nil
}

func (r *neighborhoodRead) truncatedEntityIDs() []string {
	ids := make([]string, 0, len(r.truncatedEntities))
	for id := range r.truncatedEntities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *neighborhoodRead) markEntityProvenanceTruncated(id string) {
	if r.truncatedEntities == nil {
		r.truncatedEntities = make(map[string]bool)
	}
	r.truncatedEntities[id] = true
}

type neighborhoodPayloadMeta struct {
	kind, checksum string
	characterCount int64
}

func (r *neighborhoodRead) loadAssertionProvenance(ctx context.Context, tx neo4j.ManagedTransaction, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	metadata, err := tx.Run(ctx, `
		UNWIND $owner_ids AS owner_id
		MATCH (owner:GraphRecord {project_key: $project_key, record_id: owner_id})
		OPTIONAL MATCH (owner)-[:EVIDENCED_BY {project_key: $project_key}]->(source:GraphRecord {project_key: $project_key, record_type: 'source'})
		RETURN owner.record_id AS owner_id, source.record_id AS record_id, source.record_type AS record_type,
		       size(source.payload_json) AS payload_char_count, source.payload_sha256 AS payload_sha256,
		       EXISTS {
		           MATCH (owner)-[other:EVIDENCED_BY]->(foreign)
		           WHERE other.project_key IS NULL OR other.project_key <> $project_key
		              OR foreign.project_key IS NULL OR foreign.project_key <> $project_key OR NOT foreign:GraphRecord
		              OR foreign.record_type IS NULL OR foreign.record_type <> 'source'
		       } AS invalid_links
		LIMIT $limit`, map[string]any{"project_key": r.project, "owner_ids": ids, "limit": maxNeighborhoodRefs + 1})
	if err != nil {
		return errors.New("Neo4j neighborhood provenance is unavailable")
	}
	refs := make(map[string]map[string]neighborhoodPayloadMeta, len(ids))
	sources := make(map[string]neighborhoodPayloadMeta)
	count := 0
	reservedBytes := 0
	for metadata.Next(ctx) {
		row := metadata.Record()
		owner, _ := row.Get("owner_id")
		id, _ := row.Get("record_id")
		kind, _ := row.Get("record_type")
		characters, _ := row.Get("payload_char_count")
		checksum, _ := row.Get("payload_sha256")
		invalid, _ := row.Get("invalid_links")
		ownerText, ownerOK := owner.(string)
		idText, idOK := id.(string)
		kindText, _ := kind.(string)
		characterCount, sizeOK := characters.(int64)
		checksumText, checksumOK := checksum.(string)
		invalidBool, validBool := invalid.(bool)
		ownerRecord, ownerExists := r.records[ownerText]
		count++
		r.provenanceRows++
		if count > maxNeighborhoodRefs || r.provenanceRows > maxNeighborhoodRefs || !ownerOK || !ownerExists || ownerRecord.kind == "source" ||
			!idOK || !neighborhoodIDPattern.MatchString(idText) || !strings.HasPrefix(idText, "source-") || kindText != "source" ||
			!sizeOK || !checksumOK || !projectKeyPattern.MatchString(checksumText) || idText != "source-"+checksumText || !validBool || invalidBool {
			return errors.New("Neo4j neighborhood provenance is malformed or exceeds its safe read limit")
		}
		if refs[ownerText] == nil {
			refs[ownerText] = make(map[string]neighborhoodPayloadMeta)
		}
		if _, duplicate := refs[ownerText][idText]; duplicate {
			return errors.New("Neo4j neighborhood provenance contains duplicate links")
		}
		meta := neighborhoodPayloadMeta{kind: kindText, checksum: checksumText, characterCount: characterCount}
		refs[ownerText][idText] = meta
		if existing, ok := sources[idText]; ok && existing != meta {
			return errors.New("Neo4j neighborhood source metadata is inconsistent")
		}
		sources[idText] = meta
	}
	if metadata.Err() != nil || len(refs) != len(ids) {
		return errors.New("Neo4j neighborhood provenance is incomplete")
	}
	missing := make([]string, 0, len(sources))
	missingMeta := make(map[string]neighborhoodPayloadMeta, len(sources))
	for id, meta := range sources {
		if existing, ok := r.records[id]; ok {
			if existing.kind != "source" || existing.hash != meta.checksum {
				return errors.New("Neo4j neighborhood source changed while it was being read")
			}
			continue
		}
		upperBound, err := neighborhoodPayloadUpperBound(meta.characterCount, maxNeighborhoodBytes-r.bytes-reservedBytes)
		if err != nil {
			return err
		}
		reservedBytes += upperBound
		missing = append(missing, id)
		missingMeta[id] = meta
	}
	if len(r.records)+len(missing) > maxNeighborhoodRecords {
		return errors.New("Neo4j neighborhood exceeds its safe record limit")
	}
	sort.Strings(missing)
	if err := r.loadSources(ctx, tx, missing, missingMeta); err != nil {
		return err
	}
	for _, id := range ids {
		ownerRefs := make([]behaviorgraph.Record, 0, len(refs[id]))
		for sourceID := range refs[id] {
			source, exists := r.records[sourceID]
			if !exists || source.kind != "source" {
				return errors.New("Neo4j neighborhood provenance source is missing")
			}
			ownerRefs = append(ownerRefs, source.value)
		}
		// The writer canonicalizes ref order before hashing assertion identity.
		sort.Slice(ownerRefs, func(i, j int) bool { return neighborhoodSourceKey(ownerRefs[i]) < neighborhoodSourceKey(ownerRefs[j]) })
		r.records[id].value["source_refs"] = ownerRefs
	}
	return nil
}

func neighborhoodSourceKey(source behaviorgraph.Record) string {
	var key strings.Builder
	encoder := json.NewEncoder(&key)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(source) // Source values have already passed JSON validation.
	return key.String()
}

// loadEntityProvenance retains the single-owner test seam while production
// reads use loadEntityProvenanceBatch to avoid one Bolt round trip per entity.
func (r *neighborhoodRead) loadEntityProvenance(ctx context.Context, tx neo4j.ManagedTransaction, id string, limit, byteBudget int) error {
	return r.loadEntityProvenanceBatch(ctx, tx, []string{id}, limit, byteBudget)
}

func (r *neighborhoodRead) loadEntityProvenanceBatch(ctx context.Context, tx neo4j.ManagedTransaction, ids []string, limit, byteBudget int) error {
	if len(ids) == 0 || len(ids) > maxEntityProvenanceBatch || limit < 1 || byteBudget < 0 {
		return errors.New("Neo4j neighborhood entity provenance batch is invalid")
	}
	// Read source IDs and sizes for a bounded owner batch before fetching
	// payloads. A high-degree entity or oversized source must not exceed the
	// cumulative record and byte budgets.
	result, err := tx.Run(ctx, `
		UNWIND $owner_ids AS owner_id
		CALL {
			WITH owner_id
			MATCH (owner:GraphRecord {project_key: $project_key, record_id: owner_id, record_type: 'entity'})
			OPTIONAL MATCH (owner)-[:EVIDENCED_BY {project_key: $project_key}]->(source:GraphRecord {project_key: $project_key, record_type: 'source'})
			RETURN owner.record_id AS matched_owner_id, source.record_id AS record_id, source.record_type AS record_type,
			       size(source.payload_json) AS payload_char_count, source.payload_sha256 AS payload_sha256,
			       EXISTS {
			           MATCH (owner)-[other:EVIDENCED_BY]->(foreign)
			           WHERE other.project_key IS NULL OR other.project_key <> $project_key
			              OR foreign.project_key IS NULL OR foreign.project_key <> $project_key OR NOT foreign:GraphRecord
			              OR foreign.record_type IS NULL OR foreign.record_type <> 'source'
			       } AS invalid_links
			ORDER BY source.record_id
			LIMIT $limit_each
		}
		RETURN matched_owner_id AS owner_id, record_id, record_type, payload_char_count, payload_sha256, invalid_links
		ORDER BY owner_id, record_id
		LIMIT $limit`, map[string]any{"project_key": r.project, "owner_ids": ids, "limit_each": limit + 1, "limit": len(ids)*(limit+1) + 1})
	if err != nil {
		return errors.New("Neo4j neighborhood entity provenance is unavailable")
	}
	wanted := make(map[string]bool, len(ids))
	seen := make(map[string]map[string]bool, len(ids))
	selected := make(map[string][]string, len(ids))
	reservedBytes := make(map[string]int, len(ids))
	missingMeta := make(map[string]neighborhoodPayloadMeta)
	for _, id := range ids {
		owner, exists := r.records[id]
		if !exists || owner.kind != "entity" || wanted[id] {
			return errors.New("Neo4j neighborhood entity provenance owner is invalid")
		}
		wanted[id] = true
		seen[id] = make(map[string]bool, limit+1)
	}
	rowLimit := len(ids) * (limit + 1)
	rows := 0
	for result.Next(ctx) {
		row := result.Record()
		owner, _ := row.Get("owner_id")
		sourceID, _ := row.Get("record_id")
		kind, _ := row.Get("record_type")
		characters, _ := row.Get("payload_char_count")
		checksum, _ := row.Get("payload_sha256")
		invalid, _ := row.Get("invalid_links")
		ownerText, ownerOK := owner.(string)
		sourceText, sourceOK := sourceID.(string)
		characterCount, sizeOK := characters.(int64)
		checksumText, checksumOK := checksum.(string)
		invalidBool, validBool := invalid.(bool)
		r.provenanceRows++
		rows++
		ownerRecord, ownerExists := r.records[ownerText]
		if !ownerOK || !wanted[ownerText] || !ownerExists || ownerRecord.kind != "entity" || !sourceOK ||
			!neighborhoodIDPattern.MatchString(sourceText) || !strings.HasPrefix(sourceText, "source-") || kind != "source" ||
			!sizeOK || characterCount < 2 || !checksumOK || !projectKeyPattern.MatchString(checksumText) || !validBool || invalidBool ||
			seen[ownerText][sourceText] || r.provenanceRows > maxNeighborhoodRefs || rows > rowLimit {
			return errors.New("Neo4j neighborhood entity provenance is malformed")
		}
		seen[ownerText][sourceText] = true
		if len(seen[ownerText]) > limit {
			r.markEntityProvenanceTruncated(ownerText)
			continue
		}
		if existing, ok := r.records[sourceText]; ok {
			if existing.kind != "source" || existing.hash != checksumText {
				return errors.New("Neo4j neighborhood source changed while it was being read")
			}
		} else if pending, ok := missingMeta[sourceText]; ok {
			if pending.checksum != checksumText || pending.characterCount != characterCount || pending.kind != "source" {
				return errors.New("Neo4j neighborhood source metadata is inconsistent")
			}
		} else {
			// Neo4j size(string) counts Unicode characters; four bytes per
			// character is a conservative UTF-8 bound before loading the payload.
			if characterCount > int64((byteBudget-reservedBytes[ownerText])/4) || len(r.records)+len(missingMeta) >= maxNeighborhoodRecords {
				r.markEntityProvenanceTruncated(ownerText)
				continue
			}
			reservedBytes[ownerText] += 4 * int(characterCount)
			missingMeta[sourceText] = neighborhoodPayloadMeta{kind: "source", checksum: checksumText, characterCount: characterCount}
		}
		selected[ownerText] = append(selected[ownerText], sourceText)
	}
	if result.Err() != nil || rows > rowLimit || len(seen) != len(ids) {
		return errors.New("Neo4j neighborhood entity provenance is incomplete")
	}
	missing := make([]string, 0, len(missingMeta))
	for id := range missingMeta {
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if err := r.loadSources(ctx, tx, missing, missingMeta); err != nil {
		return err
	}
	for _, ownerID := range ids {
		refs := make([]behaviorgraph.Record, 0, len(selected[ownerID]))
		for _, sourceID := range selected[ownerID] {
			source, exists := r.records[sourceID]
			if !exists || source.kind != "source" {
				return errors.New("Neo4j neighborhood entity provenance source is missing")
			}
			refs = append(refs, source.value)
		}
		if len(refs) == 0 {
			return errors.New("Neo4j neighborhood entity provenance is incomplete")
		}
		r.records[ownerID].value["source_refs"] = refs
	}
	return nil
}

func (r *neighborhoodRead) loadSources(ctx context.Context, tx neo4j.ManagedTransaction, ids []string, expected map[string]neighborhoodPayloadMeta) error {
	if len(ids) == 0 {
		return nil
	}
	result, err := tx.Run(ctx, `
		UNWIND $source_ids AS source_id
		MATCH (source:GraphRecord {project_key: $project_key, record_id: source_id, record_type: 'source'})
		RETURN source.record_id AS record_id, source.record_type AS record_type,
		       source.payload_json AS payload_json, source.payload_sha256 AS payload_sha256
		LIMIT $limit`, map[string]any{"project_key": r.project, "source_ids": ids, "limit": len(ids) + 1})
	if err != nil {
		return errors.New("Neo4j neighborhood source records are unavailable")
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	for result.Next(ctx) {
		row := result.Record()
		id, _ := row.Get("record_id")
		kind, _ := row.Get("record_type")
		payload, _ := row.Get("payload_json")
		checksum, _ := row.Get("payload_sha256")
		idText, idOK := id.(string)
		payloadText, payloadOK := payload.(string)
		checksumText, checksumOK := checksum.(string)
		meta, exists := expected[idText]
		if !idOK || !wanted[idText] || !exists || kind != "source" || meta.kind != "source" || !payloadOK || !checksumOK ||
			meta.checksum != checksumText || int64(len([]rune(payloadText))) != meta.characterCount {
			return errors.New("Neo4j neighborhood source record is malformed")
		}
		if _, err := r.add(idText, "source", payloadText, checksumText); err != nil {
			return err
		}
		delete(wanted, idText)
	}
	if result.Err() != nil || len(wanted) != 0 {
		return errors.New("Neo4j neighborhood source records are incomplete")
	}
	return nil
}
