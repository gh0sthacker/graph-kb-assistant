package graph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
)

func TestHistoricalJSONBodyOccurrencesCoverRequestAndResponseWithoutValues(t *testing.T) {
	const shared = "BODY_SHARED_VALUE_724"
	const secret = "BODY_SECRET_VALUE_912"
	requestRaw := "POST /api/items?seen=" + shared + "&label=BODY_ESCAPED_VALUE HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/json\r\n\r\n" +
		`{ "id" : "` + shared + `", "label" : "BODY_ESCAPED\u005fVALUE", "items" : [ "BODY_ARRAY_A_142", "BODY_ARRAY_B_593" ], "count" : 2, "active" : true, "password" : "` + secret + `" }`
	responseRaw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" +
		`{"result":{"id":"` + shared + `"},"password":"` + secret + `"}`
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	bundle := buildJSONBodyFieldBundle(t, "body-fields", requestRaw, responseRaw, fieldKey, false)
	validateJSONEntities(t, bundle)

	occurrences := entitiesByKind(bundle, "field_occurrence")
	if len(occurrences) != 11 {
		t.Fatalf("field occurrences = %d, want 2 query plus 9 scalar JSON values across request and response", len(occurrences))
	}
	seen := map[string]bool{}
	for _, entity := range occurrences {
		data := entity["data"].(Record)
		side, location := data["side"].(string), data["location"].(string)
		message := requestRaw
		wantBasis := "request_message"
		if side == "response" {
			message, wantBasis = responseRaw, "response_message"
		}
		if location != side+".body" && !(side == "request" && location == "query") {
			t.Fatalf("body occurrence location/side = %q/%q", location, side)
		}
		span := data["value_span"].(Record)
		start, end := span["start"].(int), span["end"].(int)
		if span["offset_basis"] != wantBasis || span["unit"] != "byte" || start < 0 || end <= start || end > len(message) {
			t.Fatalf("invalid %s body value span: %#v", side, span)
		}
		value := string(message[start:end])
		if location != "query" && data["type"] == "string" && !strings.HasPrefix(value, `"`) {
			t.Fatalf("string span does not include its JSON token: %q", value)
		}
		if location != "query" {
			sourceSpan := data["source_span"].(Record)
			if sourceSpan["start"] != span["start"] || sourceSpan["end"] != span["end"] {
				t.Fatalf("body source/value spans differ: source=%#v value=%#v", sourceSpan, span)
			}
			if data["path"] == `$["label"]` && value != `"BODY_ESCAPED\u005fVALUE"` {
				t.Fatalf("escaped JSON source span = %q, want original encoded token", value)
			}
		}
		seen[side+":"+data["path"].(string)] = true
	}
	for _, path := range []string{
		`request:query["seen"][]`, `request:query["label"][]`,
		`request:$["id"]`, `request:$["label"]`, `request:$["items"][0]`, `request:$["items"][1]`,
		`request:$["count"]`, `request:$["active"]`, `request:$["password"]`,
		`response:$["result"]["id"]`, `response:$["password"]`,
	} {
		if !seen[path] {
			t.Errorf("missing body field occurrence %q; seen=%v", path, seen)
		}
	}

	refs := entitiesByKind(bundle, "value_reference")
	if len(refs) != 4 {
		t.Fatalf("project-keyed JSON value references = %d, want 4 distinct non-sensitive strings", len(refs))
	}
	for _, ref := range refs {
		data := ref["data"].(Record)
		if data["token_version"] != "field-equality-v2" || data["normalization"] != "decoded_utf8_scalar_v2" {
			t.Fatalf("scalar string reference has wrong normalization: %#v", data)
		}
	}
	var sharedReference string
	var escapedLabelReference string
	for _, assertion := range bundle["assertions"].([]Record) {
		if assertion["predicate"] != "value_used_in" {
			continue
		}
		occurrence := fieldOccurrenceEntity(t, bundle, assertion["object"].(string))
		data := occurrence["data"].(Record)
		span := data["value_span"].(Record)
		message := requestRaw
		if data["side"] == "response" {
			message = responseRaw
		}
		start, end := span["start"].(int), span["end"].(int)
		rawValue := string(message[start:end])
		if rawValue == `"`+shared+`"` || rawValue == shared {
			if sharedReference != "" && sharedReference != assertion["subject"] {
				t.Fatal("same JSON string did not share one project-keyed reference across request and response")
			}
			sharedReference = assertion["subject"].(string)
		}
		if data["path"] == `query["label"][]` || data["path"] == `$["label"]` {
			if escapedLabelReference != "" && escapedLabelReference != assertion["subject"] {
				t.Fatal("URL-decoded and JSON-escaped equivalent strings did not share a value reference")
			}
			escapedLabelReference = assertion["subject"].(string)
		}
	}
	if sharedReference == "" {
		t.Fatal("shared request/response JSON value has no keyed equality link")
	}
	if escapedLabelReference == "" {
		t.Fatal("equivalent query and JSON-escaped strings have no shared value reference")
	}

	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{shared, "BODY_ARRAY_A_142", "BODY_ARRAY_B_593", "BODY_ESCAPED_VALUE", secret} {
		if bytes.Contains(encoded, []byte(raw)) {
			t.Fatalf("graph bundle contains raw JSON value %q", raw)
		}
	}
}

