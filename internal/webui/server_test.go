package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	capturesearch "bb-graph-assistant/internal/search"
)

type explorerAPIFixture struct{ direct, full int }

func (*explorerAPIFixture) GraphExplorerProjects(context.Context, string, int) (map[string]any, error) {
	return map[string]any{}, nil
}
func (*explorerAPIFixture) GraphExplorerSurface(context.Context, string, *capturesearch.SurfaceCursor, int) (map[string]any, error) {
	return map[string]any{}, nil
}
func (f *explorerAPIFixture) GraphExplorerNeighborhood(context.Context, string, []string) (map[string]any, error) {
	f.direct++
	return map[string]any{"view": "direct"}, nil
}
func (f *explorerAPIFixture) GraphExplorerSlice(context.Context, string, []string) (map[string]any, error) {
	f.full++
	return map[string]any{"view": "full"}, nil
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
