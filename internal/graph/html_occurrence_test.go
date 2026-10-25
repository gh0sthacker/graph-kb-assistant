package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestHistoricalHTMLResponseControlsHaveRawTokenOccurrences(t *testing.T) {
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	const body = `<html><body><form action="/search"><input name="q" type="text" value="HTML_VALUE_NEVER_STORED"><input name="q" type="text" value="HTML_SECOND_VALUE_NEVER_STORED"><select name="kind"><option value="HTML_OPTION_NEVER_STORED">A</option></select><textarea name="note">HTML_TEXTAREA_NEVER_STORED</textarea><button name="submit" value="HTML_BUTTON_NEVER_STORED">Go</button><input name="password" type="password" value="HTML_PASSWORD_NEVER_STORED"><input name="password" type="password" value="HTML_SECOND_PASSWORD_NEVER_STORED"><a href="/help">Help</a></form></body></html>`
	request := "GET /search HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Length: 0\r\n\r\n"
	response := "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
	bundle := buildHTMLResponseBundle(t, "html-controls", request, response, fieldKey, false)
	validateJSONEntities(t, bundle)

	wantTags := map[string]bool{
		`<input name="q" type="text" value="HTML_VALUE_NEVER_STORED">`:        false,
		`<input name="q" type="text" value="HTML_SECOND_VALUE_NEVER_STORED">`: false,
		`<select name="kind">`:                                    false,
		`<textarea name="note">`:                                  false,
		`<button name="submit" value="HTML_BUTTON_NEVER_STORED">`: false,
		`<input name="password" type="password" value="HTML_PASSWORD_NEVER_STORED">`:        false,
		`<input name="password" type="password" value="HTML_SECOND_PASSWORD_NEVER_STORED">`: false,
	}
	wantIndexes := map[string]int{
		`<input name="q" type="text" value="HTML_VALUE_NEVER_STORED">`:        0,
		`<input name="q" type="text" value="HTML_SECOND_VALUE_NEVER_STORED">`: 1,
		`<select name="kind">`:                                    0,
		`<textarea name="note">`:                                  0,
		`<button name="submit" value="HTML_BUTTON_NEVER_STORED">`: 0,
		`<input name="password" type="password" value="HTML_PASSWORD_NEVER_STORED">`:        0,
		`<input name="password" type="password" value="HTML_SECOND_PASSWORD_NEVER_STORED">`: 1,
	}
	occurrences := entitiesByKind(bundle, "field_occurrence")
	if len(occurrences) != len(wantTags) {
		t.Fatalf("HTML field occurrences = %d, want %d control tags", len(occurrences), len(wantTags))
	}
	for _, entity := range occurrences {
		data := entity["data"].(Record)
		if data["side"] != "response" || data["location"] != "response.body" {
			t.Fatalf("HTML control occurrence has wrong side/location: %#v", data)
		}
		if data["value_span"] != nil {
			t.Fatalf("HTML control occurrence must not index attribute values: %#v", data["value_span"])
		}
		span := data["source_span"].(Record)
		start, end := span["start"].(int), span["end"].(int)
		if span["offset_basis"] != "response_message" || span["unit"] != "byte" || start < 0 || end <= start || end > len(response) {
			t.Fatalf("invalid HTML raw-token span: %#v", span)
		}
		rawTag := response[start:end]
		if _, ok := wantTags[string(rawTag)]; !ok {
			t.Fatalf("HTML occurrence span is not one exact control start tag: %q", rawTag)
		}
		wantTags[string(rawTag)] = true
		if got, want := data["occurrence_index"].(int), wantIndexes[string(rawTag)]; got != want {
			t.Errorf("HTML control occurrence index for %q = %d, want %d", rawTag, got, want)
		}
		if strings.Contains(strings.ToLower(data["path"].(string)), "password") {
			t.Fatalf("sensitive HTML field name was exposed: %q", data["path"])
		}
	}
	for tag, seen := range wantTags {
		if !seen {
			t.Errorf("missing exact HTML control token span %q", tag)
		}
	}
	for _, entity := range entitiesByKind(bundle, "parameter") {
		path := entity["data"].(Record)["path"].(string)
		if strings.HasPrefix(path, "html.forms[") || strings.HasPrefix(path, "html.links[") {
			for _, occurrence := range occurrences {
				if occurrence["data"].(Record)["path"] == path {
					t.Fatalf("form/link structural field unexpectedly received a control occurrence: %q", path)
				}
			}
		}
	}
	if hasBodyOccurrenceGap(bundle) {
		t.Fatal("complete HTML response controls reported an occurrence coverage gap")
	}
	responseControl := occurrences[0]["data"].(Record)
	requestData := make(map[string]any, len(responseControl))
	for key, value := range responseControl {
		requestData[key] = value
	}
	requestData["side"], requestData["location"] = "request", "request.body"
	requestSpan := make(map[string]any)
	for key, value := range responseControl["source_span"].(Record) {
		requestSpan[key] = value
	}
	requestSpan["offset_basis"] = "request_message"
	requestData["source_span"] = requestSpan
	if err := validateEntityData("field_occurrence", requestData, bundle["program"].(string)); err == nil {
		t.Fatal("reader accepted a response-only HTML control type on a request occurrence")
	}
	unknownType := make(map[string]any, len(responseControl))
	for key, value := range responseControl {
		unknownType[key] = value
	}
	unknownType["type"] = "input:arbitrary"
	unknownSpan := make(map[string]any)
	for key, value := range responseControl["source_span"].(Record) {
		unknownSpan[key] = value
	}
	unknownType["source_span"] = unknownSpan
	if err := validateEntityData("field_occurrence", unknownType, bundle["program"].(string)); err == nil {
		t.Fatal("reader accepted an unrecognized HTML control type")
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"HTML_VALUE_NEVER_STORED", "HTML_SECOND_VALUE_NEVER_STORED", "HTML_OPTION_NEVER_STORED", "HTML_TEXTAREA_NEVER_STORED",
		"HTML_BUTTON_NEVER_STORED", "HTML_PASSWORD_NEVER_STORED", "HTML_SECOND_PASSWORD_NEVER_STORED", `name="password"`,
	} {
		if bytes.Contains(encoded, []byte(raw)) {
			t.Fatalf("HTML graph contains raw attribute or control value %q", raw)
		}
	}
}

