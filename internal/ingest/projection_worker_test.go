package ingest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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

func TestProjectionWorkerRecoversAcceptedJobAfterProjectReopen(t *testing.T) {
	ctx := context.Background()
	selected, originalStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, originalStore)
	if err != nil {
		t.Fatal(err)
	}
	result, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "projection-worker-restart-request", ""))
	if err != nil || result.ProjectionState != "pending" || result.GraphMapping != "pending" {
		t.Fatalf("accept job before restart: result=%+v err=%v", result, err)
	}
	var revisionID, checkpointState string
	if err := originalStore.Database.DB().QueryRowContext(ctx,
		"SELECT revision_id FROM projection_jobs WHERE state='pending'",
	).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	if err := originalStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM sync_state WHERE source_key=?", sourceKey,
	).Scan(&checkpointState); err != nil || checkpointState != "done" {
		t.Fatalf("committed historical checkpoint = %q err=%v", checkpointState, err)
	}
	projectDirectory := originalStore.Directory
	dataRoot := filepath.Dir(filepath.Dir(projectDirectory))
	pageWriter.Close()
	if err := originalStore.Close(); err != nil {
		t.Fatal(err)
	}

	maintenance, err := projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	reopened, err := store.OpenProjectForWrite(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	workerResult, err := runProjectionWorkerOnce(ctx, reopened, maintenance,
		ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 3},
		func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
			return true, true, nil
		}, time.Now().UTC())
	if err != nil || workerResult.State != "applied" || workerResult.RevisionID != revisionID {
		t.Fatalf("recover durable job after reopen: result=%+v err=%v", workerResult, err)
	}
	job, err := behaviorgraph.ProjectionJobByRevision(ctx, reopened.Database.DB(), reopened.ProjectKey, revisionID)
	if err != nil || job.State != "applied" {
		t.Fatalf("reopened projection job = %+v err=%v", job, err)
	}
	var graphMappings int
	if err := reopened.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM graph_capture_map WHERE revision_id=?", revisionID,
	).Scan(&graphMappings); err != nil || graphMappings != 1 {
		t.Fatalf("recovered graph mapping count = %d err=%v, want 1", graphMappings, err)
	}
	reopenedWriter, err := OpenPageWriter(ctx, reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedWriter.Close()
	replay, err := writePendingHistoryRevision(ctx, reopenedWriter,
		projectionWorkerExchange(selected, "projection-worker-restart-request", ""))
	if err != nil || replay.ProjectionState != "applied" || replay.GraphMapping != "mapped" || replay.Replayed != 1 {
		t.Fatalf("replay after worker acknowledgment = %+v err=%v", replay, err)
	}
}

