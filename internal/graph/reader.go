package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"bb-graph-assistant/internal/publicdata"

	_ "modernc.org/sqlite"
)

const (
	maxGraphRows           = 200_000
	MaxVariantObservations = 200_000
	maxGraphBytes          = 256 << 20
	maxRecordBytes         = 1 << 20
	maxReceiptBytes        = 4 << 20
	maxSliceReceiptBytes   = 32 << 20
	maxSliceSourceRefs     = 1_000
)

var (
	graphDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	graphIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*-[a-f0-9]{64}$`)
	jsonIntegerPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	classifierCategory = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	classifierASVS     = regexp.MustCompile(`^V[0-9V./]{1,32}$`)
	allowedKinds       = map[string]struct{}{
		"asset": {}, "endpoint": {}, "operation": {}, "actor": {}, "session_context": {}, "object": {},
		"observation": {}, "parameter": {}, "condition": {}, "flow": {},
		"field_occurrence": {}, "value_reference": {},
		"classifier_signal": {}, "hypothesis": {}, "request_variant": {}, "response_variant": {},
	}
)

// Record is one source-pinned BehaviorGraph JSON record.
type Record map[string]any

// Reader opens an existing BehaviorGraph database without migrations or writes.
// The graph rows are bounded before traversal so malformed or unexpectedly
// large local databases cannot turn a single slice request into an unbounded
// allocation.
type Reader struct {
	db                   *sql.DB
	program              string
	schema               int
	receiptRoot          string
	sourceBytesRead      int64
	sourceChecks         int
	sourceBudgetExceeded bool
	sourceIntegrityCache map[string]string
	entities             map[string]Record
	assertions           map[string]Record
	cards                []Record
	sources              []Record
	sourceByPair         map[string]Record
}

// OpenReader opens a project-local BehaviorGraph read-only. The receipt root
// constrains which source files may be hashed for freshness information.
func OpenReader(ctx context.Context, path, program, receiptRoot string) (*Reader, error) {
	if !graphDigestPattern.MatchString(program) {
		return nil, errors.New("selected graph project key is invalid")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("graph database path is invalid")
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("BehaviorGraph database is unavailable")
	}
	root, err := filepath.Abs(receiptRoot)
	if err != nil {
		return nil, errors.New("graph receipt directory is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("graph receipt directory is unavailable")
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil || rootResolved != root {
		return nil, errors.New("graph receipt directory is unavailable")
	}
	dbURL := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", dbURL)
	if err != nil {
		return nil, errors.New("BehaviorGraph database could not be opened")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	reader := &Reader{
		db: db, program: program, receiptRoot: root,
		entities: make(map[string]Record), assertions: make(map[string]Record),
		sourceByPair: make(map[string]Record),
	}
	if err := reader.load(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return reader, nil
}

// OpenReaderFromSnapshot applies the same schema, provenance, privacy, receipt,
// and record validation used for SQLite data to a backend snapshot. The
// temporary in-memory database is never persisted and contains sanitized graph
// metadata only.
func OpenReaderFromSnapshot(ctx context.Context, snapshot Record, program, receiptRoot string) (*Reader, error) {
	if !graphDigestPattern.MatchString(program) || snapshot["program"] != program || !jsonIntegerEquals(snapshot["schema_version"], 2) {
		return nil, errors.New("selected graph snapshot identity is invalid")
	}
	root, err := filepath.Abs(receiptRoot)
	if err != nil {
		return nil, errors.New("graph receipt directory is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("graph receipt directory is unavailable")
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil || rootResolved != root {
		return nil, errors.New("graph receipt directory is unavailable")
	}
	if err := validatePublic(snapshot); err != nil {
		return nil, errors.New("graph snapshot contains unsafe metadata")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, errors.New("graph snapshot validation store could not be opened")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	failed := true
	defer func() {
		if failed {
			_ = db.Close()
		}
	}()
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("graph snapshot validation store could not enable references: %w", err)
	}
	if err := createV2Schema(ctx, db, program); err != nil {
		return nil, err
	}
	for _, table := range []string{"sources", "entities", "assertions", "cards"} {
		records, ok := snapshotRecordList(snapshot[table])
		if !ok || len(records) > maxGraphRows {
			return nil, errors.New("graph snapshot record list is invalid")
		}
		for _, record := range records {
			if table == "entities" {
				refs, ok := snapshotRecordList(record["source_refs"])
				if !ok || len(refs) == 0 {
					return nil, errors.New("graph snapshot entity provenance is invalid")
				}
				base := cloneRecord(record)
				delete(base, "source_refs")
				id, _ := base["id"].(string)
				payload, encodeErr := canonicalJSON(base)
				if encodeErr != nil || id == "" {
					return nil, errors.New("graph snapshot entity is malformed")
				}
				if _, insertErr := db.ExecContext(ctx, "INSERT INTO entities(id,payload) VALUES(?,?)", id, string(payload)); insertErr != nil {
					return nil, fmt.Errorf("graph snapshot entity could not be loaded: %w", insertErr)
				}
				for _, ref := range refs {
					added := 0
					if err := insertEntityRef(ctx, db, id, ref, &added); err != nil {
						return nil, err
					}
				}
				continue
			}
			id := ""
			switch table {
			case "sources":
				id = sourceRefDigest(record)
			case "assertions", "cards":
				id, _ = record["id"].(string)
			}
			inserted := 0
			if id == "" || insertImmutable(ctx, db, table, id, record, &inserted) != nil {
				return nil, errors.New("graph snapshot record could not be loaded")
			}
		}
	}
	reader := &Reader{
		db: db, program: program, receiptRoot: root,
		entities: make(map[string]Record), assertions: make(map[string]Record),
		sourceByPair: make(map[string]Record),
	}
	if err := reader.load(ctx); err != nil {
		return nil, err
	}
	failed = false
	return reader, nil
}

func snapshotRecordList(value any) ([]Record, bool) {
	switch records := value.(type) {
	case []Record:
		return records, true
	case []any:
		result := make([]Record, 0, len(records))
		for _, item := range records {
			switch record := item.(type) {
			case map[string]any:
				result = append(result, Record(record))
			case Record:
				result = append(result, record)
			default:
				return nil, false
			}
		}
		return result, true
	default:
		return nil, false
	}
}

// Close releases the read-only graph connection.
func (r *Reader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Program returns the namespace validated from BehaviorGraph metadata.
func (r *Reader) Program() string { return r.program }

// ValidatePublicData applies the graph's private-data and secret-like value
// checks before a validated graph snapshot crosses a storage boundary.
func ValidatePublicData(value any) error { return validatePublic(value) }

// Snapshot returns a bounded, validated representation of the complete graph
// projection. It includes entity provenance joined from schema-v2's normalized
// reference table so another backend can be rebuilt without reading captures.
func (r *Reader) Snapshot() (Record, error) {
	if r == nil || r.db == nil || r.program == "" {
		return nil, errors.New("BehaviorGraph reader is unavailable")
	}
	entities := make([]Record, 0, len(r.entities))
	for _, record := range r.entities {
		entities = append(entities, cloneRecord(record))
	}
	assertions := make([]Record, 0, len(r.assertions))
	for _, record := range r.assertions {
		assertions = append(assertions, cloneRecord(record))
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i]["id"].(string) < entities[j]["id"].(string) })
	sort.Slice(assertions, func(i, j int) bool { return assertions[i]["id"].(string) < assertions[j]["id"].(string) })
	cards := make([]Record, len(r.cards))
	for i, record := range r.cards {
		cards[i] = cloneRecord(record)
	}
	sources := make([]Record, len(r.sources))
	for i, record := range r.sources {
		sources[i] = cloneRecord(record)
	}
	snapshot := Record{
		"schema_version": json.Number("2"),
		"program":        r.program,
		"sources":        sources,
		"entities":       entities,
		"assertions":     assertions,
		"cards":          cards,
	}
	encoded, err := canonicalJSON(snapshot)
	if err != nil || len(encoded) > maxGraphBytes {
		return nil, errors.New("BehaviorGraph snapshot exceeds its safe size limit")
	}
	return snapshot, nil
}

// SnapshotDigest returns a stable digest for a complete snapshot independent
// of map iteration or backend query order.
func SnapshotDigest(snapshot Record) (string, error) {
	copySnapshot := cloneRecord(snapshot)
	for _, key := range []string{"sources", "entities", "assertions", "cards"} {
		originals, ok := copySnapshot[key].([]Record)
		if !ok {
			return "", errors.New("graph snapshot records are malformed")
		}
		records := make([]Record, len(originals))
		for i, original := range originals {
			records[i] = cloneRecord(original)
		}
		for _, record := range records {
			if refs, exists := record["source_refs"]; exists {
				refRecords, ok := refs.([]Record)
				if !ok {
					return "", errors.New("graph snapshot provenance is malformed")
				}
				record["source_refs"] = sortedUniqueRefs(refRecords)
			}
		}
		sort.Slice(records, func(i, j int) bool {
			if key == "sources" {
				return sourceRefDigest(records[i]) < sourceRefDigest(records[j])
			}
			return fmt.Sprint(records[i]["id"]) < fmt.Sprint(records[j]["id"])
		})
		copySnapshot[key] = records
	}
	encoded, err := canonicalJSON(copySnapshot)
	if err != nil {
		return "", errors.New("graph snapshot digest could not be computed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ObservationIDsByVariantSignatures finds observations connected to exact
// project-local request and/or response variant signatures. The result is
// bounded and deterministic so callers can join it to their capture index.
func (r *Reader) ObservationIDsByVariantSignatures(requestSignature, responseSignature string, limit int) ([]string, bool, error) {
	if r == nil || r.db == nil || limit < 1 || limit > maxGraphRows ||
		(requestSignature == "" && responseSignature == "") ||
		(requestSignature != "" && !graphDigestPattern.MatchString(requestSignature)) ||
		(responseSignature != "" && !graphDigestPattern.MatchString(responseSignature)) {
		return nil, false, errors.New("variant signature lookup is invalid")
	}
	requestMatches := make(map[string]struct{})
	responseMatches := make(map[string]struct{})
	for _, assertion := range r.assertions {
		predicate, _ := assertion["predicate"].(string)
		if predicate != "uses_request_variant" && predicate != "received_response" {
			continue
		}
		subject, _ := assertion["subject"].(string)
		observation, exists := r.entities[subject]
		if !exists || observation["kind"] != "observation" {
			continue
		}
		objectID, _ := assertion["object"].(string)
		variant, exists := r.entities[objectID]
		if !exists {
			continue
		}
		data, _ := variant["data"].(map[string]any)
		switch predicate {
		case "uses_request_variant":
			if requestSignature != "" && variant["kind"] == "request_variant" && data["request_signature"] == requestSignature {
				requestMatches[subject] = struct{}{}
			}
		case "received_response":
			if responseSignature != "" && variant["kind"] == "response_variant" && data["response_signature"] == responseSignature {
				responseMatches[subject] = struct{}{}
			}
		}
	}

	selected := requestMatches
	if requestSignature == "" {
		selected = responseMatches
	} else if responseSignature != "" {
		for observationID := range selected {
			if _, exists := responseMatches[observationID]; !exists {
				delete(selected, observationID)
			}
		}
	}
	observationIDs := make([]string, 0, len(selected))
	for observationID := range selected {
		observationIDs = append(observationIDs, observationID)
	}
	sort.Strings(observationIDs)
	truncated := len(observationIDs) > limit
	if truncated {
		observationIDs = observationIDs[:limit]
	}
	return observationIDs, truncated, nil
}

func (r *Reader) load(ctx context.Context) error {
	var schemaVersion string
	var program string
	metadata, err := r.db.QueryContext(ctx, "SELECT key,value FROM metadata ORDER BY key")
	if err != nil {
		return errors.New("BehaviorGraph metadata is unavailable")
	}
	seenMetadata := map[string]bool{}
	for metadata.Next() {
		var key, value string
		if err := metadata.Scan(&key, &value); err != nil || seenMetadata[key] {
			_ = metadata.Close()
			return errors.New("BehaviorGraph metadata is malformed")
		}
		seenMetadata[key] = true
		switch key {
		case "schema_version":
			schemaVersion = value
		case "program":
			program = value
		default:
			_ = metadata.Close()
			return errors.New("BehaviorGraph metadata has unsupported fields")
		}
	}
	if err := metadata.Err(); err != nil {
		_ = metadata.Close()
		return errors.New("BehaviorGraph metadata could not be read")
	}
	if err := metadata.Close(); err != nil || len(seenMetadata) != 2 {
		return errors.New("BehaviorGraph metadata is malformed")
	}
	if program != r.program {
		return errors.New("BehaviorGraph belongs to another project")
	}
	if schemaVersion != "1" && schemaVersion != "2" {
		return errors.New("BehaviorGraph schema is unsupported")
	}
	if schemaVersion == "1" {
		r.schema = 1
	} else {
		r.schema = 2
	}
	for _, table := range []string{"sources", "entities", "assertions", "cards"} {
		if err := r.requireTable(ctx, table); err != nil {
			return err
		}
	}
	if r.schema == 2 {
		if err := r.requireTable(ctx, "entity_source_refs"); err != nil {
			return err
		}
	}

	var totalBytes int64
	load := func(table string) ([]Record, error) {
		return r.loadRecords(ctx, table, &totalBytes)
	}
	var errLoad error
	r.sources, errLoad = load("sources")
	if errLoad != nil {
		return errLoad
	}
	for _, source := range r.sources {
		if err := validateSource(source); err != nil {
			return errors.New("BehaviorGraph contains invalid source provenance")
		}
		key := sourcePair(source)
		// Multiple receipt locators may share one immutable path/hash pair.
		if _, exists := r.sourceByPair[key]; !exists {
			r.sourceByPair[key] = source
		}
	}
	entities, err := load("entities")
	if err != nil {
		return err
	}
	refsByEntity := map[string][]Record{}
	if r.schema == 2 {
		refsByEntity, err = r.loadEntityRefs(ctx, &totalBytes)
		if err != nil {
			return err
		}
	}
	for _, record := range entities {
		if err := validateEntity(record, r.schema, r.program); err != nil {
			return errors.New("BehaviorGraph contains a malformed entity")
		}
		if err := validateEntityContext(record, r.program); err != nil {
			return errors.New("BehaviorGraph contains an invalid entity context")
		}
		id := record["id"].(string)
		if r.schema == 2 {
			if _, exists := record["source_refs"]; exists {
				return errors.New("BehaviorGraph schema 2 entity provenance is malformed")
			}
			record["source_refs"] = refsByEntity[id]
		} else {
			refs, ok := record["source_refs"].([]any)
			if !ok {
				return errors.New("BehaviorGraph schema 1 entity provenance is malformed")
			}
			record["source_refs"] = recordsFromAny(refs)
		}
		if err := validateRefs(record["source_refs"], r.sourceByPair); err != nil {
			return errors.New("BehaviorGraph entity provenance is invalid")
		}
		if _, exists := r.entities[id]; exists {
			return errors.New("BehaviorGraph contains duplicate entity IDs")
		}
		if err := validatePublic(record); err != nil {
			return errors.New("BehaviorGraph contains unsafe graph metadata")
		}
		r.entities[id] = record
	}
	for id := range refsByEntity {
		if _, ok := r.entities[id]; !ok {
			return errors.New("BehaviorGraph has provenance for an unknown entity")
		}
	}
	assertions, err := load("assertions")
	if err != nil {
		return err
	}
	for _, record := range assertions {
		if err := validateAssertion(record, r.sourceByPair, r.program); err != nil {
			return errors.New("BehaviorGraph contains a malformed assertion")
		}
		id := record["id"].(string)
		if _, exists := r.assertions[id]; exists {
			return errors.New("BehaviorGraph contains duplicate assertion IDs")
		}
		if err := validatePublic(record); err != nil {
			return errors.New("BehaviorGraph contains unsafe graph metadata")
		}
		record["source_refs"] = recordsFromAny(record["source_refs"].([]any))
		r.assertions[id] = record
	}
	for _, assertion := range r.assertions {
		for _, endpoint := range []string{assertion["subject"].(string), assertion["object"].(string)} {
			if _, ok := r.entities[endpoint]; !ok {
				if _, ok := r.assertions[endpoint]; !ok {
					return errors.New("BehaviorGraph contains a dangling assertion reference")
				}
				if assertion["predicate"] != "supports" && assertion["predicate"] != "contradicts" {
					return errors.New("BehaviorGraph contains an invalid evidence reference")
				}
			}
		}
	}
	r.cards, err = load("cards")
	if err != nil {
		return err
	}
	for _, card := range r.cards {
		if card["program"] != r.program {
			return errors.New("BehaviorGraph card belongs to another project")
		}
		if err := validateCard(card, r.sourceByPair, r.program); err != nil {
			return errors.New("BehaviorGraph contains a malformed analysis card")
		}
		if endpoint := nestedString(card, "endpoint", "id"); endpoint != "" {
			if _, ok := r.entities[endpoint]; !ok {
				return errors.New("BehaviorGraph card references an unknown endpoint")
			}
		}
		if err := validatePublic(card); err != nil {
			return errors.New("BehaviorGraph contains unsafe graph metadata")
		}
		card["source_refs"] = recordsFromAny(card["source_refs"].([]any))
	}
	return nil
}

func (r *Reader) requireTable(ctx context.Context, name string) error {
	var count int
	if err := r.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", name).Scan(&count); err != nil || count != 1 {
		return errors.New("BehaviorGraph is missing a required table")
	}
	return nil
}

func (r *Reader) loadRecords(ctx context.Context, table string, totalBytes *int64) ([]Record, error) {
	// Table names are internal constants selected by load(), never user input.
	rows, err := r.db.QueryContext(ctx, "SELECT payload FROM "+table+" ORDER BY id LIMIT ?", maxGraphRows+1)
	if err != nil {
		return nil, errors.New("BehaviorGraph records are unavailable")
	}
	defer rows.Close()
	records := make([]Record, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil || len(payload) > maxRecordBytes {
			return nil, errors.New("BehaviorGraph record exceeds its safe read limit")
		}
		*totalBytes += int64(len(payload))
		if *totalBytes > maxGraphBytes {
			return nil, errors.New("BehaviorGraph exceeds its safe read limit")
		}
		record, err := decodeRecord(payload)
		if err != nil {
			return nil, errors.New("BehaviorGraph contains invalid JSON")
		}
		records = append(records, record)
		if len(records) > maxGraphRows {
			return nil, errors.New("BehaviorGraph has too many records")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("BehaviorGraph records could not be read")
	}
	return records, nil
}

func (r *Reader) loadEntityRefs(ctx context.Context, totalBytes *int64) (map[string][]Record, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT entity_id,source_ref_sha256,payload FROM entity_source_refs ORDER BY entity_id,payload COLLATE BINARY LIMIT ?", maxGraphRows+1)
	if err != nil {
		return nil, errors.New("BehaviorGraph entity provenance is unavailable")
	}
	defer rows.Close()
	result := make(map[string][]Record)
	count := 0
	for rows.Next() {
		var entityID, digest, payload string
		if err := rows.Scan(&entityID, &digest, &payload); err != nil || len(payload) > maxRecordBytes {
			return nil, errors.New("BehaviorGraph entity provenance is malformed")
		}
		*totalBytes += int64(len(payload))
		if *totalBytes > maxGraphBytes {
			return nil, errors.New("BehaviorGraph exceeds its safe read limit")
		}
		ref, err := decodeRecord(payload)
		if err != nil || validateSource(ref) != nil || digest != sourceRefDigest(ref) {
			return nil, errors.New("BehaviorGraph entity provenance is malformed")
		}
		result[entityID] = append(result[entityID], ref)
		count++
		if count > maxGraphRows {
			return nil, errors.New("BehaviorGraph has too many provenance records")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("BehaviorGraph entity provenance could not be read")
	}
	return result, nil
}

func decodeRecord(encoded string) (Record, error) {
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var record map[string]any
	if err := decoder.Decode(&record); err != nil || record == nil {
		return nil, errors.New("invalid record")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return Record(record), nil
}

func recordsFromAny(values []any) []Record {
	result := make([]Record, 0, len(values))
	for _, value := range values {
		if record, ok := value.(map[string]any); ok {
			result = append(result, Record(record))
		} else {
			result = append(result, nil)
		}
	}
	return result
}

func validateEntity(record Record, schema int, program string) error {
	if !exactKeys(record, "id", "kind", "data", "source_refs") {
		if schema == 1 && exactKeys(record, "id", "kind", "data", "source_refs") {
			return nil
		}
		if schema == 2 && exactKeys(record, "id", "kind", "data") {
			// The normalized source refs are attached after loading.
		} else {
			return errors.New("invalid entity keys")
		}
	}
	id, okID := record["id"].(string)
	kind, okKind := record["kind"].(string)
	data, okData := record["data"].(map[string]any)
	_, kindAllowed := allowedKinds[kind]
	if !okID || !okKind || !okData || !kindAllowed || !graphIDPattern.MatchString(id) || !strings.HasPrefix(id, kind+"-") {
		return errors.New("invalid entity")
	}
	if err := validateEntityData(kind, data, program); err != nil {
		return err
	}
	if kind == "parameter" {
		expectedID, err := historicalEntityID(kind, Record(data))
		if err != nil || expectedID != id {
			return errors.New("parameter ID does not match its content")
		}
	}
	if kind == "field_occurrence" || kind == "value_reference" {
		expectedID, err := historicalEntityID(kind, Record(data))
		if err != nil || expectedID != id {
			return errors.New("field correlation entity ID does not match its content")
		}
	}
	if kind == "operation" {
		expectedID, err := historicalEntityID(kind, Record(data))
		if err != nil || expectedID != id {
			return errors.New("operation ID does not match its content")
		}
	}
	if schema == 1 {
		if _, ok := record["source_refs"].([]any); !ok {
			return errors.New("invalid legacy refs")
		}
	}
	return nil
}

func validateEntityData(kind string, data map[string]any, program string) error {
	switch kind {
	case "observation":
		return validateObservationStates(data)
	case "operation":
		selectedProgram, okProgram := data["program"].(string)
		origin, okOrigin := data["origin"].(string)
		method, okMethod := data["method"].(string)
		version, okVersion := data["route_family_version"].(string)
		family, okFamily := data["route_family"].(string)
		parsedOrigin, originErr := url.Parse(origin)
		canonicalFamily, familyOK := RouteFamilyV1(family)
		if !okProgram || selectedProgram != program || !okOrigin || len(origin) > 4096 || originErr != nil ||
			parsedOrigin == nil || (parsedOrigin.Scheme != "http" && parsedOrigin.Scheme != "https") ||
			parsedOrigin.Hostname() == "" || parsedOrigin.User != nil || parsedOrigin.Path != "" ||
			parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" || !okMethod || !httpMethodPattern.MatchString(method) ||
			!okVersion || version != RouteFamilyV1Version || !okFamily || !familyOK || canonicalFamily != family {
			return errors.New("invalid operation metadata")
		}
	case "parameter":
		selectedProgram, okProgram := data["program"].(string)
		endpointID, okEndpoint := data["endpoint_id"].(string)
		side, okSide := data["side"].(string)
		path, okPath := data["path"].(string)
		typeName, okType := data["type"].(string)
		location, okLocation := data["location"].(string)
		if !okProgram || selectedProgram != program || !okEndpoint || !strings.HasPrefix(endpointID, "endpoint-") ||
			!graphIDPattern.MatchString(endpointID) || !okSide || (side != "request" && side != "response") ||
			!okPath || len(path) == 0 || len(path) > maxRecordBytes || !okType || len(typeName) == 0 || len(typeName) > 128 ||
			!okLocation || len(location) == 0 || len(location) > 256 {
			return errors.New("invalid parameter metadata")
		}
	case "field_occurrence":
		if !exactKeys(data, "program", "capture_id", "revision_id", "observation_id", "parameter_id", "side", "location", "path", "occurrence_index", "type", "source_span", "value_span") {
			return errors.New("invalid field occurrence shape")
		}
		selectedProgram, okProgram := data["program"].(string)
		captureID, okCapture := data["capture_id"].(string)
		revisionID, okRevision := data["revision_id"].(string)
		observationID, okObservation := data["observation_id"].(string)
		parameterID, okParameter := data["parameter_id"].(string)
		side, okSide := data["side"].(string)
		location, okLocation := data["location"].(string)
		path, okPath := data["path"].(string)
		typeName, okType := data["type"].(string)
		expectedBasis := "request_message"
		validSideLocation := side == "request" && (location == "query" || location == "request.body")
		if side == "response" {
			expectedBasis = "response_message"
			validSideLocation = location == "response.body"
		}
		validOccurrenceType := typeName == "string"
		if location == "request.body" || location == "response.body" {
			validOccurrenceType = validOccurrenceType || typeName == "number" || typeName == "boolean" || typeName == "null"
		}
		if !okProgram || selectedProgram != program || !okCapture || !graphDigestPattern.MatchString(captureID) ||
			!okRevision || !graphDigestPattern.MatchString(revisionID) ||
			!okObservation || !graphIDPattern.MatchString(observationID) || !okParameter || !strings.HasPrefix(parameterID, "parameter-") || !graphIDPattern.MatchString(parameterID) ||
			!okSide || !validSideLocation || !okLocation || !okPath || len(path) == 0 || len(path) > maxRecordBytes ||
			!okType || !validOccurrenceType || !validNonnegativeInteger(data["occurrence_index"]) ||
			!validFieldSourceSpan(data["source_span"], expectedBasis) || !validOptionalFieldValueSpan(data["value_span"], data["source_span"]) {
			return errors.New("invalid field occurrence metadata")
		}
	case "value_reference":
		tokenVersion := data["token_version"]
		normalization := data["normalization"]
		legacyNormalization := tokenVersion == "field-equality-v1" && normalization == "urlencoded_scalar_v1"
		currentNormalization := tokenVersion == "field-equality-v2" && normalization == "decoded_utf8_scalar_v2"
		if !exactKeys(data, "program", "token_version", "value_type", "normalization", "equality_signature") ||
			data["program"] != program || (!legacyNormalization && !currentNormalization) || data["value_type"] != "string" {
			return errors.New("invalid project-keyed value reference")
		}
		token, okToken := data["equality_signature"].(string)
		if !okToken || !graphDigestPattern.MatchString(token) {
			return errors.New("invalid project-keyed value token")
		}
	case "request_variant", "response_variant":
		return validateVariantData(kind, data, program)
	case "condition":
		if value, present := data["value"]; present {
			switch selected := value.(type) {
			case bool:
			case nil:
			case string:
				if selected != "unknown" {
					return errors.New("invalid condition value")
				}
			default:
				return errors.New("invalid condition value")
			}
		}
	case "classifier_signal":
		captureID, okCapture := data["capture_id"].(string)
		selectedProgram, okProgram := data["program"].(string)
		findingStatus, okFinding := data["finding_status"].(string)
		priority, okPriority := data["priority"].(string)
		analysisState, okState := data["analysis_state"].(string)
		if !okProgram || selectedProgram != program || !okCapture || captureID == "" ||
			!okFinding || findingStatus != "candidate_only" || !okPriority ||
			(priority != "critical" && priority != "high" && priority != "medium" && priority != "low") ||
			!okState || (analysisState != "needs_review" && analysisState != "incomplete") {
			return errors.New("invalid classifier signal")
		}
		categories, okCategories := data["categories"].([]any)
		asvsRefs, okASVS := data["asvs_refs"].([]any)
		if !okCategories || len(categories) == 0 || !okASVS {
			return errors.New("invalid classifier labels")
		}
		for _, category := range categories {
			label, ok := category.(string)
			if !ok || !classifierCategory.MatchString(label) {
				return errors.New("invalid classifier category")
			}
		}
		for _, asvs := range asvsRefs {
			label, ok := asvs.(string)
			if !ok || !classifierASVS.MatchString(label) {
				return errors.New("invalid classifier ASVS reference")
			}
		}
	case "hypothesis":
		if !exactKeys(data, "program", "packet_sha256", "analyst", "rank", "weakness_class", "claim", "status", "evidence_strength", "impact_if_true", "missing_evidence", "alternative_explanation", "plan_only") {
			return errors.New("invalid offline hypothesis shape")
		}
		selectedProgram, okProgram := data["program"].(string)
		packetSHA, okPacket := data["packet_sha256"].(string)
		analyst, okAnalyst := data["analyst"].(string)
		rank, okRank := data["rank"].(json.Number)
		status, okStatus := data["status"].(string)
		evidenceStrength, okStrength := data["evidence_strength"].(string)
		planOnly, okPlan := data["plan_only"].(bool)
		if !okProgram || selectedProgram != program || !okPacket || !graphDigestPattern.MatchString(packetSHA) ||
			!okAnalyst || analyst == "" || !okRank || !positiveInteger(rank) ||
			!nonemptyText(data["weakness_class"]) || !nonemptyText(data["claim"]) ||
			!nonemptyText(data["impact_if_true"]) || !nonemptyText(data["alternative_explanation"]) ||
			!okStatus || status != "candidate" || !okStrength ||
			(evidenceStrength != "low" && evidenceStrength != "medium" && evidenceStrength != "high") ||
			!okPlan || !planOnly {
			return errors.New("invalid offline hypothesis")
		}
		missingEvidence, okMissing := data["missing_evidence"].([]any)
		if !okMissing {
			return errors.New("invalid hypothesis missing evidence")
		}
		for _, item := range missingEvidence {
			if _, ok := item.(string); !ok {
				return errors.New("invalid hypothesis missing evidence")
			}
		}
	}
	return nil
}

func validNonnegativeInteger(value any) bool {
	switch number := value.(type) {
	case int:
		return number >= 0
	case int64:
		return number >= 0
	case json.Number:
		integer, ok := new(big.Int).SetString(number.String(), 10)
		return ok && integer.Sign() >= 0 && integer.IsInt64()
	default:
		return false
	}
}

func fieldSpanBounds(value any) (int64, int64, string, bool) {
	span, ok := value.(map[string]any)
	if !ok || !exactKeys(span, "start", "end", "unit", "offset_basis", "wire_fidelity") || span["unit"] != "byte" {
		return 0, 0, "", false
	}
	offsetBasis, ok := span["offset_basis"].(string)
	if !ok || (offsetBasis != "request_message" && offsetBasis != "response_message") {
		return 0, 0, "", false
	}
	wireFidelity, ok := span["wire_fidelity"].(string)
	if !ok || (wireFidelity != "source_bytes" && wireFidelity != "reconstructed_text" && wireFidelity != "unknown") {
		return 0, 0, "", false
	}
	start, startOK := fieldSpanInteger(span["start"])
	end, endOK := fieldSpanInteger(span["end"])
	return start, end, offsetBasis, startOK && endOK && end >= start && end <= 1<<30
}

func fieldSpanInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), number >= 0
	case int64:
		return number, number >= 0
	case json.Number:
		integer, ok := new(big.Int).SetString(number.String(), 10)
		if !ok || integer.Sign() < 0 || !integer.IsInt64() {
			return 0, false
		}
		return integer.Int64(), true
	default:
		return 0, false
	}
}

func validFieldSourceSpan(value any, expectedBasis string) bool {
	_, _, basis, valid := fieldSpanBounds(value)
	return valid && basis == expectedBasis
}

func validOptionalFieldValueSpan(value, sourceSpan any) bool {
	if value == nil {
		return true
	}
	valueStart, valueEnd, valueBasis, valueOK := fieldSpanBounds(value)
	sourceStart, sourceEnd, sourceBasis, sourceOK := fieldSpanBounds(sourceSpan)
	return valueOK && sourceOK && valueBasis == sourceBasis && valueStart >= sourceStart && valueEnd <= sourceEnd
}

func validateVariantData(kind string, data map[string]any, program string) error {
	selectedProgram, okProgram := data["program"].(string)
	state, okState := data["signature_state"].(string)
	parseState, okParseState := data["parse_state"].(string)
	contentTypeValue, okContentType := data["content_type"].(string)
	signatureField := "request_signature"
	if kind == "response_variant" {
		signatureField = "response_signature"
	}
	signature, okSignature := data[signatureField].(string)
	if !okProgram || selectedProgram != program || !okState ||
		(state != "complete" && state != "partial_prefix" && state != "unknown_extent") ||
		!okParseState || (parseState != "complete" && parseState != "incomplete") ||
		!okContentType || !isCanonicalMediaType(contentTypeValue) ||
		!okSignature || !graphDigestPattern.MatchString(signature) ||
		!nonnegativeJSONIntegerOrNull(data["body_bytes"]) {
		return errors.New("invalid message variant metadata")
	}
	if _, present := data["body_bytes"]; !present {
		return errors.New("message variant byte count is missing")
	}
	if kind == "request_variant" {
		method, ok := data["method"].(string)
		if !ok || method != "UNKNOWN" && !httpMethodPattern.MatchString(method) {
			return errors.New("invalid request variant method")
		}
	} else if status, present := data["status"]; present && status != nil && !validHTTPStatus(status) {
		return errors.New("invalid response variant status")
	}
	if _, ok := data["fields"].([]any); !ok {
		return errors.New("invalid message variant fields")
	}
	if _, ok := data["header_metadata"].([]any); !ok {
		return errors.New("invalid message variant header metadata")
	}
	if _, ok := data["characteristics"].(map[string]any); !ok {
		return errors.New("invalid message variant characteristics")
	}
	return nil
}

func nonnegativeJSONIntegerOrNull(value any) bool {
	if value == nil {
		return true
	}
	number, ok := value.(json.Number)
	if !ok || !isJSONInteger(number) {
		return false
	}
	integer, ok := new(big.Int).SetString(number.String(), 10)
	return ok && integer.Sign() >= 0
}

func validateObservationStates(value any) error {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			switch key {
			case "status", "http_status", "status_code", "statusCode", "response_status":
				if !validHTTPStatus(child) {
					return errors.New("invalid observation HTTP status")
				}
			case "evidence_state", "observation_state", "state":
				state, ok := child.(string)
				if !ok {
					return errors.New("invalid observation evidence state")
				}
				if _, ok := allowedEvidence[state]; !ok {
					return errors.New("invalid observation evidence state")
				}
			case "finding_state":
				if child != nil {
					return errors.New("observation cannot carry finding state")
				}
			}
			if err := validateObservationStates(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range node {
			if err := validateObservationStates(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validHTTPStatus(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	status, err := number.Int64()
	return err == nil && status >= 100 && status <= 599
}

func positiveInteger(value json.Number) bool {
	integer, ok := new(big.Int).SetString(value.String(), 10)
	return ok && integer.Sign() > 0
}

func nonemptyText(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

func validateAssertion(record Record, sources map[string]Record, program string) error {
	if !exactKeys(record, "id", "subject", "predicate", "object", "context", "origin_kind", "evidence_state", "source_refs") {
		return errors.New("invalid assertion shape")
	}
	id, okID := record["id"].(string)
	subject, okSubject := record["subject"].(string)
	object, okObject := record["object"].(string)
	predicate, okPredicate := record["predicate"].(string)
	origin, okOrigin := record["origin_kind"].(string)
	evidence, okEvidence := record["evidence_state"].(string)
	context, okContext := record["context"].(map[string]any)
	if !okID || !graphIDPattern.MatchString(id) || !strings.HasPrefix(id, "assertion-") ||
		!okSubject || !graphIDPattern.MatchString(subject) || !okObject || !graphIDPattern.MatchString(object) ||
		!okPredicate || !okOrigin || !okEvidence || !okContext {
		return errors.New("invalid assertion")
	}
	if _, ok := allowedPredicates[predicate]; !ok {
		return errors.New("invalid predicate")
	}
	if _, ok := allowedOrigins[origin]; !ok {
		return errors.New("invalid origin")
	}
	if _, ok := allowedEvidence[evidence]; !ok {
		return errors.New("invalid evidence")
	}
	if err := validateContext(context, program); err != nil {
		return err
	}
	identity := make(Record, len(record)-1)
	for key, value := range record {
		if key != "id" {
			identity[key] = value
		}
	}
	encoded, err := canonicalJSON(Record{"kind": "assertion", "identity": identity})
	if err != nil {
		return errors.New("invalid assertion identity")
	}
	digest := sha256.Sum256(encoded)
	if id != "assertion-"+hex.EncodeToString(digest[:]) {
		return errors.New("assertion ID does not match its content")
	}
	return validateRefs(record["source_refs"], sources)
}

func validateCard(record Record, sources map[string]Record, selectedProgram string) error {
	if !exactKeys(record, "schema_version", "id", "capture_id", "program", "source_refs", "endpoint", "context", "request_analysis", "response_analysis", "business_action", "hypotheses", "missing_evidence", "analysis_state") {
		return errors.New("invalid card shape")
	}
	version, okVersion := record["schema_version"].(json.Number)
	id, okID := record["id"].(string)
	program, okProgram := record["program"].(string)
	captureID, okCapture := record["capture_id"].(string)
	context, okContext := record["context"].(map[string]any)
	state, okState := record["analysis_state"].(string)
	if !okVersion || version.String() != "1" || !okID || !graphIDPattern.MatchString(id) || !strings.HasPrefix(id, "card-") ||
		!okProgram || program == "" || !okCapture || captureID == "" || !okContext || !okState ||
		(state != "needs_review" && state != "incomplete") {
		return errors.New("invalid card")
	}
	if err := validateContext(context, selectedProgram); err != nil {
		return err
	}
	endpoint, ok := record["endpoint"].(map[string]any)
	if !ok {
		return errors.New("invalid card endpoint")
	}
	if endpointID, present := endpoint["id"]; present && endpointID != nil {
		id, ok := endpointID.(string)
		if !ok || !graphIDPattern.MatchString(id) || !strings.HasPrefix(id, "endpoint-") {
			return errors.New("invalid endpoint id")
		}
	}
	for _, side := range []string{"request_analysis", "response_analysis"} {
		analysis, ok := record[side].(map[string]any)
		if !ok {
			return errors.New("invalid card field")
		}
		parseState, ok := analysis["parse_state"].(string)
		if !ok || (parseState != "complete" && parseState != "incomplete") {
			return errors.New("invalid card parse state")
		}
		if side == "response_analysis" {
			if status, present := analysis["status"]; present && status != nil {
				number, ok := status.(json.Number)
				if !ok {
					return errors.New("invalid response status")
				}
				statusCode, err := number.Int64()
				if err != nil || statusCode < 100 || statusCode > 599 {
					return errors.New("invalid response status")
				}
			}
		}
		if state != "incomplete" && parseState == "incomplete" {
			return errors.New("incomplete card parsing")
		}
	}
	if _, ok := record["business_action"].(map[string]any); !ok {
		return errors.New("invalid card field")
	}
	hypotheses, ok := record["hypotheses"].([]any)
	if !ok {
		return errors.New("invalid hypotheses")
	}
	for _, item := range hypotheses {
		hypothesis, ok := item.(map[string]any)
		if !ok || hypothesis["outcome"] != "candidate" {
			return errors.New("promoted graph hypothesis")
		}
	}
	missingEvidence, ok := record["missing_evidence"].([]any)
	if !ok {
		return errors.New("invalid missing evidence")
	}
	for _, item := range missingEvidence {
		if _, ok := item.(string); !ok {
			return errors.New("invalid missing evidence")
		}
	}
	return validateRefs(record["source_refs"], sources)
}

func validateSource(ref Record) error {
	if !exactKeys(ref, "path", "sha256", "record_locator") {
		return errors.New("invalid source reference")
	}
	path, okPath := ref["path"].(string)
	digest, okDigest := ref["sha256"].(string)
	locator, okLocator := ref["record_locator"].(string)
	if !okPath || !filepath.IsAbs(path) || filepath.Clean(path) != path || !okDigest || !graphDigestPattern.MatchString(digest) || !okLocator || strings.TrimSpace(locator) == "" {
		return errors.New("invalid source reference")
	}
	return nil
}

func validateRefs(value any, sources map[string]Record) error {
	refs, ok := value.([]Record)
	if !ok {
		if raw, isRaw := value.([]any); isRaw {
			refs = recordsFromAny(raw)
		} else {
			return errors.New("invalid source refs")
		}
	}
	if len(refs) == 0 {
		return errors.New("empty source refs")
	}
	for _, ref := range refs {
		if validateSource(ref) != nil {
			return errors.New("invalid source ref")
		}
		if _, ok := sources[sourcePair(ref)]; !ok {
			return errors.New("undeclared source ref")
		}
	}
	return nil
}

func validateContext(value map[string]any, program string) error {
	if selected, present := value["program"]; present {
		selectedProgram, ok := selected.(string)
		if !ok || selectedProgram != program {
			return errors.New("context belongs to another project")
		}
	}
	for _, key := range contextKeys {
		if key == "program" {
			continue
		}
		if scalar, ok := value[key]; ok && scalar != nil {
			switch scalar.(type) {
			case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
			default:
				if number, ok := scalar.(json.Number); !ok || !isJSONInteger(number) {
					return errors.New("context must contain opaque scalars")
				}
			}
		}
	}
	return nil
}

func isJSONInteger(value json.Number) bool {
	if !jsonIntegerPattern.MatchString(value.String()) {
		return false
	}
	_, ok := new(big.Int).SetString(value.String(), 10)
	return ok
}

func validateEntityContext(record Record, program string) error {
	data, _ := record["data"].(map[string]any)
	direct := make(map[string]any)
	for _, key := range contextKeys {
		if value, ok := data[key]; ok {
			direct[key] = value
		}
	}
	if err := validateContext(direct, program); err != nil {
		return errors.New("invalid direct context")
	}
	if nested, ok := data["context"]; ok {
		value, ok := nested.(map[string]any)
		if !ok || validateContext(value, program) != nil {
			return errors.New("invalid nested context")
		}
	}
	return nil
}

func exactKeys(record map[string]any, keys ...string) bool {
	if len(record) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := record[key]; !ok {
			return false
		}
	}
	return true
}

func sourcePair(ref Record) string {
	return fmt.Sprint(ref["path"]) + "\x00" + fmt.Sprint(ref["sha256"])
}

func sourceRefDigest(ref Record) string {
	encoded, _ := canonicalJSON(ref)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func canonicalJSON(value any) ([]byte, error) {
	normalized, err := normalizeCanonicalNumbers(value)
	if err != nil {
		return nil, err
	}
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return []byte(unescapeJSONLineSeparators(strings.TrimSuffix(builder.String(), "\n"))), nil
}

func normalizeCanonicalNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		number, err := canonicalPythonNumber(typed.String())
		if err != nil {
			return nil, err
		}
		return json.RawMessage(number), nil
	case Record:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized, err := normalizeCanonicalNumbers(child)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized, err := normalizeCanonicalNumbers(child)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			normalized, err := normalizeCanonicalNumbers(child)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	case []Record:
		result := make([]any, len(typed))
		for index, child := range typed {
			normalized, err := normalizeCanonicalNumbers(child)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	case []map[string]any:
		result := make([]any, len(typed))
		for index, child := range typed {
			normalized, err := normalizeCanonicalNumbers(child)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	default:
		return value, nil
	}
}

func canonicalPythonNumber(encoded string) (string, error) {
	if !strings.ContainsAny(encoded, ".eE") {
		integer, ok := new(big.Int).SetString(encoded, 10)
		if !ok {
			return "", errors.New("invalid canonical integer")
		}
		return integer.String(), nil
	}
	value, err := strconv.ParseFloat(encoded, 64)
	if err != nil && !(errors.Is(err, strconv.ErrRange) && value == 0) {
		return "", errors.New("invalid canonical float")
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.SplitN(scientific, "e", 2)
	if len(parts) != 2 {
		return "", errors.New("invalid canonical float")
	}
	mantissa := parts[0]
	exponent, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", errors.New("invalid canonical float")
	}
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = strings.TrimPrefix(mantissa, "-")
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	if exponent < -4 || exponent >= 16 {
		exponentSign := "+"
		if exponent < 0 {
			exponentSign = "-"
			exponent = -exponent
		}
		return sign + mantissa + "e" + exponentSign + fmt.Sprintf("%02d", exponent), nil
	}
	decimalPosition := 1 + exponent
	var fixed string
	switch {
	case decimalPosition <= 0:
		fixed = "0." + strings.Repeat("0", -decimalPosition) + digits
	case decimalPosition >= len(digits):
		fixed = digits + strings.Repeat("0", decimalPosition-len(digits)) + ".0"
	default:
		fixed = digits[:decimalPosition] + "." + digits[decimalPosition:]
	}
	return sign + fixed, nil
}

func unescapeJSONLineSeparators(encoded string) string {
	const (
		lineSeparator      = "\u2028"
		paragraphSeparator = "\u2029"
	)
	var result strings.Builder
	result.Grow(len(encoded))
	inString := false
	for index := 0; index < len(encoded); {
		character := encoded[index]
		if !inString {
			if character == '"' {
				inString = true
			}
			result.WriteByte(character)
			index++
			continue
		}
		if character == '"' {
			inString = false
			result.WriteByte(character)
			index++
			continue
		}
		if character == '\\' {
			if strings.HasPrefix(encoded[index:], `\u2028`) {
				result.WriteString(lineSeparator)
				index += len(`\u2028`)
				continue
			}
			if strings.HasPrefix(encoded[index:], `\u2029`) {
				result.WriteString(paragraphSeparator)
				index += len(`\u2029`)
				continue
			}
			result.WriteByte(character)
			index++
			if index < len(encoded) {
				result.WriteByte(encoded[index])
				index++
			}
			continue
		}
		result.WriteByte(character)
		index++
	}
	return result.String()
}

func validatePublic(value any) error {
	return publicdata.Validate(value)
}

func (r *Reader) sourceIntegrity(refs []Record) ([]Record, map[string]string, bool) {
	ordered := append([]Record(nil), refs...)
	sort.Slice(ordered, func(i, j int) bool {
		left := sourcePair(ordered[i]) + "\x00" + fmt.Sprint(ordered[i]["record_locator"])
		right := sourcePair(ordered[j]) + "\x00" + fmt.Sprint(ordered[j]["record_locator"])
		return left < right
	})
	result := make([]Record, 0, len(ordered))
	stateByPair := make(map[string]string, len(ordered))
	r.sourceBytesRead = 0
	r.sourceChecks = 0
	r.sourceBudgetExceeded = false
	r.sourceIntegrityCache = make(map[string]string)
	for _, ref := range ordered {
		pair := sourcePair(ref)
		state, cached := r.sourceIntegrityCache[pair]
		if !cached {
			state = r.checkSource(ref)
			r.sourceIntegrityCache[pair] = state
		}
		freshness := "unknown"
		if state == "current" {
			freshness = "current"
		} else if state == "stale" || state == "missing" {
			freshness = "stale"
		}
		copyRef := Record{"path": ref["path"], "sha256": ref["sha256"], "record_locator": ref["record_locator"], "integrity": state, "freshness": freshness}
		result = append(result, copyRef)
		stateByPair[sourcePair(ref)] = state
	}
	return result, stateByPair, r.sourceBudgetExceeded
}

func (r *Reader) checkSource(ref Record) string {
	path, ok := ref["path"].(string)
	if !ok {
		return "unavailable"
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(r.receiptRoot, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "unknown"
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}
		return "unknown"
	}
	resolvedRel, err := filepath.Rel(r.receiptRoot, resolved)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(os.PathSeparator)) || resolved != clean {
		return "unknown"
	}
	info, err := os.Lstat(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}
		return "unknown"
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "unknown"
	}
	if info.Size() > maxReceiptBytes {
		r.sourceBudgetExceeded = true
		return "unknown"
	}
	r.sourceChecks++
	if r.sourceChecks > maxSliceSourceRefs || r.sourceBytesRead+info.Size() > maxSliceReceiptBytes {
		r.sourceBudgetExceeded = true
		return "unknown"
	}
	file, err := os.Open(clean)
	if err != nil {
		return "unknown"
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return "unknown"
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(file, maxReceiptBytes+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return "unknown"
	}
	if read > maxReceiptBytes || r.sourceBytesRead+read > maxSliceReceiptBytes {
		r.sourceBudgetExceeded = true
		return "unknown"
	}
	r.sourceBytesRead += read
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != ref["sha256"] {
		return "stale"
	}
	return "current"
}

func sourceFreshness(refs []Record, states map[string]string) string {
	if len(refs) == 0 {
		return "unknown"
	}
	allCurrent := true
	for _, ref := range refs {
		state := states[sourcePair(ref)]
		if state == "stale" || state == "missing" {
			return "stale"
		}
		if state != "current" {
			allCurrent = false
		}
	}
	if allCurrent {
		return "current"
	}
	return "unknown"
}
