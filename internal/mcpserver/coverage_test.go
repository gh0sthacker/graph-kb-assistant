package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/coverage"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCoverageMatrixCatalogAndCall(t *testing.T) {
	root := t.TempDir()
	store, err := coverage.Open(filepath.Join(root, coverage.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	m := &coverage.Matrix{Run: "demo"}
	m.SetBudget("xss", 1)
	m.Record("xss", coverage.Confirmed, "req-1")
	m.SetBudget("idor", 1)
	if err := store.Save(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tools := &Tools{root: root}
	found := false
	for _, spec := range tools.catalog() {
		if spec.Name != "bb_coverage_matrix" {
			continue
		}
		found = true
		if spec.Annotations["readOnlyHint"] != true || spec.InputSchema["additionalProperties"] != false {
			t.Fatalf("unsafe spec = %+v", spec)
		}
		if !strings.Contains(spec.Description, "can_finish") || !strings.Contains(spec.Description, "no vulnerabilities") {
			t.Fatalf("missing coverage caveat: %s", spec.Description)
		}
		encoded, err := json.Marshal(spec.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		if len(schema.Required) != 1 || schema.Required[0] != "run" || schema.Properties["run"].Type != "string" {
			t.Fatalf("schema = %s", encoded)
		}
	}
	if !found {
		t.Fatal("bb_coverage_matrix missing from catalog")
	}
	server, err := NewServer(tools)
	if err != nil {
		t.Fatal(err)
	}
	result, err := server.callTool(context.Background(), json.RawMessage(`{"name":"bb_coverage_matrix","arguments":{"run":"demo"}}`))
	if err != nil || result["isError"] == true {
		t.Fatalf("tool result = %+v, %v", result, err)
	}
	encoded, err := json.Marshal(result["structuredContent"])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["run"] != "demo" || decoded["can_finish"] != false || decoded["coverage_pct"] != float64(50) || len(decoded["cells"].([]any)) != 2 {
		t.Fatalf("matrix result = %s", encoded)
	}
	for _, args := range []string{`{}`, `{"run":""}`, `{"run":" "}`, `{"run":12}`, `{"run":"demo","extra":true}`, `{"run":"missing"}`, `{"run":"` + strings.Repeat("a", 257) + `"}`} {
		if _, err := tools.call(context.Background(), "bb_coverage_matrix", json.RawMessage(args)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", args)
		}
	}
}

func TestCoverageMatrixStreamableHTTP(t *testing.T) {
	root := t.TempDir()
	store, err := coverage.Open(filepath.Join(root, coverage.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	matrix := &coverage.Matrix{Run: "http-demo"}
	matrix.SetBudget("xss", 1)
	if err := store.Save(t.Context(), matrix); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	handler, err := NewStreamableHTTPHandler(tools)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "coverage-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "bb_coverage_matrix", Arguments: map[string]any{"run": matrix.Run}})
	if err != nil || result.IsError {
		t.Fatalf("HTTP tool result = %+v, %v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["run"] != matrix.Run || decoded["can_finish"] != false {
		t.Fatalf("HTTP structured content = %s", encoded)
	}
}

func TestCoverageMatrixMissingDoesNotCreateDatabase(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	tools := &Tools{root: root}
	if _, err := tools.call(context.Background(), "bb_coverage_matrix", json.RawMessage(`{"run":"demo"}`)); err == nil {
		t.Fatal("missing database succeeded")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only tool created directory: %v", err)
	}
}
