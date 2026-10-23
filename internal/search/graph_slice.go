package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

const maxCaptureProvenanceRows = 1_000

// GraphSlice reads one project's existing BehaviorGraph and joins selected
// observations to exact historical/live Caido request IDs.
func GraphSlice(ctx context.Context, projectStore *store.ProjectStore, seedIDs []string, maxNodes, maxEdges, maxDepth int, selectedContext map[string]any) (map[string]any, error) {
	return GraphSliceWithService(ctx, graphaccess.DefaultService(), projectStore, seedIDs, maxNodes, maxEdges, maxDepth, selectedContext)
}

// GraphSliceWithService runs the shared graph query through a process-owned
// graph access service.
func GraphSliceWithService(ctx context.Context, service *graphaccess.Service, projectStore *store.ProjectStore, seedIDs []string, maxNodes, maxEdges, maxDepth int, selectedContext map[string]any) (map[string]any, error) {
	if err := validateGraphProjectStore(projectStore); err != nil {
		return nil, err
	}
	var reader graphaccess.Reader
	var nativeSlice neo4jstore.AnalyticalSlice
	backend := graphaccess.ConfiguredBackend()
	var err error
	switch backend {
	case "sqlite":
		reader, err = service.OpenReader(ctx, projectStore)
	case "neo4j":
		nativeSlice, err = service.ReadAnalyticalSlice(ctx, projectStore, seedIDs, neo4jstore.AnalyticalSliceOptions{
			MaxNodes: maxNodes, MaxEdges: maxEdges, MaxDepth: maxDepth,
		})
		if err == nil {
			reader, err = behaviorgraph.OpenReaderFromSnapshot(ctx, nativeSlice.Snapshot, projectStore.ProjectKey, projectStore.BehaviorReceiptPath)
		}
	default:
		return nil, errors.New("configured graph backend is unsupported")
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	slice, err := reader.Slice(ctx, behaviorgraph.SliceOptions{
		SeedIDs: seedIDs, MaxNodes: maxNodes, MaxEdges: maxEdges, MaxDepth: maxDepth, Context: selectedContext,
	})
	if err != nil {
		return nil, err
	}
	if backend == "neo4j" {
		slice["view"] = "analytical"
		slice["coverage_state"] = "partial"
		slice["incomplete"] = true
		slice["project_snapshot_integrity"] = "not_assessed"
		slice["integrity_scope"] = "selected_records"
		slice["analysis_closure_scope"] = "bounded_frontier"
		slice["analysis_closure_complete"] = false
		slice["graph_generation"] = nativeSlice.Generation
		slice["query_frontier_hops"] = nativeSlice.FrontierHops
		slice["candidate_assertion_count"] = nativeSlice.CandidateAssertions
		slice["candidate_assertion_limit_reached"] = nativeSlice.CandidateAssertionsTruncated
		slice["analytical_frontier_node_limit_reached"] = nativeSlice.FrontierNodeLimitReached
		slice["analytical_frontier_nodes_omitted"] = nativeSlice.FrontierNodesOmitted
		slice["candidate_card_count"] = nativeSlice.CandidateCards
		slice["candidate_card_limit_reached"] = nativeSlice.CandidateCardsTruncated
		slice["provenance_truncated"] = nativeSlice.ProvenanceTruncated
		slice["provenance_truncated_entity_ids"] = append([]string{}, nativeSlice.TruncatedEntityIDs...)
		slice["entity_source_ref_limit"] = nativeSlice.EntitySourceRefLimit
		if nativeSlice.CandidateAssertionsTruncated || nativeSlice.FrontierNodeLimitReached || nativeSlice.CandidateCardsTruncated || nativeSlice.ProvenanceTruncated {
			slice["truncated"] = true
		}
		limitations, _ := slice["limitations"].([]string)
		slice["limitations"] = append(limitations,
			"Neo4j analytical reads load a bounded selected-query frontier; full-project integrity is not assessed.",
			"Contradiction and evidence closure is limited to the loaded frontier and is not project-wide.",
		)
		if nativeSlice.CandidateAssertionsTruncated {
			slice["limitations"] = append(slice["limitations"].([]string), "The Neo4j assertion candidate budget was reached; additional frontier records were not loaded by this analytical request.")
		}
		if nativeSlice.FrontierNodeLimitReached {
			slice["limitations"] = append(slice["limitations"].([]string), "The Neo4j analytical frontier node budget was reached; omitted nodes require a separate focused neighborhood request.")
		}
		if nativeSlice.CandidateCardsTruncated {
			slice["limitations"] = append(slice["limitations"].([]string), "The Neo4j analysis-card candidate budget was reached; additional cards were not loaded.")
		}
		if nativeSlice.ProvenanceTruncated {
			slice["limitations"] = append(slice["limitations"].([]string), "Entity source references are deterministically sampled; the listed entities have incomplete provenance.")
		}
	}
	return attachGraphProvenance(ctx, projectStore, slice)
}

// GraphNeighborhood serves the interactive one-hop view without loading the
// entire Neo4j project. Full analysis slices continue through GraphSlice.
func GraphNeighborhood(ctx context.Context, projectStore *store.ProjectStore, seedIDs []string) (map[string]any, error) {
	return GraphNeighborhoodWithService(ctx, graphaccess.DefaultService(), projectStore, seedIDs)
}

// GraphNeighborhoodWithService runs the shared one-hop query through a
// process-owned graph access service.
func GraphNeighborhoodWithService(ctx context.Context, service *graphaccess.Service, projectStore *store.ProjectStore, seedIDs []string) (map[string]any, error) {
	page, err := GraphNeighborhoodPageWithService(ctx, service, projectStore, seedIDs, nil)
	if err != nil {
		return nil, err
	}
	return page.Payload, nil
}

// GraphNeighborhoodPageWithService runs one page of the shared one-hop read
// and retains the backend generation/cursor for application-level encoding.
func GraphNeighborhoodPageWithService(ctx context.Context, service *graphaccess.Service, projectStore *store.ProjectStore, seedIDs []string, cursor *neo4jstore.NeighborhoodCursor) (graphaccess.NeighborhoodPage, error) {
	if err := validateGraphProjectStore(projectStore); err != nil {
		return graphaccess.NeighborhoodPage{}, err
	}
	page, err := service.ReadNeighborhoodPage(ctx, projectStore, seedIDs, cursor)
	if err != nil {
		return graphaccess.NeighborhoodPage{}, err
	}
	page.Payload, err = attachGraphProvenance(ctx, projectStore, page.Payload)
	if err != nil {
		return graphaccess.NeighborhoodPage{}, err
	}
	return page, nil
}

func validateGraphProjectStore(projectStore *store.ProjectStore) error {
	if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil || !projectStore.Database.ReadOnly() {
		return errors.New("graph slice requires a read-only selected project store")
	}
	projectKey, err := projectStore.Project.StorageKey()
	if err != nil || projectKey != projectStore.ProjectKey {
		return errors.New("selected project identity is inconsistent")
	}
	projectDirectory, err := filepath.Abs(projectStore.Directory)
	if err != nil {
		return errors.New("selected project data directory is invalid")
	}
	directoryInfo, err := os.Lstat(projectDirectory)
	if err != nil || directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return errors.New("selected project data directory is unavailable")
	}
	expectedGraph, err := filepath.Abs(filepath.Join(projectDirectory, "behavior.sqlite3"))
	if err != nil {
		return errors.New("BehaviorGraph path is invalid")
	}
	selectedGraph, err := filepath.Abs(projectStore.BehaviorGraphPath)
	if err != nil || selectedGraph != expectedGraph {
		return errors.New("BehaviorGraph path is outside the selected project")
	}
	expectedReceipts, err := filepath.Abs(filepath.Join(projectDirectory, "behavior-receipts"))
	if err != nil {
		return errors.New("graph receipt directory is invalid")
	}
	selectedReceipts, err := filepath.Abs(projectStore.BehaviorReceiptPath)
	if err != nil || selectedReceipts != expectedReceipts {
		return errors.New("graph receipt directory is outside the selected project")
	}
	return nil
}

