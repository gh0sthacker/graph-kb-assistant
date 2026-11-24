package graph

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"strings"
	"testing"
)

const multipartFixtureBoundary = "BB-GRAPH-FIXTURE-BOUNDARY"

func TestMultipartBodyOccurrencesBindRepeatedSimpleTextParts(t *testing.T) {
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	const shared = "MULTIPART_SHARED_VALUE_642"
	const secret = "MULTIPART_SECRET_VALUE_813"
	request := multipartOccurrenceRequest(t, [][2]string{
		{"id", shared}, {"tag", "MULTIPART_TAG_A_417"}, {"tag", "MULTIPART_TAG_B_529"}, {"password", secret},
	})
	response := multipartResponseFixture(t, [][2]string{{"id", shared}, {"password", secret}})
	bundle := buildBodyFieldBundle(t, "multipart-body-fields", request, response, fieldKey, false)
	validateJSONEntities(t, bundle)

	occurrences := entitiesByKind(bundle, "field_occurrence")
	if len(occurrences) != 6 {
		t.Fatalf("multipart field occurrences = %d, want 6 across request and response", len(occurrences))
	}
	seenPaths := map[string]bool{}
	tagOrder := map[int]string{}
	secretOccurrences := map[string]bool{}
	sharedReference := ""
	for _, entity := range occurrences {
		data := entity["data"].(Record)
		message := request
		basis := "request_message"
		if data["side"] == "response" {
			message, basis = response, "response_message"
		}
		span := data["value_span"].(Record)
		start, end := span["start"].(int), span["end"].(int)
		if span["offset_basis"] != basis || start < 0 || end < start || end > len(message) {
			t.Fatalf("invalid multipart source span: %#v", span)
		}
		value := string(message[start:end])
		path := data["side"].(string) + ":" + data["path"].(string)
		seenPaths[path] = true
		if value == secret {
			secretOccurrences[entity["id"].(string)] = true
			if strings.Contains(strings.ToLower(data["path"].(string)), "password") {
				t.Fatalf("sensitive multipart field name was exposed in path: %q", data["path"])
			}
		}
		switch data["path"] {
		case `request.body["tag"][0]`:
			tagOrder[data["occurrence_index"].(int)] = value
		case `request.body["tag"][1]`:
			tagOrder[data["occurrence_index"].(int)] = value
		}
		if sourceSpan := data["source_span"].(Record); sourceSpan["start"] != span["start"] || sourceSpan["end"] != span["end"] {
			t.Fatalf("multipart part source/value spans differ: %#v / %#v", sourceSpan, span)
		}
	}
	for _, path := range []string{
		`request:request.body["id"][]`, `request:request.body["tag"][0]`, `request:request.body["tag"][1]`,
		`response:response.body["id"][]`,
	} {
		if !seenPaths[path] {
			t.Errorf("missing multipart occurrence %q; seen=%v", path, seenPaths)
		}
	}
	if len(secretOccurrences) != 2 {
		t.Fatalf("sensitive multipart field occurrence count = %d, want request and response occurrences", len(secretOccurrences))
	}
	if tagOrder[0] != "MULTIPART_TAG_A_417" || tagOrder[1] != "MULTIPART_TAG_B_529" {
		t.Fatalf("repeated multipart order = %#v", tagOrder)
	}
	for _, assertion := range bundle["assertions"].([]Record) {
		if assertion["predicate"] != "value_used_in" {
			continue
		}
		if secretOccurrences[assertion["object"].(string)] {
			t.Fatal("sensitive multipart field received a keyed equality link")
		}
		occurrence := fieldOccurrenceEntity(t, bundle, assertion["object"].(string))
		data := occurrence["data"].(Record)
		span := data["value_span"].(Record)
		message := request
		if data["side"] == "response" {
			message = response
		}
		start, end := span["start"].(int), span["end"].(int)
		if string(message[start:end]) == shared {
			if sharedReference != "" && sharedReference != assertion["subject"] {
				t.Fatal("same multipart string did not share a project-keyed value across captures/sides")
			}
			sharedReference = assertion["subject"].(string)
		}
		context := assertion["context"].(Record)
		if context["scenario"] != "keyed_value_equality" || context["scenario_step"] != "correlation_only" {
			t.Fatalf("multipart value link overstates its meaning: %#v", context)
		}
	}
	if sharedReference == "" {
		t.Fatal("same multipart string has no keyed equality link")
	}
	if hasBodyOccurrenceGap(bundle) {
		t.Fatal("complete simple-text multipart message reported an occurrence coverage gap")
	}
	refs := entitiesByKind(bundle, "value_reference")
	if len(refs) != 3 {
		t.Fatalf("multipart value references = %d, want 3 safe unique strings", len(refs))
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{shared, secret, "MULTIPART_TAG_A_417", "MULTIPART_TAG_B_529", "password"} {
		if bytes.Contains(encoded, []byte(raw)) {
			t.Fatalf("multipart graph contains raw field input %q", raw)
		}
	}
}

func TestMultipartFileEncodedAndNestedPartsStayStructuralWithCoverageGap(t *testing.T) {
	fieldKey := fieldValueKeyForProject(t, "fixture-project")
	defer clear(fieldKey)
	fileRequest := multipartRequestWithFilePart(t)
	fileBundle := buildBodyFieldBundle(t, "multipart-file", fileRequest, emptyHTTPResponse(), fieldKey, false)
	fileOccurrences := entitiesByKind(fileBundle, "field_occurrence")
	if len(fileOccurrences) != 1 || fileOccurrences[0]["data"].(Record)["path"] != `request.body["mode"][]` {
		t.Fatalf("file part should stay structural while simple text is spanned: %#v", fileOccurrences)
	}
	if !hasBodyOccurrenceGap(fileBundle) {
		t.Fatal("file part did not leave an explicit occurrence coverage gap")
	}
	fileJSON, err := json.Marshal(fileBundle)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(fileJSON, []byte("FILE_CONTENT_NEVER_HASH")) {
		t.Fatal("multipart file content was stored in graph records")
	}
	for _, assertion := range fileBundle["assertions"].([]Record) {
		if assertion["predicate"] != "value_used_in" {
			continue
		}
		occurrence := fieldOccurrenceEntity(t, fileBundle, assertion["object"].(string))
		if occurrence["data"].(Record)["path"] == `request.body["upload"][]` {
			t.Fatal("multipart file content received a keyed equality link")
		}
	}
	extendedFileRequest := multipartRequestWithExtendedFilenamePart()
	extendedFileBundle := buildBodyFieldBundle(t, "multipart-extended-file", extendedFileRequest, emptyHTTPResponse(), fieldKey, false)
	for _, entity := range entitiesByKind(extendedFileBundle, "field_occurrence") {
		if entity["data"].(Record)["location"] == "request.body" {
			t.Fatal("filename* multipart file produced a source-bound occurrence")
		}
	}
	extendedFileJSON, err := json.Marshal(extendedFileBundle)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(extendedFileJSON, []byte("EXTENDED_FILE_CONTENT_NEVER_HASH")) {
		t.Fatal("filename* multipart file content was stored in graph records")
	}
	if !hasBodyOccurrenceGap(extendedFileBundle) {
		t.Fatal("filename* multipart file did not leave an explicit occurrence coverage gap")
	}

	encodedRequest := multipartRequestWithEncodedPart()
	encodedBundle := buildBodyFieldBundle(t, "multipart-encoded", encodedRequest, emptyHTTPResponse(), fieldKey, false)
	encodedOccurrences := entitiesByKind(encodedBundle, "field_occurrence")
	if len(encodedOccurrences) != 1 || encodedOccurrences[0]["data"].(Record)["path"] != `request.body["mode"][]` {
		t.Fatalf("encoded part should stay structural while simple text is spanned: %#v", encodedOccurrences)
	}
	for _, entity := range entitiesByKind(encodedBundle, "field_occurrence") {
		if entity["data"].(Record)["path"] == `request.body["encoded"][]` {
			t.Fatal("transfer-encoded multipart part produced a source-bound occurrence")
		}
	}
	if !hasBodyOccurrenceGap(encodedBundle) {
		t.Fatal("transfer-encoded multipart part did not leave an explicit coverage gap")
	}

	nestedRequest := nestedMultipartRequest()
	nestedBundle := buildBodyFieldBundle(t, "multipart-nested", nestedRequest, emptyHTTPResponse(), fieldKey, false)
	for _, entity := range entitiesByKind(nestedBundle, "field_occurrence") {
		if entity["data"].(Record)["location"] == "request.body" {
			t.Fatal("nested multipart part produced a source-bound occurrence")
		}
	}
	if !hasBodyOccurrenceGap(nestedBundle) {
		t.Fatal("nested multipart part did not leave an explicit coverage gap")
	}

	truncatedBundle := buildBodyFieldBundle(t, "multipart-truncated", fileRequest, emptyHTTPResponse(), fieldKey, true)
	for _, entity := range entitiesByKind(truncatedBundle, "field_occurrence") {
		if entity["data"].(Record)["location"] == "request.body" {
			t.Fatal("truncated multipart message produced a source-bound occurrence")
		}
	}
}

func multipartResponseFixture(t *testing.T, fields [][2]string) string {
	t.Helper()
	request := multipartOccurrenceRequest(t, fields)
	parts := strings.SplitN(request, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("synthetic multipart request has no header/body boundary")
	}
	return "HTTP/1.1 200 OK\r\nContent-Type: multipart/form-data; boundary=" + multipartFixtureBoundary + "\r\n\r\n" + parts[1]
}

func multipartRequestWithFilePart(t *testing.T) string {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(multipartFixtureBoundary); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("mode", "MULTIPART_SAFE_MODE_276"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("upload", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("FILE_CONTENT_NEVER_HASH")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return "POST /upload HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: " + writer.FormDataContentType() + "\r\n\r\n" + body.String()
}

func multipartRequestWithExtendedFilenamePart() string {
	body := "--" + multipartFixtureBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"upload\"; filename*=UTF-8''fixture.txt\r\n" +
		"Content-Type: text/plain\r\n\r\nEXTENDED_FILE_CONTENT_NEVER_HASH\r\n" +
		"--" + multipartFixtureBoundary + "--\r\n"
	return "POST /upload HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: multipart/form-data; boundary=" + multipartFixtureBoundary + "\r\n\r\n" + body
}

func multipartRequestWithEncodedPart() string {
	body := "--" + multipartFixtureBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"mode\"\r\n\r\nMULTIPART_SAFE_MODE_276\r\n" +
		"--" + multipartFixtureBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"encoded\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nU0VDUkVU\r\n" +
		"--" + multipartFixtureBoundary + "--\r\n"
	return "POST /upload HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: multipart/form-data; boundary=" + multipartFixtureBoundary + "\r\n\r\n" + body
}

func nestedMultipartRequest() string {
	outer, inner := "BB-GRAPH-OUTER-BOUNDARY", "BB-GRAPH-INNER-BOUNDARY"
	body := "--" + outer + "\r\n" +
		"Content-Disposition: form-data; name=\"container\"\r\n" +
		"Content-Type: multipart/mixed; boundary=" + inner + "\r\n\r\n" +
		"--" + inner + "\r\nContent-Disposition: attachment; name=\"inner\"\r\n\r\nNESTED_PART_VALUE_515\r\n" +
		"--" + inner + "--\r\n--" + outer + "--\r\n"
	return "POST /upload HTTP/1.1\r\nHost: fixture.invalid\r\nContent-Type: multipart/form-data; boundary=" + outer + "\r\n\r\n" + body
}

func emptyHTTPResponse() string {
	return "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
}

func hasBodyOccurrenceGap(bundle Record) bool {
	cards, _ := bundle["cards"].([]Record)
	for _, card := range cards {
		missing, _ := card["missing_evidence"].([]any)
		for _, item := range missing {
			if text, ok := item.(string); ok && strings.Contains(text, "body field occurrences") {
				return true
			}
		}
	}
	return false
}
