package search

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestContinuationExcludedCardIDsAreValidatedAndBounded(t *testing.T) {
	valid := "card-" + strings.Repeat("f", 64)
	got, truncated, err := continuationExcludedCardIDs([]behaviorgraph.Record{{"id": valid}, {"id": valid}})
	if err != nil || truncated || len(got) != 1 || got[0] != valid {
		t.Fatalf("valid card exclusions = %v, truncated %t, err %v", got, truncated, err)
	}
	if _, _, err := continuationExcludedCardIDs("malformed"); err == nil {
		t.Fatal("accepted malformed card exclusion list")
	}
	if _, _, err := continuationExcludedCardIDs([]behaviorgraph.Record{{"id": "card-short"}}); err == nil {
		t.Fatal("accepted malformed card identity")
	}
}

func TestContinuationEntitySourceRefStateBuildsBoundedOwnerKeys(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("1", 64)
	refs := []behaviorgraph.Record{
		{"path": "/synthetic/one.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.one"},
		{"path": "/synthetic/two.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.two"},
		{"path": "/synthetic/one.json", "sha256": strings.Repeat("a", 64), "record_locator": "$.one"},
	}
	entities := []behaviorgraph.Record{{"id": owner, "source_refs": refs}}
	excluded, anchors, truncated, err := continuationEntitySourceRefState(entities, []string{owner})
	if err != nil || truncated || len(excluded) != 2 || len(anchors) != 1 {
		t.Fatalf("source-ref continuation state = %v, %v, truncated=%t, err=%v", excluded, anchors, truncated, err)
	}
	if !sort.StringsAreSorted(excluded) || !sort.StringsAreSorted(anchors) || !strings.HasPrefix(anchors[0], owner+"|") {
		t.Fatalf("source-ref keys are not stable owner pairs: excluded=%v anchors=%v", excluded, anchors)
	}
	if _, found := slices.BinarySearch(excluded, anchors[0]); !found {
		t.Fatalf("anchor key %q is absent from prior refs %v", anchors[0], excluded)
	}
}

