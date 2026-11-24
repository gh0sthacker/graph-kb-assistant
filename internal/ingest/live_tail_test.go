package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
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

func TestLiveTailOutcomeRequiresUnfinishedWorkForPartialAndExitTwo(t *testing.T) {
	tests := []struct {
		name             string
		pendingResponses int
		limitReached     bool
		cursorPending    bool
		wantState        string
		wantExitCode     int
	}{
		{name: "nothing pending", wantState: "complete", wantExitCode: 0},
		{name: "pending responses", pendingResponses: 1, wantState: "partial", wantExitCode: 2},
		{name: "limit reached", limitReached: true, wantState: "partial", wantExitCode: 2},
		{name: "cursor pending", cursorPending: true, wantState: "partial", wantExitCode: 2},
		{name: "pending and limit", pendingResponses: 1, limitReached: true, wantState: "partial", wantExitCode: 2},
		{name: "pending and cursor", pendingResponses: 1, cursorPending: true, wantState: "partial", wantExitCode: 2},
		{name: "limit and cursor", limitReached: true, cursorPending: true, wantState: "partial", wantExitCode: 2},
		{name: "all pending signals", pendingResponses: 1, limitReached: true, cursorPending: true, wantState: "partial", wantExitCode: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, exitCode := liveTailOutcome(tc.pendingResponses, tc.limitReached, tc.cursorPending)
			if state != tc.wantState || exitCode != tc.wantExitCode {
				t.Fatalf("liveTailOutcome() = (%q, %d), want (%q, %d)", state, exitCode, tc.wantState, tc.wantExitCode)
			}
		})
	}
}

func TestRunLiveTailImportsOnceAndDoesNotPersistRawMessages(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)

	requestRaw := []byte("GET /api/items?query=fixture-query-marker HTTP/1.1\r\n" +
		"Host: api.fixture.invalid\r\n" +
		"X-Fixture-Header: fixture-header-marker\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"name":"item","api_key":"fixture-request-body-marker"}`)
	responseRaw := []byte("HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"result":"ok","token":"fixture-response-body-marker"}`)
	status := 200
	capturedAt := "2026-10-22T12:00:00Z"
	requestID := "fixture-request-1"
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt: &capturedAt, RequestRaw: requestRaw, ResponseRaw: responseRaw,
		HasResponse: true, Method: "GET", Scheme: "https", Host: "api.fixture.invalid",
		Port: 443, Target: "https://api.fixture.invalid/api/items?query=fixture-query-marker",
		ResponseStatus: &status, RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	source := &liveTailSourceFixture{
		project: selected,
		pages: []caido.RequestMetadataPage{{Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET",
			URL: "https://api.fixture.invalid/api/items?query=fixture-query-marker", StatusCode: &status,
		}}}, {Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET",
			URL: "https://api.fixture.invalid/api/items?query=fixture-query-marker", StatusCode: &status,
		}}}},
		exchanges: map[string]caido.HttpExchange{requestID: exchange},
	}
	options := LiveTailOptions{PageSize: 1, MaxPages: 4, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}

	first, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("first RunLiveTail() error = %v", err)
	}
	if first.State != "complete" || first.RequestsSeen != 1 || first.RequestsAdded != 1 || first.PagesCompleted != 1 || first.CursorPending || first.LimitReached || first.ResponseRefreshPending {
		t.Fatalf("first RunLiveTail() result = %+v, want one imported request and an idle checkpoint", first)
	}
	if source.refreshCalls != 1 {
		t.Fatalf("terminal response refresh pages = %d, want the sweep to stop after its terminal page", source.refreshCalls)
	}
	if len(source.fetchedExchanges) != 1 || !allBytesZero(source.fetchedExchanges[0].RequestRaw) || !allBytesZero(source.fetchedExchanges[0].ResponseRaw) {
		t.Fatal("RunLiveTail() retained raw request or response bytes after processing")
	}

	second, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("second RunLiveTail() error = %v", err)
	}
	if second.State != "complete" || second.RequestsSeen != 1 || second.RequestsAdded != 0 || second.PagesCompleted != 1 || second.ResponseRefreshPending {
		t.Fatalf("second RunLiveTail() result = %+v, want an idle deduplicated page", second)
	}
	if source.refreshCalls != 2 {
		t.Fatalf("response refresh pages after second cycle = %d, want one terminal page per cycle", source.refreshCalls)
	}
	if source.fetchCalls != 2 || source.skippedCounts[1] != 1 {
		t.Fatalf("detail fetches = %d, skipped IDs on replay = %d; want 2 fetches and 1 skipped ID", source.fetchCalls, source.skippedCounts[1])
	}

	var count int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_index").Scan(&count); err != nil {
		t.Fatalf("count live captures: %v", err)
	}
	if count != 1 {
		t.Fatalf("live capture count = %d, want 1 after replay", count)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_fts").Scan(&count); err != nil {
		t.Fatalf("count live full-text rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("live full-text row count = %d, want 1 after replay", count)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM captures").Scan(&count); err != nil {
		t.Fatalf("count historical captures: %v", err)
	}
	if count != 0 {
		t.Fatalf("historical capture count = %d, want 0 for live-tail ingestion", count)
	}
	vaultEntries, err := os.ReadDir(projectStore.VaultDirectory)
	if err != nil {
		t.Fatalf("list project vault: %v", err)
	}
	if len(vaultEntries) != 0 {
		t.Fatalf("live-tail created %d vault files; want none", len(vaultEntries))
	}
	var checkpointState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&checkpointState); err != nil {
		t.Fatalf("read live-tail checkpoint: %v", err)
	}
	if checkpointState != "idle" {
		t.Fatalf("checkpoint state = %q, want idle", checkpointState)
	}
	if _, err := os.Stat(projectStore.BehaviorGraphPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live intake materialized the graph before the worker: err=%v", err)
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{}, materializeLocalProjection, time.Now().UTC())
	if closeErr := maintenance.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || workerResult.State != "applied" {
		t.Fatalf("local projection worker = %+v err=%v", workerResult, err)
	}
	if _, err := os.Stat(projectStore.BehaviorGraphPath); err != nil {
		t.Fatalf("BehaviorGraph was not created: %v", err)
	}

	for _, marker := range []string{
		"fixture-query-marker", "fixture-header-marker", "fixture-request-body-marker", "fixture-response-body-marker",
	} {
		// Only hunt's bounded nonsecret samples may retain query/header values.
		// Credentials remain forbidden in every file, including hunt.sqlite3.
		if marker == "fixture-query-marker" || marker == "fixture-header-marker" {
			assertAbsentFromProjectFiles(t, projectStore.Directory, marker, "hunt.sqlite3")
		} else {
			assertAbsentFromProjectFiles(t, projectStore.Directory, marker)
		}
	}
}

