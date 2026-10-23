package appquery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bb-graph-assistant/internal/graphaccess"
)

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
		{name: "direct rejects budget", request: GraphRequest{ProjectKey: strings.Repeat("a", 64), View: GraphViewDirect, SeedIDs: []string{entity}, Budget: GraphBudget{MaxNodes: 1}}, wantErr: true},
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
