package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
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
		name, line, origin, wantReason string
	}{
		{name: "userinfo", line: "GET /api HTTP/1.1", origin: "http://user:pass@example.invalid", wantReason: operationBackfillReasonInvalidOrigin},
		{name: "asterisk", line: "OPTIONS * HTTP/1.1", origin: "http://example.invalid", wantReason: operationBackfillReasonUnsupported},
		{name: "network path", line: "GET //other.invalid/api HTTP/1.1", origin: "http://example.invalid", wantReason: operationBackfillReasonUnsupported},
		{name: "cross-origin absolute target", line: "GET https://other.invalid/api HTTP/1.1", origin: "http://example.invalid", wantReason: operationBackfillReasonCrossOrigin},
		{name: "malformed request line", line: "not-http", origin: "http://example.invalid", wantReason: operationBackfillReasonLineMalformed},
		{name: "invalid method", line: "GE(T /api HTTP/1.1", origin: "http://example.invalid", wantReason: operationBackfillReasonInvalidMethod},
		{name: "request line over limit", line: "GET /" + strings.Repeat("a", operationBackfillMaxRequestLineBytes) + " HTTP/1.1", origin: "http://example.invalid", wantReason: operationBackfillReasonLineTooLong},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, _, _, reason, ok := operationBackfillTargetWithReason([]byte(test.line+"\r\n\r\n"), test.origin)
			if ok {
				t.Fatal("operationBackfillTarget() accepted an unsupported route")
			}
			if reason != test.wantReason {
				t.Fatalf("operationBackfillTargetWithReason() reason=%q, want %q", reason, test.wantReason)
			}
		})
	}
}

func TestOperationBackfillNoOperationReasonCountsStayRedactedAndAdditive(t *testing.T) {
	preview := OperationBackfillPreview{}
	countOperationBackfillNoOperation(&preview, operationBackfillReasonLineMalformed)
	countOperationBackfillNoOperation(&preview, operationBackfillReasonLineMalformed)
	countOperationBackfillNoOperation(&preview, operationBackfillReasonCrossOrigin)
	if preview.SkippedNoOperation != 3 || preview.SkippedNoOperationReasons[operationBackfillReasonLineMalformed] != 2 ||
		preview.SkippedNoOperationReasons[operationBackfillReasonCrossOrigin] != 1 {
		t.Fatalf("unexpected no-operation breakdown: %+v", preview)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatalf("json.Marshal(preview) error = %v", err)
	}
	if strings.Contains(string(encoded), "GET /") || strings.Contains(string(encoded), "example.invalid") {
		t.Fatalf("preview reason counts exposed request data: %s", encoded)
	}
}

