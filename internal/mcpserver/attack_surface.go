package mcpserver

import (
	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/hunt"
	"context"
	"errors"
)

type attackSurfaceArgs struct {
	ProjectKey string   `json:"project_key"`
	Classes    []string `json:"classes,omitempty"`
	Limit      *int     `json:"limit,omitempty"`
}

func (a attackSurfaceArgs) request() (appquery.AttackSurfaceRequest, error) {
	r := appquery.AttackSurfaceRequest{ProjectKey: a.ProjectKey, Classes: a.Classes, Limit: 50}
	if a.Limit != nil {
		if *a.Limit < 1 {
			return r, errors.New("attack-surface limit must be between 1 and 1000")
		}
		r.Limit = *a.Limit
	}
	return r, nil
}
func (t *Tools) AttackSurface(ctx context.Context, r appquery.AttackSurfaceRequest) (hunt.Result, error) {
	return t.graphQueries.AttackSurface(ctx, r)
}
func attackSurfaceToolSpec() toolSpec {
	return toolSpec{Name: "bb_attack_surface", Title: "Plan evidence-backed vulnerability checks", Description: "Rank deterministic hunting hypotheses from one project's imported captures. Returns endpoint, parameter, sink, score and why[] capture/observation references. Error co-occurrence is not a taint proof; candidates are not confirmed vulnerabilities. Missing live-body projections are explicit coverage_gaps. No scans, LLM, or writes.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"project_key"}, "properties": map[string]any{
		"project_key": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
		"classes":     map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string", "enum": hunt.Classes}},
		"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 50},
	}}, Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false}}
}
