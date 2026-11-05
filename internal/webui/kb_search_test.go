package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"bb-graph-assistant/internal/kb"
)

type kbSearchAPIFixture struct {
	*explorerAPIFixture
	result      kb.SearchResult
	err         error
	calls       int
	lastQuery   string
	lastTopK    int
	lastSources []string
}

func (f *kbSearchAPIFixture) KBSearch(_ context.Context, query string, topK int, sources []string) (kb.SearchResult, error) {
	f.calls++
	f.lastQuery, f.lastTopK = query, topK
	f.lastSources = append([]string(nil), sources...)
	return f.result, f.err
}

func newKBSearchHandler(t *testing.T, fixture *kbSearchAPIFixture) http.Handler {
	t.Helper()
	handler, err := NewHandler(fixture)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func performKBSearchRequest(t *testing.T, handler http.Handler, method string, parameters url.Values) *httptest.ResponseRecorder {
	t.Helper()
	target := "http://127.0.0.1:8765/api/kb/search"
	if parameters != nil {
		target += "?" + parameters.Encode()
	}
	request := httptest.NewRequest(method, target, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeKBError(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return payload["error"]
}

func TestKBSearchHandlerReturnsFlattenedIndexedResult(t *testing.T) {
	coverageHash := strings.Repeat("d", 64)
	fixture := &kbSearchAPIFixture{
		explorerAPIFixture: &explorerAPIFixture{},
		result: kb.SearchResult{
			SnapshotID: "snapshot-2026-11", CoverageState: "PARTIAL", CoverageReportHash: coverageHash,
			RetrievalMode: "lexical_model_unavailable", LexicalEngine: "sqlite-fts5",
			SupplementalIncluded: false,
			Hits: []kb.Hit{{
				ChunkID: "chunk-1", SourceName: "cve", SourceID: "CVE-2026-0001",
				Title: "Synthetic advisory", Excerpt: "synthetic excerpt", StartChar: 12, EndChar: 29,
				SourceSHA256: strings.Repeat("e", 64), ChunkSHA256: strings.Repeat("f", 64),
				BM25: 1.5, RankScore: 0.75,
			}},
		},
	}
	handler := newKBSearchHandler(t, fixture)

	parameters := url.Values{}
	parameters.Set("query", "  synthetic cve  ")
	parameters.Set("top_k", "7")
	parameters.Add("source", "cve")
	parameters.Add("source", "vendor-advisory")
	parameters.Add("source", "cve")
	recorder := performKBSearchRequest(t, handler, http.MethodGet, parameters)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	if payload["query"] != "synthetic cve" {
		t.Errorf("query = %#v; want the trimmed request query", payload["query"])
	}
	tookMS, ok := payload["took_ms"].(float64)
	if !ok || tookMS < 0 {
		t.Errorf("took_ms = %#v; want a non-negative number", payload["took_ms"])
	}
	for key, want := range map[string]any{
		"snapshot_id":                     "snapshot-2026-11",
		"coverage_state":                  "PARTIAL",
		"coverage_report_sha256":          coverageHash,
		"retrieval_mode":                  "lexical_model_unavailable",
		"lexical_engine":                  "sqlite-fts5",
		"supplemental_included":           false,
		"reference_material_is_untrusted": true,
	} {
		got, exists := payload[key]
		if !exists || got != want {
			t.Errorf("%s = %#v (present %t); want %#v", key, got, exists, want)
		}
	}
	hits, ok := payload["hits"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("hits = %#v; want one flattened hit", payload["hits"])
	}
	hit, _ := hits[0].(map[string]any)
	if hit["chunk_id"] != "chunk-1" || hit["source_name"] != "cve" || hit["source_id"] != "CVE-2026-0001" ||
		hit["title"] != "Synthetic advisory" || hit["excerpt"] != "synthetic excerpt" ||
		hit["start_char"] != float64(12) || hit["end_char"] != float64(29) {
		t.Errorf("hit = %#v; want the provider hit passed through unchanged", hit)
	}
	if fixture.calls != 1 {
		t.Errorf("provider calls = %d; want 1", fixture.calls)
	}
	if fixture.lastQuery != "synthetic cve" || fixture.lastTopK != 7 {
		t.Errorf("provider query/top_k = %q/%d; want trimmed query and 7", fixture.lastQuery, fixture.lastTopK)
	}
	if want := []string{"cve", "vendor-advisory"}; !slices.Equal(fixture.lastSources, want) {
		t.Errorf("provider sources = %#v; want %#v", fixture.lastSources, want)
	}
}

func TestKBSearchHandlerDefaultsTopKAndReturnsEmptyHitsArray(t *testing.T) {
	fixture := &kbSearchAPIFixture{explorerAPIFixture: &explorerAPIFixture{}}
	handler := newKBSearchHandler(t, fixture)

	recorder := performKBSearchRequest(t, handler, http.MethodGet, url.Values{"query": {"synthetic"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	if fixture.lastTopK != defaultKBSearchTopK {
		t.Errorf("top_k = %d; want default %d", fixture.lastTopK, defaultKBSearchTopK)
	}
	if len(fixture.lastSources) != 0 {
		t.Errorf("sources = %#v; want no source filter", fixture.lastSources)
	}
	if !strings.Contains(recorder.Body.String(), `"hits":[]`) {
		t.Errorf("body = %s; want an empty hits array instead of null", recorder.Body.String())
	}
}

func TestKBSearchHandlerAcceptsQueryAtRuneLimit(t *testing.T) {
	fixture := &kbSearchAPIFixture{explorerAPIFixture: &explorerAPIFixture{}}
	handler := newKBSearchHandler(t, fixture)

	query := strings.Repeat("ж", maxKBQueryRunes)
	recorder := performKBSearchRequest(t, handler, http.MethodGet, url.Values{"query": {query}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a %d-rune query; body = %s", recorder.Code, maxKBQueryRunes, recorder.Body.String())
	}
	if fixture.lastQuery != query {
		t.Errorf("provider query length = %d runes; want %d", len([]rune(fixture.lastQuery)), maxKBQueryRunes)
	}
}

func TestKBSearchHandlerRejectsInvalidParameters(t *testing.T) {
	oversizedSources := url.Values{"query": {"synthetic"}}
	for index := 0; index <= maxKBSearchSources; index++ {
		oversizedSources.Add("source", "family-"+strconv.Itoa(index))
	}
	cases := []struct {
		name       string
		parameters url.Values
	}{
		{"missing query", url.Values{}},
		{"empty query", url.Values{"query": {""}}},
		{"blank query", url.Values{"query": {"   "}}},
		{"oversized query", url.Values{"query": {strings.Repeat("a", maxKBQueryRunes+1)}}},
		{"zero top_k", url.Values{"query": {"synthetic"}, "top_k": {"0"}}},
		{"top_k above limit", url.Values{"query": {"synthetic"}, "top_k": {"21"}}},
		{"negative top_k", url.Values{"query": {"synthetic"}, "top_k": {"-1"}}},
		{"non-numeric top_k", url.Values{"query": {"synthetic"}, "top_k": {"ten"}}},
		{"repeated top_k", url.Values{"query": {"synthetic"}, "top_k": {"3", "4"}}},
		{"empty source", url.Values{"query": {"synthetic"}, "source": {""}}},
		{"untrimmed source", url.Values{"query": {"synthetic"}, "source": {" cve"}}},
		{"oversized source", url.Values{"query": {"synthetic"}, "source": {strings.Repeat("a", maxKBSourceNameBytes+1)}}},
		{"too many sources", oversizedSources},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := &kbSearchAPIFixture{explorerAPIFixture: &explorerAPIFixture{}}
			handler := newKBSearchHandler(t, fixture)
			recorder := performKBSearchRequest(t, handler, http.MethodGet, testCase.parameters)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if message := decodeKBError(t, recorder); message == "" {
				t.Error("error body is empty")
			}
			if fixture.calls != 0 {
				t.Errorf("provider calls = %d; want 0 for rejected parameters", fixture.calls)
			}
		})
	}
}

func TestKBSearchHandlerRejectsUnsupportedMethods(t *testing.T) {
	fixture := &kbSearchAPIFixture{explorerAPIFixture: &explorerAPIFixture{}}
	handler := newKBSearchHandler(t, fixture)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		recorder := performKBSearchRequest(t, handler, method, url.Values{"query": {"synthetic"}})
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405; body = %s", method, recorder.Code, recorder.Body.String())
		}
		if message := decodeKBError(t, recorder); message == "" {
			t.Errorf("%s error body is empty", method)
		}
	}
	if fixture.calls != 0 {
		t.Errorf("provider calls = %d; want 0 for unsupported methods", fixture.calls)
	}
}

func TestKBSearchHandlerMapsUnavailableIndexToServiceUnavailable(t *testing.T) {
	fixture := &kbSearchAPIFixture{explorerAPIFixture: &explorerAPIFixture{}, err: kb.ErrIndexUnavailable}
	handler := newKBSearchHandler(t, fixture)

	recorder := performKBSearchRequest(t, handler, http.MethodGet, url.Values{"query": {"synthetic"}})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	if message := decodeKBError(t, recorder); message != "local KB index is unavailable" {
		t.Errorf("error = %q; want the stable unavailability message", message)
	}
}

func TestKBSearchHandlerHidesInternalSearchFailures(t *testing.T) {
	fixture := &kbSearchAPIFixture{
		explorerAPIFixture: &explorerAPIFixture{},
		err:                errors.New("sqlite: /home/max/private/index.sqlite3 is locked"),
	}
	handler := newKBSearchHandler(t, fixture)

	recorder := performKBSearchRequest(t, handler, http.MethodGet, url.Values{"query": {"synthetic"}})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", recorder.Code, recorder.Body.String())
	}
	if message := decodeKBError(t, recorder); message != "KB search failed" {
		t.Errorf("error = %q; want a generic search failure", message)
	}
	if body := recorder.Body.String(); strings.Contains(body, "sqlite") || strings.Contains(body, "private") {
		t.Errorf("response leaked internal error details: %s", body)
	}
}

func TestKBSearchRouteIsAbsentWithoutProvider(t *testing.T) {
	handler, err := NewHandler(&explorerAPIFixture{})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	recorder := performKBSearchRequest(t, handler, http.MethodGet, url.Values{"query": {"synthetic"}})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the API does not expose KBSearch; body = %s", recorder.Code, recorder.Body.String())
	}
}
