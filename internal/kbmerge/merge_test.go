package kbmerge

import (
	"math"
	"testing"

	"bb-graph-assistant/internal/hackerone"
	"bb-graph-assistant/internal/kb"
)

func TestSplitSources(t *testing.T) {
	include, families := SplitSources(nil)
	if !include || families != nil {
		t.Fatalf("SplitSources(nil) = %v/%v", include, families)
	}
	include, families = SplitSources([]string{"hackerone"})
	if !include || len(families) != 0 {
		t.Fatalf("SplitSources(hackerone) = %v/%v", include, families)
	}
	include, families = SplitSources([]string{"nvd", "hackerone", "owasp"})
	if !include || len(families) != 2 || families[0] != "nvd" || families[1] != "owasp" {
		t.Fatalf("SplitSources(mixed) = %v/%v", include, families)
	}
	include, families = SplitSources([]string{"nvd"})
	if include || len(families) != 1 || families[0] != "nvd" {
		t.Fatalf("SplitSources(nvd) = %v/%v", include, families)
	}
}

func TestCandidateLimit(t *testing.T) {
	for _, test := range []struct{ topK, want int }{{1, 2}, {5, 10}, {20, 40}} {
		if got := CandidateLimit(test.topK); got != test.want {
			t.Fatalf("CandidateLimit(%d) = %d; want %d", test.topK, got, test.want)
		}
	}
}

func TestHackerOneOnlyMapping(t *testing.T) {
	bounty := 2500.0
	meta := hackerone.Meta{SnapshotID: "snap", DatasetCommit: "commit"}
	hits := []hackerone.Hit{
		{ReportID: "1", Program: "acme", Title: "RCE", URL: "https://hackerone.com/reports/1",
			VulnType: "rce", Severity: "critical", Weakness: "CWE-94", Bounty: &bounty,
			DisclosedAt: "2022-05-01", HasBody: true, Excerpt: "excerpt", BM25: -1.5},
		{ReportID: "2", Title: "XSS"},
	}
	result := HackerOneOnly(hits, meta)
	if result.SnapshotID != "snap" || result.CoverageState != "DONE" || result.RetrievalMode != "hackerone_fts" ||
		result.LexicalEngine != "sqlite-fts5" || !result.SupplementalIncluded {
		t.Fatalf("result metadata = %+v", result)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("hits = %d", len(result.Hits))
	}
	first := result.Hits[0]
	if first.ChunkID != "hackerone:1" || first.SourceName != "hackerone" || first.SourceID != "1" {
		t.Fatalf("hit identity = %+v", first)
	}
	if first.SourceLocator == nil || *first.SourceLocator != "reports/1.md" {
		t.Fatalf("locator = %+v", first.SourceLocator)
	}
	if first.SourceRevision == nil || *first.SourceRevision != "commit" {
		t.Fatalf("revision = %+v", first.SourceRevision)
	}
	if first.Attribution == nil || *first.Attribution != Attribution {
		t.Fatalf("attribution = %+v", first.Attribution)
	}
	if first.Metadata["bounty"] != 2500.0 || first.Metadata["has_body"] != true || first.Metadata["severity"] != "critical" {
		t.Fatalf("metadata = %#v", first.Metadata)
	}
	if first.BM25 != -1.5 || first.RankScore != 1.0/61 {
		t.Fatalf("scores = bm25:%v rank:%v", first.BM25, first.RankScore)
	}
	if second := result.Hits[1]; second.Metadata["bounty"] != nil || second.Metadata["upvotes"] != nil ||
		second.RankScore != 1.0/62 {
		t.Fatalf("second hit = %+v", second)
	}
}

func TestMergeInterleavesAndTruncates(t *testing.T) {
	result := kb.SearchResult{
		SnapshotID: "snap", CoverageState: "PARTIAL", RetrievalMode: "fulltext",
		Hits: []kb.Hit{{ChunkID: "k1", SourceName: "nvd"}, {ChunkID: "k2", SourceName: "nvd"}},
	}
	hits := []hackerone.Hit{{ReportID: "1"}, {ReportID: "2"}}
	merged := Merge(result, hits, hackerone.Meta{SnapshotID: "other", DatasetCommit: "commit"}, 10)
	if merged.SnapshotID != "snap" || !merged.SupplementalIncluded {
		t.Fatalf("merged metadata = %+v", merged)
	}
	if len(merged.Hits) != 4 {
		t.Fatalf("merged hits = %d", len(merged.Hits))
	}
	wantSources := []string{"nvd", "hackerone", "nvd", "hackerone"}
	wantScores := []float64{1.0 / 61, 1.0 / 61, 1.0 / 62, 1.0 / 62}
	for index, hit := range merged.Hits {
		if hit.SourceName != wantSources[index] {
			t.Fatalf("hit %d source = %q; want %q", index, hit.SourceName, wantSources[index])
		}
		if math.Abs(hit.RankScore-wantScores[index]) > 1e-12 {
			t.Fatalf("hit %d rank_score = %v; want %v", index, hit.RankScore, wantScores[index])
		}
	}

	truncated := Merge(result, hits, hackerone.Meta{}, 3)
	if len(truncated.Hits) != 3 {
		t.Fatalf("truncated hits = %d; want top_k=3", len(truncated.Hits))
	}
}

func TestMergeAccumulatesDuplicateIdentity(t *testing.T) {
	result := kb.SearchResult{Hits: []kb.Hit{{ChunkID: "hackerone:1", SourceName: "nvd"}}}
	merged := Merge(result, []hackerone.Hit{{ReportID: "1"}}, hackerone.Meta{}, 5)
	if len(merged.Hits) != 1 {
		t.Fatalf("duplicate identity produced %d hits", len(merged.Hits))
	}
	if merged.Hits[0].SourceName != "nvd" {
		t.Fatalf("duplicate kept %q instead of the primary KB hit", merged.Hits[0].SourceName)
	}
	if math.Abs(merged.Hits[0].RankScore-2.0/61) > 1e-12 {
		t.Fatalf("duplicate rank_score = %v; want both contributions", merged.Hits[0].RankScore)
	}
}

func TestMergeWithoutSupplementalHits(t *testing.T) {
	result := kb.SearchResult{Hits: []kb.Hit{{ChunkID: "k1", SourceName: "nvd"}}}
	merged := Merge(result, nil, hackerone.Meta{}, 5)
	if merged.SupplementalIncluded || len(merged.Hits) != 1 {
		t.Fatalf("merged = %+v", merged)
	}
	if math.Abs(merged.Hits[0].RankScore-1.0/61) > 1e-12 {
		t.Fatalf("rank_score = %v", merged.Hits[0].RankScore)
	}
}
