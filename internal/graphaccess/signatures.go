package graphaccess

import (
	"context"
	"errors"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

var ErrSignatureIndexUnavailable = neo4jstore.ErrSignatureIndexUnavailable

// ObservationIDsByVariantSignatures resolves exact request/response
// signatures through the selected graph backend. Neo4j uses native indexed
// lookups; SQLite uses the shared validated graph reader.
func (s *Service) ObservationIDsByVariantSignatures(
	ctx context.Context,
	selected *store.ProjectStore,
	requestSignature, responseSignature string,
	limit int,
) ([]string, bool, error) {
	if err := s.checkOpen(); err != nil {
		return nil, false, err
	}
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil || !selected.Database.ReadOnly() {
		return nil, false, errors.New("variant signature lookup requires a read-only selected project store")
	}
	switch ConfiguredBackend() {
	case "sqlite":
		reader, err := behaviorgraph.OpenReader(ctx, selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
		if err != nil {
			return nil, false, err
		}
		defer reader.Close()
		return reader.ObservationIDsByVariantSignatures(requestSignature, responseSignature, limit)
	case "neo4j":
		graphStore, release, err := s.acquireNeo4jStore(ctx)
		if err != nil {
			return nil, false, err
		}
		defer release()
		return graphStore.ReadProjectObservationIDsByVariantSignatures(
			ctx, selected.ProjectKey, requestSignature, responseSignature, limit,
		)
	default:
		return nil, false, errors.New("configured graph backend is unsupported")
	}
}
