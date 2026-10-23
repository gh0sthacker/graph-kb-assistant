package caido

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type noSwitchToolCall struct {
	name string
	args map[string]any
}

type noSwitchToolCaller struct {
	selectedID              string
	changedProjectID        string
	changeAfterRequest      bool
	multipleCurrentProjects bool
	requestPayloads         []map[string]any
	requestCalls            int
	calls                   []noSwitchToolCall
}

func (c *noSwitchToolCaller) CallTool(name string, args map[string]any) (map[string]any, error) {
	c.calls = append(c.calls, noSwitchToolCall{name: name, args: cloneArgs(args)})
	switch name {
	case toolListProjects:
		currentID := c.selectedID
		if c.changeAfterRequest && c.requestCalls > 0 {
			currentID = c.changedProjectID
		}
		return noSwitchResult(map[string]any{"projects": c.projects(currentID)}), nil
	case toolListRequests:
		c.requestCalls++
		if len(c.requestPayloads) == 0 {
			return noSwitchResult(map[string]any{"requests": []any{}, "hasMore": false}), nil
		}
		index := c.requestCalls - 1
		if index >= len(c.requestPayloads) {
			index = len(c.requestPayloads) - 1
		}
		return noSwitchResult(c.requestPayloads[index]), nil
	case toolSelectProject, toolGetRequest:
		return nil, errors.New("forbidden Caido tool called")
	default:
		return nil, errors.New("unexpected Caido tool called")
	}
}

func (c *noSwitchToolCaller) CallToolContext(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.CallTool(name, args)
}

func (c *noSwitchToolCaller) projects(currentID string) []any {
	rows := []any{
		map[string]any{"id": "selected-project", "name": "Selected fixture", "status": "READY", "version": "fixture", "isCurrent": currentID == "selected-project"},
		map[string]any{"id": "other-project", "name": "Other fixture", "status": "READY", "version": "fixture", "isCurrent": currentID == "other-project"},
	}
	if c.multipleCurrentProjects {
		rows[1].(map[string]any)["isCurrent"] = true
	}
	return rows
}

func noSwitchResult(payload map[string]any) map[string]any {
	return map[string]any{"structuredContent": payload}
}

func cloneArgs(args map[string]any) map[string]any {
	copy := make(map[string]any, len(args))
	for key, value := range args {
		copy[key] = value
	}
	return copy
}

func newNoSwitchSource(t *testing.T, caller *noSwitchToolCaller) *MCPReadSource {
	t.Helper()
	source, err := NewMCPReadSource(caller, "fixture-caido-instance")
	if err != nil {
		t.Fatalf("NewMCPReadSource() error = %v", err)
	}
	return source
}

func selectedProjectRef() ProjectRef {
	return ProjectRef{InstanceID: "fixture-caido-instance", ProjectID: "selected-project"}
}

func assertNoProjectMutationOrDetailCalls(t *testing.T, calls []noSwitchToolCall) {
	t.Helper()
	for _, call := range calls {
		if call.name == toolSelectProject || call.name == toolGetRequest {
			t.Errorf("read-only inventory called forbidden tool %q", call.name)
		}
	}
}

func TestCurrentProjectContextReturnsSelectedProjectWithoutMutation(t *testing.T) {
	caller := &noSwitchToolCaller{selectedID: "selected-project"}
	source := newNoSwitchSource(t, caller)

	got, err := source.CurrentProjectContext(context.Background())
	if err != nil {
		t.Fatalf("CurrentProjectContext() error = %v", err)
	}
	if !got.SameProject(selectedProjectRef()) {
		t.Fatalf("CurrentProjectContext() = %+v; want selected fixture project", got)
	}
	if caller.requestCalls != 0 {
		t.Fatalf("CurrentProjectContext() made %d request-list calls; want none", caller.requestCalls)
	}
	assertNoProjectMutationOrDetailCalls(t, caller.calls)
	for _, call := range caller.calls {
		if call.name != toolListProjects {
			t.Errorf("CurrentProjectContext() called %q; want only %q", call.name, toolListProjects)
		}
	}
}

func TestCurrentProjectContextRejectsMissingOrAmbiguousSelection(t *testing.T) {
	for _, test := range []struct {
		name                    string
		selectedID              string
		multipleCurrentProjects bool
	}{
		{name: "no current project"},
		{name: "ambiguous current project", selectedID: "selected-project", multipleCurrentProjects: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := &noSwitchToolCaller{
				selectedID:              test.selectedID,
				multipleCurrentProjects: test.multipleCurrentProjects,
			}
			source := newNoSwitchSource(t, caller)
			if _, err := source.CurrentProjectContext(context.Background()); err == nil {
				t.Fatal("CurrentProjectContext() succeeded without one unambiguous selected project")
			}
			assertNoProjectMutationOrDetailCalls(t, caller.calls)
		})
	}
}

