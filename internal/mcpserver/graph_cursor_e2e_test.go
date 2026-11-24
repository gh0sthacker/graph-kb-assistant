package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
	"bb-graph-assistant/internal/webui"
)

const graphCursorFixtureProvenanceLimit = 64

type cursorFixtureGraphStore struct {
	projectKey string
	ownerID    string
	refs       []behaviorgraph.Record
	anchor     int
	calls      int
}

func (s *cursorFixtureGraphStore) ReadProjectSnapshot(context.Context, string) (behaviorgraph.Record, error) {
	return nil, fmt.Errorf("unexpected full snapshot read")
}

func (*cursorFixtureGraphStore) ReadProjectNeighborhood(context.Context, string, []string, int) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, fmt.Errorf("unexpected direct neighborhood read")
}

func (*cursorFixtureGraphStore) ReadProjectNeighborhoodPage(context.Context, string, []string, int, *neo4jstore.NeighborhoodCursor) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, fmt.Errorf("unexpected direct neighborhood page")
}

func (s *cursorFixtureGraphStore) ReadProjectAnalyticalSlice(_ context.Context, projectKey string, seedIDs []string, options neo4jstore.AnalyticalSliceOptions) (neo4jstore.AnalyticalSlice, error) {
	if projectKey != s.projectKey || len(seedIDs) != 1 || seedIDs[0] != s.ownerID {
		return neo4jstore.AnalyticalSlice{}, fmt.Errorf("unexpected analytical fixture identity")
	}
	s.calls++
	refs := s.refs[:graphCursorFixtureProvenanceLimit]
	truncated := true
	if len(options.ExcludedEntitySourceRefKeys) > 0 {
		anchor := s.refs[s.anchor]
		wantAnchor := s.ownerID + "|source-" + hex.EncodeToString(sha256Digest(anchor))
		if len(options.ExcludedEntitySourceRefKeys) != graphCursorFixtureProvenanceLimit ||
			len(options.EntitySourceRefAnchorKeys) != 1 || options.EntitySourceRefAnchorKeys[0] != wantAnchor {
			return neo4jstore.AnalyticalSlice{}, fmt.Errorf("provenance continuation exclusions or anchor are malformed: excluded=%d anchors=%v want=%s", len(options.ExcludedEntitySourceRefKeys), options.EntitySourceRefAnchorKeys, wantAnchor)
		}
		refs = []behaviorgraph.Record{anchor, s.refs[graphCursorFixtureProvenanceLimit], s.refs[graphCursorFixtureProvenanceLimit+1]}
		truncated = false
	}
	entity := behaviorgraph.Record{
		"id": s.ownerID, "kind": "endpoint", "data": map[string]any{"method": "GET"}, "source_refs": refs,
	}
	snapshot := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": s.projectKey,
		"sources": refs, "entities": []behaviorgraph.Record{entity},
		"assertions": []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{},
	}
	result := neo4jstore.AnalyticalSlice{
		Snapshot: snapshot, Generation: 7, FrontierHops: 0,
		ProvenanceTruncated: truncated, EntitySourceRefLimit: graphCursorFixtureProvenanceLimit,
	}
	if truncated {
		result.TruncatedEntityIDs = []string{s.ownerID}
	}
	return result, nil
}

func (*cursorFixtureGraphStore) ReadProjectObservationIDsByVariantSignatures(context.Context, string, string, string, int) ([]string, bool, error) {
	return nil, false, nil
}

func (*cursorFixtureGraphStore) ReadProjectFieldValueTraversalSeeds(context.Context, string, string, int) ([]string, bool, int64, error) {
	return nil, false, 0, fmt.Errorf("unexpected field-value traversal")
}

func (*cursorFixtureGraphStore) Close(context.Context) error { return nil }

func sha256Digest(record behaviorgraph.Record) []byte {
	encoded, _ := json.Marshal(record)
	digest := sha256.Sum256(encoded)
	return digest[:]
}