func attachGraphProvenance(ctx context.Context, projectStore *store.ProjectStore, slice map[string]any) (map[string]any, error) {
	observationIDs := make([]string, 0)
	entities, _ := slice["entities"].([]behaviorgraph.Record)
	for _, entity := range entities {
		if entity["kind"] == "observation" {
			if id, ok := entity["id"].(string); ok && id != "" {
				observationIDs = append(observationIDs, id)
			}
		}
	}
	sort.Strings(observationIDs)
	observationIDs = uniqueStrings(observationIDs)
	provenance, truncated, err := captureProvenance(ctx, projectStore, observationIDs)
	if err != nil {
		return nil, err
	}
	slice["project_key"] = projectStore.ProjectKey
	slice["capture_provenance"] = provenance
	slice["capture_provenance_truncated"] = truncated
	if truncated {
		slice["truncated"] = true
		slice["incomplete"] = true
	}
	encoded, err := json.Marshal(slice)
	if err != nil {
		return nil, errors.New("graph slice output could not be encoded")
	}
	// The graph library computes its local slice ID before the capture-store join.
	// This secondary digest pins the combined response without claiming that it
	// matches Python's full-snapshot digest.
	digest := sha256.Sum256(encoded)
	slice["result_id"] = fmt.Sprintf("result-%x", digest[:])
	encoded, err = json.Marshal(slice)
	if err != nil || len(encoded) > 8<<20 {
		return nil, errors.New("graph slice output exceeds its safe size limit")
	}
	return slice, nil
}

