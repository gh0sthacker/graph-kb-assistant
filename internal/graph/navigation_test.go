package graph

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestAddPageNavigationAssertionsLinksExactRefererAndRedirectEvidence(t *testing.T) {
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	privateQuery := "synthetic-secret"
	startURL := "https://app.invalid/start?api_key=" + privateQuery
	start := navigationExchange(project, "request-start", startURL, "", "302 Found", "/next")
	next := navigationExchange(project, "request-next", "https://app.invalid/next", startURL, "200 OK", "")
	exchanges := []caido.HttpExchange{start, next}
	bundles := make([]Record, 0, len(exchanges))
	for index, exchange := range exchanges {
		bundle, _, buildErr := BuildHistoricalBundle(exchange, projectKey, Record{
			"path": fmt.Sprintf("/synthetic/%d.json", index), "sha256": strings.Repeat(string(rune('a'+index)), 64),
			"record_locator": fmt.Sprintf("$.records[%d]", index),
		})
		if buildErr != nil {
			t.Fatalf("BuildHistoricalBundle(%d): %v", index, buildErr)
		}
		bundles = append(bundles, bundle)
	}
	if err := AddPageNavigationAssertions(bundles, exchanges); err != nil {
		t.Fatalf("AddPageNavigationAssertions(): %v", err)
	}
	observationIDs := make([]string, 2)
	for index, bundle := range bundles {
		for _, entity := range bundle["entities"].([]Record) {
			if entity["kind"] == "observation" {
				observationIDs[index] = entity["id"].(string)
			}
		}
	}
	assertions := bundles[1]["assertions"].([]Record)
	for _, bundle := range bundles {
		if strings.Contains(fmt.Sprint(bundle), privateQuery) {
			t.Fatal("matched URL value was persisted in the graph bundle")
		}
		writer := &Writer{program: projectKey, receiptRoot: t.TempDir()}
		encoded, err := marshalBoundedJSON(bundle, MaxBundleJSONBytes)
		if err != nil {
			t.Fatalf("encode navigation bundle: %v", err)
		}
		decoded, err := DecodeBundleJSON(encoded)
		if err != nil {
			t.Fatalf("decode navigation bundle: %v", err)
		}
		if _, err := writer.normalizeBundle(decoded); err != nil {
			t.Fatalf("graph writer rejected navigation bundle: %v", err)
		}
	}
	links := make(map[string]Record)
	for _, assertion := range assertions {
		predicate, _ := assertion["predicate"].(string)
		if predicate == "referrer_context_for" || predicate == "redirects_to" {
			links[predicate] = assertion
		}
	}
	for predicate, basis := range map[string]string{
		"redirects_to": "redirect_exact_url", "referrer_context_for": "referrer_exact_url",
	} {
		link, ok := links[predicate]
		if !ok {
			t.Fatalf("missing %s assertion", predicate)
		}
		if link["subject"] != observationIDs[0] || link["object"] != observationIDs[1] {
			t.Fatalf("%s endpoints = %v -> %v, want predecessor -> current", predicate, link["subject"], link["object"])
		}
		context := link["context"].(Record)
		if context["link_basis"] != basis || context["causality"] != "navigation_evidence_only" {
			t.Fatalf("%s context = %#v", predicate, context)
		}
		if len(link["source_refs"].([]Record)) != 2 {
			t.Fatalf("%s source references = %d, want both exchange receipts", predicate, len(link["source_refs"].([]Record)))
		}
	}
	for _, assertion := range assertions {
		if assertion["predicate"] == "precedes" {
			t.Fatal("navigation evidence must not be promoted to a precedes assertion")
		}
	}
	payload, err := marshalBoundedJSON(bundles[1], MaxBundleJSONBytes)
	if err != nil {
		t.Fatalf("encode page navigation bundle: %v", err)
	}
	if _, _, err := validateProjectionBundleWithRecord(payload, projectKey); err != nil {
		t.Fatalf("navigation bundle failed projection validation: %v", err)
	}
	var captureID string
	for _, entity := range bundles[1]["entities"].([]Record) {
		if entity["id"] == observationIDs[1] {
			data, _ := asRecord(entity["data"])
			captureID, _ = data["capture_id"].(string)
		}
	}
	projectedObservationID, err := ProjectionObservationID(payload, projectKey, captureID)
	if err != nil || projectedObservationID != observationIDs[1] {
		t.Fatalf("projection job did not retain its owning observation with a linked endpoint: %q, %v", projectedObservationID, err)
	}
	requestKey, refererKey, redirectKey, err := NavigationURLIndexKeys(next, bytes.Repeat([]byte{0x5a}, 32))
	if err != nil || len(requestKey) != 64 || len(refererKey) != 64 || redirectKey != "" {
		t.Fatalf("NavigationURLIndexKeys() = %q, %q, %q, %v", requestKey, refererKey, redirectKey, err)
	}
	if strings.Contains(requestKey+refererKey+redirectKey, privateQuery) {
		t.Fatal("navigation URL index keys contain a raw query value")
	}
}

