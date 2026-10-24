package appquery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
)

func TestGraphQueryServiceCloseAndReopenPreservesFullCursor(t *testing.T) {
	dataRoot := t.TempDir()
	first, err := NewGraphQueryServiceWithPersistentCursors(dataRoot, nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("construct first graph query service: %v", err)
	}
	binding := testFullCursorBinding()
	branch := "assertion-" + strings.Repeat("c", 64)
	cursor, err := first.fullCursors.create(binding, 7, []string{branch}, map[string]any{"page": 0})
	if err != nil {
		_ = first.Close(context.Background())
		t.Fatalf("create continuation cursor: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("close first graph query service: %v", err)
	}

	second, err := NewGraphQueryServiceWithPersistentCursors(dataRoot, nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("reconstruct graph query service: %v", err)
	}
	defer second.Close(context.Background())
	continued, nextCursor, page, err := second.fullCursors.next(cursor, binding, func(branches []string, _ map[string]any, page int) (map[string]any, []string, int64, error) {
		if len(branches) != 1 || branches[0] != branch {
			t.Fatalf("reopened service cursor branches = %v, want %q", branches, branch)
		}
		return map[string]any{"page": page}, nil, 7, nil
	}, nil)
	if err != nil || page != 1 || nextCursor != "" || continued["continuation_branches_complete"] != true {
		t.Fatalf("continuation after service restart = %#v, next %q, page %d, err %v", continued, nextCursor, page, err)
	}
}

func TestGraphQueryRejectsNonEntityNeighborhoodSeeds(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	_, err = service.Graph(context.Background(), GraphRequest{
		ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect,
		SeedIDs: []string{"source-" + strings.Repeat("b", 64)},
	})
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("Graph() error = %v, want typed invalid-input error", err)
	}
}

func TestGraphRequestValidatesViewSpecificSeedAndBudgetRules(t *testing.T) {
	entity := "endpoint-" + strings.Repeat("b", 64)
	tests := []struct {
		name    string
		request GraphRequest
		wantErr bool
	}{
		{name: "direct entity", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect, SeedIDs: []string{entity}}},
		{name: "direct source is rejected", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect, SeedIDs: []string{"source-" + strings.Repeat("b", 64)}}, wantErr: true},
		{name: "direct assertion is rejected", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect, SeedIDs: []string{"assertion-" + strings.Repeat("b", 64)}}, wantErr: true},
		{name: "direct rejects budget", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect, SeedIDs: []string{entity}, Budget: GraphBudget{MaxNodes: 1}}, wantErr: true},
		{name: "direct rejects analysis context", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect, SeedIDs: []string{entity}, Context: map[string]any{"actor": "actor-a"}}, wantErr: true},
		{name: "full allows source provenance seed", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewFull, SeedIDs: []string{"source-" + strings.Repeat("b", 64)}}},
		{name: "full rejects partial budget", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewFull, SeedIDs: []string{entity}, Budget: GraphBudget{MaxNodes: 101, MaxEdges: 160, MaxDepth: 3}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateGraphRequest(test.request)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateGraphRequest() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestGraphContextHashIsStableAndDistinct(t *testing.T) {
	first, err := graphContextHash(map[string]any{"actor": "actor-a", "tenant": "tenant-a"})
	if err != nil {
		t.Fatalf("graphContextHash(first): %v", err)
	}
	second, err := graphContextHash(map[string]any{"tenant": "tenant-a", "actor": "actor-a"})
	if err != nil {
		t.Fatalf("graphContextHash(second): %v", err)
	}
	other, err := graphContextHash(map[string]any{"actor": "actor-b", "tenant": "tenant-a"})
	if err != nil {
		t.Fatalf("graphContextHash(other): %v", err)
	}
	if first == "" || first != second || first == other {
		t.Fatalf("context hashes are not stable and distinct: %q, %q, %q", first, second, other)
	}
	if _, err := graphContextHash(map[string]any{"unsupported": func() {}}); err == nil {
		t.Fatal("graphContextHash accepted a non-JSON value")
	}
	if _, err := graphContextHash(map[string]any{"oversized": strings.Repeat("x", maxGraphContextBytes)}); err == nil {
		t.Fatal("graphContextHash accepted a context above the byte limit")
	}
}

func TestPrepareGraphPayloadRedactsLocalSourcePathsAndPreservesWireFields(t *testing.T) {
	input := map[string]any{
		"schema_version": 1,
		"entities":       []any{map[string]any{"id": "endpoint-abc", "kind": "endpoint"}},
		"sources": []any{
			map[string]any{"sha256": "digest-a", "record_locator": "$[1]", "path": "/private/receipt.json"},
			map[string]any{"sha256": "digest-b", "record_locator": "/home/user/private.json", "path": "/home/user/private.json"},
		},
	}
	redacted, paths, locators, err := redactSourcePaths(input)
	if err != nil {
		t.Fatalf("redactSourcePaths: %v", err)
	}
	if paths != 2 || locators != 1 {
		t.Fatalf("redaction counts = paths %d, locators %d; want 2 and 1", paths, locators)
	}
	sources := redacted["sources"].([]any)
	safe := sources[0].(map[string]any)
	unsafe := sources[1].(map[string]any)
	if _, exists := safe["path"]; exists || safe["path_redacted"] != true || safe["record_locator"] != "$[1]" {
		t.Fatalf("safe source redaction mismatch: %#v", safe)
	}
	if _, exists := unsafe["path"]; exists || unsafe["path_redacted"] != true || unsafe["record_locator"] != "[REDACTED_UNSAFE_LOCATOR]" {
		t.Fatalf("unsafe source redaction mismatch: %#v", unsafe)
	}
	first, err := fingerprintGraphPayload(redacted)
	if err != nil {
		t.Fatalf("fingerprintGraphPayload: %v", err)
	}
	firstID := first["result_id"]
	secondInput, _, _, err := redactSourcePaths(input)
	if err != nil {
		t.Fatalf("second redactSourcePaths: %v", err)
	}
	second, err := fingerprintGraphPayload(secondInput)
	if err != nil {
		t.Fatalf("second fingerprintGraphPayload: %v", err)
	}
	if firstID == nil || firstID != second["result_id"] {
		t.Fatalf("result IDs are not stable: %v and %v", firstID, second["result_id"])
	}
	if first["schema_version"] != json.Number("1") || first["entities"] == nil || first["sources"] == nil {
		t.Fatalf("wire payload fields were lost: %#v", first)
	}
	if _, err := json.Marshal((GraphResult{Payload: first}).WireMap()); err != nil {
		t.Fatalf("WireMap is not JSON compatible: %v", err)
	}
}

func TestGraphQueryRequiresARegisteredProject(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	_, err = service.Graph(context.Background(), GraphRequest{
		ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect,
		SeedIDs: []string{"endpoint-" + strings.Repeat("b", 64)},
	})
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorProjectNotRegistered {
		t.Fatalf("Graph() error = %v, want typed project-not-registered error", err)
	}
}

func TestGraphQueryRejectsInvalidFullSliceBudget(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	_, err = service.Graph(context.Background(), GraphRequest{
		ProjectKey: strings.Repeat("a", 64), View: GraphViewFull,
		SeedIDs: []string{"endpoint-" + strings.Repeat("b", 64)},
		Budget:  GraphBudget{MaxNodes: 101, MaxEdges: 160, MaxDepth: 3},
	})
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("Graph() error = %v, want typed invalid-input error", err)
	}
}

