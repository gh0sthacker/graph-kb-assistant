package neo4jstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
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

	interactive, err := store.ReadProjectNeighborhood(ctx, project, []string{seed}, 30)
	if err != nil {
		t.Fatalf("read interactive neighborhood with analysis cards: %v", err)
	}
	interactiveCards, ok := interactive.Snapshot["cards"].([]behaviorgraph.Record)
	if !ok || len(interactiveCards) != 2 || interactive.AnalysisCardCandidatesTruncated {
		t.Fatalf("interactive neighborhood omitted or truncated the bounded matching cards: %+v", interactive)
	}
	interactiveReader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, interactive.Snapshot, project, t.TempDir())
	if err != nil {
		t.Fatalf("open interactive snapshot with cards: %v", err)
	}
	defer interactiveReader.Close()
	interactiveSlice, err := interactiveReader.Slice(ctx, behaviorgraph.SliceOptions{SeedIDs: []string{seed}, MaxNodes: 30, MaxEdges: 30, MaxDepth: 1})
	if err != nil {
		t.Fatalf("slice interactive snapshot with cards: %v", err)
	}
	if got := len(interactiveSlice["cards"].([]behaviorgraph.Record)); got != 1 {
		t.Fatalf("interactive slice returned %d cards, want the selected endpoint's card", got)
	}
	interactiveCard := interactiveSlice["cards"].([]behaviorgraph.Record)[0]
	if interactiveCard["id"] != cardID {
		t.Fatalf("interactive slice attached an unrelated analysis card: %#v", interactiveCard)
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

func TestNeo4jAnalyticalSliceContinuesEntityProvenanceWithExclusions(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	owner := neighborhoodTestID("endpoint", "analytical-provenance-continuation")
	sources := make([]behaviorgraph.Record, maxNeighborhoodEntityRefs+2)
	for i := range sources {
		sources[i] = behaviorgraph.Record{
			"path": fmt.Sprintf("/synthetic/provenance/%03d.json", i), "sha256": strings.Repeat("a", 64),
			"record_locator": fmt.Sprintf("$.records[%d]", i),
		}
	}
	fixture := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"sources": sources,
		"entities": []behaviorgraph.Record{{
			"id": owner, "kind": "endpoint", "data": map[string]any{"method": "GET"}, "source_refs": sources,
		}},
		"assertions": []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{},
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	first, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{owner}, AnalyticalSliceOptions{MaxNodes: 10, MaxEdges: 10, MaxDepth: 0})
	if err != nil {
		t.Fatal(err)
	}
	firstEntity := first.Snapshot["entities"].([]behaviorgraph.Record)[0]
	firstRefs := firstEntity["source_refs"].([]behaviorgraph.Record)
	if len(firstRefs) != maxNeighborhoodEntityRefs || !first.ProvenanceTruncated || !slices.Contains(first.TruncatedEntityIDs, owner) {
		t.Fatalf("first provenance page was not marked resumable: refs=%d result=%+v", len(firstRefs), first)
	}
	firstIDs := neighborhoodSourceRecordIDs(firstRefs)
	excluded := make([]string, 0, len(firstIDs))
	for _, id := range firstIDs {
		excluded = append(excluded, owner+"|"+id)
	}
	anchor := owner + "|" + firstIDs[0]
	second, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{owner}, AnalyticalSliceOptions{
		MaxNodes: 10, MaxEdges: 10, MaxDepth: 0,
		ExcludedEntitySourceRefKeys: excluded, EntitySourceRefAnchorKeys: []string{anchor},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEntity := second.Snapshot["entities"].([]behaviorgraph.Record)[0]
	secondRefs := secondEntity["source_refs"].([]behaviorgraph.Record)
	secondIDs := neighborhoodSourceRecordIDs(secondRefs)
	if second.ProvenanceTruncated || len(secondIDs) != 3 || !slices.Contains(secondIDs, firstIDs[0]) {
		t.Fatalf("final provenance page did not include one anchor and the remaining sources: refs=%v result=%+v", secondIDs, second)
	}
	for _, id := range secondIDs {
		if id != firstIDs[0] && slices.Contains(firstIDs, id) {
			t.Fatalf("continued provenance repeated a prior source %q", id)
		}
	}
}

func TestAnalyticalCandidatePageTruncatesOnlyWhenAnotherCandidateExists(t *testing.T) {
	ids := make([]string, maxAnalyticalCandidateAssertions)
	for i := range ids {
		ids[i] = analyticalFixtureID("assertion", fmt.Sprintf("candidate-%d", i))
	}
	page, omitted, truncated, err := analyticalCandidatePage(ids, maxAnalyticalCandidateAssertions)
	if err != nil || truncated || len(page) != maxAnalyticalCandidateAssertions || len(omitted) != 0 {
		t.Fatalf("exact candidate limit was reported as truncated: len=%d omitted=%d truncated=%t err=%v", len(page), len(omitted), truncated, err)
	}
	page, omitted, truncated, err = analyticalCandidatePage(nil, 0)
	if err != nil || truncated || len(page) != 0 || len(omitted) != 0 {
		t.Fatalf("empty probe at exhausted budget was reported as truncated: len=%d omitted=%d truncated=%t err=%v", len(page), len(omitted), truncated, err)
	}
	page, omitted, truncated, err = analyticalCandidatePage([]string{"assertion-extra"}, 0)
	if err != nil || !truncated || len(page) != 0 || len(omitted) != 1 || omitted[0] != "assertion-extra" {
		t.Fatalf("extra candidate at exhausted budget was not returned for continuation: len=%d omitted=%v truncated=%t err=%v", len(page), omitted, truncated, err)
	}
	page, omitted, truncated, err = analyticalCandidatePage(ids, maxAnalyticalCandidateAssertions-1)
	if err != nil || !truncated || len(page) != maxAnalyticalCandidateAssertions-1 || len(omitted) != 1 || omitted[0] != ids[maxAnalyticalCandidateAssertions-1] {
		t.Fatalf("over-budget candidate page has wrong result: len=%d omitted=%v truncated=%t err=%v", len(page), omitted, truncated, err)
	}
}

func TestAnalyticalAssertionShareDistributesRemainingBudget(t *testing.T) {
	for _, test := range []struct {
		name          string
		remaining     int
		frontierCount int
		want          int
		wantError     bool
	}{
		{name: "single frontier can use remaining global budget", remaining: 1_000, frontierCount: 1, want: 1_000},
		{name: "even share", remaining: 1_000, frontierCount: 2, want: 500},
		{name: "bounded frontier share rounds up", remaining: 1_000, frontierCount: 128, want: 8},
		{name: "one candidate reaches every frontier", remaining: 1, frontierCount: 128, want: 1},
		{name: "exhausted budget", remaining: 0, frontierCount: 3, want: 0},
		{name: "empty frontier", remaining: 1_000, frontierCount: 0, want: 0},
		{name: "negative budget rejected", remaining: -1, frontierCount: 1, wantError: true},
		{name: "negative frontier rejected", remaining: 1, frontierCount: -1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := analyticalAssertionShare(test.remaining, test.frontierCount)
			if (err != nil) != test.wantError || (!test.wantError && got != test.want) {
				t.Fatalf("analyticalAssertionShare(%d, %d) = %d, %v; want %d, error=%t", test.remaining, test.frontierCount, got, err, test.want, test.wantError)
			}
		})
	}
}