func TestAddPageNavigationAssertionsSkipsAmbiguousAndMalformedEvidence(t *testing.T) {
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	first := navigationExchange(project, "request-first", "https://app.invalid/start", "", "302 Found", "/next")
	duplicate := navigationExchange(project, "request-duplicate", "https://app.invalid/start", "", "200 OK", "")
	current := navigationExchange(project, "request-current", "https://app.invalid/next", "https://app.invalid/start", "200 OK", "")
	exchanges := []caido.HttpExchange{first, duplicate, current}
	bundles := make([]Record, 0, len(exchanges))
	for index, exchange := range exchanges {
		bundle, _, buildErr := BuildHistoricalBundle(exchange, projectKey, Record{
			"path": fmt.Sprintf("/synthetic/%d.json", index), "sha256": strings.Repeat("b", 64),
			"record_locator": fmt.Sprintf("$.records[%d]", index),
		})
		if buildErr != nil {
			t.Fatalf("BuildHistoricalBundle(%d): %v", index, buildErr)
		}
		bundles = append(bundles, bundle)
	}
	if err := AddPageNavigationAssertions(bundles, exchanges); err != nil {
		t.Fatalf("AddPageNavigationAssertions(): %v", err)
	}
	referrerLinks := 0
	redirectLinks := 0
	for _, bundle := range bundles {
		for _, assertion := range bundle["assertions"].([]Record) {
			if assertion["predicate"] == "referrer_context_for" {
				referrerLinks++
			}
			if assertion["predicate"] == "redirects_to" {
				redirectLinks++
			}
		}
	}
	if referrerLinks != 0 || redirectLinks != 1 {
		t.Fatalf("ambiguous referer and unique redirect links = %d/%d, want 0/1", referrerLinks, redirectLinks)
	}

	bad := navigationExchange(project, "request-bad", "https://app.invalid/other", "https://user:password@app.invalid/start", "200 OK", "")
	badBundle, _, err := BuildHistoricalBundle(bad, projectKey, Record{
		"path": "/synthetic/bad.json", "sha256": strings.Repeat("c", 64), "record_locator": "$.records[0]",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddPageNavigationAssertions([]Record{badBundle}, []caido.HttpExchange{bad}); err != nil {
		t.Fatalf("malformed link evidence should be ignored safely: %v", err)
	}
	for _, assertion := range badBundle["assertions"].([]Record) {
		if assertion["predicate"] == "referrer_context_for" || assertion["predicate"] == "redirects_to" {
			t.Fatal("credential-bearing Referer must not create a navigation assertion")
		}
	}
}

func TestAddPageNavigationAssertionsSkipsInvalidRequestURLWithoutFailingImport(t *testing.T) {
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	exchange := navigationExchange(project, "request-invalid-url", "https://app.invalid/path", "", "200 OK", "")
	bundle, _, err := BuildHistoricalBundle(exchange, projectKey, Record{
		"path": "/synthetic/invalid-url.json", "sha256": strings.Repeat("d", 64), "record_locator": "$.records[0]",
	})
	if err != nil {
		t.Fatalf("BuildHistoricalBundle(): %v", err)
	}
	exchange.Target = "https://[invalid-host/path"
	if err := AddPageNavigationAssertions([]Record{bundle}, []caido.HttpExchange{exchange}); err != nil {
		t.Fatalf("invalid navigation URL should not fail capture import: %v", err)
	}
	for _, assertion := range bundle["assertions"].([]Record) {
		if assertion["predicate"] == "referrer_context_for" || assertion["predicate"] == "redirects_to" {
			t.Fatal("invalid request URL produced a navigation assertion")
		}
	}
}

func TestResolveNavigationURLRejectsMalformedURLsAndNormalizesSafeOriginParts(t *testing.T) {
	exchange := navigationExchange(
		caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"},
		"request-url", "https://APP.invalid:443/start", "", "200 OK", "",
	)
	got, ok := resolveNavigationURL("/next#fragment", exchange)
	if !ok || got != "https://app.invalid/next" {
		t.Fatalf("resolveNavigationURL(relative) = %q, %v; want canonical HTTPS URL", got, ok)
	}
	if _, ok := resolveNavigationURL("https://user:pass@app.invalid/next", exchange); ok {
		t.Fatal("credential-bearing URL was accepted as navigation evidence")
	}
	if _, ok := resolveNavigationURL("https://app.invalid/%zz", exchange); ok {
		t.Fatal("malformed percent escape was normalized into an exact URL")
	}
	ipv6 := exchange
	ipv6.Host = "2001:db8::1"
	ipv6.Target = "https://[2001:db8::1]/"
	if got, ok := resolveNavigationURL("https://[2001:db8::1]/next", ipv6); !ok || got != "https://[2001:db8::1]/next" {
		t.Fatalf("resolveNavigationURL(IPv6) = %q, %v; want bracketed IPv6 URL", got, ok)
	}
}

func navigationExchange(project caido.ProjectRef, requestID, target, referer, responseStatusLine, location string) caido.HttpExchange {
	request := "GET / HTTP/1.1\r\nHost: app.invalid\r\n"
	if target != "" {
		parsed, _ := url.Parse(target)
		request = "GET " + parsed.RequestURI() + " HTTP/1.1\r\nHost: " + parsed.Host + "\r\n"
	}
	if referer != "" {
		request += "Referer: " + referer + "\r\n"
	}
	request += "\r\n"
	response := "HTTP/1.1 " + responseStatusLine + "\r\n"
	if location != "" {
		response += "Location: " + location + "\r\n"
	}
	response += "\r\n"
	status := 200
	if strings.HasPrefix(responseStatusLine, "302") {
		status = 302
	}
	requestExtent, responseExtent := false, false
	return caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: project, RequestID: requestID},
		RequestRaw: []byte(request), ResponseRaw: []byte(response), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "app.invalid", Port: 443, Target: target,
		ResponseStatus: &status, RequestTruncated: &requestExtent, ResponseTruncated: &responseExtent,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
}