func TestFullGraphCursorRoundTripsThroughAppQueryHTTPAndMCP(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	t.Setenv("BB_ASSISTANT_NEO4J_URI", "bolt://127.0.0.1:1")
	t.Setenv("BB_ASSISTANT_NEO4J_USERNAME", "fixture")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "synthetic-password")
	t.Setenv("BB_ASSISTANT_NEO4J_DATABASE", "neo4j")

	ctx := context.Background()
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "cursor-e2e-instance", ProjectID: "cursor-e2e-project"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	for _, row := range []struct {
		requestID, observationID, method, route string
	}{
		{"surface-get-1", strings.Repeat("1", 64), "GET", "/items"},
		{"surface-get-2", strings.Repeat("2", 64), "GET", "/items"},
		{"surface-post-1", strings.Repeat("3", 64), "POST", "/submit"},
	} {
		insertGraphCursorSurfaceRow(t, ctx, projectStore, ref, row.requestID, row.observationID, row.method, row.route)
	}
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close synthetic project: %v", err)
	}
	projectKey := projectStore.ProjectKey
	ownerID := "endpoint-" + strings.Repeat("a", 64)
	graphStore := newCursorFixtureGraphStore(projectKey, ownerID)
	graphService := graphaccess.NewServiceWithFactory(func(context.Context, neo4jstore.Config) (graphaccess.GraphStore, error) {
		return graphStore, nil
	})
	queryService, err := appquery.NewGraphQueryService(root, []project.CaidoProjectRef{ref}, graphService)
	if err != nil {
		t.Fatalf("create appquery service: %v", err)
	}
	tools := &Tools{
		root: root, projects: []project.CaidoProjectRef{ref},
		projectsByKey: map[string]project.CaidoProjectRef{projectKey: ref},
		graphService:  graphService, graphQueries: queryService,
	}
	t.Cleanup(func() { _ = tools.Close() })

	handler, err := webui.NewHandler(tools)
	if err != nil {
		t.Fatalf("create synthetic HTTP handler: %v", err)
	}
	firstHTTP := graphHTTPPage(t, handler, projectKey, ownerID, "")
	firstHTTPCursor := cursorFromPage(t, firstHTTP)
	if countSourceRefs(t, firstHTTP, ownerID) != graphCursorFixtureProvenanceLimit {
		t.Fatalf("first HTTP page did not stop at the exact provenance page limit")
	}
	finalHTTP := graphHTTPPage(t, handler, projectKey, ownerID, firstHTTPCursor)
	assertFinalGraphPage(t, finalHTTP, ownerID)
	replayedHTTP := graphHTTPPage(t, handler, projectKey, ownerID, firstHTTPCursor)
	if replayedHTTP["result_id"] != finalHTTP["result_id"] {
		t.Fatalf("HTTP cursor replay returned a different accepted page")
	}

	server, err := NewServer(tools)
	if err != nil {
		t.Fatalf("create synthetic MCP server: %v", err)
	}
	arguments := map[string]any{
		"project_key": projectKey, "seed_ids": []string{ownerID},
		"max_nodes": 80, "max_edges": 160, "max_depth": 3,
	}
	firstMCP := callGraphSliceMCP(t, server, 1, arguments, true)
	firstMCPCursor := cursorFromPage(t, firstMCP)
	if countSourceRefs(t, firstMCP, ownerID) != graphCursorFixtureProvenanceLimit {
		t.Fatalf("first MCP page did not stop at the exact provenance page limit")
	}
	surfaceMCP := callMCPTool(t, server, 4, "bb_surface_map", map[string]any{"project_key": projectKey}, false)
	if firstMCP["total_operations"] != surfaceMCP["total_operations"] || firstMCP["total_observations"] != surfaceMCP["total_observations"] ||
		firstMCP["total_operations"] != float64(2) || firstMCP["total_observations"] != float64(3) {
		t.Fatalf("graph slice totals (%v, %v) do not match surface map totals (%v, %v)",
			firstMCP["total_operations"], firstMCP["total_observations"], surfaceMCP["total_operations"], surfaceMCP["total_observations"])
	}
	if note, _ := firstMCP["totals_coverage_note"].(string); !strings.Contains(note, "local structural indexes") {
		t.Fatalf("graph slice totals have no local-index coverage note: %q", note)
	}
	arguments["cursor"] = firstMCPCursor
	finalMCP := callGraphSliceMCP(t, server, 2, arguments, false)
	assertFinalGraphPage(t, finalMCP, ownerID)
	if finalMCP["total_operations"] != surfaceMCP["total_operations"] || finalMCP["total_observations"] != surfaceMCP["total_observations"] {
		t.Fatalf("continued graph slice totals (%v, %v) do not match surface map totals (%v, %v)",
			finalMCP["total_operations"], finalMCP["total_observations"], surfaceMCP["total_operations"], surfaceMCP["total_observations"])
	}
	replayedMCP := callGraphSliceMCP(t, server, 3, arguments, false)
	if replayedMCP["result_id"] != finalMCP["result_id"] {
		t.Fatalf("MCP cursor replay returned a different accepted page")
	}
	if graphStore.calls != 4 {
		t.Fatalf("synthetic native graph calls=%d, want two pages for each adapter", graphStore.calls)
	}
}