func TestListRequestPageNoSwitchContextReadsOnlyAlreadySelectedProject(t *testing.T) {
	cursor := "cursor-before-page"
	caller := &noSwitchToolCaller{
		selectedID: "selected-project",
		requestPayloads: []map[string]any{{
			"requests": []any{
				map[string]any{"id": "fixture-request-1", "method": "GET", "url": "https://fixture.invalid/a", "statusCode": float64(200)},
			},
			"hasMore":    true,
			"nextCursor": "cursor-after-page",
		}},
	}
	source := newNoSwitchSource(t, caller)

	got, err := source.ListRequestPageNoSwitchContext(context.Background(), selectedProjectRef(), &cursor, 7)
	if err != nil {
		t.Fatalf("ListRequestPageNoSwitchContext() error = %v", err)
	}
	if len(got.Requests) != 1 || got.Requests[0].RequestID != "fixture-request-1" ||
		got.NextCursor == nil || *got.NextCursor != "cursor-after-page" || !got.HasMore {
		t.Fatalf("ListRequestPageNoSwitchContext() = %+v; want the fixture page", got)
	}
	if caller.requestCalls != 1 {
		t.Fatalf("request-list calls = %d; want 1", caller.requestCalls)
	}
	var requestArgs map[string]any
	for _, call := range caller.calls {
		if call.name == toolListRequests {
			requestArgs = call.args
		}
	}
	if requestArgs == nil || requestArgs["limit"] != 7 || requestArgs["after"] != cursor {
		t.Errorf("list request arguments = %#v; want page size 7 and cursor %q", requestArgs, cursor)
	}
	assertNoProjectMutationOrDetailCalls(t, caller.calls)
}

func TestListRequestPageNoSwitchContextRejectsWrongSelectedProjectBeforeListing(t *testing.T) {
	caller := &noSwitchToolCaller{selectedID: "other-project"}
	source := newNoSwitchSource(t, caller)

	if _, err := source.ListRequestPageNoSwitchContext(context.Background(), selectedProjectRef(), nil, 10); err == nil {
		t.Fatal("ListRequestPageNoSwitchContext() accepted a different current project")
	}
	if caller.requestCalls != 0 {
		t.Fatalf("request-list calls = %d; want none for a mismatched project", caller.requestCalls)
	}
	assertNoProjectMutationOrDetailCalls(t, caller.calls)
}

func TestListRequestPageNoSwitchContextRejectsSelectionChangeAfterListing(t *testing.T) {
	caller := &noSwitchToolCaller{
		selectedID:         "selected-project",
		changedProjectID:   "other-project",
		changeAfterRequest: true,
		requestPayloads: []map[string]any{{
			"requests": []any{map[string]any{"id": "fixture-request-2", "method": "POST", "url": "https://fixture.invalid/b"}},
			"hasMore":  false,
		}},
	}
	source := newNoSwitchSource(t, caller)

	if _, err := source.ListRequestPageNoSwitchContext(context.Background(), selectedProjectRef(), nil, 10); err == nil {
		t.Fatal("ListRequestPageNoSwitchContext() accepted a page after Caido selection changed")
	}
	if caller.requestCalls != 1 {
		t.Fatalf("request-list calls = %d; want the page read followed by selection verification", caller.requestCalls)
	}
	assertNoProjectMutationOrDetailCalls(t, caller.calls)
}

func TestListRequestPageNoSwitchContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	caller := &noSwitchToolCaller{selectedID: "selected-project"}
	source := newNoSwitchSource(t, caller)

	if _, err := source.ListRequestPageNoSwitchContext(ctx, selectedProjectRef(), nil, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListRequestPageNoSwitchContext() error = %v; want context.Canceled", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("canceled inventory made %d Caido calls; want none", len(caller.calls))
	}
}

func TestCurrentProjectContextRejectsDifferentInstance(t *testing.T) {
	caller := &noSwitchToolCaller{selectedID: "selected-project"}
	source := newNoSwitchSource(t, caller)
	otherInstance := selectedProjectRef()
	otherInstance.InstanceID = "different-caido-instance"

	if _, err := source.ListRequestPageNoSwitchContext(context.Background(), otherInstance, nil, 10); err == nil {
		t.Fatal("ListRequestPageNoSwitchContext() accepted a project from another Caido instance")
	}
	if caller.requestCalls != 0 {
		t.Fatalf("request-list calls = %d; want none for a different instance", caller.requestCalls)
	}
	assertNoProjectMutationOrDetailCalls(t, caller.calls)
}

