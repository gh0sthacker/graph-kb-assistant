package mcpserver

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStreamableHTTPUsesSharedToolCatalogAndSessions(t *testing.T) {
	t.Setenv("BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL", "")
	root := t.TempDir()
	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatalf("NewTools() error = %v", err)
	}
	defer tools.Close()
	handler, err := NewStreamableHTTPHandler(tools)
	if err != nil {
		t.Fatalf("NewStreamableHTTPHandler() error = %v", err)
	}
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: httpServer.URL, DisableStandaloneSSE: true}
	session, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect Streamable HTTP client: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list MCP tools: %v", err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "bb_list_projects" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("remote MCP catalog omitted bb_list_projects")
	}

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "bb_list_projects",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call remote bb_list_projects: %v", err)
	}
	if result.IsError {
		t.Fatalf("remote project listing returned a tool error: %#v", result.Content)
	}
	if len(result.Content) != 1 || result.StructuredContent == nil {
		t.Fatalf("remote tool result is incomplete: %#v", result)
	}
}

func TestStreamableHTTPRejectsUntrustedOrigin(t *testing.T) {
	root := t.TempDir()
	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatalf("NewTools() error = %v", err)
	}
	defer tools.Close()
	handler, err := NewStreamableHTTPHandler(tools)
	if err != nil {
		t.Fatalf("NewStreamableHTTPHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	request := httptest.NewRequest("POST", server.URL, nil)
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("untrusted Origin status=%d; want 403", response.Code)
	}
}