func newCursorFixtureGraphStore(projectKey, ownerID string) *cursorFixtureGraphStore {
	fixture := &cursorFixtureGraphStore{projectKey: projectKey, ownerID: ownerID}
	minimumSourceID := ""
	for i := 0; i < graphCursorFixtureProvenanceLimit+2; i++ {
		ref := behaviorgraph.Record{
			"path":           fmt.Sprintf("/synthetic/receipt-%02d.json", i),
			"sha256":         fmt.Sprintf("%064x", i+1),
			"record_locator": fmt.Sprintf("$.observations[%d]", i),
		}
		fixture.refs = append(fixture.refs, ref)
		if i < graphCursorFixtureProvenanceLimit {
			sourceID := "source-" + hex.EncodeToString(sha256Digest(ref))
			if minimumSourceID == "" || sourceID < minimumSourceID {
				minimumSourceID, fixture.anchor = sourceID, i
			}
		}
	}
	return fixture
}

func graphHTTPPage(t *testing.T, handler http.Handler, projectKey, seedID, cursor string) map[string]any {
	t.Helper()
	query := url.Values{"project_key": {projectKey}, "seed": {seedID}, "view": {"full"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?"+query.Encode(), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP graph page status=%d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode HTTP graph page: %v", err)
	}
	return payload
}

func callGraphSliceMCP(t *testing.T, server *Server, id int, arguments map[string]any, initialize bool) map[string]any {
	return callMCPTool(t, server, id, "bb_graph_slice", arguments, initialize)
}

func callMCPTool(t *testing.T, server *Server, id int, name string, arguments map[string]any, initialize bool) map[string]any {
	t.Helper()
	encodedArgs, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	var input strings.Builder
	if initialize {
		input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}` + "\n")
		input.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n")
	}
	input.WriteString(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, encodedArgs) + "\n")
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input.String()), &output); err != nil {
		t.Fatalf("serve MCP tool call: %v", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil || len(lines) == 0 {
		t.Fatalf("read MCP tool response: lines=%d err=%v", len(lines), err)
	}
	var response struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
			IsError           bool           `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &response); err != nil {
		t.Fatalf("decode MCP tool response: %v", err)
	}
	if response.Error != nil || response.Result.IsError || response.Result.StructuredContent == nil {
		t.Fatalf("MCP %s call failed: error=%v result=%#v", name, response.Error, response.Result)
	}
	return response.Result.StructuredContent
}

func insertGraphCursorSurfaceRow(t *testing.T, ctx context.Context, projectStore *store.ProjectStore, ref project.CaidoProjectRef, requestID, observationID, method, route string) {
	t.Helper()
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,request_size,request_vault_ref,parse_state)
		 VALUES(?,?,?,?,?,0,'fixture-vault-ref','complete')`, observationID, ref.InstanceID, ref.ProjectID, requestID, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("insert synthetic capture: %v", err)
	}
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO capture_search(observation_id,method,host,port,route,response_status,request_content_type,response_content_type,field_names_json,header_names_json)
		 VALUES(?,?,?,?,?,200,'','application/json','[]','[]')`, observationID, method, "api.fixture", 443, route); err != nil {
		t.Fatalf("insert synthetic capture search row: %v", err)
	}
}

func cursorFromPage(t *testing.T, payload map[string]any) string {
	t.Helper()
	cursor, ok := payload["next_cursor"].(string)
	if !ok || len(cursor) != 43 {
		t.Fatalf("graph page cursor=%v, want a 43-character continuation token", payload["next_cursor"])
	}
	return cursor
}

func countSourceRefs(t *testing.T, payload map[string]any, ownerID string) int {
	t.Helper()
	entities, ok := payload["entities"].([]any)
	if !ok {
		t.Fatalf("graph page entities have type %T", payload["entities"])
	}
	for _, value := range entities {
		entity, _ := value.(map[string]any)
		if entity["id"] == ownerID {
			refs, ok := entity["source_refs"].([]any)
			if !ok {
				t.Fatalf("source_refs have type %T", entity["source_refs"])
			}
			return len(refs)
		}
	}
	t.Fatalf("graph page omitted owner %q", ownerID)
	return 0
}

func assertFinalGraphPage(t *testing.T, payload map[string]any, ownerID string) {
	t.Helper()
	if _, exists := payload["next_cursor"]; exists || payload["continuation_branches_complete"] != true || payload["provenance_truncated"] != false ||
		countSourceRefs(t, payload, ownerID) != graphCursorFixtureProvenanceLimit+2 {
		t.Fatalf("final graph page did not merge all provenance and close its cursor: %#v", payload)
	}
}