func TestNoSwitchFixturePageDoesNotChangeAcrossCalls(t *testing.T) {
	caller := &noSwitchToolCaller{
		selectedID: "selected-project",
		requestPayloads: []map[string]any{{
			"requests": []any{},
			"hasMore":  false,
		}},
	}
	source := newNoSwitchSource(t, caller)
	project, err := source.CurrentProjectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	page, err := source.ListRequestPageNoSwitchContext(context.Background(), project, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Requests, []RequestMetadata{}) {
		t.Fatalf("empty request page = %#v; want an empty metadata slice", page.Requests)
	}
	assertNoProjectMutationOrDetailCalls(t, caller.calls)
}

func TestParseRequestPageRejectsOversizedIDsAndCursors(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload map[string]any
	}{
		{
			name: "request ID",
			payload: map[string]any{
				"requests": []any{map[string]any{
					"id": strings.Repeat("x", 513), "method": "GET", "url": "https://fixture.invalid/",
				}},
				"hasMore": false,
			},
		},
		{
			name: "cursor",
			payload: map[string]any{
				"requests":   []any{},
				"hasMore":    true,
				"nextCursor": strings.Repeat("x", cursorMaximumBytes+1),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseRequestPage(test.payload, nil); err == nil {
				t.Fatalf("parseRequestPage() accepted oversized %s", test.name)
			}
		})
	}
}

func TestParseRequestLineAcceptsCaidoLineLongerThanEightKiB(t *testing.T) {
	line := "GET /" + strings.Repeat("a", 9_000) + " HTTP/1.1"
	got, err := parseRequestLine(line, "GET")
	if err != nil {
		t.Fatalf("parseRequestLine() rejected a bounded long request line (%d bytes): %v", len(line), err)
	}
	if got != line {
		t.Fatal("parseRequestLine() changed the request line")
	}
}

func TestParseRequestLineRejectsLineBeyondConfiguredMaximum(t *testing.T) {
	line := "GET /" + strings.Repeat("a", MessageLineBytesMaximum+1) + " HTTP/1.1"
	if _, err := parseRequestLine(line, "GET"); err == nil {
		t.Fatalf("parseRequestLine() accepted a %d-byte line beyond the configured maximum", len(line))
	}
}

func TestParseHeadersReportsControlCharacterIndexWithoutEchoingValue(t *testing.T) {
	privateValue := "private-header-value\r\nInjected: do-not-log"
	_, err := parseHeaders([]any{
		map[string]any{"name": "X-Trace", "value": "ok"},
		map[string]any{"name": "X-Unsafe", "value": privateValue},
	})
	if err == nil {
		t.Fatal("parseHeaders() accepted a header value with a control character")
	}
	if !strings.Contains(err.Error(), "header 2") || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("parseHeaders() error does not identify the safe failure location: %v", err)
	}
	if strings.Contains(err.Error(), privateValue) || strings.Contains(err.Error(), "private-header-value") {
		t.Fatal("parseHeaders() included the private header value in its error")
	}
}