func captureProvenance(ctx context.Context, projectStore *store.ProjectStore, observationIDs []string) ([]map[string]any, bool, error) {
	if len(observationIDs) == 0 {
		return []map[string]any{}, false, nil
	}
	result := make([]map[string]any, 0)
	truncated := false
	for offset := 0; offset < len(observationIDs); offset += 400 {
		end := min(offset+400, len(observationIDs))
		batch := observationIDs[offset:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch)+3)
		for _, id := range batch {
			args = append(args, id)
		}
		args = append(args, projectStore.Project.InstanceID, projectStore.Project.ProjectID, maxCaptureProvenanceRows-len(result)+1)
		query := `SELECT gr.graph_observation_id,cr.revision_id,c.observation_id,c.request_id,
		                 cr.request_sha256,cr.response_sha256,cr.request_size,cr.response_size,
		                 cr.first_seen_at,cr.request_truncated,cr.response_truncated,
		                 cr.request_wire_fidelity,cr.response_wire_fidelity
		          FROM graph_capture_revisions gr
		          JOIN capture_revisions cr ON cr.revision_id=gr.revision_id
		          JOIN captures c ON c.observation_id=cr.observation_id
		          WHERE gr.graph_observation_id IN (` + placeholders + `)
		            AND c.caido_instance_id=? AND c.caido_project_id=?
		          ORDER BY cr.first_seen_at,cr.revision_id LIMIT ?`
		rows, err := projectStore.Database.DB().QueryContext(ctx, query, args...)
		if err != nil {
			return nil, false, errors.New("historical capture provenance could not be read")
		}
		for rows.Next() {
			var graphID, revisionID, observationID, requestID string
			var requestSHA string
			var responseSHA, firstSeen, requestFidelity, responseFidelity sql.NullString
			var requestSize int64
			var responseSize, requestTruncated, responseTruncated sql.NullInt64
			if err := rows.Scan(&graphID, &revisionID, &observationID, &requestID, &requestSHA, &responseSHA,
				&requestSize, &responseSize, &firstSeen, &requestTruncated, &responseTruncated,
				&requestFidelity, &responseFidelity); err != nil {
				_ = rows.Close()
				return nil, false, errors.New("historical capture provenance is malformed")
			}
			result = append(result, map[string]any{
				"graph_observation_id": graphID, "revision_id": revisionID, "observation_id": observationID,
				"request_id": requestID, "request_sha256": requestSHA, "response_sha256": nullableString(responseSHA),
				"request_size": requestSize, "response_size": nullableInt64(responseSize),
				"first_seen_at": nullableString(firstSeen), "request_truncated": nullableBool(requestTruncated),
				"response_truncated":     nullableBool(responseTruncated),
				"request_wire_fidelity":  valueOrUnknown(requestFidelity),
				"response_wire_fidelity": valueOrUnknown(responseFidelity),
				"record_source":          "encrypted_local_capture", "raw_stored_locally": true,
				"caido_request_reference": map[string]string{
					"instance_id": projectStore.Project.InstanceID, "project_id": projectStore.Project.ProjectID, "request_id": requestID,
				},
			})
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil {
			return nil, false, errors.New("historical capture provenance could not finish")
		}
		if len(result) > maxCaptureProvenanceRows {
			truncated = true
			result = result[:maxCaptureProvenanceRows]
			return result, truncated, nil
		}

		liveArgs := make([]any, 0, len(batch)*2+5)
		for _, id := range batch {
			liveArgs = append(liveArgs, id)
		}
		liveArgs = append(liveArgs, projectStore.Project.InstanceID, projectStore.Project.ProjectID)
		for _, id := range batch {
			liveArgs = append(liveArgs, id)
		}
		liveArgs = append(liveArgs, projectStore.Project.InstanceID, projectStore.Project.ProjectID, maxCaptureProvenanceRows-len(result)+1)
		liveQuery := `SELECT r.graph_observation_id,
	                     CASE WHEN r.legacy_mapping=1 THEN NULL ELSE r.revision_id END,
	                     CASE WHEN r.legacy_mapping=1 THEN 'legacy' ELSE 'revision' END,
	                     l.observation_id,l.request_id,
	                     CASE WHEN r.graph_observation_id=l.graph_observation_id THEN l.synced_at ELSE r.updated_at END,
		                     l.request_signature,
	                     CASE WHEN r.graph_observation_id=l.graph_observation_id THEN l.response_signature ELSE NULL END,
		                     l.request_size,
	                     CASE WHEN r.graph_observation_id=l.graph_observation_id THEN l.response_size ELSE NULL END,
	                     CASE WHEN r.graph_observation_id=l.graph_observation_id THEN l.parse_state ELSE 'unknown' END,
		                     l.caido_instance_id,l.caido_project_id
		              FROM live_graph_capture_revisions r
		              JOIN live_capture_index l ON l.observation_id=r.observation_id
		              WHERE r.graph_observation_id IN (` + placeholders + `)
		                AND l.caido_instance_id=? AND l.caido_project_id=?
		              UNION ALL
		              SELECT l.graph_observation_id,l.current_revision_id,'current',l.observation_id,l.request_id,l.synced_at,
		                     l.request_signature,l.response_signature,l.request_size,l.response_size,l.parse_state,
		                     l.caido_instance_id,l.caido_project_id
		              FROM live_capture_index l
		              WHERE l.graph_observation_id IN (` + placeholders + `)
		                AND l.caido_instance_id=? AND l.caido_project_id=?
		                AND NOT EXISTS (SELECT 1 FROM live_graph_capture_revisions r
		                                WHERE r.observation_id=l.observation_id
		                                  AND r.graph_observation_id=l.graph_observation_id)
		              ORDER BY 5,4 LIMIT ?`
		liveRows, err := projectStore.Database.DB().QueryContext(ctx, liveQuery, liveArgs...)
		if err != nil {
			return nil, false, errors.New("live capture provenance could not be read")
		}
		for liveRows.Next() {
			var graphID, revisionMapping, observationID, requestID, syncedAt, requestSignature, parseState, instanceID, projectID string
			var revisionID sql.NullString
			var responseSignature sql.NullString
			var requestSize int64
			var responseSize sql.NullInt64
			if err := liveRows.Scan(&graphID, &revisionID, &revisionMapping, &observationID, &requestID, &syncedAt, &requestSignature,
				&responseSignature, &requestSize, &responseSize, &parseState, &instanceID, &projectID); err != nil {
				_ = liveRows.Close()
				return nil, false, errors.New("live capture provenance is malformed")
			}
			result = append(result, map[string]any{
				"observation_id": observationID, "revision_id": nullableString(revisionID),
				"revision_mapping": revisionMapping,
				"request_id":       requestID, "graph_observation_id": graphID,
				"synced_at": syncedAt, "request_signature": requestSignature,
				"response_signature": nullableString(responseSignature), "request_size": requestSize,
				"response_size": nullableInt64(responseSize), "parse_state": parseState,
				"caido_instance_id": instanceID, "caido_project_id": projectID,
				"record_source": "caido_live_tail", "raw_stored_locally": false,
				"caido_request_reference": map[string]string{
					"instance_id": instanceID, "project_id": projectID, "request_id": requestID,
				},
			})
		}
		liveErr := liveRows.Err()
		liveCloseErr := liveRows.Close()
		if liveErr != nil || liveCloseErr != nil {
			return nil, false, errors.New("live capture provenance could not finish")
		}
		if len(result) > maxCaptureProvenanceRows {
			truncated = true
			result = result[:maxCaptureProvenanceRows]
			return result, truncated, nil
		}
	}
	return result, truncated, nil
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func valueOrUnknown(value sql.NullString) string {
	if !value.Valid || value.String == "" {
		return "unknown"
	}
	return value.String
}
