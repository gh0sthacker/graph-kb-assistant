package kb

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeRedAmonLuceneQuery(t *testing.T) {
	for input, want := range map[string]string{
		"CVE-2025/42 && auth||tenant":        "CVE 2025 42   auth tenant",
		`user:"name" AND (role:admin)`:       "user  name  AND  role admin",
		" + - ! ( ) { } [ ] ^ ~ * ? : \\ / ": "",
		"  AND OR NOT  ":                     "AND OR NOT",
	} {
		if got := sanitizeRedAmonLuceneQuery(input); got != want {
			t.Errorf("sanitizeRedAmonLuceneQuery(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNeo4jLuceneProviderDoesNotReuseGraphDatabaseConfiguration(t *testing.T) {
	t.Setenv("BB_ASSISTANT_NEO4J_URI", "bolt://127.0.0.1:7687")
	t.Setenv("BB_ASSISTANT_NEO4J_USERNAME", "graph-user")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "graph-secret")
	t.Setenv("BB_ASSISTANT_NEO4J_DATABASE", "neo4j")
	for _, suffix := range []string{"URI", "USERNAME", "PASSWORD", "DATABASE"} {
		t.Setenv("BB_ASSISTANT_KB_NEO4J_"+suffix, "")
	}

	if _, err := NewNeo4jLuceneProvider(); err == nil {
		t.Fatal("Neo4j KB provider unexpectedly reused the application's graph settings")
	}
}

func TestNeo4jLuceneProviderBindsGenerationAndUsesUpstreamIndexContract(t *testing.T) {
	snapshot := strings.Repeat("1", 64)
	executor := &fakeLuceneExecutor{generation: validLuceneGeneration(snapshot), candidates: []LuceneCandidate{{
		ChunkID: strings.Repeat("a", 16), SnapshotID: snapshot, SourceName: "nvd", Score: 3.25,
	}}}
	provider := newNeo4jLuceneProviderWithExecutor(executor)
	got, err := provider.Search(context.Background(), "CVE-2025/42", snapshot, 30, []string{"nvd"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if executor.indexName != "kb_"+snapshot || executor.query != "CVE 2025 42" || executor.limit != 30 ||
		!reflect.DeepEqual(executor.sources, []string{"nvd"}) {
		t.Fatalf("query contract = index:%q query:%q limit:%d sources:%v", executor.indexName, executor.query, executor.limit, executor.sources)
	}
	if len(got) != 1 || got[0].Score != 3.25 {
		t.Fatalf("Search() = %#v", got)
	}
	if _, err := provider.Search(context.Background(), "topic", snapshot, 30, nil, true); err != nil {
		t.Fatal(err)
	}
	if executor.indexName != "kb_"+snapshot+"_all" {
		t.Fatalf("supplemental index = %q", executor.indexName)
	}
}

func TestNeo4jLuceneProviderRejectsGenerationAndCandidateMismatches(t *testing.T) {
	snapshot := strings.Repeat("1", 64)
	otherSnapshot := strings.Repeat("2", 64)
	tests := []struct {
		name       string
		generation luceneGeneration
		candidates []LuceneCandidate
	}{
		{name: "snapshot marker mismatch", generation: validLuceneGeneration(otherSnapshot)},
		{name: "non-ready generation", generation: luceneGeneration{SnapshotID: snapshot, State: "building", SchemaVersion: 1, CoreCount: 1, TotalCount: 1, CoreSHA256: snapshot, TotalSHA256: snapshot}},
		{name: "empty core generation", generation: luceneGeneration{SnapshotID: snapshot, State: "ready", SchemaVersion: 1, CoreCount: 0, TotalCount: 1, CoreSHA256: snapshot, TotalSHA256: snapshot}},
		{name: "node snapshot mismatch", generation: validLuceneGeneration(snapshot), candidates: []LuceneCandidate{{ChunkID: strings.Repeat("a", 16), SnapshotID: otherSnapshot, SourceName: "nvd", Score: 1}}},
		{name: "duplicate identity", generation: validLuceneGeneration(snapshot), candidates: []LuceneCandidate{{ChunkID: strings.Repeat("a", 16), SnapshotID: snapshot, SourceName: "nvd", Score: 1}, {ChunkID: strings.Repeat("a", 16), SnapshotID: snapshot, SourceName: "nvd", Score: 2}}},
		{name: "non-finite score", generation: validLuceneGeneration(snapshot), candidates: []LuceneCandidate{{ChunkID: strings.Repeat("a", 16), SnapshotID: snapshot, SourceName: "nvd", Score: math.Inf(1)}}},
		{name: "negative score", generation: validLuceneGeneration(snapshot), candidates: []LuceneCandidate{{ChunkID: strings.Repeat("a", 16), SnapshotID: snapshot, SourceName: "nvd", Score: -1}}},
		{name: "too many results", generation: validLuceneGeneration(snapshot), candidates: []LuceneCandidate{{ChunkID: strings.Repeat("a", 16), SnapshotID: snapshot, SourceName: "nvd", Score: 1}, {ChunkID: strings.Repeat("b", 16), SnapshotID: snapshot, SourceName: "nvd", Score: 2}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeLuceneExecutor{generation: test.generation, candidates: test.candidates}
			provider := newNeo4jLuceneProviderWithExecutor(executor)
			limit := 10
			if test.name == "too many results" {
				limit = 1
			}
			if _, err := provider.Search(context.Background(), "topic", snapshot, limit, nil, false); err == nil {
				t.Fatal("Search() unexpectedly accepted an invalid generation/candidate")
			}
		})
	}
}

func TestNeo4jLuceneProviderRejectsLimitAboveMaximum(t *testing.T) {
	snapshot := strings.Repeat("1", 64)
	provider := newNeo4jLuceneProviderWithExecutor(&fakeLuceneExecutor{generation: validLuceneGeneration(snapshot)})
	if _, err := provider.Search(context.Background(), "topic", snapshot, 601, nil, false); err == nil {
		t.Fatal("Search() accepted a limit greater than 600")
	}
}

func validLuceneGeneration(snapshot string) luceneGeneration {
	return luceneGeneration{
		SnapshotID: snapshot, State: "ready", SchemaVersion: 1, CoreCount: 2, TotalCount: 3,
		CoreSHA256: strings.Repeat("3", 64), TotalSHA256: strings.Repeat("4", 64),
	}
}

type fakeLuceneExecutor struct {
	generation luceneGeneration
	candidates []LuceneCandidate
	indexName  string
	query      string
	limit      int
	sources    []string
}

func (f *fakeLuceneExecutor) ReadGeneration(_ context.Context, _ string) (luceneGeneration, error) {
	return f.generation, nil
}

func (f *fakeLuceneExecutor) QueryFulltext(_ context.Context, indexName, query string, limit int, sources []string) ([]LuceneCandidate, error) {
	f.indexName, f.query, f.limit = indexName, query, limit
	f.sources = append([]string(nil), sources...)
	return append([]LuceneCandidate(nil), f.candidates...), nil
}

func (*fakeLuceneExecutor) Close() error { return nil }
