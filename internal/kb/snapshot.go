// Package kb reads and validates immutable, provenance-preserving KB snapshots.
// It deliberately has no source-fetching behavior: callers must provide a
// previously materialized snapshot directory.
package kb

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

const ChunkSchema = "bb-kb-chunk-v1"

const maxJSONLRecordBytes = 64 << 20

// SourceDocument retains the original source identity and attribution. Metadata
// is limited to JSON scalar values, matching the Python canonical model.
type SourceDocument struct {
	SourceName     string         `json:"source_name"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Text           string         `json:"text"`
	SourceURL      *string        `json:"source_url"`
	SourceLocator  *string        `json:"source_locator"`
	SourceRevision *string        `json:"source_revision"`
	LicenseID      *string        `json:"license_id"`
	Attribution    *string        `json:"attribution"`
	Metadata       map[string]any `json:"metadata"`
	ContentSHA256  string         `json:"content_sha256"`
}

// CanonicalChunk is one deterministic source-text window with provenance.
type CanonicalChunk struct {
	ChunkID        string         `json:"chunk_id"`
	SourceName     string         `json:"source_name"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Ordinal        int            `json:"ordinal"`
	StartChar      int            `json:"start_char"`
	EndChar        int            `json:"end_char"`
	Text           string         `json:"text"`
	SourceURL      *string        `json:"source_url"`
	SourceLocator  *string        `json:"source_locator"`
	SourceRevision *string        `json:"source_revision"`
	LicenseID      *string        `json:"license_id"`
	Attribution    *string        `json:"attribution"`
	Metadata       map[string]any `json:"metadata"`
	SourceSHA256   string         `json:"source_sha256"`
	ChunkSHA256    string         `json:"chunk_sha256"`
	ChunkSchema    string         `json:"chunk_schema"`
}

// Manifest describes a committed snapshot. Coverage state describes source
// completeness, while ExportComplete only describes file commit completion.
type Manifest struct {
	SchemaVersion              int             `json:"schema_version"`
	SnapshotID                 string          `json:"snapshot_id"`
	SnapshotDir                string          `json:"snapshot_dir"`
	SourceManifestSHA256       string          `json:"source_manifest_sha256"`
	SourceDocumentsSHA256      string          `json:"source_documents_sha256"`
	ChunksSHA256               string          `json:"chunks_sha256"`
	SourceCoverageReportSHA256 *string         `json:"source_coverage_report_sha256"`
	ChunkSchema                string          `json:"chunk_schema"`
	MaxChars                   int             `json:"max_chars"`
	OverlapChars               int             `json:"overlap_chars"`
	SourceCount                int             `json:"source_count"`
	DocumentCount              int             `json:"document_count"`
	ChunkCount                 int             `json:"chunk_count"`
	SourceDocumentsBytes       int64           `json:"source_documents_bytes"`
	ChunksBytes                int64           `json:"chunks_bytes"`
	ExportComplete             bool            `json:"export_complete"`
	SourceCoverageState        string          `json:"source_coverage_state"`
	SourceCoverageReport       json.RawMessage `json:"source_coverage_report"`
}

// Snapshot is a read-only handle to an existing versioned snapshot directory.
type Snapshot struct {
	root     string
	Manifest Manifest
}

// OpenSnapshot validates the manifest identity and basic schema without
// reading the potentially large JSONL files. Call Validate before importing.
func OpenSnapshot(path string) (*Snapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("snapshot path must be a regular version directory")
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot path: %w", err)
	}
	manifestPath := filepath.Join(root, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, errors.New("snapshot manifest is missing or not a regular file")
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return nil, errors.New("snapshot manifest is missing or not a regular file")
	}
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest Manifest
	if err := decodeOneJSON(payload, &manifest); err != nil {
		return nil, errors.New("snapshot manifest is invalid JSON")
	}
	s := &Snapshot{root: root, Manifest: manifest}
	if err := s.validateManifest(); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate checks the JSONL byte digests and sizes, record counts, source
