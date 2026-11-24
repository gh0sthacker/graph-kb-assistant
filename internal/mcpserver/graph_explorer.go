package mcpserver

import (
	"context"
	"encoding/json"
	"errors"

	"bb-graph-assistant/internal/appquery"
)

// GraphExplorerProjects returns one page from the current local registry
// snapshot. It does not open project databases before the user selects one.
func (t *Tools) GraphExplorerProjects(ctx context.Context, afterProjectKey string, limit int) (map[string]any, error) {
	if limit < 1 || limit > maxProjectPageSize {
		return nil, errors.New("project page limit must be between 1 and 200")
	}
	projects := t.projectRefs()
	start := 0
	if afterProjectKey != "" {
		if !projectKeyPattern.MatchString(afterProjectKey) {
			return nil, errors.New("project cursor has an invalid format")
		}
		found := false
		for index, ref := range projects {
			key, _ := ref.StorageKey()
			if key == afterProjectKey {
				start, found = index+1, true
				break
			}
		}
		if !found {
			return nil, errors.New("project cursor is not in the local catalog")
		}
	}
	end := min(start+limit, len(projects))
	rows := make([]map[string]any, 0, end-start)
	for _, ref := range projects[start:end] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, err := ref.StorageKey()
		if err != nil {
			return nil, errors.New("local project identity is invalid")
		}
		rows = append(rows, map[string]any{
			"project_key": key, "instance_id": ref.InstanceID,
			"project_id": ref.ProjectID, "display_name": ref.DisplayName,
		})
	}
	var next any
	if end < len(projects) && end > start {
		next, _ = projects[end-1].StorageKey()
	}
	return map[string]any{
		"projects": rows, "project_count": len(rows), "total_project_count": len(projects),
		"next_after_project_key": next, "scope": "registered local project identities; project data is read after selection",
	}, nil
}

// GraphExplorerSurface reads a page from the shared, typed local operation catalog.
func (t *Tools) GraphExplorerSurface(ctx context.Context, request appquery.SurfaceRequest) (appquery.SurfaceResult, error) {
	if t.graphQueries == nil {
		return appquery.SurfaceResult{}, &appquery.QueryError{Code: appquery.ErrorGraphUnavailable, Message: "surface query service is unavailable"}
	}
	return t.graphQueries.Surface(ctx, request)
}

// GraphExplorerSlice returns a bounded, source-redacted graph neighborhood
// from exactly one selected project.
func (t *Tools) GraphExplorerSlice(ctx context.Context, projectKey string, seeds []string) (map[string]any, error) {
	result, err := t.Graph(ctx, appquery.GraphRequest{
		ProjectKey: projectKey, View: appquery.GraphViewFull, SeedIDs: seeds,
		Budget: appquery.GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	})
	if err != nil {
		return nil, err
	}
	return t.checkGraphWireSize(result.WireMap())
}

// GraphExplorerNeighborhood is the shared bounded one-hop use case for web and MCP.
func (t *Tools) GraphExplorerNeighborhood(ctx context.Context, projectKey string, seeds []string) (map[string]any, error) {
	return t.GraphExplorerNeighborhoodPage(ctx, projectKey, seeds, "")
}

func (t *Tools) GraphExplorerNeighborhoodPage(ctx context.Context, projectKey string, seeds []string, cursor string) (map[string]any, error) {
	result, err := t.Graph(ctx, appquery.GraphRequest{ProjectKey: projectKey, View: appquery.GraphViewDirect, SeedIDs: seeds, Cursor: appquery.GraphCursor(cursor)})
	if err != nil {
		return nil, err
	}
	return t.checkGraphWireSize(result.WireMap())
}

func (t *Tools) graphNeighborhood(ctx context.Context, args graphNeighborhoodArgs) (map[string]any, error) {
	// The direct view is fixed at one hop. Accept max_depth=1 as the explicit,
	// truthful spelling of that behavior; anything else must point the caller
	// at the tool that actually supports configurable depth.
	if args.MaxDepth != nil && *args.MaxDepth != 1 {
		return nil, errors.New("max_depth is not supported by bb_graph_neighborhood: it always reads exactly one hop (max_depth=1). Use bb_graph_slice with max_depth 0-6 for configurable traversal")
	}
	return t.GraphExplorerNeighborhoodPage(ctx, args.ProjectKey, args.SeedIDs, args.Cursor)
}

func (t *Tools) Graph(ctx context.Context, request appquery.GraphRequest) (appquery.GraphResult, error) {
	if t.graphQueries == nil {
		return appquery.GraphResult{}, &appquery.QueryError{Code: appquery.ErrorGraphUnavailable, Message: "graph query service is unavailable"}
	}
	return t.graphQueries.Graph(ctx, request)
}

// CaptureMessageRange exposes the exact local evidence resolver to the loopback
// UI only. It is deliberately absent from the MCP tool catalog and dispatch.
func (t *Tools) CaptureMessageRange(ctx context.Context, request appquery.CaptureMessageRangeRequest) (appquery.CaptureMessageRange, error) {
	if t.graphQueries == nil {
		return appquery.CaptureMessageRange{}, &appquery.QueryError{
			Code: appquery.ErrorEvidenceUnavailable, Message: "capture evidence service is unavailable",
		}
	}
	return t.graphQueries.CaptureMessageRange(ctx, request)
}

// CompareCaptureMessages exposes deterministic local revision comparison to
// the loopback UI only. It is deliberately absent from MCP dispatch.
func (t *Tools) CompareCaptureMessages(ctx context.Context, request appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error) {
	if t.graphQueries == nil {
		return appquery.CaptureComparison{}, &appquery.QueryError{
			Code: appquery.ErrorEvidenceUnavailable, Message: "capture comparison service is unavailable",
		}
	}
	return t.graphQueries.CompareCaptureMessages(ctx, request)
}

// BuildEvidencePacket is used by the loopback UI to preview exact selected
// historical spans. It is deliberately absent from MCP dispatch and never
// sends the packet to a model.
func (t *Tools) BuildEvidencePacket(ctx context.Context, request appquery.EvidencePacketRequest) (appquery.EvidencePacket, error) {
	if t.graphQueries == nil {
		return appquery.EvidencePacket{}, &appquery.QueryError{
			Code: appquery.ErrorEvidenceUnavailable, Message: "evidence packet service is unavailable",
		}
	}
	return t.graphQueries.BuildEvidencePacket(ctx, request)
}

func (t *Tools) checkGraphWireSize(wire map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, &appquery.QueryError{Code: appquery.ErrorGraphUnavailable, Message: "graph result could not be encoded", Err: errors.New("graph result encoding failed")}
	}
	if len(encoded) > maxToolResultBytes {
		return nil, &appquery.QueryError{Code: appquery.ErrorOutputTooLarge, Message: "graph result exceeds the output limit"}
	}
	return wire, nil
}

func intPointer(value int) *int { return &value }
