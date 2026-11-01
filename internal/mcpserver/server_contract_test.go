package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"bb-graph-assistant/internal/kb"
	_ "modernc.org/sqlite"
)

func TestServerAdvertisesBoundedConsultationToolContract(t *testing.T) {
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
		"bb_candidate_save", "bb_consult_compare", "bb_consult_create_session", "bb_consult_record_proposal",
		"bb_consult_resume", "bb_consult_validate_proposal",
		"bb_get_capture", "bb_graph_neighborhood", "bb_graph_slice", "bb_kb_get_source", "bb_kb_search",
		"bb_kb_stats", "bb_list_projects", "bb_search_captures", "bb_surface_map",
	}
	gotNames := make([]string, 0, len(listedTools))
	neighborhoodAdvertised := false
	sliceContinuationAdvertised := false
	sliceProvenanceDescribed := false
	surfaceCatalogAdvertised := false
	for _, value := range listedTools {
		spec, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("tool spec has type %T; want object", value)
		}
		name, _ := spec["name"].(string)
		gotNames = append(gotNames, name)
		annotations, ok := spec["annotations"].(map[string]any)
		if !ok || annotations["openWorldHint"] != false {
			t.Errorf("tool %q is missing explicit local-only annotations: %#v", name, spec["annotations"])
		} else if name == "bb_consult_resume" || name == "bb_get_capture" || name == "bb_graph_neighborhood" || name == "bb_graph_slice" ||
			name == "bb_kb_get_source" || name == "bb_kb_search" || name == "bb_kb_stats" || name == "bb_list_projects" ||
			name == "bb_search_captures" || name == "bb_surface_map" {
			if annotations["readOnlyHint"] != true {
				t.Errorf("tool %q must be marked read-only: %#v", name, annotations)
			}
		} else if annotations["readOnlyHint"] != false || annotations["destructiveHint"] != false || annotations["idempotentHint"] != true {
			t.Errorf("tool %q must be marked as an idempotent non-destructive write: %#v", name, annotations)
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
		if name == "bb_kb_search" {
			properties, _ := schema["properties"].(map[string]any)
			minCVSS, _ := properties["min_cvss"].(map[string]any)
			if minCVSS["type"] != "number" || minCVSS["minimum"] != float64(0) || minCVSS["maximum"] != float64(10) {
				t.Errorf("KB min_cvss schema is not bounded to 0-10: %#v", minCVSS)
			}
			severity, _ := properties["severity"].(map[string]any)
			enum, ok := severity["enum"].([]any)
			wantSeverities := map[string]bool{
				"critical": true, "high": true, "medium": true, "low": true,
				"none": true, "info": true, "informational": true, "unknown": true,
			}
			if ok {
				for _, value := range enum {
					if text, valid := value.(string); valid {
						delete(wantSeverities, text)
					}
				}
			}
			if severity["type"] != "string" || !ok || len(enum) != 8 || len(wantSeverities) != 0 {
				t.Errorf("KB severity schema does not expose the accepted values: %#v", severity)
			}
		}
		if name == "bb_graph_slice" {
			sliceContinuationAdvertised = true
			description, _ := spec["description"].(string)
			sliceProvenanceDescribed = strings.Contains(description, "provenance branches") && strings.Contains(description, "provenance cursor caps")
			if !sliceProvenanceDescribed {
				t.Errorf("full graph continuation does not describe resumable provenance and its cap: %q", description)
			}
			properties, _ := schema["properties"].(map[string]any)
			cursor, _ := properties["cursor"].(map[string]any)
			if cursor["type"] != "string" || cursor["minLength"] != float64(43) || cursor["maxLength"] != float64(43) || cursor["pattern"] != `^[A-Za-z0-9_-]{43}$` {
				t.Errorf("full graph continuation cursor is not an exact bounded token: %#v", cursor)
			}
		}
		if name == "bb_surface_map" {
			surfaceCatalogAdvertised = true
			properties, _ := schema["properties"].(map[string]any)
			cursor, _ := properties["cursor"].(map[string]any)
			filter, _ := properties["filter"].(map[string]any)
			limit, _ := properties["limit"].(map[string]any)
			if cursor["type"] != "string" || cursor["maxLength"] != float64(16<<10) || cursor["minLength"] != float64(1) {
				t.Errorf("surface cursor is not bounded: %#v", cursor)
			}
			if filter["type"] != "string" || filter["maxLength"] != float64(256) {
				t.Errorf("surface filter is not bounded: %#v", filter)
			}
			if limit["maximum"] != float64(100) {
				t.Errorf("surface page limit is not 100: %#v", limit)
			}
			if _, legacy := properties["after"]; legacy {
				t.Errorf("surface tool still exposes row-based cursors: %#v", properties["after"])
			}
		}
	}
	sort.Strings(gotNames)
	if len(listedTools) != len(wantNames) || len(gotNames) != len(wantNames) {
		t.Fatalf("advertised %d tools %v; want exactly %d %v", len(listedTools), gotNames, len(wantNames), wantNames)
	}
	if !sliceContinuationAdvertised || !sliceProvenanceDescribed || !surfaceCatalogAdvertised {
		t.Fatal("bounded full graph or surface catalog continuation was not advertised")
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

func TestKBSearchMCPArgumentsForwardMetadataFilters(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "kb-index.sqlite3")
	createKBFilterIndex(t, indexPath)
	index, err := kb.OpenIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	var args kbSearchArgs
	if err := json.Unmarshal([]byte(`{"query":"synthetic","min_cvss":7.0,"severity":"high"}`), &args); err != nil {
		t.Fatalf("decode MCP KB search arguments: %v", err)
	}
	if args.MinCVSS == nil || *args.MinCVSS != 7 || args.Severity != "high" {
		t.Fatalf("decoded MCP filter arguments = %+v", args)
	}
	tools := &Tools{kbIndex: index}
	result, err := tools.searchKB(context.Background(), args)
	if err != nil {
		t.Fatalf("searchKB() error = %v", err)
	}
	hits, ok := result["hits"].([]map[string]any)
	if !ok || len(hits) != 1 || hits[0]["source_id"] != "cve-high" {
		t.Fatalf("MCP search returned %#v; want only the inclusive high-severity boundary hit", result["hits"])
	}
	invalidCVSS := 10.1
	if _, err := tools.searchKB(context.Background(), kbSearchArgs{Query: "synthetic", MinCVSS: &invalidCVSS}); err == nil {
		t.Fatal("searchKB accepted min_cvss above 10")
	}
	if _, err := tools.searchKB(context.Background(), kbSearchArgs{Query: "synthetic", Severity: "Critical"}); err == nil {
		t.Fatal("searchKB accepted a noncanonical severity")
	}
}

