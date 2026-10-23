package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestProjectionWorkerAcknowledgesOlderRevisionWithoutReplacingCurrentMapping(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	pageWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic graph projection interruption")
	}

	requestID := "projection-worker-revision-request"
	first := projectionWorkerExchange(selected, requestID, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nfirst")
	second := projectionWorkerExchange(selected, requestID, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nsecond")
	if _, err := writePendingHistoryRevision(ctx, pageWriter, first); err != nil {
		t.Fatalf("accept first capture revision: %v", err)
	}
	if _, err := writePendingHistoryRevision(ctx, pageWriter, second); err != nil {
		t.Fatalf("accept second capture revision: %v", err)
	}

	var oldRevision, currentRevision string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT revision_id FROM projection_jobs ORDER BY sequence_id LIMIT 1`,
	).Scan(&oldRevision); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", requestID,
	).Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	if oldRevision == currentRevision {
		t.Fatal("test did not create a newer current capture revision")
	}

	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	options := ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 3}
	now := time.Now().UTC()
	projectorCalls := 0
	projector := func(_ context.Context, _ *store.ProjectStore, bundles []behaviorgraph.Record) (bool, bool, error) {
		projectorCalls++
		if len(bundles) != 1 {
			t.Fatalf("worker projected %d bundles, want one durable job", len(bundles))
		}
		return true, true, nil
	}

	result, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, options, projector, now)
	if err != nil || result.State != "applied" || result.RevisionID != oldRevision {
		t.Fatalf("process older revision: result=%+v err=%v", result, err)
	}
	var revisionMappings, currentMappings int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM graph_capture_revisions WHERE revision_id=?", oldRevision,
	).Scan(&revisionMappings); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM graph_capture_map WHERE observation_id=(SELECT observation_id FROM captures WHERE request_id=?)", requestID,
	).Scan(&currentMappings); err != nil {
		t.Fatal(err)
	}
	if revisionMappings != 1 || currentMappings != 0 {
		t.Fatalf("older job replaced the current map or lost its revision mapping: revision=%d current=%d", revisionMappings, currentMappings)
	}

	result, err = runProjectionWorkerOnce(ctx, projectStore, maintenance, options, projector, now.Add(time.Second))
	if err != nil || result.State != "applied" || result.RevisionID != currentRevision {
		t.Fatalf("process current revision: result=%+v err=%v", result, err)
	}
	var mappedRevision, mappedGraphID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT revision_id,graph_observation_id FROM graph_capture_map
		 WHERE observation_id=(SELECT observation_id FROM captures WHERE request_id=?)`, requestID,
	).Scan(&mappedRevision, &mappedGraphID); err != nil {
		t.Fatal(err)
	}
	if mappedRevision != currentRevision || mappedGraphID == "" || projectorCalls != 2 {
		t.Fatalf("current capture graph mapping = revision %s graph %q; calls=%d", mappedRevision, mappedGraphID, projectorCalls)
	}

	result, err = runProjectionWorkerOnce(ctx, projectStore, maintenance, options, projector, now.Add(2*time.Second))
	if err != nil || result.State != "idle" {
		t.Fatalf("empty projection queue: result=%+v err=%v", result, err)
	}
}

func TestProjectionWorkerRetriesUnavailableNeo4jAndHonorsBackoff(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	pageWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic Neo4j outage")
	}
	if _, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "projection-worker-retry-request", "")); err != nil {
		t.Fatalf("accept pending history revision: %v", err)
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()

	now := time.Now().UTC()
	options := ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 2}
	failed, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, options,
		func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
			return true, false, nil
		}, now)
	if err != nil || failed.State != "pending" || failed.ErrorCode != "neo4j_unavailable" || failed.AttemptCount != 1 {
		t.Fatalf("unavailable Neo4j result = %+v err=%v", failed, err)
	}
	var storedState, nextAttempt string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state,next_attempt_at FROM projection_jobs",
	).Scan(&storedState, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if storedState != "pending" || nextAttempt != failed.NextAttempt {
		t.Fatalf("failed job state = %q next=%s, result=%+v", storedState, nextAttempt, failed)
	}

	tooEarly, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, options,
		func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
			t.Fatal("worker called projector before backoff elapsed")
			return false, false, nil
		}, now.Add(time.Second))
	if err != nil || tooEarly.State != "idle" {
		t.Fatalf("backoff-blocked queue: result=%+v err=%v", tooEarly, err)
	}

	deadline, err := time.Parse(projectionWorkerTimestampLayout, failed.NextAttempt)
	if err != nil {
		t.Fatal(err)
	}
	succeeded, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, options,
		func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
			return true, true, nil
		}, deadline.Add(time.Second))
	if err != nil || succeeded.State != "applied" || succeeded.AttemptCount != 2 {
		t.Fatalf("retry after backoff: result=%+v err=%v", succeeded, err)
	}
}

