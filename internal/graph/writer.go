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

// ImportResult is deliberately limited to structural import accounting.
type ImportResult struct {
	SchemaVersion   int            `json:"schema_version"`
	Program         string         `json:"program"`
	ProjectKey      string         `json:"project_key"`
	BundleID        string         `json:"bundle_id"`
	Replayed        bool           `json:"replayed"`
	Inserted        map[string]int `json:"inserted"`
	UpdatedEntities int            `json:"updated_entities"`
}

// OpenWriter binds a writer to an explicitly selected ProjectStore. The
// capture/search store must be open read-only; only BehaviorGraph may be
// written by ImportBundles.
func OpenWriter(ctx context.Context, selected *store.ProjectStore) (*Writer, error) {
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil || !selected.Database.ReadOnly() {
		return nil, errors.New("graph writer requires a read-only selected project store")
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
	projectRoot := filepath.Dir(filepath.Dir(projectDir))
	validated, err := store.OpenProjectReadOnly(ctx, selected.Project, projectRoot)
	if err != nil {
		return nil, errors.New("selected project failed store validation")
	}
	validatedMatches := validated.ProjectKey == projectKey &&
		filepath.Clean(validated.Directory) == projectDir &&
		validated.Database != nil && validated.Database.Path() == selected.Database.Path() &&
		filepath.Clean(validated.VaultDirectory) == filepath.Join(projectDir, "vault") &&
		filepath.Clean(selected.VaultDirectory) == filepath.Join(projectDir, "vault") &&
		filepath.Clean(validated.BehaviorGraphPath) == filepath.Join(projectDir, "behavior.sqlite3") &&
		filepath.Clean(selected.BehaviorGraphPath) == filepath.Join(projectDir, "behavior.sqlite3") &&
		filepath.Clean(validated.BehaviorReceiptPath) == filepath.Join(projectDir, "behavior-receipts") &&
		filepath.Clean(selected.BehaviorReceiptPath) == filepath.Join(projectDir, "behavior-receipts")
	closeErr := validated.Close()
	if !validatedMatches || closeErr != nil {
		return nil, errors.New("selected project store paths are inconsistent")
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
		reader, err := OpenReader(ctx, expectedGraph, projectKey, expectedReceipts)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		if reader.schema != 2 {
			return nil, errors.New("Go graph writer supports schema v2 only")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("BehaviorGraph path is unavailable")
	}
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
		return ImportResult{}, errors.New("graph writer is unavailable")
	}
	if len(bundles) == 0 || len(bundles) > maxWriterBundles {
		return ImportResult{}, errors.New("graph batch count is outside the allowed limit")
	}
	normalizedBundles := make([]Record, 0, len(bundles))
	var inputBytes int
	for _, bundle := range bundles {
		encoded, err := marshalBoundedJSON(bundle, MaxBundleJSONBytes)
		if err != nil || len(encoded) == 0 {
			return ImportResult{}, errors.New("graph bundle is invalid or exceeds the size limit")
		}
		inputBytes += len(encoded)
		if inputBytes > MaxBundleJSONBytes {
			return ImportResult{}, errors.New("graph batch exceeds the size limit")
		}
		decoded, err := DecodeBundleJSON(encoded)
		if err != nil {
			return ImportResult{}, err
		}
		normalized, err := w.normalizeBundle(decoded)
		if err != nil {
			return ImportResult{}, err
		}
		normalizedBundles = append(normalizedBundles, normalized)
	}
	merged, err := w.mergeBundles(normalizedBundles)
	if err != nil {
		return ImportResult{}, err
	}
	canonical, err := canonicalJSON(merged)
	if err != nil || len(canonical) > MaxBundleJSONBytes {
		return ImportResult{}, errors.New("normalized graph batch exceeds the size limit")
	}
	digest := sha256.Sum256(canonical)
	bundleID := "bundle-" + hex.EncodeToString(digest[:])
	sources := merged["sources"].([]Record)
	if err := w.verifyReceipts(sources); err != nil {
		return ImportResult{}, err
	}
	return w.writeNormalized(ctx, merged, bundleID)
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
	for _, source := range sourceRows {
		if err := validateSource(source); err != nil {
			return nil, errors.New("bundle source reference is invalid")
		}
		sourcesByPair[sourcePair(source)] = source
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
			if err := mergeImmutableRecord(cards, item); err != nil {
				return nil, err
			}
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

func (w *Writer) writeNormalized(ctx context.Context, bundle Record, bundleID string) (ImportResult, error) {
	db, created, err := w.openGraphDatabase(ctx)
	if err != nil {
		return ImportResult{}, err
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
		return ImportResult{}, errors.New("BehaviorGraph transaction could not begin")
	}
	transactionOpen = true
	if created {
		if err := createV2Schema(ctx, db, w.program); err != nil {
			return ImportResult{}, err
		}
	}
	if err := validateV2Schema(ctx, db, w.program); err != nil {
		return ImportResult{}, err
	}
	if err := validateBatchReferences(ctx, db, bundle); err != nil {
		return ImportResult{}, err
	}
	result, err := insertNormalized(ctx, db, bundle, bundleID, w.program)
	if err != nil {
		return ImportResult{}, err
	}
	if err := w.verifyReceipts(bundle["sources"].([]Record)); err != nil {
		return ImportResult{}, err
	}
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		return ImportResult{}, errors.New("BehaviorGraph transaction could not commit")
	}
	transactionOpen = false
	committed = true
	return result, nil
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
		return ImportResult{}, errors.New("BehaviorGraph import identity could not be read")
	}
	result.Replayed = existing > 0
	for _, source := range bundle["sources"].([]Record) {
		inserted := result.Inserted["sources"]
		if err := insertImmutable(ctx, db, "sources", sourceRefDigest(source), source, &inserted); err != nil {
			return ImportResult{}, err
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
			return ImportResult{}, errors.New("BehaviorGraph entity could not be encoded")
		}
		var previous string
		err = db.QueryRowContext(ctx, "SELECT payload FROM entities WHERE id=?", id).Scan(&previous)
		existed := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ImportResult{}, errors.New("BehaviorGraph entity could not be read")
		}
		if !existed {
			if _, err := db.ExecContext(ctx, "INSERT INTO entities(id,payload) VALUES(?,?)", id, string(payload)); err != nil {
				return ImportResult{}, errors.New("BehaviorGraph entity could not be inserted")
			}
			result.Inserted["entities"]++
		} else {
			stored, err := decodeRecord(previous)
			if err != nil || (!sameCanonical(stored, base) && !compatibleVariantProjection(stored, base, program)) {
				if err != nil {
					return ImportResult{}, errors.New("entity identity collision; graph history cannot be overwritten")
				}
				return ImportResult{}, fmt.Errorf("entity identity collision for kind %s fields %s; graph history cannot be overwritten", base["kind"], strings.Join(recordDifferencePaths(stored, base, ""), ","))
			}
		}
		addedRefs := 0
		for _, ref := range refs {
			if err := insertEntityRef(ctx, db, id, ref, &addedRefs); err != nil {
				return ImportResult{}, err
			}
		}
		if existed && addedRefs > 0 {
			result.UpdatedEntities++
		}
	}
	for _, assertion := range bundle["assertions"].([]Record) {
		inserted := result.Inserted["assertions"]
		if err := insertImmutable(ctx, db, "assertions", assertion["id"].(string), assertion, &inserted); err != nil {
			return ImportResult{}, err
		}
		result.Inserted["assertions"] = inserted
	}
	for _, card := range bundle["cards"].([]Record) {
		inserted := result.Inserted["cards"]
		if err := insertImmutable(ctx, db, "cards", card["id"].(string), card, &inserted); err != nil {
			return ImportResult{}, err
		}
		result.Inserted["cards"] = inserted
	}
	if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO imports(id) VALUES(?)", bundleID); err != nil {
		return ImportResult{}, errors.New("BehaviorGraph import identity could not be recorded")
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
		return errors.New("BehaviorGraph record exceeds the size limit")
	}
	var previous string
	err = db.QueryRowContext(ctx, "SELECT payload FROM "+table+" WHERE id=?", id).Scan(&previous)
	if err == nil {
		stored, decodeErr := decodeRecord(previous)
		if decodeErr != nil || !sameCanonical(stored, record) {
			return errors.New("immutable graph record identity collision")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return errors.New("BehaviorGraph record could not be read")
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+"(id,payload) VALUES(?,?)", id, string(payload)); err != nil {
		return errors.New("BehaviorGraph record could not be inserted")
	}
	*inserted++
	return nil
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
		return errors.New("BehaviorGraph provenance could not be inserted")
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
