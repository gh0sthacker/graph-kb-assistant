package graphaccess

import (
	"context"
	"errors"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

// ReadNeighborhood is the interactive, one-hop graph view. Neo4j verifies and
// loads only selected records and their dependencies; it makes no claim about
// full-project integrity or the global evidence closure used by Reader.Slice.
func ReadNeighborhood(ctx context.Context, selected *store.ProjectStore, seeds []string) (map[string]any, error) {
	return processService.ReadNeighborhood(ctx, selected, seeds)
}

// ReadNeighborhood loads a bounded one-hop graph view through this
// process-owned service.
func (s *Service) ReadNeighborhood(ctx context.Context, selected *store.ProjectStore, seeds []string) (map[string]any, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if selected == nil || selected.Database == nil || !selected.Database.ReadOnly() || len(seeds) < 1 || len(seeds) > 20 {
		return nil, errors.New("graph neighborhood requires one selected read-only project and 1-20 seeds")
	}
	options := behaviorgraph.SliceOptions{SeedIDs: seeds, MaxNodes: 80, MaxEdges: 160, MaxDepth: 1}
	var reader *behaviorgraph.Reader
	var generation int64
	var candidates int
	var candidateLimit bool
	var neighborhoodResult neo4jstore.Neighborhood
	var err error
	backend := ConfiguredBackend()
	switch backend {
	case "sqlite":
		reader, err = behaviorgraph.OpenReader(ctx, selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
	case "neo4j":
		graphStore, release, openErr := s.acquireNeo4jStore(ctx)
		if openErr != nil {
			return nil, openErr
		}
		neighborhoodResult, err = graphStore.ReadProjectNeighborhood(ctx, selected.ProjectKey, seeds, options.MaxEdges)
		release()
		if err != nil {
			return nil, err
		}
		generation, candidates, candidateLimit = neighborhoodResult.Generation, neighborhoodResult.CandidateCount, neighborhoodResult.Truncated
		reader, err = behaviorgraph.OpenReaderFromSnapshot(ctx, neighborhoodResult.Snapshot, selected.ProjectKey, selected.BehaviorReceiptPath)
	default:
		return nil, errors.New("configured graph backend is unsupported")
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	result, err := reader.Slice(ctx, options)
	if err != nil {
		return nil, err
	}
	delete(result, "slice_id") // The combined response gets its own result_id.
	result["view"] = "direct"
	result["coverage_state"] = "partial"
	result["incomplete"] = true
	result["analysis_cards_loaded"] = backend == "sqlite"
	result["project_snapshot_integrity"] = "not_assessed"
	result["integrity_scope"] = "selected_records"
	result["neighborhood_candidate_limit_reached"] = candidateLimit
	if backend == "neo4j" {
		result["graph_generation"] = generation
		result["candidate_count_at_least"] = candidates
	}
	if candidateLimit {
		result["truncated"] = true
	}
	limitations, _ := result["limitations"].([]string)
	result["limitations"] = append(limitations,
		"Interactive view loads direct neighbors only; expand a selected node to continue.",
		"Selected records are validated; full-project integrity and global evidence closure are not assessed in this view.")
	if backend == "neo4j" {
		result["limitations"] = append(result["limitations"].([]string), "Analysis cards are not loaded in the interactive Neo4j view; use the full slice for evidence analysis.")
		applyNeighborhoodProvenanceCoverage(result, neighborhoodResult)
	}
	return result, nil
}

func applyNeighborhoodProvenanceCoverage(result map[string]any, neighborhood neo4jstore.Neighborhood) {
	result["provenance_truncated"] = neighborhood.ProvenanceTruncated
	result["provenance_truncated_entity_ids"] = append([]string{}, neighborhood.TruncatedEntityIDs...)
	result["entity_source_ref_limit"] = neighborhood.EntitySourceRefLimit
	if !neighborhood.ProvenanceTruncated {
		return
	}
	limitations, _ := result["limitations"].([]string)
	result["limitations"] = append(limitations, "Entity source references are deterministically sampled; inspect the listed entities through a full evidence query for complete provenance.")
}
