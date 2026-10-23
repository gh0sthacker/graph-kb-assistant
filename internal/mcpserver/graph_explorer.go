package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	capturesearch "bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

// GraphExplorerProjects returns one page of project identities from the local
// registry. It does not open project databases before the user selects one.
func (t *Tools) GraphExplorerProjects(ctx context.Context, afterProjectKey string, limit int) (map[string]any, error) {
	if limit < 1 || limit > maxProjectPageSize {
		return nil, errors.New("project page limit must be between 1 and 200")
	}
	start := 0
	if afterProjectKey != "" {
		if !projectKeyPattern.MatchString(afterProjectKey) {
			return nil, errors.New("project cursor has an invalid format")
		}
		found := false
		for index, ref := range t.projects {
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
	end := min(start+limit, len(t.projects))
	rows := make([]map[string]any, 0, end-start)
	for _, ref := range t.projects[start:end] {
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
	if end < len(t.projects) && end > start {
		next, _ = t.projects[end-1].StorageKey()
	}
	return map[string]any{
		"projects": rows, "project_count": len(rows), "total_project_count": len(t.projects),
		"next_after_project_key": next, "scope": "registered local project identities; project data is read after selection",
	}, nil
}

// GraphExplorerSurface reads one bounded page from exactly one selected
// project's local structural index.
func (t *Tools) GraphExplorerSurface(ctx context.Context, projectKey string, after *capturesearch.SurfaceCursor, limit int) (map[string]any, error) {
	if limit < 1 || limit > capturesearch.MaximumSurfaceScanSize {
		return nil, errors.New("surface limit is outside the supported range")
	}
	return t.surfaceMap(ctx, surfaceMapArgs{ProjectKey: projectKey, After: after, Limit: intPointer(limit)})
}

// GraphExplorerSlice returns a bounded, source-redacted graph neighborhood
// from exactly one selected project.
func (t *Tools) GraphExplorerSlice(ctx context.Context, projectKey string, seeds []string) (map[string]any, error) {
	return t.graphSlice(ctx, graphSliceArgs{
		ProjectKey: projectKey,
		SeedIDs:    seeds,
		MaxNodes:   intPointer(80),
		MaxEdges:   intPointer(160),
		MaxDepth:   intPointer(3),
	})
}

// GraphExplorerNeighborhood is the interactive web-only view. The MCP
// graph_slice tool retains the full validated reader and evidence semantics.
func (t *Tools) GraphExplorerNeighborhood(ctx context.Context, projectKey string, seeds []string) (map[string]any, error) {
	if !projectKeyPattern.MatchString(projectKey) || len(seeds) < 1 || len(seeds) > 20 {
		return nil, errors.New("select one project and 1-20 graph seeds")
	}
	ref, ok := t.projectsByKey[projectKey]
	if !ok {
		return nil, errors.New("selected project is not in the local catalog")
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, t.root)
	if err != nil {
		return nil, errors.New("selected project is unavailable for graph reads")
	}
	defer selected.Close()
	result, err := capturesearch.GraphNeighborhood(ctx, selected, seeds)
	if err != nil {
		return nil, errors.New("selected graph neighborhood is unavailable or exceeds its limits")
	}
	result["graph_version"] = "behavior-graph-direct-v1"
	result["coverage_note"] = "Direct structural neighborhood; coverage and full-project evidence integrity remain partial."
	result, redactedPaths, redactedLocators, err := redactGraphSourcePaths(result)
	if err != nil {
		return nil, errors.New("graph neighborhood could not be prepared")
	}
	result["local_source_paths_redacted"] = redactedPaths
	result["unsafe_source_locators_redacted"] = redactedLocators
	delete(result, "result_id")
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, errors.New("graph neighborhood could not be fingerprinted")
	}
	digest := sha256.Sum256(encoded)
	result["result_id"] = "result-" + hex.EncodeToString(digest[:])
	encoded, err = json.Marshal(result)
	if err != nil || len(encoded) > maxToolResultBytes {
		return nil, errors.New("graph neighborhood exceeds the output limit")
	}
	return result, nil
}

func intPointer(value int) *int { return &value }