// content hashes, deterministic chunk spans/IDs, and snapshot identity. It
// streams rows and keeps at most one source document and its chunks in memory.
func (s *Snapshot) Validate() error {
	if s == nil {
		return errors.New("nil KB snapshot")
	}
	if err := s.validateManifest(); err != nil {
		return err
	}
	sourcePath := filepath.Join(s.root, "source-documents.jsonl")
	chunkPath := filepath.Join(s.root, "chunks.jsonl")
	sourceRows, err := openJSONL(sourcePath, s.Manifest.SourceDocumentsSHA256, s.Manifest.SourceDocumentsBytes)
	if err != nil {
		return err
	}
	defer sourceRows.close()
	chunkRows, err := openJSONL(chunkPath, s.Manifest.ChunksSHA256, s.Manifest.ChunksBytes)
	if err != nil {
		return err
	}
	defer chunkRows.close()

	seenDocs := make(map[string]struct{}, min(s.Manifest.DocumentCount, 1<<20))
	sourceNames := make(map[string]struct{}, s.Manifest.SourceCount)
	var docCount, chunkCount int
	for {
		line, err := sourceRows.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source document row %d: %w", docCount+1, err)
		}
		doc, err := decodeSourceDocument(line)
		if err != nil {
			return fmt.Errorf("invalid source document row %d: %w", docCount+1, err)
		}
		key := doc.SourceName + "\x00" + doc.SourceID
		if _, ok := seenDocs[key]; ok {
			return fmt.Errorf("duplicate source document identity %q/%q", doc.SourceName, doc.SourceID)
		}
		seenDocs[key] = struct{}{}
		sourceNames[doc.SourceName] = struct{}{}
		if sha256Hex([]byte(doc.Text)) != doc.ContentSHA256 {
			return fmt.Errorf("source document content hash mismatch: %s/%s", doc.SourceName, doc.SourceID)
		}
		expected, err := chunkDocument(doc, s.Manifest.MaxChars, s.Manifest.OverlapChars)
		if err != nil {
			return fmt.Errorf("chunk source document %s/%s: %w", doc.SourceName, doc.SourceID, err)
		}
		for _, expectedChunk := range expected {
			line, err := chunkRows.next()
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("snapshot has fewer chunks than expected at chunk %d", chunkCount)
			}
			if err != nil {
				return fmt.Errorf("read chunk row %d: %w", chunkCount+1, err)
			}
			actual, err := decodeChunk(line)
			if err != nil {
				return fmt.Errorf("invalid chunk row %d: %w", chunkCount+1, err)
			}
			if !reflect.DeepEqual(actual, expectedChunk) {
				return fmt.Errorf("snapshot chunk hash, identity, provenance, or span mismatch at row %d", chunkCount)
			}
			chunkCount++
		}
		docCount++
	}
	if err := sourceRows.finish(s.Manifest.DocumentCount); err != nil {
		return err
	}
	if err := chunkRows.finish(s.Manifest.ChunkCount); err != nil {
		return err
	}
	if docCount == 0 {
		return errors.New("snapshot contains no source documents")
	}
	if len(sourceNames) != s.Manifest.SourceCount {
		return errors.New("snapshot manifest source count mismatch")
	}
	if chunkCount != s.Manifest.ChunkCount {
		return errors.New("snapshot chunk count does not match its source documents")
	}
	if err := sourceRows.checkDigest(s.Manifest.SourceDocumentsSHA256); err != nil {
		return err
	}
	if err := chunkRows.checkDigest(s.Manifest.ChunksSHA256); err != nil {
		return err
	}
	return nil
}

// ForEachDocument streams validated source records to fn. Call Validate first
// when the caller requires proof that the complete snapshot is consistent.
func (s *Snapshot) ForEachDocument(fn func(SourceDocument) error) error {
	if fn == nil {
		return errors.New("nil source document callback")
	}
	rows, err := openJSONL(filepath.Join(s.root, "source-documents.jsonl"), "", -1)
	if err != nil {
		return err
	}
	defer rows.close()
	var count int
	for {
		line, err := rows.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source document row %d: %w", count+1, err)
		}
		doc, err := decodeSourceDocument(line)
		if err != nil {
			return fmt.Errorf("invalid source document row %d: %w", count+1, err)
		}
		if sha256Hex([]byte(doc.Text)) != doc.ContentSHA256 {
			return fmt.Errorf("source document content hash mismatch: %s/%s", doc.SourceName, doc.SourceID)
		}
		if err := fn(doc); err != nil {
			return err
		}
		count++
	}
	return rows.finish(count)
}

// ForEachChunk streams canonical chunks to fn. Call Validate first when the
// caller requires proof that the chunks match the source documents.
func (s *Snapshot) ForEachChunk(fn func(CanonicalChunk) error) error {
	if fn == nil {
		return errors.New("nil chunk callback")
	}
	rows, err := openJSONL(filepath.Join(s.root, "chunks.jsonl"), "", -1)
	if err != nil {
		return err
	}
	defer rows.close()
	var count int
	for {
		line, err := rows.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read chunk row %d: %w", count+1, err)
		}
		chunk, err := decodeChunk(line)
		if err != nil {
			return fmt.Errorf("invalid chunk row %d: %w", count+1, err)
		}
		if err := fn(chunk); err != nil {
			return err
		}
		count++
	}
	return rows.finish(count)
}

