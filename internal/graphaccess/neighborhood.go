package graphaccess

import (
	"context"
	"errors"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

const maxNeighborhoodSliceNodes = 100

var ErrNeighborhoodCursorUnsupported = errors.New("configured graph backend does not support generation-pinned continuation")
var ErrAnalyticalSliceBackendUnsupported = errors.New("configured graph backend does not support native analytical slices")

// ReadNeighborhood is the interactive, one-hop graph view. Neo4j verifies and
// loads only selected records and their dependencies; it makes no claim about
// full-project integrity or the global evidence closure used by Reader.Slice.
func ReadNeighborhood(ctx context.Context, selected *store.ProjectStore, seeds []string) (map[string]any, error) {
	return processService.ReadNeighborhood(ctx, selected, seeds)
}

type NeighborhoodPage struct {
	Payload    map[string]any
	Generation int64
	NextCursor *neo4jstore.NeighborhoodCursor
}

// ReadNeighborhood loads a bounded one-hop graph view through this
// process-owned service.
func (s *Service) ReadNeighborhood(ctx context.Context, selected *store.ProjectStore, seeds []string) (map[string]any, error) {
	page, err := s.ReadNeighborhoodPage(ctx, selected, seeds, nil)
	if err != nil {
		return nil, err
	}
	return page.Payload, nil
}

// ReadNeighborhoodPage loads one bounded one-hop page and returns its pinned
// generation and continuation anchor for the application layer.
func (s *Service) ReadNeighborhoodPage(ctx context.Context, selected *store.ProjectStore, seeds []string, cursor *neo4jstore.NeighborhoodCursor) (NeighborhoodPage, error) {
	if err := s.checkOpen(); err != nil {
		return NeighborhoodPage{}, err
	}
	if selected == nil || selected.Database == nil || !selected.Database.ReadOnly() || len(seeds) < 1 || len(seeds) > 20 {
		return NeighborhoodPage{}, errors.New("graph neighborhood requires one selected read-only project and 1-20 seeds")
	}
	if ConfiguredBackend() == "sqlite" && cursor != nil {
		return NeighborhoodPage{}, ErrNeighborhoodCursorUnsupported
	}
	options := neighborhoodSliceOptions(seeds)
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
			return NeighborhoodPage{}, openErr
		}
		neighborhoodResult, err = graphStore.ReadProjectNeighborhoodPage(ctx, selected.ProjectKey, seeds, options.MaxEdges, cursor)
		release()
		if err != nil {
			return NeighborhoodPage{}, err
		}
		generation, candidates, candidateLimit = neighborhoodResult.Generation, neighborhoodResult.CandidateCount, neighborhoodResult.Truncated
		reader, err = behaviorgraph.OpenReaderFromSnapshot(ctx, neighborhoodResult.Snapshot, selected.ProjectKey, selected.BehaviorReceiptPath)
	default:
		return NeighborhoodPage{}, errors.New("configured graph backend is unsupported")
	}
	if err != nil {
		return NeighborhoodPage{}, err
	}
	defer reader.Close()
	result, err := reader.Slice(ctx, options)
	if err != nil {
		return NeighborhoodPage{}, err
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
	return NeighborhoodPage{Payload: result, Generation: generation, NextCursor: neighborhoodResult.NextCursor}, nil
}

// ReadAnalyticalSlice uses Neo4j's bounded selected-query path. It deliberately
// does not fall back to the full snapshot reader when Neo4j is selected.
func (s *Service) ReadAnalyticalSlice(ctx context.Context, selected *store.ProjectStore, seeds []string, options neo4jstore.AnalyticalSliceOptions) (neo4jstore.AnalyticalSlice, error) {
	if err := s.checkOpen(); err != nil {
		return neo4jstore.AnalyticalSlice{}, err
	}
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil || !selected.Database.ReadOnly() || len(seeds) < 1 || len(seeds) > 80 {
		return neo4jstore.AnalyticalSlice{}, errors.New("analytical graph slice requires one selected read-only project and 1-80 seeds")
	}
	switch ConfiguredBackend() {
	case "sqlite":
		return neo4jstore.AnalyticalSlice{}, ErrAnalyticalSliceBackendUnsupported
	case "neo4j":
		graphStore, release, err := s.acquireNeo4jStore(ctx)
		if err != nil {
			return neo4jstore.AnalyticalSlice{}, err
		}
		defer release()
		return graphStore.ReadProjectAnalyticalSlice(ctx, selected.ProjectKey, seeds, options)
	default:
		return neo4jstore.AnalyticalSlice{}, errors.New("configured graph backend is unsupported")
	}
}

func neighborhoodSliceOptions(seeds []string) behaviorgraph.SliceOptions {
	return behaviorgraph.SliceOptions{
		SeedIDs: seeds, MaxNodes: maxNeighborhoodSliceNodes,
		MaxEdges: maxNeighborhoodSliceNodes - len(seeds), MaxDepth: 1,
	}
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
