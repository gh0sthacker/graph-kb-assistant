package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

func TestRunBackfillTerminalCursorDoesNotClaimCoverageReconciled(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	t.Cleanup(pageWriter.Close)
	requestID := "synthetic-history-request"
	target := "https://api.fixture.invalid/api/items"
	metadata := caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{
		RequestID: requestID, Method: "GET", URL: target,
	}}}
	history := caido.HistoryPage{Exchanges: []caido.HttpExchange{{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		RequestRaw:           []byte("GET /api/items HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               target,
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}}}
	source := &backfillSourceFixture{project: selected, metadata: metadata, history: history}

	result, err := RunBackfill(ctx, source, pageWriter, selected, BackfillOptions{
		PageSize: 1, MaxBodyBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("RunBackfill() error = %v", err)
	}
	if result.State != "done" || result.CursorState != "complete" || result.LimitReached || result.PagesThisRun != 1 || result.ProjectionPendingPages != 1 {
		t.Fatalf("RunBackfill() terminal enumeration state = %+v", result)
	}
	if result.CoverageReconciled {
		t.Fatal("RunBackfill() marked coverage reconciled without a stable source snapshot")
	}
	if !strings.Contains(result.CoverageNote, "no stable snapshot token") || !strings.Contains(result.CoverageNote, "expected history total") {
		t.Fatalf("RunBackfill() coverage note does not explain the unresolved gap: %q", result.CoverageNote)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal backfill result: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("decode backfill JSON: %v", err)
	}
	if output["state"] != "done" || output["coverage_reconciled"] != false || output["coverage_note"] != result.CoverageNote || output["projection_pending_pages"] != float64(1) {
		t.Fatalf("serialized backfill result obscures partial coverage: %s", encoded)
	}
	if source.pageCalls != 1 || source.fetchCalls != 1 {
		t.Fatalf("Caido calls = list:%d fetch:%d; want one terminal page", source.pageCalls, source.fetchCalls)
	}
}

func TestRunBackfillReportsProgressOnlyAfterDurablePageAcceptance(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	t.Cleanup(pageWriter.Close)
	const requestID = "sensitive-request-id"
	const rawRequest = "GET /private-path HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"
	target := "https://api.fixture.invalid/private-path"
	metadata := caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{
		RequestID: requestID, Method: "GET", URL: target,
	}}}
	history := caido.HistoryPage{Exchanges: []caido.HttpExchange{{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		RequestRaw:           []byte(rawRequest),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               target,
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}}}
	source := &backfillSourceFixture{project: selected, metadata: metadata, history: history}
	progress := make([]BackfillProgress, 0, 1)

	result, err := RunBackfill(ctx, source, pageWriter, selected, BackfillOptions{
		PageSize: 1, MaxBodyBytes: 64 << 10,
		OnProgress: func(item BackfillProgress) {
			var captures, jobs int
			var state string
			if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM captures").Scan(&captures); err != nil {
				t.Fatalf("read captures during progress callback: %v", err)
			}
			if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs").Scan(&jobs); err != nil {
				t.Fatalf("read projection jobs during progress callback: %v", err)
			}
			if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT state FROM sync_state WHERE source_key=?", sourceKey).Scan(&state); err != nil {
				t.Fatalf("read checkpoint during progress callback: %v", err)
			}
			if captures != 1 || jobs != 1 || state != "done" {
				t.Fatalf("progress was reported before durable acceptance: captures=%d jobs=%d checkpoint=%q", captures, jobs, state)
			}
			progress = append(progress, item)
		},
	})
	if err != nil {
		t.Fatalf("RunBackfill() error = %v", err)
	}
	if len(progress) != 1 {
		t.Fatalf("progress events = %d, want 1", len(progress))
	}
	item := progress[0]
	if item.PagesThisRun != 1 || item.PageCount != 1 || item.Stored != 1 || item.ProjectionPendingPages != 1 || item.State != "done" {
		t.Fatalf("progress summary = %+v", item)
	}
	if result.MetadataDurationMS < 0 || result.DetailDurationMS < 0 || result.PageProcessingDurationMS < 0 || result.GraphProjectionDurationMS < 0 {
		t.Fatalf("backfill timings must be non-negative: %+v", result)
	}
	if result.GraphProjectionDurationMS > result.PageProcessingDurationMS {
		t.Fatalf("graph projection timing cannot exceed page processing: %+v", result)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal progress summary: %v", err)
	}
	if strings.Contains(string(encoded), requestID) || strings.Contains(string(encoded), rawRequest) || strings.Contains(string(encoded), "/private-path") {
		t.Fatalf("progress summary contains request identity or raw HTTP data: %s", encoded)
	}
}

