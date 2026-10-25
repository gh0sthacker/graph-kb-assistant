package graph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestHistoricalQueryValuesUseProjectKeyedReferencesAndSourceSpans(t *testing.T) {
	firstRaw := "GET /items?item=shared%20value&item=SECOND_VALUE_482&password=PASSWORD_VALUE_913 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"
	projectKeyA := fieldValueKeyForProject(t, "fixture-project")
	projectKeyB := fieldValueKeyForProject(t, "fixture-project-other")
	defer clear(projectKeyA)
	defer clear(projectKeyB)
	first := buildFieldValueBundleForProject(t, "fixture-project", "value-first", firstRaw, projectKeyA)
	validateJSONEntities(t, first)
	secondRaw := "GET /items?other=shared+value HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"
	second := buildFieldValueBundleForProject(t, "fixture-project", "value-second", secondRaw, projectKeyA)
	otherProjectKey := buildFieldValueBundleForProject(t, "fixture-project-other", "value-other-project", secondRaw, projectKeyB)

	firstRefs := valueReferenceEntities(t, first)
	secondRefs := valueReferenceEntities(t, second)
	otherProjectRefs := valueReferenceEntities(t, otherProjectKey)
	if len(firstRefs) != 2 || len(secondRefs) != 1 || len(otherProjectRefs) != 1 {
		t.Fatalf("value references = %d/%d/%d, want 2/1/1 (sensitive fields and oversized values must be omitted)", len(firstRefs), len(secondRefs), len(otherProjectRefs))
	}
	if firstRefs[0]["id"] != secondRefs[0]["id"] {
		t.Fatal("equivalent URL-decoded values in different fields did not share a project-scoped reference")
	}
	if firstRefs[0]["id"] == otherProjectRefs[0]["id"] {
		t.Fatal("value reference was reusable across different project keys")
	}

	links := valueUsageAssertions(t, first)
	if len(links) != 2 {
		t.Fatalf("value usage assertions = %d, want one for each non-sensitive occurrence", len(links))
	}
	seenSpans := map[string]bool{}
	occurrenceIndexes := []int{}
	spanStarts := []int{}
	for _, assertion := range links {
		if assertion["predicate"] != "value_used_in" {
			t.Fatalf("value relation predicate = %v", assertion["predicate"])
		}
		context := assertion["context"].(Record)
		if context["scenario"] != "keyed_value_equality" || context["scenario_step"] != "correlation_only" {
			t.Fatalf("value relation overstates its meaning: %#v", context)
		}
		occurrence := fieldOccurrenceEntity(t, first, assertion["object"].(string))
		data := occurrence["data"].(Record)
		occurrenceIndexes = append(occurrenceIndexes, data["occurrence_index"].(int))
		span := data["value_span"].(Record)
		start, end := span["start"].(int), span["end"].(int)
		if start < 0 || end < start || end > len(firstRaw) || span["unit"] != "byte" || span["offset_basis"] != "request_message" {
			t.Fatalf("invalid source span: %#v", span)
		}
		spanStarts = append(spanStarts, start)
		seenSpans[string(firstRaw[start:end])] = true
	}
	if !seenSpans["shared%20value"] || !seenSpans["SECOND_VALUE_482"] {
		t.Fatalf("source spans did not point at the original encoded values: %#v", seenSpans)
	}
	if !reflect.DeepEqual(occurrenceIndexes, []int{0, 1}) || spanStarts[0] >= spanStarts[1] {
		t.Fatalf("repeated query occurrence order was not preserved: indexes=%v starts=%v", occurrenceIndexes, spanStarts)
	}

	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal graph bundle: %v", err)
	}
	for _, secret := range []string{"shared%20value", "SECOND_VALUE_482", "PASSWORD_VALUE_913"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("graph bundle contains raw query value %q", secret)
		}
	}
}

