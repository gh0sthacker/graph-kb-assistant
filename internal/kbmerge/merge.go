// Package kbmerge fuses the local knowledge-base ranking with the optional
// HackerOne supplemental index.
//
// It mirrors the merge used by the MCP facade's KB search tool so the
// standalone read-only search service returns identical hits without importing
// the MCP server, which depends on project storage and Neo4j.
package kbmerge

import (
	"sort"

	"bb-graph-assistant/internal/hackerone"
	"bb-graph-assistant/internal/kb"
)

const (
	// SourceName is the pseudo source family that selects the supplemental
	// HackerOne index. It is stripped before source names reach the KB index.
	SourceName = "hackerone"

	// Attribution is attached to every HackerOne hit so downstream consumers
	// keep treating the excerpt as reference material, not as project evidence.
	Attribution = "HackerOne disclosed report; local reference only"

	rrfRankOffset = 60.0
)

// SplitSources separates the HackerOne pseudo-source from real KB source
// families. An empty filter selects every source, including HackerOne.
func SplitSources(sources []string) (bool, []string) {
	if len(sources) == 0 {
		return true, nil
	}
	includeHackerOne := false
	families := make([]string, 0, len(sources))
	for _, name := range sources {
		if name == SourceName {
			includeHackerOne = true
			continue
		}
		families = append(families, name)
	}
	return includeHackerOne, families
}

// CandidateLimit bounds the HackerOne candidate pool fetched for fusion. A
// wider pool keeps supplemental hits from being truncated before RRF can rank
// them against the primary results.
func CandidateLimit(topK int) int {
	if doubled := 2 * topK; doubled > topK {
		return doubled
	}
	return topK
}

func reciprocalRank(rank int) float64 {
	return 1.0 / (rrfRankOffset + float64(rank))
}

// MapHit projects a supplemental hit into the shared KB citation shape.
// HackerOne reports have no per-chunk hash, so content hashes stay empty
// rather than being fabricated.
func MapHit(hit hackerone.Hit, meta hackerone.Meta) kb.Hit {
	locator := "reports/" + hit.ReportID + ".md"
	url := hit.URL
	revision := meta.DatasetCommit
	attribution := Attribution
	var bounty any
	if hit.Bounty != nil {
		bounty = *hit.Bounty
	}
	var upvotes any
	if hit.Upvotes != nil {
		upvotes = *hit.Upvotes
	}
	return kb.Hit{
		ChunkID:        SourceName + ":" + hit.ReportID,
		SourceName:     SourceName,
		SourceID:       hit.ReportID,
		Title:          hit.Title,
		Excerpt:        hit.Excerpt,
		SourceLocator:  &locator,
		SourceURL:      &url,
		SourceRevision: &revision,
		Attribution:    &attribution,
		Metadata: map[string]any{
			"program":      hit.Program,
			"vuln_type":    hit.VulnType,
			"severity":     hit.Severity,
			"weakness":     hit.Weakness,
			"bounty":       bounty,
			"upvotes":      upvotes,
			"disclosed_at": hit.DisclosedAt,
			"has_body":     hit.HasBody,
		},
		BM25: hit.BM25,
	}
}

// HackerOneOnly builds the synthetic result used when a caller asks for the
// supplemental index alone. hits must already be ordered best-first. The
// HackerOne index is complete for its own dataset, so coverage is DONE; the
// caller is searching that dataset alone rather than a partial KB snapshot.
func HackerOneOnly(hits []hackerone.Hit, meta hackerone.Meta) kb.SearchResult {
	mapped := make([]kb.Hit, 0, len(hits))
	for rank, hit := range hits {
		mappedHit := MapHit(hit, meta)
		mappedHit.RankScore = reciprocalRank(rank + 1)
		mapped = append(mapped, mappedHit)
	}
	return kb.SearchResult{
		SnapshotID:           meta.SnapshotID,
		CoverageState:        "DONE",
		RetrievalMode:        "hackerone_fts",
		LexicalEngine:        "sqlite-fts5",
		SupplementalIncluded: true,
		Hits:                 mapped,
	}
}

type fusedHit struct {
	hit   kb.Hit
	score float64
}

// Merge fuses the KB and HackerOne rankings using reciprocal rank fusion.
// Both lists are already ordered best-first, so the rank alone is enough; a
// hit that appears in both lists (by chunk identity) accumulates both
// contributions.
func Merge(result kb.SearchResult, hackeroneHits []hackerone.Hit, meta hackerone.Meta, topK int) kb.SearchResult {
	entries := make([]fusedHit, 0, len(result.Hits)+len(hackeroneHits))
	positions := make(map[string]int, cap(entries))
	add := func(hit kb.Hit, rank int) {
		score := reciprocalRank(rank)
		if position, ok := positions[hit.ChunkID]; ok {
			entries[position].score += score
			return
		}
		positions[hit.ChunkID] = len(entries)
		entries = append(entries, fusedHit{hit: hit, score: score})
	}
	for rank, hit := range result.Hits {
		add(hit, rank+1)
	}
	for rank, hit := range hackeroneHits {
		add(MapHit(hit, meta), rank+1)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return entries[left].score > entries[right].score
	})
	if topK >= 0 && len(entries) > topK {
		entries = entries[:topK]
	}
	hits := make([]kb.Hit, 0, len(entries))
	supplemental := false
	for _, entry := range entries {
		entry.hit.RankScore = entry.score
		if entry.hit.SourceName == SourceName {
			supplemental = true
		}
		hits = append(hits, entry.hit)
	}
	result.Hits = hits
	result.SupplementalIncluded = result.SupplementalIncluded || supplemental
	return result
}
