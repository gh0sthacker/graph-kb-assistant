package mcpserver

import (
	"context"
	"errors"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

// PreviewOperationBackfill is a web-UI maintenance use case. It is deliberately
// not registered in the model-facing MCP tool catalog.
func (t *Tools) PreviewOperationBackfill(
	ctx context.Context,
	projectKey string,
	options ingest.OperationBackfillOptions,
) (ingest.OperationBackfillPreview, error) {
	selected, err := t.operationBackfillProject(projectKey)
	if err != nil {
		return ingest.OperationBackfillPreview{}, err
	}
	directory, err := store.ProjectDirectory(selected, t.root)
	if err != nil {
		return ingest.OperationBackfillPreview{}, errors.New("selected project directory is unavailable")
	}
	maintenance, err := projectlock.Acquire(ctx, directory)
	if err != nil {
		return ingest.OperationBackfillPreview{}, errors.New("selected project is busy or unavailable for maintenance")
	}
	defer maintenance.Close()
	projectStore, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected, t.root, maintenance)
	if err != nil {
		return ingest.OperationBackfillPreview{}, errors.New("selected project cannot be opened read-only for operation preview")
	}
	preview, previewErr := ingest.PreviewHistoricalOperationBackfill(ctx, projectStore, maintenance, options)
	closeErr := projectStore.Close()
	preview.Bundles = nil
	if previewErr != nil || closeErr != nil {
		return ingest.OperationBackfillPreview{}, errors.New("operation backfill preview could not be completed")
	}
	return preview, nil
}

// ApplyOperationBackfill reauthenticates a complete preview under the same
// exclusive project lease, then imports its bundles into the local graph and
// durable Neo4j outbox. It never drains the outbox or calls a model.
func (t *Tools) ApplyOperationBackfill(
	ctx context.Context,
	projectKey string,
	candidateSHA256 string,
	options ingest.OperationBackfillOptions,
) (ingest.OperationBackfillApplyResult, error) {
	selected, err := t.operationBackfillProject(projectKey)
	if err != nil {
		return ingest.OperationBackfillApplyResult{}, err
	}
	directory, err := store.ProjectDirectory(selected, t.root)
	if err != nil {
		return ingest.OperationBackfillApplyResult{}, errors.New("selected project directory is unavailable")
	}
	maintenance, err := projectlock.Acquire(ctx, directory)
	if err != nil {
		return ingest.OperationBackfillApplyResult{}, errors.New("selected project is busy or unavailable for maintenance")
	}
	defer maintenance.Close()
	readOnlyProject, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected, t.root, maintenance)
	if err != nil {
		return ingest.OperationBackfillApplyResult{}, errors.New("selected project cannot be reopened for preview verification")
	}
	preview, previewErr := ingest.PreviewHistoricalOperationBackfill(ctx, readOnlyProject, maintenance, options)
	closeReadOnlyErr := readOnlyProject.Close()
	if previewErr != nil || closeReadOnlyErr != nil {
		return ingest.OperationBackfillApplyResult{}, errors.New("operation backfill preview could not be revalidated")
	}
	if preview.State != "ready" || preview.CandidateSHA256 != candidateSHA256 {
		return ingest.OperationBackfillApplyResult{}, ingest.ErrOperationBackfillCandidateChanged
	}
	projectStore, err := store.OpenProjectForWrite(ctx, selected, t.root)
	if err != nil {
		return ingest.OperationBackfillApplyResult{}, errors.New("selected project cannot be opened for the confirmed operation update")
	}
	result, applyErr := ingest.ApplyHistoricalOperationBackfill(ctx, projectStore, maintenance, preview, candidateSHA256)
	closeErr := projectStore.Close()
	if applyErr != nil || closeErr != nil {
		return result, errors.New("confirmed operation update did not finish cleanly; its outbox remains retryable")
	}
	return result, nil
}

func (t *Tools) operationBackfillProject(projectKey string) (project.CaidoProjectRef, error) {
	if t == nil || !projectKeyPattern.MatchString(projectKey) {
		return project.CaidoProjectRef{}, errors.New("selected project key is invalid")
	}
	selected, ok := t.projectsByKey[projectKey]
	if !ok {
		return project.CaidoProjectRef{}, errors.New("selected project is not registered")
	}
	return selected, nil
}