func TestHistoricalJSONBodyOccurrencesRequireCompleteBoundedMessages(t *testing.T) {
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	request := "POST /api/items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/json\r\n\r\n{\"id\":\"TRUNCATED_BODY_VALUE_216\"}"
	response := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"id\":\"RESPONSE_BODY_VALUE_315\"}"
	bundle := buildJSONBodyFieldBundle(t, "body-truncated", request, response, fieldKey, true)
	for _, entity := range entitiesByKind(bundle, "field_occurrence") {
		if entity["data"].(Record)["side"] != "response" {
			t.Fatal("truncated request body produced a source-bound occurrence")
		}
	}

	malformed := "POST /api/items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/json\r\n\r\n{\"id\":\"MALFORMED_BODY_VALUE_827\""
	malformedBundle := buildJSONBodyFieldBundle(t, "body-malformed", malformed, response, fieldKey, false)
	for _, entity := range entitiesByKind(malformedBundle, "field_occurrence") {
		if entity["data"].(Record)["side"] == "request" {
			t.Fatal("malformed request JSON produced a source-bound occurrence")
		}
	}

	lengthMismatch := strings.Replace(request, "\r\n\r\n", "\r\nContent-Length: 999\r\n\r\n", 1)
	lengthMismatchBundle := buildJSONBodyFieldBundle(t, "body-length-mismatch", lengthMismatch, response, fieldKey, false)
	for _, entity := range entitiesByKind(lengthMismatchBundle, "field_occurrence") {
		if entity["data"].(Record)["side"] == "request" {
			t.Fatal("JSON body with a mismatched Content-Length produced source-bound occurrences")
		}
	}
}

func TestLiveJSONBodyOccurrencesUseTheSharedHistoricalProjection(t *testing.T) {
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	request := "POST /api/items HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: application/json\r\n\r\n{\"id\":\"LIVE_REQUEST_BODY_VALUE_315\"}"
	response := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"id\":\"LIVE_RESPONSE_BODY_VALUE_729\"}"
	exchange, projectKey, source, receiptRoot := syntheticJSONBodyFixture(t, "body-live", request, response, false)
	responseSignature := strings.Repeat("f", 64)
	bundle, _, err := BuildLiveBundleWithSignaturesAndFieldValueKey(
		exchange, projectKey, source, strings.Repeat("a", 64), &responseSignature, fieldKey, strings.Repeat("c", 64),
	)
	if err != nil {
		t.Fatalf("build synthetic live JSON body bundle: %v", err)
	}
	seenSides := map[string]bool{}
	for _, entity := range entitiesByKind(bundle, "field_occurrence") {
		seenSides[entity["data"].(Record)["side"].(string)] = true
	}
	if !seenSides["request"] || !seenSides["response"] {
		t.Fatalf("live projection omitted request/response occurrences: %v", seenSides)
	}
	for _, entity := range entitiesByKind(bundle, "observation") {
		if entity["data"].(Record)["observation_kind"] != "live_capture" {
			t.Fatalf("live observation kind = %v", entity["data"].(Record)["observation_kind"])
		}
	}
	writer := &Writer{program: projectKey, graphPath: filepath.Join(t.TempDir(), "live.sqlite3"), receiptRoot: receiptRoot}
	if _, err := writer.ImportBundle(context.Background(), bundle); err != nil {
		t.Fatalf("import synthetic live JSON body bundle: %v", err)
	}
}

func buildJSONBodyFieldBundle(t *testing.T, requestID, requestRaw, responseRaw string, fieldKey []byte, requestTruncated bool) Record {
	t.Helper()
	exchange, projectKey, source, receiptRoot := syntheticJSONBodyFixture(t, requestID, requestRaw, responseRaw, requestTruncated)
	responseSignature := strings.Repeat("e", 64)
	bundle, _, err := BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, projectKey, source,
		strings.Repeat("b", 64), &responseSignature, fieldKey, strings.Repeat("d", 64))
	if err != nil {
		t.Fatalf("build JSON body graph bundle: %v", err)
	}
	writer := &Writer{program: projectKey, graphPath: filepath.Join(t.TempDir(), "behavior.sqlite3"), receiptRoot: receiptRoot}
	if _, err := writer.ImportBundle(context.Background(), bundle); err != nil {
		t.Fatalf("import synthetic JSON body graph bundle: %v", err)
	}
	return bundle
}

func syntheticJSONBodyFixture(t *testing.T, requestID, requestRaw, responseRaw string, requestTruncated bool) (caido.HttpExchange, string, Record, string) {
	t.Helper()
	project := caido.ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := project.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	status := 200
	requestTruncatedValue, responseTruncated := requestTruncated, false
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: project, RequestID: requestID},
		RequestRaw: []byte(requestRaw), ResponseRaw: []byte(responseRaw), HasResponse: true,
		Method: "POST", Scheme: "https", Host: "fixture.invalid", Port: 443,
		Target: "https://fixture.invalid" + requestTargetFromFixture(requestRaw), ResponseStatus: &status,
		RequestTruncated: &requestTruncatedValue, ResponseTruncated: &responseTruncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	root := t.TempDir()
	receiptRoot := filepath.Join(root, "receipts")
	if err := os.Mkdir(receiptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(receiptRoot, "capture.json")
	receiptBytes := []byte("synthetic-json-body-receipt-" + requestID)
	if err := os.WriteFile(receiptPath, receiptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	receiptDigest := sha256.Sum256(receiptBytes)
	source := Record{
		"path": receiptPath, "sha256": hex.EncodeToString(receiptDigest[:]), "record_locator": "$.records[0]",
	}
	return exchange, projectKey, source, receiptRoot
}