func TestRunLiveTailExactOriginScopeFiltersInitialAndResponseRefreshPages(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, unscopedWriter := newLiveTailFixture(t)
	unscopedWriter.Close()
	writer, err := OpenLiveTailWriterForScope(ctx, projectStore, "HTTPS", "API.FIXTURE.INVALID", 443)
	if err != nil {
		t.Fatalf("OpenLiveTailWriterForScope() error = %v", err)
	}
	t.Cleanup(writer.Close)
	status := 200
	requestID := "scoped-request"
	otherPortID := "out-of-scope-port"
	otherHostID := "out-of-scope-host"
	otherSchemeID := "out-of-scope-scheme"
	capturedAt := "2026-10-25T12:00:00Z"
	requestRaw := []byte("GET /api/items HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	withoutResponse := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: selected, RequestID: requestID}, CapturedAt: &capturedAt,
		RequestRaw: requestRaw, HasResponse: false, Method: "GET", Scheme: "https",
		Host: "api.fixture.invalid", Port: 443, Target: "https://api.fixture.invalid/api/items",
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}
	withResponse := cloneLiveFixtureExchange(withoutResponse)
	withResponse.ResponseRaw = []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	withResponse.HasResponse = true
	withResponse.ResponseStatus = &status
	withResponse.ResponseWireFidelity = "source_bytes"
	makeRows := func() []caido.RequestMetadata {
		return []caido.RequestMetadata{
			{RequestID: requestID, Method: "GET", URL: "https://api.fixture.invalid/api/items"},
			{RequestID: otherPortID, Method: "GET", URL: "https://api.fixture.invalid:8443/api/items"},
			{RequestID: otherHostID, Method: "GET", URL: "https://elsewhere.fixture.invalid/api/items"},
			{RequestID: otherSchemeID, Method: "GET", URL: "http://api.fixture.invalid:443/api/items"},
		}
	}
	page := func() caido.RequestMetadataPage { return caido.RequestMetadataPage{Requests: makeRows()} }
	source := &liveTailSourceFixture{
		project:           selected,
		pages:             []caido.RequestMetadataPage{page(), page()},
		refreshPages:      []caido.RequestMetadataPage{page(), page()},
		exchangeSnapshots: map[string][]caido.HttpExchange{requestID: {withoutResponse, withResponse}},
	}
	options := LiveTailOptions{PageSize: 4, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}
	first, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("first scoped RunLiveTail() error = %v", err)
	}
	if first.ScopeScheme != "https" || first.ScopeHost != "api.fixture.invalid" || first.ScopePort != 443 || first.RequestsSeen != 4 || first.RequestsExcluded != 6 ||
		first.RequestsAdded != 1 || !first.ResponseRefreshPending {
		t.Fatalf("first scoped RunLiveTail() = %+v, want only the exact origin imported and pending response retained", first)
	}
	second, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("second scoped RunLiveTail() error = %v", err)
	}
	if second.RequestsSeen != 4 || second.RequestsExcluded != 6 || second.RequestsAdded != 0 || second.ResponsesRefreshed != 1 || second.ResponseRefreshPending {
		t.Fatalf("second scoped RunLiveTail() = %+v, want scoped response refresh and no unrelated rows", second)
	}
	if len(source.fetchedExchanges) != 2 {
		t.Fatalf("detail fetches = %d, want initial request and one in-scope response refresh", len(source.fetchedExchanges))
	}
	for _, exchange := range source.fetchedExchanges {
		if exchange.Ref.RequestID != requestID {
			t.Fatalf("detail fetch escaped exact origin filter: request ID %q", exchange.Ref.RequestID)
		}
		clear(exchange.RequestRaw)
		clear(exchange.ResponseRaw)
	}
	var count int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_index").Scan(&count); err != nil {
		t.Fatalf("count scoped captures: %v", err)
	}
	if count != 1 {
		t.Fatalf("live capture count = %d, want exactly one in-scope capture", count)
	}
}

