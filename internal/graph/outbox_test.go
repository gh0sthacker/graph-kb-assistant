package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"database/sql"
	_ "modernc.org/sqlite"
)

func TestOutboxIsChecksummedIdempotentAndAcknowledgedAfterMaterialization(t *testing.T) {
	ctx := context.Background()
	program := strings.Repeat("a", 64)
	path := filepath.Join(t.TempDir(), "behavior.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if err := ensureNeo4jOutboxSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	bundle := Record{
		"schema_version": json.Number("1"), "program": program,
		"sources": []Record{}, "entities": []Record{}, "assertions": []Record{}, "cards": []Record{},
	}
	encoded, err := canonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	bundleID := "bundle-" + hex.EncodeToString(digest[:])
	if err := enqueueNeo4jOutbox(ctx, db, bundle, bundleID); err != nil {
		t.Fatal(err)
	}
	if err := enqueueNeo4jOutbox(ctx, db, bundle, bundleID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	writer := &Writer{program: program, graphPath: path}
	items, err := writer.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].BundleID != bundleID || items[0].PayloadSHA != hex.EncodeToString(digest[:]) {
		t.Fatalf("pending outbox = %#v", items)
	}
	if err := writer.MarkOutboxApplied(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	if err := writer.MarkOutboxApplied(ctx, items[0]); err != nil {
		t.Fatalf("repeated acknowledgement should be idempotent: %v", err)
	}
	items, err = writer.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("outbox still has %d pending event(s) after acknowledgement", len(items))
	}
}
