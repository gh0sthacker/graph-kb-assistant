// Package graphsync drains the durable SQLite-to-Neo4j graph outbox.
package graphsync

import (
	"context"
	"errors"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

const maxOutboxBatch = 100

type Result struct {
	State      string `json:"state"`
	Applied    int    `json:"applied"`
	Pending    int    `json:"pending"`
	ProjectKey string `json:"project_key"`
	Backend    string `json:"backend"`
}

// Drain applies pending events in order. It is a no-op while SQLite is the
// selected read backend; the durable outbox remains available for cutover.
func Drain(ctx context.Context, selected *store.ProjectStore) (Result, error) {
	if selected == nil || selected.ProjectKey == "" || selected.Database == nil || selected.Database.DB() == nil {
		return Result{}, errors.New("graph outbox sync requires one schema-validated selected project")
	}
	result := Result{State: "disabled", ProjectKey: selected.ProjectKey, Backend: graphaccess.ConfiguredBackend()}
	if result.Backend != "neo4j" {
		return result, nil
	}
	writer, err := behaviorgraph.OpenWriter(ctx, selected)
	if err != nil {
		return Result{}, err
	}
	return DrainWithWriter(ctx, selected, writer)
}

// DrainWithWriter applies pending events through an already validated local
// graph writer. The caller must hold the selected project's exclusive
// maintenance lease for the complete operation.
func DrainWithWriter(ctx context.Context, selected *store.ProjectStore, writer *behaviorgraph.Writer) (Result, error) {
	if selected == nil || selected.ProjectKey == "" || selected.Database == nil || selected.Database.DB() == nil ||
		writer == nil || !writer.MatchesProjectStore(selected) {
		return Result{}, errors.New("graph outbox sync requires a writer for the selected project")
	}
	result := Result{State: "disabled", ProjectKey: selected.ProjectKey, Backend: graphaccess.ConfiguredBackend()}
	if result.Backend != "neo4j" {
		return result, nil
	}
	items, err := writer.PendingOutbox(ctx, maxOutboxBatch)
	if err != nil {
		return Result{}, err
	}
	if len(items) == 0 {
		result.State = "ready"
		return result, nil
	}
	cfg, err := neo4jstore.ConfigFromEnv()
	if err != nil {
		return Result{}, err
	}
	graphStore, err := neo4jstore.New(ctx, cfg)
	if err != nil {
		return Result{State: "partial", ProjectKey: selected.ProjectKey, Backend: "neo4j"}, err
	}
	defer graphStore.Close(context.Background())
	for _, item := range items {
		if _, err := graphStore.ApplyOutboxBundle(ctx, selected.ProjectKey, item.BundleID, item.BundleJSON); err != nil {
			failureCtx, cancelFailure := context.WithTimeout(context.Background(), 2*time.Second)
			_ = writer.MarkOutboxFailed(failureCtx, item)
			cancelFailure()
			result.State = "partial"
			result.Pending = len(items) - result.Applied
			return result, errors.New("Neo4j graph sync is partial; the outbox event remains retryable")
		}
		if err := writer.MarkOutboxApplied(ctx, item); err != nil {
			result.State = "partial"
			result.Pending = len(items) - result.Applied
			return result, err
		}
		result.Applied++
	}
	remaining, err := writer.PendingOutbox(ctx, maxOutboxBatch)
	if err != nil {
		return Result{}, err
	}
	result.Pending = len(remaining)
	result.State = "ready"
	if result.Pending > 0 {
		result.State = "partial"
	}
	if result.Applied > 0 {
		graphaccess.Invalidate(selected.ProjectKey)
	}
	return result, nil
}

// DrainUntilBundleApplied advances ordered outbox pages until the requested
// bundle is acknowledged. Older backlog does not count as a failed projection
// delivery; context cancellation leaves the projection lease recoverable.
func DrainUntilBundleApplied(ctx context.Context, selected *store.ProjectStore, bundleID string) (Result, error) {
	if selected == nil || selected.ProjectKey == "" || selected.Database == nil || selected.Database.DB() == nil || bundleID == "" {
		return Result{}, errors.New("targeted graph outbox sync requires one selected project and bundle")
	}
	if backend := graphaccess.ConfiguredBackend(); backend != "neo4j" {
		return Result{State: "disabled", ProjectKey: selected.ProjectKey, Backend: backend}, nil
	}
	writer, err := behaviorgraph.OpenWriter(ctx, selected)
	if err != nil {
		return Result{}, err
	}
	return DrainUntilBundleAppliedWithWriter(ctx, selected, writer, bundleID)
}

// DrainUntilBundleAppliedWithWriter reuses a validated graph writer for the
// exact target check and all ordered outbox pages. The caller must hold the
// selected project's exclusive maintenance lease for the complete operation.
func DrainUntilBundleAppliedWithWriter(
	ctx context.Context,
	selected *store.ProjectStore,
	writer *behaviorgraph.Writer,
	bundleID string,
) (Result, error) {
	if selected == nil || selected.ProjectKey == "" || selected.Database == nil || selected.Database.DB() == nil ||
		writer == nil || !writer.MatchesProjectStore(selected) || bundleID == "" {
		return Result{}, errors.New("targeted graph outbox sync requires one selected project, matching writer and bundle")
	}
	if backend := graphaccess.ConfiguredBackend(); backend != "neo4j" {
		return Result{State: "disabled", ProjectKey: selected.ProjectKey, Backend: backend}, nil
	}
	return drainUntilBundleApplied(ctx, selected.ProjectKey, bundleID,
		func(ctx context.Context, target string) (bool, error) { return writer.OutboxBundleApplied(ctx, target) },
		func(ctx context.Context) (Result, error) { return DrainWithWriter(ctx, selected, writer) },
	)
}

func drainUntilBundleApplied(
	ctx context.Context,
	projectKey string,
	bundleID string,
	check func(context.Context, string) (bool, error),
	drain func(context.Context) (Result, error),
) (Result, error) {
	if ctx == nil || projectKey == "" || bundleID == "" || check == nil || drain == nil {
		return Result{}, errors.New("targeted graph outbox sync is invalid")
	}
	result := Result{State: "partial", ProjectKey: projectKey, Backend: "neo4j"}
	for {
		applied, err := check(ctx, bundleID)
		if err != nil {
			return result, err
		}
		if applied {
			result.State = "ready"
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, err := drain(ctx)
		result.Applied += page.Applied
		result.Pending = page.Pending
		if page.Backend != "" {
			result.Backend = page.Backend
		}
		if err != nil {
			// A drain page can acknowledge the target and then fail on a later
			// unrelated item. The target job is complete once its own durable
			// applied marker is present, even if the following backlog item failed.
			applied, checkErr := check(ctx, bundleID)
			if checkErr == nil && applied {
				result.State = "ready"
				return result, nil
			}
			return result, err
		}
		if page.State == "disabled" {
			result.State = "disabled"
			return result, nil
		}
		if page.Applied == 0 {
			return result, errors.New("Neo4j outbox made no progress before the target bundle was applied")
		}
	}
}
