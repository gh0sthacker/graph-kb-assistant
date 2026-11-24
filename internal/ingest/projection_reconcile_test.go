package ingest

import (
	"context"
	"encoding/json"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestProjectionReconciliationDryRunThenAcknowledgesOnlyExactSubset(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	first, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "reconcile-present-request", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\npresent"))
	if err != nil || first.ProjectionState != "pending" {
		t.Fatalf("accept first projection: result=%+v err=%v", first, err)
	}
	second, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "reconcile-missing-request", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nmissing"))
	if err != nil || second.ProjectionState != "pending" {
		t.Fatalf("accept second projection: result=%+v err=%v", second, err)
	}

	var firstJob behaviorgraph.ProjectionJob
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT revision_id FROM projection_jobs ORDER BY sequence_id LIMIT 1").Scan(&firstJob.RevisionID); err != nil {
		t.Fatal(err)
	}
	firstJob, err = behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, firstJob.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	var firstBundle behaviorgraph.Record
	if err := json.Unmarshal([]byte(firstJob.PayloadJSON), &firstBundle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := materializeLocalProjection(ctx, projectStore, []behaviorgraph.Record{firstBundle}); err != nil {
		t.Fatal(err)
	}
	snapshot := projectionReconcileTestSnapshot(t, projectStore)
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()

	firstPage, err := RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot,
		ProjectionReconcileOptions{MaxJobs: 1, MaxBytes: 8 << 20})
	if err != nil || !firstPage.Truncated || firstPage.Scanned != 1 || firstPage.NextSequenceID <= 0 {
		t.Fatalf("bounded reconciliation page did not return a cursor: result=%+v err=%v", firstPage, err)
	}
	secondPage, err := RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot,
		ProjectionReconcileOptions{MaxJobs: 10, MaxBytes: 8 << 20, AfterSequenceID: firstPage.NextSequenceID})
	if err != nil || secondPage.Truncated || secondPage.Scanned != 1 || secondPage.ConflictingRecords != 1 || secondPage.NextSequenceID != 0 {
		t.Fatalf("reconciliation cursor did not resume after the first page: result=%+v err=%v", secondPage, err)
	}

	options := ProjectionReconcileOptions{MaxJobs: 10, MaxBytes: 8 << 20}
	result, err := RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot, options)
	if err != nil {
		t.Fatalf("dry-run reconciliation: result=%+v err=%v", result, err)
	}
	if result.State != "partial" || result.Scanned != 2 || result.ExactMatches != 1 ||
		result.MissingRecords != 0 || result.ConflictingRecords != 1 || result.Applied != 0 || result.Pending != 2 {
		t.Fatalf("dry-run reconciliation did not preserve incomplete work: %+v", result)
	}

	options.Apply = true
	result, err = RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot, options)
	if err != nil {
		t.Fatalf("apply exact matches: result=%+v err=%v", result, err)
	}
	if result.State != "partial" || result.ExactMatches != 1 || result.Applied != 1 || result.Pending != 1 {
		t.Fatalf("reconciliation acknowledged the wrong work: %+v", result)
	}
	var firstState, secondState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM projection_jobs WHERE revision_id=?", firstJob.RevisionID).Scan(&firstState); err != nil {
		t.Fatal(err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM projection_jobs WHERE revision_id=(SELECT revision_id FROM projection_jobs ORDER BY sequence_id DESC LIMIT 1)").Scan(&secondState); err != nil {
		t.Fatal(err)
	}
	if firstState != "applied" || secondState != "pending" {
		t.Fatalf("queue states after reconciliation = first %q, second %q", firstState, secondState)
	}
	var mappedGraphID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT graph_observation_id FROM graph_capture_revisions WHERE revision_id=?", firstJob.RevisionID).Scan(&mappedGraphID); err != nil || mappedGraphID == "" {
		t.Fatalf("reconciled revision mapping was not persisted: id=%q err=%v", mappedGraphID, err)
	}
}