func TestGraphSliceTraversesProjectValueMatchToSecondCapture(t *testing.T) {
	ctx := context.Background()
	projectID := "slice-field-value-links"
	projectKey := fieldValueKeyForProject(t, projectID)
	defer clear(projectKey)
	root := t.TempDir()
	receiptRoot := filepath.Join(root, "receipts")
	if err := os.Mkdir(receiptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	rawA := "GET /items?id=SYNTHETIC_SHARED_601 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"
	rawB := "GET /cart?item=SYNTHETIC_SHARED_601 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"
	bundleA := buildFieldValueBundleInReceiptRoot(t, projectID, "capture-a", rawA, projectKey, receiptRoot)
	bundleB := buildFieldValueBundleInReceiptRoot(t, projectID, "capture-b", rawB, projectKey, receiptRoot)
	snapshot := mergeFieldValueBundles(bundleA, bundleB)
	reader, err := OpenReaderFromSnapshot(ctx, snapshot, snapshot["program"].(string), receiptRoot)
	if err != nil {
		t.Fatalf("open synthetic graph snapshot: %v", err)
	}
	defer reader.Close()

	observationA := entitiesByKind(bundleA, "observation")[0]["id"].(string)
	observationB := entitiesByKind(bundleB, "observation")[0]["id"].(string)
	fieldA := entitiesByKind(bundleA, "field_occurrence")[0]["id"].(string)
	fieldB := entitiesByKind(bundleB, "field_occurrence")[0]["id"].(string)
	valueMatch := entitiesByKind(bundleA, "value_reference")[0]["id"].(string)
	seeds, truncated, err := reader.FieldValueTraversalSeeds(observationA, MaxFieldValueTraversalPaths)
	if err != nil || truncated {
		t.Fatalf("expand field value traversal: seeds=%v truncated=%t err=%v", seeds, truncated, err)
	}
	for _, want := range []string{observationA, fieldA, valueMatch, fieldB, observationB} {
		if !containsString(seeds, want) {
			t.Errorf("expanded traversal omitted %s: %v", want, seeds)
		}
	}

	slice, err := reader.Slice(ctx, SliceOptions{SeedIDs: seeds, MaxNodes: 40, MaxEdges: 60, MaxDepth: 1})
	if err != nil {
		t.Fatalf("read focused traversal slice: %v", err)
	}
	seenPredicates := map[string]bool{}
	seenEntities := map[string]bool{}
	for _, entity := range slice["entities"].([]Record) {
		seenEntities[entity["id"].(string)] = true
	}
	for _, assertion := range slice["assertions"].([]Record) {
		seenPredicates[assertion["predicate"].(string)] = true
	}
	for _, want := range []string{observationA, fieldA, valueMatch, fieldB, observationB} {
		if !seenEntities[want] {
			t.Errorf("focused graph slice omitted path node %s", want)
		}
	}
	for _, predicate := range []string{"has_occurrence", "occurrence_of", "value_used_in"} {
		if !seenPredicates[predicate] {
			t.Errorf("focused graph slice omitted %q", predicate)
		}
	}
	encoded, err := json.Marshal(slice)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SYNTHETIC_SHARED_601")) {
		t.Fatal("field value traversal exposed the raw query value")
	}

	otherProject := "slice-field-value-other-project"
	otherKey := fieldValueKeyForProject(t, otherProject)
	defer clear(otherKey)
	otherBundle := buildFieldValueBundleInReceiptRoot(t, otherProject, "other-project-capture", rawA, otherKey, receiptRoot)
	otherObservation := entitiesByKind(otherBundle, "observation")[0]["id"].(string)
	if _, _, err := reader.FieldValueTraversalSeeds(otherObservation, MaxFieldValueTraversalPaths); err == nil {
		t.Fatal("selected project graph resolved an observation from another project")
	}
}

func buildFieldValueBundleInReceiptRoot(t *testing.T, projectID, requestID, raw string, fieldValueKey []byte, receiptRoot string) Record {
	t.Helper()
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: projectID}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(receiptRoot, requestID+".json")
	receiptBytes := []byte("synthetic receipt " + requestID)
	if err := os.WriteFile(receiptPath, receiptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	receiptDigest := sha256.Sum256(receiptBytes)
	status, notTruncated := 200, false
	exchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: project, RequestID: requestID}, RequestRaw: []byte(raw),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "fixture.invalid", Port: 443,
		Target: requestTargetFromFixture(raw), ResponseStatus: &status,
		RequestTruncated: &notTruncated, ResponseTruncated: &notTruncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	bundle, _, err := BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, projectKey, Record{
		"path": receiptPath, "sha256": hex.EncodeToString(receiptDigest[:]), "record_locator": "$.records[0]",
	}, strings.Repeat("b", 64), ptrString(strings.Repeat("c", 64)), fieldValueKey, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func mergeFieldValueBundles(bundles ...Record) Record {
	sources := make([]Record, 0)
	entitiesByID := make(map[string]Record)
	assertions := make([]Record, 0)
	cards := make([]Record, 0)
	for _, bundle := range bundles {
		sources = append(sources, bundle["sources"].([]Record)...)
		for _, entity := range bundle["entities"].([]Record) {
			id := entity["id"].(string)
			if _, exists := entitiesByID[id]; !exists {
				entitiesByID[id] = entity
			}
		}
		assertions = append(assertions, bundle["assertions"].([]Record)...)
		cards = append(cards, bundle["cards"].([]Record)...)
	}
	entities := make([]Record, 0, len(entitiesByID))
	for _, entity := range entitiesByID {
		entities = append(entities, entity)
	}
	return Record{
		"schema_version": json.Number("2"), "program": bundles[0]["program"],
		"sources": sources, "entities": entities, "assertions": assertions, "cards": cards,
	}
}

