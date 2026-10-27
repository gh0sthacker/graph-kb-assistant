package graph

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestBuildHistoricalBundlePreservesRepeatedQueryOccurrencesWithoutValues(t *testing.T) {
	bundle, _ := buildQueryOccurrenceBundle(t, "repeated", "https://fixture.invalid/items?id=VALUE_A_92&id=VALUE_B_41&mode=VALUE_C_83")
	paths := queryParameterPaths(bundle)
	want := []string{`query["id"][0]`, `query["id"][1]`, `query["mode"][]`}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("query parameter paths = %#v, want %#v", paths, want)
	}

	serialized, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	for _, value := range []string{"VALUE_A_92", "VALUE_B_41", "VALUE_C_83"} {
		if strings.Contains(string(serialized), value) {
			t.Fatalf("graph bundle contains raw query value %q", value)
		}
	}
}

func TestBuildHistoricalBundleRecordsParserStateSeparatelyFromAnalysisState(t *testing.T) {
	bundle, _ := buildHistoricalOccurrenceBundle(t, "parse-state", "GET", "https://fixture.invalid/items",
		"GET /items HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")

	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "observation" {
			continue
		}
		data := entity["data"].(Record)
		if got := data["parse_state"]; got != "complete" {
			t.Fatalf("observation parse_state = %v, want complete", got)
		}
		if got := data["analysis_state"]; got != "incomplete" {
			t.Fatalf("observation analysis_state = %v, want incomplete", got)
		}
		return
	}
	t.Fatal("historical bundle has no observation entity")
}

func TestBuildHistoricalBundleGroupsDynamicRoutesByVersionedOperation(t *testing.T) {
	first, firstEndpointID := buildHistoricalOccurrenceBundle(t, "route-50", "GET", "https://fixture.invalid/api/products/50",
		"GET /api/products/50 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	second, secondEndpointID := buildHistoricalOccurrenceBundle(t, "route-51", "GET", "https://fixture.invalid/api/products/51",
		"GET /api/products/51 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	firstOperation := operationEntity(t, first)
	secondOperation := operationEntity(t, second)
	firstData := firstOperation["data"].(Record)
	if firstOperation["id"] != secondOperation["id"] || firstEndpointID != secondEndpointID {
		t.Fatalf("dynamic paths should preserve legacy endpoint grouping and share a versioned operation: endpoints %q/%q, operations %q/%q",
			firstEndpointID, secondEndpointID, firstOperation["id"], secondOperation["id"])
	}
	if firstData["route_family_version"] != RouteFamilyV2Version || firstData["route_family"] != "/api/products/{value}" ||
		firstData["semantic_status"] != "heuristic_candidate" {
		t.Fatalf("versioned operation data = %#v", firstData)
	}
	operationRecord := Record{
		"id": firstOperation["id"], "kind": "operation", "data": map[string]any(firstData),
	}
	program := firstData["program"].(string)
	if err := validateEntity(operationRecord, 2, program); err != nil {
		t.Fatalf("reader rejected valid versioned operation: %v", err)
	}
	invalidOperation := cloneRecord(operationRecord)
	invalidData := cloneRecord(firstData)
	invalidData["route_family"] = "/api/products/secret-segment"
	invalidOperation["data"] = map[string]any(invalidData)
	if err := validateEntity(invalidOperation, 2, program); err == nil {
		t.Fatal("reader accepted an unmasked operation route family")
	}
	invalidSemantics := cloneRecord(operationRecord)
	invalidSemanticsData := cloneRecord(firstData)
	invalidSemanticsData["semantic_status"] = "verified"
	invalidSemantics["data"] = map[string]any(invalidSemanticsData)
	if err := validateEntity(invalidSemantics, 2, program); err == nil {
		t.Fatal("reader accepted a heuristic route grouping as verified semantics")
	}
	if !hasOperationGroupingAssertion(first, firstEndpointID, firstOperation["id"].(string)) {
		t.Fatal("endpoint is not linked to its grouped operation")
	}

	static, _ := buildHistoricalOccurrenceBundle(t, "route-search", "GET", "https://fixture.invalid/api/products/search",
		"GET /api/products/search HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	otherMethod, _ := buildHistoricalOccurrenceBundle(t, "route-post", "POST", "https://fixture.invalid/api/products/50",
		"POST /api/products/50 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	encodedSeparator, _ := buildHistoricalOccurrenceBundle(t, "route-encoded-separator", "GET", "https://fixture.invalid/api/products/a%2Fb",
		"GET /api/products/a%2Fb HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	plainSegments, _ := buildHistoricalOccurrenceBundle(t, "route-plain-segments", "GET", "https://fixture.invalid/api/products/a/b",
		"GET /api/products/a/b HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	if operationEntity(t, static)["id"] == firstOperation["id"] {
		t.Fatal("static search route was grouped with a dynamic product route")
	}
	if operationEntity(t, otherMethod)["id"] == firstOperation["id"] {
		t.Fatal("different HTTP methods shared one operation identity")
	}
	if hasOperationEntity(encodedSeparator) || hasOperationEntity(plainSegments) {
		t.Fatal("ambiguous path segments were guessed into a heuristic operation group")
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal grouped graph: %v", err)
	}
	if strings.Contains(string(encoded), "/api/products/50") {
		t.Fatal("grouped graph contains the concrete dynamic path")
	}
}

func hasOperationEntity(bundle Record) bool {
	entities, _ := bundle["entities"].([]Record)
	for _, entity := range entities {
		if entity["kind"] == "operation" {
			return true
		}
	}
	return false
}

func operationEntity(t *testing.T, bundle Record) Record {
	t.Helper()
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] == "operation" {
			return entity
		}
	}
	t.Fatal("historical bundle has no operation entity")
	return nil
}

