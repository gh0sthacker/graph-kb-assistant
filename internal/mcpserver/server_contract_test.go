package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestServerAdvertisesBoundedReadOnlyToolContract(t *testing.T) {
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
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}},
		map[string]any{
			"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{"name": "bb_list_projects", "arguments": map[string]any{}},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 4, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_graph_neighborhood",
				"arguments": map[string]any{
					"project_key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"seed_ids":    []string{"endpoint-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				},
			},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 5, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_graph_neighborhood",
				"arguments": map[string]any{
					"project_key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"seed_ids":    []string{"not-a-graph-id"},
				},
			},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 6, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_graph_neighborhood",
				"arguments": map[string]any{
					"project_key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"seed_ids": []string{
						"endpoint-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
						"endpoint-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					},
				},
			},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 7, "method": "tools/call",
			"params": map[string]any{
				"name": "bb_graph_neighborhood",
				"arguments": map[string]any{
					"project_key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"seed_ids":    []string{"source-" + strings.Repeat("c", 64)},
				},
			},
		},
	)

	listResult := responseForID(t, responses, "2").Result.(map[string]any)
	listedTools, ok := listResult["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result has type %T; want []any", listResult["tools"])
	}
	wantNames := []string{
		"bb_get_capture", "bb_graph_neighborhood", "bb_graph_slice", "bb_kb_get_source", "bb_kb_search",
		"bb_kb_stats", "bb_list_projects", "bb_search_captures", "bb_surface_map",
	}
	gotNames := make([]string, 0, len(listedTools))
	neighborhoodAdvertised := false
	sliceContinuationAdvertised := false
	for _, value := range listedTools {
		spec, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("tool spec has type %T; want object", value)
		}
		name, _ := spec["name"].(string)
		gotNames = append(gotNames, name)
		annotations, ok := spec["annotations"].(map[string]any)
		if !ok || annotations["readOnlyHint"] != true || annotations["openWorldHint"] != false {
			t.Errorf("tool %q is missing explicit read-only/local annotations: %#v", name, spec["annotations"])
		}
		schema, ok := spec["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("tool %q does not have a closed object input schema: %#v", name, spec["inputSchema"])
		}
		if name == "bb_graph_neighborhood" {
			neighborhoodAdvertised = true
			properties, _ := schema["properties"].(map[string]any)
			seeds, _ := properties["seed_ids"].(map[string]any)
			if seeds["minItems"] != float64(1) || seeds["maxItems"] != float64(20) {
				t.Errorf("graph neighborhood seed limit is not bounded to 1-20: %#v", seeds)
			}
			if seeds["uniqueItems"] != true {
				t.Errorf("graph neighborhood seeds do not require unique IDs: %#v", seeds)
			}
			cursor, _ := properties["cursor"].(map[string]any)
			if cursor["type"] != "string" || cursor["maxLength"] != float64(4096) || cursor["minLength"] != float64(1) {
				t.Errorf("graph neighborhood continuation cursor is not bounded: %#v", cursor)
			}
			seedItemSchema, _ := seeds["items"].(map[string]any)
			if seedItemSchema["pattern"] != graphNeighborhoodSeedPattern.String() {
				t.Errorf("graph neighborhood seed pattern %#v does not match runtime validation %q", seedItemSchema["pattern"], graphNeighborhoodSeedPattern.String())
			}
			forbiddenKinds, _ := seedItemSchema["not"].(map[string]any)
			if forbiddenKinds["pattern"] != `^(source|card|assertion)-` {
				t.Errorf("graph neighborhood schema permits non-seed record kinds: %#v", seedItemSchema)
			}
			required, _ := schema["required"].([]any)
			if len(required) != 2 || required[0] != "project_key" || required[1] != "seed_ids" {
				t.Errorf("graph neighborhood does not require project and seeds: %#v", schema["required"])
			}
		}
		if name == "bb_graph_slice" {
			sliceContinuationAdvertised = true
			properties, _ := schema["properties"].(map[string]any)
			cursor, _ := properties["cursor"].(map[string]any)
			if cursor["type"] != "string" || cursor["minLength"] != float64(43) || cursor["maxLength"] != float64(43) || cursor["pattern"] != `^[A-Za-z0-9_-]{43}$` {
				t.Errorf("full graph continuation cursor is not an exact bounded token: %#v", cursor)
			}
		}
	}
	sort.Strings(gotNames)
	if len(listedTools) != len(wantNames) || len(gotNames) != len(wantNames) {
		t.Fatalf("advertised %d tools %v; want exactly %d %v", len(listedTools), gotNames, len(wantNames), wantNames)
	}
	if !sliceContinuationAdvertised {
		t.Fatal("bounded full graph continuation cursor was not advertised")
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Errorf("advertised tool names %v; want %v", gotNames, wantNames)
			break
		}
	}

	callResult := responseForID(t, responses, "3").Result.(map[string]any)
	if callResult["isError"] == true {
		t.Fatalf("bb_list_projects call failed: %#v", callResult)
	}
	structured, ok := callResult["structuredContent"].(map[string]any)
	if !ok || structured["project_count"] != float64(0) || structured["total_project_count"] != float64(0) {
		t.Fatalf("empty synthetic project catalog returned %#v", callResult["structuredContent"])
	}
	neighborhoodCall := responseForID(t, responses, "4").Result.(map[string]any)
	if !neighborhoodAdvertised || neighborhoodCall["isError"] != true {
		t.Fatalf("bounded graph neighborhood tool was not advertised or rejected an unregistered project: %#v", neighborhoodCall)
	}
	content, _ := neighborhoodCall["content"].([]any)
	if len(content) != 1 || !bytes.Contains([]byte(content[0].(map[string]any)["text"].(string)), []byte("not in the local project catalog")) {
		t.Errorf("graph neighborhood call did not use the selected project path: %#v", neighborhoodCall["content"])
	}
	for id, wantMessage := range map[string]string{
		"5": "graph neighborhood seed ID is malformed",
		"6": "graph neighborhood seed IDs must be unique",
		"7": "graph neighborhood seed ID is malformed",
	} {
		result := responseForID(t, responses, id).Result.(map[string]any)
		resultContent, _ := result["content"].([]any)
		if result["isError"] != true || len(resultContent) != 1 || !bytes.Contains([]byte(resultContent[0].(map[string]any)["text"].(string)), []byte(wantMessage)) {
			t.Errorf("graph neighborhood request %s did not reject invalid seeds: %#v", id, result)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(temp root) error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("read-only MCP setup created files in its empty synthetic data root: %v", entries)
	}
}