func TestGraphCursorIsBoundToProjectAndSortedSeedSet(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	seedA := "endpoint-" + strings.Repeat("b", 64)
	seedB := "observation-" + strings.Repeat("c", 64)
	cursor := graphCursorData{
		Version: 1, ProjectKey: projectKey, SeedHash: graphSeedHash([]string{seedA, seedB}),
		Generation: 7, Priority: 1, AfterAssertionID: "assertion-" + strings.Repeat("d", 64),
	}
	token, err := encodeGraphCursor(cursor)
	if err != nil {
		t.Fatalf("encodeGraphCursor: %v", err)
	}
	decoded, err := decodeGraphCursor(token, projectKey, []string{seedB, seedA})
	if err != nil || decoded.Generation != 7 || decoded.AfterAssertionID != cursor.AfterAssertionID {
		t.Fatalf("round-trip decoded %#v, %v", decoded, err)
	}
	for _, test := range []struct {
		name       string
		projectKey string
		seeds      []string
		wantErr    ErrorCode
	}{
		{name: "other project", projectKey: strings.Repeat("e", 64), seeds: []string{seedA, seedB}, wantErr: ErrorInvalidInput},
		{name: "other seed set", projectKey: projectKey, seeds: []string{seedA}, wantErr: ErrorInvalidInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeGraphCursor(token, test.projectKey, test.seeds)
			var queryErr *QueryError
			if !errors.As(err, &queryErr) || queryErr.Code != test.wantErr {
				t.Fatalf("decodeGraphCursor() error = %v, want %s", err, test.wantErr)
			}
		})
	}
}