func hasOperationGroupingAssertion(bundle Record, endpointID, operationID string) bool {
	for _, assertion := range bundle["assertions"].([]Record) {
		if assertion["subject"] == endpointID && assertion["predicate"] == "belongs_to_operation" && assertion["object"] == operationID {
			return true
		}
	}
	return false
}

func TestBuildHistoricalBundlePreservesRepeatedFormOccurrencesWithoutValues(t *testing.T) {
	target := "https://fixture.invalid/items"
	body := "id=FORM_VALUE_A_92&id=FORM_VALUE_B_41&mode=FORM_VALUE_C_83"
	request := "POST /items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	bundle, endpointID := buildHistoricalOccurrenceBundle(t, "form", "POST", target, request)
	paths := parameterPathsWithPrefix(bundle, "request.body[")
	want := []string{`request.body["id"][0]`, `request.body["id"][1]`, `request.body["mode"][]`}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("form parameter paths = %#v, want %#v", paths, want)
	}

	serialized, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	for _, value := range []string{"FORM_VALUE_A_92", "FORM_VALUE_B_41", "FORM_VALUE_C_83"} {
		if strings.Contains(string(serialized), value) {
			t.Fatalf("graph bundle contains raw form value %q", value)
		}
	}

	singleBody := "mode=FORM_SINGLE_VALUE_56"
	singleRequest := "POST /items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: " + strconv.Itoa(len(singleBody)) + "\r\n\r\n" + singleBody
	single, singleEndpointID := buildHistoricalOccurrenceBundle(t, "form-single", "POST", target, singleRequest)
	if singleEndpointID != endpointID {
		t.Fatalf("endpoint ID changed with form occurrences: singleton %q, repeated %q", singleEndpointID, endpointID)
	}
	singleIDs := parameterIDsByPath(single, "request.body[")
	repeatedIDs := parameterIDsByPath(bundle, "request.body[")
	if singleIDs[`request.body["mode"][]`] != repeatedIDs[`request.body["mode"][]`] {
		t.Fatalf("singleton form parameter ID changed: %q != %q", singleIDs[`request.body["mode"][]`], repeatedIDs[`request.body["mode"][]`])
	}
}

