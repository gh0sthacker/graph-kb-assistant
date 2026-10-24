package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
)

type explorerAPIFixture struct {
	direct, full        int
	graphErr            error
	lastRequest         appquery.GraphRequest
	surfaceErr          error
	lastSurfaceRequest  appquery.SurfaceRequest
	evidenceErr         error
	evidenceResult      appquery.CaptureMessageRange
	lastEvidenceRequest appquery.CaptureMessageRangeRequest
}

func (*explorerAPIFixture) GraphExplorerProjects(context.Context, string, int) (map[string]any, error) {
	return map[string]any{}, nil
}
func (f *explorerAPIFixture) GraphExplorerSurface(_ context.Context, request appquery.SurfaceRequest) (appquery.SurfaceResult, error) {
	f.lastSurfaceRequest = request
	return appquery.SurfaceResult{ProjectKey: request.ProjectKey, Filter: request.Filter, Coverage: "partial", CoverageGaps: []string{}}, f.surfaceErr
}
func (f *explorerAPIFixture) Graph(_ context.Context, request appquery.GraphRequest) (appquery.GraphResult, error) {
	f.lastRequest = request
	if request.View == appquery.GraphViewFull {
		f.full++
	} else {
		f.direct++
	}
	return appquery.GraphResult{View: request.View, Payload: map[string]any{"view": string(request.View)}}, f.graphErr
}
func (f *explorerAPIFixture) CaptureMessageRange(_ context.Context, request appquery.CaptureMessageRangeRequest) (appquery.CaptureMessageRange, error) {
	f.lastEvidenceRequest = request
	return f.evidenceResult, f.evidenceErr
}

func TestEvidenceHandlerReadsOnePinnedBoundedLocalMessageRange(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revisionID := strings.Repeat("b", 64)
	api := &explorerAPIFixture{evidenceResult: appquery.CaptureMessageRange{
		ProjectKey: projectKey, RequestID: "239", RevisionID: revisionID, Side: "request",
		Offset: 16, TotalSize: 21, Data: []byte("GET /"), SHA256: strings.Repeat("c", 64),
		ParseState: "complete", WireFidelity: "reconstructed_text",
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/evidence?project_key="+projectKey+"&request_id=239&revision_id="+revisionID+"&side=request&offset=16&max_bytes=4096", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive evidence response is cacheable")
	}
	if got := api.lastEvidenceRequest; got.ProjectKey != projectKey || got.RequestID != "239" || got.RevisionID != revisionID || got.Side != "request" || got.Offset != 16 || got.Maximum != 4096 {
		t.Fatalf("evidence request = %#v", got)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["data"] != "GET /" || result["encoding"] != "utf-8" || result["revision_id"] != revisionID || result["next_offset"] != nil {
		t.Fatalf("evidence response = %#v", result)
	}
}

func TestEvidenceHandlerRejectsDuplicateAndUnknownQueryParameters(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	for _, target := range []string{
		"/api/evidence?project_key=" + projectKey + "&project_key=" + projectKey + "&request_id=239&side=request",
		"/api/evidence?project_key=" + projectKey + "&request_id=239&side=request&unexpected=value",
		"/api/evidence?project_key=" + projectKey + "&request_id=239&side=request&max_bytes=1048577",
	} {
		api := &explorerAPIFixture{}
		handler, _ := NewHandler(api)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("target %q status=%d, want 400", target, response.Code)
		}
		if api.lastEvidenceRequest.RequestID != "" {
			t.Errorf("invalid evidence query reached service: %#v", api.lastEvidenceRequest)
		}
	}
}

