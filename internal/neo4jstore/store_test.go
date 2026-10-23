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

func TestPrepareSnapshotPromotesVariantSignaturesForIndexedLookup(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	requestSignature := strings.Repeat("b", 64)
	responseSignature := strings.Repeat("c", 64)
	snapshot := testSnapshot(projectKey)
	entities := snapshot["entities"].([]behaviorgraph.Record)
	entities = append(entities,
		behaviorgraph.Record{
			"id": "observation-" + strings.Repeat("d", 64), "kind": "observation",
			"data":        map[string]any{},
			"source_refs": []behaviorgraph.Record{{"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange"}},
		},
		behaviorgraph.Record{
			"id": "request_variant-" + strings.Repeat("e", 64), "kind": "request_variant",
			"data":        behaviorgraph.Record{"request_signature": requestSignature},
			"source_refs": []behaviorgraph.Record{{"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange"}},
		},
		behaviorgraph.Record{
			"id": "response_variant-" + strings.Repeat("f", 64), "kind": "response_variant",
			"data":        behaviorgraph.Record{"response_signature": responseSignature},
			"source_refs": []behaviorgraph.Record{{"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange"}},
		},
	)
	snapshot["entities"] = entities
	assertions := snapshot["assertions"].([]behaviorgraph.Record)
	assertions = append(assertions,
		behaviorgraph.Record{
			"id": "assertion-" + strings.Repeat("1", 64), "subject": "observation-" + strings.Repeat("d", 64),
			"predicate": "uses_request_variant", "object": "request_variant-" + strings.Repeat("e", 64),
			"source_refs": []behaviorgraph.Record{{"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange"}},
		},
		behaviorgraph.Record{
			"id": "assertion-" + strings.Repeat("3", 64), "subject": "observation-" + strings.Repeat("d", 64),
			"predicate": "received_response", "object": "response_variant-" + strings.Repeat("f", 64),
			"source_refs": []behaviorgraph.Record{{"path": "/private/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.exchange"}},
		},
	)
	snapshot["assertions"] = assertions
	prepared, err := prepareSnapshot(projectKey, snapshot)
	if err != nil {
		t.Fatalf("prepare signature fixture: %v", err)
	}
	var request, response preparedRecord
	for _, record := range prepared.Records {
		switch record.Kind {
		case "request_variant":
			request = record
		case "response_variant":
			response = record
		}
	}
	if request.RequestSignature != requestSignature || request.ResponseSignature != "" {
		t.Fatalf("request variant index fields = %#v", request)
	}
	if response.ResponseSignature != responseSignature || response.RequestSignature != "" {
		t.Fatalf("response variant index fields = %#v", response)
	}
	rows := recordRows([]preparedRecord{request, response})
	if rows[0]["request_signature"] != requestSignature || rows[1]["response_signature"] != responseSignature {
		t.Fatalf("record rows omitted signature lookup properties: %#v", rows)
	}
}

func TestObservationIDsByVariantSignaturesQueryUsesScopedIndexedProperties(t *testing.T) {
	requestOnly := observationIDsByVariantSignaturesQuery(true, false)
	responseOnly := observationIDsByVariantSignaturesQuery(false, true)
	both := observationIDsByVariantSignaturesQuery(true, true)
	for name, query := range map[string]string{"request": requestOnly, "response": responseOnly, "both": both} {
		if !strings.Contains(query, "$project_key") || !strings.Contains(query, "LIMIT $limit_plus_one") || strings.Contains(query, "payload_json") {
			t.Errorf("%s signature query is not project scoped, bounded, or payload-free: %s", name, query)
		}
	}
	if !strings.Contains(requestOnly, "request_signature: $request_signature") || strings.Contains(requestOnly, "response_signature") {
		t.Fatalf("request-only signature query has the wrong index key: %s", requestOnly)
	}
	if !strings.Contains(responseOnly, "response_signature: $response_signature") || strings.Contains(responseOnly, "request_signature") {
		t.Fatalf("response-only signature query has the wrong index key: %s", responseOnly)
	}
	if strings.Count(both, "->(observation:GraphRecord") != 2 {
		t.Fatalf("dual-signature query does not intersect on the same observation: %s", both)
	}
}

func TestSignatureIndexMarkerMustMatchCurrentGraphGeneration(t *testing.T) {
	tests := []struct {
		name        string
		version     any
		generation  any
		indexedGen  any
		wantCurrent bool
	}{
		{name: "current index", version: int64(1), generation: int64(7), indexedGen: int64(7), wantCurrent: true},
		{name: "old writer advanced graph", version: int64(1), generation: int64(8), indexedGen: int64(7)},
		{name: "index generation absent", version: int64(1), generation: int64(7)},
		{name: "index version unsupported", version: int64(2), generation: int64(7), indexedGen: int64(7)},
		{name: "graph generation invalid", version: int64(1), generation: int64(0), indexedGen: int64(0)},
		{name: "unexpected driver value", version: 1, generation: int64(7), indexedGen: int64(7)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := signatureIndexMarkerCurrent(test.version, test.generation, test.indexedGen); got != test.wantCurrent {
				t.Fatalf("signatureIndexMarkerCurrent() = %t, want %t", got, test.wantCurrent)
			}
		})
	}
}

func TestIncrementalProjectionDoesNotRevalidateAStaleSignatureMarker(t *testing.T) {
	for _, condition := range []string{
		"m.signature_index_version = 1",
		"m.signature_index_generation = previous_generation",
		"previous_generation >= 1",
		"ELSE null",
	} {
		if !strings.Contains(incrementalProjectionMarkerQuery, condition) {
			t.Errorf("incremental projection marker query omits guard %q", condition)
		}
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

func TestSnapshotRecordBudgetIncludesMeasuredOccurrenceProjection(t *testing.T) {
	if !withinSnapshotRecordBudget(227_626, 0) {
		t.Fatal("measured 227,626-record occurrence projection exceeds the bounded record budget")
	}
	if !withinSnapshotRecordBudget(maxSnapshotRecords, maxSnapshotRecords) {
		t.Fatal("accepted maximum snapshot record counts were rejected")
	}
	if withinSnapshotRecordBudget(maxSnapshotRecords+1, 0) {
		t.Fatal("accepted record count above the bounded snapshot budget")
	}
	if withinSnapshotRecordBudget(1, maxSnapshotRecords+1) {
		t.Fatal("accepted provenance count above the bounded snapshot budget")
	}
	if withinSnapshotRecordBudget(0, 0) {
		t.Fatal("accepted empty snapshot")
	}
}
