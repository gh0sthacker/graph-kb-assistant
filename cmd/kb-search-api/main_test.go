package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/hackerone"
	"bb-graph-assistant/internal/kb"
)

const (
	kbFixtureSnapshot       = "1111111111111111111111111111111111111111111111111111111111111111"
	kbFixtureCoverageReport = "2222222222222222222222222222222222222222222222222222222222222222"
	hackeroneFixtureSnap    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	hackeroneFixtureCommit  = "1234567890abcdef1234567890abcdef12345678"
)

// createKBFixture builds the same Python-compatible schema the MCP server tests
// use, with three chunks across two source families.
func createKBFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open KB fixture: %v", err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE source_documents (source_name TEXT NOT NULL, source_id TEXT NOT NULL, title TEXT NOT NULL,
			source_url TEXT, source_locator TEXT, source_revision TEXT, license_id TEXT, attribution TEXT,
			metadata_json TEXT NOT NULL, content_sha256 TEXT NOT NULL, text TEXT NOT NULL,
			PRIMARY KEY(source_name,source_id))`,
		`CREATE TABLE chunks (chunk_id TEXT PRIMARY KEY, source_name TEXT NOT NULL, source_id TEXT NOT NULL,
			title TEXT NOT NULL, start_char INTEGER NOT NULL, end_char INTEGER NOT NULL, source_locator TEXT,
			source_url TEXT, source_revision TEXT, license_id TEXT, attribution TEXT, metadata_json TEXT NOT NULL,
			source_sha256 TEXT NOT NULL, chunk_sha256 TEXT NOT NULL, chunk_schema TEXT NOT NULL, text TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE chunk_search USING fts5(chunk_id UNINDEXED,source_name UNINDEXED,title,text,
			tokenize='unicode61 remove_diacritics 2')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create KB fixture schema: %v", err)
		}
	}
	for key, value := range map[string]string{
		"snapshot_id": kbFixtureSnapshot, "coverage_state": "PARTIAL",
		"coverage_report_sha256": kbFixtureCoverageReport, "source_coverage": `[]`,
	} {
		if _, err := db.Exec(`INSERT INTO kb_meta(key,value) VALUES(?,?)`, key, value); err != nil {
			t.Fatalf("insert KB metadata %s: %v", key, err)
		}
	}
	rows := []struct {
		source, id, title, text string
	}{
		{"nvd", "cve-0001", "Synthetic kernel exploit one", "synthetic kernel exploit primitive"},
		{"nvd", "cve-0002", "Synthetic kernel exploit two", "synthetic kernel exploit primitive two"},
		{"tool_docs", "gtfobins-note", "Synthetic gtfobins note", "synthetic gtfobins note"},
	}
	for _, row := range rows {
		textHash := sha256.Sum256([]byte(row.text))
		digest := hex.EncodeToString(textHash[:])
		chunkHash := sha256.Sum256([]byte(row.source + "/" + row.id))
		chunkID := hex.EncodeToString(chunkHash[:])
		metadata := `{"document_role":"content"}`
		if _, err := db.Exec(`INSERT INTO source_documents(source_name,source_id,title,metadata_json,content_sha256,text)
			VALUES(?,?,?,?,?,?)`, row.source, row.id, row.title, metadata, digest, row.text); err != nil {
			t.Fatalf("insert KB fixture source %s: %v", row.id, err)
		}
		if _, err := db.Exec(`INSERT INTO chunks(chunk_id,source_name,source_id,title,start_char,end_char,metadata_json,
			source_sha256,chunk_sha256,chunk_schema,text) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			chunkID, row.source, row.id, row.title, 0, len(row.text), metadata,
			digest, digest, "bb-kb-chunk-v2", row.text); err != nil {
			t.Fatalf("insert KB fixture chunk %s: %v", row.id, err)
		}
		if _, err := db.Exec(`INSERT INTO chunk_search(chunk_id,source_name,title,text) VALUES(?,?,?,?)`,
			chunkID, row.source, row.title, row.text); err != nil {
			t.Fatalf("insert KB fixture FTS row %s: %v", row.id, err)
		}
	}
}

// createHackerOneFixture builds the same optional FTS5 index shape the
// HackerOne builder writes, with three synthetic reports.
func createHackerOneFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open HackerOne fixture: %v", err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE reports(
			report_id TEXT PRIMARY KEY, program TEXT NOT NULL, title TEXT NOT NULL, url TEXT NOT NULL,
			vuln_type TEXT NOT NULL, severity TEXT NOT NULL, weakness TEXT NOT NULL, bounty REAL,
			upvotes INTEGER, disclosed_at TEXT NOT NULL, body TEXT, has_body INTEGER NOT NULL,
			content_sha256 TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE reports_fts USING fts5(title,body,program,vuln_type,weakness,
			content='reports',content_rowid='rowid',
			tokenize="porter unicode61 remove_diacritics 2")`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create HackerOne fixture schema: %v", err)
		}
	}
	for key, value := range map[string]string{
		"schema_version":  hackerone.SchemaVersion,
		"dataset_commit":  hackeroneFixtureCommit,
		"built_at":        "2026-11-05T12:00:00Z",
		"report_count":    "3",
		"with_body_count": "3",
		"snapshot_id":     hackeroneFixtureSnap,
	} {
		if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, key, value); err != nil {
			t.Fatalf("insert HackerOne fixture metadata %s: %v", key, err)
		}
	}
	rows := []struct {
		id, program, title, vulnType, severity, weakness string
		body                                             string
		bounty                                           any
		upvotes                                          any
	}{
		{
			id: "2001", program: "acme", title: "Synthetic remote code execution",
			vulnType: "rce", severity: "critical", weakness: "CWE-94",
			body: "synthetic hackerone fixture remote code execution", bounty: 2500.0, upvotes: 12,
		},
		{
			id: "2002", program: "globex", title: "Synthetic SQL injection",
			vulnType: "sqli", severity: "high", weakness: "CWE-89",
			body: "synthetic hackerone fixture sql injection", bounty: nil, upvotes: 4,
		},
		{
			id: "2003", program: "globex", title: "Synthetic XSS",
			vulnType: "xss", severity: "medium", weakness: "CWE-79",
			body: "synthetic hackerone fixture cross-site scripting", bounty: 100.0, upvotes: nil,
		},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO reports(report_id,program,title,url,vuln_type,severity,weakness,
			bounty,upvotes,disclosed_at,body,has_body,content_sha256) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.id, row.program, row.title, "https://hackerone.com/reports/"+row.id, row.vulnType,
			row.severity, row.weakness, row.bounty, row.upvotes, "2022-05-01", row.body, 1,
			strings.Repeat("b", 64)); err != nil {
			t.Fatalf("insert HackerOne fixture report %s: %v", row.id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO reports_fts(reports_fts) VALUES('rebuild')`); err != nil {
		t.Fatalf("rebuild HackerOne fixture FTS index: %v", err)
	}
}

func newTestAPI(t *testing.T, withHackerOne bool) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	kbPath := filepath.Join(dir, "kb-index.sqlite3")
	createKBFixture(t, kbPath)
	kbIndex, err := kb.OpenIndex(kbPath)
	if err != nil {
		t.Fatalf("kb.OpenIndex() error = %v", err)
	}
	t.Cleanup(func() { _ = kbIndex.Close() })

	var supplemental hackeroneSearcher
	if withHackerOne {
		hackeronePath := filepath.Join(dir, "hackerone-index.sqlite3")
		createHackerOneFixture(t, hackeronePath)
		hackeroneIndex, err := hackerone.Open(hackeronePath)
		if err != nil {
			t.Fatalf("hackerone.Open() error = %v", err)
		}
		t.Cleanup(func() { _ = hackeroneIndex.Close() })
		supplemental = hackeroneIndex
	}

	logger := log.New(io.Discard, "", 0)
	service := newServer(kbIndex, supplemental, logger)
	service.kbSnapshot = readKBSnapshot(logger, kbIndex)
	return httptest.NewServer(service.routes())
}

type testHit struct {
	ChunkID        string         `json:"chunk_id"`
	SourceName     string         `json:"source_name"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Excerpt        string         `json:"excerpt"`
	SourceLocator  *string        `json:"source_locator"`
	SourceURL      *string        `json:"source_url"`
	SourceRevision *string        `json:"source_revision"`
	Attribution    *string        `json:"attribution"`
	Metadata       map[string]any `json:"metadata"`
	BM25           float64        `json:"bm25"`
	RankScore      float64        `json:"rank_score"`
}

type testSearchResponse struct {
	Query                        string    `json:"query"`
	TookMS                       int64     `json:"took_ms"`
	SnapshotID                   string    `json:"snapshot_id"`
	CoverageState                string    `json:"coverage_state"`
	CoverageReportSHA256         string    `json:"coverage_report_sha256"`
	MaintenanceState             string    `json:"maintenance_state"`
	RetrievalMode                string    `json:"retrieval_mode"`
	LexicalEngine                string    `json:"lexical_engine"`
	SupplementalIncluded         bool      `json:"supplemental_included"`
	ReferenceMaterialIsUntrusted bool      `json:"reference_material_is_untrusted"`
	Hits                         []testHit `json:"hits"`
}

func request(t *testing.T, client *http.Client, method, target string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("http.NewRequest(%s) error = %v", method, err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, target, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read %s body: %v", target, err)
	}
	return response, body
}

func search(t *testing.T, baseURL, query string) (*http.Response, testSearchResponse) {
	t.Helper()
	response, body := request(t, http.DefaultClient, http.MethodGet, baseURL+"/api/kb/search?"+query)
	var payload testSearchResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode search response %q: %v", body, err)
	}
	return response, payload
}