func TestOperationBackfillProjectionReasonIsSpecificAndFixed(t *testing.T) {
	endpoint := behaviorgraph.Record{"data": behaviorgraph.Record{"origin": "https://example.invalid", "method": "GET"}}
	longTarget := "https://example.invalid/" + strings.Repeat("a", 4096)
	for _, test := range []struct {
		name                              string
		operation, hasEndpoint, assertion bool
		endpointEntity                    behaviorgraph.Record
		target                            string
		want                              string
	}{
		{name: "endpoint missing", assertion: true, want: operationBackfillReasonMissingEndpoint},
		{name: "assertion missing", operation: true, hasEndpoint: true, endpointEntity: endpoint, want: operationBackfillReasonMissingAssertion},
		{name: "unknown origin", hasEndpoint: true, endpointEntity: behaviorgraph.Record{"data": behaviorgraph.Record{"origin": "unknown", "method": "GET"}}, target: "https://example.invalid/api", want: operationBackfillReasonUnknownOrigin},
		{name: "unknown method", hasEndpoint: true, endpointEntity: behaviorgraph.Record{"data": behaviorgraph.Record{"origin": "https://example.invalid", "method": "UNKNOWN"}}, target: "https://example.invalid/api", want: operationBackfillReasonUnknownMethod},
		{name: "opaque route", hasEndpoint: true, endpointEntity: endpoint, target: "mailto:unclassified", want: operationBackfillReasonRouteOpaque},
		{name: "encoded route is not grouped", hasEndpoint: true, endpointEntity: endpoint, target: "https://example.invalid/api/items/%34%32", want: operationBackfillReasonRouteEncoded},
		{name: "route length limit", hasEndpoint: true, endpointEntity: endpoint, target: longTarget, want: operationBackfillReasonRoutePathTooLong},
		{name: "operation missing after inputs are valid", hasEndpoint: true, endpointEntity: endpoint, target: "https://example.invalid/api", want: operationBackfillReasonMissingOperation},
		{name: "all present fallback", operation: true, hasEndpoint: true, assertion: true, endpointEntity: endpoint, target: "https://example.invalid/api", want: operationBackfillReasonIncomplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := operationBackfillProjectionReason(test.operation, test.endpointEntity, test.hasEndpoint, test.assertion, test.target); got != test.want {
				t.Fatalf("operationBackfillProjectionReason() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOperationBackfillCardEndpointSupportsFlatAndNestedShapes(t *testing.T) {
	for _, fixture := range []struct {
		name           string
		endpoint       behaviorgraph.Record
		expectedOrigin string
	}{
		{name: "current flat card", endpoint: behaviorgraph.Record{"id": "endpoint-a", "origin": "https://flat.fixture.invalid"}, expectedOrigin: "https://flat.fixture.invalid"},
		{name: "legacy nested entity", endpoint: behaviorgraph.Record{
			"id": "endpoint-b", "data": behaviorgraph.Record{"origin": "https://nested.fixture.invalid"},
		}, expectedOrigin: "https://nested.fixture.invalid"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			id, data, ok := operationBackfillCardEndpoint(behaviorgraph.Record{"endpoint": fixture.endpoint})
			if !ok || id != fixture.endpoint["id"] || data["origin"] != fixture.expectedOrigin {
				t.Fatalf("operationBackfillCardEndpoint() = (%q, %#v, %t)", id, data, ok)
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

func TestOperationBackfillCoverageAllowsCompleteCandidateWithExplicitGaps(t *testing.T) {
	preview := OperationBackfillPreview{
		HistoricalCaptures: 1400, Scanned: 1400, EligibleCaptures: 1362,
		SkippedNoOperation: 7, EndpointIdentityMismatches: 31,
	}
	finalizeOperationBackfillCoverage(&preview)
	if preview.State != "ready" || preview.CoverageState != "partial" {
		t.Fatalf("complete candidate with gaps state=%q coverage=%q", preview.State, preview.CoverageState)
	}
	preview.Truncated = true
	finalizeOperationBackfillCoverage(&preview)
	if preview.State != "partial" || preview.CoverageState != "partial" {
		t.Fatalf("truncated candidate state=%q coverage=%q", preview.State, preview.CoverageState)
	}
	preview = OperationBackfillPreview{HistoricalCaptures: 10, Scanned: 10}
	finalizeOperationBackfillCoverage(&preview)
	if preview.State != "empty" || preview.CoverageState != "complete" {
		t.Fatalf("empty candidate state=%q coverage=%q", preview.State, preview.CoverageState)
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
	endpoint, found := operationBackfillEntity(bundle, "endpoint")
	if !found || assertion["subject"] != endpoint["id"] {
		t.Fatal("historical projection did not retain the endpoint entity for repair")
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
		endpoint := behaviorgraph.Record{
			"id": "endpoint-" + strings.Repeat("d", 64), "kind": "endpoint", "data": behaviorgraph.Record{
				"program": program, "origin": "http://example.invalid", "method": "GET", "route": "/api/items/{value}",
			}, "source_refs": []behaviorgraph.Record{source},
		}
		additions = append(additions, operationBackfillAddition{
			source: source, endpoint: endpoint, operation: operation, assertion: assertion,
		})
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
	if len(firstEntities) != 2 ||
		len(firstAssertions) != operationBackfillBundleSources || len(first["cards"].([]behaviorgraph.Record)) != 0 {
		t.Fatal("operation bundles did not retain deduplicated nodes and per-capture provenance")
	}
	for _, entity := range firstEntities {
		if len(entity["source_refs"].([]behaviorgraph.Record)) != operationBackfillBundleSources {
			t.Fatalf("merged %s provenance=%d, want %d", entity["kind"], len(entity["source_refs"].([]behaviorgraph.Record)), operationBackfillBundleSources)
		}
	}
}

func TestApplyHistoricalOperationBackfillRequiresPreviewDigestAndQueuesIdempotently(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, liveWriter := newLiveTailFixture(t)
	defer liveWriter.Close()
	projectKey := projectStore.ProjectKey
	exchange := projectionWorkerExchange(selected, "operation-backfill-apply-fixture", "")
	exchange.Target = "https://api.fixture.invalid/api/items/42"
	exchange.RequestRaw = []byte("GET /api/items/42 HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	pageResult, err := writePendingHistoryRevision(ctx, pageWriter, exchange)
	pageWriter.Close()
	if err != nil || len(pageResult.SourceRefs) != 1 {
		t.Fatalf("write synthetic history: sources=%+v err=%v", pageResult.SourceRefs, err)
	}

	var encodedBundle []byte
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT payload_json FROM projection_jobs WHERE state='pending' ORDER BY sequence_id LIMIT 1",
	).Scan(&encodedBundle); err != nil {
		t.Fatalf("read synthetic projection bundle: %v", err)
	}
	var legacyBundle behaviorgraph.Record
	if err := json.Unmarshal(encodedBundle, &legacyBundle); err != nil {
		t.Fatalf("decode synthetic projection bundle: %v", err)
	}
	entities, ok := operationBackfillRecords(legacyBundle["entities"])
	if !ok {
		t.Fatal("legacy graph entities are invalid")
	}
	legacyEntities := make([]behaviorgraph.Record, 0, len(entities))
	for _, entity := range entities {
		if entity["kind"] != "operation" {
			legacyEntities = append(legacyEntities, entity)
		}
	}
	legacyBundle["entities"] = legacyEntities
	assertions, ok := operationBackfillRecords(legacyBundle["assertions"])
	if !ok {
		t.Fatal("legacy graph assertions are invalid")
	}
	legacyAssertions := make([]behaviorgraph.Record, 0, len(assertions))
	for _, assertion := range assertions {
		if assertion["predicate"] != "belongs_to_operation" {
			legacyAssertions = append(legacyAssertions, assertion)
		}
	}
	legacyBundle["assertions"] = legacyAssertions

	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	legacyWriter, err := behaviorgraph.OpenWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyWriter.ImportBundle(ctx, legacyBundle); err != nil {
		t.Fatalf("write legacy endpoint-only graph: %v", err)
	}
	dataRoot := filepath.Dir(filepath.Dir(projectStore.Directory))
	liveWriter.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatal(err)
	}
	readOnlyProject, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected, dataRoot, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewHistoricalOperationBackfill(ctx, readOnlyProject, maintenance, OperationBackfillOptions{})
	closeReadOnlyErr := readOnlyProject.Close()
	if err != nil || closeReadOnlyErr != nil || preview.State != "ready" || preview.EligibleCaptures != 1 || len(preview.Bundles) != 1 {
		t.Fatalf("preview state=%+v err=%v close=%v", preview, err, closeReadOnlyErr)
	}
	projectStore, err = store.OpenProjectForWrite(ctx, selected, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	before, err := legacyWriter.PendingOutbox(ctx, 100)
	if err != nil || len(before) != 1 {
		t.Fatalf("legacy pending outbox=%d err=%v", len(before), err)
	}
	partialPreview := preview
	partialPreview.State = "partial"
	if _, err := ApplyHistoricalOperationBackfill(ctx, projectStore, maintenance, partialPreview, preview.CandidateSHA256); err == nil {
		t.Fatal("apply accepted a partial preview")
	}
	if _, err := ApplyHistoricalOperationBackfill(ctx, projectStore, maintenance, preview, strings.Repeat("0", 64)); err == nil {
		t.Fatal("apply accepted a candidate digest different from the preview")
	}
	stillPending, err := legacyWriter.PendingOutbox(ctx, 100)
	if err != nil || len(stillPending) != len(before) {
		t.Fatalf("digest mismatch changed the outbox: before=%d after=%d err=%v", len(before), len(stillPending), err)
	}

	result, err := ApplyHistoricalOperationBackfill(ctx, projectStore, maintenance, preview, preview.CandidateSHA256)
	if err != nil || result.State != "committed" || result.BundlesImported != 1 || result.BundlesReplayed != 0 || len(result.BundleIDs) != 1 || result.Neo4jDrain != "not_attempted" {
		t.Fatalf("apply result=%+v err=%v", result, err)
	}
	after, err := legacyWriter.PendingOutbox(ctx, 100)
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("apply did not enqueue one outbox bundle: before=%d after=%d err=%v", len(before), len(after), err)
	}

	replay, err := ApplyHistoricalOperationBackfill(ctx, projectStore, maintenance, preview, preview.CandidateSHA256)
	if err != nil || replay.BundlesImported != 0 || replay.BundlesReplayed != 1 || len(replay.BundleIDs) != 1 || replay.BundleIDs[0] != result.BundleIDs[0] {
		t.Fatalf("idempotent apply replay=%+v err=%v", replay, err)
	}
	afterReplay, err := legacyWriter.PendingOutbox(ctx, 100)
	if err != nil || len(afterReplay) != len(after) {
		t.Fatalf("replay enqueued a duplicate outbox bundle: before=%d after=%d err=%v", len(after), len(afterReplay), err)
	}

	graphReader, err := behaviorgraph.OpenReader(ctx, projectStore.BehaviorGraphPath, projectKey, projectStore.BehaviorReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := graphReader.Snapshot()
	closeGraphErr := graphReader.Close()
	if err != nil || closeGraphErr != nil {
		t.Fatalf("read synthetic graph after apply: err=%v close=%v", err, closeGraphErr)
	}
	operationRecords, ok := operationBackfillRecords(snapshot["entities"])
	if !ok {
		t.Fatal("updated graph entities are invalid")
	}
	foundOperation := false
	for _, entity := range operationRecords {
		if entity["kind"] == "operation" {
			foundOperation = true
			break
		}
	}
	assertionRecords, ok := operationBackfillRecords(snapshot["assertions"])
	if !ok {
		t.Fatal("updated graph assertions are invalid")
	}
	foundLink := false
	for _, assertion := range assertionRecords {
		if assertion["predicate"] == "belongs_to_operation" {
			foundLink = true
			break
		}
	}
	if !foundOperation || !foundLink {
		t.Fatalf("backfill graph missing operation/link: operation=%t link=%t", foundOperation, foundLink)
	}
}