func TestDurationMillisecondsPreservesSubMillisecondTotals(t *testing.T) {
	var total time.Duration
	for range 3 {
		total += 600 * time.Microsecond
	}
	if got, want := durationMilliseconds(total), 1.8; got != want {
		t.Fatalf("durationMilliseconds(sum of sub-ms intervals) = %v, want %v", got, want)
	}
}

func TestWritePageReturnsPendingWithoutSynchronousProjection(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	t.Cleanup(pageWriter.Close)
	checkpoint, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatalf("BeginRun() error = %v", err)
	}
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "projection-failure-request"},
		RequestRaw:           []byte("GET /health HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/health",
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}

	result, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
	if err != nil {
		t.Fatalf("durable capture acceptance should survive projection failure: %v", err)
	}
	if result.ProjectionState != "pending" || result.GraphMapping != "pending" || result.State != "done" {
		t.Fatalf("page result does not show accepted source with deferred projection: %+v", result)
	}
	var captureRows, pendingJobs, graphMappings int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM captures").Scan(&captureRows); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs WHERE state='pending'").Scan(&pendingJobs); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM graph_capture_map").Scan(&graphMappings); err != nil {
		t.Fatal(err)
	}
	if captureRows != 1 || pendingJobs != 1 || graphMappings != 0 {
		t.Fatalf("accepted data or deferred projection state is wrong: captures=%d mappings=%d pending_jobs=%d", captureRows, graphMappings, pendingJobs)
	}

	replayed, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
	if err != nil {
		t.Fatalf("replaying a committed page should preserve its pending projection: %v", err)
	}
	if replayed.ProjectionState != "pending" || replayed.GraphMapping != "pending" || replayed.Replayed != 1 {
		t.Fatalf("projection replay result = %+v", replayed)
	}
	var appliedJobs int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM projection_jobs WHERE state='applied'",
	).Scan(&appliedJobs); err != nil {
		t.Fatal(err)
	}
	if appliedJobs != 0 {
		t.Fatalf("intake acknowledged %d projection jobs, want none", appliedJobs)
	}
}

func TestWritePageAcceptsRevisionFirstRegisteredByLiveTail(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, liveWriter := newLiveTailFixture(t)
	capturedAt := "2026-10-23T12:00:00Z"
	status := 200
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "shared-revision-request"},
		CapturedAt:           &capturedAt,
		RequestRaw:           []byte("GET /health HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw:          []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok"),
		HasResponse:          true,
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/health",
		ResponseStatus:       &status,
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "source_bytes",
	}
	liveCheckpoint, err := liveWriter.BeginCycle(ctx, time.Date(2026, 10, 23, 12, 1, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatal(err)
	}
	liveResult, err := liveWriter.WritePage(ctx, liveCheckpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}})
	if err != nil {
		t.Fatalf("seed accepted live capture: %v", err)
	}
	if liveResult.RequestsAdded != 1 || liveResult.ProjectionState != "pending" {
		t.Fatalf("live seed result = %+v", liveResult)
	}
	var revisionID, liveGraphID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT r.revision_id,l.graph_observation_id FROM capture_revision_registry r
		 JOIN live_capture_index l ON l.observation_id=r.observation_id
		 WHERE l.request_id=?`, exchange.Ref.RequestID,
	).Scan(&revisionID, &liveGraphID); err != nil {
		t.Fatal(err)
	}
	liveJob, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}

	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	t.Cleanup(pageWriter.Close)
	checkpoint, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
	if err != nil {
		t.Fatalf("history replay rejected a revision registered by live tail: %v", err)
	}
	if result.ProjectionState != "pending" {
		t.Fatalf("projection state = %q, want pending", result.ProjectionState)
	}
	var sourceKind, graphMappedID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT r.source_kind,l.graph_observation_id FROM capture_revision_registry r
		 JOIN live_capture_index l ON l.observation_id=r.observation_id
		 WHERE r.revision_id=?`, revisionID,
	).Scan(&sourceKind, &graphMappedID); err != nil {
		t.Fatal(err)
	}
	historicalJob, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceKind != "live" {
		t.Fatalf("first-seen source provenance = %q, want live", sourceKind)
	}
	if graphMappedID != liveGraphID || historicalJob.PayloadJSON != liveJob.PayloadJSON {
		t.Fatalf("cross-source replay changed canonical projection: graph=%q want=%q payload_changed=%v", graphMappedID, liveGraphID, historicalJob.PayloadJSON != liveJob.PayloadJSON)
	}
	var historicalMappings int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM graph_capture_map WHERE revision_id=?", revisionID,
	).Scan(&historicalMappings); err != nil || historicalMappings != 0 {
		t.Fatalf("historical mapping before projection = %d err=%v, want 0", historicalMappings, err)
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{}, materializeLocalProjection, time.Now().UTC())
	if closeErr := maintenance.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || workerResult.State != "applied" || workerResult.RevisionID != revisionID {
		t.Fatalf("cross-source projection worker = %+v err=%v", workerResult, err)
	}
	var historicalGraphID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT graph_observation_id FROM graph_capture_map WHERE revision_id=?", revisionID,
	).Scan(&historicalGraphID); err != nil || historicalGraphID != liveGraphID {
		t.Fatalf("historical graph mapping = %q err=%v, want %q", historicalGraphID, err, liveGraphID)
	}
	dataRoot := filepath.Dir(filepath.Dir(projectStore.Directory))
	pageWriter.Close()
	liveWriter.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatal(err)
	}
	readStore, err := store.OpenProjectReadOnly(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readStore.Close() })
	detail, err := search.ReadCaptureDetail(ctx, readStore, exchange.Ref.RequestID)
	if err != nil || detail.GraphObservationID == nil || *detail.GraphObservationID != liveGraphID {
		t.Fatalf("historical capture detail graph mapping = %+v err=%v, want %q", detail.GraphObservationID, err, liveGraphID)
	}
	catalog, err := search.ReadSurfaceCatalog(ctx, readStore, search.SurfaceCatalogRequest{Limit: 10})
	if err != nil || len(catalog.Operations) != 1 || catalog.Operations[0].GraphMappedObservationCount != 1 {
		t.Fatalf("cross-source surface mapping = %+v err=%v, want one mapped capture", catalog.Operations, err)
	}
}

