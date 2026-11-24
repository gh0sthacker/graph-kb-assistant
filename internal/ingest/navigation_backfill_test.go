package ingest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestNavigationBackfillRetainsRevisionsRejectsStaleAndReplaysWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	selected, original, liveWriter := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"start", "target-a", "target-b"} {
		exchange := projectionWorkerExchange(selected, id, "HTTP/1.1 200 OK\r\n\r\n")
		path := "/target?token=synthetic-private-navigation-value"
		if id == "start" {
			path = "/start"
			status := 302
			exchange.ResponseStatus = &status
			exchange.ResponseRaw = []byte("HTTP/1.1 302 Found\r\nLocation: /target?token=synthetic-private-navigation-value\r\n\r\n")
		}
		exchange.Target = "https://api.fixture.invalid" + path
		exchange.RequestRaw = []byte("GET " + path + " HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
		if _, err := writePendingHistoryRevision(ctx, pageWriter, exchange); err != nil {
			t.Fatal(err)
		}
	}
	pageWriter.Close()
	liveWriter.Close()
	maintenance, err := projectlock.Acquire(ctx, original.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	graphWriter, err := behaviorgraph.OpenWriter(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := original.Database.DB().QueryContext(ctx, "SELECT payload_json FROM projection_jobs ORDER BY sequence_id")
	if err != nil {
		t.Fatal(err)
	}
	legacy := []behaviorgraph.Record{}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var bundle behaviorgraph.Record
		if err := json.Unmarshal([]byte(encoded), &bundle); err != nil {
			t.Fatal(err)
		}
		entities, assertions := []behaviorgraph.Record{}, []behaviorgraph.Record{}
		for _, entity := range projectionRecordsMust(t, bundle["entities"]) {
			if entity["kind"] != "url_reference" {
				entities = append(entities, entity)
			}
		}
		for _, assertion := range projectionRecordsMust(t, bundle["assertions"]) {
			switch assertion["predicate"] {
			case "uses_request_url", "referrer_url_matches", "redirect_url_matches":
			default:
				assertions = append(assertions, assertion)
			}
		}
		bundle["entities"], bundle["assertions"] = entities, assertions
		legacy = append(legacy, bundle)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, bundle := range legacy {
		if _, err := graphWriter.ImportBundle(ctx, bundle); err != nil {
			t.Fatal(err)
		}
		// Construct the exact revision mapping that the materialization worker
		// publishes, without acknowledging or replacing accepted job payloads.
		for _, entity := range bundle["entities"].([]behaviorgraph.Record) {
			if entity["kind"] != "observation" {
				continue
			}
			data, _ := operationBackfillRecord(entity["data"])
			if _, err := original.Database.DB().ExecContext(ctx,
				`INSERT INTO graph_capture_revisions(revision_id,graph_observation_id,updated_at)
				 SELECT current_revision_id,?,datetime('now') FROM captures WHERE observation_id=?`, entity["id"], data["capture_id"]); err != nil {
				t.Fatal(err)
			}
		}
	}
	root := filepath.Dir(filepath.Dir(original.Directory))
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	previewCurrent := func(options NavigationBackfillOptions) NavigationBackfillPreview {
		t.Helper()
		readOnly, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected, root, maintenance)
		if err != nil {
			t.Fatal(err)
		}
		defer readOnly.Close()
		preview, err := PreviewHistoricalNavigationBackfill(ctx, readOnly, maintenance, options)
		if err != nil {
			t.Fatal(err)
		}
		return preview
	}
	bounded := previewCurrent(NavigationBackfillOptions{MaxCaptures: 1})
	if bounded.State != "partial" || !bounded.Truncated || bounded.Scanned != 1 {
		t.Fatalf("bounded preview=%+v", bounded)
	}
	preview := previewCurrent(NavigationBackfillOptions{})
	if preview.State != "ready" || preview.EligibleCaptures != 3 || preview.URLReferences != 2 || preview.NewURLReferences != 2 || preview.AssertionLinks != 4 || preview.NewAssertionLinks != 4 {
		t.Fatalf("preview=%+v", preview)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private") || strings.Contains(string(encoded), "bundles") {
		t.Fatal("preview exposes in-memory candidate")
	}
	encoded, err = json.Marshal(preview.Bundles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private") {
		t.Fatal("graph candidate exposes raw URL")
	}
	writable, err := store.OpenProjectForWrite(ctx, selected, root)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	var beforeLedger string
	if err := writable.Database.DB().QueryRowContext(ctx, "SELECT group_concat(payload_sha256,',') FROM projection_jobs ORDER BY sequence_id").Scan(&beforeLedger); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyHistoricalNavigationBackfill(ctx, writable, maintenance, preview, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong digest accepted")
	}
	if _, err := ApplyHistoricalNavigationBackfill(ctx, writable, maintenance, bounded, bounded.CandidateSHA256); err == nil {
		t.Fatal("partial inventory accepted")
	}
	var revision, observation string
	if err := writable.Database.DB().QueryRowContext(ctx, "SELECT revision_id,graph_observation_id FROM graph_capture_revisions LIMIT 1").Scan(&revision, &observation); err != nil {
		t.Fatal(err)
	}
	if _, err := writable.Database.DB().ExecContext(ctx, "UPDATE graph_capture_revisions SET graph_observation_id='missing' WHERE revision_id=?", revision); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyHistoricalNavigationBackfill(ctx, writable, maintenance, preview, preview.CandidateSHA256); err == nil {
		t.Fatal("stale capture mapping accepted")
	}
	if _, err := writable.Database.DB().ExecContext(ctx, "UPDATE graph_capture_revisions SET graph_observation_id=? WHERE revision_id=?", observation, revision); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyHistoricalNavigationBackfill(ctx, writable, maintenance, preview, preview.CandidateSHA256)
	if err != nil || result.State != "committed" || result.BundlesImported != 1 || result.Neo4jDrain != "not_attempted" {
		t.Fatalf("apply=%+v err=%v", result, err)
	}
	replay, err := ApplyHistoricalNavigationBackfill(ctx, writable, maintenance, preview, preview.CandidateSHA256)
	if err != nil || replay.BundlesReplayed != 1 || replay.BundlesImported != 0 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	fresh := previewCurrent(NavigationBackfillOptions{})
	if fresh.CandidateSHA256 != preview.CandidateSHA256 || fresh.NewAssertionLinks != 0 || fresh.NewURLReferences != 0 {
		t.Fatalf("post-apply candidate unstable: %+v", fresh)
	}
	var afterLedger string
	if err := writable.Database.DB().QueryRowContext(ctx, "SELECT group_concat(payload_sha256,',') FROM projection_jobs ORDER BY sequence_id").Scan(&afterLedger); err != nil {
		t.Fatal(err)
	}
	if beforeLedger != afterLedger {
		t.Fatal("navigation enrichment rewrote accepted projection jobs")
	}
	reader, err := behaviorgraph.OpenReader(ctx, writable.BehaviorGraphPath, writable.ProjectKey, writable.BehaviorReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	snapshot, err := reader.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	uses := map[string]int{}
	for _, assertion := range projectionRecordsMust(t, snapshot["assertions"]) {
		if assertion["predicate"] == "uses_request_url" {
			uses[assertion["object"].(string)]++
		}
	}
	foundDuplicate := false
	for _, count := range uses {
		if count == 2 {
			foundDuplicate = true
		}
	}
	if !foundDuplicate {
		t.Fatal("shared URL reference lost duplicate target captures")
	}
}

func TestNavigationBackfillLimitsAndContextAreRequired(t *testing.T) {
	if _, err := PreviewHistoricalNavigationBackfill(context.Background(), nil, nil, NavigationBackfillOptions{}); err == nil {
		t.Fatal("missing maintenance context accepted")
	}
	for _, options := range []NavigationBackfillOptions{{MaxCaptures: -1}, {MaxCaptures: 20001}, {MaxBytes: 1}, {MaxBytes: MaximumNavigationBackfillMaxBytes + 1}} {
		if _, err := navigationBackfillOptions(options); err == nil {
			t.Fatalf("invalid limits accepted: %+v", options)
		}
	}
}