func TestLiveTailScopeUsesIndependentDurableCheckpoint(t *testing.T) {
	ctx := context.Background()
	_, projectStore, unscopedWriter := newLiveTailFixture(t)
	unscopedWriter.Close()
	first, err := OpenLiveTailWriterForScope(ctx, projectStore, "https", "api.fixture.invalid", 8443)
	if err != nil {
		t.Fatalf("first scoped writer: %v", err)
	}
	defer first.Close()
	second, err := OpenLiveTailWriterForScope(ctx, projectStore, "http", "api.fixture.invalid", 8443)
	if err != nil {
		t.Fatalf("second scoped writer: %v", err)
	}
	defer second.Close()
	if first.sourceKey == second.sourceKey {
		t.Fatal("distinct origins share one durable live-tail checkpoint key")
	}
	firstCheckpoint, err := first.BeginCycle(ctx, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatalf("first BeginCycle(): %v", err)
	}
	if err := first.MarkPartial(ctx, firstCheckpoint); err != nil {
		t.Fatalf("mark first scope partial: %v", err)
	}
	secondCheckpoint, err := second.BeginCycle(ctx, time.Date(2026, 10, 25, 12, 1, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatalf("second BeginCycle(): %v", err)
	}
	if secondCheckpoint.Watermark != nil || secondCheckpoint.WindowUntil == firstCheckpoint.WindowUntil {
		t.Fatalf("second scope reused first scope checkpoint: first=%+v second=%+v", firstCheckpoint, secondCheckpoint)
	}
	if err := second.MarkPartial(ctx, secondCheckpoint); err != nil {
		t.Fatalf("mark second scope partial: %v", err)
	}
	var count int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_sync_state WHERE source_key LIKE ?", liveTailSourceKey+":origin:%").Scan(&count); err != nil {
		t.Fatalf("count scoped checkpoints: %v", err)
	}
	if count != 2 {
		t.Fatalf("scoped checkpoint count = %d, want 2", count)
	}
}

func TestNormalizeLiveTailScopeRejectsInvalidOrigin(t *testing.T) {
	for _, scope := range []*LiveTailScope{
		{Scheme: "", Host: "api.fixture.invalid", Port: 443},
		{Scheme: "ftp", Host: "api.fixture.invalid", Port: 443},
		{Scheme: "https", Host: "*.fixture.invalid", Port: 443},
		{Scheme: "https", Host: "api.fixture.invalid", Port: 0},
		{Scheme: "https", Host: "api.fixture.invalid/path", Port: 443},
	} {
		if _, _, err := normalizeLiveTailScope(scope); err == nil {
			t.Fatalf("normalizeLiveTailScope(%+v) succeeded; want invalid scope rejected", scope)
		}
	}
	for _, request := range []caido.RequestMetadata{
		{URL: "https://api.fixture.invalid:bad/path"},
		{URL: "ftp://api.fixture.invalid/path"},
		{URL: "/relative/path"},
		{URL: "https://user:secret@api.fixture.invalid/path"},
	} {
		if metadataMatchesLiveScope(request, LiveTailScope{Scheme: "https", Host: "api.fixture.invalid", Port: 443}) {
			t.Fatalf("malformed or unsupported URL matched exact scope: %q", request.URL)
		}
	}
	if metadataMatchesLiveScope(caido.RequestMetadata{URL: "http://api.fixture.invalid:443/path"}, LiveTailScope{Scheme: "https", Host: "api.fixture.invalid", Port: 443}) {
		t.Fatal("HTTP URL matched an HTTPS scope on the same host and port")
	}
	if !metadataMatchesLiveScope(caido.RequestMetadata{URL: "HTTPS://API.FIXTURE.INVALID/path"}, LiveTailScope{Scheme: "https", Host: "api.fixture.invalid", Port: 443}) {
		t.Fatal("uppercase HTTPS URL did not match normalized exact origin")
	}
}