func TestMergeAnalyticalSnapshotsUnionsEntityProvenancePages(t *testing.T) {
	project := strings.Repeat("a", 64)
	owner := "endpoint-" + strings.Repeat("b", 64)
	first := behaviorgraph.Record{"path": "/synthetic/one.json", "sha256": strings.Repeat("1", 64), "record_locator": "$.one"}
	second := behaviorgraph.Record{"path": "/synthetic/two.json", "sha256": strings.Repeat("2", 64), "record_locator": "$.two"}
	page := func(ref behaviorgraph.Record, sourceID string) map[string]any {
		return map[string]any{
			"project_key": project,
			"entities":    []behaviorgraph.Record{{"id": owner, "kind": "endpoint", "data": map[string]any{"method": "GET"}, "source_refs": []behaviorgraph.Record{ref}}},
			"assertions":  []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{},
			"sources": []behaviorgraph.Record{{"id": sourceID, "path": ref["path"], "sha256": ref["sha256"], "record_locator": ref["record_locator"]}},
		}
	}
	firstID, err := continuationSourceRecordID(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := continuationSourceRecordID(second)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := mergeAnalyticalSnapshots(project,
		page(first, firstID), page(second, secondID))
	if err != nil {
		t.Fatalf("merge entity provenance pages: %v", err)
	}
	entities := snapshot["entities"].([]behaviorgraph.Record)
	refs := entities[0]["source_refs"].([]behaviorgraph.Record)
	if len(refs) != 2 {
		t.Fatalf("merged entity retains %d refs, want 2: %#v", len(refs), refs)
	}
}

func TestCaptureProvenanceRetainsRevisionsAndCurrentParseState(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "live-graph-provenance"}
	root := t.TempDir()
	writable, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = writable.Close()
		}
	})

	const observationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const oldRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const currentRevision = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const oldGraphID = "observation-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	const currentGraphID = "observation-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const requestSignature = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	const responseSignature = "1111111111111111111111111111111111111111111111111111111111111111"
	const legacyObservationID = "9999999999999999999999999999999999999999999999999999999999999999"
	const legacyGraphID = "observation-2222222222222222222222222222222222222222222222222222222222222222"
	const historicalObservationID = "8888888888888888888888888888888888888888888888888888888888888888"
	const historicalOldRevision = "7777777777777777777777777777777777777777777777777777777777777777"
	const historicalCurrentRevision = "6666666666666666666666666666666666666666666666666666666666666666"
	const historicalOldGraphID = "observation-5555555555555555555555555555555555555555555555555555555555555555"
	const historicalCurrentGraphID = "observation-4444444444444444444444444444444444444444444444444444444444444444"
	const historicalRequestSHA = "3333333333333333333333333333333333333333333333333333333333333333"
	const historicalOldRequestSHA = "2222222222222222222222222222222222222222222222222222222222222222"
	for _, revision := range []string{oldRevision, currentRevision} {
		if _, err := writable.Database.DB().ExecContext(ctx,
			`INSERT INTO capture_revision_registry(revision_id,observation_id,source_kind,first_seen_at)
			 VALUES(?,?,'live',?)`, revision, observationID, "2026-10-23T12:00:00Z",
		); err != nil {
			t.Fatalf("insert live revision registry row: %v", err)
		}
	}
	if _, err := writable.Database.DB().ExecContext(ctx,
		`INSERT INTO live_capture_index(observation_id,caido_instance_id,caido_project_id,request_id,
		 graph_observation_id,synced_at,method,host,port,route,response_status,request_content_type,
		 response_content_type,request_size,response_size,request_signature,response_signature,request_url_key,
		 referer_url_key,redirect_url_key,field_names_json,header_names_json,parse_state,current_revision_id)
		 VALUES(?,?,?,?,?,?,'GET','fixture.invalid',443,'/item',200,'','application/json',12,24,?,?,?,NULL,NULL,'[]','[]','complete',?)`,
		observationID, ref.InstanceID, ref.ProjectID, "request-239", currentGraphID,
		"2026-10-23T12:00:02Z", requestSignature, responseSignature,
		"2222222222222222222222222222222222222222222222222222222222222222", currentRevision,
	); err != nil {
		t.Fatalf("insert current live capture: %v", err)
	}
	if _, err := writable.Database.DB().ExecContext(ctx,
		`INSERT INTO live_capture_index(observation_id,caido_instance_id,caido_project_id,request_id,
		 graph_observation_id,synced_at,method,host,port,route,response_status,request_content_type,
		 response_content_type,request_size,response_size,request_signature,response_signature,request_url_key,
		 referer_url_key,redirect_url_key,field_names_json,header_names_json,parse_state,current_revision_id)
		 VALUES(?,?,?,?,?,?,'GET','fixture.invalid',443,'/legacy',NULL,'','',12,NULL,?,NULL,?,NULL,NULL,'[]','[]','partial',NULL)`,
		legacyObservationID, ref.InstanceID, ref.ProjectID, "request-legacy", legacyGraphID,
		"2026-10-23T11:00:00Z", requestSignature,
		"3333333333333333333333333333333333333333333333333333333333333333",
	); err != nil {
		t.Fatalf("insert legacy live capture without registry row: %v", err)
	}
	if _, err := writable.Database.DB().ExecContext(ctx,
		`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,
		 request_sha256,request_size,current_revision_id,request_vault_ref,parse_state)
		 VALUES(?,?,?,?,?,10,?,?,?)`,
		historicalObservationID, ref.InstanceID, ref.ProjectID, "request-history",
		historicalRequestSHA, historicalCurrentRevision, "request-history.bba", "complete",
	); err != nil {
		t.Fatalf("insert historical capture: %v", err)
	}
	for revision, requestSHA := range map[string]string{
		historicalOldRevision:     historicalOldRequestSHA,
		historicalCurrentRevision: historicalRequestSHA,
	} {
		if _, err := writable.Database.DB().ExecContext(ctx,
			`INSERT INTO capture_revisions(revision_id,observation_id,request_sha256,request_size,
			 request_vault_ref,first_seen_at) VALUES(?,?,?,10,?,?)`,
			revision, historicalObservationID, requestSHA, "request-history-"+revision+".bba", "2026-10-23T10:00:00Z",
		); err != nil {
			t.Fatalf("insert historical capture revision: %v", err)
		}
	}
	for revision, graphID := range map[string]string{
		historicalOldRevision:     historicalOldGraphID,
		historicalCurrentRevision: historicalCurrentGraphID,
	} {
		if _, err := writable.Database.DB().ExecContext(ctx,
			`INSERT INTO graph_capture_revisions(revision_id,graph_observation_id,updated_at) VALUES(?,?,?)`,
			revision, graphID, "2026-10-23T10:00:01Z",
		); err != nil {
			t.Fatalf("insert historical graph revision: %v", err)
		}
	}
	if _, err := writable.Database.DB().ExecContext(ctx,
		`INSERT INTO graph_capture_map(observation_id,revision_id,graph_observation_id,updated_at) VALUES(?,?,?,?)`,
		historicalObservationID, historicalCurrentRevision, historicalCurrentGraphID, "2026-10-23T10:00:02Z",
	); err != nil {
		t.Fatalf("insert current historical graph mapping: %v", err)
	}
	for revision, graphID := range map[string]string{oldRevision: oldGraphID} {
		if _, err := writable.Database.DB().ExecContext(ctx,
			`INSERT INTO live_graph_capture_revisions(revision_id,observation_id,graph_observation_id,updated_at)
			 VALUES(?,?,?,?)`, revision, observationID, graphID, "2026-10-23T12:00:03Z",
		); err != nil {
			t.Fatalf("insert live graph revision mapping: %v", err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable project: %v", err)
	}
	closed = true

	readonly, err := store.OpenProjectReadOnly(ctx, ref, root)
	if err != nil {
		t.Fatalf("OpenProjectReadOnly(): %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	rows, truncated, err := captureProvenance(ctx, readonly, []string{
		oldGraphID, currentGraphID, legacyGraphID, historicalOldGraphID, historicalCurrentGraphID,
	})
	if err != nil {
		t.Fatalf("captureProvenance(): %v", err)
	}
	if truncated || len(rows) != 5 {
		t.Fatalf("graph provenance rows=%d truncated=%v, want both live revisions, both historical revisions and the legacy mapping", len(rows), truncated)
	}
	byGraphID := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		byGraphID[row["graph_observation_id"].(string)] = row
	}
	old := byGraphID[oldGraphID]
	oldRevisionValue, oldRevisionOK := old["revision_id"].(*string)
	oldResponseSignature, oldSignatureOK := old["response_signature"].(*string)
	oldResponseSize, oldSizeOK := old["response_size"].(*int64)
	if old == nil || !oldRevisionOK || oldRevisionValue == nil || *oldRevisionValue != oldRevision || old["request_id"] != "request-239" ||
		old["revision_mapping"] != "revision" || !oldSignatureOK || oldResponseSignature != nil ||
		!oldSizeOK || oldResponseSize != nil || old["parse_state"] != "unknown" {
		t.Fatalf("earlier graph revision provenance = %#v", old)
	}
	current := byGraphID[currentGraphID]
	currentRevisionValue, currentRevisionOK := current["revision_id"].(*string)
	currentResponseSignature, currentSignatureOK := current["response_signature"].(*string)
	currentResponseSize, currentSizeOK := current["response_size"].(*int64)
	if current == nil || !currentRevisionOK || currentRevisionValue == nil || *currentRevisionValue != currentRevision || current["request_id"] != "request-239" ||
		current["revision_mapping"] != "current" || !currentSignatureOK || currentResponseSignature == nil ||
		!currentSizeOK || currentResponseSize == nil || current["parse_state"] != "complete" {
		t.Fatalf("current graph revision provenance = %#v", current)
	}
	legacy := byGraphID[legacyGraphID]
	legacyRevision, legacyRevisionOK := legacy["revision_id"].(*string)
	if legacy == nil || !legacyRevisionOK || legacyRevision != nil || legacy["revision_mapping"] != "current" || legacy["request_id"] != "request-legacy" {
		t.Fatalf("legacy graph provenance without revision registry = %#v", legacy)
	}
	historicalOld := byGraphID[historicalOldGraphID]
	if historicalOld == nil || historicalOld["parse_state"] != "unknown" {
		t.Fatalf("older historical graph revision parse state = %#v, want unknown", historicalOld)
	}
	historicalCurrent := byGraphID[historicalCurrentGraphID]
	if historicalCurrent == nil || historicalCurrent["parse_state"] != "complete" {
		t.Fatalf("current historical graph revision parse state = %#v, want complete", historicalCurrent)
	}
}

