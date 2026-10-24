package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

const maxCaptureProvenanceRows = 1_000
const maxContinuationExcludedAssertionIDs = 10_000
const maxContinuationExcludedCardIDs = 10_000
const maxContinuationExcludedEntitySourceRefs = 10_000
const maxContinuationEntitySourceRefAnchors = 5_000

var continuationEntityRecordIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*-[a-f0-9]{64}$`)

var ErrAnalyticalGenerationChanged = errors.New("Neo4j graph generation changed during analytical continuation")

// GraphSlice reads one project's existing BehaviorGraph and joins selected
// observations to exact historical/live Caido request IDs.
func GraphSlice(ctx context.Context, projectStore *store.ProjectStore, seedIDs []string, maxNodes, maxEdges, maxDepth int, selectedContext map[string]any) (map[string]any, error) {
	return GraphSliceWithService(ctx, graphaccess.DefaultService(), projectStore, seedIDs, maxNodes, maxEdges, maxDepth, selectedContext)
}

// GraphSliceWithService runs the shared graph query through a process-owned
// graph access service.
func GraphSliceWithService(ctx context.Context, service *graphaccess.Service, projectStore *store.ProjectStore, seedIDs []string, maxNodes, maxEdges, maxDepth int, selectedContext map[string]any) (map[string]any, error) {
	return graphSliceWithExclusions(ctx, service, projectStore, seedIDs, maxNodes, maxEdges, maxDepth, selectedContext, nil, nil, nil, nil)
}

func graphSliceWithExclusions(ctx context.Context, service *graphaccess.Service, projectStore *store.ProjectStore, seedIDs []string, maxNodes, maxEdges, maxDepth int, selectedContext map[string]any, excludedAssertionIDs, excludedCardIDs, excludedEntitySourceRefKeys, entitySourceRefAnchorKeys []string) (map[string]any, error) {
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
			ExcludedAssertionIDs: excludedAssertionIDs, ExcludedCardIDs: excludedCardIDs,
			ExcludedEntitySourceRefKeys: excludedEntitySourceRefKeys, EntitySourceRefAnchorKeys: entitySourceRefAnchorKeys,
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
		if len(nativeSlice.CandidateAssertionIDsOmitted) > 0 {
			branches, _ := slice["missing_branches"].([]behaviorgraph.Record)
			for _, id := range nativeSlice.CandidateAssertionIDsOmitted {
				branches = append(branches, behaviorgraph.Record{
					"assertion_id": id, "reason": "analytical_assertion_candidate_limit",
				})
			}
			slice["missing_branches"] = branches
		}
		if nativeSlice.AssertionSeedsTruncated {
			slice["missing_branches_truncated"] = true
		}
		if len(nativeSlice.CandidateCardOwnerIDsOmitted) > 0 {
			branches, _ := slice["missing_branches"].([]behaviorgraph.Record)
			for _, id := range nativeSlice.CandidateCardOwnerIDsOmitted {
				branches = append(branches, behaviorgraph.Record{
					"frontier_id": id, "reason": "analytical_card_candidate_limit",
				})
			}
			slice["missing_branches"] = branches
		}
		if nativeSlice.CardSeedsTruncated {
			slice["missing_branches_truncated"] = true
		}
		if len(nativeSlice.TruncatedEntityIDs) > 0 {
			branches, _ := slice["missing_branches"].([]behaviorgraph.Record)
			for _, id := range nativeSlice.TruncatedEntityIDs {
				branches = append(branches, behaviorgraph.Record{
					"frontier_id": id, "reason": "analytical_provenance_limit",
				})
			}
			slice["missing_branches"] = branches
		}
		if nativeSlice.ProvenanceSeedsTruncated {
			slice["missing_branches_truncated"] = true
		}
		slice["analytical_frontier_node_limit_reached"] = nativeSlice.FrontierNodeLimitReached
		slice["analytical_frontier_nodes_omitted"] = nativeSlice.FrontierNodesOmitted
		if len(nativeSlice.FrontierNodeIDsOmitted) > 0 {
			branches, _ := slice["missing_branches"].([]behaviorgraph.Record)
			for _, id := range nativeSlice.FrontierNodeIDsOmitted {
				branches = append(branches, behaviorgraph.Record{
					"frontier_id": id, "reason": "analytical_frontier_node_limit",
				})
			}
			slice["missing_branches"] = branches
		}
		if nativeSlice.FrontierSeedsTruncated {
			slice["missing_branches_truncated"] = true
		}
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
			slice["limitations"] = append(slice["limitations"].([]string), "The Neo4j global assertion candidate budget was reached; omitted assertion IDs are available through generation-pinned continuation when the bounded queue permits.")
		}
		if nativeSlice.FrontierNodeLimitReached {
			slice["limitations"] = append(slice["limitations"].([]string), "The Neo4j analytical frontier node budget was reached; omitted node branches are available through generation-pinned graph continuation when the bounded queue permits.")
		}
		if nativeSlice.CandidateCardsTruncated {
			slice["limitations"] = append(slice["limitations"].([]string), "The Neo4j analysis-card candidate budget was reached; owner branches can continue through the generation-pinned cursor when the bounded queue permits.")
		}
		if nativeSlice.ProvenanceTruncated {
			slice["limitations"] = append(slice["limitations"].([]string), "Entity source references are deterministically sampled; the listed entities have incomplete provenance.")
		}
	}
	return attachGraphProvenance(ctx, projectStore, slice)
}

// ContinueGraphSliceWithService adds one bounded set of omitted renderer or
// native-frontier branches to the accumulated selected records, then reruns
// the shared Reader from the original seeds. This preserves one cumulative
// graph interpretation instead of concatenating independently rendered slices.
func ContinueGraphSliceWithService(
	ctx context.Context,
	service *graphaccess.Service,
	projectStore *store.ProjectStore,
	originalSeedIDs, branchSeedIDs []string,
	maxNodes, maxEdges, maxDepth, page int,
	selectedContext map[string]any,
	accumulated map[string]any,
) (map[string]any, error) {
	if page < 1 || page > 5_000 || accumulated == nil || len(originalSeedIDs) == 0 || len(branchSeedIDs) == 0 {
		return nil, errors.New("analytical graph continuation input is invalid")
	}
	if generationFromGraphPayload(accumulated) < 1 {
		return nil, errors.New("analytical graph continuation has no pinned generation")
	}
	excludedAssertionIDs, excludedTruncated, err := continuationExcludedAssertionIDs(accumulated["assertions"])
	if err != nil {
		return nil, err
	}
	excludedCardIDs, excludedCardsTruncated, err := continuationExcludedCardIDs(accumulated["cards"])
	if err != nil {
		return nil, err
	}
	excludedEntitySourceRefKeys, entitySourceRefAnchorKeys, provenanceCursorTruncated, err := continuationEntitySourceRefState(accumulated["entities"], branchSeedIDs)
	if err != nil {
		return nil, err
	}
	nextPage, err := graphSliceWithExclusions(ctx, service, projectStore, branchSeedIDs, maxNodes, maxEdges, maxDepth, selectedContext,
		excludedAssertionIDs, excludedCardIDs, excludedEntitySourceRefKeys, entitySourceRefAnchorKeys)
	if err != nil {
		return nil, err
	}
	if excludedTruncated {
		nextPage["candidate_assertion_exclusion_truncated"] = true
		nextPage["truncated"] = true
		limitations := graphStringList(nextPage["limitations"])
		limitations = append(limitations, "Continuation excluded at most 10,000 previously loaded assertions; larger accumulated views may revisit candidates.")
		nextPage["limitations"] = uniqueStrings(limitations)
	}
	if excludedCardsTruncated {
		nextPage["candidate_card_exclusion_truncated"] = true
		nextPage["truncated"] = true
		limitations := graphStringList(nextPage["limitations"])
		limitations = append(limitations, "Continuation excluded at most 10,000 previously loaded cards; larger accumulated views may revisit candidates.")
		nextPage["limitations"] = uniqueStrings(limitations)
	}
	if provenanceCursorTruncated {
		nextPage["provenance_cursor_truncated"] = true
		nextPage["truncated"] = true
		limitations := graphStringList(nextPage["limitations"])
		limitations = append(limitations, "Continuation retained at most 10,000 entity source references and 5,000 provenance anchors; larger views may revisit or omit provenance branches.")
		nextPage["limitations"] = uniqueStrings(limitations)
	}
	if generationFromGraphPayload(nextPage) != generationFromGraphPayload(accumulated) {
		return nil, ErrAnalyticalGenerationChanged
	}

	snapshot, counts, err := mergeAnalyticalSnapshots(projectStore.ProjectKey, accumulated, nextPage)
	if err != nil {
		return nil, err
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, snapshot, projectStore.ProjectKey, projectStore.BehaviorReceiptPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	continuedNodes := min(100, maxNodes+20*page)
	continuedEdges := min(300, maxEdges+60*page)
	slice, err := reader.Slice(ctx, behaviorgraph.SliceOptions{
		SeedIDs: originalSeedIDs, MaxNodes: continuedNodes, MaxEdges: continuedEdges, MaxDepth: maxDepth, Context: selectedContext,
	})
	if err != nil {
		return nil, err
	}
	slice["graph_generation"] = generationFromGraphPayload(accumulated)
	slice["view"] = "analytical"
	slice["coverage_state"] = "partial"
	slice["incomplete"] = true
	slice["project_snapshot_integrity"] = "not_assessed"
	slice["integrity_scope"] = "selected_records"
	slice["analysis_closure_scope"] = "bounded_frontier"
	slice["analysis_closure_complete"] = false
	slice["query_frontier_hops"] = maxIntValue(accumulated["query_frontier_hops"], nextPage["query_frontier_hops"])
	slice["candidate_assertion_count"] = counts["assertions"]
	slice["candidate_card_count"] = counts["cards"]
	slice["candidate_assertion_limit_reached"] = graphPayloadFlag(nextPage, "candidate_assertion_limit_reached")
	slice["candidate_assertion_exclusion_truncated"] = anyGraphFlag(accumulated, nextPage, "candidate_assertion_exclusion_truncated")
	slice["analytical_frontier_node_limit_reached"] = graphPayloadFlag(nextPage, "analytical_frontier_node_limit_reached")
	slice["candidate_card_limit_reached"] = graphPayloadFlag(nextPage, "candidate_card_limit_reached")
	slice["candidate_card_exclusion_truncated"] = anyGraphFlag(accumulated, nextPage, "candidate_card_exclusion_truncated")
	slice["provenance_truncated"] = graphPayloadFlag(nextPage, "provenance_truncated")
	slice["provenance_cursor_truncated"] = anyGraphFlag(accumulated, nextPage, "provenance_cursor_truncated")
	branches, branchesTruncated := mergeContinuationBranches(slice["missing_branches"], nextPage["missing_branches"])
	slice["missing_branches"] = branches
	if branchesTruncated || graphPayloadFlag(accumulated, "missing_branches_truncated") ||
		graphPayloadFlag(nextPage, "missing_branches_truncated") {
		slice["missing_branches_truncated"] = true
	}
	// Renderer omissions may resolve as the cumulative slice grows. Assertion,
	// frontier, card, and provenance flags describe the newest native page;
	// cursor-cap flags remain sticky because they can affect later pages.
	slice["truncated"] = analyticalContinuationTruncated(slice, accumulated, nextPage)
	slice["provenance_truncated_entity_ids"] = graphStringList(nextPage["provenance_truncated_entity_ids"])
	slice["entity_source_ref_limit"] = maxIntValue(accumulated["entity_source_ref_limit"], nextPage["entity_source_ref_limit"])
	limitations := append(graphStringList(accumulated["limitations"]), graphStringList(nextPage["limitations"])...)
	slice["limitations"] = uniqueStrings(limitations)
	return attachGraphProvenance(ctx, projectStore, slice)
}

const maxMergedContinuationBranches = 5_000

func mergeContinuationBranches(values ...any) ([]behaviorgraph.Record, bool) {
	merged := make(map[string]behaviorgraph.Record)
	for _, value := range values {
		for _, branch := range graphBranchRecords(value) {
			id := graphBranchSeedID(branch)
			reason, _ := branch["reason"].(string)
			if id == "" || reason == "" {
				continue
			}
			merged[reason+"\x00"+id] = branch
		}
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	truncated := len(keys) > maxMergedContinuationBranches
	if truncated {
		keys = keys[:maxMergedContinuationBranches]
	}
	branches := make([]behaviorgraph.Record, 0, len(keys))
	for _, key := range keys {
		branches = append(branches, merged[key])
	}
	return branches, truncated
}

func graphBranchRecords(value any) []behaviorgraph.Record {
	var records []behaviorgraph.Record
	switch rows := value.(type) {
	case []behaviorgraph.Record:
		return rows
	case []map[string]any:
		for _, row := range rows {
			records = append(records, behaviorgraph.Record(row))
		}
	case []any:
		for _, item := range rows {
			if row, ok := item.(map[string]any); ok {
				records = append(records, behaviorgraph.Record(row))
			}
		}
	}
	return records
}

func graphBranchSeedID(branch behaviorgraph.Record) string {
	if id, ok := branch["assertion_id"].(string); ok {
		return id
	}
	id, _ := branch["frontier_id"].(string)
	return id
}

func continuationExcludedAssertionIDs(value any) ([]string, bool, error) {
	records, ok := graphRecordList(value)
	if !ok {
		return nil, false, errors.New("analytical graph continuation assertion list is malformed")
	}
	seen := make(map[string]bool, len(records))
	ids := make([]string, 0, len(records))
	for _, record := range records {
		id, ok := record["id"].(string)
		if !ok || !strings.HasPrefix(id, "assertion-") {
			continue
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	truncated := len(ids) > maxContinuationExcludedAssertionIDs
	if truncated {
		ids = ids[:maxContinuationExcludedAssertionIDs]
	}
	return ids, truncated, nil
}

func continuationExcludedCardIDs(value any) ([]string, bool, error) {
	records, ok := graphRecordList(value)
	if !ok {
		return nil, false, errors.New("analytical graph continuation card list is malformed")
	}
	seen := make(map[string]bool, len(records))
	ids := make([]string, 0, len(records))
	for _, record := range records {
		id, ok := record["id"].(string)
		if !ok || !strings.HasPrefix(id, "card-") {
			continue
		}
		if !digestPattern.MatchString(strings.TrimPrefix(id, "card-")) {
			return nil, false, errors.New("analytical graph continuation card identity is malformed")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	truncated := len(ids) > maxContinuationExcludedCardIDs
	if truncated {
		ids = ids[:maxContinuationExcludedCardIDs]
	}
	return ids, truncated, nil
}

func continuationEntitySourceRefState(value any, ownerIDs []string) ([]string, []string, bool, error) {
	entities, ok := graphRecordList(value)
	if !ok {
		return nil, nil, false, errors.New("analytical graph continuation entity list is malformed")
	}
	wanted := make(map[string]bool, len(ownerIDs))
	for _, id := range ownerIDs {
		if !continuationEntityRecordIDPattern.MatchString(id) || strings.HasPrefix(id, "source-") || strings.HasPrefix(id, "card-") {
			continue
		}
		wanted[id] = true
	}
	pairs := make(map[string]bool)
	anchors := make([]string, 0, len(wanted))
	for _, entity := range entities {
		ownerID, ok := entity["id"].(string)
		if !ok || !wanted[ownerID] {
			continue
		}
		refs, ok := graphRecordList(entity["source_refs"])
		if !ok || len(refs) == 0 {
			return nil, nil, false, errors.New("analytical graph continuation entity provenance is malformed")
		}
		sourceIDs := make([]string, 0, len(refs))
		seenSources := make(map[string]bool, len(refs))
		for _, ref := range refs {
			sourceID, err := continuationSourceRecordID(ref)
			if err != nil {
				return nil, nil, false, err
			}
			if seenSources[sourceID] {
				continue
			}
			seenSources[sourceID] = true
			sourceIDs = append(sourceIDs, sourceID)
			pairs[ownerID+"|"+sourceID] = true
		}
		sort.Strings(sourceIDs)
		if len(sourceIDs) > 0 {
			anchors = append(anchors, ownerID+"|"+sourceIDs[0])
		}
	}
	sort.Strings(anchors)
	truncated := len(anchors) > maxContinuationEntitySourceRefAnchors
	if truncated {
		anchors = anchors[:maxContinuationEntitySourceRefAnchors]
	}
	anchorSet := make(map[string]bool, len(anchors))
	for _, key := range anchors {
		anchorSet[key] = true
	}
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		if !anchorSet[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	keys = append(append([]string(nil), anchors...), keys...)
	if len(keys) > maxContinuationExcludedEntitySourceRefs {
		keys = keys[:maxContinuationExcludedEntitySourceRefs]
		truncated = true
	}
	return keys, anchors, truncated, nil
}

func continuationSourceRecordID(source behaviorgraph.Record) (string, error) {
	path, pathOK := source["path"].(string)
	checksum, checksumOK := source["sha256"].(string)
	locator, locatorOK := source["record_locator"].(string)
	if len(source) != 3 || !pathOK || path == "" || !checksumOK || !digestPattern.MatchString(checksum) || !locatorOK || locator == "" {
		return "", errors.New("analytical graph continuation source reference is malformed")
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return "", errors.New("analytical graph continuation source reference could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return "source-" + hex.EncodeToString(digest[:]), nil
}

func analyticalContinuationTruncated(current, accumulated, nextPage map[string]any) bool {
	if graphPayloadFlag(current, "truncated") ||
		graphPayloadFlag(current, "missing_branches_truncated") ||
		graphPayloadFlag(current, "omitted_cards_truncated") ||
		graphPayloadFlag(current, "missing_evidence_truncated") ||
		graphPayloadFlag(current, "source_integrity_budget_exceeded") {
		return true
	}
	for _, key := range []string{"candidate_assertion_limit_reached", "analytical_frontier_node_limit_reached", "candidate_card_limit_reached", "provenance_truncated"} {
		if graphPayloadFlag(nextPage, key) {
			return true
		}
	}
	for _, key := range []string{"candidate_assertion_exclusion_truncated", "candidate_card_exclusion_truncated", "provenance_cursor_truncated"} {
		if anyGraphFlag(accumulated, nextPage, key) {
			return true
		}
	}
	return false
}

func graphPayloadFlag(payload map[string]any, key string) bool {
	flag, _ := payload[key].(bool)
	return flag
}

func mergeAnalyticalSnapshots(projectKey string, pages ...map[string]any) (behaviorgraph.Record, map[string]int, error) {
	fields := []string{"entities", "assertions", "cards", "sources"}
	merged := make(map[string]map[string]behaviorgraph.Record, len(fields))
	counts := make(map[string]int, len(fields))
	for _, field := range fields {
		merged[field] = make(map[string]behaviorgraph.Record)
	}
	for _, page := range pages {
		if page == nil || page["project_key"] != projectKey {
			return nil, nil, errors.New("analytical graph continuation project is inconsistent")
		}
		for _, field := range fields {
			records, ok := graphRecordList(page[field])
			if !ok {
				return nil, nil, errors.New("analytical graph continuation record list is malformed")
			}
			for _, record := range records {
				id, ok := record["id"].(string)
				if !ok || id == "" {
					return nil, nil, errors.New("analytical graph continuation record has no identity")
				}
				if previous, exists := merged[field][id]; exists && field == "entities" {
					combined, mergeErr := mergeEntityProvenanceRecord(previous, record)
					if mergeErr != nil {
						return nil, nil, mergeErr
					}
					merged[field][id] = combined
					continue
				}
				if previous, exists := merged[field][id]; exists && field != "sources" {
					previousJSON, previousErr := json.Marshal(previous)
					recordJSON, recordErr := json.Marshal(record)
					if previousErr != nil || recordErr != nil || !bytes.Equal(previousJSON, recordJSON) {
						return nil, nil, errors.New("analytical graph continuation contains conflicting record versions")
					}
				}
				merged[field][id] = record
			}
		}
	}
	snapshot := behaviorgraph.Record{"schema_version": json.Number("2"), "program": projectKey}
	for _, field := range fields {
		ids := make([]string, 0, len(merged[field]))
		for id := range merged[field] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		records := make([]behaviorgraph.Record, 0, len(ids))
		for _, id := range ids {
			records = append(records, merged[field][id])
		}
		snapshot[field] = records
		counts[field] = len(records)
	}
	return snapshot, counts, nil
}

func mergeEntityProvenanceRecord(previous, current behaviorgraph.Record) (behaviorgraph.Record, error) {
	previousCore := make(behaviorgraph.Record, len(previous))
	currentCore := make(behaviorgraph.Record, len(current))
	for key, value := range previous {
		if key != "source_refs" {
			previousCore[key] = value
		}
	}
	for key, value := range current {
		if key != "source_refs" {
			currentCore[key] = value
		}
	}
	previousJSON, previousErr := json.Marshal(previousCore)
	currentJSON, currentErr := json.Marshal(currentCore)
	if previousErr != nil || currentErr != nil || !bytes.Equal(previousJSON, currentJSON) {
		return nil, errors.New("analytical graph continuation contains conflicting entity versions")
	}
	refsByJSON := make(map[string]behaviorgraph.Record)
	for _, value := range []any{previous["source_refs"], current["source_refs"]} {
		refs, ok := graphRecordList(value)
		if !ok || len(refs) == 0 {
			return nil, errors.New("analytical graph continuation entity provenance is malformed")
		}
		for _, ref := range refs {
			if _, err := continuationSourceRecordID(ref); err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(ref)
			if err != nil {
				return nil, errors.New("analytical graph continuation entity provenance could not be encoded")
			}
			refsByJSON[string(encoded)] = ref
		}
	}
	keys := make([]string, 0, len(refsByJSON))
	for key := range refsByJSON {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	refs := make([]behaviorgraph.Record, 0, len(keys))
	for _, key := range keys {
		refs = append(refs, refsByJSON[key])
	}
	merged := make(behaviorgraph.Record, len(previous)+1)
	for key, value := range previous {
		merged[key] = value
	}
	merged["source_refs"] = refs
	return merged, nil
}

func graphRecordList(value any) ([]behaviorgraph.Record, bool) {
	switch records := value.(type) {
	case []behaviorgraph.Record:
		return records, true
	case []map[string]any:
		result := make([]behaviorgraph.Record, 0, len(records))
		for _, record := range records {
			result = append(result, behaviorgraph.Record(record))
		}
		return result, true
	case []any:
		result := make([]behaviorgraph.Record, 0, len(records))
		for _, value := range records {
			record, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			result = append(result, behaviorgraph.Record(record))
		}
		return result, true
	default:
		return nil, false
	}
}

func generationFromGraphPayload(payload map[string]any) int64 {
	switch value := payload["graph_generation"].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case json.Number:
		generation, _ := value.Int64()
		return generation
	case float64:
		if value >= 1 && value == float64(int64(value)) {
			return int64(value)
		}
	}
	return 0
}

func anyGraphFlag(left, right map[string]any, key string) bool {
	leftFlag, _ := left[key].(bool)
	rightFlag, _ := right[key].(bool)
	return leftFlag || rightFlag
}

func maxIntValue(left, right any) int {
	return max(graphIntValue(left), graphIntValue(right))
}

func graphIntValue(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case float64:
		return int(number)
	}
	return 0
}

func graphStringList(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
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
		                 cr.request_wire_fidelity,cr.response_wire_fidelity,
		                 CASE WHEN gm.revision_id=gr.revision_id AND gm.graph_observation_id=gr.graph_observation_id
		                      THEN c.parse_state ELSE 'unknown' END
		          FROM graph_capture_revisions gr
		          JOIN capture_revisions cr ON cr.revision_id=gr.revision_id
		          JOIN captures c ON c.observation_id=cr.observation_id
		          LEFT JOIN graph_capture_map gm ON gm.observation_id=c.observation_id
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
			var parseState string
			var requestSize int64
			var responseSize, requestTruncated, responseTruncated sql.NullInt64
			if err := rows.Scan(&graphID, &revisionID, &observationID, &requestID, &requestSHA, &responseSHA,
				&requestSize, &responseSize, &firstSeen, &requestTruncated, &responseTruncated,
				&requestFidelity, &responseFidelity, &parseState); err != nil {
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
				"parse_state":            parseState,
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