func TestAnalyticalExcludedAssertionIDsAreBoundedAndValidated(t *testing.T) {
	validA := analyticalFixtureID("assertion", "excluded-a")
	validB := analyticalFixtureID("assertion", "excluded-b")
	got, err := analyticalExcludedAssertionIDs([]string{validB, validA})
	if err != nil || len(got) != 2 || got[0] >= got[1] {
		t.Fatalf("valid exclusion IDs were not normalized: %v, %v", got, err)
	}
	for _, invalid := range [][]string{
		{validA, validA},
		{"endpoint-" + strings.Repeat("a", 64)},
		{"assertion-invalid"},
		make([]string, maxAnalyticalExcludedAssertions+1),
	} {
		if _, err := analyticalExcludedAssertionIDs(invalid); err == nil {
			t.Errorf("invalid exclusion list of length %d was accepted", len(invalid))
		}
	}
}

func TestAnalyticalFrontierPageReportsOmittedNodes(t *testing.T) {
	ids := []string{"entity-a", "entity-b", "entity-c"}
	page, omitted := analyticalFrontierPage(ids, 2)
	if len(page) != 2 || page[0] != "entity-a" || page[1] != "entity-b" || len(omitted) != 1 || omitted[0] != "entity-c" {
		t.Fatalf("frontier cap did not identify omitted nodes: page=%v omitted=%v", page, omitted)
	}
	page, omitted = analyticalFrontierPage(ids, len(ids))
	if len(page) != len(ids) || len(omitted) != 0 {
		t.Fatalf("exact frontier limit was reported as truncated: page=%v omitted=%v", page, omitted)
	}
}

