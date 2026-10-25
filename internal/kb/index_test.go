package kb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenIndexReadsPythonCompatibleFTSIndexReadOnly(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite3")
	content := "Authorization boundaries associate an actor with a tenant. " +
		strings.Repeat("The synthetic fixture documents ownership checks and request context. ", 6)
	sourceHash := digestHex(content)
	chunkHash := digestHex(content)

	createPythonCompatibleIndex(t, indexPath, content, sourceHash, chunkHash)
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read fixture before opening: %v", err)
	}

	index, err := OpenIndex(indexPath)
	if err != nil {
		t.Fatalf("OpenIndex() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := index.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.SnapshotID != strings.Repeat("1", 64) || stats.CoverageState != "PARTIAL" {
		t.Fatalf("Stats() snapshot/coverage = %q/%q", stats.SnapshotID, stats.CoverageState)
	}
	if stats.SourceFamilies != 1 || stats.SourceDocuments != 1 || stats.Chunks != 1 {
		t.Fatalf("Stats() counts = %d families, %d documents, %d chunks; want 1/1/1", stats.SourceFamilies, stats.SourceDocuments, stats.Chunks)
	}
	if stats.CoverageReportHash == nil || *stats.CoverageReportHash != strings.Repeat("2", 64) || !stats.ReferenceMaterialIsUntrusted {
		t.Fatalf("Stats() lost coverage provenance or untrusted-reference marker: %+v", stats)
	}
	if len(stats.SourceCoverage) != 1 || stats.SourceCoverage[0]["source_name"] != "synthetic" || stats.SourceCoverage[0]["state"] != "partial" {
		t.Fatalf("Stats() source coverage = %#v", stats.SourceCoverage)
	}

	search, err := index.Search(ctx, "authorization tenant", SearchOptions{TopK: 5, SourceNames: []string{"synthetic"}})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if search.SnapshotID != stats.SnapshotID || search.CoverageState != stats.CoverageState || len(search.Hits) != 1 {
		t.Fatalf("Search() snapshot/coverage/hits = %q/%q/%d", search.SnapshotID, search.CoverageState, len(search.Hits))
	}
	hit := search.Hits[0]
	if hit.SourceName != "synthetic" || hit.SourceID != "document-1" || hit.Title != "Synthetic authorization guide" {
		t.Fatalf("Search() source identity = %+v", hit)
	}
	if hit.SourceSHA256 != sourceHash || hit.ChunkSHA256 != chunkHash || hit.StartChar != 0 || hit.EndChar != len([]rune(content)) {
		t.Fatalf("Search() provenance/span = %+v", hit)
	}
	if hit.Metadata["kind"] != "fixture" || hit.Metadata["revision"] != json.Number("7") {
		t.Fatalf("Search() metadata = %#v", hit.Metadata)
	}

	filtered, err := index.Search(ctx, "authorization tenant", SearchOptions{SourceNames: []string{"other"}})
	if err != nil || len(filtered.Hits) != 0 {
		t.Fatalf("Search() with an unmatched source filter = (%d hits, %v); want (0, nil)", len(filtered.Hits), err)
	}

	source, err := index.GetSource(ctx, "synthetic", "document-1", 512)
	if err != nil {
		t.Fatalf("GetSource() error = %v", err)
	}
	if source.Content != content || source.TotalChars != int64(len([]rune(content))) || source.Truncated || !source.ContentHashVerified {
		t.Fatalf("GetSource() content verification = chars:%d total:%d truncated:%v verified:%v", source.ContentChars, source.TotalChars, source.Truncated, source.ContentHashVerified)
	}
	if source.ContentSHA256 != sourceHash || source.SourceURL == nil || *source.SourceURL != "https://example.invalid/guide" || source.Metadata["kind"] != "fixture" {
		t.Fatalf("GetSource() provenance = %+v", source)
	}

	truncated, err := index.GetSource(ctx, "synthetic", "document-1", 256)
	if err != nil {
		t.Fatalf("GetSource() truncated read error = %v", err)
	}
	if !truncated.Truncated || truncated.ContentHashVerified || truncated.ContentChars != 256 || truncated.TotalChars <= int64(truncated.ContentChars) {
		t.Fatalf("GetSource() truncation state = chars:%d total:%d truncated:%v verified:%v", truncated.ContentChars, truncated.TotalChars, truncated.Truncated, truncated.ContentHashVerified)
	}

	if _, err := index.GetSource(ctx, "synthetic", "missing", 512); err == nil {
		t.Fatal("GetSource() for an unknown source succeeded")
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read fixture after opening: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only KB operations modified the SQLite index")
	}
	entries, err := os.ReadDir(filepath.Dir(indexPath))
	if err != nil {
		t.Fatalf("read fixture directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-wal") || strings.HasSuffix(entry.Name(), "-shm") || strings.HasSuffix(entry.Name(), "-journal") {
			t.Fatalf("read-only KB operations left a SQLite sidecar file: %s", entry.Name())
		}
	}
}

