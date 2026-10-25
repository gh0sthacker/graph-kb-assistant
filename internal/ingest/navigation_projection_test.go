package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

func TestNavigationProjectionSurvivesReopenOutOfOrderDeliveryAndPageReplay(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	selected, original, _ := newLiveTailFixture(t)
	makeExchange := func(id, path, referer, location string) caido.HttpExchange {
		status, reason := 200, "OK"
		if location != "" {
			status, reason = 302, "Found"
		}
		request := "GET " + path + " HTTP/1.1\r\nHost: api.fixture.invalid\r\n"
		if referer != "" {
			request += "Referer: https://api.fixture.invalid" + referer + "\r\n"
		}
		response := fmt.Sprintf("HTTP/1.1 %d %s\r\n", status, reason)
		if location != "" {
			response += "Location: " + location + "\r\n"
		}
		notTruncated := false
		return caido.HttpExchange{
			Ref:        caido.ExchangeRef{Project: selected, RequestID: id},
			RequestRaw: []byte(request + "\r\n"), ResponseRaw: []byte(response + "\r\n"), HasResponse: true,
			Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
			Target: "https://api.fixture.invalid" + path, ResponseStatus: &status,
			RequestTruncated: &notTruncated, ResponseTruncated: &notTruncated,
			RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
		}
	}
	exchanges := []caido.HttpExchange{
		makeExchange("projection-navigation-start", "/start", "", "/next"),
		makeExchange("projection-navigation-context", "/context", "", ""),
		makeExchange("projection-navigation-next", "/next", "/context", ""),
	}
	pageWriter, err := OpenPageWriter(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID, Page: caido.HistoryPage{Exchanges: exchanges},
	})
	pageWriter.Close()
	if err != nil || accepted.Stored != 3 || accepted.ProjectionState != "pending" {
		t.Fatalf("accept related capture page: result=%+v err=%v", accepted, err)
	}
	var revisionID string
	if err := original.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", exchanges[2].Ref.RequestID,
	).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	job, err := behaviorgraph.ProjectionJobByRevision(ctx, original.Database.DB(), original.ProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Minute)
	if _, err := original.Database.DB().ExecContext(ctx,
		"UPDATE projection_jobs SET next_attempt_at=? WHERE revision_id<>?",
		now.Add(time.Hour).Format(projectionWorkerTimestampLayout), revisionID,
	); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Dir(filepath.Dir(original.Directory))
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenProjectForWrite(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	maintenance, err := projectlock.Acquire(ctx, reopened.Directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = maintenance.Close() })
	options := ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 3}
	// Materialize the real local graph. Neo4j readiness is an explicit test seam;
	// this scenario does not connect to a Neo4j service.
	projected, err := runProjectionWorkerOnce(ctx, reopened, maintenance, options, materializeLocalProjection, now)
	if err != nil || projected.State != "applied" || projected.RevisionID != revisionID {
		t.Fatalf("project linked target before its dependencies: result=%+v err=%v", projected, err)
	}
	reader, err := behaviorgraph.OpenReader(ctx, reopened.BehaviorGraphPath, reopened.ProjectKey, reopened.BehaviorReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := reader.Slice(ctx, behaviorgraph.SliceOptions{
		SeedIDs: []string{job.GraphObservationID}, MaxNodes: 100, MaxEdges: 300, MaxDepth: 2,
	})
	_ = reader.Close()
	if err != nil {
		t.Fatalf("read out-of-order graph projection: %v", err)
	}
	assertProjectedNavigationLinks(t, partial)
	for i := 0; i < 2; i++ {
		result, err := runProjectionWorkerOnce(ctx, reopened, maintenance, options,
			materializeLocalProjection, now.Add(2*time.Hour+time.Duration(i)*time.Second))
		if err != nil || result.State != "applied" {
			t.Fatalf("project remaining dependency: result=%+v err=%v", result, err)
		}
	}
	replayWriter, err := OpenPageWriter(ctx, reopened)
	if err != nil {
		t.Fatal(err)
	}
	replayRun, err := replayWriter.BeginRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := replayWriter.WritePage(ctx, HistoryPageInput{
		RunID: replayRun.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchanges[2], exchanges[1], exchanges[0]}},
	})
	replayWriter.Close()
	if err != nil || replayed.Replayed != 3 || replayed.ProjectionState != "applied" {
		t.Fatalf("replay reordered page: result=%+v err=%v", replayed, err)
	}
	replayedJob, err := behaviorgraph.ProjectionJobByRevision(ctx, reopened.Database.DB(), reopened.ProjectKey, revisionID)
	if err != nil || replayedJob.PayloadSHA256 != job.PayloadSHA256 {
		t.Fatalf("page replay changed the durable navigation payload: err=%v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenProjectReadOnly(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	service := graphaccess.NewService()
	defer service.Close(ctx)
	slice, err := search.GraphSliceWithService(ctx, service, readOnly,
		[]string{job.GraphObservationID}, 100, 300, 2, nil)
	if err != nil {
		t.Fatalf("query projected navigation through application graph API: %v", err)
	}
	assertProjectedNavigationLinks(t, slice)
	provenance, ok := slice["capture_provenance"].([]map[string]any)
	if !ok || len(provenance) != 3 {
		t.Fatalf("graph API resolved %d capture revisions; want all three exact endpoints", len(provenance))
	}
	for _, capture := range provenance {
		if capture["request_id"] == "" || capture["revision_id"] == "" {
			t.Fatal("graph API lost exact request or revision identity")
		}
	}
}

func assertProjectedNavigationLinks(t *testing.T, slice map[string]any) {
	t.Helper()
	links := make(map[string]int)
	for _, assertion := range projectionRecordsMust(t, slice["assertions"]) {
		predicate, _ := assertion["predicate"].(string)
		if predicate != "redirects_to" && predicate != "referrer_context_for" {
			continue
		}
		links[predicate]++
		if len(projectionRecordsMust(t, assertion["source_refs"])) != 2 {
			t.Fatalf("projected %s link lost its exact two-source provenance", predicate)
		}
	}
	if links["redirects_to"] != 1 || links["referrer_context_for"] != 1 {
		t.Fatalf("projected navigation links=%v; want exactly one redirect and one Referer link", links)
	}
}

func TestHistoricalURLNavigationAcrossPagesRetainsEveryDuplicateMatch(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	privateValue := "fixture-url-correlation"
	requestIDs := []string{"historical-url-source", "historical-url-target", "historical-url-duplicate", "historical-url-other"}
	paths := []string{"/dir/start", "/dir/next?trace=" + privateValue, "/dir/next?trace=" + privateValue, "/dir/next?trace=other-value"}
	for i, id := range requestIDs {
		exchange := projectionWorkerExchange(selected, id, "HTTP/1.1 200 OK\r\n\r\n")
		exchange.RequestRaw = []byte("GET " + paths[i] + " HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
		exchange.Target = "https://api.fixture.invalid" + paths[i]
		if i == 0 {
			status := 302
			exchange.ResponseStatus = &status
			exchange.ResponseRaw = []byte("HTTP/1.1 302 Found\r\nLocation: next?trace=" + privateValue + "\r\n\r\n")
		}
		if result, err := writePendingHistoryRevision(ctx, pageWriter, exchange); err != nil || result.Stored != 1 {
			t.Fatalf("accept separate historical page %d: result=%+v err=%v", i, result, err)
		}
	}
	pageWriter.Close()
	var targetRevision string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", requestIDs[1],
	).Scan(&targetRevision); err != nil {
		t.Fatal(err)
	}
	targetJob, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, targetRevision)
	if err != nil {
		t.Fatal(err)
	}
	var targetURLID string
	for _, entity := range projectionRecordsMust(t, targetJob.PayloadBundle["entities"]) {
		if entity["kind"] == "url_reference" {
			targetURLID, _ = entity["id"].(string)
		}
	}
	if targetURLID == "" {
		t.Fatal("historical capture job has no project URL reference")
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = maintenance.Close() })
	now := time.Now().UTC().Add(time.Minute)
	for i := range requestIDs {
		// The local graph import is real. Backend readiness is a synthetic seam.
		result, err := runProjectionWorkerOnce(ctx, projectStore, maintenance,
			ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 3}, materializeLocalProjection,
			now.Add(time.Duration(i)*time.Second))
		if err != nil || result.State != "applied" {
			t.Fatalf("project separate page %d: result=%+v err=%v", i, result, err)
		}
	}
	dataRoot := filepath.Dir(filepath.Dir(projectStore.Directory))
	assertAbsentFromProjectFiles(t, projectStore.Directory, privateValue)
	if err := projectStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenProjectReadOnly(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	service := graphaccess.NewService()
	defer service.Close(ctx)
	slice, err := search.GraphSliceWithService(ctx, service, readOnly, []string{targetURLID}, 100, 300, 1, nil)
	if err != nil {
		t.Fatalf("navigate across historical pages using the shared URL: %v", err)
	}
	matches := make(map[string]bool)
	for _, capture := range slice["capture_provenance"].([]map[string]any) {
		id, _ := capture["request_id"].(string)
		matches[id] = true
	}
	if len(matches) != 3 || !matches[requestIDs[0]] || !matches[requestIDs[1]] || !matches[requestIDs[2]] || matches[requestIDs[3]] {
		t.Fatalf("URL navigation lost a duplicate or included a different query: matches=%v", matches)
	}
	links := make(map[string]int)
	for _, assertion := range projectionRecordsMust(t, slice["assertions"]) {
		predicate, _ := assertion["predicate"].(string)
		if predicate == "precedes" || predicate == "redirects_to" {
			t.Fatal("URL equality was promoted to an initiating-request observation link")
		}
		if predicate != "uses_request_url" && predicate != "redirect_url_matches" {
			continue
		}
		links[predicate]++
		context, _ := projectionRecord(assertion["context"])
		if context["link_basis"] != "project_keyed_url_equality" || context["causality"] != "url_match_only" ||
			len(projectionRecordsMust(t, assertion["source_refs"])) != 1 {
			t.Fatal("cross-page URL match lost its exact source or equality-only interpretation")
		}
	}
	if links["uses_request_url"] != 2 || links["redirect_url_matches"] != 1 {
		t.Fatalf("shared URL links=%v; want two matching requests and the redirect source", links)
	}
}