func TestRunLiveTailReturnsPendingWithoutSynchronousProjection(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	status := 200
	capturedAt := "2026-10-23T12:00:00Z"
	requestID := "projection-failure-live-request"
	exchange := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt:  &capturedAt,
		RequestRaw:  []byte("GET /health HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok"),
		HasResponse: true, Method: "GET", Scheme: "https", Host: "api.fixture.invalid",
		Port: 443, Target: "https://api.fixture.invalid/health", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	source := &liveTailSourceFixture{
		project: selected,
		pages: []caido.RequestMetadataPage{{Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET", URL: exchange.Target, StatusCode: &status,
		}}}},
		exchanges: map[string]caido.HttpExchange{requestID: exchange},
	}
	result, err := RunLiveTail(ctx, source, writer, selected, LiveTailOptions{
		PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("durable live capture acceptance failed: %v", err)
	}
	if result.State != "complete" || result.RequestsAdded != 1 || result.ProjectionPendingPages != 1 {
		t.Fatalf("live result does not show accepted capture with pending projection: %+v", result)
	}
	var pendingJobs, graphMappings int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs WHERE state='pending'").Scan(&pendingJobs); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM graph_capture_map").Scan(&graphMappings); err != nil {
		t.Fatal(err)
	}
	if pendingJobs != 1 || graphMappings != 0 {
		t.Fatalf("live intake did not leave projection queued: pending_jobs=%d graph_mappings=%d", pendingJobs, graphMappings)
	}

	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	workerResult, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{}, func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return true, true, nil
	}, time.Now().UTC())
	if err != nil || workerResult.State != "applied" {
		t.Fatalf("background projection worker result = %+v err=%v", workerResult, err)
	}
	var appliedJobs int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM projection_jobs WHERE state='applied'",
	).Scan(&appliedJobs); err != nil {
		t.Fatal(err)
	}
	if appliedJobs != 1 {
		t.Fatalf("applied live projection job count = %d, want 1", appliedJobs)
	}
}

func TestProjectionWorkerAckRejectsGraphMappingChangedAfterAcceptance(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	status := 200
	capturedAt := "2026-10-23T12:00:00Z"
	requestID := "projection-mapping-live-request"
	exchange := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt:  &capturedAt,
		RequestRaw:  []byte("GET /health HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok"),
		HasResponse: true, Method: "GET", Scheme: "https", Host: "api.fixture.invalid",
		Port: 443, Target: "https://api.fixture.invalid/health", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	source := &liveTailSourceFixture{
		project: selected,
		pages: []caido.RequestMetadataPage{{Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET", URL: exchange.Target, StatusCode: &status,
		}}}},
		exchanges: map[string]caido.HttpExchange{requestID: exchange},
	}
	result, err := RunLiveTail(ctx, source, writer, selected, LiveTailOptions{
		PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("RunLiveTail() error = %v", err)
	}
	if result.RequestsAdded != 1 || result.ProjectionPendingPages != 1 {
		t.Fatalf("changed graph mapping should leave the projection pending: %+v", result)
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	workerResult, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{}, func(_ context.Context, selected *store.ProjectStore, _ []behaviorgraph.Record) (bool, bool, error) {
		if _, err := selected.Database.DB().ExecContext(ctx,
			"UPDATE live_capture_index SET graph_observation_id=? WHERE request_id=?",
			"observation-"+strings.Repeat("f", 64), requestID,
		); err != nil {
			t.Fatalf("simulate a changed live graph mapping: %v", err)
		}
		return true, true, nil
	}, time.Now().UTC())
	if err != nil || workerResult.State != "pending" || workerResult.ErrorCode != "mapping_invalid" {
		t.Fatalf("mapping mismatch should reject worker acknowledgment: result=%+v err=%v", workerResult, err)
	}
	var state string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM projection_jobs WHERE revision_id=(SELECT revision_id FROM live_capture_index WHERE request_id=?)", requestID,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("projection job state = %q, want pending after live mapping mismatch", state)
	}
}

func TestRunLiveTailMarksCheckpointPartialWhenCaidoReadFails(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	readErr := errors.New("fixture Caido read failure")
	source := &liveTailSourceFixture{project: selected, pageErr: readErr}

	result, err := RunLiveTail(ctx, source, writer, selected,
		LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10},
	)
	if !errors.Is(err, readErr) {
		t.Fatalf("RunLiveTail() error = %v, want fixture read error", err)
	}
	if result.State != "complete" || result.PagesCompleted != 0 || result.RequestsAdded != 0 {
		t.Fatalf("RunLiveTail() result = %+v, want no unfinished work after the read error", result)
	}
	var checkpointState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&checkpointState); err != nil {
		t.Fatalf("read live-tail checkpoint: %v", err)
	}
	if checkpointState != "partial" {
		t.Fatalf("checkpoint state = %q, want partial", checkpointState)
	}
}

