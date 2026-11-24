package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/caido"
)

type receiptRecord struct {
	SchemaVersion        int     `json:"schema_version"`
	Source               string  `json:"source"`
	ProjectKey           string  `json:"project_key"`
	HashBasis            string  `json:"hash_basis"`
	ObservationID        string  `json:"observation_id"`
	RequestSHA256        string  `json:"request_sha256"`
	RequestBytes         int     `json:"request_bytes"`
	RequestTruncated     *bool   `json:"request_truncated"`
	RequestWireFidelity  string  `json:"request_wire_fidelity"`
	ResponseSHA256       *string `json:"response_sha256"`
	ResponseBytes        *int    `json:"response_bytes"`
	ResponseTruncated    *bool   `json:"response_truncated"`
	ResponseWireFidelity string  `json:"response_wire_fidelity"`
	CapturedAt           *string `json:"captured_at,omitempty"`
}

type pageReceipt struct {
	SchemaVersion    int             `json:"schema_version"`
	ProjectKey       string          `json:"project_key"`
	Source           string          `json:"source"`
	SourceType       string          `json:"source_type"`
	HashBasis        string          `json:"hash_basis"`
	CursorDigest     string          `json:"cursor_digest"`
	NextCursorDigest *string         `json:"next_cursor_digest"`
	Records          []receiptRecord `json:"records"`
}

func writePageReceipt(directory, projectKey, cursorDigest string, nextCursorDigest *string, exchanges []caido.HttpExchange, observationIDs []string, requestHashes []string, responseHashes []*string) (string, string, []SourceRef, bool, error) {
	records := make([]receiptRecord, len(exchanges))
	for index, exchange := range exchanges {
		var responseSize *int
		if exchange.HasResponse {
			size := len(exchange.ResponseRaw)
			responseSize = &size
		}
		var capturedAt *string
		if exchange.CapturedAt != nil && isTimestamp(*exchange.CapturedAt) {
			value := *exchange.CapturedAt
			capturedAt = &value
		}
		records[index] = receiptRecord{
			SchemaVersion: 1, Source: "caido_http_history", ProjectKey: projectKey,
			HashBasis: "HttpExchange bytes delivered to the parser", ObservationID: observationIDs[index],
			RequestSHA256: requestHashes[index], RequestBytes: len(exchange.RequestRaw),
			RequestTruncated: exchange.RequestTruncated, RequestWireFidelity: exchange.RequestWireFidelity,
			ResponseSHA256: responseHashes[index], ResponseBytes: responseSize,
			ResponseTruncated: exchange.ResponseTruncated, ResponseWireFidelity: exchange.ResponseWireFidelity,
			CapturedAt: capturedAt,
		}
	}
	receipt := pageReceipt{
		SchemaVersion: 1, ProjectKey: projectKey, Source: "caido_http_history_page",
		SourceType: "caido_http_history", HashBasis: "HttpExchange bytes delivered to the parser",
		CursorDigest: cursorDigest, NextCursorDigest: nextCursorDigest, Records: records,
	}
	encoded, err := marshalCanonical(receipt)
	if err != nil || len(encoded) > 32<<20 {
		return "", "", nil, false, errors.New("history receipt exceeds its safe size limit")
	}
	encoded = append(encoded, '\n')
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	projectDirectory := filepath.Join(directory, projectKey)
	if err := ensureReceiptProjectDirectory(directory, projectDirectory); err != nil {
		return "", "", nil, false, err
	}
	// Content-address the immutable receipt so identical page evidence reuses
	// the same source path and graph observation IDs across separate sync runs.
	path := filepath.Join(projectDirectory, "page-"+digest+".receipt.json")
	reused, err := writeImmutableReceipt(projectDirectory, path, encoded, 32<<20)
	if err != nil {
		return "", "", nil, false, errors.New("history receipt could not be published or verified")
	}
	refs := make([]SourceRef, len(records))
	for index, record := range records {
		recordBytes, err := marshalCanonical(record)
		if err != nil || len(recordBytes) == 0 || len(recordBytes)+1 > 4<<20 {
			return "", "", nil, false, errors.New("history record receipt exceeds its safe size limit")
		}
		recordBytes = append(recordBytes, '\n')
		recordDigestBytes := sha256.Sum256(recordBytes)
		recordDigest := hex.EncodeToString(recordDigestBytes[:])
		recordPath := filepath.Join(projectDirectory, observationIDs[index]+"."+recordDigest+".receipt.json")
		if _, err := writeImmutableReceipt(projectDirectory, recordPath, recordBytes, 4<<20); err != nil {
			return "", "", nil, false, errors.New("history record receipt could not be published or verified")
		}
		refs[index] = SourceRef{Path: recordPath, SHA256: recordDigest, RecordLocator: "$"}
	}
	return path, digest, refs, reused, nil
}

