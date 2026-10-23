package neo4jstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
)

func TestNeo4jAnalyticalSliceReadsBoundedClosureWithoutProjectSnapshot(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	source := behaviorgraph.Record{
		"path": "/synthetic/analytical-slice.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.capture",
	}
	cardSource := behaviorgraph.Record{
		"path": "/synthetic/analytical-card.json", "sha256": strings.Repeat("c", 64), "record_locator": "$.card",
	}
	seed := neighborhoodTestID("endpoint", "analytical-seed")
	cardEndpoint := neighborhoodTestID("endpoint", "analytical-card-endpoint")
	near := neighborhoodTestID("asset", "analytical-near")
	far := neighborhoodTestID("asset", "analytical-far")
	entities := []behaviorgraph.Record{
		{"id": seed, "kind": "endpoint", "data": map[string]any{"method": "GET"}, "source_refs": []behaviorgraph.Record{source}},
		{"id": cardEndpoint, "kind": "endpoint", "data": map[string]any{"method": "POST"}, "source_refs": []behaviorgraph.Record{source}},
		{"id": near, "kind": "asset", "data": map[string]any{"label": "near"}, "source_refs": []behaviorgraph.Record{source}},
		{"id": far, "kind": "asset", "data": map[string]any{"label": "far"}, "source_refs": []behaviorgraph.Record{source}},
	}
	for i := 0; i < 350; i++ {
		id := neighborhoodTestID("asset", fmt.Sprintf("unrelated-%d", i))
		entities = append(entities, behaviorgraph.Record{
			"id": id, "kind": "asset", "data": map[string]any{"label": fmt.Sprintf("unrelated-%d", i)}, "source_refs": []behaviorgraph.Record{source},
		})
	}
	first := neighborhoodFixtureAssertion(project, seed, near, "references", source)
	second := neighborhoodFixtureAssertion(project, near, far, "contains", source)
	contradiction := neighborhoodFixtureAssertion(project, first["id"].(string), second["id"].(string), "contradicts", source)
	cardID := neighborhoodTestID("card", "analytical-card")
	card := behaviorgraph.Record{
		"schema_version": json.Number("1"), "id": cardID, "capture_id": "capture-analytical",
		"program": project, "source_refs": []behaviorgraph.Record{cardSource},
		"endpoint": map[string]any{"id": seed}, "context": map[string]any{"program": project},
		"request_analysis":  map[string]any{"parse_state": "incomplete"},
		"response_analysis": map[string]any{"parse_state": "incomplete", "status": nil},
		"business_action":   map[string]any{"known_effect": "unknown"},
		"hypotheses":        []any{}, "missing_evidence": []any{}, "analysis_state": "incomplete",
	}
	indirectCardID := neighborhoodTestID("card", "analytical-card-endpoint-dependency")
	indirectCard := behaviorgraph.Record{
		"schema_version": json.Number("1"), "id": indirectCardID, "capture_id": "capture-analytical-unrelated",
		"program": project, "source_refs": []behaviorgraph.Record{source},
		"endpoint": map[string]any{"id": cardEndpoint}, "context": map[string]any{"program": project},
		"request_analysis":  map[string]any{"parse_state": "incomplete"},
		"response_analysis": map[string]any{"parse_state": "incomplete", "status": nil},
		"business_action":   map[string]any{"known_effect": "unknown"},
		"hypotheses":        []any{}, "missing_evidence": []any{}, "analysis_state": "incomplete",
	}
	fixture := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"sources": []behaviorgraph.Record{source, cardSource}, "entities": entities,
		"assertions": []behaviorgraph.Record{first, second, contradiction}, "cards": []behaviorgraph.Record{card, indirectCard},
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}

	result, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{seed}, AnalyticalSliceOptions{MaxNodes: 30, MaxEdges: 30, MaxDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation < 1 || result.Snapshot["program"] != project {
		t.Fatalf("analytical read lost selected project generation: %#v", result)
	}
	loadedEntities := result.Snapshot["entities"].([]behaviorgraph.Record)
	if len(loadedEntities) >= len(entities) {
		t.Fatalf("bounded analytical read loaded the full project entity set: %d of %d", len(loadedEntities), len(entities))
	}
	assertions := make(map[string]behaviorgraph.Record)
	for _, assertion := range result.Snapshot["assertions"].([]behaviorgraph.Record) {
		assertions[assertion["id"].(string)] = assertion
	}
	if len(assertions) != 3 || assertions[contradiction["id"].(string)] == nil {
		t.Fatalf("bounded read omitted the linked contradiction closure: %#v", assertions)
	}
	if cards := result.Snapshot["cards"].([]behaviorgraph.Record); len(cards) != 2 {
		t.Fatalf("bounded read did not retain both selected evidence cards: %#v", cards)
	}
	loadedCardEndpoint := false
	for _, entity := range loadedEntities {
		if entity["id"] == cardEndpoint {
			loadedCardEndpoint = true
		}
	}
	if !loadedCardEndpoint {
		t.Fatalf("bounded read omitted an analysis card endpoint dependency %q", cardEndpoint)
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, result.Snapshot, project, t.TempDir())
	if err != nil {
		t.Fatalf("bounded analytical snapshot failed shared graph validation: %v", err)
	}
	defer reader.Close()
	slice, err := reader.Slice(ctx, behaviorgraph.SliceOptions{SeedIDs: []string{seed}, MaxNodes: 30, MaxEdges: 30, MaxDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(slice["cards"].([]behaviorgraph.Record)); got != 1 {
		t.Fatalf("analysis slice returned %d cards, want 1 card attached to the selected endpoint", got)
	}
	conflictingEvidence := slice["conflicting_evidence"].([]string)
	contradictionID := contradiction["id"].(string)
	foundContradiction := false
	for _, assertionID := range conflictingEvidence {
		if assertionID == contradictionID {
			foundContradiction = true
			break
		}
	}
	if !foundContradiction {
		t.Fatalf("analysis slice omitted linked contradiction %q from conflicting evidence: %v", contradictionID, conflictingEvidence)
	}
}

func TestAnalyticalCandidatePageTruncatesOnlyWhenAnotherCandidateExists(t *testing.T) {
	ids := make([]string, maxAnalyticalCandidateAssertions)
	for i := range ids {
		ids[i] = analyticalFixtureID("assertion", fmt.Sprintf("candidate-%d", i))
	}
	page, truncated, err := analyticalCandidatePage(ids, maxAnalyticalCandidateAssertions)
	if err != nil || truncated || len(page) != maxAnalyticalCandidateAssertions {
		t.Fatalf("exact candidate limit was reported as truncated: len=%d truncated=%t err=%v", len(page), truncated, err)
	}
	page, truncated, err = analyticalCandidatePage(nil, 0)
	if err != nil || truncated || len(page) != 0 {
		t.Fatalf("empty probe at exhausted budget was reported as truncated: len=%d truncated=%t err=%v", len(page), truncated, err)
	}
	page, truncated, err = analyticalCandidatePage([]string{"assertion-extra"}, 0)
	if err != nil || !truncated || len(page) != 0 {
		t.Fatalf("extra candidate at exhausted budget was not reported as truncated: len=%d truncated=%t err=%v", len(page), truncated, err)
	}
	page, truncated, err = analyticalCandidatePage(ids, maxAnalyticalCandidateAssertions-1)
	if err != nil || !truncated || len(page) != maxAnalyticalCandidateAssertions-1 {
		t.Fatalf("over-budget candidate page has wrong result: len=%d truncated=%t err=%v", len(page), truncated, err)
	}
}

func TestAnalyticalFrontierPageReportsOmittedNodes(t *testing.T) {
	ids := []string{"entity-a", "entity-b", "entity-c"}
	page, omitted := analyticalFrontierPage(ids, 2)
	if len(page) != 2 || page[0] != "entity-a" || page[1] != "entity-b" || omitted != 1 {
		t.Fatalf("frontier cap did not identify omitted nodes: page=%v omitted=%d", page, omitted)
	}
	page, omitted = analyticalFrontierPage(ids, len(ids))
	if len(page) != len(ids) || omitted != 0 {
		t.Fatalf("exact frontier limit was reported as truncated: page=%v omitted=%d", page, omitted)
	}
}

func TestNeo4jAnalyticalSliceMarksPerNodeCandidateTruncation(t *testing.T) {
	if os.Getenv("BB_ASSISTANT_NEO4J_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_TEST=1 to run against a local Neo4j instance")
	}
	store, ctx, project := neighborhoodIntegrationStore(t)
	source := behaviorgraph.Record{
		"path": "/synthetic/analytical-limit.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.capture",
	}
	seed := neighborhoodTestID("asset", "analytical-limit-seed")
	entities := []behaviorgraph.Record{{"id": seed, "kind": "asset", "data": map[string]any{"label": "seed"}, "source_refs": []behaviorgraph.Record{source}}}
	assertions := make([]behaviorgraph.Record, 0, maxAnalyticalCandidateAssertions+1)
	for i := 0; i <= maxAnalyticalCandidateAssertions; i++ {
		endpoint := neighborhoodTestID("asset", fmt.Sprintf("analytical-limit-endpoint-%d", i))
		entities = append(entities, behaviorgraph.Record{
			"id": endpoint, "kind": "asset", "data": map[string]any{"label": fmt.Sprintf("endpoint-%d", i)}, "source_refs": []behaviorgraph.Record{source},
		})
		assertions = append(assertions, neighborhoodFixtureAssertion(project, seed, endpoint, "references", source))
	}
	fixture := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"sources": []behaviorgraph.Record{source}, "entities": entities, "assertions": assertions, "cards": []behaviorgraph.Record{},
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{seed}, AnalyticalSliceOptions{MaxNodes: 30, MaxEdges: 30, MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CandidateAssertionsTruncated || result.CandidateAssertions != maxAnalyticalAssertionsPerNode {
		t.Fatalf("candidate limit was not surfaced: %#v", result)
	}
}

func analyticalFixtureID(kind, name string) string {
	digest := sha256.Sum256([]byte(name))
	return kind + "-" + hex.EncodeToString(digest[:])
}

func TestAnalyticalSliceOptionsAreBounded(t *testing.T) {
	project := strings.Repeat("a", 64)
	seed := analyticalFixtureID("endpoint", "seed")
	for _, options := range []AnalyticalSliceOptions{
		{}, {MaxNodes: 0, MaxEdges: 1, MaxDepth: 1},
		{MaxNodes: 101, MaxEdges: 1, MaxDepth: 1},
		{MaxNodes: 10, MaxEdges: 301, MaxDepth: 1},
		{MaxNodes: 10, MaxEdges: 1, MaxDepth: 7},
	} {
		if _, err := analyticalSliceSeeds(project, []string{seed}, options); err == nil {
			t.Fatalf("accepted invalid analytical slice options: %#v", options)
		}
	}
	if _, err := analyticalSliceSeeds(project, []string{seed}, AnalyticalSliceOptions{MaxNodes: 10, MaxEdges: 10, MaxDepth: 2}); err != nil {
		t.Fatalf("rejected bounded analytical slice: %v", err)
	}
}
