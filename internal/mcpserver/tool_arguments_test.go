package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func catalogSpec(t *testing.T, name string) toolSpec {
	t.Helper()
	for _, spec := range toolCatalog(false) {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("tool %q is not advertised", name)
	return toolSpec{}
}

func TestArgumentSchemasNameArgumentsAndDifferences(t *testing.T) {
	search := catalogSpec(t, "bb_search_captures")
	searchProperties, ok := search.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("bb_search_captures properties have type %T", search.InputSchema["properties"])
	}
	if _, exists := searchProperties["project_key"]; !exists {
		t.Error("bb_search_captures schema does not advertise the accepted project_key alias")
	}
	if _, exists := searchProperties["project_keys"]; !exists {
		t.Error("bb_search_captures schema does not advertise project_keys")
	}
	alternatives, ok := search.InputSchema["anyOf"].([]map[string]any)
	if !ok || len(alternatives) != 2 {
		t.Fatalf("bb_search_captures schema does not require one of project_key/project_keys: %#v", search.InputSchema["anyOf"])
	}
	for _, alternative := range alternatives {
		required, _ := alternative["required"].([]string)
		if len(required) != 1 || (required[0] != "project_key" && required[0] != "project_keys") {
			t.Fatalf("bb_search_captures anyOf entry is not a single project argument: %#v", alternative)
		}
	}
	if !strings.Contains(search.Description, "project_key") || !strings.Contains(search.Description, "project_keys") {
		t.Errorf("bb_search_captures description does not name both project arguments: %q", search.Description)
	}

	neighborhood := catalogSpec(t, "bb_graph_neighborhood")
	neighborhoodProperties, _ := neighborhood.InputSchema["properties"].(map[string]any)
	depth, ok := neighborhoodProperties["max_depth"].(map[string]any)
	if !ok {
		t.Fatal("bb_graph_neighborhood schema does not advertise max_depth")
	}
	if depth["maximum"] != 6 {
		t.Errorf("bb_graph_neighborhood max_depth is not bounded to the shared range: %#v", depth)
	}
	if !strings.Contains(neighborhood.Description, "max_depth") || !strings.Contains(neighborhood.Description, "bb_graph_slice") {
		t.Errorf("bb_graph_neighborhood description does not explain max_depth: %q", neighborhood.Description)
	}

	capture := catalogSpec(t, "bb_get_capture")
	captureProperties, _ := capture.InputSchema["properties"].(map[string]any)
	requestID, _ := captureProperties["request_id"].(map[string]any)
	if requestID["description"] == nil || !strings.Contains(requestID["description"].(string), "observation") {
		t.Errorf("bb_get_capture request_id schema does not warn about observation ids: %#v", requestID)
	}
	if !strings.Contains(capture.Description, "observation_id") {
		t.Errorf("bb_get_capture description does not warn about observation ids: %q", capture.Description)
	}

	slice := catalogSpec(t, "bb_graph_slice")
	if !strings.Contains(slice.Description, "max_depth") {
		t.Errorf("bb_graph_slice description does not name max_depth: %q", slice.Description)
	}
}

func TestSearchCapturesAcceptsProjectKeyAlias(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "argument-fixture-instance", ProjectID: "argument-fixture-project"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	key := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close synthetic project: %v", err)
	}
	tools := &Tools{
		root: root, projects: []project.CaidoProjectRef{ref},
		projectsByKey: map[string]project.CaidoProjectRef{key: ref},
	}
	t.Cleanup(func() { _ = tools.Close() })

	for name, arguments := range map[string]string{
		"project_key alias": fmt.Sprintf(`{"project_key":%q}`, key),
		"project_keys list": fmt.Sprintf(`{"project_keys":[%q]}`, key),
	} {
		value, err := tools.call(ctx, "bb_search_captures", json.RawMessage(arguments))
		if err != nil {
			t.Fatalf("%s: searchCaptures() error = %v", name, err)
		}
		result, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: result has type %T", name, value)
		}
		selected, ok := result["selected_project_keys"].([]string)
		if !ok || len(selected) != 1 || selected[0] != key {
			t.Fatalf("%s: selected_project_keys = %#v; want [%s]", name, result["selected_project_keys"], key)
		}
	}

	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"ambiguous selection", fmt.Sprintf(`{"project_key":%q,"project_keys":[%q]}`, key, key), "not both"},
		{"missing selection", `{}`, "project_key"},
		{"unknown argument", fmt.Sprintf(`{"project":%q}`, key), `argument "project" is not accepted`},
		{"wrong argument type", fmt.Sprintf(`{"project_keys":%q}`, key), `argument "project_keys" has an invalid value type`},
		{"malformed key", `{"project_key":"not-a-project-key"}`, "invalid format"},
	}
	for _, testCase := range cases {
		_, err := tools.call(ctx, "bb_search_captures", json.RawMessage(testCase.arguments))
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: error = %v; want text containing %q", testCase.name, err, testCase.want)
		}
	}
}