func TestRunLiveTailResumesFromCommittedCursorAfterStoreRestart(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	dataRoot := filepath.Dir(filepath.Dir(projectStore.Directory))

	firstRequestRaw := []byte("GET /api/first HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\nfirst-page-marker")
	secondRequestRaw := []byte("GET /api/second HTTP/1.1\r\nHost: api.fixture.invalid\r\n" +
		"Referer: https://api.fixture.invalid/api/first\r\n\r\nsecond-page-marker")
	firstID, secondID := "fixture-request-first", "fixture-request-second"
	firstExchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: selected, RequestID: firstID}, RequestRaw: firstRequestRaw,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/api/first", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}
	secondExchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: selected, RequestID: secondID}, RequestRaw: secondRequestRaw,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/api/second", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}
	nextCursor := "fixture-cursor-next-page"
	firstPage := caido.RequestMetadataPage{
		Requests:   []caido.RequestMetadata{{RequestID: firstID, Method: "GET", URL: firstExchange.Target}},
		NextCursor: &nextCursor, HasMore: true,
	}
	secondPage := caido.RequestMetadataPage{
		Requests: []caido.RequestMetadata{{RequestID: secondID, Method: "GET", URL: secondExchange.Target}},
	}
	source := &liveTailSourceFixture{
		project: selected, pages: []caido.RequestMetadataPage{firstPage, secondPage},
		exchanges: map[string]caido.HttpExchange{firstID: firstExchange, secondID: secondExchange},
	}
	options := LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}

	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatalf("acquire first-cycle project lease: %v", err)
	}
	t.Cleanup(func() { _ = maintenance.Close() })
	firstRun, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("first RunLiveTail() error = %v", err)
	}
	if firstRun.State != "partial" || firstRun.RequestsAdded != 1 || firstRun.PagesCompleted != 1 || !firstRun.CursorPending || !firstRun.LimitReached {
		t.Fatalf("first RunLiveTail() result = %+v, want a committed partial page with a pending cursor", firstRun)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatalf("close first-cycle project lease: %v", err)
	}
	writer.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close project store before restart: %v", err)
	}
	projectStore, err = store.OpenProjectForWrite(ctx, selected, dataRoot)
	if err != nil {
		t.Fatalf("reopen project store after restart: %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })
	writer, err = OpenLiveTailWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("reopen live-tail writer after restart: %v", err)
	}
	t.Cleanup(writer.Close)

	maintenance, err = projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatalf("acquire resumed-cycle project lease: %v", err)
	}
	t.Cleanup(func() { _ = maintenance.Close() })
	secondRun, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("resumed RunLiveTail() error = %v", err)
	}
	if secondRun.State != "partial" || secondRun.RequestsSeen != 1 || secondRun.RequestsAdded != 1 || secondRun.PagesCompleted != 1 || secondRun.PendingResponses == 0 {
		t.Fatalf("resumed RunLiveTail() result = %+v, want pending responses", secondRun)
	}
	var refererKey sql.NullString
	var secondRevisionID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT referer_url_key,current_revision_id FROM live_capture_index WHERE request_id=?`, secondID,
	).Scan(&refererKey, &secondRevisionID); err != nil || !refererKey.Valid || !validDigest(refererKey.String) {
		t.Fatalf("second-page keyed Referer index = %q, %v; want a persisted HMAC key (err=%v)", refererKey.String, refererKey.Valid, err)
	}
	var secondPayload []byte
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT payload_json FROM projection_jobs WHERE revision_id=?", secondRevisionID,
	).Scan(&secondPayload); err != nil {
		t.Fatalf("read second-page projection job: %v", err)
	}
	secondBundle, err := behaviorgraph.DecodeBundleJSON(secondPayload)
	if err != nil {
		t.Fatalf("decode second-page projection job: %v", err)
	}
	assertions, ok := projectionRecords(secondBundle["assertions"])
	if !ok {
		t.Fatal("second-page assertions are malformed")
	}
	var firstGraphObservationID, secondGraphObservationID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT graph_observation_id FROM live_capture_index WHERE request_id=?", firstID,
	).Scan(&firstGraphObservationID); err != nil {
		t.Fatalf("read first graph observation ID: %v", err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT graph_observation_id FROM live_capture_index WHERE request_id=?", secondID,
	).Scan(&secondGraphObservationID); err != nil {
		t.Fatalf("read second graph observation ID: %v", err)
	}
	linked := false
	for _, assertion := range assertions {
		if assertion["predicate"] != "referrer_context_for" {
			continue
		}
		linked = assertion["subject"] == firstGraphObservationID && assertion["object"] == secondGraphObservationID
		context, _ := projectionRecord(assertion["context"])
		if context["link_basis"] != "referrer_exact_url" || context["causality"] != "navigation_evidence_only" {
			t.Fatalf("cross-page Referer context = %#v", context)
		}
		if len(projectionRecordsMust(t, assertion["source_refs"])) != 2 {
			t.Fatal("cross-page Referer assertion must cite both capture receipts")
		}
	}
	if !linked {
		t.Fatal("second page did not link its exact Referer to the first page observation")
	}
	if bytes.Contains(secondPayload, []byte("https://api.fixture.invalid/api/first")) {
		t.Fatal("cross-page graph projection persisted the matched URL value instead of its HMAC")
	}
	if len(source.cursors) != 2 || source.cursors[0] != "" || source.cursors[1] != nextCursor {
		t.Fatalf("metadata cursors = %v, want [empty %q]", source.cursors, nextCursor)
	}
	if len(source.httpqlFilters) != 2 || source.httpqlFilters[0] != source.httpqlFilters[1] {
		t.Fatalf("resumed live-tail time filter changed: %v", source.httpqlFilters)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatalf("close resumed-cycle project lease: %v", err)
	}
	if len(source.fetchedExchanges) != 2 || !allBytesZero(source.fetchedExchanges[0].RequestRaw) || !allBytesZero(source.fetchedExchanges[1].RequestRaw) {
		t.Fatal("resumed RunLiveTail() retained raw request bytes after processing")
	}
	writer.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close project store before replay restart: %v", err)
	}
	projectStore, err = store.OpenProjectForWrite(ctx, selected, dataRoot)
	if err != nil {
		t.Fatalf("reopen project store before replay: %v", err)
	}
	writer, err = OpenLiveTailWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("reopen live-tail writer before replay: %v", err)
	}
	t.Cleanup(writer.Close)

	replay := &liveTailSourceFixture{
		project: selected,
		pages: []caido.RequestMetadataPage{{Requests: []caido.RequestMetadata{
			{RequestID: firstID, Method: "GET", URL: firstExchange.Target},
			{RequestID: secondID, Method: "GET", URL: secondExchange.Target},
		}}},
	}
	maintenance, err = projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatalf("acquire replay-cycle project lease: %v", err)
	}
	t.Cleanup(func() { _ = maintenance.Close() })
	replayOptions := options
	replayOptions.PageSize = 2
	replayRun, err := RunLiveTail(ctx, replay, writer, selected, replayOptions)
	if err != nil {
		t.Fatalf("replayed RunLiveTail() error = %v", err)
	}
	if replayRun.State != "partial" || replayRun.RequestsSeen != 2 || replayRun.RequestsAdded != 0 || replayRun.PagesCompleted != 1 || replayRun.PendingResponses == 0 {
		t.Fatalf("replayed RunLiveTail() result = %+v, want deduplicated requests with pending responses", replayRun)
	}
	if len(replay.skippedCounts) != 1 || replay.skippedCounts[0] != 2 {
		t.Fatalf("replay skip counts = %v, want both known request IDs skipped", replay.skippedCounts)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatalf("close replay-cycle project lease: %v", err)
	}

	if len(source.fetchedExchanges) != 2 || !allBytesZero(source.fetchedExchanges[0].RequestRaw) || !allBytesZero(source.fetchedExchanges[1].RequestRaw) {
		t.Fatal("live-tail run retained raw request bytes after processing")
	}
	var count int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_index").Scan(&count); err != nil {
		t.Fatalf("count live captures: %v", err)
	}
	if count != 2 {
		t.Fatalf("live capture count after restart and replay = %d, want 2", count)
	}
	var checkpointState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&checkpointState); err != nil {
		t.Fatalf("read live-tail checkpoint: %v", err)
	}
	if checkpointState != "idle" {
		t.Fatalf("checkpoint state after cursor resume = %q, want idle", checkpointState)
	}
	for _, marker := range []string{"first-page-marker", "second-page-marker"} {
		assertAbsentFromProjectFiles(t, projectStore.Directory, marker)
	}
}

func TestLiveTailLinksRedirectAcrossImportedPages(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	checkpoint, err := writer.BeginCycle(ctx, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatalf("BeginCycle(): %v", err)
	}
	firstID, secondID := "fixture-redirect-source", "fixture-redirect-target"
	status := 302
	first := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: selected, RequestID: firstID},
		RequestRaw:  []byte("GET /start HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 302 Found\r\nLocation: /next\r\n\r\n"), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/start", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	cursor := "redirect-page-2"
	firstPage, err := writer.WritePage(ctx, checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{first}, NextCursor: &cursor, HasMore: true})
	if err != nil || firstPage.RequestsAdded != 1 {
		t.Fatalf("write redirect source page: result=%+v err=%v", firstPage, err)
	}
	second := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: selected, RequestID: secondID},
		RequestRaw: []byte("GET /next HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		Method:     "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/next", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}
	secondPage, err := writer.WritePage(ctx, firstPage.Checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{second}})
	if err != nil || secondPage.RequestsAdded != 1 {
		t.Fatalf("write redirect target page: result=%+v err=%v", secondPage, err)
	}
	var sourceKey, targetKey, revisionID, sourceGraphID, targetGraphID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT redirect_url_key,graph_observation_id FROM live_capture_index WHERE request_id=?`, firstID,
	).Scan(&sourceKey, &sourceGraphID); err != nil || !validDigest(sourceKey) {
		t.Fatalf("redirect source HMAC key = %q, err=%v", sourceKey, err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT request_url_key,current_revision_id,graph_observation_id FROM live_capture_index WHERE request_id=?`, secondID,
	).Scan(&targetKey, &revisionID, &targetGraphID); err != nil || sourceKey != targetKey {
		t.Fatalf("redirect HMAC keys do not match: source=%q target=%q err=%v", sourceKey, targetKey, err)
	}
	var payload []byte
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT payload_json FROM projection_jobs WHERE revision_id=?", revisionID,
	).Scan(&payload); err != nil {
		t.Fatalf("read cross-page redirect projection: %v", err)
	}
	bundle, err := behaviorgraph.DecodeBundleJSON(payload)
	if err != nil {
		t.Fatalf("decode cross-page redirect projection: %v", err)
	}
	assertions, ok := projectionRecords(bundle["assertions"])
	if !ok {
		t.Fatal("redirect projection assertions are malformed")
	}
	for _, assertion := range assertions {
		if assertion["predicate"] != "redirects_to" || assertion["subject"] != sourceGraphID || assertion["object"] != targetGraphID {
			continue
		}
		if len(projectionRecordsMust(t, assertion["source_refs"])) != 2 {
			t.Fatal("cross-page redirect must cite both capture receipts")
		}
		if bytes.Contains(payload, []byte("https://api.fixture.invalid/next")) {
			t.Fatal("cross-page redirect projection persisted the matched URL value")
		}
		return
	}
	t.Fatal("redirect source in the earlier page was not linked to the exact target request")
}

func TestLiveTailSkipsAmbiguousCrossPageRefererMatches(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	checkpoint, err := writer.BeginCycle(ctx, time.Date(2026, 10, 25, 13, 0, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatalf("BeginCycle(): %v", err)
	}
	makeExchange := func(id string, referer string) caido.HttpExchange {
		raw := "GET /same HTTP/1.1\r\nHost: api.fixture.invalid\r\n"
		if referer != "" {
			raw += "Referer: " + referer + "\r\n"
		}
		raw += "\r\n"
		return caido.HttpExchange{
			Ref: caido.ExchangeRef{Project: selected, RequestID: id}, RequestRaw: []byte(raw),
			Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
			Target: "https://api.fixture.invalid/same", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
		}
	}
	firstID, duplicateID, currentID := "fixture-ambiguous-a", "fixture-ambiguous-b", "fixture-ambiguous-current"
	cursor := "ambiguous-page-2"
	firstPage, err := writer.WritePage(ctx, checkpoint, caido.HistoryPage{
		Exchanges:  []caido.HttpExchange{makeExchange(firstID, ""), makeExchange(duplicateID, "")},
		NextCursor: &cursor, HasMore: true,
	})
	if err != nil || firstPage.RequestsAdded != 2 {
		t.Fatalf("write duplicate URL page: result=%+v err=%v", firstPage, err)
	}
	current, err := writer.WritePage(ctx, firstPage.Checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{
		makeExchange(currentID, "https://api.fixture.invalid/same"),
	}})
	if err != nil || current.RequestsAdded != 1 {
		t.Fatalf("write Referer page: result=%+v err=%v", current, err)
	}
	var revisionID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM live_capture_index WHERE request_id=?", currentID,
	).Scan(&revisionID); err != nil {
		t.Fatalf("read current revision: %v", err)
	}
	var payload []byte
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT payload_json FROM projection_jobs WHERE revision_id=?", revisionID,
	).Scan(&payload); err != nil {
		t.Fatalf("read current projection: %v", err)
	}
	bundle, err := behaviorgraph.DecodeBundleJSON(payload)
	if err != nil {
		t.Fatalf("decode current projection: %v", err)
	}
	assertions, ok := projectionRecords(bundle["assertions"])
	if !ok {
		t.Fatal("current assertions are malformed")
	}
	for _, assertion := range assertions {
		if assertion["predicate"] == "referrer_context_for" {
			t.Fatal("ambiguous URL match was assigned to one prior observation")
		}
	}
}

func projectionRecordsMust(t *testing.T, value any) []behaviorgraph.Record {
	t.Helper()
	records, ok := projectionRecords(value)
	if !ok {
		t.Fatal("projection record list is malformed")
	}
	return records
}

type liveTailSourceFixture struct {
	project           project.CaidoProjectRef
	pages             []caido.RequestMetadataPage
	refreshPages      []caido.RequestMetadataPage
	exchanges         map[string]caido.HttpExchange
	exchangeSnapshots map[string][]caido.HttpExchange
	snapshotCalls     map[string]int
	fetchedExchanges  []caido.HttpExchange
	pageErr           error
	pageCalls         int
	refreshCalls      int
	fetchCalls        int
	cursors           []string
	refreshCursors    []string
	httpqlFilters     []string
	skippedCounts     []int
}

func (s *liveTailSourceFixture) ListProjects() ([]caido.ProjectRef, error) {
	return []caido.ProjectRef{s.project}, nil
}

func (s *liveTailSourceFixture) ListRequestPage(selected project.CaidoProjectRef, cursor *string, _ int, httpql *string) (caido.RequestMetadataPage, error) {
	if !selected.SameProject(s.project) {
		return caido.RequestMetadataPage{}, errors.New("fixture received an invalid project or live-tail time filter")
	}
	if s.pageErr != nil {
		return caido.RequestMetadataPage{}, s.pageErr
	}
	cursorValue := ""
	if cursor != nil {
		cursorValue = *cursor
	}
	if httpql == nil {
		s.refreshCursors = append(s.refreshCursors, cursorValue)
		if s.refreshCalls >= len(s.refreshPages) {
			s.refreshCalls++
			return caido.RequestMetadataPage{}, nil
		}
		page := s.refreshPages[s.refreshCalls]
		s.refreshCalls++
		return page, nil
	}
	s.cursors = append(s.cursors, cursorValue)
	s.httpqlFilters = append(s.httpqlFilters, *httpql)
	if !strings.Contains(*httpql, "req.created_at.gt:") {
		return caido.RequestMetadataPage{}, errors.New("fixture received an invalid live-tail time filter")
	}
	if s.pageCalls >= len(s.pages) {
		return caido.RequestMetadataPage{}, errors.New("fixture has no metadata page for the requested cursor")
	}
	page := s.pages[s.pageCalls]
	s.pageCalls++
	return page, nil
}

func (s *liveTailSourceFixture) FetchHistoryPage(selected project.CaidoProjectRef, page caido.RequestMetadataPage, skipIDs map[string]struct{}, _ int) (caido.HistoryPage, error) {
	if !selected.SameProject(s.project) {
		return caido.HistoryPage{}, errors.New("fixture received a different selected project")
	}
	s.fetchCalls++
	s.skippedCounts = append(s.skippedCounts, len(skipIDs))
	exchanges := make([]caido.HttpExchange, 0, len(page.Requests))
	for _, request := range page.Requests {
		if _, skip := skipIDs[request.RequestID]; skip {
			continue
		}
		if snapshots := s.exchangeSnapshots[request.RequestID]; len(snapshots) != 0 {
			if s.snapshotCalls == nil {
				s.snapshotCalls = make(map[string]int)
			}
			index := s.snapshotCalls[request.RequestID]
			if index >= len(snapshots) {
				index = len(snapshots) - 1
			}
			s.snapshotCalls[request.RequestID]++
			exchange := cloneLiveFixtureExchange(snapshots[index])
			s.fetchedExchanges = append(s.fetchedExchanges, exchange)
			exchanges = append(exchanges, exchange)
			continue
		}
		exchange, ok := s.exchanges[request.RequestID]
		if !ok {
			return caido.HistoryPage{}, errors.New("fixture metadata identity has no exchange")
		}
		exchange = cloneLiveFixtureExchange(exchange)
		s.fetchedExchanges = append(s.fetchedExchanges, exchange)
		exchanges = append(exchanges, exchange)
	}
	return caido.HistoryPage{Exchanges: exchanges, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
}

func cloneLiveFixtureExchange(exchange caido.HttpExchange) caido.HttpExchange {
	exchange.RequestRaw = append([]byte(nil), exchange.RequestRaw...)
	if exchange.ResponseRaw != nil {
		exchange.ResponseRaw = append([]byte{}, exchange.ResponseRaw...)
	}
	if exchange.CapturedAt != nil {
		value := *exchange.CapturedAt
		exchange.CapturedAt = &value
	}
	if exchange.ResponseStatus != nil {
		value := *exchange.ResponseStatus
		exchange.ResponseStatus = &value
	}
	if exchange.RequestTruncated != nil {
		value := *exchange.RequestTruncated
		exchange.RequestTruncated = &value
	}
	if exchange.ResponseTruncated != nil {
		value := *exchange.ResponseTruncated
		exchange.ResponseTruncated = &value
	}
	return exchange
}

func (s *liveTailSourceFixture) GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error) {
	return nil, errors.New("fixture does not implement direct exchange lookup")
}

func newLiveTailFixture(t *testing.T) (project.CaidoProjectRef, *store.ProjectStore, *LiveTailWriter) {
	t.Helper()
	ctx := context.Background()
	selected := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "live-tail-project"}
	projectStore, err := store.CreateProject(ctx, selected, t.TempDir())
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	fixtureKey := bytes.Repeat([]byte("k"), 32)
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(fixtureKey))
	clear(fixtureKey)
	writer, err := OpenLiveTailWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenLiveTailWriter() error = %v", err)
	}
	t.Cleanup(writer.Close)
	return selected, projectStore, writer
}

func allBytesZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

func assertAbsentFromProjectFiles(t *testing.T, root, marker string, allowedSampleFiles ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		for _, allowed := range allowedSampleFiles {
			if filepath.Base(path) == allowed {
				return nil
			}
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, []byte(marker)) {
			t.Errorf("raw fixture value was persisted in %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect project fixture files: %v", err)
	}
}
