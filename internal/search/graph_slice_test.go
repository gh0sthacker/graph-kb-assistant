package search

import (
	"context"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestCaptureProvenanceRetainsEarlierLiveGraphRevision(t *testing.T) {
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
	rows, truncated, err := captureProvenance(ctx, readonly, []string{oldGraphID, currentGraphID, legacyGraphID})
	if err != nil {
		t.Fatalf("captureProvenance(): %v", err)
	}
	if truncated || len(rows) != 3 {
		t.Fatalf("live graph provenance rows=%d truncated=%v, want both revisions and the legacy mapping", len(rows), truncated)
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
}
