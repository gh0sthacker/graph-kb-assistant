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
