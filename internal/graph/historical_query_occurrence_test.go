package graph

import (
	"encoding/json"
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