func TestLiveTailRefreshesLateResponseAsNewRevisionAndKeepsOlderJobFenced(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, liveWriter := newLiveTailFixture(t)
	requestID := "live-late-response-request"
	responseStatus := 200
	capturedAt := "2026-10-20T12:00:00Z"
	requestRaw := []byte("GET /late HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	absent := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt:           &capturedAt,
		RequestRaw:           requestRaw,
		HasResponse:          false,
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/late",
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}
	completed := cloneLiveFixtureExchange(absent)
	completed.ResponseRaw = []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"late_response_field\":\"refresh-secret-marker\"}")
	completed.HasResponse = true
	completed.ResponseStatus = &responseStatus
	completed.ResponseWireFidelity = "source_bytes"
	oldCursor := "older-page"
	source := &liveTailSourceFixture{
		project: selected,
		pages: []caido.RequestMetadataPage{
			{Requests: []caido.RequestMetadata{{RequestID: requestID, Method: "GET", URL: absent.Target}}},
			{Requests: []caido.RequestMetadata{{RequestID: requestID, Method: "GET", URL: absent.Target}}},
		},
		refreshPages: []caido.RequestMetadataPage{
			{Requests: []caido.RequestMetadata{{RequestID: "unrelated-old-request", Method: "GET", URL: "https://api.fixture.invalid/other"}}, HasMore: true, NextCursor: &oldCursor},
			{Requests: []caido.RequestMetadata{{RequestID: requestID, Method: "GET", URL: absent.Target, StatusCode: &responseStatus}}},
		},
		exchangeSnapshots: map[string][]caido.HttpExchange{requestID: {absent, completed}},
	}
	options := LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}

	first, err := RunLiveTail(ctx, source, liveWriter, selected, options)
	if err != nil || first.State != "idle" || first.RequestsAdded != 1 || first.ResponsesRefreshed != 0 || first.ResponseRefreshPages != 1 {
		t.Fatalf("initial incomplete live capture = %+v err=%v", first, err)
	}
	var originalRevision string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM live_capture_index WHERE request_id=?", requestID,
	).Scan(&originalRevision); err != nil {
		t.Fatal(err)
	}
	second, err := RunLiveTail(ctx, source, liveWriter, selected, options)
	if err != nil || second.State != "idle" || second.RequestsAdded != 0 || second.ResponsesRefreshed != 1 || second.ResponseRefreshPages != 1 || second.ResponseRefreshPending {
		t.Fatalf("late response refresh outside the moving creation window = %+v err=%v", second, err)
	}
	if len(source.refreshCursors) != 2 || source.refreshCursors[0] != "" || source.refreshCursors[1] != oldCursor {
		t.Fatalf("durable response sweep cursors = %v, want initial page then %q", source.refreshCursors, oldCursor)
	}
	responseReplay := cloneLiveFixtureExchange(completed)
	replay, err := liveWriter.WriteResponseRefreshPage(
		ctx, nil,
		caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{RequestID: requestID, Method: "GET", URL: absent.Target, StatusCode: &responseStatus}}},
		caido.HistoryPage{Exchanges: []caido.HttpExchange{responseReplay}},
		map[string]struct{}{},
	)
	clearHistoryPage(&caido.HistoryPage{Exchanges: []caido.HttpExchange{responseReplay}})
	if err != nil || replay.ResponsesRefreshed != 0 || replay.ProjectionState != "not_applicable" {
		t.Fatalf("replaying the accepted response revision = %+v err=%v", replay, err)
	}

	var currentRevision, currentGraphID, responseFieldNames, ftsFields string
	var responseSize int64
	var captureCount, revisionCount, jobCount int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id,graph_observation_id,response_size,field_names_json FROM live_capture_index WHERE request_id=?", requestID,
	).Scan(&currentRevision, &currentGraphID, &responseSize, &responseFieldNames); err != nil {
		t.Fatal(err)
	}
	if currentRevision == originalRevision || responseSize == 0 || !strings.Contains(responseFieldNames, "late_response_field") {
		t.Fatalf("live capture did not advance to a response revision: rev=%q old=%q size=%d fields=%s", currentRevision, originalRevision, responseSize, responseFieldNames)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT fields FROM live_capture_fts WHERE observation_id=(SELECT observation_id FROM live_capture_index WHERE request_id=?)", requestID,
	).Scan(&ftsFields); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ftsFields, "late_response_field") {
		t.Fatalf("live FTS row was not refreshed with response fields: %q", ftsFields)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM live_capture_index WHERE request_id=?", requestID,
	).Scan(&captureCount); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM capture_revision_registry WHERE observation_id=(SELECT observation_id FROM live_capture_index WHERE request_id=?) AND source_kind='live'", requestID,
	).Scan(&revisionCount); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM projection_jobs j JOIN capture_revision_registry r ON r.revision_id=j.revision_id
		 WHERE r.observation_id=(SELECT observation_id FROM live_capture_index WHERE request_id=?)`, requestID,
	).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if captureCount != 1 || revisionCount != 2 || jobCount != 2 {
		t.Fatalf("refresh duplicated the capture or lost immutable work: captures=%d revisions=%d jobs=%d", captureCount, revisionCount, jobCount)
	}
	var oldJobState, currentJobState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM projection_jobs WHERE revision_id=?", originalRevision,
	).Scan(&oldJobState); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM projection_jobs WHERE revision_id=?", currentRevision,
	).Scan(&currentJobState); err != nil {
		t.Fatal(err)
	}
	if oldJobState != "pending" || currentJobState != "pending" {
		t.Fatalf("initial and current projection states = %q, %q, want pending/pending before worker", oldJobState, currentJobState)
	}

	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	workerProjection := func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return true, true, nil
	}
	workerNow := time.Now().UTC().Add(time.Second)
	for _, wantRevision := range []string{originalRevision, currentRevision} {
		workerResult, err := runProjectionWorkerOnce(ctx, projectStore, maintenance,
			ProjectionWorkerOptions{LeaseDuration: time.Minute, MaxAttempts: 3}, workerProjection, workerNow,
		)
		if err != nil || workerResult.State != "applied" || workerResult.RevisionID != wantRevision {
			t.Fatalf("projection job %s = %+v err=%v", wantRevision, workerResult, err)
		}
		workerNow = workerNow.Add(time.Second)
	}
	var graphAfterOldAck string
	var revisionMappings int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT graph_observation_id FROM live_capture_index WHERE request_id=?", requestID,
	).Scan(&graphAfterOldAck); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM live_graph_capture_revisions WHERE observation_id=(SELECT observation_id FROM live_capture_index WHERE request_id=?)", requestID,
	).Scan(&revisionMappings); err != nil {
		t.Fatal(err)
	}
	if graphAfterOldAck != currentGraphID || revisionMappings != 2 {
		t.Fatalf("older job rewound current graph or lost revision mappings: graph=%s current=%s mappings=%d", graphAfterOldAck, currentGraphID, revisionMappings)
	}

	if len(source.fetchedExchanges) != 2 ||
		!allBytesZero(source.fetchedExchanges[0].RequestRaw) ||
		!allBytesZero(source.fetchedExchanges[1].RequestRaw) ||
		!allBytesZero(source.fetchedExchanges[1].ResponseRaw) {
		t.Fatal("live response refresh retained raw fixture bytes")
	}
	if entries, err := os.ReadDir(projectStore.VaultDirectory); err != nil || len(entries) != 0 {
		t.Fatalf("live response refresh wrote raw vault files: entries=%d err=%v", len(entries), err)
	}
	assertAbsentFromProjectFiles(t, projectStore.Directory, "refresh-secret-marker")
}

func TestLiveResponseRefreshIdentityConflictRollsBackCursorAndCapture(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, liveWriter := newLiveTailFixture(t)
	requestID := "live-refresh-conflicting-request"
	capturedAt := "2026-10-20T12:00:00Z"
	initial := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt:           &capturedAt,
		RequestRaw:           []byte("GET /original HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/original",
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}
	checkpoint, err := liveWriter.BeginCycle(ctx, time.Now().UTC(), 90)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := liveWriter.WritePage(ctx, checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{initial}})
	if err != nil || accepted.RequestsAdded != 1 {
		t.Fatalf("accept initial pending response: result=%+v err=%v", accepted, err)
	}
	responseStatus := 200
	conflict := cloneLiveFixtureExchange(initial)
	conflict.Target = "https://api.fixture.invalid/changed"
	conflict.HasResponse = true
	conflict.ResponseRaw = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nchanged")
	conflict.ResponseStatus = &responseStatus
	conflict.ResponseWireFidelity = "source_bytes"
	oldCursor := "refresh-cursor-must-not-advance"
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		"UPDATE live_sync_state SET response_refresh_cursor=? WHERE source_key=?", oldCursor, liveTailSourceKey,
	); err != nil {
		t.Fatal(err)
	}
	_, err = liveWriter.WriteResponseRefreshPage(ctx, &oldCursor,
		caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET", URL: conflict.Target, StatusCode: &responseStatus,
		}}},
		caido.HistoryPage{Exchanges: []caido.HttpExchange{conflict}}, map[string]struct{}{},
	)
	clear(conflict.RequestRaw)
	clear(conflict.ResponseRaw)
	if err == nil || !strings.Contains(err.Error(), "identity conflicts") {
		t.Fatalf("changed request identity refresh error = %v, want identity conflict", err)
	}
	var storedCursor sql.NullString
	var responseSize sql.NullInt64
	var currentRevision string
	var revisions, jobs int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT response_refresh_cursor FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&storedCursor); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT response_size,current_revision_id FROM live_capture_index WHERE request_id=?", requestID,
	).Scan(&responseSize, &currentRevision); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM capture_revision_registry WHERE observation_id=(SELECT observation_id FROM live_capture_index WHERE request_id=?)", requestID,
	).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM projection_jobs",
	).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if !storedCursor.Valid || storedCursor.String != oldCursor || responseSize.Valid || currentRevision == "" || revisions != 1 || jobs != 1 {
		t.Fatalf("conflicting refresh left partial state: cursor=%v response=%v current=%q revisions=%d jobs=%d",
			storedCursor, responseSize, currentRevision, revisions, jobs)
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

func materializeLocalProjection(ctx context.Context, selected *store.ProjectStore, bundles []behaviorgraph.Record) (bool, bool, error) {
	graphWriter, err := behaviorgraph.OpenWriter(ctx, selected)
	if err != nil {
		return false, false, err
	}
	if _, err := graphWriter.ImportBundles(ctx, bundles); err != nil {
		return false, false, err
	}
	return true, true, nil
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
