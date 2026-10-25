package graph

import (
	"bytes"
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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

const (
	// MaxBundleJSONBytes bounds one CLI/API bundle and the merged batch.
	MaxBundleJSONBytes       = 16 << 20
	maxWriterBundles         = 100
	maxWriterRecords         = 20_000
	maxWriterSources         = 1_000
	maxWriterSourceRefs      = 20_000
	maxWriterReceiptBytes    = 4 << 20
	maxWriterReceiptsPerPage = 32 << 20
	writerQueryBatchSize     = 400
)

// Writer is scoped to the registered project selected by its ProjectStore.
// It never receives or derives a caller-supplied database path.
type Writer struct {
	program     string
	projectDir  string
	graphPath   string
	receiptRoot string
}

// MatchesProjectStore reports whether this writer is bound to the selected
// project's exact local storage directory and graph files.
func (w *Writer) MatchesProjectStore(selected *store.ProjectStore) bool {
	if w == nil || selected == nil || selected.ProjectKey == "" || selected.Database == nil {
		return false
	}
	projectDir, err := filepath.Abs(selected.Directory)
	if err != nil || filepath.Clean(projectDir) != projectDir {
		return false
	}
	graphPath := filepath.Join(projectDir, "behavior.sqlite3")
	receiptRoot := filepath.Join(projectDir, "behavior-receipts")
	return w.program == selected.ProjectKey && w.projectDir == projectDir &&
		w.graphPath == graphPath && w.receiptRoot == receiptRoot &&
		selected.Database.Path() == filepath.Join(projectDir, "graph.sqlite3") &&
		filepath.Clean(selected.BehaviorGraphPath) == graphPath &&
		filepath.Clean(selected.BehaviorReceiptPath) == receiptRoot
}

// ImportResult is deliberately limited to structural import accounting.
type ImportResult struct {
	SchemaVersion   int            `json:"schema_version"`
	Program         string         `json:"program"`
	ProjectKey      string         `json:"project_key"`
	BundleID        string         `json:"bundle_id"`
	Replayed        bool           `json:"replayed"`
	Inserted        map[string]int `json:"inserted"`
	UpdatedEntities int            `json:"updated_entities"`
	UpdatedCards    int            `json:"updated_cards"`
}

type importFailure struct {
	code  string
	cause error
}

func (failure *importFailure) Error() string {
	if failure == nil || failure.cause == nil {
		return "BehaviorGraph import failed"
	}
	return failure.cause.Error()
}

func (failure *importFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

// ImportFailureCode returns a fixed diagnostic code for a failed graph import.
// It never exposes the underlying error text, which may contain local details.
func ImportFailureCode(err error) string {
	var failure *importFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	return ""
}

func wrapImportFailure(code string, err error) error {
	if err == nil {
		return nil
	}
	var failure *importFailure
	if errors.As(err, &failure) {
		return err
	}
	return &importFailure{code: code, cause: err}
}

// OpenWriter binds a writer to an explicitly selected, schema-validated
// ProjectStore. The caller must hold the project's exclusive maintenance
// lease; only BehaviorGraph and its receipts are written by ImportBundles.
func OpenWriter(ctx context.Context, selected *store.ProjectStore) (*Writer, error) {
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil {
		return nil, errors.New("graph writer requires a schema-validated selected project store")
	}
	projectKey, err := selected.Project.StorageKey()
	if err != nil || projectKey != selected.ProjectKey || !graphDigestPattern.MatchString(projectKey) {
		return nil, errors.New("selected project identity is inconsistent")
	}
	projectDir, err := filepath.Abs(selected.Directory)
	if err != nil || filepath.Base(projectDir) != projectKey || filepath.Base(filepath.Dir(projectDir)) != "projects" {
		return nil, errors.New("selected project directory is invalid")
	}
	registered, err := project.LoadProjectRefsReadOnly(filepath.Dir(filepath.Dir(projectDir)))
	if err != nil {
		return nil, errors.New("selected project registry is unavailable")
	}
	registeredSelected := false
	for _, ref := range registered {
		key, keyErr := ref.StorageKey()
		if keyErr == nil && key == projectKey {
			registeredSelected = true
			break
		}
	}
	if !registeredSelected {
		return nil, errors.New("selected project is not registered")
	}
	dirInfo, err := os.Lstat(projectDir)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("selected project directory is unavailable")
	}
	resolvedDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil || resolvedDir != projectDir {
		return nil, errors.New("selected project directory is unavailable")
	}
	if selected.Database.Path() != filepath.Join(projectDir, "graph.sqlite3") {
		return nil, errors.New("selected capture store does not match the project")
	}
	if filepath.Clean(selected.VaultDirectory) != filepath.Join(projectDir, "vault") ||
		filepath.Clean(selected.BehaviorGraphPath) != filepath.Join(projectDir, "behavior.sqlite3") ||
		filepath.Clean(selected.BehaviorReceiptPath) != filepath.Join(projectDir, "behavior-receipts") {
		return nil, errors.New("selected project storage paths are inconsistent")
	}

	expectedGraph := filepath.Join(projectDir, "behavior.sqlite3")
	selectedGraph, err := filepath.Abs(selected.BehaviorGraphPath)
	if err != nil || selectedGraph != expectedGraph {
		return nil, errors.New("BehaviorGraph path is outside the selected project")
	}
	expectedReceipts := filepath.Join(projectDir, "behavior-receipts")
	selectedReceipts, err := filepath.Abs(selected.BehaviorReceiptPath)
	if err != nil || selectedReceipts != expectedReceipts {
		return nil, errors.New("graph receipt directory is outside the selected project")
	}
	receiptInfo, err := os.Lstat(expectedReceipts)
	if err != nil || receiptInfo.Mode()&os.ModeSymlink != 0 || !receiptInfo.IsDir() || receiptInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("graph receipt directory is unavailable")
	}
	resolvedReceipts, err := filepath.EvalSymlinks(expectedReceipts)
	if err != nil || resolvedReceipts != expectedReceipts {
		return nil, errors.New("graph receipt directory is unavailable")
	}

	writer := &Writer{program: projectKey, projectDir: projectDir, graphPath: expectedGraph, receiptRoot: expectedReceipts}
	graphInfo, err := os.Lstat(expectedGraph)
	if err == nil {
		if graphInfo.Mode()&os.ModeSymlink != 0 || !graphInfo.Mode().IsRegular() || graphInfo.Mode().Perm()&0o077 != 0 || graphInfo.Mode().Perm()&0o600 != 0o600 {
			return nil, errors.New("BehaviorGraph must be an owner-only regular file")
		}
		// Writer startup needs the graph namespace and schema, not a full
		// materialization of every payload. OpenReader remains responsible for
		// validating records when a graph snapshot is actually read.
		graphURL := (&url.URL{Scheme: "file", Path: expectedGraph}).String() + "?mode=ro"
		db, err := sql.Open("sqlite", graphURL)
		if err != nil {
			return nil, errors.New("BehaviorGraph database could not be opened for schema validation")
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if err := validateV2Schema(ctx, db, projectKey); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, errors.New("BehaviorGraph schema validation connection could not close")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("BehaviorGraph path is unavailable")
	}
	return writer, nil
}

// OpenShadowWriter binds a new graph database to the selected project's
// verified provenance receipts while keeping its active BehaviorGraph
// untouched. The caller must hold the selected project's exclusive
// maintenance lease for the complete rebuild. The shadow path must be absent
// and inside an existing owner-only local directory outside the selected
// project directory.
func OpenShadowWriter(ctx context.Context, selected *store.ProjectStore, shadowGraphPath string) (*Writer, error) {
	writer, err := OpenWriter(ctx, selected)
	if err != nil {
		return nil, err
	}
	if shadowGraphPath == "" {
		return nil, errors.New("shadow graph path is invalid")
	}
	absolute, err := filepath.Abs(shadowGraphPath)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == writer.graphPath {
		return nil, errors.New("shadow graph path is invalid")
	}
	projectRelative, err := filepath.Rel(writer.projectDir, absolute)
	if err != nil || projectRelative == "." || projectRelative != ".." && !strings.HasPrefix(projectRelative, ".."+string(filepath.Separator)) {
		return nil, errors.New("shadow graph path must be outside the selected project")
	}
	parent := filepath.Dir(absolute)
	parentInfo, err := os.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("shadow graph directory must be owner-only")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return nil, errors.New("shadow graph directory is unavailable")
	}
	if _, err := project.EnsurePrivateDirectory(parent); err != nil {
		return nil, errors.New("shadow graph directory must be on a local filesystem")
	}
	if _, err := os.Lstat(absolute); err == nil {
		return nil, errors.New("shadow graph path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("shadow graph path is unavailable")
	}
	writer.graphPath = absolute
	return writer, nil
}

// DecodeBundleJSON strictly decodes one bounded JSON object and rejects
// duplicate keys so canonical bundle identity is unambiguous.
func DecodeBundleJSON(encoded []byte) (Record, error) {
	if len(encoded) == 0 || len(encoded) > MaxBundleJSONBytes || !utf8.Valid(encoded) {
		return nil, errors.New("bundle JSON is empty or exceeds the size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, 0)
	if err != nil {
		return nil, errors.New("bundle JSON is malformed")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("bundle JSON must contain exactly one value")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, errors.New("bundle JSON must be an object")
	}
	return Record(object), nil
}

func decodeStrictJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting exceeds the limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object key is invalid")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, errors.New("JSON object has duplicate keys")
			}
			value, err := decodeStrictJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("JSON object is unterminated")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeStrictJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("JSON array is unterminated")
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// ImportBundle imports one source-pinned, already-redacted graph bundle.
func (w *Writer) ImportBundle(ctx context.Context, bundle Record) (ImportResult, error) {
	return w.ImportBundles(ctx, []Record{bundle})
}

// ImportBundles normalizes a bounded batch before writing, then stores its
// rows, provenance refs, and import identity in one immediate SQLite
// transaction. Receipt files are consumed as immutable existing inputs; this
// method does not create or modify receipt files.
func (w *Writer) ImportBundles(ctx context.Context, bundles []Record) (ImportResult, error) {
	if w == nil || w.graphPath == "" || w.receiptRoot == "" {
		return ImportResult{}, wrapImportFailure("graph_writer_unavailable", errors.New("graph writer is unavailable"))
	}
	if len(bundles) == 0 || len(bundles) > maxWriterBundles {
		return ImportResult{}, wrapImportFailure("bundle_limit_exceeded", errors.New("graph batch count is outside the allowed limit"))
	}
	normalizedBundles := make([]Record, 0, len(bundles))
	var inputBytes int
	for _, bundle := range bundles {
		encoded, err := marshalBoundedJSON(bundle, MaxBundleJSONBytes)
		if err != nil || len(encoded) == 0 {
			return ImportResult{}, wrapImportFailure("bundle_encode_failed", errors.New("graph bundle is invalid or exceeds the size limit"))
		}
		inputBytes += len(encoded)
		if inputBytes > MaxBundleJSONBytes {
			return ImportResult{}, wrapImportFailure("bundle_limit_exceeded", errors.New("graph batch exceeds the size limit"))
		}
		decoded, err := DecodeBundleJSON(encoded)
		if err != nil {
			return ImportResult{}, wrapImportFailure("bundle_decode_failed", err)
		}
		normalized, err := w.normalizeBundle(decoded)
		if err != nil {
			return ImportResult{}, wrapImportFailure("bundle_validation_failed", err)
		}
		normalizedBundles = append(normalizedBundles, normalized)
	}
	merged, err := w.mergeBundles(normalizedBundles)
	if err != nil {
		return ImportResult{}, wrapImportFailure("bundle_merge_failed", err)
	}
	sources := merged["sources"].([]Record)
	if err := w.verifyReceipts(sources); err != nil {
		return ImportResult{}, wrapImportFailure("receipt_validation_failed", err)
	}
	return w.writeNormalized(ctx, merged)
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > buffer.limit-buffer.Len() {
		return 0, errors.New("encoded JSON exceeds the size limit")
	}
	return buffer.Buffer.Write(value)
}

func marshalBoundedJSON(value any, maximum int) ([]byte, error) {
	buffer := &boundedBuffer{limit: maximum + 1}
	if err := json.NewEncoder(buffer).Encode(value); err != nil {
		return nil, err
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if len(encoded) == 0 || len(encoded) > maximum {
		return nil, errors.New("encoded JSON exceeds the size limit")
	}
	return encoded, nil
}

func (w *Writer) normalizeBundle(bundle Record) (Record, error) {
	if err := validatePublic(map[string]any(bundle)); err != nil {
		return nil, errors.New("bundle contains unsafe or private graph data")
	}
	if !exactKeys(map[string]any(bundle), "schema_version", "program", "sources", "entities", "assertions", "cards") {
		return nil, errors.New("bundle shape is invalid")
	}
	if !jsonIntegerEquals(bundle["schema_version"], 1) || bundle["program"] != w.program {
		return nil, errors.New("bundle schema or project namespace is invalid")
	}
	sourceRows, err := recordList(bundle["sources"], maxWriterSources)
	if err != nil {
		return nil, errors.New("bundle sources are invalid")
	}
	sourcesByPair := make(map[string]Record, len(sourceRows))
	declaredSourceRefs := make(map[string]struct{}, len(sourceRows))
	for _, source := range sourceRows {
		if err := validateSource(source); err != nil {
			return nil, errors.New("bundle source reference is invalid")
		}
		sourcesByPair[sourcePair(source)] = source
		if encoded, err := canonicalJSON(source); err == nil {
			declaredSourceRefs[string(encoded)] = struct{}{}
		}
	}
	sources := sortedUniqueRefs(sourceRows)
	declared := make(map[string]Record, len(sourcesByPair))
	for key, source := range sourcesByPair {
		declared[key] = source
	}

	entities, err := recordList(bundle["entities"], maxWriterRecords)
	if err != nil {
		return nil, errors.New("bundle entities are invalid")
	}
	assertions, err := recordList(bundle["assertions"], maxWriterRecords)
	if err != nil {
		return nil, errors.New("bundle assertions are invalid")
	}
	cards, err := recordList(bundle["cards"], maxWriterRecords)
	if err != nil {
		return nil, errors.New("bundle cards are invalid")
	}
	if len(entities)+len(assertions)+len(cards) > maxWriterRecords {
		return nil, errors.New("bundle record count exceeds the size limit")
	}
	var refCount int
	for _, table := range [][]Record{entities, assertions, cards} {
		for _, record := range table {
			refs, err := checkedRecordRefs(record, declared)
			if err != nil {
				return nil, errors.New("bundle record provenance is invalid")
			}
			refCount += len(refs)
			if refCount > maxWriterSourceRefs {
				return nil, errors.New("bundle provenance count exceeds the size limit")
			}
			record["source_refs"] = refsAsAny(refs)
			if requiresExactSourceRefs(record) && !allExactSourceRefsDeclared(refs, declaredSourceRefs) {
				return nil, errors.New("field correlation provenance does not match an exact source locator")
			}
		}
	}
	for _, entity := range entities {
		// Restore the source JSON list shape expected by the shared validator.
		refs, err := recordList(entity["source_refs"], maxWriterSources)
		if err != nil {
			return nil, errors.New("bundle entity provenance is invalid")
		}
		entity["source_refs"] = refsAsAny(refs)
		if err := validateEntity(entity, 1, w.program); err != nil {
			return nil, errors.New("bundle contains an invalid entity")
		}
		if err := validateEntityContext(entity, w.program); err != nil {
			return nil, errors.New("bundle contains an invalid entity context")
		}
		entity["source_refs"] = sortedUniqueRefs(refs)
		if encoded, err := canonicalJSON(entity); err != nil || len(encoded) > maxRecordBytes {
			return nil, errors.New("bundle entity exceeds the record size limit")
		}
	}
	for _, assertion := range assertions {
		refs, err := recordList(assertion["source_refs"], maxWriterSources)
		if err != nil {
			return nil, errors.New("bundle assertion provenance is invalid")
		}
		assertion["source_refs"] = refsAsAny(refs)
		if err := validateAssertion(assertion, declared, w.program); err != nil {
			return nil, errors.New("bundle contains an invalid assertion")
		}
		assertion["source_refs"] = refs
		if encoded, err := canonicalJSON(assertion); err != nil || len(encoded) > maxRecordBytes {
			return nil, errors.New("bundle assertion exceeds the record size limit")
		}
	}
	for _, card := range cards {
		refs, err := recordList(card["source_refs"], maxWriterSources)
		if err != nil {
			return nil, errors.New("bundle card provenance is invalid")
		}
		card["source_refs"] = refsAsAny(refs)
		if err := validateCard(card, declared, w.program); err != nil {
			return nil, errors.New("bundle contains an invalid card")
		}
		card["source_refs"] = sortedUniqueRefs(refs)
		if encoded, err := canonicalJSON(card); err != nil || len(encoded) > maxRecordBytes {
			return nil, errors.New("bundle card exceeds the record size limit")
		}
	}
	uniqueEntities, err := uniqueRecords(entities)
	if err != nil {
		return nil, err
	}
	uniqueAssertions, err := uniqueRecords(assertions)
	if err != nil {
		return nil, err
	}
	uniqueCards, err := uniqueRecords(cards)
	if err != nil {
		return nil, err
	}
	return Record{
		"schema_version": json.Number("1"), "program": w.program,
		"sources": sources, "entities": uniqueEntities,
		"assertions": uniqueAssertions, "cards": uniqueCards,
	}, nil
}

func requiresExactSourceRefs(record Record) bool {
	if record["kind"] == "field_occurrence" || record["kind"] == "url_reference" {
		return true
	}
	if record["predicate"] == "has_occurrence" || record["predicate"] == "occurrence_of" || record["predicate"] == "value_used_in" {
		return true
	}
	if record["predicate"] == "uses_request_url" || record["predicate"] == "referrer_url_matches" || record["predicate"] == "redirect_url_matches" {
		return true
	}
	return false
}

func allExactSourceRefsDeclared(refs []Record, declared map[string]struct{}) bool {
	for _, ref := range refs {
		encoded, err := canonicalJSON(ref)
		if err != nil {
			return false
		}
		if _, exists := declared[string(encoded)]; !exists {
			return false
		}
	}
	return len(refs) != 0
}

func (w *Writer) mergeBundles(bundles []Record) (Record, error) {
	sources := make([]Record, 0)
	entities := make(map[string]Record)
	assertions := make(map[string]Record)
	cards := make(map[string]Record)
	for _, bundle := range bundles {
		sources = append(sources, bundle["sources"].([]Record)...)
		for _, entity := range bundle["entities"].([]Record) {
			id := entity["id"].(string)
			previous, exists := entities[id]
			if !exists {
				entities[id] = entity
				continue
			}
			oldContent := cloneRecord(previous)
			newContent := cloneRecord(entity)
			oldRefs := oldContent["source_refs"].([]Record)
			newRefs := newContent["source_refs"].([]Record)
			delete(oldContent, "source_refs")
			delete(newContent, "source_refs")
			if !sameCanonical(oldContent, newContent) && !compatibleVariantProjection(oldContent, newContent, w.program) {
				return nil, fmt.Errorf("entity identity collision for kind %s fields %s; graph history cannot be overwritten", entity["kind"], strings.Join(recordDifferencePaths(oldContent, newContent, ""), ","))
			}
			merged := cloneRecord(previous)
			merged["source_refs"] = sortedUniqueRefs(append(oldRefs, newRefs...))
			entities[id] = merged
		}
		for _, item := range bundle["assertions"].([]Record) {
			if err := mergeImmutableRecord(assertions, item); err != nil {
				return nil, err
			}
		}
		for _, item := range bundle["cards"].([]Record) {
			id, ok := item["id"].(string)
			if !ok {
				return nil, errors.New("graph card ID is invalid")
			}
			previous, exists := cards[id]
			if !exists {
				cards[id] = item
				continue
			}
			merged, _, err := mergeCardProjection(previous, item)
			if err != nil {
				return nil, err
			}
			cards[id] = merged
		}
	}
	if len(sources) > maxWriterSources*maxWriterBundles {
		return nil, errors.New("merged source count exceeds the size limit")
	}
	if len(entities)+len(assertions)+len(cards) > maxWriterRecords {
		return nil, errors.New("merged graph record count exceeds the size limit")
	}
	mergedSources := sortedUniqueRefs(sources)
	if len(mergedSources) > maxWriterSources {
		return nil, errors.New("merged source count exceeds the size limit")
	}
	refCount := 0
	for _, records := range [][]Record{sortedRecordMap(entities), sortedRecordMap(assertions), sortedRecordMap(cards)} {
		for _, record := range records {
			refs, _ := record["source_refs"].([]Record)
			refCount += len(refs)
			if refCount > maxWriterSourceRefs {
				return nil, errors.New("merged provenance count exceeds the size limit")
			}
		}
	}
	return Record{
		"schema_version": json.Number("1"), "program": w.program,
		"sources": mergedSources, "entities": sortedRecordMap(entities),
		"assertions": sortedRecordMap(assertions), "cards": sortedRecordMap(cards),
	}, nil
}

func mergeImmutableRecord(target map[string]Record, record Record) error {
	id, ok := record["id"].(string)
	if !ok {
		return errors.New("graph record ID is invalid")
	}
	if previous, exists := target[id]; exists && !sameCanonical(previous, record) {
		return errors.New("conflicting graph ID; graph history cannot be overwritten")
	}
	target[id] = record
	return nil
}

func (w *Writer) verifyReceipts(sources []Record) error {
	var total int64
	for _, source := range sources {
		path := source["path"].(string)
		digest := source["sha256"].(string)
		remaining := maxWriterReceiptsPerPage - total
		bytesRead, err := w.verifyReceipt(path, digest, remaining)
		if err != nil {
			return err
		}
		total += bytesRead
	}
	return nil
}

func (w *Writer) verifyReceipt(path, expectedDigest string, maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return 0, errors.New("receipt verification exceeds the batch size limit")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, errors.New("receipt path is invalid")
	}
	relative, err := filepath.Rel(w.receiptRoot, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return 0, errors.New("receipt path is outside the selected project")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return 0, errors.New("receipt path is unavailable or resolves outside the receipt directory")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() < 0 || info.Size() > maxWriterReceiptBytes || info.Size() > maxBytes {
		return 0, errors.New("receipt must be an owner-only regular file within the size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, errors.New("receipt file could not be opened")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Mode().Perm()&0o077 != 0 || openedInfo.Size() < 0 || openedInfo.Size() > maxWriterReceiptBytes || openedInfo.Size() > maxBytes {
		return 0, errors.New("receipt file changed during validation")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, maxBytes))
	if err != nil || count > maxBytes {
		return 0, errors.New("receipt file exceeds the size limit")
	}
	afterInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, afterInfo) || openedInfo.Size() != count || afterInfo.Size() != count {
		return 0, errors.New("receipt file changed during validation")
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expectedDigest {
		return 0, errors.New("receipt hash is stale")
	}
	return count, nil
}

func (w *Writer) writeNormalized(ctx context.Context, bundle Record) (ImportResult, error) {
	db, created, err := w.openGraphDatabase(ctx)
	if err != nil {
		return ImportResult{}, wrapImportFailure("graph_database_open_failed", err)
	}
	transactionOpen := false
	committed := false
	defer func() {
		if transactionOpen {
			_, _ = db.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = db.Close()
		if created && !committed {
			_ = os.Remove(w.graphPath)
		}
	}()
	if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return ImportResult{}, wrapImportFailure("graph_transaction_begin_failed", errors.New("BehaviorGraph transaction could not begin"))
	}
	transactionOpen = true
	if created {
		if err := createV2Schema(ctx, db, w.program); err != nil {
			return ImportResult{}, wrapImportFailure("graph_schema_create_failed", err)
		}
	}
	if err := validateV2Schema(ctx, db, w.program); err != nil {
		return ImportResult{}, wrapImportFailure("graph_schema_invalid", err)
	}
	if err := ensureNeo4jOutboxSchema(ctx, db); err != nil {
		return ImportResult{}, wrapImportFailure("graph_outbox_schema_failed", err)
	}
	updatedCards, err := reconcileExistingCardProjections(ctx, db, bundle)
	if err != nil {
		return ImportResult{}, wrapImportFailure("graph_cards_reconcile_failed", err)
	}
	bundleID, err := graphBundleID(bundle)
	if err != nil {
		return ImportResult{}, wrapImportFailure("bundle_digest_failed", err)
	}
	if err := validateBatchReferences(ctx, db, bundle); err != nil {
		return ImportResult{}, wrapImportFailure("graph_reference_validation_failed", err)
	}
	result, err := insertNormalized(ctx, db, bundle, bundleID, w.program)
	if err != nil {
		return ImportResult{}, wrapImportFailure("graph_records_write_failed", err)
	}
	result.UpdatedCards = updatedCards
	if err := enqueueNeo4jOutbox(ctx, db, bundle, bundleID); err != nil {
		return ImportResult{}, wrapImportFailure("graph_outbox_write_failed", err)
	}
	if err := w.verifyReceipts(bundle["sources"].([]Record)); err != nil {
		return ImportResult{}, wrapImportFailure("receipt_validation_failed", err)
	}
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		return ImportResult{}, wrapImportFailure("graph_transaction_commit_failed", errors.New("BehaviorGraph transaction could not commit"))
	}
	transactionOpen = false
	committed = true
	return result, nil
}

func graphBundleID(bundle Record) (string, error) {
	encoded, err := canonicalJSON(bundle)
	if err != nil || len(encoded) == 0 || len(encoded) > MaxBundleJSONBytes {
		return "", errors.New("normalized graph batch exceeds the size limit")
	}
	digest := sha256.Sum256(encoded)
	return "bundle-" + hex.EncodeToString(digest[:]), nil
}

func reconcileExistingCardProjections(ctx context.Context, db *sql.DB, bundle Record) (int, error) {
	cards, ok := bundle["cards"].([]Record)
	if !ok {
		return 0, wrapImportFailure("graph_card_bundle_invalid", errors.New("BehaviorGraph cards are invalid"))
	}
	updated := 0
	for index, incoming := range cards {
		id, ok := incoming["id"].(string)
		if !ok || id == "" {
			return 0, wrapImportFailure("graph_card_identity_invalid", errors.New("BehaviorGraph card identity is invalid"))
		}
		var previous string
		err := db.QueryRowContext(ctx, "SELECT payload FROM cards WHERE id=?", id).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, wrapImportFailure("graph_card_read_failed", errors.New("BehaviorGraph card could not be read"))
		}
		stored, err := decodeRecord(previous)
		if err != nil {
			return 0, wrapImportFailure("graph_card_existing_invalid", errors.New("stored BehaviorGraph card is invalid"))
		}
		merged, changed, err := mergeCardProjection(stored, incoming)
		if err != nil {
			return 0, err
		}
		cards[index] = merged
		if !changed {
			continue
		}
		encoded, err := canonicalJSON(merged)
		if err != nil || len(encoded) == 0 || len(encoded) > maxRecordBytes {
			return 0, wrapImportFailure("graph_card_projection_invalid", errors.New("updated BehaviorGraph card exceeds the size limit"))
		}
		result, err := db.ExecContext(ctx,
			"UPDATE cards SET payload=? WHERE id=? AND payload=?", string(encoded), id, previous,
		)
		if err != nil {
			return 0, wrapImportFailure("graph_card_update_failed", errors.New("BehaviorGraph card could not be updated"))
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return 0, wrapImportFailure("graph_card_update_conflict", errors.New("BehaviorGraph card changed during projection"))
		}
		updated++
	}
	bundle["cards"] = cards
	return updated, nil
}

func mergeCardProjection(stored, incoming Record) (Record, bool, error) {
	for _, key := range []string{"schema_version", "id", "capture_id", "program", "source_refs"} {
		if !sameCanonical(stored[key], incoming[key]) {
			return nil, false, wrapImportFailure("graph_card_identity_conflict", errors.New("BehaviorGraph card identity cannot be reconciled"))
		}
	}
	for _, key := range []string{
		"endpoint", "context", "request_analysis", "response_analysis",
		"business_action", "hypotheses", "analysis_state", "missing_evidence",
	} {
		if _, exists := stored[key]; !exists {
			return nil, false, wrapImportFailure("graph_card_existing_invalid", errors.New("stored BehaviorGraph card is incomplete"))
		}
		if _, exists := incoming[key]; !exists {
			return nil, false, wrapImportFailure("graph_card_projection_invalid", errors.New("incoming BehaviorGraph card is incomplete"))
		}
	}
	storedState, stateOK := stored["analysis_state"].(string)
	if !stateOK || (storedState != "incomplete" && storedState != "needs_review") {
		return nil, false, wrapImportFailure("graph_card_existing_invalid", errors.New("stored BehaviorGraph card state is invalid"))
	}
	if _, actionOK := asRecord(stored["business_action"]); !actionOK {
		return nil, false, wrapImportFailure("graph_card_existing_invalid", errors.New("stored BehaviorGraph card analysis is invalid"))
	}
	if _, ok := stored["hypotheses"].([]any); !ok {
		return nil, false, wrapImportFailure("graph_card_existing_invalid", errors.New("stored BehaviorGraph card hypotheses are invalid"))
	}
	merged := cloneRecord(stored)
	for _, key := range []string{"endpoint", "context", "request_analysis", "response_analysis"} {
		merged[key] = incoming[key]
	}
	incomingRequest, requestOK := asRecord(incoming["request_analysis"])
	incomingResponse, responseOK := asRecord(incoming["response_analysis"])
	if !requestOK || !responseOK {
		return nil, false, wrapImportFailure("graph_card_projection_invalid", errors.New("incoming BehaviorGraph card analysis is invalid"))
	}
	requestParseState, requestParseOK := incomingRequest["parse_state"].(string)
	responseParseState, responseParseOK := incomingResponse["parse_state"].(string)
	if !requestParseOK || !responseParseOK || (requestParseState != "complete" && requestParseState != "incomplete") ||
		(responseParseState != "complete" && responseParseState != "incomplete") {
		return nil, false, wrapImportFailure("graph_card_projection_invalid", errors.New("incoming BehaviorGraph card parse state is invalid"))
	}
	if storedState == "needs_review" && requestParseState == "complete" && responseParseState == "complete" {
		merged["analysis_state"] = "needs_review"
	} else {
		merged["analysis_state"] = incoming["analysis_state"]
	}
	missingEvidence, err := mergeMissingEvidence(stored["missing_evidence"], incoming["missing_evidence"])
	if err != nil {
		return nil, false, wrapImportFailure("graph_card_evidence_invalid", err)
	}
	merged["missing_evidence"] = missingEvidence
	return merged, !sameCanonical(stored, merged), nil
}

func mergeMissingEvidence(stored, incoming any) ([]any, error) {
	storedItems, storedOK := stored.([]any)
	incomingItems, incomingOK := incoming.([]any)
	if !storedOK || !incomingOK {
		return nil, errors.New("BehaviorGraph missing evidence is invalid")
	}
	merged := make([]any, 0, len(storedItems)+len(incomingItems))
	seen := make(map[string]struct{}, len(storedItems)+len(incomingItems))
	for _, group := range [][]any{storedItems, incomingItems} {
		for _, item := range group {
			value, ok := item.(string)
			if !ok {
				return nil, errors.New("BehaviorGraph missing evidence is invalid")
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged, nil
}

func ensureNeo4jOutboxSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS neo4j_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		bundle_id TEXT NOT NULL UNIQUE,
		payload_sha256 TEXT NOT NULL,
		bundle_json TEXT NOT NULL,
		state TEXT NOT NULL CHECK(state IN ('pending','applied')) DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error_code TEXT,
		created_at TEXT NOT NULL,
		applied_at TEXT,
		applied_sha256 TEXT
	)`)
	if err != nil {
		return errors.New("Neo4j outbox table could not be initialized")
	}
	return nil
}

func enqueueNeo4jOutbox(ctx context.Context, db *sql.DB, bundle Record, bundleID string) error {
	encoded, err := canonicalJSON(bundle)
	if err != nil || len(encoded) == 0 || len(encoded) > MaxBundleJSONBytes {
		return errors.New("Neo4j outbox bundle could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	digestText := hex.EncodeToString(digest[:])
	if bundleID != "bundle-"+digestText {
		return errors.New("Neo4j outbox bundle identity does not match its payload")
	}
	if _, err := db.ExecContext(ctx,
		"INSERT OR IGNORE INTO neo4j_outbox(bundle_id,payload_sha256,bundle_json,state,created_at) VALUES(?,?,?,'pending',?)",
		bundleID, digestText, string(encoded), time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return errors.New("Neo4j outbox event could not be recorded")
	}
	var storedDigest string
	if err := db.QueryRowContext(ctx, "SELECT payload_sha256 FROM neo4j_outbox WHERE bundle_id=?", bundleID).Scan(&storedDigest); err != nil || storedDigest != digestText {
		return errors.New("Neo4j outbox event identity conflicts with its payload")
	}
	return nil
}

// OutboxItem is one checksummed sanitized graph bundle waiting for Neo4j.
type OutboxItem struct {
	ID         int64
	BundleID   string
	PayloadSHA string
	BundleJSON []byte
	Attempts   int64
}

// PendingOutbox returns a bounded ordered page of locally committed graph
// events. It opens BehaviorGraph read-only and never reads capture bytes.
func (w *Writer) PendingOutbox(ctx context.Context, limit int) ([]OutboxItem, error) {
	if w == nil || w.graphPath == "" || limit < 1 || limit > 1000 {
		return nil, errors.New("Neo4j outbox query is invalid")
	}
	info, err := os.Lstat(w.graphPath)
	if errors.Is(err, os.ErrNotExist) {
		return []OutboxItem{}, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("BehaviorGraph outbox database is unavailable")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: w.graphPath}).String()+"?mode=ro")
	if err != nil {
		return nil, errors.New("BehaviorGraph outbox database could not be opened")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var tableCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='neo4j_outbox'").Scan(&tableCount); err != nil {
		return nil, errors.New("BehaviorGraph outbox could not be inspected")
	}
	if tableCount == 0 {
		return []OutboxItem{}, nil
	}
	rows, err := db.QueryContext(ctx,
		"SELECT id,bundle_id,payload_sha256,bundle_json,attempts FROM neo4j_outbox WHERE state='pending' ORDER BY id LIMIT ?", limit,
	)
	if err != nil {
		return nil, errors.New("Neo4j outbox events could not be read")
	}
	defer rows.Close()
	items := make([]OutboxItem, 0)
	for rows.Next() {
		var item OutboxItem
		var encoded string
		if err := rows.Scan(&item.ID, &item.BundleID, &item.PayloadSHA, &encoded, &item.Attempts); err != nil || len(encoded) == 0 || len(encoded) > MaxBundleJSONBytes {
			return nil, errors.New("Neo4j outbox event is malformed")
		}
		item.BundleJSON = []byte(encoded)
		digest := sha256.Sum256(item.BundleJSON)
		if item.PayloadSHA != hex.EncodeToString(digest[:]) || item.BundleID != "bundle-"+item.PayloadSHA {
			return nil, errors.New("Neo4j outbox event checksum is invalid")
		}
		bundle, decodeErr := DecodeBundleJSON(item.BundleJSON)
		if decodeErr != nil || bundle["program"] != w.program || ValidatePublicData(bundle) != nil {
			return nil, errors.New("Neo4j outbox event contains invalid graph data")
		}
		items = append(items, item)
		if len(items) > limit {
			return nil, errors.New("Neo4j outbox query exceeded its row limit")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("Neo4j outbox events could not be read")
	}
	return items, nil
}

// OutboxBundleApplied reports whether the exact imported bundle has been
// acknowledged by Neo4j. It reads only the selected BehaviorGraph outbox.
func (w *Writer) OutboxBundleApplied(ctx context.Context, bundleID string) (bool, error) {
	if w == nil || w.graphPath == "" || len(bundleID) != len("bundle-")+64 || !strings.HasPrefix(bundleID, "bundle-") {
		return false, errors.New("Neo4j outbox bundle identity is invalid")
	}
	digest := strings.TrimPrefix(bundleID, "bundle-")
	if !graphDigestPattern.MatchString(digest) {
		return false, errors.New("Neo4j outbox bundle identity is invalid")
	}
	info, err := os.Lstat(w.graphPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, errors.New("BehaviorGraph outbox database is unavailable")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: w.graphPath}).String()+"?mode=ro")
	if err != nil {
		return false, errors.New("BehaviorGraph outbox database could not be opened")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var state, payloadSHA string
	if err := db.QueryRowContext(ctx,
		"SELECT state,payload_sha256 FROM neo4j_outbox WHERE bundle_id=?", bundleID,
	).Scan(&state, &payloadSHA); err != nil || payloadSHA != digest || (state != "pending" && state != "applied") {
		return false, errors.New("Neo4j outbox bundle state is unavailable")
	}
	if state == "pending" {
		return false, nil
	}
	var appliedSHA string
	if err := db.QueryRowContext(ctx, "SELECT applied_sha256 FROM neo4j_outbox WHERE bundle_id=?", bundleID).Scan(&appliedSHA); err != nil || appliedSHA != digest {
		return false, errors.New("Neo4j outbox applied checksum is invalid")
	}
	return true, nil
}

// MarkOutboxApplied acknowledges one item only after its Neo4j transaction
// committed. The payload hash remains available for local audit.
func (w *Writer) MarkOutboxApplied(ctx context.Context, item OutboxItem) error {
	if w == nil || w.graphPath == "" || item.ID < 1 || !graphDigestPattern.MatchString(item.PayloadSHA) || item.BundleID != "bundle-"+item.PayloadSHA {
		return errors.New("Neo4j outbox acknowledgement is invalid")
	}
	digest := sha256.Sum256(item.BundleJSON)
	if hex.EncodeToString(digest[:]) != item.PayloadSHA {
		return errors.New("Neo4j outbox acknowledgement checksum is invalid")
	}
	db, created, err := w.openGraphDatabase(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if created {
		_ = os.Remove(w.graphPath)
		return errors.New("BehaviorGraph outbox database unexpectedly disappeared")
	}
	if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return errors.New("Neo4j outbox acknowledgement transaction could not begin")
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	result, err := db.ExecContext(ctx,
		"UPDATE neo4j_outbox SET state='applied',applied_at=?,applied_sha256=?,last_error_code=NULL WHERE id=? AND bundle_id=? AND payload_sha256=? AND state='pending'",
		time.Now().UTC().Format(time.RFC3339Nano), item.PayloadSHA, item.ID, item.BundleID, item.PayloadSHA,
	)
	if err != nil {
		return errors.New("Neo4j outbox acknowledgement could not be recorded")
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return errors.New("Neo4j outbox acknowledgement could not be recorded")
	}
	if updated == 0 {
		var state string
		var appliedSHA sql.NullString
		if err := db.QueryRowContext(ctx,
			"SELECT state,applied_sha256 FROM neo4j_outbox WHERE id=? AND bundle_id=? AND payload_sha256=?",
			item.ID, item.BundleID, item.PayloadSHA,
		).Scan(&state, &appliedSHA); err != nil || state != "applied" || !appliedSHA.Valid || appliedSHA.String != item.PayloadSHA {
			return errors.New("Neo4j outbox event is no longer pending")
		}
	} else if updated != 1 {
		return errors.New("Neo4j outbox event is no longer pending")
	}
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		return errors.New("Neo4j outbox acknowledgement could not be committed")
	}
	committed = true
	return nil
}

// MarkOutboxFailed records a fixed error category and increments its bounded
// retry counter without storing the underlying message or any graph payload.
func (w *Writer) MarkOutboxFailed(ctx context.Context, item OutboxItem) error {
	if w == nil || w.graphPath == "" || item.ID < 1 || !graphDigestPattern.MatchString(item.PayloadSHA) {
		return errors.New("Neo4j outbox failure record is invalid")
	}
	db, created, err := w.openGraphDatabase(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if created {
		_ = os.Remove(w.graphPath)
		return errors.New("BehaviorGraph outbox database unexpectedly disappeared")
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE neo4j_outbox SET attempts=attempts+1,last_error_code='materialization_failed' WHERE id=? AND bundle_id=? AND payload_sha256=? AND state='pending'",
		item.ID, item.BundleID, item.PayloadSHA,
	); err != nil {
		return errors.New("Neo4j outbox failure state could not be recorded")
	}
	return nil
}

func (w *Writer) openGraphDatabase(ctx context.Context) (*sql.DB, bool, error) {
	created := false
	info, err := os.Lstat(w.graphPath)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(w.graphPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr == nil {
			created = true
			if chmodErr := file.Chmod(0o600); chmodErr != nil {
				_ = file.Close()
				_ = os.Remove(w.graphPath)
				return nil, false, errors.New("BehaviorGraph permissions could not be restricted")
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(w.graphPath)
				return nil, false, errors.New("BehaviorGraph file could not be created")
			}
		} else if errors.Is(createErr, os.ErrExist) {
			info, err = os.Lstat(w.graphPath)
		} else {
			return nil, false, errors.New("BehaviorGraph file could not be created")
		}
	} else if err != nil {
		return nil, false, errors.New("BehaviorGraph path is unavailable")
	}
	if !created {
		if info == nil {
			info, err = os.Lstat(w.graphPath)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o600 != 0o600 {
			return nil, false, errors.New("BehaviorGraph must be an owner-only regular file")
		}
	}
	fileURL := (&url.URL{Scheme: "file", Path: w.graphPath}).String() + "?mode=rw"
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		if created {
			_ = os.Remove(w.graphPath)
		}
		return nil, false, errors.New("BehaviorGraph database could not be opened")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		_ = db.Close()
		if created {
			_ = os.Remove(w.graphPath)
		}
		return nil, false, errors.New("BehaviorGraph journal mode could not be configured")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		if created {
			_ = os.Remove(w.graphPath)
		}
		return nil, false, errors.New("BehaviorGraph foreign-key mode could not be enabled")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=30000"); err != nil {
		_ = db.Close()
		if created {
			_ = os.Remove(w.graphPath)
		}
		return nil, false, errors.New("BehaviorGraph busy timeout could not be configured")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		_ = db.Close()
		if created {
			_ = os.Remove(w.graphPath)
		}
		return nil, false, errors.New("BehaviorGraph durability could not be configured")
	}
	return db, created, nil
}

func createV2Schema(ctx context.Context, db *sql.DB, program string) error {
	statements := []string{
		"CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE sources (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE entities (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE assertions (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE cards (id TEXT PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE imports (id TEXT PRIMARY KEY)",
		"CREATE TABLE entity_source_refs (entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE, source_ref_sha256 TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(entity_id,source_ref_sha256))",
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return errors.New("BehaviorGraph v2 schema could not be created")
		}
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO metadata(key,value) VALUES('schema_version','2'),('program',?)", program,
	); err != nil {
		return errors.New("BehaviorGraph namespace could not be initialized")
	}
	return nil
}

func validateV2Schema(ctx context.Context, db *sql.DB, program string) error {
	rows, err := db.QueryContext(ctx, "SELECT key,value FROM metadata ORDER BY key")
	if err != nil {
		return errors.New("BehaviorGraph metadata is unavailable")
	}
	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return errors.New("BehaviorGraph metadata is malformed")
		}
		if _, exists := metadata[key]; exists {
			_ = rows.Close()
			return errors.New("BehaviorGraph metadata is malformed")
		}
		metadata[key] = value
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil || len(metadata) != 2 || metadata["schema_version"] != "2" || metadata["program"] != program {
		return errors.New("BehaviorGraph schema or project namespace is unsupported")
	}
	for _, table := range []string{"sources", "entities", "assertions", "cards", "imports", "entity_source_refs"} {
		var found int
		if err := db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table,
		).Scan(&found); err != nil || found != 1 {
			return errors.New("BehaviorGraph v2 schema is incomplete")
		}
	}
	return nil
}

func validateBatchReferences(ctx context.Context, db *sql.DB, bundle Record) error {
	entities := make(map[string]Record)
	entityKinds := make(map[string]string)
	for _, entity := range bundle["entities"].([]Record) {
		id := entity["id"].(string)
		entities[id] = entity
		entityKinds[id] = entity["kind"].(string)
	}
	assertions := make(map[string]struct{})
	for _, assertion := range bundle["assertions"].([]Record) {
		assertions[assertion["id"].(string)] = struct{}{}
	}
	referenced := make(map[string]struct{})
	for _, assertion := range bundle["assertions"].([]Record) {
		referenced[assertion["subject"].(string)] = struct{}{}
		referenced[assertion["object"].(string)] = struct{}{}
	}
	for _, card := range bundle["cards"].([]Record) {
		if endpoint, ok := card["endpoint"].(map[string]any); ok {
			if id, ok := endpoint["id"].(string); ok {
				referenced[id] = struct{}{}
			}
		}
	}
	externalIDs := make([]string, 0, len(referenced))
	for id := range referenced {
		if _, exists := entities[id]; exists {
			continue
		}
		if _, exists := assertions[id]; exists {
			continue
		}
		externalIDs = append(externalIDs, id)
	}
	sort.Strings(externalIDs)
	storedEntities := make(map[string]Record)
	storedAssertions := make(map[string]struct{})
	for offset := 0; offset < len(externalIDs); offset += writerQueryBatchSize {
		end := min(offset+writerQueryBatchSize, len(externalIDs))
		batch := externalIDs[offset:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for index, id := range batch {
			args[index] = id
		}
		rows, err := db.QueryContext(ctx, "SELECT id,payload FROM entities WHERE id IN ("+placeholders+")", args...)
		if err != nil {
			return errors.New("BehaviorGraph entity references could not be read")
		}
		for rows.Next() {
			var id, payload string
			if err := rows.Scan(&id, &payload); err != nil {
				_ = rows.Close()
				return errors.New("BehaviorGraph entity reference is malformed")
			}
			record, err := decodeRecord(payload)
			if err != nil {
				_ = rows.Close()
				return errors.New("BehaviorGraph entity reference is malformed")
			}
			kind, _ := record["kind"].(string)
			if kind == "" {
				_ = rows.Close()
				return errors.New("BehaviorGraph entity reference is malformed")
			}
			storedEntities[id] = record
			entityKinds[id] = kind
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil {
			return errors.New("BehaviorGraph entity references could not be read")
		}
		rows, err = db.QueryContext(ctx, "SELECT id FROM assertions WHERE id IN ("+placeholders+")", args...)
		if err != nil {
			return errors.New("BehaviorGraph assertion references could not be read")
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return errors.New("BehaviorGraph assertion reference is malformed")
			}
			storedAssertions[id] = struct{}{}
		}
		rowsErr = rows.Err()
		closeErr = rows.Close()
		if rowsErr != nil || closeErr != nil {
			return errors.New("BehaviorGraph assertion references could not be read")
		}
	}
	knownEntities := make(map[string]struct{}, len(entityKinds))
	for id := range entityKinds {
		knownEntities[id] = struct{}{}
	}
	knownAssertions := make(map[string]struct{}, len(assertions)+len(storedAssertions))
	for id := range assertions {
		knownAssertions[id] = struct{}{}
	}
	for id := range storedAssertions {
		knownAssertions[id] = struct{}{}
	}
	proposalLinks := make(map[string]struct{})
	for _, assertion := range bundle["assertions"].([]Record) {
		for _, key := range []string{"subject", "object"} {
			id := assertion[key].(string)
			_, entityExists := knownEntities[id]
			_, assertionExists := knownAssertions[id]
			if !entityExists && !assertionExists {
				return errors.New("dangling BehaviorGraph assertion reference")
			}
			if assertionExists && assertion["predicate"] != "supports" && assertion["predicate"] != "contradicts" {
				return errors.New("only evidence relations may reference assertions")
			}
		}
		subject := assertion["subject"].(string)
		object := assertion["object"].(string)
		subjectKind, objectKind := entityKinds[subject], entityKinds[object]
		switch assertion["predicate"] {
		case "belongs_to_operation":
			if subjectKind != "endpoint" || objectKind != "operation" {
				return errors.New("operation relation has incompatible entity kinds")
			}
			endpoint := entities[subject]
			if endpoint == nil {
				endpoint = storedEntities[subject]
			}
			operation := entities[object]
			if operation == nil {
				operation = storedEntities[object]
			}
			endpointData, _ := endpoint["data"].(map[string]any)
			operationData, _ := operation["data"].(map[string]any)
			if endpointData["program"] != operationData["program"] || endpointData["origin"] != operationData["origin"] || endpointData["method"] != operationData["method"] {
				return errors.New("operation relation does not match its endpoint")
			}
		case "has_parameter":
			if subjectKind != "endpoint" || objectKind != "parameter" {
				return errors.New("parameter relation has incompatible entity kinds")
			}
			parameter := entities[object]
			if parameter == nil {
				parameter = storedEntities[object]
			}
			parameterData, _ := parameter["data"].(map[string]any)
			if parameterData["endpoint_id"] != subject || parameterData["side"] != "request" {
				return errors.New("request parameter relation is inconsistent")
			}
		case "returns":
			if subjectKind != "observation" || objectKind != "parameter" {
				return errors.New("response parameter relation has incompatible entity kinds")
			}
			observation := entities[subject]
			if observation == nil {
				observation = storedEntities[subject]
			}
			observationData, _ := observation["data"].(map[string]any)
			endpointID, _ := observationData["endpoint_id"].(string)
			if entityKinds[endpointID] != "endpoint" {
				return errors.New("response parameter endpoint is unavailable")
			}
			parameter := entities[object]
			if parameter == nil {
				parameter = storedEntities[object]
			}
			parameterData, _ := parameter["data"].(map[string]any)
			if parameterData["endpoint_id"] != endpointID || parameterData["side"] != "response" {
				return errors.New("response parameter relation is inconsistent")
			}
		case "has_occurrence":
			if subjectKind != "observation" || objectKind != "field_occurrence" {
				return errors.New("field occurrence relation has incompatible entity kinds")
			}
			observation := entities[subject]
			if observation == nil {
				observation = storedEntities[subject]
			}
			occurrence := entities[object]
			if occurrence == nil {
				occurrence = storedEntities[object]
			}
			observationData, _ := observation["data"].(map[string]any)
			occurrenceData, _ := occurrence["data"].(map[string]any)
			if occurrenceData["observation_id"] != subject || occurrenceData["capture_id"] != observationData["capture_id"] || occurrenceData["program"] != observationData["program"] {
				return errors.New("field occurrence does not match its observation")
			}
		case "occurrence_of":
			if subjectKind != "field_occurrence" || objectKind != "parameter" {
				return errors.New("field schema relation has incompatible entity kinds")
			}
			occurrence := entities[subject]
			if occurrence == nil {
				occurrence = storedEntities[subject]
			}
			parameter := entities[object]
			if parameter == nil {
				parameter = storedEntities[object]
			}
			occurrenceData, _ := occurrence["data"].(map[string]any)
			parameterData, _ := parameter["data"].(map[string]any)
			observationID, okObservationID := occurrenceData["observation_id"].(string)
			if !okObservationID {
				return errors.New("field occurrence has no observation reference")
			}
			observation := entities[observationID]
			if observation == nil {
				observation = storedEntities[observationID]
			}
			if observation == nil {
				return errors.New("field occurrence observation is unavailable")
			}
			observationData, _ := observation["data"].(map[string]any)
			if occurrenceData["parameter_id"] != object || occurrenceData["program"] != parameterData["program"] ||
				occurrenceData["path"] != parameterData["path"] || occurrenceData["side"] != parameterData["side"] ||
				observationData["endpoint_id"] != parameterData["endpoint_id"] {
				return errors.New("field occurrence does not match its parameter")
			}
		case "value_used_in":
			if subjectKind != "value_reference" || objectKind != "field_occurrence" {
				return errors.New("value equality relation has incompatible entity kinds")
			}
			value := entities[subject]
			if value == nil {
				value = storedEntities[subject]
			}
			occurrence := entities[object]
			if occurrence == nil {
				occurrence = storedEntities[object]
			}
			valueData, _ := value["data"].(map[string]any)
			occurrenceData, _ := occurrence["data"].(map[string]any)
			if valueData["program"] != occurrenceData["program"] || occurrenceData["value_span"] == nil {
				return errors.New("value equality relation does not match its field occurrence")
			}
			context, _ := assertion["context"].(map[string]any)
			if context["scenario"] != "keyed_value_equality" || context["scenario_step"] != "correlation_only" {
				return errors.New("value equality relation overstates its evidence")
			}
		}
		if subjectKind == "hypothesis" || objectKind == "hypothesis" {
			hypothesisID := subject
			if subjectKind != "hypothesis" {
				hypothesisID = object
			}
			hypothesis := entities[hypothesisID]
			if hypothesis == nil {
				hypothesis = storedEntities[hypothesisID]
			}
			data, _ := hypothesis["data"].(map[string]any)
			context, _ := assertion["context"].(map[string]any)
			if subjectKind != "hypothesis" || (objectKind != "observation" && objectKind != "endpoint" && objectKind != "parameter" && objectKind != "flow" && objectKind != "classifier_signal") ||
				(assertion["predicate"] != "supports" && assertion["predicate"] != "contradicts") ||
				assertion["origin_kind"] != "model_proposed" || assertion["evidence_state"] != "proposed" ||
				context["packet_sha256"] != data["packet_sha256"] {
				return errors.New("model hypothesis links must remain source-pinned proposals")
			}
			proposalLinks[hypothesisID] = struct{}{}
		}
	}
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] == "hypothesis" {
			if _, ok := proposalLinks[entity["id"].(string)]; !ok {
				return errors.New("offline hypothesis requires an evidence link")
			}
		}
	}
	for _, card := range bundle["cards"].([]Record) {
		if endpoint, ok := card["endpoint"].(map[string]any); ok {
			if id, ok := endpoint["id"].(string); ok {
				if _, exists := knownEntities[id]; !exists {
					return errors.New("dangling BehaviorGraph card endpoint")
				}
			}
		}
	}
	return nil
}

func insertNormalized(ctx context.Context, db *sql.DB, bundle Record, bundleID, program string) (ImportResult, error) {
	result := ImportResult{
		SchemaVersion: 1, Program: program, ProjectKey: program, BundleID: bundleID,
		Inserted: map[string]int{"sources": 0, "entities": 0, "assertions": 0, "cards": 0},
	}
	var existing int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM imports WHERE id=?", bundleID).Scan(&existing); err != nil {
		return ImportResult{}, wrapImportFailure("graph_import_identity_read_failed", errors.New("BehaviorGraph import identity could not be read"))
	}
	result.Replayed = existing > 0
	for _, source := range bundle["sources"].([]Record) {
		inserted := result.Inserted["sources"]
		if err := insertImmutable(ctx, db, "sources", sourceRefDigest(source), source, &inserted); err != nil {
			return ImportResult{}, wrapImportFailure("graph_sources_write_failed", err)
		}
		result.Inserted["sources"] = inserted
	}
	for _, entity := range bundle["entities"].([]Record) {
		id := entity["id"].(string)
		base := cloneRecord(entity)
		refs := base["source_refs"].([]Record)
		delete(base, "source_refs")
		payload, err := canonicalJSON(base)
		if err != nil {
			return ImportResult{}, wrapImportFailure("graph_entity_encode_failed", errors.New("BehaviorGraph entity could not be encoded"))
		}
		var previous string
		err = db.QueryRowContext(ctx, "SELECT payload FROM entities WHERE id=?", id).Scan(&previous)
		existed := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ImportResult{}, wrapImportFailure("graph_entity_read_failed", errors.New("BehaviorGraph entity could not be read"))
		}
		if !existed {
			if _, err := db.ExecContext(ctx, "INSERT INTO entities(id,payload) VALUES(?,?)", id, string(payload)); err != nil {
				return ImportResult{}, wrapImportFailure("graph_entity_insert_failed", errors.New("BehaviorGraph entity could not be inserted"))
			}
			result.Inserted["entities"]++
		} else {
			stored, err := decodeRecord(previous)
			if err != nil || (!sameCanonical(stored, base) && !compatibleVariantProjection(stored, base, program)) {
				if err != nil {
					return ImportResult{}, wrapImportFailure("graph_entity_identity_conflict", errors.New("entity identity collision; graph history cannot be overwritten"))
				}
				return ImportResult{}, wrapImportFailure("graph_entity_identity_conflict", fmt.Errorf("entity identity collision for kind %s fields %s; graph history cannot be overwritten", base["kind"], strings.Join(recordDifferencePaths(stored, base, ""), ",")))
			}
		}
		addedRefs := 0
		for _, ref := range refs {
			if err := insertEntityRef(ctx, db, id, ref, &addedRefs); err != nil {
				return ImportResult{}, wrapImportFailure("graph_entity_source_refs_write_failed", err)
			}
		}
		if existed && addedRefs > 0 {
			result.UpdatedEntities++
		}
	}
	for _, assertion := range bundle["assertions"].([]Record) {
		inserted := result.Inserted["assertions"]
		if err := insertImmutable(ctx, db, "assertions", assertion["id"].(string), assertion, &inserted); err != nil {
			return ImportResult{}, wrapImportFailure("graph_assertions_write_failed", err)
		}
		result.Inserted["assertions"] = inserted
	}
	for _, card := range bundle["cards"].([]Record) {
		inserted := result.Inserted["cards"]
		if err := insertImmutable(ctx, db, "cards", card["id"].(string), card, &inserted); err != nil {
			return ImportResult{}, wrapImportFailure("graph_cards_write_failed", err)
		}
		result.Inserted["cards"] = inserted
	}
	if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO imports(id) VALUES(?)", bundleID); err != nil {
		return ImportResult{}, wrapImportFailure("graph_import_identity_write_failed", errors.New("BehaviorGraph import identity could not be recorded"))
	}
	return result, nil
}

// recordDifferencePaths reports schema field paths only, never record values.
// It makes identity-collision diagnostics actionable without including graph
// payloads or capture-derived values in errors.
func recordDifferencePaths(left, right any, prefix string) []string {
	if sameCanonical(left, right) {
		return nil
	}
	leftRecord, leftOK := asRecord(left)
	rightRecord, rightOK := asRecord(right)
	if !leftOK || !rightOK {
		if prefix == "" {
			return []string{"record"}
		}
		return []string{prefix}
	}
	keys := make(map[string]struct{}, len(leftRecord)+len(rightRecord))
	for key := range leftRecord {
		keys[key] = struct{}{}
	}
	for key := range rightRecord {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	differences := make([]string, 0)
	for _, key := range ordered {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if sameCanonical(leftRecord[key], rightRecord[key]) {
			continue
		}
		leftChild, leftChildOK := asRecord(leftRecord[key])
		rightChild, rightChildOK := asRecord(rightRecord[key])
		if leftChildOK && rightChildOK {
			differences = append(differences, recordDifferencePaths(leftChild, rightChild, path)...)
		} else {
			differences = append(differences, path)
		}
		if len(differences) >= 16 {
			return differences[:16]
		}
	}
	if len(differences) == 0 {
		return []string{"record"}
	}
	return differences
}

func asRecord(value any) (Record, bool) {
	switch typed := value.(type) {
	case Record:
		return typed, true
	case map[string]any:
		return Record(typed), true
	default:
		return nil, false
	}
}

// compatibleVariantProjection accepts parser-specific safe metadata for the
// same keyed message identity. Python and Go intentionally share variant IDs,
// while their field/header projections have different schemas. Keep the first
// stored projection as canonical and merge only provenance for that identity.
func compatibleVariantProjection(existing, incoming Record, program string) bool {
	kind, ok := existing["kind"].(string)
	if !ok || (kind != "request_variant" && kind != "response_variant") || incoming["kind"] != kind {
		return false
	}
	existingData, okExisting := existing["data"].(map[string]any)
	incomingData, okIncoming := incoming["data"].(map[string]any)
	if !okExisting || !okIncoming || existingData["program"] != program || incomingData["program"] != program {
		return false
	}
	signatureField := "request_signature"
	if kind == "response_variant" {
		signatureField = "response_signature"
	}
	existingSignature, okExisting := existingData[signatureField].(string)
	incomingSignature, okIncoming := incomingData[signatureField].(string)
	if !okExisting || !okIncoming || !graphDigestPattern.MatchString(existingSignature) || existingSignature != incomingSignature {
		return false
	}
	if !sameCanonical(existingData["signature_state"], incomingData["signature_state"]) ||
		!compatibleStringMetadata(existingData["content_type"], incomingData["content_type"], "unknown") ||
		!compatibleNullableMetadata(existingData["body_bytes"], incomingData["body_bytes"]) {
		return false
	}
	if kind == "request_variant" {
		if !compatibleStringMetadata(existingData["method"], incomingData["method"], "UNKNOWN") {
			return false
		}
	}
	if kind == "response_variant" {
		existingStatus, _ := existingData["status"]
		incomingStatus, _ := incomingData["status"]
		if !compatibleNullableMetadata(existingStatus, incomingStatus) {
			return false
		}
	}
	identity := Record{"program": program, signatureField: existingSignature}
	expectedID, err := historicalEntityID(kind, identity)
	if err != nil {
		return false
	}
	return existing["id"] == expectedID && incoming["id"] == expectedID
}

func compatibleStringMetadata(existing, incoming any, unknown string) bool {
	existingValue, existingOK := existing.(string)
	incomingValue, incomingOK := incoming.(string)
	return existingOK && incomingOK && (existingValue == incomingValue || existingValue == unknown || incomingValue == unknown)
}

func compatibleNullableMetadata(existing, incoming any) bool {
	return existing == nil || incoming == nil || sameCanonical(existing, incoming)
}

func insertImmutable(ctx context.Context, db *sql.DB, table, id string, record Record, inserted *int) error {
	payload, err := canonicalJSON(record)
	if err != nil || len(payload) > maxRecordBytes {
		return wrapImportFailure("graph_immutable_record_invalid", errors.New("BehaviorGraph record exceeds the size limit"))
	}
	var previous string
	err = db.QueryRowContext(ctx, "SELECT payload FROM "+table+" WHERE id=?", id).Scan(&previous)
	if err == nil {
		stored, decodeErr := decodeRecord(previous)
		if decodeErr != nil || !sameCanonical(stored, record) {
			code := immutableRecordConflictCode(table, stored, record, decodeErr)
			return wrapImportFailure(code, errors.New("immutable graph record identity collision"))
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return wrapImportFailure("graph_immutable_record_read_failed", errors.New("BehaviorGraph record could not be read"))
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+"(id,payload) VALUES(?,?)", id, string(payload)); err != nil {
		return wrapImportFailure("graph_immutable_record_insert_failed", errors.New("BehaviorGraph record could not be inserted"))
	}
	*inserted++
	return nil
}

func immutableRecordConflictCode(table string, stored, incoming Record, decodeErr error) string {
	if decodeErr != nil || table != "cards" {
		return "graph_immutable_record_conflict"
	}
	for _, key := range []string{
		"schema_version", "capture_id", "program", "source_refs", "endpoint", "context",
		"request_analysis", "response_analysis",
	} {
		if !sameCanonical(stored[key], incoming[key]) {
			return "graph_card_projection_conflict"
		}
	}
	for _, key := range []string{"business_action", "hypotheses", "missing_evidence", "analysis_state"} {
		if !sameCanonical(stored[key], incoming[key]) {
			return "graph_card_analysis_conflict"
		}
	}
	return "graph_immutable_record_conflict"
}

func insertEntityRef(ctx context.Context, db *sql.DB, entityID string, ref Record, added *int) error {
	payload, err := canonicalJSON(ref)
	if err != nil || len(payload) > maxRecordBytes {
		return errors.New("BehaviorGraph provenance reference is invalid")
	}
	digestBytes := sha256.Sum256(payload)
	digest := hex.EncodeToString(digestBytes[:])
	var previous string
	err = db.QueryRowContext(ctx,
		"SELECT payload FROM entity_source_refs WHERE entity_id=? AND source_ref_sha256=?", entityID, digest,
	).Scan(&previous)
	if err == nil {
		if previous != string(payload) {
			return errors.New("BehaviorGraph provenance digest collision")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return errors.New("BehaviorGraph provenance could not be read")
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO entity_source_refs(entity_id,source_ref_sha256,payload) VALUES(?,?,?)", entityID, digest, string(payload),
	); err != nil {
		return fmt.Errorf("BehaviorGraph provenance could not be inserted: %w", err)
	}
	*added++
	return nil
}

func recordList(value any, maximum int) ([]Record, error) {
	items, ok := value.([]any)
	if !ok || len(items) > maximum {
		return nil, errors.New("expected a bounded JSON list")
	}
	result := make([]Record, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok || object == nil {
			return nil, errors.New("expected a JSON object")
		}
		result = append(result, Record(object))
	}
	return result, nil
}

func checkedRecordRefs(record Record, declared map[string]Record) ([]Record, error) {
	refs, err := recordList(record["source_refs"], maxWriterSources)
	if err != nil || len(refs) == 0 {
		return nil, errors.New("every graph record needs provenance")
	}
	for _, ref := range refs {
		if err := validateSource(ref); err != nil {
			return nil, err
		}
		if _, exists := declared[sourcePair(ref)]; !exists {
			return nil, errors.New("graph record references an undeclared source")
		}
	}
	return refs, nil
}

func refsAsAny(refs []Record) []any {
	result := make([]any, len(refs))
	for index, ref := range refs {
		result[index] = map[string]any(ref)
	}
	return result
}

func sortedUniqueRefs(refs []Record) []Record {
	byCanonical := make(map[string]Record, len(refs))
	for _, ref := range refs {
		encoded, err := canonicalJSON(ref)
		if err == nil {
			byCanonical[string(encoded)] = ref
		}
	}
	keys := make([]string, 0, len(byCanonical))
	for key := range byCanonical {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]Record, 0, len(keys))
	for _, key := range keys {
		result = append(result, byCanonical[key])
	}
	return result
}

func uniqueRecords(records []Record) ([]Record, error) {
	byID := make(map[string]Record, len(records))
	for _, record := range records {
		id, _ := record["id"].(string)
		if previous, exists := byID[id]; exists && !sameCanonical(previous, record) {
			return nil, errors.New("conflicting duplicate graph ID")
		}
		byID[id] = record
	}
	return sortedRecordMap(byID), nil
}

func sortedRecordMap(records map[string]Record) []Record {
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]Record, 0, len(ids))
	for _, id := range ids {
		result = append(result, records[id])
	}
	return result
}

func sameCanonical(left, right any) bool {
	leftJSON, leftErr := canonicalJSON(left)
	rightJSON, rightErr := canonicalJSON(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func jsonIntegerEquals(value any, expected int64) bool {
	number, ok := value.(json.Number)
	if !ok || !isJSONInteger(number) {
		return false
	}
	integer, ok := new(big.Int).SetString(number.String(), 10)
	return ok && integer.Cmp(big.NewInt(expected)) == 0
}