func TestHealthReportsBothIndexes(t *testing.T) {
	server := newTestAPI(t, true)
	defer server.Close()

	response, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/health")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d body = %s", response.StatusCode, body)
	}
	var payload struct {
		Status           string `json:"status"`
		KB               bool   `json:"kb"`
		HackerOne        bool   `json:"hackerone"`
		KBSnapshot       string `json:"kb_snapshot"`
		HackerOneReports int64  `json:"hackerone_reports"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload.Status != "ok" || !payload.KB || !payload.HackerOne {
		t.Fatalf("health = %+v", payload)
	}
	if payload.KBSnapshot != kbFixtureSnapshot {
		t.Fatalf("kb_snapshot = %q; want %q", payload.KBSnapshot, kbFixtureSnapshot)
	}
	if payload.HackerOneReports != 3 {
		t.Fatalf("hackerone_reports = %d; want 3", payload.HackerOneReports)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("health Cache-Control = %q", got)
	}
}

func TestHealthWithoutHackerOneIndex(t *testing.T) {
	server := newTestAPI(t, false)
	defer server.Close()

	response, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/health")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d body = %s", response.StatusCode, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload["hackerone"] != false || payload["kb"] != true {
		t.Fatalf("health = %#v", payload)
	}
	if payload["hackerone_reports"] != float64(0) {
		t.Fatalf("hackerone_reports = %#v", payload["hackerone_reports"])
	}
	if payload["kb_snapshot"] != kbFixtureSnapshot {
		t.Fatalf("kb_snapshot = %#v", payload["kb_snapshot"])
	}
}

func TestSearchKBOnlyFiltersSources(t *testing.T) {
	server := newTestAPI(t, true)
	defer server.Close()

	response, payload := search(t, server.URL, "query=synthetic&source=nvd")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header.Get("Cache-Control"))
	}
	if payload.Query != "synthetic" || payload.SnapshotID != kbFixtureSnapshot || payload.CoverageState != "PARTIAL" {
		t.Fatalf("search metadata = %+v", payload)
	}
	if payload.CoverageReportSHA256 != kbFixtureCoverageReport {
		t.Fatalf("coverage_report_sha256 = %q", payload.CoverageReportSHA256)
	}
	if payload.RetrievalMode == "" || payload.RetrievalMode == "hackerone_fts" || payload.LexicalEngine != "sqlite-fts5" {
		t.Fatalf("retrieval = %q/%q", payload.RetrievalMode, payload.LexicalEngine)
	}
	if payload.SupplementalIncluded || !payload.ReferenceMaterialIsUntrusted {
		t.Fatalf("flags = supplemental:%v untrusted:%v", payload.SupplementalIncluded, payload.ReferenceMaterialIsUntrusted)
	}
	if len(payload.Hits) != 2 {
		t.Fatalf("hits = %d; want the two nvd chunks", len(payload.Hits))
	}
	for _, hit := range payload.Hits {
		if hit.SourceName != "nvd" || strings.HasPrefix(hit.ChunkID, "hackerone:") {
			t.Fatalf("unexpected hit %+v", hit)
		}
	}

	_, toolDocs := search(t, server.URL, "query=synthetic&source=tool_docs")
	if len(toolDocs.Hits) != 1 || toolDocs.Hits[0].SourceName != "tool_docs" {
		t.Fatalf("tool_docs hits = %+v", toolDocs.Hits)
	}
}

func TestSearchResponseIsFlatJSON(t *testing.T) {
	server := newTestAPI(t, true)
	defer server.Close()

	_, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/kb/search?query=synthetic&top_k=3")
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	for _, key := range []string{"query", "took_ms", "snapshot_id", "coverage_state", "retrieval_mode",
		"lexical_engine", "supplemental_included", "reference_material_is_untrusted", "hits"} {
		if _, present := payload[key]; !present {
			t.Fatalf("response is missing flat key %q: %s", key, body)
		}
	}
	if _, present := payload["SearchResult"]; present {
		t.Fatalf("response nests SearchResult: %s", body)
	}
	if _, present := payload["maintenance_state"]; present {
		t.Fatalf("maintenance_state should be omitted when empty: %s", body)
	}
}

func TestSearchHackerOneOnlyReturnsSyntheticResult(t *testing.T) {
	server := newTestAPI(t, true)
	defer server.Close()

	response, payload := search(t, server.URL, "query=synthetic&top_k=5&source=hackerone")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", response.StatusCode)
	}
	if payload.SnapshotID != hackeroneFixtureSnap || payload.CoverageState != "DONE" ||
		payload.RetrievalMode != "hackerone_fts" || payload.LexicalEngine != "sqlite-fts5" ||
		!payload.SupplementalIncluded {
		t.Fatalf("synthetic result metadata = %+v", payload)
	}
	if len(payload.Hits) != 3 {
		t.Fatalf("hits = %d; want the three fixture reports", len(payload.Hits))
	}
	for rank, hit := range payload.Hits {
		if hit.SourceName != "hackerone" || hit.ChunkID != "hackerone:"+hit.SourceID {
			t.Fatalf("hit identity = %+v", hit)
		}
		if hit.SourceLocator == nil || *hit.SourceLocator != "reports/"+hit.SourceID+".md" {
			t.Fatalf("hit locator = %+v", hit.SourceLocator)
		}
		if hit.SourceRevision == nil || *hit.SourceRevision != hackeroneFixtureCommit {
			t.Fatalf("hit revision = %+v", hit.SourceRevision)
		}
		if hit.Attribution == nil || *hit.Attribution != "HackerOne disclosed report; local reference only" {
			t.Fatalf("hit attribution = %+v", hit.Attribution)
		}
		if hit.Metadata["program"] == nil || hit.Metadata["severity"] == nil || hit.Metadata["has_body"] != true {
			t.Fatalf("hit metadata = %#v", hit.Metadata)
		}
		wantRank := 1.0 / float64(60+rank+1)
		if hit.RankScore != wantRank {
			t.Fatalf("hit %d rank_score = %v; want %v", rank, hit.RankScore, wantRank)
		}
	}

	_, limited := search(t, server.URL, "query=synthetic&top_k=2&source=hackerone")
	if len(limited.Hits) != 2 {
		t.Fatalf("top_k=2 hits = %d", len(limited.Hits))
	}

	_, withKB := search(t, server.URL, "query=synthetic&source=hackerone&source=nvd")
	if !withKB.SupplementalIncluded || withKB.SnapshotID != kbFixtureSnapshot {
		t.Fatalf("hackerone+nvd metadata = %+v", withKB)
	}
	if len(withKB.Hits) != 5 {
		t.Fatalf("hackerone+nvd hits = %d; want 2 KB + 3 HackerOne", len(withKB.Hits))
	}
}

func TestSearchMergedOrdering(t *testing.T) {
	server := newTestAPI(t, true)
	defer server.Close()

	_, payload := search(t, server.URL, "query=synthetic&top_k=10")
	if payload.SnapshotID != kbFixtureSnapshot || payload.CoverageState != "PARTIAL" {
		t.Fatalf("merged metadata = %+v", payload)
	}
	if !payload.SupplementalIncluded {
		t.Fatal("merged search did not flag supplemental material")
	}
	if len(payload.Hits) != 6 {
		t.Fatalf("merged hits = %d; want 3 KB + 3 HackerOne", len(payload.Hits))
	}
	// RRF gives KB rank n and HackerOne rank n the same score. Stable sorting
	// plus KB-first insertion must interleave them deterministically.
	for index, hit := range payload.Hits {
		wantSupplemental := index%2 == 1
		if (hit.SourceName == "hackerone") != wantSupplemental {
			t.Fatalf("hit %d source = %q; want supplemental=%v", index, hit.SourceName, wantSupplemental)
		}
		wantScore := 1.0 / float64(60+index/2+1)
		if hit.RankScore != wantScore {
			t.Fatalf("hit %d rank_score = %v; want %v", index, hit.RankScore, wantScore)
		}
	}

	_, truncated := search(t, server.URL, "query=synthetic&top_k=2")
	if len(truncated.Hits) != 2 || truncated.Hits[1].SourceName != "hackerone" {
		t.Fatalf("top_k=2 merged hits = %+v", truncated.Hits)
	}
}

func TestSearchValidationErrors(t *testing.T) {
	server := newTestAPI(t, true)
	defer server.Close()

	values := url.Values{}
	values.Set("query", "synthetic")
	for index := 0; index < 21; index++ {
		values.Add("source", "family-"+strings.Repeat("x", index))
	}
	cases := []string{
		"query=",
		"query=%20%20",
		"query=synthetic&top_k=0",
		"query=synthetic&top_k=21",
		"query=synthetic&top_k=2&top_k=3",
		"query=synthetic&top_k=abc",
		"query=synthetic&source=",
		"query=synthetic&source=%20nvd",
		"query=" + strings.Repeat("a", 4097),
		values.Encode(),
	}
	for _, query := range cases {
		response, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/kb/search?"+query)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q status = %d body = %s", query, response.StatusCode, body)
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil || payload["error"] == "" {
			t.Errorf("query %q error body = %s", query, body)
		}
		if response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("query %q Content-Type = %q", query, response.Header.Get("Content-Type"))
		}
	}
}

type fakeKBSearch struct {
	result  kb.SearchResult
	err     error
	queries []string
	options []kb.SearchOptions
}

func (f *fakeKBSearch) Search(_ context.Context, query string, options kb.SearchOptions) (kb.SearchResult, error) {
	f.queries = append(f.queries, query)
	f.options = append(f.options, options)
	if f.err != nil {
		return kb.SearchResult{}, f.err
	}
	return f.result, nil
}

type fakeHackerOne struct {
	hits  []hackerone.Hit
	meta  hackerone.Meta
	err   error
	limit int
}

func (f *fakeHackerOne) Search(_ context.Context, _ string, limit int) ([]hackerone.Hit, error) {
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func (f *fakeHackerOne) Meta() hackerone.Meta { return f.meta }

func fakeAPI(searcher kbSearcher, supplemental hackeroneSearcher) *httptest.Server {
	service := newServer(searcher, supplemental, log.New(io.Discard, "", 0))
	return httptest.NewServer(service.routes())
}

func TestSearchUnavailableIndex(t *testing.T) {
	server := fakeAPI(&fakeKBSearch{err: kb.ErrIndexUnavailable}, nil)
	defer server.Close()

	response, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/kb/search?query=synthetic")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != `{"error":"local KB index is unavailable"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestSearchInternalErrorHidesDetails(t *testing.T) {
	server := fakeAPI(&fakeKBSearch{err: errors.New("secret sqlite failure")}, nil)
	defer server.Close()

	response, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/kb/search?query=synthetic")
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s", response.StatusCode, body)
	}
	if strings.Contains(string(body), "secret") || strings.Contains(string(body), "sqlite") {
		t.Fatalf("body leaked internals: %s", body)
	}
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil || payload["error"] != "KB search failed" {
		t.Fatalf("error body = %s", body)
	}
}

func TestSearchSupplementalFailureKeepsKBResults(t *testing.T) {
	kbResult := kb.SearchResult{
		SnapshotID: kbFixtureSnapshot, CoverageState: "PARTIAL", RetrievalMode: "fulltext",
		LexicalEngine: "sqlite-fts5", Hits: []kb.Hit{{ChunkID: strings.Repeat("a", 64), SourceName: "nvd", SourceID: "cve-1", Title: "t"}},
	}
	server := fakeAPI(&fakeKBSearch{result: kbResult}, &fakeHackerOne{err: errors.New("supplemental unavailable")})
	defer server.Close()

	response, payload := search(t, server.URL, "query=synthetic")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if len(payload.Hits) != 1 || payload.SupplementalIncluded {
		t.Fatalf("degraded result = %+v", payload)
	}
}

func TestSearchHackerOneOnlyWithoutIndexIsUnavailable(t *testing.T) {
	server := fakeAPI(&fakeKBSearch{err: kb.ErrIndexUnavailable}, nil)
	defer server.Close()

	response, body := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/kb/search?query=synthetic&source=hackerone")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.StatusCode, body)
	}
}