func TestParseHeadersReportsNonTextValueWithoutEchoingValue(t *testing.T) {
	_, err := parseHeaders([]any{map[string]any{"name": "X-Trace", "value": map[string]any{"secret": "private"}}})
	if err == nil {
		t.Fatal("parseHeaders() accepted a non-text header value")
	}
	if !strings.Contains(err.Error(), "header 1") || !strings.Contains(err.Error(), "non-text value") {
		t.Fatalf("parseHeaders() error does not identify the safe failure location: %v", err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("parseHeaders() included a private header value in its error")
	}
}

func TestParseHeadersPreservesNonTokenPrintableName(t *testing.T) {
	name := "X-Invalid/Name"
	got, err := parseHeaders([]any{map[string]any{"name": name, "value": "preserved"}})
	if err != nil {
		t.Fatalf("parseHeaders() rejected a printable non-token name: %v", err)
	}
	want := [][2]string{{name, "preserved"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHeaders() = %#v, want %#v", got, want)
	}
}

func TestParseHeadersRejectsControlCharacterInNameWithoutEchoingValue(t *testing.T) {
	privateName := "X-Unsafe\r\nInjected"
	_, err := parseHeaders([]any{map[string]any{"name": privateName, "value": "private"}})
	if err == nil {
		t.Fatal("parseHeaders() accepted a header name with a control character")
	}
	if !strings.Contains(err.Error(), "header 1") || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("parseHeaders() error does not identify the safe failure category: %v", err)
	}
	if strings.Contains(err.Error(), privateName) || strings.Contains(err.Error(), "private") {
		t.Fatal("parseHeaders() included a private header in its error")
	}
}

func TestParseHeadersReportsNonTextNameCategoryWithoutEchoingValue(t *testing.T) {
	_, err := parseHeaders([]any{map[string]any{"name": map[string]any{"private": "name"}, "value": "private-value"}})
	if err == nil {
		t.Fatal("parseHeaders() accepted a non-text header name")
	}
	if !strings.Contains(err.Error(), "header 1") || !strings.Contains(err.Error(), "non-text name") {
		t.Fatalf("parseHeaders() error does not identify the safe failure category: %v", err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("parseHeaders() included a private header value in its error")
	}
}

func TestParseHeadersAcceptsHTTP2PseudoHeaderNames(t *testing.T) {
	input := []any{
		map[string]any{"name": ":method", "value": "GET"},
		map[string]any{"name": ":authority", "value": "fixture.invalid"},
		map[string]any{"name": ":path", "value": "/items?id=1"},
	}
	got, err := parseHeaders(input)
	if err != nil {
		t.Fatalf("parseHeaders() rejected valid HTTP/2 pseudo-header names: %v", err)
	}
	want := [][2]string{{":method", "GET"}, {":authority", "fixture.invalid"}, {":path", "/items?id=1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHeaders() = %#v, want %#v", got, want)
	}
}

func TestHasMalformedHeaderNameRequiresHTTP2ForPseudoHeaders(t *testing.T) {
	headers := [][2]string{{":method", "GET"}, {":authority", "fixture.invalid"}}
	if hasMalformedHeaderName(headers, "GET / HTTP/2") {
		t.Fatal("hasMalformedHeaderName() rejected HTTP/2 pseudo-headers")
	}
	if !hasMalformedHeaderName(headers, "GET / HTTP/1.1") {
		t.Fatal("hasMalformedHeaderName() accepted pseudo-headers on HTTP/1.1")
	}
}

func TestFetchExchangeFlagsPrintableNonTokenHeaderName(t *testing.T) {
	selected := ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	metadata := RequestMetadata{RequestID: "fixture-request", Method: "GET", URL: "https://fixture.invalid/"}
	payload := map[string]any{
		"request": map[string]any{
			"firstLine": "GET / HTTP/1.1",
			"headers": []any{map[string]any{
				"name": "X-Invalid/Name", "value": "preserved",
			}},
			"fingerprint": map[string]any{"kind": "empty", "bodySize": 0},
		},
	}
	exchange, err := (&MCPReadSource{}).fetchExchange(selected, metadata, payload, bodyChunkBytes)
	if err != nil {
		t.Fatalf("fetchExchange() rejected a printable non-token header name: %v", err)
	}
	if !exchange.MalformedHeaderNamePresent {
		t.Fatal("fetchExchange() did not retain the malformed-header-name state")
	}
	if !strings.Contains(string(exchange.RequestRaw), "X-Invalid/Name: preserved") {
		t.Fatal("fetchExchange() did not preserve the printable header name in reconstructed text")
	}
}

func TestFetchExchangePreservesDetailedResponseStatusOnMetadataMismatch(t *testing.T) {
	selected := ProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	metadataStatus := 200
	metadata := RequestMetadata{
		RequestID: "fixture-request", Method: "GET", URL: "https://fixture.invalid/", StatusCode: &metadataStatus,
	}
	payload := map[string]any{
		"request": map[string]any{
			"firstLine":   "GET / HTTP/1.1",
			"headers":     []any{map[string]any{"name": "Host", "value": "fixture.invalid"}},
			"fingerprint": map[string]any{"kind": "empty", "bodySize": 0},
		},
		"response": map[string]any{
			"firstLine":   "HTTP/1.1 201 Created",
			"headers":     []any{},
			"bodySize":    0,
			"fingerprint": map[string]any{"kind": "empty", "bodySize": 0},
		},
	}
	exchange, err := (&MCPReadSource{}).fetchExchange(selected, metadata, payload, bodyChunkBytes)
	if err != nil {
		t.Fatalf("fetchExchange() rejected a detailed response with differing history metadata: %v", err)
	}
	if exchange.ResponseStatus == nil || *exchange.ResponseStatus != 201 {
		t.Fatalf("fetchExchange() status = %v, want detailed status 201", exchange.ResponseStatus)
	}
	if !exchange.ResponseStatusMetadataMismatch {
		t.Fatal("fetchExchange() did not retain the metadata mismatch state")
	}
}
