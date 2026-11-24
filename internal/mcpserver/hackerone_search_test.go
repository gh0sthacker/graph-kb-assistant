package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bb-graph-assistant/internal/hackerone"
	"bb-graph-assistant/internal/kb"
)

const (
	hackeroneFixtureSnapshot = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	hackeroneFixtureCommit   = "1234567890abcdef1234567890abcdef12345678"
)

func createHackerOneIndexFixture(t *testing.T, path string) {
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
		"snapshot_id":     hackeroneFixtureSnapshot,
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

func openHackerOneIndexFixture(t *testing.T) *hackerone.Index {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hackerone-index.sqlite3")
	createHackerOneIndexFixture(t, path)
	index, err := hackerone.Open(path)
	if err != nil {
		t.Fatalf("hackerone.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func openKBIndexFixture(t *testing.T) *kb.Index {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kb-index.sqlite3")
	createKBFilterIndex(t, path)
	index, err := kb.OpenIndex(path)
	if err != nil {
		t.Fatalf("kb.OpenIndex() error = %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func TestKBSearchHackerOneOnlyReturnsSyntheticResult(t *testing.T) {
	index := openHackerOneIndexFixture(t)
	tools := &Tools{hackeroneIndex: index}
	result, err := tools.KBSearch(context.Background(), "remote", 5, []string{"hackerone"})
	if err != nil {
		t.Fatalf("KBSearch() error = %v", err)
	}
	if result.SnapshotID != hackeroneFixtureSnapshot || result.CoverageState != "PARTIAL" ||
		result.RetrievalMode != "hackerone_fts" || result.LexicalEngine != "sqlite-fts5" ||
		!result.SupplementalIncluded || result.CoverageReportHash != "" {
		t.Fatalf("KBSearch() metadata = %+v", result)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("KBSearch() hits = %d; want the single remote-execution report", len(result.Hits))
	}
	hit := result.Hits[0]
	if hit.ChunkID != "hackerone:2001" || hit.SourceName != "hackerone" || hit.SourceID != "2001" ||
		hit.Title != "Synthetic remote code execution" {
		t.Fatalf("KBSearch() hit identity = %+v", hit)
	}
	if hit.SourceURL == nil || *hit.SourceURL != "https://hackerone.com/reports/2001" ||
		hit.SourceLocator == nil || *hit.SourceLocator != "reports/2001.md" ||
		hit.SourceRevision == nil || *hit.SourceRevision != hackeroneFixtureCommit {
		t.Fatalf("KBSearch() hit provenance = %+v", hit)
	}
	if hit.Attribution == nil || *hit.Attribution != hackeroneAttribution || hit.LicenseID != nil {
		t.Fatalf("KBSearch() hit attribution = %v/%v", hit.Attribution, hit.LicenseID)
	}
	if hit.Metadata["program"] != "acme" || hit.Metadata["vuln_type"] != "rce" ||
		hit.Metadata["severity"] != "critical" || hit.Metadata["weakness"] != "CWE-94" ||
		hit.Metadata["disclosed_at"] != "2022-05-01" || hit.Metadata["has_body"] != true {
		t.Fatalf("KBSearch() hit metadata = %#v", hit.Metadata)
	}
	if hit.Metadata["bounty"] != float64(2500) || hit.Metadata["upvotes"] != 12 {
		t.Fatalf("KBSearch() hit bounty/upvotes = %#v", hit.Metadata)
	}
	if hit.BM25 >= 0 || hit.RankScore != reciprocalRank(1) {
		t.Fatalf("KBSearch() hit scores = bm25:%v rank:%v", hit.BM25, hit.RankScore)
	}
}

func TestKBSearchHackerOneOnlyHonorsTopK(t *testing.T) {
	tools := &Tools{hackeroneIndex: openHackerOneIndexFixture(t)}
	result, err := tools.KBSearch(context.Background(), "synthetic", 1, []string{"hackerone"})
	if err != nil {
		t.Fatalf("KBSearch() error = %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("KBSearch() hits = %d; want top_k=1", len(result.Hits))
	}
}

func TestKBSearchHackerOneSourceFiltering(t *testing.T) {
	tools := &Tools{kbIndex: openKBIndexFixture(t), hackeroneIndex: openHackerOneIndexFixture(t)}
	ctx := context.Background()

	t.Run("kb family excludes hackerone", func(t *testing.T) {
		result, err := tools.KBSearch(ctx, "synthetic", 5, []string{"nvd"})
		if err != nil {
			t.Fatalf("KBSearch() error = %v", err)
		}
		if len(result.Hits) != 2 || result.SupplementalIncluded {
			t.Fatalf("KBSearch() = %d hits supplemental=%v; want only the two KB hits", len(result.Hits), result.SupplementalIncluded)
		}
		for _, hit := range result.Hits {
			if hit.SourceName != "nvd" {
				t.Fatalf("KBSearch() hit = %+v; want no HackerOne hits", hit)
			}
		}
	})

	t.Run("hackerone only skips the KB index", func(t *testing.T) {
		result, err := tools.KBSearch(ctx, "synthetic", 5, []string{hackeroneSourceName})
		if err != nil {
			t.Fatalf("KBSearch() error = %v", err)
		}
		if result.RetrievalMode != "hackerone_fts" || len(result.Hits) != 3 {
			t.Fatalf("KBSearch() = %+v; want the three HackerOne-only hits", result)
		}
		for _, hit := range result.Hits {
			if hit.SourceName != hackeroneSourceName {
				t.Fatalf("KBSearch() hit = %+v; want only HackerOne hits", hit)
			}
		}
	})

	t.Run("mixed sources merge", func(t *testing.T) {
		result, err := tools.KBSearch(ctx, "synthetic", 3, []string{"nvd", hackeroneSourceName})
		if err != nil {
			t.Fatalf("KBSearch() error = %v", err)
		}
		if len(result.Hits) != 3 || !result.SupplementalIncluded {
			t.Fatalf("KBSearch() = %d hits supplemental=%v", len(result.Hits), result.SupplementalIncluded)
		}
		sources := []string{result.Hits[0].SourceName, result.Hits[1].SourceName, result.Hits[2].SourceName}
		if !reflect.DeepEqual(sources, []string{"nvd", hackeroneSourceName, "nvd"}) {
			t.Fatalf("KBSearch() source order = %v; want the fused nvd/hackerone/nvd order", sources)
		}
	})

	t.Run("unknown family excludes every source", func(t *testing.T) {
		result, err := tools.KBSearch(ctx, "synthetic", 5, []string{"missing-family"})
		if err != nil {
			t.Fatalf("KBSearch() error = %v", err)
		}
		if len(result.Hits) != 0 || result.SupplementalIncluded || result.SnapshotID != strings.Repeat("1", 64) {
			t.Fatalf("KBSearch() = %+v; want an empty KB result without supplemental hits", result)
		}
	})
}

func TestKBSearchMergesWithReciprocalRankFusion(t *testing.T) {
	tools := &Tools{kbIndex: openKBIndexFixture(t), hackeroneIndex: openHackerOneIndexFixture(t)}
	result, err := tools.KBSearch(context.Background(), "synthetic", 3, nil)
	if err != nil {
		t.Fatalf("KBSearch() error = %v", err)
	}
	if result.SnapshotID != strings.Repeat("1", 64) || result.CoverageState != "PARTIAL" {
		t.Fatalf("KBSearch() snapshot = %q/%q; want the KB snapshot fields preserved", result.SnapshotID, result.CoverageState)
	}
	if !result.SupplementalIncluded {
		t.Fatal("KBSearch() did not mark the merged result as supplemental")
	}
	if len(result.Hits) != 3 {
		t.Fatalf("KBSearch() hits = %d; want top_k=3", len(result.Hits))
	}
	wantScores := []float64{reciprocalRank(1), reciprocalRank(1), reciprocalRank(2)}
	wantSources := []string{"nvd", hackeroneSourceName, "nvd"}
	for index, hit := range result.Hits {
		if hit.SourceName != wantSources[index] {
			t.Fatalf("KBSearch() hit %d source = %q; want %q", index, hit.SourceName, wantSources[index])
		}
		if math.Abs(hit.RankScore-wantScores[index]) > 1e-12 {
			t.Fatalf("KBSearch() hit %d rank score = %v; want %v", index, hit.RankScore, wantScores[index])
		}
		if index > 0 && result.Hits[index-1].RankScore < hit.RankScore {
			t.Fatalf("KBSearch() rank scores are not sorted descending: %v then %v", result.Hits[index-1].RankScore, hit.RankScore)
		}
	}
}

func TestKBSearchDegradesGracefullyWithoutTheKBPrimaryIndex(t *testing.T) {
	ctx := context.Background()
	hackeroneIndex := openHackerOneIndexFixture(t)

	result, err := (&Tools{hackeroneIndex: hackeroneIndex}).KBSearch(ctx, "synthetic", 2, nil)
	if err != nil {
		t.Fatalf("KBSearch() error = %v; want a degraded HackerOne result", err)
	}
	if result.RetrievalMode != "hackerone_fts" || result.SnapshotID != hackeroneFixtureSnapshot {
		t.Fatalf("KBSearch() = %+v; want the synthetic HackerOne result", result)
	}

	if _, err := (&Tools{}).KBSearch(ctx, "synthetic", 2, nil); !errors.Is(err, ErrKBUnavailable) {
		t.Fatalf("KBSearch() error = %v; want ErrKBUnavailable when both indexes are absent", err)
	}
	if _, err := (&Tools{hackeroneIndex: hackeroneIndex}).KBSearch(ctx, "synthetic", 2, []string{"nvd"}); !errors.Is(err, ErrKBUnavailable) {
		t.Fatalf("KBSearch() error = %v; want no HackerOne fallback when the filter excludes it", err)
	}
	if _, err := (&Tools{kbIndex: openKBIndexFixture(t)}).KBSearch(ctx, "synthetic", 2, []string{hackeroneSourceName}); !errors.Is(err, ErrKBUnavailable) {
		t.Fatalf("KBSearch() error = %v; want ErrKBUnavailable for a HackerOne-only request without that index", err)
	}
}

func TestSplitKBSourcesStripsThePseudoSource(t *testing.T) {
	include, families := splitKBSources([]string{"nvd", hackeroneSourceName, "cve"})
	if !include || !reflect.DeepEqual(families, []string{"nvd", "cve"}) {
		t.Fatalf("splitKBSources() = %v, %v", include, families)
	}
	if include, families := splitKBSources(nil); !include || families != nil {
		t.Fatalf("splitKBSources(nil) = %v, %v", include, families)
	}
	if include, families := splitKBSources([]string{hackeroneSourceName}); !include || len(families) != 0 {
		t.Fatalf("splitKBSources() = %v, %v; want no KB families", include, families)
	}
}

func TestNewToolsLoadsTheOptionalHackerOneIndex(t *testing.T) {
	t.Setenv("BB_ASSISTANT_KB_BACKEND", "")
	t.Setenv("BB_ASSISTANT_KB_LEXICAL_ENGINE", "")
	t.Setenv("BB_ASSISTANT_KB_PYTHON", "")
	t.Setenv("BB_ASSISTANT_KB_MODELS", "0")
	root := t.TempDir()
	indexPath := filepath.Join(root, "hackerone.sqlite3")
	createHackerOneIndexFixture(t, indexPath)
	t.Setenv("BB_ASSISTANT_HACKERONE_INDEX", indexPath)

	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatalf("NewTools() error = %v", err)
	}
	defer tools.Close()
	if tools.hackeroneIndex == nil {
		t.Fatal("NewTools() did not open BB_ASSISTANT_HACKERONE_INDEX")
	}
	result, err := tools.KBSearch(context.Background(), "remote", 2, nil)
	if err != nil {
		t.Fatalf("KBSearch() error = %v", err)
	}
	if len(result.Hits) != 1 || result.Hits[0].SourceName != hackeroneSourceName {
		t.Fatalf("KBSearch() = %+v; want the HackerOne fixture hit", result)
	}
}

func TestNewToolsIgnoresAnInvalidHackerOneIndex(t *testing.T) {
	t.Setenv("BB_ASSISTANT_KB_BACKEND", "")
	t.Setenv("BB_ASSISTANT_KB_LEXICAL_ENGINE", "")
	t.Setenv("BB_ASSISTANT_KB_PYTHON", "")
	t.Setenv("BB_ASSISTANT_KB_MODELS", "0")
	root := t.TempDir()
	invalidPath := filepath.Join(root, "hackerone.sqlite3")
	if err := os.WriteFile(invalidPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BB_ASSISTANT_HACKERONE_INDEX", invalidPath)

	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatalf("NewTools() error = %v; an invalid supplemental index must not fail startup", err)
	}
	defer tools.Close()
	if tools.hackeroneIndex != nil {
		t.Fatal("NewTools() kept an invalid HackerOne index open")
	}
	if _, err := tools.KBSearch(context.Background(), "remote", 2, nil); !errors.Is(err, ErrKBUnavailable) {
		t.Fatalf("KBSearch() error = %v; want ErrKBUnavailable", err)
	}
}