func writeImmutableReceipt(directory, path string, encoded []byte, maxBytes int64) (bool, error) {
	if filepath.Dir(path) != directory || len(encoded) == 0 || int64(len(encoded)) > maxBytes {
		return false, errors.New("receipt path or size is invalid")
	}
	for attempt := 0; attempt < 2; attempt++ {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o600 != 0o600 {
				return false, errors.New("existing receipt is not a private regular file")
			}
			existing, readErr := readPrivateFile(path, maxBytes)
			if readErr != nil || !bytes.Equal(existing, encoded) {
				return false, errors.New("existing receipt failed immutable reuse verification")
			}
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("receipt path is unavailable")
		}
		if err := publishImmutable(directory, path, encoded, ".receipt-"); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrExist) {
			return false, err
		}
	}
	return false, errors.New("receipt could not be safely published")
}

func publishImmutable(directory, path string, contents []byte, prefix string) error {
	temp, err := os.CreateTemp(directory, prefix+"*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(tempName, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	_ = dir.Close()
	return err
}

func ensureReceiptProjectDirectory(root, path string) error {
	if filepath.Dir(path) != root {
		return errors.New("receipt namespace escaped the selected project")
	}
	created := false
	if err := os.Mkdir(path, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return errors.New("receipt project directory could not be created")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("receipt project directory is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("receipt project directory is unavailable")
	}
	if created {
		parent, err := os.Open(root)
		if err != nil {
			return errors.New("receipt parent directory could not be opened")
		}
		err = parent.Sync()
		_ = parent.Close()
		if err != nil {
			return errors.New("receipt project directory could not be synchronized")
		}
	}
	return nil
}

func readPrivateFile(path string, maxBytes int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o077 != 0 || before.Mode().Perm()&0o600 != 0o600 || before.Size() > maxBytes {
		return nil, errors.New("private file is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private file could not be opened")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0o077 != 0 ||
		after.Mode().Perm()&0o600 != 0o600 || after.Size() > maxBytes {
		return nil, errors.New("private file changed during validation")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(contents)) > maxBytes {
		return nil, errors.New("private file exceeds its read limit")
	}
	return contents, nil
}

func marshalCanonical(value any) ([]byte, error) {
	return marshalCanonicalMode(value, false)
}

func marshalPythonCanonical(value any) ([]byte, error) {
	return marshalCanonicalMode(value, true)
}

func marshalCanonicalMode(value any, escapeNonASCII bool) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeCanonicalValue(&output, parsed, escapeNonASCII); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonicalValue(output *bytes.Buffer, value any, escapeNonASCII bool) error {
	switch item := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index != 0 {
				output.WriteByte(',')
			}
			if err := writeJSONString(output, key, escapeNonASCII); err != nil {
				return err
			}
			output.WriteByte(':')
			if err := writeCanonicalValue(output, item[key], escapeNonASCII); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	case []any:
		output.WriteByte('[')
		for index, nested := range item {
			if index != 0 {
				output.WriteByte(',')
			}
			if err := writeCanonicalValue(output, nested, escapeNonASCII); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case string:
		return writeJSONString(output, item, escapeNonASCII)
	case json.Number:
		output.WriteString(string(item))
	case nil:
		output.WriteString("null")
	case bool:
		if item {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	default:
		return errors.New("unsupported canonical receipt value")
	}
	return nil
}

func writeJSONString(output *bytes.Buffer, value string, escapeNonASCII bool) error {
	if !utf8.ValidString(value) {
		return errors.New("canonical JSON string is not valid UTF-8")
	}
	const hexDigits = "0123456789abcdef"
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if character < 0x20 {
				output.WriteString(`\u00`)
				output.WriteByte(hexDigits[byte(character)>>4])
				output.WriteByte(hexDigits[byte(character)&0x0f])
			} else if escapeNonASCII && character > 0x7f {
				if character <= 0xffff {
					writeUnicodeEscape(output, uint16(character), hexDigits)
				} else {
					value := uint32(character) - 0x10000
					writeUnicodeEscape(output, uint16(0xd800+(value>>10)), hexDigits)
					writeUnicodeEscape(output, uint16(0xdc00+(value&0x3ff)), hexDigits)
				}
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
	return nil
}

func writeUnicodeEscape(output *bytes.Buffer, value uint16, hexDigits string) {
	output.WriteString(`\u`)
	output.WriteByte(hexDigits[(value>>12)&0x0f])
	output.WriteByte(hexDigits[(value>>8)&0x0f])
	output.WriteByte(hexDigits[(value>>4)&0x0f])
	output.WriteByte(hexDigits[value&0x0f])
}

func isTimestamp(value string) bool {
	if len(value) > 128 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}
