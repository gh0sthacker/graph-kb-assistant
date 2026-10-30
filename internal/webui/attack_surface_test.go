package webui

import (
	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/hunt"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

type attackAPIFixture struct {
	*explorerAPIFixture
	last appquery.AttackSurfaceRequest
}

func (f *attackAPIFixture) AttackSurface(_ context.Context, r appquery.AttackSurfaceRequest) (hunt.Result, error) {
	f.last = r
	return hunt.Build(nil).Query(r.Classes, r.Limit)
}
func TestAttackSurfaceHTTP(t *testing.T) {
	api := &attackAPIFixture{explorerAPIFixture: &explorerAPIFixture{}}
	h, e := NewHandler(api)
	if e != nil {
		t.Fatal(e)
	}
	key := strings.Repeat("a", 64)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/api/attack-surface?project_key="+key+"&classes=sqli,xss&limit=3", nil))
	if w.Code != 200 || api.last.ProjectKey != key || len(api.last.Classes) != 2 || api.last.Limit != 3 || !strings.Contains(w.Body.String(), `"candidates":[]`) {
		t.Fatalf("status=%d body=%s last=%+v", w.Code, w.Body, api.last)
	}
	for _, query := range []string{"project_key=bad", "project_key=" + key + "&limit=-1", "project_key=" + key + "&classes=bogus", "project_key=" + key + "&limit=1&limit=2"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/api/attack-surface?"+query, nil))
		if w.Code != 400 {
			t.Fatalf("%s: %d", query, w.Code)
		}
	}
}