func TestSearchForwardsSourceFamiliesToKB(t *testing.T) {
	searcher := &fakeKBSearch{result: kb.SearchResult{SnapshotID: kbFixtureSnapshot, CoverageState: "PARTIAL",
		RetrievalMode: "fulltext", LexicalEngine: "sqlite-fts5", Hits: []kb.Hit{}}}
	supplemental := &fakeHackerOne{meta: hackerone.Meta{SnapshotID: hackeroneFixtureSnap, DatasetCommit: hackeroneFixtureCommit}}
	server := fakeAPI(searcher, supplemental)
	defer server.Close()

	response, _ := request(t, http.DefaultClient, http.MethodGet, server.URL+"/api/kb/search?query=synthetic&top_k=4&source=nvd&source=hackerone")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if len(searcher.options) != 1 {
		t.Fatalf("KB search calls = %d", len(searcher.options))
	}
	if got := searcher.options[0].SourceNames; len(got) != 1 || got[0] != "nvd" {
		t.Fatalf("KB source names = %v; want [nvd]", got)
	}
	if searcher.options[0].TopK != 4 {
		t.Fatalf("KB top-k = %d", searcher.options[0].TopK)
	}
	if supplemental.limit != 8 {
		t.Fatalf("HackerOne candidate limit = %d; want 2*top_k", supplemental.limit)
	}
}