func TestMergeAnalyticalSnapshotsDeduplicatesAndSortsRecordFamilies(t *testing.T) {
	projectKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sourceRef := behaviorgraph.Record{"path": "/synthetic/fixture.json", "sha256": strings.Repeat("e", 64), "record_locator": "$.endpoint"}
	sourceID, err := continuationSourceRecordID(sourceRef)
	if err != nil {
		t.Fatalf("continuationSourceRecordID: %v", err)
	}
	sourceRefs := []behaviorgraph.Record{sourceRef}
	first := map[string]any{
		"project_key": projectKey,
		"entities": []behaviorgraph.Record{
			{"id": "endpoint-" + strings.Repeat("b", 64), "kind": "endpoint", "source_refs": sourceRefs},
		},
		"assertions": []behaviorgraph.Record{{"id": "assertion-" + strings.Repeat("c", 64), "predicate": "exposes"}},
		"cards":      []behaviorgraph.Record{{"id": "card-" + strings.Repeat("d", 64), "analysis_state": "candidate"}},
		"sources":    []behaviorgraph.Record{{"id": sourceID, "path": sourceRef["path"], "sha256": sourceRef["sha256"], "record_locator": sourceRef["record_locator"]}},
	}
	second := map[string]any{
		"project_key": projectKey,
		"entities": []any{
			map[string]any{"id": "endpoint-" + strings.Repeat("b", 64), "kind": "endpoint", "source_refs": sourceRefs},
			map[string]any{"id": "observation-" + strings.Repeat("f", 64), "kind": "observation"},
		},
		"assertions": []any{map[string]any{"id": "assertion-" + strings.Repeat("a", 64), "predicate": "returns"}},
		"cards":      []any{},
		"sources":    []any{},
	}
	snapshot, counts, err := mergeAnalyticalSnapshots(projectKey, first, second)
	if err != nil {
		t.Fatalf("mergeAnalyticalSnapshots: %v", err)
	}
	entities, ok := snapshot["entities"].([]behaviorgraph.Record)
	if !ok || len(entities) != 2 || entities[0]["id"] != "endpoint-"+strings.Repeat("b", 64) || entities[1]["id"] != "observation-"+strings.Repeat("f", 64) {
		t.Fatalf("merged entities = %#v", snapshot["entities"])
	}
	if counts["entities"] != 2 || counts["assertions"] != 2 || counts["cards"] != 1 || counts["sources"] != 1 {
		t.Fatalf("merged record counts = %#v", counts)
	}
	if snapshot["program"] != projectKey || snapshot["schema_version"] == nil {
		t.Fatalf("merged snapshot identity missing: %#v", snapshot)
	}
}