func (s *Snapshot) validateManifest() error {
	m := s.Manifest
	if m.SchemaVersion != 1 {
		return errors.New("unsupported snapshot manifest schema")
	}
	if !isSHA256(m.SnapshotID) || m.SnapshotDir != m.SnapshotID || filepath.Base(s.root) != m.SnapshotID {
		return errors.New("snapshot manifest identity does not match its version directory")
	}
	if !m.ExportComplete {
		return errors.New("snapshot export is not marked complete")
	}
	if !isSHA256(m.SourceManifestSHA256) || !isSHA256(m.SourceDocumentsSHA256) || !isSHA256(m.ChunksSHA256) {
		return errors.New("snapshot manifest has an invalid SHA-256 field")
	}
	if m.ChunkSchema != ChunkSchema {
		return errors.New("snapshot chunk schema is unsupported")
	}
	if m.MaxChars < 256 || m.OverlapChars < 0 || m.OverlapChars >= m.MaxChars {
		return errors.New("snapshot manifest has invalid chunk window")
	}
	if m.SourceCount < 1 || m.DocumentCount < 0 || m.ChunkCount < 0 || m.SourceDocumentsBytes < 0 || m.ChunksBytes < 0 {
		return errors.New("snapshot manifest has invalid counts or byte sizes")
	}
	if _, err := s.validateCoverage(); err != nil {
		return err
	}
	identity := map[string]any{
		"source_manifest_sha256":        m.SourceManifestSHA256,
		"source_documents_sha256":       m.SourceDocumentsSHA256,
		"chunks_sha256":                 m.ChunksSHA256,
		"source_coverage_report_sha256": m.SourceCoverageReportSHA256,
		"chunk_schema":                  m.ChunkSchema,
		"max_chars":                     m.MaxChars,
		"overlap_chars":                 m.OverlapChars,
	}
	canonical, err := canonicalJSON(identity)
	if err != nil {
		return fmt.Errorf("canonicalize snapshot identity: %w", err)
	}
	if sha256Hex(canonical) != m.SnapshotID {
		return errors.New("snapshot manifest id does not match its content digests")
	}
	for _, name := range []string{"source-documents.jsonl", "chunks.jsonl"} {
		path := filepath.Join(s.root, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot JSONL file is missing or not a regular file: %s", name)
		}
	}
	return nil
}

func (s *Snapshot) validateCoverage() (map[string]any, error) {
	m := s.Manifest
	state := m.SourceCoverageState
	if state != "DONE" && state != "PARTIAL" && state != "BLOCKED" && state != "unverified" {
		return nil, errors.New("snapshot manifest has invalid source_coverage_state")
	}
	trimmed := bytes.TrimSpace(m.SourceCoverageReport)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		if state != "unverified" || m.SourceCoverageReportSHA256 != nil {
			return nil, errors.New("snapshot coverage state requires a matching coverage report")
		}
		return nil, nil
	}
	var report map[string]any
	if err := decodeOneJSON(trimmed, &report); err != nil || report == nil {
		return nil, errors.New("snapshot source coverage report must be an object")
	}
	reportState, ok := report["coverage_state"].(string)
	if !ok || (reportState != "DONE" && reportState != "PARTIAL" && reportState != "BLOCKED") {
		return nil, errors.New("snapshot source coverage report has invalid coverage_state")
	}
	sources, ok := report["sources"].([]any)
	if !ok || state != reportState || m.SourceCoverageReportSHA256 == nil || !isSHA256(*m.SourceCoverageReportSHA256) {
		return nil, errors.New("snapshot coverage state does not match its report")
	}
	canonical, err := canonicalJSON(report)
	if err != nil {
		return nil, errors.New("snapshot source coverage report is not canonical JSON data")
	}
	canonical = append(canonical, '\n')
	if sha256Hex(canonical) != *m.SourceCoverageReportSHA256 {
		return nil, errors.New("snapshot source coverage report hash mismatch")
	}
	if reportState == "DONE" {
		for _, item := range sources {
			row, ok := item.(map[string]any)
			if !ok || row["state"] != "done" {
				return nil, errors.New("snapshot report claims DONE while a source is not done")
			}
		}
		for _, key := range []string{"blocked_source_count", "partial_source_count"} {
			if n, ok := report[key].(json.Number); !ok || n.String() != "0" {
				return nil, errors.New("snapshot report claims DONE with blocked or partial sources")
			}
		}
		if count, ok := report["source_count"].(json.Number); !ok || count.String() != fmt.Sprint(len(sources)) {
			return nil, errors.New("snapshot report claims DONE with an inconsistent source count")
		}
	}
	return report, nil
}