func TestProjectionReconciliationIncludesDeadLettersOnlyWhenRequested(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	for _, requestID := range []string{"reconcile-dead-letter"} {
		accepted, writeErr := writePendingHistoryRevision(ctx, pageWriter,
			projectionWorkerExchange(selected, requestID, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nsynthetic"))
		if writeErr != nil || accepted.ProjectionState != "pending" {
			t.Fatalf("accept synthetic projection %s: result=%+v err=%v", requestID, accepted, writeErr)
		}
	}
	rows, err := projectStore.Database.DB().QueryContext(ctx,
		"SELECT revision_id FROM projection_jobs ORDER BY sequence_id")
	if err != nil {
		t.Fatal(err)
	}
	var revisionIDs []string
	for rows.Next() {
		var revisionID string
		if err := rows.Scan(&revisionID); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		revisionIDs = append(revisionIDs, revisionID)
	}
	if err := rows.Close(); err != nil || len(revisionIDs) != 1 {
		t.Fatalf("read synthetic revision IDs: count=%d err=%v", len(revisionIDs), err)
	}
	bundles := make([]behaviorgraph.Record, 0, len(revisionIDs))
	for _, revisionID := range revisionIDs {
		job, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, revisionID)
		if err != nil {
			t.Fatal(err)
		}
		var bundle behaviorgraph.Record
		if err := json.Unmarshal([]byte(job.PayloadJSON), &bundle); err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, bundle)
	}
	if _, _, err := materializeLocalProjection(ctx, projectStore, bundles); err != nil {
		t.Fatalf("materialize exact synthetic bundles: %v", err)
	}
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		"UPDATE projection_jobs SET state='dead_letter',attempt_count=8,last_error_code='synthetic_failure' WHERE revision_id=?",
		revisionIDs[0]); err != nil {
		t.Fatalf("mark synthetic job dead-letter: %v", err)
	}
	snapshot := projectionReconcileTestSnapshot(t, projectStore)
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()

	defaultDryRun, err := RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot,
		ProjectionReconcileOptions{MaxJobs: 10, MaxBytes: 8 << 20})
	if err != nil || defaultDryRun.Scanned != 0 || defaultDryRun.ExactMatches != 0 ||
		defaultDryRun.Pending != 0 || defaultDryRun.DeadLetter != 1 || defaultDryRun.DeadLetterIncluded {
		t.Fatalf("default reconciliation should leave dead letters excluded: result=%+v err=%v", defaultDryRun, err)
	}

	deadLetterDryRun, err := RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot,
		ProjectionReconcileOptions{MaxJobs: 10, MaxBytes: 8 << 20, IncludeDeadLetter: true})
	if err != nil || deadLetterDryRun.Scanned != 1 || deadLetterDryRun.ExactMatches != 1 ||
		deadLetterDryRun.Applied != 0 || deadLetterDryRun.Pending != 0 || deadLetterDryRun.DeadLetter != 1 ||
		!deadLetterDryRun.DeadLetterIncluded {
		t.Fatalf("dead-letter dry-run did not inspect without mutation: result=%+v err=%v", deadLetterDryRun, err)
	}

	deadLetterDryRun, err = RunProjectionReconciliation(ctx, projectStore, maintenance, snapshot, snapshot,
		ProjectionReconcileOptions{MaxJobs: 10, MaxBytes: 8 << 20, IncludeDeadLetter: true, Apply: true})
	if err != nil || deadLetterDryRun.State != "reconciled" || deadLetterDryRun.ExactMatches != 1 ||
		deadLetterDryRun.Applied != 1 || deadLetterDryRun.Pending != 0 || deadLetterDryRun.DeadLetter != 0 {
		t.Fatalf("apply should acknowledge only exact dead-letter matches: result=%+v err=%v", deadLetterDryRun, err)
	}
	for _, revisionID := range revisionIDs {
		job, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, revisionID)
		if err != nil || job.State != "applied" || job.LastErrorCode.Valid {
			t.Fatalf("reconciled job state = %+v err=%v, want applied with cleared error", job, err)
		}
	}
}