func TestAnalyticalContinuationTruncationRecomputesResolvedRendererOmissions(t *testing.T) {
	current := map[string]any{"truncated": false}
	prior := map[string]any{"truncated": true}
	if analyticalContinuationTruncated(current, prior, nil) {
		t.Fatal("resolved renderer omissions remained truncated from an earlier page")
	}
	for _, key := range []string{"candidate_assertion_limit_reached", "analytical_frontier_node_limit_reached", "candidate_card_limit_reached", "provenance_truncated"} {
		if analyticalContinuationTruncated(current, map[string]any{key: true}, nil) {
			t.Errorf("resolved native continuation limit %q remained set from an earlier page", key)
		}
		if !analyticalContinuationTruncated(current, nil, map[string]any{key: true}) {
			t.Errorf("current native continuation limit %q was lost", key)
		}
	}
	for _, key := range []string{"candidate_assertion_exclusion_truncated", "candidate_card_exclusion_truncated", "provenance_cursor_truncated"} {
		if !analyticalContinuationTruncated(current, map[string]any{key: true}, nil) {
			t.Errorf("unpaged or exclusion limit %q was lost", key)
		}
	}
	for _, key := range []string{
		"truncated", "missing_branches_truncated", "omitted_cards_truncated",
		"missing_evidence_truncated", "source_integrity_budget_exceeded",
	} {
		if !analyticalContinuationTruncated(map[string]any{key: true}, nil, nil) {
			t.Errorf("current rendered limit %q was lost", key)
		}
	}
}

