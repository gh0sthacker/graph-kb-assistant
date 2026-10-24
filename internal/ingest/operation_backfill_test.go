package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
)

func TestOperationBackfillTargetPinsOriginAndKeepsRequestTarget(t *testing.T) {
	method, target, scheme, host, port, ok := operationBackfillTarget(
		[]byte("GET /api/products/50?q=blue HTTP/1.1\r\nHost: ignored.invalid\r\n\r\n"),
		"http://192.168.1.149:3000",
	)
	if !ok || method != "GET" || target != "http://192.168.1.149:3000/api/products/50?q=blue" || scheme != "http" || host != "192.168.1.149" || port != 3000 {
		t.Fatalf("operationBackfillTarget() = (%q, %q, %q, %q, %d, %t)", method, target, scheme, host, port, ok)
	}
}

func TestOperationBackfillTargetRejectsInvalidOriginAndAsteriskRoute(t *testing.T) {
	for _, test := range []struct {
		name, line, origin string
	}{
		{name: "userinfo", line: "GET /api HTTP/1.1", origin: "http://user:pass@example.invalid"},
		{name: "asterisk", line: "OPTIONS * HTTP/1.1", origin: "http://example.invalid"},
		{name: "network path", line: "GET //other.invalid/api HTTP/1.1", origin: "http://example.invalid"},
		{name: "cross-origin absolute target", line: "GET https://other.invalid/api HTTP/1.1", origin: "http://example.invalid"},
		{name: "bad request line", line: "not-http", origin: "http://example.invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, _, ok := operationBackfillTarget([]byte(test.line+"\r\n\r\n"), test.origin); ok {
				t.Fatal("operationBackfillTarget() accepted an unsupported route")
			}
		})
	}
}

func TestOperationBackfillPreviewJSONDoesNotExposeBundles(t *testing.T) {
	preview := OperationBackfillPreview{
		ProjectKey: strings.Repeat("a", 64), State: "ready", WriteMode: "preview_only",
		Bundles: []behaviorgraph.Record{{"raw_request": "sensitive fixture value"}},
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatalf("json.Marshal(preview) error = %v", err)
	}
	if strings.Contains(string(encoded), "sensitive fixture value") || strings.Contains(string(encoded), "raw_request") {
		t.Fatalf("preview JSON exposed an in-memory bundle: %s", encoded)
	}
}

func TestOperationBackfillExtractsOperationFromHistoricalProjectionFixture(t *testing.T) {
	projectRef := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "operation-backfill"}
	exchangeRef := caido.ExchangeRef{Project: projectRef, RequestID: "fixture-request"}
	projectKey, err := projectRef.StorageKey()
	if err != nil {
		t.Fatalf("project StorageKey() error = %v", err)
	}
	source := behaviorgraph.Record{
		"path": "/private/fixture-receipt.json", "sha256": strings.Repeat("a", 64),
		"record_locator": "fixture-record",
	}
	exchange := caido.HttpExchange{
		Ref: exchangeRef, RequestRaw: []byte("GET /api/products/50?q=blue HTTP/1.1\r\nHost: example.invalid\r\n\r\n"),
		Method: "GET", Scheme: "http", Host: "example.invalid", Port: 80,
		Target: "http://example.invalid/api/products/50?q=blue", RequestWireFidelity: "source_bytes",
		ResponseWireFidelity: "unknown",
	}
	bundle, _, err := behaviorgraph.BuildHistoricalBundle(exchange, projectKey, source)
	if err != nil {
		t.Fatalf("BuildHistoricalBundle() error = %v", err)
	}
	operation, found := operationBackfillEntity(bundle, "operation")
	if !found {
		t.Fatal("historical projection did not produce an operation entity")
	}
	assertion, found := operationBackfillAssertion(bundle, "belongs_to_operation")
	if !found || assertion["object"] != operation["id"] {
		t.Fatal("historical projection did not connect its endpoint to the operation")
	}
	data, ok := operationBackfillRecord(operation["data"])
	if !ok || data["route_family"] != "/api/products/{value}" {
		t.Fatalf("operation route family = %#v, want /api/products/{value}", data["route_family"])
	}
}

func TestBuildOperationBackfillBundlesMergesOperationSourcesAndBoundsPages(t *testing.T) {
	program := strings.Repeat("a", 64)
	opID := "operation-" + strings.Repeat("b", 64)
	var additions []operationBackfillAddition
	for index := 0; index < operationBackfillBundleSources+1; index++ {
		source := behaviorgraph.Record{
			"path":   fmt.Sprintf("/private/receipt-%04d.json", index),
			"sha256": strings.Repeat("c", 64), "record_locator": fmt.Sprintf("receipt-%04d", index),
		}
		assertionID := fmt.Sprintf("assertion-%064x", index+1)
		assertion := behaviorgraph.Record{
			"id": assertionID, "subject": "endpoint-" + strings.Repeat("d", 64),
			"predicate": "belongs_to_operation", "object": opID,
			"context":     behaviorgraph.Record{"program": program, "origin": "http://example.invalid"},
			"origin_kind": "parsed", "evidence_state": "observed", "source_refs": []behaviorgraph.Record{source},
		}
		operation := behaviorgraph.Record{
			"id": opID, "kind": "operation", "data": behaviorgraph.Record{
				"program": program, "origin": "http://example.invalid", "method": "GET",
				"route_family_version": "route-family-v1", "route_family": "/api/items/{value}",
			}, "source_refs": []behaviorgraph.Record{source},
		}
		additions = append(additions, operationBackfillAddition{source: source, operation: operation, assertion: assertion})
	}

	bundles, err := buildOperationBackfillBundles(program, additions)
	if err != nil {
		t.Fatalf("buildOperationBackfillBundles() error = %v", err)
	}
	if len(bundles) != 2 {
		t.Fatalf("buildOperationBackfillBundles() returned %d bundles, want 2", len(bundles))
	}
	first, second := bundles[0], bundles[1]
	firstSources := first["sources"].([]behaviorgraph.Record)
	secondSources := second["sources"].([]behaviorgraph.Record)
	if len(firstSources) != operationBackfillBundleSources || len(secondSources) != 1 {
		t.Fatalf("bundle source counts = %d and %d, want %d and 1", len(firstSources), len(secondSources), operationBackfillBundleSources)
	}
	firstEntities := first["entities"].([]behaviorgraph.Record)
	firstAssertions := first["assertions"].([]behaviorgraph.Record)
	if len(firstEntities) != 1 || firstEntities[0]["id"] != opID ||
		len(firstEntities[0]["source_refs"].([]behaviorgraph.Record)) != operationBackfillBundleSources ||
		len(firstAssertions) != operationBackfillBundleSources || len(first["cards"].([]behaviorgraph.Record)) != 0 {
		t.Fatal("operation bundles did not retain deduplicated nodes and per-capture provenance")
	}
}