type jsonlReader struct {
	file         *os.File
	reader       *bufio.Reader
	digest       hash.Hash
	name         string
	expectedHash string
	expectedSize int64
	bytesRead    int64
	rows         int
	ended        bool
}

func openJSONL(path, expectedHash string, expectedSize int64) (*jsonlReader, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("snapshot JSONL file is missing or not a regular file: %s", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open snapshot JSONL %s: %w", filepath.Base(path), err)
	}
	if expectedSize >= 0 && info.Size() != expectedSize {
		file.Close()
		return nil, fmt.Errorf("snapshot manifest byte count mismatch: %s", filepath.Base(path))
	}
	return &jsonlReader{
		file: file, reader: bufio.NewReaderSize(file, 128*1024), digest: sha256.New(),
		name: filepath.Base(path), expectedHash: expectedHash, expectedSize: expectedSize,
	}, nil
}

func (r *jsonlReader) next() ([]byte, error) {
	if r.ended {
		return nil, io.EOF
	}
	line, err := r.reader.ReadBytes('\n')
	if errors.Is(err, io.EOF) {
		r.ended = true
		if len(line) != 0 {
			_, _ = r.digest.Write(line)
			r.bytesRead += int64(len(line))
			return nil, errors.New("JSONL file has an unterminated final row")
		}
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.name, err)
	}
	if len(line) > maxJSONLRecordBytes {
		return nil, fmt.Errorf("JSONL record exceeds %d bytes", maxJSONLRecordBytes)
	}
	_, _ = r.digest.Write(line)
	r.bytesRead += int64(len(line))
	line = line[:len(line)-1]
	if len(line) == 0 {
		return nil, errors.New("JSONL file contains a blank row")
	}
	if !utf8.Valid(line) {
		return nil, errors.New("JSONL row is not valid UTF-8")
	}
	r.rows++
	return line, nil
}

func (r *jsonlReader) finish(expectedRows int) error {
	if expectedRows >= 0 && r.rows != expectedRows {
		return fmt.Errorf("snapshot JSONL row count mismatch: %s", r.name)
	}
	if r.expectedSize >= 0 && r.bytesRead != r.expectedSize {
		return fmt.Errorf("snapshot manifest byte count mismatch: %s", r.name)
	}
	return nil
}

func (r *jsonlReader) checkDigest(expected string) error {
	if expected != "" && hex.EncodeToString(r.digest.Sum(nil)) != expected {
		return fmt.Errorf("snapshot JSONL hash mismatch: %s", r.name)
	}
	return nil
}

func (r *jsonlReader) close() error { return r.file.Close() }

func decodeSourceDocument(line []byte) (SourceDocument, error) {
	var doc SourceDocument
	if err := decodeExactRecord(line, sourceDocumentKeys, &doc); err != nil {
		return doc, err
	}
	if doc.SourceName == "" || doc.SourceID == "" || !isSHA256(doc.ContentSHA256) {
		return doc, errors.New("invalid identity or content hash")
	}
	if err := validateMetadata(doc.Metadata); err != nil {
		return doc, err
	}
	return doc, nil
}

func decodeChunk(line []byte) (CanonicalChunk, error) {
	var chunk CanonicalChunk
	if err := decodeExactRecord(line, chunkKeys, &chunk); err != nil {
		return chunk, err
	}
	if chunk.ChunkSchema != ChunkSchema || chunk.Ordinal < 0 || chunk.StartChar < 0 || chunk.EndChar < chunk.StartChar || strings.TrimSpace(chunk.Text) == "" {
		return chunk, errors.New("invalid chunk schema, ordinal, span, or text")
	}
	if err := validateMetadata(chunk.Metadata); err != nil {
		return chunk, err
	}
	return chunk, nil
}

func decodeExactRecord(line []byte, expected []string, target any) error {
	var fields map[string]json.RawMessage
	if err := decodeOneJSON(line, &fields); err != nil || fields == nil {
		return errors.New("JSONL row must be a JSON object")
	}
	if len(fields) != len(expected) {
		return errors.New("record has unexpected schema")
	}
	for _, key := range expected {
		if _, ok := fields[key]; !ok {
			return errors.New("record has unexpected schema")
		}
	}
	if err := decodeOneJSON(line, target); err != nil {
		return errors.New("record fields have invalid types")
	}
	return nil
}

