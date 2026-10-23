package neo4jstore

import (
	"encoding/json"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
)

func testSnapshot(projectKey string) behaviorgraph.Record {
	endpointID := "endpoint-" + strings.Repeat("1", 64)
	assertionID := "assertion-" + strings.Repeat("2", 64)
	source := behaviorgraph.Record{
		"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange",
	}
	return behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"sources": []behaviorgraph.Record{source},
		"entities": []behaviorgraph.Record{{
			"id": endpointID, "kind": "endpoint", "data": map[string]any{"method": "GET"},
			"source_refs": []behaviorgraph.Record{source},
		}},
		"assertions": []behaviorgraph.Record{{
			"id": assertionID, "subject": endpointID, "predicate": "references", "object": endpointID,
			"source_refs": []behaviorgraph.Record{source},
		}},
		"cards": []behaviorgraph.Record{},
	}
}

func TestPrepareSnapshotKeepsProjectScopedAssertionAndEvidenceLinks(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	prepared, err := prepareSnapshot(projectKey, testSnapshot(projectKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Records) != 3 || len(prepared.Assertions) != 1 || len(prepared.Refs) != 2 {
		t.Fatalf("prepared snapshot counts = records:%d assertions:%d refs:%d", len(prepared.Records), len(prepared.Assertions), len(prepared.Refs))
	}
	if prepared.Assertions[0].SubjectID != prepared.Assertions[0].ObjectID {
		t.Fatal("assertion endpoints changed during preparation")
	}
	if prepared.Digest == "" {
		t.Fatal("snapshot digest is empty")
	}
}

func TestPrepareSnapshotPromotesAnalysisCardLookupFields(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	snapshot := testSnapshot(projectKey)
	endpointID := "endpoint-" + strings.Repeat("1", 64)
	snapshot["cards"] = []behaviorgraph.Record{{
		"id": "card-" + strings.Repeat("b", 64), "capture_id": "capture-239",
		"endpoint": map[string]any{"id": endpointID},
		"source_refs": []behaviorgraph.Record{{
			"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange",
		}},
	}}
	prepared, err := prepareSnapshot(projectKey, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var card preparedRecord
	for _, record := range prepared.Records {
		if record.Type == "card" {
			card = record
			break
		}
	}
	if card.CardEndpointID != endpointID || card.CardCaptureID != "capture-239" {
		t.Fatalf("promoted card lookup keys = endpoint %q, capture %q", card.CardEndpointID, card.CardCaptureID)
	}
	rows := recordRows([]preparedRecord{card})
	if len(rows) != 1 || rows[0]["card_endpoint_id"] != endpointID || rows[0]["card_capture_id"] != "capture-239" {
		t.Fatalf("record row omitted card lookup properties: %#v", rows)
	}
}

func TestPrepareOutboxBundleAllowsReferencesToEarlierBatches(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	snapshot := testSnapshot(projectKey)
	assertions := snapshot["assertions"].([]behaviorgraph.Record)
	assertions[0]["object"] = "endpoint-" + strings.Repeat("3", 64)
	prepared, err := prepareGraphSnapshot(projectKey, snapshot, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Assertions) != 1 || len(prepared.Refs) != 2 {
		t.Fatalf("outbox bundle relationships = assertions:%d refs:%d", len(prepared.Assertions), len(prepared.Refs))
	}
	if prepared.Assertions[0].ObjectID != "endpoint-"+strings.Repeat("3", 64) {
		t.Fatal("outbox assertion endpoint was not preserved for validation against earlier project batches")
	}
	for _, record := range prepared.Records {
		if strings.Contains(record.Payload, "source_refs") {
			t.Fatalf("source references should be represented as project-scoped relationships: %s", record.Type)
		}
	}
}

func TestPrepareSnapshotRejectsCrossProjectAndUnknownEndpoints(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	snapshot := testSnapshot(strings.Repeat("b", 64))
	if _, err := prepareSnapshot(projectKey, snapshot); err == nil {
		t.Fatal("accepted a snapshot with another project namespace")
	}
	snapshot = testSnapshot(projectKey)
	assertions := snapshot["assertions"].([]behaviorgraph.Record)
	assertions[0]["object"] = "endpoint-" + strings.Repeat("3", 64)
	if _, err := prepareSnapshot(projectKey, snapshot); err == nil {
		t.Fatal("accepted an assertion endpoint absent from the selected project")
	}
}

func TestPrepareSnapshotRejectsPrivateProperties(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	snapshot := testSnapshot(projectKey)
	entities := snapshot["entities"].([]behaviorgraph.Record)
	entities[0]["data"].(map[string]any)["cookie"] = "session=secret"
	if _, err := prepareSnapshot(projectKey, snapshot); err == nil {
		t.Fatal("accepted a graph snapshot with a private property")
	}
}

func TestNeo4jConfigRejectsEmbeddedCredentialsAndMissingPassword(t *testing.T) {
	cases := []Config{
		{URI: "bolt://user:password@127.0.0.1:7687", Username: "neo4j", Password: "secret", Database: "neo4j"},
		{URI: "bolt://127.0.0.1:7687", Username: "neo4j", Database: "neo4j"},
		{URI: "https://127.0.0.1:7687", Username: "neo4j", Password: "secret", Database: "neo4j"},
	}
	for _, config := range cases {
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted invalid Neo4j config: URI=%q", config.URI)
		}
	}
}

func TestAddSnapshotBytesEnforcesCumulativeBudget(t *testing.T) {
	current, err := addSnapshotBytes(maxSnapshotBytes-5, 5)
	if err != nil || current != maxSnapshotBytes {
		t.Fatalf("exact snapshot byte limit: current=%d err=%v", current, err)
	}
	if _, err := addSnapshotBytes(maxSnapshotBytes-5, 6); err == nil {
		t.Fatal("accepted provenance bytes beyond the cumulative snapshot limit")
	}
}
