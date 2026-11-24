package appquery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

func TestFullGraphCursorContinuesWhenMaxEdgesOmitsEveryBranch(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "fixture-password")

	ctx := context.Background()
	dataRoot := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "cursor-regression-instance", ProjectID: "cursor-regression-project"}
	projectKey, err := ref.StorageKey()
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	writable, err := store.CreateProject(ctx, ref, dataRoot)
	if err != nil {
		t.Fatalf("create local project fixture: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close local project fixture: %v", err)
	}

	snapshot, seedID := cursorRegressionSnapshot(t, projectKey)
	fixtureStore := &cursorRegressionGraphStore{snapshot: snapshot}
	graphService := graphaccess.NewServiceWithFactory(func(context.Context, neo4jstore.Config) (graphaccess.GraphStore, error) {
		return fixtureStore, nil
	})
	service, err := NewGraphQueryService(dataRoot, []project.CaidoProjectRef{ref}, graphService)
	if err != nil {
		t.Fatalf("create graph query service: %v", err)
	}
	t.Cleanup(func() { _ = service.Close(ctx) })

	request := GraphRequest{
		ProjectKey: projectKey,
		View:       GraphViewFull,
		SeedIDs:    []string{seedID},
		Budget:     GraphBudget{MaxNodes: 1, MaxEdges: 0, MaxDepth: 6},
	}
	first, err := service.Graph(ctx, request)
	if err != nil {
		var queryErr *QueryError
		if errors.As(err, &queryErr) {
			t.Fatalf("first page: %v (cause: %v)", err, queryErr.Err)
		}
		t.Fatalf("first page: %v", err)
	}
	counts, ok := first.Payload["counts"].(map[string]any)
	if !ok || graphRegressionInt(counts["edges"]) != 0 || graphRegressionInt(counts["entities"]) != 1 || graphRegressionInt(counts["cards"]) != 1 || graphRegressionInt(counts["nodes"]) != 1 {
		t.Fatalf("first page does not reproduce the zero-edge card case: counts=%#v", first.Payload["counts"])
	}
	if first.Payload["truncated"] != true || first.Payload["continuation_state"] != "available" {
		t.Fatalf("first page is not truncated with continuation available: truncated=%#v state=%#v",
			first.Payload["truncated"], first.Payload["continuation_state"])
	}
	branches, ok := first.Payload["missing_branches"].([]any)
	if !ok || len(branches) == 0 {
		t.Fatalf("first page has no omitted branches: %#v", first.Payload["missing_branches"])
	}
	for _, item := range branches {
		branch, ok := item.(map[string]any)
		if !ok || branch["reason"] != "max_edges" {
			t.Fatalf("first page branch was not omitted by max_edges: %#v", item)
		}
	}
	cursor, ok := first.Payload["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("first page has no fresh cursor: %#v", first.Payload["continuation_state"])
	}
	if _, leaked := first.Payload[search.GraphContinuationSnapshotField]; leaked {
		t.Fatal("internal continuation snapshot was exposed in the response")
	}

	request.Cursor = GraphCursor(cursor)
	second, err := service.Graph(ctx, request)
	if err != nil {
		t.Fatalf("continuation page: %v", err)
	}
	if _, leaked := second.Payload[search.GraphContinuationSnapshotField]; leaked {
		t.Fatal("internal continuation snapshot was exposed in the continuation response")
	}
	if next, ok := second.Payload["next_cursor"].(string); ok && next != "" {
		if next == cursor {
			t.Fatal("continuation returned the consumed cursor instead of a fresh cursor")
		}
		return
	}
	if second.Payload["continuation_branches_complete"] != true {
		t.Fatalf("continuation returned neither a fresh cursor nor an explicit complete state: state=%#v complete=%#v",
			second.Payload["continuation_state"], second.Payload["continuation_branches_complete"])
	}
}

