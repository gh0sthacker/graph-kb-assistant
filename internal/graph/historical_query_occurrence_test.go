package graph

import (
	"encoding/json"
	"reflect"
	"sort"
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
	t.Helper()
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	status := 200
	truncated := false
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: project, RequestID: requestID},
		RequestRaw:           []byte("GET " + target + " HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"),
		ResponseRaw:          []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		HasResponse:          true,
		Method:               "GET",
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
	var paths []string
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "parameter" {
			continue
		}
		data, _ := entity["data"].(Record)
		path, _ := data["path"].(string)
		if strings.HasPrefix(path, "query[") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func queryParameterIDsByPath(bundle Record) map[string]string {
	ids := make(map[string]string)
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "parameter" {
			continue
		}
		data, _ := entity["data"].(Record)
		path, _ := data["path"].(string)
		if strings.HasPrefix(path, "query[") {
			id, _ := entity["id"].(string)
			ids[path] = id
		}
	}
	return ids
}
