package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
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
	pageWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic Neo4j outage")
	}

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
	pageWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic projection pending")
	}

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

func TestWritePageCommitsCaptureCheckpointAndProjectionJobBeforeGraphFailure(t *testing.T) {
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

	projectorCalled := false
	pageWriter.projector = func(_ context.Context, _ *store.ProjectStore, _ []behaviorgraph.Record) (bool, bool, error) {
		projectorCalled = true
		var captures, jobs int
		var state string
		if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM captures").Scan(&captures); err != nil {
			t.Fatal(err)
		}
		if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs").Scan(&jobs); err != nil {
			t.Fatal(err)
		}
		if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT state FROM sync_state WHERE source_key=?", sourceKey).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if captures != 1 || jobs != 1 || state != "done" {
			t.Fatalf("graph projection began before acceptance committed: captures=%d jobs=%d checkpoint=%q", captures, jobs, state)
		}
		return true, false, errors.New("synthetic Neo4j outage after local materialization")
	}

	result, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
	if err != nil {
		t.Fatalf("durable capture acceptance should survive projection failure: %v", err)
	}
	if !projectorCalled || result.ProjectionState != "pending" || result.GraphMapping != "mapped" || result.State != "done" {
		t.Fatalf("page result does not show accepted source with pending projection: called=%v result=%+v", projectorCalled, result)
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
	if captureRows != 1 || pendingJobs != 1 || graphMappings != 1 {
		t.Fatalf("accepted data, local mapping, or retryable projection job missing: captures=%d mappings=%d pending_jobs=%d", captureRows, graphMappings, pendingJobs)
	}

	pageWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return true, true, nil
	}
	replayed, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	})
	if err != nil {
		t.Fatalf("replaying a committed page should retry its pending projection: %v", err)
	}
	if replayed.ProjectionState != "applied" || replayed.GraphMapping != "mapped" || replayed.Replayed != 1 {
		t.Fatalf("projection retry result = %+v", replayed)
	}
	var appliedJobs int
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM projection_jobs WHERE state='applied'",
	).Scan(&appliedJobs); err != nil {
		t.Fatal(err)
	}
	if appliedJobs != 1 {
		t.Fatalf("applied projection job count = %d, want 1", appliedJobs)
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
	liveWriter.projector = func(context.Context, *store.ProjectStore, []behaviorgraph.Record) (bool, bool, error) {
		return false, false, errors.New("synthetic projection outage")
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
	captureID, err := exchange.Ref.ObservationID()
	if err != nil {
		t.Fatal(err)
	}
	pageWriter.projector = func(_ context.Context, _ *store.ProjectStore, bundles []behaviorgraph.Record) (bool, bool, error) {
		if len(bundles) != 1 || !hasProjectionObservation(bundles[0], liveGraphID, captureID, "live_capture") {
			t.Fatalf("historical import did not reuse the canonical live projection: %#v", bundles)
		}
		return true, false, errors.New("synthetic Neo4j outage after local materialization")
	}
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
	var sourceKind, graphMappedID, liveJobPayload string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		`SELECT r.source_kind,m.graph_observation_id,j.payload_json FROM capture_revision_registry r
		 JOIN graph_capture_map m ON m.revision_id=r.revision_id
		 JOIN projection_jobs j ON j.revision_id=r.revision_id WHERE r.revision_id=?`, revisionID,
	).Scan(&sourceKind, &graphMappedID, &liveJobPayload); err != nil {
		t.Fatal(err)
	}
	if sourceKind != "live" {
		t.Fatalf("first-seen source provenance = %q, want live", sourceKind)
	}
	if graphMappedID != liveGraphID || liveJobPayload != liveJob.PayloadJSON {
		t.Fatalf("cross-source replay changed canonical projection: graph=%q want=%q payload_changed=%v", graphMappedID, liveGraphID, liveJobPayload != liveJob.PayloadJSON)
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
	if !selected.SameProject(s.project) || s.fetchCalls != 0 || len(page.Requests) != len(s.metadata.Requests) {
		return caido.HistoryPage{}, errors.New("fixture received an unexpected history detail request")
	}
	s.fetchCalls++
	if s.fetchErr != nil {
		return caido.HistoryPage{}, s.fetchErr
	}
	return s.history, nil
}

func (s *backfillSourceFixture) GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error) {
	return nil, errors.New("fixture does not implement direct exchange lookup")
}