func TestHistoricalHTMLControlOccurrencesRequireCompleteResponseSource(t *testing.T) {
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	const body = `<html><input name="search" value="HTML_VALUE_NEVER_STORED"></html>`
	request := "POST /upload HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: text/html\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
	response := "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body

	requestBundle := buildBodyFieldBundle(t, "html-request-controls", request, emptyHTTPResponse(), fieldKey, false)
	for _, entity := range entitiesByKind(requestBundle, "field_occurrence") {
		if entity["data"].(Record)["location"] == "request.body" {
			t.Fatal("request HTML control received a response-only occurrence")
		}
	}
	if !hasBodyOccurrenceGap(requestBundle) {
		t.Fatal("unbound request HTML control did not report its coverage gap")
	}

	truncatedBundle := buildHTMLResponseBundle(t, "html-truncated", request, response, fieldKey, true)
	if len(entitiesByKind(truncatedBundle, "field_occurrence")) != 0 || !hasBodyOccurrenceGap(truncatedBundle) {
		t.Fatal("truncated HTML response was not kept structural-only with a coverage gap")
	}
	invalidExtent := strings.Replace(response, "Content-Length: "+strconv.Itoa(len(body)), "Content-Length: 1", 1)
	invalidExtentBundle := buildBodyFieldBundle(t, "html-invalid-extent", request, invalidExtent, fieldKey, false)
	if len(entitiesByKind(invalidExtentBundle, "field_occurrence")) != 0 || !hasBodyOccurrenceGap(invalidExtentBundle) {
		t.Fatal("HTML response with mismatched extent produced an occurrence or lost its coverage gap")
	}

	unsupportedCharset := strings.Replace(response, "charset=utf-8", "charset=unsupported", 1)
	unsupportedBundle := buildBodyFieldBundle(t, "html-unsupported-charset", request, unsupportedCharset, fieldKey, false)
	if len(entitiesByKind(unsupportedBundle, "field_occurrence")) != 0 || !hasBodyOccurrenceGap(unsupportedBundle) {
		t.Fatal("unsupported HTML charset produced an occurrence or lost its coverage gap")
	}
}

func buildHTMLResponseBundle(t *testing.T, requestID, requestRaw, responseRaw string, fieldKey []byte, responseTruncated bool) Record {
	t.Helper()
	exchange, projectKey, source, receiptRoot := syntheticBodyFixture(t, requestID, requestRaw, responseRaw, false)
	truncated := responseTruncated
	exchange.ResponseTruncated = &truncated
	responseSignature := strings.Repeat("e", 64)
	bundle, _, err := BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, projectKey, source,
		strings.Repeat("b", 64), &responseSignature, fieldKey, strings.Repeat("d", 64))
	if err != nil {
		t.Fatalf("build synthetic HTML graph bundle: %v", err)
	}
	validateJSONEntities(t, bundle)
	writer := &Writer{program: projectKey, graphPath: filepath.Join(t.TempDir(), "html.sqlite3"), receiptRoot: receiptRoot}
	if _, err := writer.ImportBundle(context.Background(), bundle); err != nil {
		t.Fatalf("import synthetic HTML graph bundle: %v", err)
	}
	return bundle
}