func TestBuildHistoricalBundlePreservesRepeatedMultipartOccurrencesWithoutValues(t *testing.T) {
	target := "https://fixture.invalid/upload"
	singleRequest := multipartOccurrenceRequest(t, [][2]string{
		{"mode", "MODE_VALUE_17"}, {"tag", "MULTIPART_VALUE_A_31"},
	})
	single, _ := buildHistoricalOccurrenceBundle(t, "multipart-single", "POST", target, singleRequest)
	if paths := parameterPathsByLocation(single, "request.body"); !reflect.DeepEqual(paths, []string{`request.body["mode"][]`, `request.body["tag"][]`}) {
		t.Fatalf("singleton multipart paths = %#v", paths)
	}

	repeatedRequest := multipartOccurrenceRequest(t, [][2]string{
		{"mode", "MODE_VALUE_17"}, {"tag", "MULTIPART_VALUE_A_31"}, {"tag", "MULTIPART_VALUE_B_42"},
	})
	repeated, _ := buildHistoricalOccurrenceBundle(t, "multipart-repeated", "POST", target, repeatedRequest)
	want := []string{`request.body["mode"][]`, `request.body["tag"][0]`, `request.body["tag"][1]`}
	if paths := parameterPathsByLocation(repeated, "request.body"); !reflect.DeepEqual(paths, want) {
		t.Fatalf("repeated multipart paths = %#v, want %#v", paths, want)
	}
	singleIDs := parameterIDsByLocation(single, "request.body")
	repeatedIDs := parameterIDsByLocation(repeated, "request.body")
	if singleIDs[`request.body["mode"][]`] != repeatedIDs[`request.body["mode"][]`] {
		t.Fatal("adding a repeated multipart field changed an unrelated singleton parameter identity")
	}
	if repeatedIDs[`request.body["tag"][0]`] == repeatedIDs[`request.body["tag"][1]`] {
		t.Fatal("repeated multipart occurrences share a parameter identity")
	}

	encoded, err := json.Marshal(repeated)
	if err != nil {
		t.Fatalf("marshal multipart graph: %v", err)
	}
	for _, value := range []string{"MODE_VALUE_17", "MULTIPART_VALUE_A_31", "MULTIPART_VALUE_B_42"} {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("multipart graph contains raw field value %q", value)
		}
	}

	sensitiveRequest := multipartOccurrenceRequest(t, [][2]string{
		{"password", "PASSWORD_VALUE_A_53"}, {"password", "PASSWORD_VALUE_B_64"},
	})
	sensitive, _ := buildHistoricalOccurrenceBundle(t, "multipart-sensitive", "POST", target, sensitiveRequest)
	sensitivePaths := parameterPathsByLocation(sensitive, "request.body")
	if len(sensitivePaths) != 2 || !strings.HasSuffix(sensitivePaths[0], "[0]") || !strings.HasSuffix(sensitivePaths[1], "[1]") ||
		strings.TrimSuffix(sensitivePaths[0], "[0]") != strings.TrimSuffix(sensitivePaths[1], "[1]") {
		t.Fatalf("repeated sensitive multipart name lost its shared masked occurrence path: %#v", sensitivePaths)
	}
	sensitiveJSON, err := json.Marshal(sensitive)
	if err != nil {
		t.Fatalf("marshal sensitive multipart graph: %v", err)
	}
	for _, value := range []string{"password", "PASSWORD_VALUE_A_53", "PASSWORD_VALUE_B_64"} {
		if strings.Contains(string(sensitiveJSON), value) {
			t.Fatalf("sensitive multipart graph contains raw input %q", value)
		}
	}
}

func multipartOccurrenceRequest(t *testing.T, fields [][2]string) string {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary("BB-GRAPH-FIXTURE-BOUNDARY"); err != nil {
		t.Fatalf("set multipart boundary: %v", err)
	}
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			t.Fatalf("write multipart fixture field: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish multipart fixture: %v", err)
	}
	return "POST /upload HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: " + writer.FormDataContentType() +
		"\r\nContent-Length: " + strconv.Itoa(body.Len()) + "\r\n\r\n" + body.String()
}

func TestBuildHistoricalBundlePreservesRepeatedJSONArrayOccurrences(t *testing.T) {
	target := "https://fixture.invalid/items"
	body := `{"items":[{"id":"JSON_VALUE_A_13"},{"id":"JSON_VALUE_B_27"}],"mode":"JSON_VALUE_C_35"}`
	request := "POST /items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	bundle, endpointID := buildHistoricalOccurrenceBundle(t, "json", "POST", target, request)
	paths := parameterPathsByLocation(bundle, "request.body")
	want := []string{
		`$`, `$["items"]`, `$["items"][0]`, `$["items"][0]["id"]`,
		`$["items"][1]`, `$["items"][1]["id"]`, `$["mode"]`,
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("JSON parameter paths = %#v, want %#v", paths, want)
	}
	serialized, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	for _, value := range []string{"JSON_VALUE_A_13", "JSON_VALUE_B_27", "JSON_VALUE_C_35"} {
		if strings.Contains(string(serialized), value) {
			t.Fatalf("graph bundle contains raw JSON value %q", value)
		}
	}

	singleBody := `{"items":[{"id":"JSON_SINGLE_VALUE_44"}],"mode":"JSON_MODE_VALUE_52"}`
	singleRequest := "POST /items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(singleBody)) + "\r\n\r\n" + singleBody
	single, singleEndpointID := buildHistoricalOccurrenceBundle(t, "json-single", "POST", target, singleRequest)
	if singleEndpointID != endpointID {
		t.Fatalf("endpoint ID changed with JSON array occurrence count: singleton %q, repeated %q", singleEndpointID, endpointID)
	}
	singlePaths := parameterPathsByLocation(single, "request.body")
	if !containsString(singlePaths, `$["items"][]`) || !containsString(singlePaths, `$["items"][]["id"]`) {
		t.Fatalf("singleton JSON array lost its legacy template path: %#v", singlePaths)
	}
	singleIDs := parameterIDsByLocation(single, "request.body")
	repeatedIDs := parameterIDsByLocation(bundle, "request.body")
	if singleIDs[`$["mode"]`] != repeatedIDs[`$["mode"]`] {
		t.Fatalf("singleton JSON field ID changed: %q != %q", singleIDs[`$["mode"]`], repeatedIDs[`$["mode"]`])
	}
	if singleIDs[`$["items"][]`] == "" || repeatedIDs[`$["items"][0]["id"]`] == repeatedIDs[`$["items"][1]["id"]`] {
		t.Fatal("JSON array occurrences were not represented by distinct, indexed parameter IDs")
	}
}

