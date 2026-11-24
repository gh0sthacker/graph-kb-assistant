package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
)

func TestWriteNormalizedReprojectsCardAndPreservesAnalysis(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	receiptRoot := filepath.Join(root, "receipts")
	if err := os.Mkdir(receiptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	program := strings.Repeat("a", 64)
	writer := &Writer{
		program: program, graphPath: filepath.Join(root, "behavior.sqlite3"), receiptRoot: receiptRoot,
	}

	original := cardProjectionTestBundle(program, Record{
		"schema_version": json.Number("1"), "id": "card-" + strings.Repeat("b", 64),
		"capture_id": "capture-1", "program": program, "source_refs": []Record{},
		"endpoint": Record{"path": "/before"}, "context": Record{"origin": "unknown"},
		"request_analysis":  Record{"parse_state": "incomplete"},
		"response_analysis": Record{"parse_state": "incomplete"},
		"business_action":   Record{"known_effect": "unknown"},
		"hypotheses":        []any{Record{"outcome": "candidate", "claim": "synthetic analyst hypothesis"}},
		"missing_evidence":  []any{"analyst-marked gap"}, "analysis_state": "needs_review",
	})
	if _, err := writer.writeNormalized(ctx, original); err != nil {
		t.Fatalf("write initial card: %v", err)
	}

	reprojection := cardProjectionTestBundle(program, Record{
		"schema_version": json.Number("1"), "id": "card-" + strings.Repeat("b", 64),
		"capture_id": "capture-1", "program": program, "source_refs": []Record{},
		"endpoint": Record{"path": "/after"}, "context": Record{"origin": "unknown"},
		"request_analysis":  Record{"parse_state": "complete"},
		"response_analysis": Record{"parse_state": "complete", "status": json.Number("200")},
		"business_action":   Record{"known_effect": "unknown"}, "hypotheses": []any{},
		"missing_evidence": []any{"new parser gap"}, "analysis_state": "incomplete",
	})
	retryBundle := cardProjectionTestBundle(program, cloneRecord(reprojection["cards"].([]Record)[0]))
	result, err := writer.writeNormalized(ctx, reprojection)
	if err != nil {
		t.Fatalf("write updated card projection: %v", err)
	}
	if result.UpdatedCards != 1 {
		t.Fatalf("updated cards = %d, want 1", result.UpdatedCards)
	}
	db, err := sql.Open("sqlite", writer.graphPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := db.QueryRowContext(ctx, "SELECT payload FROM cards WHERE id=?", "card-"+strings.Repeat("b", 64)).Scan(&payload); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	stored, err := decodeRecord(payload)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if stored["endpoint"].(map[string]any)["path"] != "/after" || stored["analysis_state"] != "needs_review" {
		t.Fatalf("stored projection or analysis state was not reconciled")
	}
	if len(stored["hypotheses"].([]any)) != 1 || stored["business_action"].(map[string]any)["known_effect"] != "unknown" {
		t.Fatalf("stored analyst data was not preserved")
	}
	missing := stored["missing_evidence"].([]any)
	if len(missing) != 2 || missing[0] != "analyst-marked gap" || missing[1] != "new parser gap" {
		t.Fatalf("merged missing evidence = %#v", missing)
	}

	items, err := writer.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("pending outbox events = %d, want 2", len(items))
	}
	outboxBundle, err := DecodeBundleJSON(items[1].BundleJSON)
	if err != nil {
		t.Fatal(err)
	}
	outboxCards, ok := outboxBundle["cards"].([]any)
	if !ok || len(outboxCards) != 1 {
		t.Fatal("outbox card list is invalid")
	}
	outboxCard, ok := asRecord(outboxCards[0])
	if !ok || !sameCanonical(outboxCard, stored) {
		t.Fatal("outbox card does not match the committed BehaviorGraph card")
	}
	replayed, err := writer.writeNormalized(ctx, retryBundle)
	if err != nil {
		t.Fatalf("replay updated card projection: %v", err)
	}
	if !replayed.Replayed || replayed.UpdatedCards != 0 {
		t.Fatalf("replay result = replayed %t, updated cards %d", replayed.Replayed, replayed.UpdatedCards)
	}
}

func TestMergeCardProjectionDowngradesStaleReviewWhenParserBecomesIncomplete(t *testing.T) {
	program := strings.Repeat("a", 64)
	stored := Record{
		"schema_version": json.Number("1"), "id": "card-" + strings.Repeat("b", 64),
		"capture_id": "capture-1", "program": program, "source_refs": []Record{},
		"endpoint": Record{"path": "/before"}, "context": Record{"origin": "unknown"},
		"request_analysis":  Record{"parse_state": "complete"},
		"response_analysis": Record{"parse_state": "complete"},
		"business_action":   Record{"known_effect": "unknown"},
		"hypotheses":        []any{Record{"outcome": "candidate"}},
		"missing_evidence":  []any{"retained gap"}, "analysis_state": "needs_review",
	}
	incoming := cloneRecord(stored)
	incoming["endpoint"] = Record{"path": "/after"}
	incoming["request_analysis"] = Record{"parse_state": "incomplete"}
	incoming["analysis_state"] = "incomplete"
	merged, changed, err := mergeCardProjection(stored, incoming)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || merged["analysis_state"] != "incomplete" {
		t.Fatal("reprojection kept needs_review despite incomplete parsing")
	}
	if !sameCanonical(merged["hypotheses"], stored["hypotheses"]) {
		t.Fatal("reprojection discarded the saved hypothesis")
	}
}

func TestMergeBundlesReconcilesDuplicateCardIdentity(t *testing.T) {
	program := strings.Repeat("a", 64)
	stored := Record{
		"schema_version": json.Number("1"), "id": "card-" + strings.Repeat("b", 64),
		"capture_id": "capture-1", "program": program, "source_refs": []Record{},
		"endpoint": Record{"path": "/before"}, "context": Record{"origin": "unknown"},
		"request_analysis":  Record{"parse_state": "complete"},
		"response_analysis": Record{"parse_state": "complete"},
		"business_action":   Record{"known_effect": "unknown"},
		"hypotheses":        []any{Record{"outcome": "candidate"}},
		"missing_evidence":  []any{"stored gap"}, "analysis_state": "needs_review",
	}
	incoming := cloneRecord(stored)
	incoming["endpoint"] = Record{"path": "/after"}
	incoming["hypotheses"] = []any{}
	incoming["missing_evidence"] = []any{"incoming gap"}
	incoming["analysis_state"] = "incomplete"
	writer := &Writer{program: program}
	merged, err := writer.mergeBundles([]Record{
		cardProjectionTestBundle(program, stored), cardProjectionTestBundle(program, incoming),
	})
	if err != nil {
		t.Fatalf("mergeBundles() rejected duplicate card identity: %v", err)
	}
	cards := merged["cards"].([]Record)
	if len(cards) != 1 {
		t.Fatalf("merged card count = %d, want 1", len(cards))
	}
	card := cards[0]
	endpoint, ok := asRecord(card["endpoint"])
	if !ok || endpoint["path"] != "/after" || card["analysis_state"] != "needs_review" {
		t.Fatal("batch merge did not apply the new projection or preserve review state")
	}
	if len(card["hypotheses"].([]any)) != 1 || len(card["missing_evidence"].([]any)) != 2 {
		t.Fatal("batch merge discarded prior analysis or missing evidence")
	}
}

func TestImportBundlesReprojectsCardWithoutDroppingAnalysisOrOutboxParity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	receiptRoot := filepath.Join(root, "receipts")
	if err := os.Mkdir(receiptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	program := project.CaidoProjectRef{InstanceID: "synthetic-instance", ProjectID: "card-reprojection"}
	projectKey, err := program.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(receiptRoot, "synthetic-receipt.json")
	receiptBytes := []byte("synthetic-receipt")
	if err := os.WriteFile(receiptPath, receiptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	receiptDigest := sha256.Sum256(receiptBytes)
	source := Record{
		"path": receiptPath, "sha256": hex.EncodeToString(receiptDigest[:]),
		"record_locator": "$.records[0]",
	}
	status, truncated := 200, false
	exchange := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: program, RequestID: "request-1"},
		RequestRaw:  []byte("GET /items HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		HasResponse: true, Method: "GET", Scheme: "https", Host: "fixture.invalid",
		Port: 443, Target: "https://fixture.invalid/items", ResponseStatus: &status,
		RequestTruncated: &truncated, ResponseTruncated: &truncated,
		RequestWireFidelity: "reconstructed_text", ResponseWireFidelity: "reconstructed_text",
	}
	initial, _, err := BuildHistoricalBundle(exchange, projectKey, source)
	if err != nil {
		t.Fatal(err)
	}
	initialCards := initial["cards"].([]Record)
	initialCards[0]["business_action"] = Record{"known_effect": "unknown", "analyst_note": "retain"}
	initialCards[0]["hypotheses"] = []any{Record{"outcome": "candidate", "claim": "synthetic hypothesis"}}
	initialCards[0]["missing_evidence"] = append(initialCards[0]["missing_evidence"].([]any), "analyst-marked gap")

	writer := &Writer{
		program: projectKey, graphPath: filepath.Join(root, "behavior.sqlite3"), receiptRoot: receiptRoot,
	}
	if _, err := writer.ImportBundle(ctx, initial); err != nil {
		t.Fatalf("import initial valid graph bundle: %v", err)
	}

	reprojection := cloneRecord(initial)
	incomingCard := cloneRecord(initialCards[0])
	requestAnalysis, ok := asRecord(incomingCard["request_analysis"])
	if !ok {
		t.Fatal("initial request analysis is invalid")
	}
	incomingCard["request_analysis"] = cloneRecord(requestAnalysis)
	incomingCard["request_analysis"].(Record)["parse_state"] = "complete"
	responseAnalysis, ok := asRecord(incomingCard["response_analysis"])
	if !ok {
		t.Fatal("initial response analysis is invalid")
	}
	incomingCard["response_analysis"] = cloneRecord(responseAnalysis)
	incomingCard["response_analysis"].(Record)["parse_state"] = "complete"
	incomingCard["business_action"] = Record{"known_effect": "unknown"}
	incomingCard["hypotheses"] = []any{}
	incomingCard["missing_evidence"] = []any{"new parser gap"}
	reprojection["cards"] = []Record{incomingCard}
	retryBundle := cloneRecord(reprojection)
	result, err := writer.ImportBundle(ctx, reprojection)
	if err != nil {
		t.Fatalf("reproject existing card through ImportBundle(): %v", err)
	}
	if result.UpdatedCards != 1 {
		t.Fatalf("updated cards = %d, want 1", result.UpdatedCards)
	}

	reader, err := OpenReader(ctx, writer.graphPath, projectKey, receiptRoot)
	if err != nil {
		t.Fatalf("open reprojected graph: %v", err)
	}
	snapshot, err := reader.Snapshot()
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	storedCards := snapshot["cards"].([]Record)
	if len(storedCards) != 1 {
		t.Fatalf("stored cards = %d, want 1", len(storedCards))
	}
	stored := storedCards[0]
	if stored["analysis_state"] != "incomplete" || stored["request_analysis"].(map[string]any)["parse_state"] != "complete" {
		t.Fatal("new parser projection was not committed consistently")
	}
	if stored["business_action"].(map[string]any)["analyst_note"] != "retain" || len(stored["hypotheses"].([]any)) != 1 {
		t.Fatal("reprojection discarded existing analyst data")
	}
	if !containsStringValue(stored["missing_evidence"].([]any), "analyst-marked gap") ||
		!containsStringValue(stored["missing_evidence"].([]any), "new parser gap") {
		t.Fatal("reprojection did not preserve and extend missing evidence")
	}

	items, err := writer.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("pending outbox events = %d, want 2", len(items))
	}
	outboxBundle, err := DecodeBundleJSON(items[1].BundleJSON)
	if err != nil {
		t.Fatal(err)
	}
	outboxCards, ok := outboxBundle["cards"].([]any)
	if !ok || len(outboxCards) != 1 {
		t.Fatal("outbox card list is invalid")
	}
	outboxCard, ok := asRecord(outboxCards[0])
	if !ok || !sameCanonical(outboxCard, stored) {
		t.Fatal("outbox card differs from the committed BehaviorGraph card")
	}
	replay, err := writer.ImportBundle(ctx, retryBundle)
	if err != nil {
		t.Fatalf("replay normalized input: %v", err)
	}
	if !replay.Replayed || replay.UpdatedCards != 0 {
		t.Fatalf("replay result = replayed %t, updated cards %d", replay.Replayed, replay.UpdatedCards)
	}
}

func containsStringValue(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cardProjectionTestBundle(program string, card Record) Record {
	return Record{
		"schema_version": json.Number("1"), "program": program,
		"sources": []Record{}, "entities": []Record{}, "assertions": []Record{},
		"cards": []Record{card},
	}
}
