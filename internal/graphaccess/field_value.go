package graphaccess

import (
	"context"
	"errors"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

var ErrFieldValueIndexUnavailable = neo4jstore.ErrFieldValueIndexUnavailable

// FieldValueTraversalSeeds returns a small project-local seed set connecting
// one observation to other captures with the same safe keyed query value.
func (s *Service) FieldValueTraversalSeeds(
	ctx context.Context,
	selected *store.ProjectStore,
	observationID string,
	maxPaths int,
) ([]string, bool, int64, error) {
	if err := s.checkOpen(); err != nil {
		return nil, false, 0, err
	}
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil || !selected.Database.ReadOnly() {
		return nil, false, 0, errors.New("field-value traversal requires a read-only selected project store")
	}
	switch ConfiguredBackend() {
	case "sqlite":
		reader, err := behaviorgraph.OpenReader(ctx, selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
		if err != nil {
			return nil, false, 0, err
		}
		defer reader.Close()
		ids, truncated, err := reader.FieldValueTraversalSeeds(observationID, maxPaths)
		return ids, truncated, 0, err
	case "neo4j":
		graphStore, release, err := s.acquireNeo4jStore(ctx)
		if err != nil {
			return nil, false, 0, err
		}
		defer release()
		return graphStore.ReadProjectFieldValueTraversalSeeds(ctx, selected.ProjectKey, observationID, maxPaths)
	default:
		return nil, false, 0, errors.New("configured graph backend is unsupported")
	}
}
