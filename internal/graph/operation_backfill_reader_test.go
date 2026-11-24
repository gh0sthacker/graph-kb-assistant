package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestOperationBackfillReaderLoadsOnlyRequestedCardAndCandidateIDs(t *testing.T) {
	ctx := context.Background()
	exchange, program, source, receiptRoot := syntheticBodyFixture(
		t, "operation-backfill-reader", "GET /items/1 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", false,
	)
	bundle, _, err := BuildHistoricalBundle(exchange, program, source)
	if err != nil {
		t.Fatalf("BuildHistoricalBundle(): %v", err)
	}
	graphPath := filepath.Join(t.TempDir(), "behavior.sqlite3")
	writer := &Writer{program: program, graphPath: graphPath, receiptRoot: receiptRoot}
	if _, err := writer.ImportBundle(ctx, bundle); err != nil {
		t.Fatalf("ImportBundle(): %v", err)
	}
	before, err := os.ReadFile(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeDigest := sha256.Sum256(before)

	reader, err := OpenOperationBackfillReader(ctx, graphPath, program, receiptRoot)
	if err != nil {
		t.Fatalf("OpenOperationBackfillReader(): %v", err)
	}
	defer reader.Close()
	captureID := bundle["cards"].([]Record)[0]["capture_id"].(string)
	cards, err := reader.CardsByCapture(ctx, []string{captureID})
	if err != nil {
		t.Fatalf("CardsByCapture(): %v", err)
	}
	card, ok := cards[captureID]
	if !ok || card["capture_id"] != captureID {
		t.Fatalf("requested graph card = %#v, found=%t", card, ok)
	}

	var entityCandidates, assertionCandidates []string
	for _, entity := range bundle["entities"].([]Record) {
		entityCandidates = append(entityCandidates, entity["id"].(string))
	}
	for _, assertion := range bundle["assertions"].([]Record) {
		assertionCandidates = append(assertionCandidates, assertion["id"].(string))
	}
	entities, assertions, err := reader.LookupIDs(ctx, entityCandidates, assertionCandidates)
	if err != nil {
		t.Fatalf("LookupIDs(): %v", err)
	}
	for _, id := range entityCandidates {
		if !entities[id] {
			t.Errorf("entity %q was not found", id)
		}
	}
	for _, id := range assertionCandidates {
		if !assertions[id] {
			t.Errorf("assertion %q was not found", id)
		}
	}

	after, err := os.ReadFile(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	afterDigest := sha256.Sum256(after)
	if hex.EncodeToString(beforeDigest[:]) != hex.EncodeToString(afterDigest[:]) {
		t.Fatal("read-only operation-backfill graph lookup changed the graph database")
	}
}
