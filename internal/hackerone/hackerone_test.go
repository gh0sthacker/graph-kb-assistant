package hackerone

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureSnapshot = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fixtureCommit   = "abcdef0123456789abcdef0123456789abcdef01"
)

func createFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
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
			t.Fatalf("create fixture schema: %v", err)
		}
	}
	meta := map[string]string{
		"schema_version":  SchemaVersion,
		"dataset_commit":  fixtureCommit,
		"built_at":        "2026-11-05T12:00:00Z",
		"report_count":    "5",
		"with_body_count": "4",
		"snapshot_id":     fixtureSnapshot,
	}
	for key, value := range meta {
		if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, key, value); err != nil {
			t.Fatalf("insert fixture metadata %s: %v", key, err)
		}
	}
	rows := []struct {
		id, program, title, url, vulnType, severity, weakness, disclosed string
		body                                                             any
		bounty                                                           any
		upvotes                                                          any
		hasBody                                                          int
	}{
		{
			id: "1001", program: "acme", title: "Remote code execution in Log4j lookup",
			url: "https://hackerone.com/reports/1001", vulnType: "rce", severity: "critical",
			weakness: "CWE-502", disclosed: "2021-12-10", hasBody: 1,
			body:   "synthetic security fixture: CVE-2021-44228 JNDI lookup allows remote code execution",
			bounty: 5000.0, upvotes: 42,
		},
		{
			id: "1002", program: "acme", title: "SQL injection in login form",
			url: "https://hackerone.com/reports/1002", vulnType: "sqli", severity: "high",
			weakness: "CWE-89", disclosed: "2022-01-05", hasBody: 1,
			body:   "synthetic security fixture: authentication bypass through sql injection",
			bounty: nil, upvotes: 10,
		},
		{
			id: "1003", program: "globex", title: "Stored XSS in profile",
			url: "https://hackerone.com/reports/1003", vulnType: "xss", severity: "medium",
			weakness: "CWE-79", disclosed: "2022-02-01", hasBody: 1,
			body:   "synthetic security fixture: stored cross-site scripting payload",
			bounty: 500.0, upvotes: 3,
		},
		{
			id: "1004", program: "globex", title: "Information disclosure in export",
			url: "https://hackerone.com/reports/1004", vulnType: "info", severity: "low",
			weakness: "CWE-200", disclosed: "2022-03-01", hasBody: 0,
			body: nil, bounty: nil, upvotes: nil,
		},
		{
			id: "1005", program: "initech", title: "SSRF in image proxy",
			url: "https://hackerone.com/reports/1005", vulnType: "ssrf", severity: "high",
			weakness: "CWE-918", disclosed: "2022-04-01", hasBody: 1,
			body:   "synthetic security fixture: server-side request forgery reaches internal metadata",
			bounty: 1500.0, upvotes: 7,
		},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO reports(report_id,program,title,url,vuln_type,severity,weakness,
			bounty,upvotes,disclosed_at,body,has_body,content_sha256)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.id, row.program, row.title, row.url, row.vulnType, row.severity, row.weakness,
			row.bounty, row.upvotes, row.disclosed, row.body, row.hasBody, strings.Repeat("a", 64)); err != nil {
			t.Fatalf("insert fixture report %s: %v", row.id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO reports_fts(reports_fts) VALUES('rebuild')`); err != nil {
		t.Fatalf("rebuild fixture FTS index: %v", err)
	}
}

func openFixture(t *testing.T) *Index {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.sqlite3")
	createFixture(t, path)
	index, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func TestOpenReadsMetadata(t *testing.T) {
	index := openFixture(t)
	meta := index.Meta()
	if meta.SchemaVersion != SchemaVersion || meta.DatasetCommit != fixtureCommit ||
		meta.SnapshotID != fixtureSnapshot || meta.ReportCount != 5 || meta.WithBodyCount != 4 ||
		meta.BuiltAt != "2026-11-05T12:00:00Z" {
		t.Fatalf("Meta() = %+v", meta)
	}
}

func TestSearchMatchesAndMapsReportFields(t *testing.T) {
	index := openFixture(t)
	hits, err := index.Search(context.Background(), "jndi", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search() hits = %d; want 1", len(hits))
	}
	hit := hits[0]
	if hit.ReportID != "1001" || hit.Program != "acme" || hit.URL != "https://hackerone.com/reports/1001" ||
		hit.VulnType != "rce" || hit.Severity != "critical" || hit.Weakness != "CWE-502" ||
		hit.DisclosedAt != "2021-12-10" || !hit.HasBody {
		t.Fatalf("Search() hit = %+v", hit)
	}
	if hit.Bounty == nil || *hit.Bounty != 5000 || hit.Upvotes == nil || *hit.Upvotes != 42 {
		t.Fatalf("Search() bounty/upvotes = %v/%v", hit.Bounty, hit.Upvotes)
	}
	if hit.Excerpt == "" || !strings.Contains(strings.ToLower(hit.Excerpt), "jndi") {
		t.Fatalf("Search() excerpt = %q", hit.Excerpt)
	}
	if hit.BM25 >= 0 {
		t.Fatalf("Search() bm25 = %v; want a negative FTS5 score", hit.BM25)
	}
}

func TestSearchQuotesCVEToken(t *testing.T) {
	index := openFixture(t)
	for _, query := range []string{"CVE-2021-44228", `"CVE-2021-44228"`} {
		hits, err := index.Search(context.Background(), query, 5)
		if err != nil {
			t.Fatalf("Search(%q) error = %v", query, err)
		}
		if len(hits) != 1 || hits[0].ReportID != "1001" {
			t.Fatalf("Search(%q) = %+v; want report 1001", query, hits)
		}
	}
}

func TestSearchNeutralizesFTSOperators(t *testing.T) {
	index := openFixture(t)
	hits, err := index.Search(context.Background(), "log4j OR nonexistentterm", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("Search() = %+v; quoted tokens must be combined as an AND phrase, not an OR operator", hits)
	}
}

func TestSearchRespectsLimitAndRanksByBM25(t *testing.T) {
	index := openFixture(t)
	hits, err := index.Search(context.Background(), "security", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search() hits = %d; want the requested top 2", len(hits))
	}
	if hits[0].BM25 > hits[1].BM25 {
		t.Fatalf("Search() order = %v then %v; want ascending (more negative first)", hits[0].BM25, hits[1].BM25)
	}
}

func TestSearchHandlesReportWithoutBody(t *testing.T) {
	index := openFixture(t)
	hits, err := index.Search(context.Background(), "disclosure", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 || hits[0].ReportID != "1004" || hits[0].HasBody {
		t.Fatalf("Search() = %+v; want the title-only report without a body", hits)
	}
	if hits[0].Excerpt != "" {
		t.Fatalf("Search() excerpt = %q; want empty for a report without a body", hits[0].Excerpt)
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	index := openFixture(t)
	for _, query := range []string{"", "   ", `"`} {
		if _, err := index.Search(context.Background(), query, 5); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("Search(%q) error = %v; want ErrInvalidQuery", query, err)
		}
	}
}