func TestKBStatsMCPExposesStagedVectorProgress(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "kb-index.sqlite3")
	createKBFilterIndex(t, indexPath)
	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE chunk_embeddings(
		chunk_id TEXT PRIMARY KEY,model_name TEXT NOT NULL,dimensions INTEGER NOT NULL,vector BLOB NOT NULL)`)
	if err == nil {
		_, err = db.Exec(`INSERT INTO chunk_embeddings(chunk_id,model_name,dimensions,vector)
			VALUES(?,?,1024,zeroblob(4096))`, strings.Repeat("a", 64), "intfloat/e5-large-v2")
	}
	if err == nil {
		_, err = db.Exec(`INSERT INTO kb_meta(key,value) VALUES
			('vector_staging_model','intfloat/e5-large-v2'),
			('vector_staging_dimensions','1024'),
			('vector_staging_snapshot_id',?)`, strings.Repeat("1", 64))
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("prepare staged KB stats fixture: %v", err)
	}

	tools, err := NewTools(root, indexPath)
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
		map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "bb_kb_stats", "arguments": map[string]any{}},
		},
	)
	call := responseForID(t, responses, "2").Result.(map[string]any)
	if call["isError"] == true {
		t.Fatalf("bb_kb_stats call failed: %#v", call)
	}
	stats, ok := call["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("bb_kb_stats structured result has type %T", call["structuredContent"])
	}
	if stats["total_content_chunks"] != float64(2) || stats["embedded_content_chunks"] != float64(1) || stats["vector_ready"] != false {
		t.Fatalf("bb_kb_stats progress = %#v; want 1/2 content chunks and not ready", stats)
	}
	if stats["embedding_target_model"] != "intfloat/e5-large-v2" ||
		stats["embedding_target_snapshot_id"] != strings.Repeat("1", 64) || stats["embedding_target_dimensions"] != float64(1024) {
		t.Fatalf("bb_kb_stats target = model:%#v snapshot:%#v dimensions:%#v", stats["embedding_target_model"], stats["embedding_target_snapshot_id"], stats["embedding_target_dimensions"])
	}
}

func createKBFilterIndex(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE source_documents (source_name TEXT NOT NULL, source_id TEXT NOT NULL, title TEXT NOT NULL,
			source_url TEXT, source_locator TEXT, source_revision TEXT, license_id TEXT, attribution TEXT,
			metadata_json TEXT NOT NULL, content_sha256 TEXT NOT NULL, text TEXT NOT NULL,
			PRIMARY KEY(source_name,source_id))`,
		`CREATE TABLE chunks (chunk_id TEXT PRIMARY KEY, source_name TEXT NOT NULL, source_id TEXT NOT NULL,
			title TEXT NOT NULL, start_char INTEGER NOT NULL, end_char INTEGER NOT NULL, source_locator TEXT,
			source_url TEXT, source_revision TEXT, license_id TEXT, attribution TEXT, metadata_json TEXT NOT NULL,
			source_sha256 TEXT NOT NULL, chunk_sha256 TEXT NOT NULL, chunk_schema TEXT NOT NULL, text TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE chunk_search USING fts5(chunk_id UNINDEXED,source_name UNINDEXED,title,text,
			tokenize='unicode61 remove_diacritics 2')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create KB search fixture schema: %v", err)
		}
	}
	for key, value := range map[string]string{
		"snapshot_id": strings.Repeat("1", 64), "coverage_state": "PARTIAL",
		"coverage_report_sha256": strings.Repeat("2", 64), "source_coverage": `[]`,
	} {
		if _, err := db.Exec(`INSERT INTO kb_meta(key,value) VALUES(?,?)`, key, value); err != nil {
			t.Fatalf("insert KB metadata %s: %v", key, err)
		}
	}
	for n, row := range []struct {
		id, severity string
		score        float64
	}{
		{id: "cve-high", severity: "high", score: 7.0},
		{id: "cve-low", severity: "high", score: 6.9},
	} {
		text := "synthetic CVSS severity fixture " + row.id
		hash := sha256.Sum256([]byte(text))
		digest := hex.EncodeToString(hash[:])
		metadata, _ := json.Marshal(map[string]any{"cvss_score": row.score, "severity": row.severity})
		chunkID := strings.Repeat(string(rune('a'+n)), 64)
		if _, err := db.Exec(`INSERT INTO chunks(chunk_id,source_name,source_id,title,start_char,end_char,metadata_json,
			source_sha256,chunk_sha256,chunk_schema,text) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			chunkID, "nvd", row.id, row.id, 0, len(text), string(metadata), digest, digest, "bb-kb-chunk-v2", text); err != nil {
			t.Fatalf("insert KB fixture chunk %s: %v", row.id, err)
		}
		if _, err := db.Exec(`INSERT INTO chunk_search(chunk_id,source_name,title,text) VALUES(?,?,?,?)`, chunkID, "nvd", row.id, text); err != nil {
			t.Fatalf("insert KB fixture FTS row %s: %v", row.id, err)
		}
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