func TestSearchDoesNotReturnLicenseNoticeOnlyMatches(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite3")
	content := "ordinary authentication reference content"
	createPythonCompatibleIndex(t, indexPath, content, digestHex(content), digestHex(content))
	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	notice := "rarequestermonly"
	_, err = db.Exec(`INSERT INTO chunks(
		chunk_id,source_name,source_id,title,start_char,end_char,metadata_json,source_sha256,chunk_sha256,chunk_schema,text
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, strings.Repeat("b", 64), "synthetic", "NOTICE:license", "License", 0,
		len(notice), `{"document_role":"license_notice"}`, digestHex(notice), digestHex(notice), "bb-kb-chunk-v1", notice)
	if err == nil {
		_, err = db.Exec(`INSERT INTO chunk_search(chunk_id,source_name,title,text) VALUES(?,?,?,?)`,
			strings.Repeat("b", 64), "synthetic", "License", notice)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("add synthetic license-only row: %v", err)
	}
	index, err := OpenIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	result, err := index.Search(context.Background(), "rarequestermonly", SearchOptions{})
	if err != nil || len(result.Hits) != 0 {
		t.Fatalf("license-only search = %d hits, %v; want no hits", len(result.Hits), err)
	}
}

func TestOpenIndexDoesNotCreateMissingIndex(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "missing", "index.sqlite3")
	if _, err := OpenIndex(indexPath); err == nil {
		t.Fatal("OpenIndex() accepted a missing index")
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("OpenIndex() created a missing index or returned an unexpected stat error: %v", err)
	}
}

func TestRankFullTextHitsUsesRedAmonBoostsDiversityAndTopK(t *testing.T) {
	hits := []Hit{
		{ChunkID: "a", SourceName: "tool_docs", Title: "Authentication Guide"},
		{ChunkID: "b", SourceName: "tool_docs", Title: "Authentication Guide duplicate"},
		{ChunkID: "c", SourceName: "gtfobins", Title: "Linux privilege escalation"},
		{ChunkID: "d", SourceName: "nvd", Title: "CVE record"},
	}
	ranked := rankFullTextHits(hits, 2)
	if len(ranked) != 2 || ranked[0].ChunkID != "a" || ranked[1].ChunkID != "c" {
		t.Fatalf("rankFullTextHits() IDs = %#v, want [a c]", []string{ranked[0].ChunkID, ranked[1].ChunkID})
	}
	if ranked[0].RankScore <= ranked[1].RankScore || ranked[1].RankScore <= 0 {
		t.Fatalf("rankFullTextHits() scores = %f, %f", ranked[0].RankScore, ranked[1].RankScore)
	}
}

func TestKBChunkSimilarityMatchesSourceAndTitleProxy(t *testing.T) {
	similar := kbChunkSimilarity(
		Hit{SourceName: "owasp", Title: "Session authentication testing"},
		Hit{SourceName: "owasp", Title: "Session authentication checks"},
	)
	different := kbChunkSimilarity(
		Hit{SourceName: "owasp", Title: "Session authentication testing"},
		Hit{SourceName: "nvd", Title: "Kernel memory corruption"},
	)
	if similar <= different || similar > 1 || different < 0 {
		t.Fatalf("similarity values = %f and %f", similar, different)
	}
}

func createPythonCompatibleIndex(t *testing.T, path, content, sourceHash, chunkHash string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture SQLite database: %v", err)
	}
	defer db.Close()

	statements := []string{
		`CREATE TABLE kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE source_documents (
			source_name TEXT NOT NULL, source_id TEXT NOT NULL, title TEXT NOT NULL,
			source_url TEXT, source_locator TEXT, source_revision TEXT, license_id TEXT, attribution TEXT,
			metadata_json TEXT NOT NULL, content_sha256 TEXT NOT NULL, text TEXT NOT NULL,
			PRIMARY KEY(source_name,source_id))`,
		`CREATE TABLE chunks (
			chunk_id TEXT PRIMARY KEY, source_name TEXT NOT NULL, source_id TEXT NOT NULL, title TEXT NOT NULL,
			start_char INTEGER NOT NULL, end_char INTEGER NOT NULL, source_locator TEXT, source_url TEXT,
			source_revision TEXT, license_id TEXT, attribution TEXT, metadata_json TEXT NOT NULL,
			source_sha256 TEXT NOT NULL, chunk_sha256 TEXT NOT NULL, chunk_schema TEXT NOT NULL, text TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE chunk_search USING fts5(
			chunk_id UNINDEXED, source_name UNINDEXED, title, text,
			tokenize='unicode61 remove_diacritics 2')`,
		`CREATE INDEX chunks_source_idx ON chunks(source_name,source_id)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create Python-compatible KB schema: %v", err)
		}
	}

	metadata := `{"kind":"fixture","revision":7}`
	const snapshotID = "1111111111111111111111111111111111111111111111111111111111111111"
	const reportHash = "2222222222222222222222222222222222222222222222222222222222222222"
	const chunkID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	coverage := `[{"source_name":"synthetic","state":"partial"}]`
	inserts := []struct {
		statement string
		args      []any
	}{
		{`INSERT INTO kb_meta(key,value) VALUES(?,?)`, []any{"snapshot_id", snapshotID}},
		{`INSERT INTO kb_meta(key,value) VALUES(?,?)`, []any{"coverage_state", "PARTIAL"}},
		{`INSERT INTO kb_meta(key,value) VALUES(?,?)`, []any{"coverage_report_sha256", reportHash}},
		{`INSERT INTO kb_meta(key,value) VALUES(?,?)`, []any{"source_coverage", coverage}},
		{`INSERT INTO source_documents(
			source_name,source_id,title,source_url,source_locator,source_revision,license_id,attribution,
			metadata_json,content_sha256,text) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			[]any{"synthetic", "document-1", "Synthetic authorization guide", "https://example.invalid/guide",
				"section-1", "rev-7", "CC0-1.0", "Synthetic fixture", metadata, sourceHash, content}},
		{`INSERT INTO chunks(
			chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,source_revision,
			license_id,attribution,metadata_json,source_sha256,chunk_sha256,chunk_schema,text)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			[]any{chunkID, "synthetic", "document-1", "Synthetic authorization guide", 0, len([]rune(content)),
				"section-1", "https://example.invalid/guide", "rev-7", "CC0-1.0", "Synthetic fixture", metadata,
				sourceHash, chunkHash, "bb-kb-chunk-v1", content}},
		{`INSERT INTO chunk_search(chunk_id,source_name,title,text) VALUES(?,?,?,?)`,
			[]any{chunkID, "synthetic", "Synthetic authorization guide", content}},
	}
	for _, insert := range inserts {
		if _, err := db.Exec(insert.statement, insert.args...); err != nil {
			t.Fatalf("populate Python-compatible KB fixture: %v", err)
		}
	}
}

func digestHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
