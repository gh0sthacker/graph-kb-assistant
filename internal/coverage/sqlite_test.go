package coverage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSQLiteRoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "private", "coverage.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &Matrix{Run: "run-1"}
	m.SetBudget("xss", 2)
	m.SetBudget("sqli", 1)
	m.Record("xss", Confirmed, "req-1")
	m.Record("sqli", BlockedFirstParty, "first-party blocked")
	if err := store.Save(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Record("xss", RuledOut, "req-2")
	if err := store.Save(ctx, m); err != nil {
		t.Fatal(err)
	}
	other := &Matrix{Run: "run-2"}
	other.SetBudget("xss", 3)
	if err := store.Save(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = %v, %v", info, err)
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.Load(ctx, m.Run)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(m)
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("round-trip = %s; want %s", gotJSON, wantJSON)
	}
	gotOther, err := reader.Load(ctx, other.Run)
	if err != nil || !reflect.DeepEqual(gotOther, other) {
		t.Fatalf("other run = %+v, %v", gotOther, err)
	}
	var rows int
	if err := reader.db.QueryRow("SELECT count(*) FROM coverage_cells WHERE run = ?", m.Run).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("upsert row count = %d, %v", rows, err)
	}
	if _, err := reader.Load(ctx, "missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run error = %v", err)
	}
	if err := reader.Save(ctx, m); err == nil {
		t.Fatal("read-only store accepted write")
	}
}

func TestSQLiteInvalidMatrixDoesNotPartiallySave(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "coverage.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := &Matrix{Run: "invalid", Cells: map[string]*Cell{
		"a": {Class: "a", Status: Confirmed, Tests: 1},
		"z": {Class: "z", Status: "invalid", Tests: 1},
	}}
	if err := store.Save(ctx, m); err == nil {
		t.Fatal("invalid status accepted")
	}
	if _, err := store.Load(ctx, m.Run); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("partial write: %v", err)
	}
	for _, invalid := range []*Matrix{nil, {Run: ""}, {Run: "empty"}, {Run: "bad", Cells: map[string]*Cell{"x": nil}}} {
		if err := store.Save(ctx, invalid); err == nil {
			t.Fatalf("invalid matrix accepted: %+v", invalid)
		}
	}
}

func TestSQLiteReadOnlyMissingDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	if _, err := OpenReadOnly(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenReadOnly error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database was created: %v", err)
	}
}