func cursorRegressionSnapshot(t *testing.T, projectKey string) (behaviorgraph.Record, string) {
	t.Helper()
	status := 200
	requestTruncated, responseTruncated := false, false
	bundle, seedID, err := behaviorgraph.BuildHistoricalBundle(caido.HttpExchange{
		Ref: caido.ExchangeRef{
			Project:   caido.ProjectRef{InstanceID: "cursor-regression-instance", ProjectID: "cursor-regression-project"},
			RequestID: "request-1",
		},
		RequestRaw:  []byte("GET /items HTTP/1.1\r\nHost: app.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\n\r\n"), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "app.invalid", Port: 443,
		Target: "https://app.invalid/items", ResponseStatus: &status,
		RequestTruncated: &requestTruncated, ResponseTruncated: &responseTruncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}, projectKey, behaviorgraph.Record{
		"path": "/synthetic/cursor-regression.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.records[0]",
	})
	if err != nil {
		t.Fatalf("build graph fixture: %v", err)
	}
	entities, ok := bundle["entities"].([]behaviorgraph.Record)
	if !ok {
		t.Fatalf("historical fixture entities have type %T", bundle["entities"])
	}
	var endpointID string
	var observation behaviorgraph.Record
	for _, entity := range entities {
		if entity["kind"] == "endpoint" {
			endpointID, _ = entity["id"].(string)
		}
		if entity["id"] == seedID {
			observation = entity
		}
	}
	if endpointID == "" || observation == nil {
		t.Fatalf("historical fixture lacks endpoint or observation: endpoint=%q observation=%#v", endpointID, observation)
	}
	refs, ok := observation["source_refs"].([]behaviorgraph.Record)
	if !ok || len(refs) == 0 {
		t.Fatalf("historical observation has no source refs: %#v", observation["source_refs"])
	}
	cards, ok := bundle["cards"].([]behaviorgraph.Record)
	if !ok {
		t.Fatalf("historical fixture cards have type %T", bundle["cards"])
	}
	cards = append(cards, behaviorgraph.Record{
		"schema_version": json.Number("1"), "id": "card-" + strings.Repeat("c", 64),
		"capture_id": "capture-cursor-regression", "program": projectKey, "source_refs": refs,
		"endpoint":          behaviorgraph.Record{"id": endpointID, "method": "GET", "path": "/items"},
		"context":           behaviorgraph.Record{},
		"request_analysis":  behaviorgraph.Record{"parse_state": "complete"},
		"response_analysis": behaviorgraph.Record{"parse_state": "complete", "status": json.Number("200")},
		"business_action":   behaviorgraph.Record{"known_effect": "unknown"},
		"hypotheses":        []any{}, "missing_evidence": []any{}, "analysis_state": "needs_review",
	})
	bundle["cards"] = cards
	snapshot := behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"sources": bundle["sources"], "entities": bundle["entities"],
		"assertions": bundle["assertions"], "cards": cards,
	}
	return snapshot, seedID
}

func graphRegressionInt(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case float64:
		return int(number)
	default:
		return -1
	}
}

type cursorRegressionGraphStore struct {
	snapshot behaviorgraph.Record
}

func (s *cursorRegressionGraphStore) ReadProjectSnapshot(context.Context, string) (behaviorgraph.Record, error) {
	return nil, errors.New("unused in cursor regression")
}

func (s *cursorRegressionGraphStore) ReadProjectNeighborhood(context.Context, string, []string, int) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, errors.New("unused in cursor regression")
}

func (s *cursorRegressionGraphStore) ReadProjectNeighborhoodPage(context.Context, string, []string, int, *neo4jstore.NeighborhoodCursor) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, errors.New("unused in cursor regression")
}

func (s *cursorRegressionGraphStore) ReadProjectAnalyticalSlice(context.Context, string, []string, neo4jstore.AnalyticalSliceOptions) (neo4jstore.AnalyticalSlice, error) {
	assertions, _ := s.snapshot["assertions"].([]behaviorgraph.Record)
	cards, _ := s.snapshot["cards"].([]behaviorgraph.Record)
	return neo4jstore.AnalyticalSlice{
		Snapshot: s.snapshot, Generation: 7, CandidateAssertions: len(assertions), CandidateCards: len(cards),
	}, nil
}

func (s *cursorRegressionGraphStore) ReadProjectObservationIDsByVariantSignatures(context.Context, string, string, string, int) ([]string, bool, error) {
	return nil, false, errors.New("unused in cursor regression")
}

func (s *cursorRegressionGraphStore) ReadProjectFieldValueTraversalSeeds(context.Context, string, string, int) ([]string, bool, int64, error) {
	return nil, false, 0, errors.New("unused in cursor regression")
}

func (s *cursorRegressionGraphStore) Close(context.Context) error { return nil }