func TestOpenRejectsMissingOrInvalidIndex(t *testing.T) {
	if index, err := Open(""); index != nil || err != nil {
		t.Fatalf("Open(\"\") = %v, %v; want a nil no-op index", index, err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.sqlite3")); err == nil {
		t.Fatal("Open() accepted a missing index")
	}

	path := filepath.Join(t.TempDir(), "index.sqlite3")
	createFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value='hackerone-fts-v0' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open() accepted an unsupported schema version")
	}
}

func TestNilIndexIsUnavailable(t *testing.T) {
	var index *Index
	if _, err := index.Search(context.Background(), "log4j", 5); !errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("Search() error = %v; want ErrIndexUnavailable", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if meta := index.Meta(); meta != (Meta{}) {
		t.Fatalf("Meta() = %+v; want the zero value", meta)
	}
}

func TestBuildMatchExpression(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{query: "log4j", want: `"log4j"`},
		{query: "  remote   code  ", want: `"remote" "code"`},
		{query: `log4j "remote code execution"`, want: `"log4j" "remote code execution"`},
		{query: `qu"ote`, want: `"qu""ote"`},
		{query: `"say ""hello"""`, want: `"say ""hello"""`},
		{query: `"unterminated phrase`, want: `"unterminated phrase"`},
		{query: `"   "`, want: ""},
	}
	for _, testCase := range cases {
		if got := buildMatchExpression(testCase.query); got != testCase.want {
			t.Errorf("buildMatchExpression(%q) = %q; want %q", testCase.query, got, testCase.want)
		}
	}
	long := buildMatchExpression(strings.Repeat("a", maximumQueryRunes+500))
	if runes := []rune(long); len(runes) > maximumQueryRunes+2 {
		t.Fatalf("buildMatchExpression() kept %d runes; want the query capped at %d", len(runes), maximumQueryRunes)
	}
}