var sourceDocumentKeys = []string{
	"source_name", "source_id", "title", "text", "source_url", "source_locator",
	"source_revision", "license_id", "attribution", "metadata", "content_sha256",
}

var chunkKeys = []string{
	"chunk_id", "source_name", "source_id", "title", "ordinal", "start_char", "end_char", "text",
	"source_url", "source_locator", "source_revision", "license_id", "attribution", "metadata",
	"source_sha256", "chunk_sha256", "chunk_schema",
}

func validateMetadata(metadata map[string]any) error {
	if metadata == nil {
		return errors.New("record has invalid metadata")
	}
	for key, value := range metadata {
		if key == "" {
			// Empty keys are valid JSON mapping keys; do not reject them.
		}
		switch value.(type) {
		case nil, string, bool, json.Number:
		default:
			return errors.New("record metadata values must be JSON scalars")
		}
		if number, ok := value.(json.Number); ok {
			if _, err := number.Float64(); err != nil {
				return errors.New("record metadata has invalid number")
			}
		}
	}
	return nil
}

func decodeOneJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func chunkDocument(document SourceDocument, maxChars, overlapChars int) ([]CanonicalChunk, error) {
	if maxChars < 256 || overlapChars < 0 || overlapChars >= maxChars {
		return nil, errors.New("invalid chunk window")
	}
	runes := []rune(document.Text)
	if len(runes) == 0 {
		return nil, nil
	}
	chunks := make([]CanonicalChunk, 0, (len(runes)+maxChars-1)/maxChars)
	start := 0
	for start < len(runes) {
		end := min(len(runes), start+maxChars)
		if end < len(runes) {
			paragraph := lastDoubleNewline(runes, start+maxChars/2, end)
			if paragraph > start {
				end = paragraph + 2
			}
		}
		text := string(runes[start:end])
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("chunk text must not be blank")
		}
		chunkHash := sha256Hex([]byte(text))
		identity, err := canonicalJSON(map[string]any{
			"schema":        ChunkSchema,
			"source_name":   document.SourceName,
			"source_id":     document.SourceID,
			"source_sha256": document.ContentSHA256,
			"ordinal":       len(chunks),
			"start_char":    start,
			"end_char":      end,
			"chunk_sha256":  chunkHash,
		})
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, CanonicalChunk{
			ChunkID: sha256Hex(identity), SourceName: document.SourceName, SourceID: document.SourceID,
			Title: document.Title, Ordinal: len(chunks), StartChar: start, EndChar: end, Text: text,
			SourceURL: document.SourceURL, SourceLocator: document.SourceLocator,
			SourceRevision: document.SourceRevision, LicenseID: document.LicenseID,
			Attribution: document.Attribution, Metadata: document.Metadata,
			SourceSHA256: document.ContentSHA256, ChunkSHA256: chunkHash, ChunkSchema: ChunkSchema,
		})
		if end == len(runes) {
			break
		}
		nextStart := max(start+1, end-overlapChars)
		limit := min(len(runes), nextStart+200)
		for i := nextStart; i < limit; i++ {
			if runes[i] == '\n' && i < end {
				nextStart = i + 1
				break
			}
		}
		start = nextStart
	}
	return chunks, nil
}

func lastDoubleNewline(text []rune, start, end int) int {
	start = max(0, start)
	end = min(len(text), end)
	last := -1
	for i := start; i+1 < end; i++ {
		if text[i] == '\n' && text[i+1] == '\n' {
			last = i
		}
	}
	return last
}

func canonicalJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := appendCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonical(out *bytes.Buffer, value any) error {
	if value != nil {
		reflected := reflect.ValueOf(value)
		if reflected.Kind() == reflect.Pointer {
			if reflected.IsNil() {
				out.WriteString("null")
				return nil
			}
			return appendCanonical(out, reflected.Elem().Interface())
		}
	}
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		appendJSONString(out, v)
	case int:
		out.WriteString(fmt.Sprintf("%d", v))
	case int64:
		out.WriteString(fmt.Sprintf("%d", v))
	case uint64:
		out.WriteString(fmt.Sprintf("%d", v))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("non-finite JSON number")
		}
		out.WriteString(strings.ReplaceAll(strings.ReplaceAll(fmt.Sprintf("%g", v), "e-0", "e-"), "e+0", "e+"))
	case json.Number:
		if _, err := v.Float64(); err != nil {
			return err
		}
		out.WriteString(v.String())
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			appendJSONString(out, key)
			out.WriteByte(':')
			if err := appendCanonical(out, v[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON type %T", value)
	}
	return nil
}

func appendJSONString(out *bytes.Buffer, value string) {
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				out.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func sha256Hex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