func TestServerRejectsCallsBeforeInitializationAndInvalidToolArguments(t *testing.T) {
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

	beforeInit := serveFixtureRequests(t, server,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}},
	)
	if responseForID(t, beforeInit, "1").Error == nil {
		t.Fatal("tools/list succeeded before initialize")
	}

	server, err = NewServer(tools)
	if err != nil {
		t.Fatalf("NewServer() second instance error = %v", err)
	}
	responses := serveFixtureRequests(t, server,
		map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
				"clientInfo": map[string]any{"name": "fixture", "version": "1"},
			},
		},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}},
		map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "bb_list_projects", "arguments": map[string]any{"limit": 201}},
		},
		map[string]any{
			"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{"name": "unknown_tool", "arguments": map[string]any{}},
		},
	)
	for _, id := range []string{"2", "3"} {
		result, ok := responseForID(t, responses, id).Result.(map[string]any)
		if !ok || result["isError"] != true {
			t.Errorf("invalid/unknown tool call %s was not rejected: %#v", id, result)
		}
	}
}

func serveFixtureRequests(t *testing.T, server *Server, requests ...map[string]any) []rpcResponse {
	t.Helper()
	var input bytes.Buffer
	for _, request := range requests {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("json.Marshal(request) error = %v", err)
		}
		input.Write(encoded)
		input.WriteByte('\n')
	}
	var output bytes.Buffer
	if err := server.Serve(context.Background(), &input, &output); err != nil {
		t.Fatalf("Server.Serve() error = %v", err)
	}

	responses := make([]rpcResponse, 0, len(requests))
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var response rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("server emitted invalid JSON-RPC response: %v", err)
		}
		responses = append(responses, response)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("response scanner error = %v", err)
	}
	return responses
}

func responseForID(t *testing.T, responses []rpcResponse, id string) rpcResponse {
	t.Helper()
	for _, response := range responses {
		if string(response.ID) == id {
			return response
		}
	}
	t.Fatalf("response ID %s was not emitted", id)
	return rpcResponse{}
}