func TestHistoricalReplayLinksProjectionAlreadyAppliedFromLiveTail(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, liveWriter := newLiveTailFixture(t)
	status := 200
	capturedAt := "2026-10-23T12:00:00Z"
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "live-applied-before-history"},
		CapturedAt:           &capturedAt,
		RequestRaw:           []byte("GET /health HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw:          []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok"),
		HasResponse:          true,
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               "https://api.fixture.invalid/health",
		ResponseStatus:       &status,
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "source_bytes",
	}
	liveCheckpoint, err := liveWriter.BeginCycle(ctx, time.Date(2026, 10, 23, 12, 1, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := liveWriter.WritePage(ctx, liveCheckpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}}); err != nil {
		t.Fatalf("seed live capture: %v", err)
	}
	var revisionID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT revision_id FROM capture_revision_registry WHERE observation_id=(
		 SELECT observation_id FROM live_capture_index WHERE request_id=?)`, exchange.Ref.RequestID,
	).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{}, materializeLocalProjection, time.Now().UTC())
	if closeErr := maintenance.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || workerResult.State != "applied" || workerResult.RevisionID != revisionID {
		t.Fatalf("live projection before history = %+v err=%v", workerResult, err)
	}

	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	defer pageWriter.Close()
	checkpoint, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
	if err != nil || replayed.ProjectionState != "applied" || replayed.GraphMapping != "mapped" {
		t.Fatalf("history replay after live projection = %+v err=%v", replayed, err)
	}
	var graphObservationID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT graph_observation_id FROM graph_capture_map WHERE revision_id=?", revisionID,
	).Scan(&graphObservationID); err != nil {
		t.Fatalf("historical map after applied live replay: %v", err)
	}
	dataRoot := filepath.Dir(filepath.Dir(projectStore.Directory))
	pageWriter.Close()
	liveWriter.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatal(err)
	}
	readStore, err := store.OpenProjectReadOnly(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readStore.Close() })
	detail, err := search.ReadCaptureDetail(ctx, readStore, exchange.Ref.RequestID)
	if err != nil || detail.GraphObservationID == nil || *detail.GraphObservationID != graphObservationID {
		t.Fatalf("historical detail after applied live replay = %+v err=%v", detail.GraphObservationID, err)
	}
}

func TestValidateExchangeAcceptsLongCaidoTargetWithinMessageLimit(t *testing.T) {
	selected := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := selected.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "long-target-fixture"},
		RequestRaw:           []byte("GET / HTTP/1.1\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "fixture.invalid",
		Port:                 443,
		Target:               "https://fixture.invalid/" + strings.Repeat("a", 9_000),
		RequestWireFidelity:  "reconstructed_text",
		ResponseWireFidelity: "unknown",
	}

	if _, err := validateExchange(exchange, projectKey); err != nil {
		t.Fatalf("validateExchange() rejected a target longer than eight KiB but within the message limit: %v", err)
	}
}

func TestValidateExchangeRejectsTargetBeyondMessageLimit(t *testing.T) {
	selected := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := selected.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "oversized-target-fixture"},
		RequestRaw:           []byte("GET / HTTP/1.1\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "fixture.invalid",
		Port:                 443,
		Target:               "https://fixture.invalid/" + strings.Repeat("a", caido.TargetURLBytesMaximum+1),
		RequestWireFidelity:  "reconstructed_text",
		ResponseWireFidelity: "unknown",
	}

	if _, err := validateExchange(exchange, projectKey); err == nil {
		t.Fatal("validateExchange() accepted a target beyond the message limit")
	}
}

type backfillSourceFixture struct {
	project    project.CaidoProjectRef
	metadata   caido.RequestMetadataPage
	history    caido.HistoryPage
	fetched    caido.RequestMetadataPage
	pageErr    error
	fetchErr   error
	pageCalls  int
	fetchCalls int
}

func (s *backfillSourceFixture) ListProjects() ([]caido.ProjectRef, error) {
	return []caido.ProjectRef{s.project}, nil
}

func (s *backfillSourceFixture) ListRequestPage(selected project.CaidoProjectRef, cursor *string, _ int, httpql *string) (caido.RequestMetadataPage, error) {
	if !selected.SameProject(s.project) || cursor != nil || httpql != nil || s.pageCalls != 0 {
		return caido.RequestMetadataPage{}, errors.New("fixture received an unexpected backfill page request")
	}
	s.pageCalls++
	if s.pageErr != nil {
		return caido.RequestMetadataPage{}, s.pageErr
	}
	return s.metadata, nil
}

func (s *backfillSourceFixture) FetchHistoryPage(selected project.CaidoProjectRef, page caido.RequestMetadataPage, _ map[string]struct{}, _ int) (caido.HistoryPage, error) {
	if !selected.SameProject(s.project) || s.fetchCalls != 0 || len(page.Requests) > len(s.metadata.Requests) {
		return caido.HistoryPage{}, errors.New("fixture received an unexpected history detail request")
	}
	s.fetchCalls++
	s.fetched = page
	if s.fetchErr != nil {
		return caido.HistoryPage{}, s.fetchErr
	}
	return s.history, nil
}

func TestRunBackfillAppliesExactOriginScopeBeforeFetchingDetails(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	insideID, outsideID := "in-scope-history", "out-of-scope-history"
	metadata := caido.RequestMetadataPage{Requests: []caido.RequestMetadata{
		{RequestID: insideID, Method: "GET", URL: "https://demo.fixture.invalid/api"},
		{RequestID: outsideID, Method: "GET", URL: "https://other.fixture.invalid/api"},
	}}
	history := caido.HistoryPage{Exchanges: []caido.HttpExchange{{
		Ref:        caido.ExchangeRef{Project: selected, RequestID: insideID},
		RequestRaw: []byte("GET /api HTTP/1.1\r\nHost: demo.fixture.invalid\r\n\r\n"),
		Method:     "GET", Scheme: "https", Host: "demo.fixture.invalid", Port: 443,
		Target: "https://demo.fixture.invalid/api", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}}}
	source := &backfillSourceFixture{project: selected, metadata: metadata, history: history}
	result, err := RunBackfill(ctx, source, pageWriter, selected, BackfillOptions{
		PageSize: 2, MaxBodyBytes: 64 << 10,
		Scope: &LiveTailScope{Scheme: "https", Host: "demo.fixture.invalid", Port: 443},
	})
	if err != nil {
		t.Fatalf("scoped backfill: %v", err)
	}
	if result.RequestsExcludedByScope != 1 || len(source.fetched.Requests) != 1 || source.fetched.Requests[0].RequestID != insideID {
		t.Fatalf("scope filtering did not happen before detail fetch: result=%+v fetched=%+v", result, source.fetched.Requests)
	}
	var captures int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM captures").Scan(&captures); err != nil {
		t.Fatal(err)
	}
	if captures != 1 {
		t.Fatalf("stored captures=%d, want only the in-scope request", captures)
	}
}

func (s *backfillSourceFixture) GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error) {
	return nil, errors.New("fixture does not implement direct exchange lookup")
}