func TestProjectionReconciliationRejectsSnapshotParityMismatch(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	accepted, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "reconcile-parity-request", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nparity"))
	if err != nil || accepted.ProjectionState != "pending" {
		t.Fatalf("accept projection: result=%+v err=%v", accepted, err)
	}
	job := projectionReconcileTestPendingJob(t, projectStore)
	var bundle behaviorgraph.Record
	if err := json.Unmarshal([]byte(job.PayloadJSON), &bundle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := materializeLocalProjection(ctx, projectStore, []behaviorgraph.Record{bundle}); err != nil {
		t.Fatal(err)
	}
	localSnapshot := projectionReconcileTestSnapshot(t, projectStore)
	remoteSnapshot := behaviorgraph.Record{}
	encoded, err := json.Marshal(localSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &remoteSnapshot); err != nil {
		t.Fatal(err)
	}
	remoteSnapshot["program"] = "different-project"
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()

	result, err := RunProjectionReconciliation(ctx, projectStore, maintenance, localSnapshot, remoteSnapshot,
		ProjectionReconcileOptions{MaxJobs: 10, MaxBytes: 8 << 20, Apply: true})
	if err != nil {
		t.Fatalf("parity mismatch should be reported without an operational error: result=%+v err=%v", result, err)
	}
	if result.State != "parity_mismatch" || result.Scanned != 0 || result.Applied != 0 {
		t.Fatalf("parity mismatch did not fail closed: %+v", result)
	}
	var state string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM projection_jobs ORDER BY sequence_id LIMIT 1").Scan(&state); err != nil || state != "pending" {
		t.Fatalf("parity mismatch changed pending queue state: state=%q err=%v", state, err)
	}
}

func TestProjectionReconciliationRequiresExclusiveProjectLease(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pageWriter.Close)
	if _, err := writePendingHistoryRevision(ctx, pageWriter,
		projectionWorkerExchange(selected, "reconcile-lock-request", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nlock")); err != nil {
		t.Fatal(err)
	}
	job := projectionReconcileTestPendingJob(t, projectStore)
	var bundle behaviorgraph.Record
	if err := json.Unmarshal([]byte(job.PayloadJSON), &bundle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := materializeLocalProjection(ctx, projectStore, []behaviorgraph.Record{bundle}); err != nil {
		t.Fatal(err)
	}
	snapshot := projectionReconcileTestSnapshot(t, projectStore)
	if _, err := RunProjectionReconciliation(ctx, projectStore, nil, snapshot, snapshot,
		ProjectionReconcileOptions{MaxJobs: 1, MaxBytes: 8 << 20}); err == nil {
		t.Fatal("projection reconciliation ran without the project maintenance lease")
	}
}

func projectionReconcileTestPendingJob(t *testing.T, selected *store.ProjectStore) behaviorgraph.ProjectionJob {
	t.Helper()
	ctx := context.Background()
	var revisionID string
	if err := selected.Database.DB().QueryRowContext(ctx,
		"SELECT revision_id FROM projection_jobs WHERE state='pending' ORDER BY sequence_id LIMIT 1").Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	job, err := behaviorgraph.ProjectionJobByRevision(ctx, selected.Database.DB(), selected.ProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func projectionReconcileTestSnapshot(t *testing.T, selected *store.ProjectStore) behaviorgraph.Record {
	t.Helper()
	reader, err := behaviorgraph.OpenReader(context.Background(), selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, snapshotErr := reader.Snapshot()
	closeErr := reader.Close()
	if snapshotErr != nil || closeErr != nil {
		t.Fatalf("read local projection snapshot: snapshot=%v close=%v", snapshotErr, closeErr)
	}
	return snapshot
}