func TestGraphNeighborhoodExplainsUnsupportedMaxDepth(t *testing.T) {
	ctx := context.Background()
	tools := &Tools{}
	key := strings.Repeat("a", 64)
	seed := "endpoint-" + strings.Repeat("b", 64)
	depthArguments := func(depth int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"project_key":%q,"seed_ids":[%q],"max_depth":%d}`, key, seed, depth))
	}

	for _, depth := range []int{0, 2, 6} {
		_, err := tools.call(ctx, "bb_graph_neighborhood", depthArguments(depth))
		if err == nil || !strings.Contains(err.Error(), "max_depth is not supported by bb_graph_neighborhood") ||
			!strings.Contains(err.Error(), "bb_graph_slice") {
			t.Errorf("max_depth=%d: error = %v; want clear guidance toward bb_graph_slice", depth, err)
		}
	}

	// max_depth=1 matches the fixed one-hop behavior and must reach the graph
	// service instead of being rejected as an unsupported argument.
	_, err := tools.call(ctx, "bb_graph_neighborhood", depthArguments(1))
	if err == nil || !strings.Contains(err.Error(), "graph query service is unavailable") {
		t.Fatalf("max_depth=1 was rejected before the graph query: %v", err)
	}

	_, err = tools.call(ctx, "bb_graph_neighborhood", json.RawMessage(fmt.Sprintf(`{"project_key":%q,"seed_ids":[%q],"depth":2}`, key, seed)))
	if err == nil || !strings.Contains(err.Error(), `argument "depth" is not accepted`) {
		t.Errorf("unknown depth argument: error = %v; want a named unknown-argument error", err)
	}
}

func TestGetCaptureRejectsObservationIDsWithGuidance(t *testing.T) {
	ctx := context.Background()
	tools := &Tools{}
	key := strings.Repeat("a", 64)
	observationID := "observation-" + strings.Repeat("c", 64)

	for name, arguments := range map[string]string{
		"observation_id argument": fmt.Sprintf(`{"project_key":%q,"observation_id":%q}`, key, observationID),
		"observation request_id":  fmt.Sprintf(`{"project_key":%q,"request_id":%q}`, key, observationID),
	} {
		_, err := tools.call(ctx, "bb_get_capture", json.RawMessage(arguments))
		if err == nil || !strings.Contains(err.Error(), "request_id is required") ||
			!strings.Contains(err.Error(), "observation ids are not accepted") {
			t.Errorf("%s: error = %v; want explicit observation-id rejection", name, err)
		}
	}

	// A real request ID must not be rejected by the observation-id guard; it
	// proceeds to the project catalog, which is empty in this fixture.
	_, err := tools.call(ctx, "bb_get_capture", json.RawMessage(fmt.Sprintf(`{"project_key":%q,"request_id":"request-123"}`, key)))
	if err == nil || !strings.Contains(err.Error(), "not in the local project catalog") {
		t.Fatalf("valid request ID was rejected before the project lookup: %v", err)
	}
}

func TestMCPClientSeesArgumentGuidance(t *testing.T) {
	t.Setenv("BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL", "")
	root := t.TempDir()
	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatalf("NewTools() error = %v", err)
	}
	defer tools.Close()
	server, err := NewServer(tools)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	key := strings.Repeat("a", 64)
	seed := "endpoint-" + strings.Repeat("b", 64)
	responses := serveFixtureRequests(t, server,
		map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "fixture", "version": "1"},
			},
		},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}},
		map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_search_captures",
				"arguments": map[string]any{
					"project_key": key, "project_keys": []string{key},
				},
			},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_graph_neighborhood",
				"arguments": map[string]any{
					"project_key": key, "seed_ids": []string{seed}, "max_depth": 2,
				},
			},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 4, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_get_capture",
				"arguments": map[string]any{
					"project_key": key, "observation_id": "observation-" + strings.Repeat("c", 64),
				},
			},
		},
	)
	for id, wants := range map[string][]string{
		"2": {"not both"},
		"3": {"max_depth is not supported by bb_graph_neighborhood", "bb_graph_slice"},
		"4": {"request_id is required", "observation ids are not accepted"},
	} {
		result, ok := responseForID(t, responses, id).Result.(map[string]any)
		if !ok || result["isError"] != true {
			t.Fatalf("call %s was not rejected: %#v", id, result)
		}
		content, _ := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("call %s returned %d content blocks", id, len(content))
		}
		text, _ := content[0].(map[string]any)["text"].(string)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("call %s text = %q; want text containing %q", id, text, want)
			}
		}
	}
}