func TestMergeContinuationBranchesKeepsNativeFrontierSeeds(t *testing.T) {
	assertion := "assertion-" + strings.Repeat("a", 64)
	endpoint := "endpoint-" + strings.Repeat("b", 64)
	branches, truncated := mergeContinuationBranches(
		[]behaviorgraph.Record{
			{"assertion_id": assertion, "reason": "max_edges"},
			{"frontier_id": endpoint, "reason": "analytical_frontier_node_limit"},
		},
		[]any{map[string]any{"frontier_id": endpoint, "reason": "analytical_frontier_node_limit"}},
	)
	if truncated || len(branches) != 2 {
		t.Fatalf("merged continuation branches = %#v, truncated=%t", branches, truncated)
	}
	seen := map[string]bool{}
	for _, branch := range branches {
		seen[graphBranchSeedID(branch)] = true
	}
	if !seen[assertion] || !seen[endpoint] {
		t.Fatalf("merged continuation seeds = %#v", branches)
	}
}

func TestContinuationExcludedAssertionIDsAreSortedAndDeduplicated(t *testing.T) {
	first := "assertion-" + strings.Repeat("a", 64)
	second := "assertion-" + strings.Repeat("b", 64)
	ids, truncated, err := continuationExcludedAssertionIDs([]any{
		map[string]any{"id": second},
		map[string]any{"id": first},
		map[string]any{"id": second},
		map[string]any{"id": "endpoint-" + strings.Repeat("c", 64)},
	})
	if err != nil || truncated || len(ids) != 2 || ids[0] != first || ids[1] != second {
		t.Fatalf("continuation assertion exclusions = %v, truncated=%t, err=%v", ids, truncated, err)
	}
}

func TestMergeAnalyticalSnapshotsRejectsDifferentProjects(t *testing.T) {
	_, _, err := mergeAnalyticalSnapshots("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", map[string]any{
		"project_key": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"entities":    []behaviorgraph.Record{}, "assertions": []behaviorgraph.Record{},
		"cards": []behaviorgraph.Record{}, "sources": []behaviorgraph.Record{},
	})
	if err == nil {
		t.Fatal("snapshot from another project was accepted")
	}
}

func TestMergeAnalyticalSnapshotsRejectsConflictingRecordVersions(t *testing.T) {
	projectKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, _, err := mergeAnalyticalSnapshots(projectKey,
		map[string]any{
			"project_key": projectKey,
			"entities":    []behaviorgraph.Record{{"id": "endpoint-" + strings.Repeat("b", 64), "kind": "endpoint"}},
			"assertions":  []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{}, "sources": []behaviorgraph.Record{},
		},
		map[string]any{
			"project_key": projectKey,
			"entities":    []behaviorgraph.Record{{"id": "endpoint-" + strings.Repeat("b", 64), "kind": "different-kind"}},
			"assertions":  []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{}, "sources": []behaviorgraph.Record{},
		},
	)
	if err == nil {
		t.Fatal("conflicting immutable record versions were merged")
	}
}