func TestHistoricalQueryValueReferencesRequireAProjectKey(t *testing.T) {
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatalf("project storage key: %v", err)
	}
	status, notTruncated := 200, false
	exchange := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: project, RequestID: "invalid-key"},
		RequestRaw:  []byte("GET /items?id=VALUE_1 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "fixture.invalid", Port: 443, Target: "/items?id=VALUE_1",
		ResponseStatus: &status, RequestTruncated: &notTruncated, ResponseTruncated: &notTruncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	_, _, err = BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, projectKey, Record{
		"path": "/synthetic/receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.records[0]",
	}, strings.Repeat("b", 64), ptrString(strings.Repeat("c", 64)), []byte("short"), strings.Repeat("d", 64))
	if err == nil {
		t.Fatal("builder accepted an invalid field-value key")
	}
}

func TestHistoricalQueryValuesOmitUnboundSpansAndReportCoverageGap(t *testing.T) {
	bundle := buildFieldValueBundleWithTarget(t, "value-span-mismatch",
		"GET /items?id=RAW_VALUE_731 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n",
		"https://fixture.invalid/items?id=DIFFERENT_METADATA_VALUE_842", bytes.Repeat([]byte{0x61}, 32))
	if occurrences := entitiesByKind(bundle, "field_occurrence"); len(occurrences) != 0 {
		t.Fatalf("unbound request target produced %d field occurrences", len(occurrences))
	}
	if references := entitiesByKind(bundle, "value_reference"); len(references) != 0 {
		t.Fatalf("unbound request target produced %d value references", len(references))
	}
	card := bundle["cards"].([]Record)[0]
	missing := card["missing_evidence"].([]any)
	foundGap := false
	for _, item := range missing {
		if strings.Contains(item.(string), "exact request-message byte spans") {
			foundGap = true
		}
	}
	if !foundGap {
		t.Fatal("card does not explain why unmatched query spans were omitted")
	}
}

func buildFieldValueBundle(t *testing.T, requestID, requestRaw string, fieldValueKey []byte) Record {
	t.Helper()
	return buildFieldValueBundleForProject(t, "fixture-project", requestID, requestRaw, fieldValueKey)
}

func buildFieldValueBundleWithTarget(t *testing.T, requestID, requestRaw, target string, fieldValueKey []byte) Record {
	return buildFieldValueBundleWithTargetForProject(t, "fixture-project", requestID, requestRaw, target, fieldValueKey)
}

func buildFieldValueBundleForProject(t *testing.T, projectID, requestID, requestRaw string, fieldValueKey []byte) Record {
	t.Helper()
	return buildFieldValueBundleWithTargetForProject(t, projectID, requestID, requestRaw, requestTargetFromFixture(requestRaw), fieldValueKey)
}

func buildFieldValueBundleWithTargetForProject(t *testing.T, projectID, requestID, requestRaw, target string, fieldValueKey []byte) Record {
	t.Helper()
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: projectID}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatalf("project storage key: %v", err)
	}
	status, notTruncated := 200, false
	exchange := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: project, RequestID: requestID},
		RequestRaw:  []byte(requestRaw),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "fixture.invalid", Port: 443,
		Target: target, ResponseStatus: &status,
		RequestTruncated: &notTruncated, ResponseTruncated: &notTruncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	root := t.TempDir()
	receiptRoot := filepath.Join(root, "receipts")
	if err := os.Mkdir(receiptRoot, 0o700); err != nil {
		t.Fatalf("create synthetic receipt root: %v", err)
	}
	receiptPath := filepath.Join(receiptRoot, "capture.json")
	receiptBytes := []byte("synthetic-receipt-" + requestID)
	if err := os.WriteFile(receiptPath, receiptBytes, 0o600); err != nil {
		t.Fatalf("write synthetic receipt: %v", err)
	}
	receiptDigest := sha256.Sum256(receiptBytes)
	bundle, _, err := BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, projectKey, Record{
		"path": receiptPath, "sha256": hex.EncodeToString(receiptDigest[:]), "record_locator": "$.records[0]",
	}, strings.Repeat("b", 64), ptrString(strings.Repeat("c", 64)), fieldValueKey, strings.Repeat("d", 64))
	if err != nil {
		t.Fatalf("build field-value graph bundle: %v", err)
	}
	writer := &Writer{program: projectKey, graphPath: filepath.Join(root, "behavior.sqlite3"), receiptRoot: receiptRoot}
	if _, err := writer.ImportBundle(context.Background(), bundle); err != nil {
		t.Fatalf("import synthetic field-value graph bundle: %v", err)
	}
	return bundle
}

