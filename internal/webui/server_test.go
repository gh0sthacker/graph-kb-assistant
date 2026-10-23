package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
)

type explorerAPIFixture struct {
	direct, full       int
	graphErr           error
	lastRequest        appquery.GraphRequest
	surfaceErr         error
	lastSurfaceRequest appquery.SurfaceRequest
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
