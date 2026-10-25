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