func entitiesByKind(bundle Record, kind string) []Record {
	var result []Record
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] == kind {
			result = append(result, entity)
		}
	}
	return result
}

func fieldValueKeyForProject(t *testing.T, projectID string) []byte {
	t.Helper()
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: projectID}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatalf("project storage key: %v", err)
	}
	rootKey := bytes.Repeat([]byte{0x38}, 32)
	indexKey, responseKey, err := DeriveLiveKeys(projectKey, rootKey)
	clear(rootKey)
	if err != nil {
		t.Fatalf("derive project graph keys: %v", err)
	}
	defer clear(indexKey)
	defer clear(responseKey)
	fieldValueKey, err := DeriveFieldValueKey(indexKey)
	if err != nil {
		t.Fatalf("derive field-value key: %v", err)
	}
	return fieldValueKey
}

func validateJSONEntities(t *testing.T, bundle Record) {
	t.Helper()
	payload, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal generated bundle: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var decoded Record
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode generated bundle: %v", err)
	}
	if _, _, err := validateProjectionBundleWithRecord(payload, bundle["program"].(string)); err != nil {
		t.Fatalf("normalize generated projection bundle: %v", err)
	}
	entities, err := recordList(decoded["entities"], maxWriterRecords)
	if err != nil {
		t.Fatalf("decode generated entities: %v", err)
	}
	for _, entity := range entities {
		if err := validateEntity(entity, 1, bundle["program"].(string)); err != nil {
			t.Fatalf("validate generated %s entity: %v; data=%#v", entity["kind"], err, entity["data"])
		}
		if err := validateEntityContext(entity, bundle["program"].(string)); err != nil {
			t.Fatalf("validate generated %s entity context: %v", entity["kind"], err)
		}
	}
	sources, err := recordList(decoded["sources"], maxWriterSources)
	if err != nil {
		t.Fatalf("decode generated sources: %v", err)
	}
	declared := make(map[string]Record, len(sources))
	for _, source := range sources {
		declared[sourcePair(source)] = source
	}
	assertions, err := recordList(decoded["assertions"], maxWriterRecords)
	if err != nil {
		t.Fatalf("decode generated assertions: %v", err)
	}
	for _, assertion := range assertions {
		if err := validateAssertion(assertion, declared, bundle["program"].(string)); err != nil {
			t.Fatalf("validate generated %s assertion: %v; context=%#v", assertion["predicate"], err, assertion["context"])
		}
	}
	cards, err := recordList(decoded["cards"], maxWriterRecords)
	if err != nil {
		t.Fatalf("decode generated cards: %v", err)
	}
	for _, card := range cards {
		if err := validateCard(card, declared, bundle["program"].(string)); err != nil {
			t.Fatalf("validate generated card: %v", err)
		}
	}
}

func requestTargetFromFixture(raw string) string {
	fields := strings.Fields(strings.SplitN(raw, "\r\n", 2)[0])
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func valueReferenceEntities(t *testing.T, bundle Record) []Record {
	t.Helper()
	var result []Record
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] == "value_reference" {
			result = append(result, entity)
		}
	}
	return result
}

func valueUsageAssertions(t *testing.T, bundle Record) []Record {
	t.Helper()
	var result []Record
	for _, assertion := range bundle["assertions"].([]Record) {
		if assertion["predicate"] == "value_used_in" {
			result = append(result, assertion)
		}
	}
	return result
}

func fieldOccurrenceEntity(t *testing.T, bundle Record, id string) Record {
	t.Helper()
	for _, entity := range bundle["entities"].([]Record) {
		if entity["id"] == id && entity["kind"] == "field_occurrence" {
			return entity
		}
	}
	t.Fatalf("bundle has no field occurrence entity %q", id)
	return nil
}

func ptrString(value string) *string { return &value }