func TestOnlyGetAndHeadAreAllowed(t *testing.T) {
	server := fakeAPI(&fakeKBSearch{result: kb.SearchResult{Hits: []kb.Hit{}}}, nil)
	defer server.Close()

	for _, path := range []string{"/api/health", "/api/kb/search?query=synthetic"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			response, body := request(t, http.DefaultClient, method, server.URL+path)
			if response.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s status = %d body = %s", method, path, response.StatusCode, body)
				continue
			}
			if response.Header.Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q", method, path, response.Header.Get("Allow"))
			}
		}
		response, _ := request(t, http.DefaultClient, http.MethodHead, server.URL+path)
		if response.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s status = %d", path, response.StatusCode)
		}
	}
}

func TestListenAddrPrefersEnvironment(t *testing.T) {
	if got := listenAddr(""); got != defaultListenAddr {
		t.Fatalf("listenAddr(empty) = %q", got)
	}
	if got := listenAddr(" 127.0.0.1:19999 "); got != "127.0.0.1:19999" {
		t.Fatalf("listenAddr(env) = %q", got)
	}
}

func TestRunRequiresReadableKBIndex(t *testing.T) {
	env := map[string]string{}
	err := run(nil, func(key string) string { return env[key] }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), kbIndexEnvVar) {
		t.Fatalf("run without %s error = %v", kbIndexEnvVar, err)
	}

	env[kbIndexEnvVar] = filepath.Join(t.TempDir(), "missing.sqlite3")
	err = run(nil, func(key string) string { return env[key] }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "open local KB index") {
		t.Fatalf("run with missing index error = %v", err)
	}
}
