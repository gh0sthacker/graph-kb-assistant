package graph

import (
	"bytes"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestNavigationURLReferencesUseStablePrivateProjectKeysAndExactSources(t *testing.T) {
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	program, _ := project.StorageKey()
	key := bytes.Repeat([]byte{0x73}, 32)
	exchange := navigationExchange(project, "reference-source", "https://app.invalid/dir/start?api_key=fixture-private-url", "", "302 Found", "next?item=fixture-private-item#fragment")
	bundle, observationID, err := BuildHistoricalBundle(exchange, program, Record{
		"path": "/synthetic/url-reference.json", "sha256": strings.Repeat("a", 64), "record_locator": "$",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddNavigationURLReferences(bundle, exchange, observationID, key); err != nil {
		t.Fatal(err)
	}
	first, _ := canonicalJSON(bundle)
	if err := AddNavigationURLReferences(bundle, exchange, observationID, key); err != nil {
		t.Fatal(err)
	}
	repeated, _ := canonicalJSON(bundle)
	if !bytes.Equal(first, repeated) {
		t.Fatal("repeating URL reference projection changed its immutable bundle")
	}
	if strings.Contains(string(first), "fixture-private-url") || strings.Contains(string(first), "fixture-private-item") {
		t.Fatal("URL reference projection retained a raw URL value")
	}
	requestKey, _, redirectKey, err := NavigationURLIndexKeys(exchange, key)
	expectedRedirectKey, expectedErr := URLIndexKey("https://app.invalid/dir/next?item=fixture-private-item", "", key)
	if err != nil || expectedErr != nil || redirectKey != expectedRedirectKey {
		t.Fatalf("path-relative redirect key did not resolve against its request URL: err=%v expected_err=%v", err, expectedErr)
	}
	urlIDs := make(map[string]string)
	for _, entity := range bundle["entities"].([]Record) {
		if entity["kind"] != "url_reference" {
			continue
		}
		data, _ := asRecord(entity["data"])
		urlIDs[data["equality_signature"].(string)] = entity["id"].(string)
	}
	if len(urlIDs) != 2 || urlIDs[requestKey] == "" || urlIDs[redirectKey] == "" {
		t.Fatal("request and redirect URL references were not projected separately")
	}
	for _, assertion := range bundle["assertions"].([]Record) {
		if assertion["predicate"] != "uses_request_url" && assertion["predicate"] != "redirect_url_matches" {
			continue
		}
		context, _ := asRecord(assertion["context"])
		if context["link_basis"] != "project_keyed_url_equality" || context["causality"] != "url_match_only" || len(assertion["source_refs"].([]Record)) != 1 {
			t.Fatal("URL reference lost its exact source or equality-only interpretation")
		}
	}
	decoded, err := DecodeBundleJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Writer{program: program}).normalizeBundle(decoded); err != nil {
		t.Fatalf("writer rejected URL reference graph projection: %v", err)
	}
	foreign := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "foreign-project"}
	foreignProgram, _ := foreign.StorageKey()
	foreignExchange := exchange
	foreignExchange.Ref.Project = foreign
	foreignBundle, foreignObservationID, err := BuildHistoricalBundle(foreignExchange, foreignProgram, Record{
		"path": "/synthetic/foreign-url-reference.json", "sha256": strings.Repeat("b", 64), "record_locator": "$",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddNavigationURLReferences(foreignBundle, foreignExchange, foreignObservationID, key); err != nil {
		t.Fatal(err)
	}
	for _, entity := range foreignBundle["entities"].([]Record) {
		if entity["kind"] == "url_reference" && (entity["id"] == urlIDs[requestKey] || entity["id"] == urlIDs[redirectKey]) {
			t.Fatal("URL reference identity crossed project namespaces")
		}
	}
	if err := AddNavigationURLReferences(bundle, foreignExchange, observationID, key); err == nil {
		t.Fatal("foreign exchange was accepted into another project's URL graph")
	}
}

func TestNavigationHeaderReferencesResolveRelativePathsAndRejectMismatchedStatus(t *testing.T) {
	exchange := navigationExchange(caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"},
		"relative-reference", "https://app.invalid/dir/start?old=1", "", "302 Found", "next")
	for value, want := range map[string]string{
		"next": "https://app.invalid/dir/next", "../end": "https://app.invalid/end",
		"?new=2": "https://app.invalid/dir/start?new=2", "/end#fragment": "https://app.invalid/end",
		"//other.invalid/next": "https://other.invalid/next",
	} {
		if got, ok := resolveNavigationReference(value, exchange); !ok || got != want {
			t.Errorf("reference %q resolved to %q (%t); want %q", value, got, ok, want)
		}
	}
	for _, value := range []string{"https://user:password@app.invalid/end", "https://app.invalid/%zz", "file:///tmp/path"} {
		if _, ok := resolveNavigationReference(value, exchange); ok {
			t.Errorf("invalid reference %q was accepted", value)
		}
	}
	exchange.ResponseStatusMetadataMismatch = true
	_, _, redirectKey, err := NavigationURLIndexKeys(exchange, bytes.Repeat([]byte{0x73}, 32))
	if err != nil || redirectKey != "" {
		t.Fatalf("unresolved response status created a redirect reference: key=%q err=%v", redirectKey, err)
	}
}
