package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteCanonicalJSONMatchesCanonicalJSON(t *testing.T) {
	value := Record{
		"z": "<tag>&\u2028line\u2029",
		"a": []any{
			json.Number("1.500"),
			json.Number("-0"),
			int64(-42),
			uint32(42),
			true,
			false,
			nil,
			Record{"nested": "quote: \\\" and slash: \\\\"},
			string([]byte{'b', 0, '\b', '\f', '\n', '\r', '\t', 0x7f, 0xff}),
			[]Record{{"name": "alpha"}, {"name": "beta"}},
			map[string]any{"y": true, "x": json.Number("1e+20")},
		},
		"empty_object": Record(nil),
		"empty_array":  []Record(nil),
	}
	want, err := canonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err := writeCanonicalJSON(&actual, value); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), want) {
		t.Fatalf("streamed canonical JSON differs from canonicalJSON:\n got %q\nwant %q", actual.String(), want)
	}
}

func TestSnapshotDigestIgnoresBackendRecordOrder(t *testing.T) {
	first := Record{
		"schema_version": json.Number("2"), "program": strings.Repeat("a", 64),
		"sources":    []Record{{"path": "/a", "sha256": strings.Repeat("1", 64), "record_locator": "$"}, {"path": "/b", "sha256": strings.Repeat("2", 64), "record_locator": "$"}},
		"entities":   []Record{{"id": "endpoint-" + strings.Repeat("1", 64), "source_refs": []Record{{"path": "/a", "sha256": strings.Repeat("1", 64), "record_locator": "$"}}}, {"id": "endpoint-" + strings.Repeat("2", 64), "source_refs": []Record{}}},
		"assertions": []Record{}, "cards": []Record{},
	}
	second := Record{
		"schema_version": json.Number("2"), "program": strings.Repeat("a", 64),
		"sources":    []Record{first["sources"].([]Record)[1], first["sources"].([]Record)[0]},
		"entities":   []Record{first["entities"].([]Record)[1], first["entities"].([]Record)[0]},
		"assertions": []Record{}, "cards": []Record{},
	}
	left, err := SnapshotDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := SnapshotDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("snapshot digest differs by record order: %s != %s", left, right)
	}
}

func TestOpenReaderFromEmptySnapshotUsesSharedValidator(t *testing.T) {
	program := strings.Repeat("a", 64)
	receiptRoot := t.TempDir()
	snapshot := Record{
		"schema_version": json.Number("2"), "program": program,
		"sources": []Record{}, "entities": []Record{}, "assertions": []Record{}, "cards": []Record{},
	}
	reader, err := OpenReaderFromSnapshot(context.Background(), snapshot, program, receiptRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Program() != program {
		t.Fatalf("reader program = %q, want selected project", reader.Program())
	}
	loaded, err := reader.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	left, err := SnapshotDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	right, err := SnapshotDigest(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("validated snapshot digest changed: %s != %s", left, right)
	}
}

func TestSnapshotDigestDoesNotMutateInput(t *testing.T) {
	ref := Record{"path": "/a", "sha256": strings.Repeat("1", 64), "record_locator": "$"}
	entity := Record{"id": "endpoint-" + strings.Repeat("1", 64), "source_refs": []Record{ref}}
	snapshot := Record{
		"schema_version": json.Number("2"), "program": strings.Repeat("a", 64),
		"sources": []Record{ref}, "entities": []Record{entity}, "assertions": []Record{}, "cards": []Record{},
	}
	if _, err := SnapshotDigest(snapshot); err != nil {
		t.Fatal(err)
	}
	if len(entity["source_refs"].([]Record)) != 1 {
		t.Fatal("snapshot digest mutated the input provenance")
	}
}
