package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/ingest"
)

type operationBackfillFixture struct {
	*explorerAPIFixture
	previewCalls   int
	applyCalls     int
	preview        ingest.OperationBackfillPreview
	previewOptions ingest.OperationBackfillOptions
	apply          ingest.OperationBackfillApplyResult
	err            error
	projectKey     string
	digest         string
}

func (f *operationBackfillFixture) PreviewOperationBackfill(_ context.Context, projectKey string, options ingest.OperationBackfillOptions) (ingest.OperationBackfillPreview, error) {
	f.previewCalls++
	f.projectKey = projectKey
	f.previewOptions = options
	if options.MaxCaptures < 1 || options.MaxCaptures > ingest.MaximumOperationBackfillPageSize || options.MaxBytes != 1<<20 {
		return ingest.OperationBackfillPreview{}, context.Canceled
	}
	return f.preview, f.err
}

func (f *operationBackfillFixture) ApplyOperationBackfill(_ context.Context, projectKey, digest string, options ingest.OperationBackfillOptions) (ingest.OperationBackfillApplyResult, error) {
	f.applyCalls++
	f.projectKey, f.digest = projectKey, digest
	if options.MaxCaptures != 25 || options.MaxBytes != 1<<20 {
		return ingest.OperationBackfillApplyResult{}, context.Canceled
	}
	return f.apply, f.err
}

func TestOperationBackfillPreviewIsBoundedNoStoreAndOmitsBundles(t *testing.T) {
	key := strings.Repeat("a", 64)
	digest := strings.Repeat("b", 64)
	api := &operationBackfillFixture{
		explorerAPIFixture: &explorerAPIFixture{},
		preview: ingest.OperationBackfillPreview{
			ProjectKey: key, State: "ready", WriteMode: "preview_only", Scanned: 25,
			CandidateSHA256:  digest,
			OperationSamples: []ingest.OperationBackfillSample{{ID: "operation-1", Method: "GET", RouteFamily: "/items/{id}"}},
			Bundles:          []behaviorgraph.Record{{"http_body": "must-not-escape"}},
		},
	}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/operation-backfill/preview", strings.NewReader(
		`{"project_key":"`+key+`","max_captures":25,"max_request_bytes":1048576}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if api.previewCalls != 1 || api.projectKey != key {
		t.Fatalf("preview calls=%d project=%q", api.previewCalls, api.projectKey)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["candidate_sha256"] != digest || strings.Contains(response.Body.String(), "must-not-escape") || strings.Contains(response.Body.String(), "bundles") {
		t.Fatalf("preview response exposed an unexpected value: %s", response.Body.String())
	}
}

func TestOperationBackfillPreviewCarriesCursorAndCumulativeTotals(t *testing.T) {
	key := strings.Repeat("a", 64)
	api := &operationBackfillFixture{
		explorerAPIFixture: &explorerAPIFixture{},
		preview: ingest.OperationBackfillPreview{
			ProjectKey: key, State: "partial", WriteMode: "preview_only", HistoricalCaptures: 1400,
			Scanned: 100, ScannedTotal: 100, PageNumber: 1, Truncated: true, NextCursor: "signed-page-cursor",
			Totals: ingest.OperationBackfillTotals{Scanned: 100, EligibleCaptures: 12},
		},
	}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/operation-backfill/preview", strings.NewReader(
		`{"project_key":"`+key+`","max_captures":100,"max_request_bytes":1048576,"cursor":"prior-page-cursor"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !api.previewOptions.Paginated || api.previewOptions.Cursor != "prior-page-cursor" || api.previewOptions.MaxCaptures != 100 {
		t.Fatalf("status=%d options=%+v body=%s", response.Code, api.previewOptions, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["historical_captures"] != float64(1400) || result["scanned_total"] != float64(100) || result["next_cursor"] != "signed-page-cursor" || result["write_mode"] != "preview_only" {
		t.Fatalf("paginated preview response omitted continuation totals: %s", response.Body.String())
	}
}

func TestOperationBackfillApplyRequiresExactDigestAndReturnsConflictOnStalePreview(t *testing.T) {
	key, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	api := &operationBackfillFixture{explorerAPIFixture: &explorerAPIFixture{}, err: ingest.ErrOperationBackfillCandidateChanged}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	post := func(candidate string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"project_key": key, "candidate_sha256": candidate, "max_captures": 25, "max_request_bytes": 1 << 20})
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/operation-backfill/apply", strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://127.0.0.1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := post("invalid"); response.Code != http.StatusBadRequest || api.applyCalls != 0 {
		t.Fatalf("invalid digest status=%d calls=%d", response.Code, api.applyCalls)
	}
	if response := post(digest); response.Code != http.StatusConflict || api.applyCalls != 1 || api.digest != digest {
		t.Fatalf("stale candidate status=%d calls=%d digest=%q body=%s", response.Code, api.applyCalls, api.digest, response.Body.String())
	}
}

func TestOperationBackfillRejectsOutOfBoundsLimitsBeforeProjectAccess(t *testing.T) {
	key := strings.Repeat("a", 64)
	api := &operationBackfillFixture{explorerAPIFixture: &explorerAPIFixture{}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/operation-backfill/preview", strings.NewReader(
		`{"project_key":"`+key+`","max_captures":20001}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || api.previewCalls != 0 {
		t.Fatalf("out-of-bounds preview status=%d calls=%d", response.Code, api.previewCalls)
	}
}

func TestOperationBackfillRejectsCrossOriginBeforePreview(t *testing.T) {
	api := &operationBackfillFixture{explorerAPIFixture: &explorerAPIFixture{}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/operation-backfill/preview", strings.NewReader(`{"project_key":"`+strings.Repeat("a", 64)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || api.previewCalls != 0 {
		t.Fatalf("cross-origin status=%d preview calls=%d", response.Code, api.previewCalls)
	}
}
