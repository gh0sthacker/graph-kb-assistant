package mcpserver

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"bb-graph-assistant/internal/kb"
)

func TestKBSearchExportedPathSearchesTheLocalIndex(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "kb-index.sqlite3")
	createKBFilterIndex(t, indexPath)
	index, err := kb.OpenIndex(indexPath)
	if err != nil {
		t.Fatalf("OpenIndex() error = %v", err)
	}
	defer index.Close()

	tools := &Tools{kbIndex: index}
	result, err := tools.KBSearch(context.Background(), "synthetic", 2, []string{"nvd"})
	if err != nil {
		t.Fatalf("KBSearch() error = %v", err)
	}
	if result.SnapshotID != strings.Repeat("1", 64) || result.CoverageState != "PARTIAL" {
		t.Errorf("KBSearch() snapshot/coverage = %q/%q", result.SnapshotID, result.CoverageState)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("KBSearch() hits = %d; want both synthetic chunks", len(result.Hits))
	}
	if result.Hits[0].SourceName != "nvd" || result.Hits[0].ChunkID == "" || result.Hits[0].Excerpt == "" {
		t.Errorf("KBSearch() first hit = %+v", result.Hits[0])
	}
}

func TestKBSearchExportedPathReportsUnavailableIndex(t *testing.T) {
	if _, err := (&Tools{}).KBSearch(context.Background(), "synthetic", 5, nil); !errors.Is(err, ErrKBUnavailable) {
		t.Fatalf("KBSearch() error = %v; want ErrKBUnavailable", err)
	}
	if !errors.Is(ErrKBUnavailable, kb.ErrIndexUnavailable) {
		t.Fatal("ErrKBUnavailable does not alias kb.ErrIndexUnavailable")
	}
	if _, err := (&Tools{}).searchKB(context.Background(), kbSearchArgs{Query: "synthetic"}); err == nil ||
		err.Error() != "local KB index is unavailable; configure BB_ASSISTANT_KB_INDEX" {
		t.Errorf("searchKB() error = %v; want the unchanged MCP unavailability message", err)
	}
}

func TestKBSearchExportedPathRejectsInvalidArguments(t *testing.T) {
	sources := make([]string, 0, maxKBSearchSources+1)
	for index := 0; index <= maxKBSearchSources; index++ {
		sources = append(sources, "family-"+strconv.Itoa(index))
	}
	cases := []struct {
		name    string
		query   string
		topK    int
		sources []string
	}{
		{name: "empty query", query: "  ", topK: 5},
		{name: "oversized query", query: strings.Repeat("a", maxKBQueryRunes+1), topK: 5},
		{name: "zero top_k", query: "synthetic", topK: 0},
		{name: "top_k above limit", query: "synthetic", topK: maxKBSearchTopK + 1},
		{name: "too many sources", query: "synthetic", topK: 5, sources: sources},
		{name: "empty source", query: "synthetic", topK: 5, sources: []string{""}},
		{name: "untrimmed source", query: "synthetic", topK: 5, sources: []string{" nvd"}},
		{name: "oversized source", query: "synthetic", topK: 5, sources: []string{strings.Repeat("a", maxKBSourceNameBytes+1)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := (&Tools{}).KBSearch(context.Background(), testCase.query, testCase.topK, testCase.sources); err == nil {
				t.Fatal("KBSearch() accepted invalid arguments")
			} else if errors.Is(err, ErrKBUnavailable) {
				t.Fatalf("KBSearch() reported unavailability for invalid arguments: %v", err)
			}
		})
	}
}
