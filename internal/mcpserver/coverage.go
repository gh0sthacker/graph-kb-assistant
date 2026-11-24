package mcpserver

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"bb-graph-assistant/internal/coverage"
)

type coverageMatrixArgs struct {
	Run string `json:"run"`
}

// CoverageMatrix reads a run's existing matrix without creating or changing it.
func (t *Tools) CoverageMatrix(ctx context.Context, run string) (*coverage.Matrix, error) {
	if strings.TrimSpace(run) == "" || !utf8.ValidString(run) || utf8.RuneCountInString(run) > 256 {
		return nil, errors.New("coverage run must contain 1-256 characters and cannot be blank")
	}
	if t == nil || t.root == "" {
		return nil, errors.New("coverage store is unavailable")
	}
	store, err := coverage.OpenReadOnly(filepath.Join(t.root, coverage.DatabaseName))
	if err != nil {
		return nil, errors.New("coverage database is unavailable")
	}
	defer store.Close()
	matrix, err := store.Load(ctx, run)
	if err != nil {
		if errors.Is(err, coverage.ErrRunNotFound) {
			return nil, coverage.ErrRunNotFound
		}
		return nil, errors.New("coverage matrix could not be read")
	}
	return matrix, nil
}

func coverageMatrixToolSpec() toolSpec {
	return toolSpec{
		Name: "bb_coverage_matrix", Title: "Read a run's class coverage matrix",
		Description: "Read the existing local coverage matrix for one run: cells contain class, status (confirmed, ruled_out, untested, blocked_first_party), tests, budget and evidence references. Returns coverage_pct, can_finish and open classes. can_finish is false while a budget is unmet or a positive-budget class is untested. Never claim there are no vulnerabilities when can_finish is false. Budget completion covers only declared classes; blocked or untested outcomes are not negative evidence, and even can_finish=true is not proof of absence. Missing runs are errors, not empty successful reports. Does not send requests or modify stored coverage.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"run": map[string]any{"type": "string", "minLength": 1, "maxLength": 256, "pattern": `\S`, "description": "Exact coverage run ID."}},
			"required":   []string{"run"},
		},
		Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
	}
}
