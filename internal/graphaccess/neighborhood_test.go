package graphaccess

import (
	"reflect"
	"testing"

	"bb-graph-assistant/internal/neo4jstore"
)

func TestApplyNeighborhoodProvenanceCoverage(t *testing.T) {
	result := map[string]any{
		"incomplete":  true,
		"limitations": []string{"direct neighbors only"},
	}
	neighborhood := neo4jstore.Neighborhood{
		ProvenanceTruncated:  true,
		TruncatedEntityIDs:   []string{"endpoint-abc", "variant-def"},
		EntitySourceRefLimit: 64,
	}
	applyNeighborhoodProvenanceCoverage(result, neighborhood)
	if result["provenance_truncated"] != true || result["entity_source_ref_limit"] != 64 {
		t.Fatalf("provenance coverage metadata missing: %#v", result)
	}
	if got, ok := result["provenance_truncated_entity_ids"].([]string); !ok || !reflect.DeepEqual(got, neighborhood.TruncatedEntityIDs) {
		t.Fatalf("truncated entity IDs = %#v, want %#v", result["provenance_truncated_entity_ids"], neighborhood.TruncatedEntityIDs)
	}
	limitations, ok := result["limitations"].([]string)
	if !ok || len(limitations) != 2 || limitations[0] != "direct neighbors only" || limitations[1] == "" {
		t.Fatalf("sampling limitation was not made explicit: %#v", result["limitations"])
	}
}

func TestApplyNeighborhoodProvenanceCoverageWhenComplete(t *testing.T) {
	result := map[string]any{"limitations": []string{"direct neighbors only"}}
	applyNeighborhoodProvenanceCoverage(result, neo4jstore.Neighborhood{EntitySourceRefLimit: 64})
	if result["provenance_truncated"] != false || result["entity_source_ref_limit"] != 64 {
		t.Fatalf("complete provenance coverage metadata missing: %#v", result)
	}
	ids, ok := result["provenance_truncated_entity_ids"].([]string)
	if !ok || ids == nil || len(ids) != 0 {
		t.Fatalf("complete provenance must return an empty ID list, got %#v", result["provenance_truncated_entity_ids"])
	}
	if got := result["limitations"].([]string); len(got) != 1 {
		t.Fatalf("complete provenance added a false limitation: %#v", got)
	}
}