func TestProjectionWorkerValidatesLiveCaptureMappingBeforeAcknowledging(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, liveWriter := newLiveTailFixture(t)
	liveWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic graph projection interruption")
	}
	status := 200
	capturedAt := time.Now().UTC().Format(time.RFC3339Nano)
	requestID := "projection-worker-live-request"
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt:           &capturedAt,
		RequestRaw:           []byte("GET /worker-live HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw:          []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nready"),
		HasResponse:          true,
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/worker-live",
		ResponseStatus:       &status,
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "source_bytes",
	}
	checkpoint, err := liveWriter.BeginCycle(ctx, time.Now().UTC(), 90)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := liveWriter.WritePage(ctx, checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.RequestsAdded != 1 || accepted.ProjectionState != "pending" {
		t.Fatalf("live capture was not accepted with a pending projection: %+v", accepted)
	}

	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	result, err := runProjectionWorkerOnce(ctx, projectStore, maintenance,
		ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 3},
		func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
			return true, true, nil
		}, time.Now().UTC())
	if err != nil || result.State != "applied" {
		t.Fatalf("process live projection: result=%+v err=%v", result, err)
	}
	var jobState, mappedGraphID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT j.state,l.graph_observation_id FROM projection_jobs j
		 JOIN capture_revision_registry r ON r.revision_id=j.revision_id
		 JOIN live_capture_index l ON l.observation_id=r.observation_id
		 WHERE j.revision_id=?`, result.RevisionID,
	).Scan(&jobState, &mappedGraphID); err != nil {
		t.Fatal(err)
	}
	if jobState != "applied" || mappedGraphID == "" {
		t.Fatalf("live projection state=%q mapped graph=%q", jobState, mappedGraphID)
	}
}

func TestProjectionWorkerRequiresMatchingMaintenanceLease(t *testing.T) {
	ctx := context.Background()
	_, projectStore, _ := newLiveTailFixture(t)
	_, err := RunProjectionWorkerOnce(ctx, projectStore, nil, ProjectionWorkerOptions{})
	if err == nil || !strings.Contains(err.Error(), "matching project maintenance lease") {
		t.Fatalf("worker without lease error = %v", err)
	}

	shared, err := projectlock.AcquireRead(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunProjectionWorkerOnce(ctx, projectStore, shared, ProjectionWorkerOptions{})
	_ = shared.Close()
	if err == nil || !strings.Contains(err.Error(), "matching project maintenance lease") {
		t.Fatalf("worker with shared lease error = %v", err)
	}

	exclusive, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = RunProjectionWorkerOnce(ctx, projectStore, exclusive, ProjectionWorkerOptions{})
	if err == nil || !strings.Contains(err.Error(), "matching project maintenance lease") {
		t.Fatalf("worker with closed exclusive lease error = %v", err)
	}
}

func TestProjectionWorkerDoesNotLeaseJobsWhenBackendIsDisabled(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	pageWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic projection interruption")
	}
	if _, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "projection-worker-disabled-backend", "")); err != nil {
		t.Fatalf("accept synthetic pending job: %v", err)
	}
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	result, err := RunProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{})
	if err != nil || result.State != "backend_disabled" {
		t.Fatalf("disabled backend result=%+v err=%v", result, err)
	}
	var state string
	var attempts int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state,attempt_count FROM projection_jobs").Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 0 {
		t.Fatalf("disabled backend consumed job retry state: state=%s attempts=%d", state, attempts)
	}
}

func TestProjectionRetryDelayIsBoundedAndStable(t *testing.T) {
	var minDelay, maxDelay time.Duration
	for attempt := 1; attempt <= 100; attempt++ {
		first := projectionRetryDelay(strings.Repeat("a", 64), attempt)
		second := projectionRetryDelay(strings.Repeat("a", 64), attempt)
		if first != second || first < 0 || first > maxProjectionRetryDelay {
			t.Fatalf("retry delay at attempt %d = %s / %s", attempt, first, second)
		}
	}
	for index := 0; index < 128; index++ {
		delay := projectionRetryDelay(strings.Repeat("a", 64)+string(rune(index+1)), 1)
		if index == 0 || delay < minDelay {
			minDelay = delay
		}
		if index == 0 || delay > maxDelay {
			maxDelay = delay
		}
	}
	if maxDelay-minDelay < time.Millisecond {
		t.Fatalf("retry jitter is too narrow: min=%s max=%s", minDelay, maxDelay)
	}
}

func writePendingHistoryRevision(ctx context.Context, writer *PageWriter, exchange caido.HttpExchange) (PageResult, error) {
	checkpoint, err := writer.BeginRun(ctx)
	if err != nil {
		return PageResult{}, err
	}
	return writer.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
}

func projectionWorkerExchange(selected project.CaidoProjectRef, requestID, response string) caido.HttpExchange {
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		RequestRaw:           []byte("GET /worker HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/worker",
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}
	if response != "" {
		status := 200
		exchange.HasResponse = true
		exchange.ResponseRaw = []byte(response)
		exchange.ResponseStatus = &status
		exchange.ResponseWireFidelity = "source_bytes"
	}
	return exchange
}