func TestEvidenceHandlerExplainsUnavailableLocalVaultKey(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	api := &explorerAPIFixture{evidenceErr: &appquery.QueryError{
		Code: appquery.ErrorEvidenceKeyUnavailable, Message: "local vault key is unavailable or invalid",
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/evidence?project_key="+projectKey+"&request_id=239&side=request", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "local vault key is unavailable or invalid") {
		t.Fatalf("missing-key response status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEvidenceHandlerFallsBackToBase64ForControlHeavyTextWithinResponseLimit(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revisionID := strings.Repeat("b", 64)
	data := bytes.Repeat([]byte{0}, appquery.MaximumEvidenceRangeBytes)
	api := &explorerAPIFixture{evidenceResult: appquery.CaptureMessageRange{
		ProjectKey: projectKey, RequestID: "239", RevisionID: revisionID, Side: "request",
		TotalSize: int64(len(data)), Data: data,
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/evidence?project_key="+projectKey+"&request_id=239&side=request", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	if response.Body.Len() > 2<<20 {
		t.Fatalf("evidence response size=%d, want at most 2 MiB", response.Body.Len())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["encoding"] != "base64" {
		t.Fatalf("encoding=%v, want base64 fallback", result["encoding"])
	}
	encoded, ok := result["data"].(string)
	if !ok {
		t.Fatalf("data type=%T, want string", result["data"])
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatalf("base64 fallback did not preserve exact evidence bytes: decode error=%v, got %d bytes", err, len(decoded))
	}
}

func TestGraphHandlerLoadsDirectNeighborhoodUnlessFullSliceRequested(t *testing.T) {
	for _, view := range []string{"", "direct", "full", "invalid"} {
		t.Run(view, func(t *testing.T) {
			api := &explorerAPIFixture{}
			handler, err := NewHandler(api)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?project_key="+strings.Repeat("a", 64)+"&seed=endpoint-"+strings.Repeat("b", 64)+"&view="+view, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if view == "invalid" {
				if response.Code != http.StatusBadRequest || api.direct != 0 || api.full != 0 {
					t.Fatalf("invalid view dispatched: status=%d direct=%d full=%d", response.Code, api.direct, api.full)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", response.Code, response.Body.String())
			}
			if view == "full" {
				if api.full != 1 || api.direct != 0 {
					t.Fatal("explicit full view did not use the full reader")
				}
			} else if api.direct != 1 || api.full != 0 {
				t.Fatal("default view loaded the full project reader")
			}
		})
	}
}

func TestGraphHandlerMapsTypedErrorsWithoutLeakingCauses(t *testing.T) {
	tests := []struct {
		name string
		code appquery.ErrorCode
		want int
	}{
		{name: "invalid input", code: appquery.ErrorInvalidInput, want: http.StatusBadRequest},
		{name: "unknown project", code: appquery.ErrorProjectNotRegistered, want: http.StatusNotFound},
		{name: "unavailable project", code: appquery.ErrorProjectUnavailable, want: http.StatusServiceUnavailable},
		{name: "large result", code: appquery.ErrorOutputTooLarge, want: http.StatusRequestEntityTooLarge},
		{name: "stale cursor", code: appquery.ErrorStaleCursor, want: http.StatusConflict},
		{name: "unsupported cursor", code: appquery.ErrorCursorUnsupported, want: http.StatusNotImplemented},
		{name: "graph unavailable", code: appquery.ErrorGraphUnavailable, want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &explorerAPIFixture{graphErr: &appquery.QueryError{
				Code: test.code, Err: errors.New("/private/home/secret/database.sqlite3"),
			}}
			handler, err := NewHandler(api)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?project_key="+strings.Repeat("a", 64)+"&seed=endpoint-"+strings.Repeat("b", 64), nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private/home/secret") {
				t.Fatalf("typed error leaked its internal cause: %s", response.Body.String())
			}
		})
	}
}

func TestGraphHandlerPassesContinuationCursorToTypedQuery(t *testing.T) {
	api := &explorerAPIFixture{}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	cursor := "opaque-cursor-value"
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?project_key="+strings.Repeat("a", 64)+"&seed=endpoint-"+strings.Repeat("b", 64)+"&cursor="+cursor, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || string(api.lastRequest.Cursor) != cursor {
		t.Fatalf("continuation request = %#v, status %d: %s", api.lastRequest, response.Code, response.Body.String())
	}
}

func TestSurfaceHandlerPassesServerSideFilterAndOpaqueCursor(t *testing.T) {
	api := &explorerAPIFixture{}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	filter := " api.fixture "
	cursor := "opaque-signed-cursor"
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/surface?project_key="+strings.Repeat("a", 64)+"&filter="+url.QueryEscape(filter)+"&cursor="+cursor+"&limit=80", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	if api.lastSurfaceRequest.ProjectKey != strings.Repeat("a", 64) || api.lastSurfaceRequest.Filter != filter || api.lastSurfaceRequest.Cursor != cursor || api.lastSurfaceRequest.Limit != 80 {
		t.Fatalf("surface request = %#v", api.lastSurfaceRequest)
	}
}

func TestSurfaceHandlerRejectsDuplicateOrOversizedInputsAndMapsStaleCursor(t *testing.T) {
	for _, target := range []string{
		"/api/surface?project_key=" + strings.Repeat("a", 64) + "&filter=x&filter=y",
		"/api/surface?project_key=" + strings.Repeat("a", 64) + "&limit=101",
		"/api/surface?project_key=" + strings.Repeat("a", 64) + "&filter=" + strings.Repeat("x", 257),
	} {
		api := &explorerAPIFixture{}
		handler, _ := NewHandler(api)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("target %q status=%d, want 400", target, response.Code)
		}
		if api.lastSurfaceRequest.ProjectKey != "" {
			t.Errorf("invalid request reached API: %#v", api.lastSurfaceRequest)
		}
	}
	api := &explorerAPIFixture{surfaceErr: &appquery.QueryError{Code: appquery.ErrorStaleCursor}}
	handler, _ := NewHandler(api)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/surface?project_key="+strings.Repeat("a", 64)+"&cursor=stale", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale surface status=%d, want 409: %s", response.Code, response.Body.String())
	}
}