func TestBuildHistoricalBundleKeepsSingletonQueryIdentityAndEndpointID(t *testing.T) {
	single, singleEndpointID := buildQueryOccurrenceBundle(t, "single", "https://fixture.invalid/items?id=VALUE_A_92&mode=VALUE_C_83")
	repeated, repeatedEndpointID := buildQueryOccurrenceBundle(t, "repeated", "https://fixture.invalid/items?id=VALUE_A_92&id=VALUE_B_41&mode=VALUE_C_83")

	if singleEndpointID != repeatedEndpointID {
		t.Fatalf("endpoint ID changed with repeated values: singleton %q, repeated %q", singleEndpointID, repeatedEndpointID)
	}
	paths := queryParameterPaths(single)
	want := []string{`query["id"][]`, `query["mode"][]`}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("singleton query parameter paths = %#v, want %#v", paths, want)
	}
	singleIDs, repeatedIDs := queryParameterIDsByPath(single), queryParameterIDsByPath(repeated)
	if singleIDs[`query["mode"][]`] != repeatedIDs[`query["mode"][]`] {
		t.Fatalf("singleton parameter ID changed: %q != %q", singleIDs[`query["mode"][]`], repeatedIDs[`query["mode"][]`])
	}
	if repeatedIDs[`query["id"][0]`] == repeatedIDs[`query["id"][1]`] {
		t.Fatal("repeated query occurrences share a parameter ID")
	}
}

func buildQueryOccurrenceBundle(t *testing.T, requestID, target string) (Record, string) {
	return buildHistoricalOccurrenceBundle(t, requestID, "GET", target,
		"GET "+target+" HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
}

func buildHistoricalOccurrenceBundle(t *testing.T, requestID, method, target, requestRaw string) (Record, string) {
	t.Helper()
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	status := 200
	truncated := false
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: project, RequestID: requestID},
		RequestRaw:           []byte(requestRaw),
		ResponseRaw:          []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		HasResponse:          true,
		Method:               method,
		Scheme:               "https",
		Host:                 "fixture.invalid",
		Port:                 443,
		Target:               target,
		ResponseStatus:       &status,
		RequestTruncated:     &truncated,
		ResponseTruncated:    &truncated,
		RequestWireFidelity:  "reconstructed_text",
		ResponseWireFidelity: "reconstructed_text",
	}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatalf("project storage key: %v", err)
	}
	source := Record{
		"path":           "/synthetic/receipt.json",
		"sha256":         strings.Repeat("a", 64),
		"record_locator": "$.records[0]",
	}
	bundle, _, err := BuildHistoricalBundle(exchange, projectKey, source)
	if err != nil {
		t.Fatalf("build historical bundle: %v", err)
	}
	var endpointID string
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "endpoint" {
			continue
		}
		endpointID, _ = entity["id"].(string)
	}
	if endpointID == "" {
		t.Fatal("historical bundle has no endpoint entity")
	}
	return bundle, endpointID
}

func queryParameterPaths(bundle Record) []string {
	return parameterPathsWithPrefix(bundle, "query[")
}

func parameterPathsWithPrefix(bundle Record, prefix string) []string {
	return parameterPathsByLocationAndPrefix(bundle, "", prefix)
}

func parameterPathsByLocation(bundle Record, location string) []string {
	return parameterPathsByLocationAndPrefix(bundle, location, "")
}

func parameterPathsByLocationAndPrefix(bundle Record, location, prefix string) []string {
	var paths []string
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "parameter" {
			continue
		}
		data, _ := entity["data"].(Record)
		path, _ := data["path"].(string)
		fieldLocation, _ := data["location"].(string)
		if (location == "" || fieldLocation == location) && strings.HasPrefix(path, prefix) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func queryParameterIDsByPath(bundle Record) map[string]string {
	return parameterIDsByPath(bundle, "query[")
}

func parameterIDsByPath(bundle Record, prefix string) map[string]string {
	return parameterIDsByLocationAndPrefix(bundle, "", prefix)
}

func parameterIDsByLocation(bundle Record, location string) map[string]string {
	return parameterIDsByLocationAndPrefix(bundle, location, "")
}

func parameterIDsByLocationAndPrefix(bundle Record, location, prefix string) map[string]string {
	ids := make(map[string]string)
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "parameter" {
			continue
		}
		data, _ := entity["data"].(Record)
		path, _ := data["path"].(string)
		fieldLocation, _ := data["location"].(string)
		if (location == "" || fieldLocation == location) && strings.HasPrefix(path, prefix) {
			id, _ := entity["id"].(string)
			ids[path] = id
		}
	}
	return ids
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