func TestNeo4jAnalyticalSliceUsesGlobalCandidateBudget(t *testing.T) {
	if os.Getenv("BB_ASSISTANT_NEO4J_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_TEST=1 to run against a local Neo4j instance")
	}
	store, ctx, project := neighborhoodIntegrationStore(t)
	source := behaviorgraph.Record{
		"path": "/synthetic/analytical-limit.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.capture",
	}
	seed := neighborhoodTestID("asset", "analytical-limit-seed")
	entities := []behaviorgraph.Record{{"id": seed, "kind": "asset", "data": map[string]any{"label": "seed"}, "source_refs": []behaviorgraph.Record{source}}}
	assertions := make([]behaviorgraph.Record, 0, maxAnalyticalCandidateAssertions+2)
	for i := 0; i <= maxAnalyticalCandidateAssertions+1; i++ {
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
	if !result.CandidateAssertionsTruncated || result.CandidateAssertions != maxAnalyticalCandidateAssertions {
		t.Fatalf("candidate limit was not surfaced: %#v", result)
	}
	if len(result.CandidateAssertionIDsOmitted) != 2 || result.AssertionSeedsTruncated {
		t.Fatalf("over-budget assertion IDs were not made resumable: %#v", result)
	}
	excluded := make([]string, 0, maxAnalyticalCandidateAssertions)
	for _, record := range result.Snapshot["assertions"].([]behaviorgraph.Record) {
		excluded = append(excluded, record["id"].(string))
	}
	continued, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{result.CandidateAssertionIDsOmitted[0]}, AnalyticalSliceOptions{
		MaxNodes: 30, MaxEdges: 30, MaxDepth: 2, ExcludedAssertionIDs: excluded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if continued.CandidateAssertionsTruncated || !analyticalSnapshotContainsAssertion(continued.Snapshot, result.CandidateAssertionIDsOmitted[1]) {
		t.Fatalf("continuation repeated the loaded assertion page instead of advancing: first=%v next=%#v", result.CandidateAssertionIDsOmitted, continued)
	}
}

func TestNeo4jAnalyticalSliceContinuesCardCandidatesWithExclusions(t *testing.T) {
	if os.Getenv("BB_ASSISTANT_NEO4J_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_TEST=1 to run against a local Neo4j instance")
	}
	store, ctx, project := neighborhoodIntegrationStore(t)
	source := behaviorgraph.Record{
		"path": "/synthetic/analytical-card-page.json", "sha256": strings.Repeat("e", 64), "record_locator": "$.capture",
	}
	seed := neighborhoodTestID("endpoint", "analytical-card-page-seed")
	entity := behaviorgraph.Record{
		"id": seed, "kind": "endpoint", "data": map[string]any{"method": "GET"},
		"source_refs": []behaviorgraph.Record{source},
	}
	cards := make([]behaviorgraph.Record, 0, maxAnalyticalCandidateCards+2)
	for i := 0; i < maxAnalyticalCandidateCards+2; i++ {
		cards = append(cards, behaviorgraph.Record{
			"schema_version": json.Number("1"), "id": neighborhoodTestID("card", fmt.Sprintf("analytical-card-page-%d", i)),
			"capture_id": fmt.Sprintf("capture-analytical-card-page-%d", i), "program": project,
			"source_refs": []behaviorgraph.Record{source}, "endpoint": map[string]any{"id": seed},
			"context":           map[string]any{"program": project},
			"request_analysis":  map[string]any{"parse_state": "incomplete"},
			"response_analysis": map[string]any{"parse_state": "incomplete", "status": nil},
			"business_action":   map[string]any{"known_effect": "unknown"},
			"hypotheses":        []any{}, "missing_evidence": []any{}, "analysis_state": "incomplete",
		})
	}
	fixture := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"sources": []behaviorgraph.Record{source}, "entities": []behaviorgraph.Record{entity},
		"assertions": []behaviorgraph.Record{}, "cards": cards,
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	first, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{seed}, AnalyticalSliceOptions{
		MaxNodes: 10, MaxEdges: 10, MaxDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.CandidateCardsTruncated || len(first.CandidateCardOwnerIDsOmitted) != 1 || first.CandidateCardOwnerIDsOmitted[0] != seed {
		t.Fatalf("card candidate limit did not return its owner branch: %#v", first)
	}
	firstCards := first.Snapshot["cards"].([]behaviorgraph.Record)
	if len(firstCards) != maxAnalyticalCandidateCards {
		t.Fatalf("first card page has %d cards, want %d", len(firstCards), maxAnalyticalCandidateCards)
	}
	excluded := make([]string, 0, len(firstCards))
	for _, card := range firstCards {
		excluded = append(excluded, card["id"].(string))
	}
	second, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{seed}, AnalyticalSliceOptions{
		MaxNodes: 10, MaxEdges: 10, MaxDepth: 1, ExcludedCardIDs: excluded,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondCards := second.Snapshot["cards"].([]behaviorgraph.Record)
	if second.CandidateCardsTruncated || len(secondCards) != 2 {
		t.Fatalf("card continuation did not advance to the last two candidates: %#v", second)
	}
	for _, card := range secondCards {
		if slices.Contains(excluded, card["id"].(string)) {
			t.Fatalf("card continuation repeated an already loaded card %q", card["id"])
		}
	}
}

func analyticalSnapshotContainsAssertion(snapshot behaviorgraph.Record, id string) bool {
	assertions, _ := snapshot["assertions"].([]behaviorgraph.Record)
	for _, assertion := range assertions {
		if assertion["id"] == id {
			return true
		}
	}
	return false
}

func TestNeo4jAnalyticalSliceReturnsOmittedFrontierSeeds(t *testing.T) {
	if os.Getenv("BB_ASSISTANT_NEO4J_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_TEST=1 to run against a local Neo4j instance")
	}
	store, ctx, project := neighborhoodIntegrationStore(t)
	source := behaviorgraph.Record{
		"path": "/synthetic/analytical-frontier.json", "sha256": strings.Repeat("d", 64), "record_locator": "$.capture",
	}
	seed := neighborhoodTestID("asset", "analytical-frontier-seed")
	entities := []behaviorgraph.Record{{"id": seed, "kind": "asset", "data": map[string]any{"label": "seed"}, "source_refs": []behaviorgraph.Record{source}}}
	assertions := make([]behaviorgraph.Record, 0, maxAnalyticalFrontierNodes+20)
	for i := 0; i < maxAnalyticalFrontierNodes+20; i++ {
		endpoint := neighborhoodTestID("asset", fmt.Sprintf("analytical-frontier-endpoint-%d", i))
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
	result, err := store.ReadProjectAnalyticalSlice(ctx, project, []string{seed}, AnalyticalSliceOptions{MaxNodes: 30, MaxEdges: 30, MaxDepth: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !result.FrontierNodeLimitReached || result.FrontierNodesOmitted < 1 ||
		len(result.FrontierNodeIDsOmitted) != result.FrontierNodesOmitted || result.FrontierSeedsTruncated {
		t.Fatalf("native frontier omissions were not resumable: %#v", result)
	}
	knownIDs := make(map[string]bool, len(entities)+len(assertions))
	for _, entity := range entities {
		knownIDs[entity["id"].(string)] = true
	}
	for _, assertion := range assertions {
		knownIDs[assertion["id"].(string)] = true
	}
	for _, id := range result.FrontierNodeIDsOmitted {
		if !knownIDs[id] {
			t.Fatalf("frontier continuation exposed unknown candidate %q", id)
		}
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

func TestAnalyticalExcludedCardIDsAreBounded(t *testing.T) {
	valid := analyticalFixtureID("card", "valid")
	if got, err := analyticalExcludedCardIDs([]string{valid}); err != nil || len(got) != 1 || got[0] != valid {
		t.Fatalf("valid card exclusions = %v, %v", got, err)
	}
	for _, ids := range [][]string{
		{analyticalFixtureID("assertion", "wrong-kind")},
		{valid, valid},
		make([]string, maxAnalyticalExcludedCards+1),
	} {
		if _, err := analyticalExcludedCardIDs(ids); err == nil {
			t.Fatalf("accepted invalid card exclusions: count=%d first=%q", len(ids), firstString(ids))
		}
	}
}

func TestAnalyticalCardContinuationOwnersAreBoundedAndSorted(t *testing.T) {
	owners := make([]string, maxAnalyticalCardCursorSeeds+1)
	for i := range owners {
		owners[i] = analyticalFixtureID("endpoint", fmt.Sprintf("owner-%05d", len(owners)-i))
	}
	page, truncated := analyticalCardContinuationOwners(owners)
	if !truncated || len(page) != maxAnalyticalCardCursorSeeds || !sort.StringsAreSorted(page) {
		t.Fatalf("card continuation owners have len=%d truncated=%t sorted=%t", len(page), truncated, sort.StringsAreSorted(page))
	}
}

func TestAnalyticalEntitySourceRefContinuationKeysAreBoundedAndValidated(t *testing.T) {
	owner := analyticalFixtureID("endpoint", "source-ref-owner")
	firstSource := analyticalFixtureID("source", "source-ref-first")
	secondSource := analyticalFixtureID("source", "source-ref-second")
	valid := owner + "|" + firstSource
	if got, err := validateAnalyticalEntitySourceRefKeys([]string{valid}, maxAnalyticalExcludedEntitySourceRefs, false); err != nil || len(got) != 1 || got[0] != valid {
		t.Fatalf("valid excluded refs = %v, err %v", got, err)
	}
	if got, err := validateAnalyticalEntitySourceRefKeys([]string{valid}, maxAnalyticalEntitySourceRefAnchors, true); err != nil || len(got) != 1 || got[0] != valid {
		t.Fatalf("valid anchor refs = %v, err %v", got, err)
	}
	if err := validateAnalyticalEntitySourceRefAnchors([]string{valid}, []string{valid}); err != nil {
		t.Fatalf("accepted an anchor retained in the exclusion set: %v", err)
	}
	if err := validateAnalyticalEntitySourceRefAnchors(nil, []string{valid}); err == nil {
		t.Fatal("accepted an anchor that could be selected twice")
	}
	for _, keys := range [][]string{
		{"malformed"},
		{"card-" + strings.Repeat("a", 64) + "|" + firstSource},
		{owner + "|endpoint-" + strings.Repeat("b", 64)},
		{valid, valid},
		make([]string, maxAnalyticalExcludedEntitySourceRefs+1),
	} {
		if _, err := validateAnalyticalEntitySourceRefKeys(keys, maxAnalyticalExcludedEntitySourceRefs, false); err == nil {
			t.Fatalf("accepted invalid excluded source refs: count=%d", len(keys))
		}
	}
	anchors := []string{owner + "|" + firstSource, owner + "|" + secondSource}
	if _, err := validateAnalyticalEntitySourceRefKeys(anchors, maxAnalyticalEntitySourceRefAnchors, true); err == nil {
		t.Fatal("accepted more than one anchor for the same entity")
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