func TestGraphCursorRejectsMalformedAndAcceptsBoundedFullViewRequests(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	seed := "endpoint-" + strings.Repeat("b", 64)
	request := GraphRequest{ProjectKey: projectKey, View: GraphViewDirect, SeedIDs: []string{seed}, Cursor: GraphCursor("not-a-cursor")}
	_, err := decodeGraphCursor(string(request.Cursor), request.ProjectKey, request.SeedIDs)
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("malformed cursor error = %v, want invalid-input", err)
	}
	request.View = GraphViewFull
	request.Cursor = GraphCursor(strings.Repeat("A", 43))
	if err := validateGraphRequest(request); err != nil {
		t.Fatalf("full graph request rejected a bounded opaque continuation cursor: %v", err)
	}
	request.Cursor = GraphCursor("too-short")
	if err := validateGraphRequest(request); err == nil {
		t.Fatalf("full graph request accepted a malformed continuation cursor: %#v", request)
	}
}

func TestFullContinuationBranchSeedsIncludeResumableNativeOmissions(t *testing.T) {
	assertionA := "assertion-" + strings.Repeat("a", 64)
	assertionB := "assertion-" + strings.Repeat("b", 64)
	assertionC := "assertion-" + strings.Repeat("c", 64)
	frontier := "endpoint-" + strings.Repeat("c", 64)
	payload := map[string]any{
		"missing_branches": []behaviorgraph.Record{
			{"assertion_id": assertionB, "reason": "max_edges"},
			{"assertion_id": assertionA, "reason": "max_nodes"},
			{"assertion_id": assertionB, "reason": "mandatory_evidence_branch_omitted"},
			{"assertion_id": assertionC, "reason": "analytical_assertion_candidate_limit"},
			{"assertion_id": "assertion-" + strings.Repeat("d", 64), "reason": "max_depth"},
			{"assertion_id": "endpoint-" + strings.Repeat("d", 64), "reason": "max_edges"},
			{"frontier_id": frontier, "reason": "analytical_frontier_node_limit"},
			{"frontier_id": "source-" + strings.Repeat("e", 64), "reason": "analytical_frontier_node_limit"},
		},
	}
	got := fullContinuationBranchSeeds(payload)
	if len(got) != 4 || got[0] != assertionA || got[1] != assertionB || got[2] != assertionC || got[3] != frontier {
		t.Fatalf("resumable branch IDs = %v, want sorted assertion and frontier seeds", got)
	}
}

func TestFullContinuationBranchSeedsAcceptsJSONDecodedPayload(t *testing.T) {
	assertion := "assertion-" + strings.Repeat("a", 64)
	got := fullContinuationBranchSeeds(map[string]any{
		"missing_branches": []any{
			map[string]any{"assertion_id": assertion, "reason": "mandatory_evidence_branch_omitted"},
		},
	})
	if len(got) != 1 || got[0] != assertion {
		t.Fatalf("JSON-decoded resumable branch IDs = %v", got)
	}
}

func TestGraphPayloadGenerationAndNativeCandidateLimits(t *testing.T) {
	for _, value := range []any{int(7), int64(7), json.Number("7")} {
		if got := graphPayloadGeneration(map[string]any{"graph_generation": value}); got != 7 {
			t.Errorf("graph generation for %T = %d, want 7", value, got)
		}
	}
	if got := graphPayloadGeneration(map[string]any{"graph_generation": json.Number("invalid")}); got != 0 {
		t.Errorf("invalid graph generation = %d, want 0", got)
	}
	for _, flag := range []string{
		"candidate_assertion_limit_reached", "analytical_frontier_node_limit_reached",
		"candidate_card_limit_reached", "provenance_truncated", "provenance_cursor_truncated", "source_integrity_budget_exceeded",
	} {
		if !graphCandidateLimitReached(map[string]any{flag: true}) {
			t.Errorf("candidate limit flag %q was ignored", flag)
		}
	}
	if graphCandidateLimitReached(map[string]any{"candidate_assertion_limit_reached": false}) {
		t.Fatal("false candidate limit flag was treated as reached")
	}
}

func TestFullContinuationBranchSeedsIncludesCardCandidateOwner(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("e", 64)
	payload := map[string]any{
		"missing_branches": []behaviorgraph.Record{{
			"frontier_id": owner, "reason": "analytical_card_candidate_limit",
		}},
	}
	if got := fullContinuationBranchSeeds(payload); len(got) != 1 || got[0] != owner {
		t.Fatalf("card continuation owner seeds = %v, want [%q]", got, owner)
	}
}

func TestFullContinuationBranchSeedsIncludesTruncatedProvenanceOwner(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("e", 64)
	payload := map[string]any{
		"missing_branches": []behaviorgraph.Record{{
			"frontier_id": owner, "reason": "analytical_provenance_limit",
		}},
	}
	if got := fullContinuationBranchSeeds(payload); len(got) != 1 || got[0] != owner {
		t.Fatalf("provenance continuation owner seeds = %v, want [%q]", got, owner)
	}
}
