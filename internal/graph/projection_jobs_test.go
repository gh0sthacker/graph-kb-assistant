package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var projectionTestProjectKey = strings.Repeat("e", 64)

func TestProjectionBundleValidationRejectsInvalidGraphShapes(t *testing.T) {
	tests := []struct {
		name    string
		program string
		payload string
	}{
		{
			name:    "unsupported schema version",
			program: projectionTestProjectKey,
			payload: `{"schema_version":2,"program":"` + projectionTestProjectKey + `","sources":[],"entities":[],"assertions":[],"cards":[]}`,
		},
		{
			name:    "wrong project namespace",
			program: projectionTestProjectKey,
			payload: `{"schema_version":1,"program":"` + strings.Repeat("f", 64) + `","sources":[],"entities":[],"assertions":[],"cards":[]}`,
		},
		{
			name:    "unvalidated nested record field",
			program: projectionTestProjectKey,
			payload: `{"schema_version":1,"program":"` + projectionTestProjectKey + `","sources":[],"entities":[{"raw_payload":"ordinary text"}],"assertions":[],"cards":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateProjectionBundle([]byte(test.payload), test.program); err == nil {
				t.Fatal("invalid graph projection bundle was accepted")
			}
		})
	}
}

func TestProjectionJobEnqueueIsIdempotentAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite3")
	db := openProjectionJobDB(t, path)
	ctx := context.Background()
	revisionID := strings.Repeat("a", 64)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-1", now)
	first, inserted, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-1", minimalProjectionPayload(projectionTestProjectKey, "observation-1"), now)
	if err != nil || !inserted {
		t.Fatalf("first enqueue: inserted=%v err=%v", inserted, err)
	}
	second, inserted, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-1", minimalProjectionPayload(projectionTestProjectKey, "observation-1"), now.Add(time.Second))
	if err != nil || inserted {
		t.Fatalf("replayed enqueue: inserted=%v err=%v", inserted, err)
	}
	if first.JobID != second.JobID || first.PayloadSHA256 != second.PayloadSHA256 || second.State != "pending" {
		t.Fatalf("replay changed job identity or state: first=%+v second=%+v", first, second)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openProjectionJobDB(t, path)
	defer db.Close()
	got, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil {
		t.Fatalf("read durable projection job: %v", err)
	}
	if got.JobID != first.JobID || got.PayloadJSON != first.PayloadJSON || got.PayloadSHA256 != first.PayloadSHA256 {
		t.Fatalf("durable job mismatch: got=%+v want=%+v", got, first)
	}
}

func TestProjectionJobConflictDoesNotReplaceAcceptedPayload(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("b", 64)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-2", now)
	firstPayload := minimalProjectionPayload(projectionTestProjectKey, "observation-2")
	first, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-2", firstPayload, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	changedPayload := projectionPayloadWithMarker(projectionTestProjectKey, "observation-2", "changed")
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-2", changedPayload, now); !errors.Is(err, ErrProjectionJobConflict) {
		_ = tx.Rollback()
		t.Fatalf("expected revision payload conflict, got %v", err)
	}
	_ = tx.Rollback()
	job, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if job.PayloadJSON != first.PayloadJSON {
		t.Fatalf("conflict replaced first payload: %q", job.PayloadJSON)
	}
}

func TestProjectionJobEnqueueParticipatesInCallerTransaction(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("c", 64)
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-3", now)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-3", minimalProjectionPayload(projectionTestProjectKey, "observation-3"), now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rolled-back transaction left a durable job: %v", err)
	}
	var revisions int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM capture_revision_registry WHERE revision_id=?", revisionID).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 0 {
		t.Fatal("rolled-back transaction left a registered source revision")
	}
}

func TestProjectionJobRequiresRegisteredSourceRevision(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = EnqueueProjectionJob(context.Background(), tx, projectionTestProjectKey, strings.Repeat("9", 64), "observation-missing", minimalProjectionPayload(projectionTestProjectKey, "observation-missing"), time.Now().UTC())
	_ = tx.Rollback()
	if !errors.Is(err, ErrProjectionRevisionMissing) {
		t.Fatalf("expected unregistered source revision rejection, got %v", err)
	}
}

func TestProjectionJobRequiresMatchingObservationIdentity(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("9", 64)
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "registered-observation", now)
	_, _, err = EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "different-observation", minimalProjectionPayload(projectionTestProjectKey, "different-observation"), now)
	_ = tx.Rollback()
	if !errors.Is(err, ErrProjectionRevisionMismatch) {
		t.Fatalf("revision enqueue mismatch error = %v, want %v", err, ErrProjectionRevisionMismatch)
	}
}

func TestProjectionJobRequiresExactlyOneMatchingCaptureObservation(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "empty bundle",
			payload: []byte(`{"schema_version":1,"program":"` + projectionTestProjectKey + `","sources":[],"entities":[],"assertions":[],"cards":[]}`),
		},
		{
			name:    "different capture",
			payload: minimalProjectionPayload(projectionTestProjectKey, "another-capture"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
			defer db.Close()
			ctx := context.Background()
			revisionID := strings.Repeat("8", 64)
			now := time.Now().UTC()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			registerProjectionRevision(t, ctx, tx, revisionID, "registered-capture", now)
			_, _, err = EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "registered-capture", test.payload, now)
			_ = tx.Rollback()
			if !errors.Is(err, ErrInvalidProjectionJob) {
				t.Fatalf("enqueue accepted a projection without one matching capture observation: %v", err)
			}
		})
	}
}

func TestStoredProjectionJobMustMatchRegisteredCapture(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("7", 64)
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "registered-capture", now)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "registered-capture", minimalProjectionPayload(projectionTestProjectKey, "registered-capture"), now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	wrongPayload := minimalProjectionPayload(projectionTestProjectKey, "different-capture")
	digest := sha256.Sum256(wrongPayload)
	if _, err := db.ExecContext(ctx,
		"UPDATE projection_jobs SET payload_json=?,payload_sha256=? WHERE revision_id=?",
		string(wrongPayload), hex.EncodeToString(digest[:]), revisionID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID); err == nil {
		t.Fatal("stored job with a valid checksum but another capture was accepted")
	}
}

func TestProjectionJobLeaseIsExclusiveAndAckRequiresCurrentToken(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("6", 64)
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	registerAndEnqueueProjectionJob(t, db, revisionID, "capture-lease", now)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, now, time.Minute, 3)
	if err != nil || !found {
		_ = tx.Rollback()
		t.Fatalf("claim due job: found=%v err=%v", found, err)
	}
	if claimed.State != "leased" || claimed.AttemptCount != 1 || !claimed.LeaseToken.Valid || claimed.LeaseToken.String == "" || claimed.ObservationID != "capture-lease" {
		_ = tx.Rollback()
		t.Fatalf("claimed job fields = %+v", claimed)
	}
	if claimed.PayloadBundle == nil {
		_ = tx.Rollback()
		t.Fatal("claimed job did not retain its already-validated graph bundle")
	}
	observation, err := projectionObservationIDFromBundle(claimed.PayloadBundle, claimed.ObservationID)
	if err != nil || !graphIDPattern.MatchString(observation) {
		_ = tx.Rollback()
		t.Fatalf("claimed validated bundle observation = %q, err=%v", observation, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, now.Add(30*time.Second), time.Minute, 3); err != nil || found {
		_ = tx.Rollback()
		t.Fatalf("active lease was claimable again: found=%v err=%v", found, err)
	}
	_ = tx.Rollback()

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeasedProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID, strings.Repeat("b", 32), claimed.PayloadSHA256, now.Add(31*time.Second)); !errors.Is(err, ErrProjectionJobNotPending) {
		_ = tx.Rollback()
		t.Fatalf("stale lease token acknowledgement error = %v", err)
	}
	if err := MarkLeasedProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID, claimed.LeaseToken.String, claimed.PayloadSHA256, now.Add(31*time.Second)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("current lease token acknowledgement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	job, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || job.State != "applied" {
		t.Fatalf("leased job state = %q err=%v, want applied", job.State, err)
	}
}

func TestProjectionJobFailureSchedulesRetryThenDeadLetters(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("5", 64)
	now := time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)
	registerAndEnqueueProjectionJob(t, db, revisionID, "capture-retry", now)

	claim := func(at time.Time) ProjectionJob {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		job, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, at, time.Minute, 2)
		if err != nil || !found {
			_ = tx.Rollback()
			t.Fatalf("claim retry job at %s: found=%v err=%v", at, found, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return job
	}
	fail := func(job ProjectionJob, at time.Time, next time.Time) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := FailLeasedProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, job.LeaseToken.String, job.PayloadSHA256, "projection_unavailable", at, next, 2); err != nil {
			_ = tx.Rollback()
			t.Fatalf("schedule projection retry: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	first := claim(now)
	retryAt := now.Add(10 * time.Second)
	fail(first, now.Add(time.Second), retryAt)
	job, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || job.State != "pending" || job.AttemptCount != 1 || job.NextAttemptAt != retryAt.Format(projectionTimestampLayout) || !job.LastErrorCode.Valid || job.LastErrorCode.String != "projection_unavailable" {
		t.Fatalf("retry state = %+v err=%v", job, err)
	}

	second := claim(retryAt)
	fail(second, retryAt.Add(time.Second), retryAt.Add(20*time.Second))
	job, err = ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || job.State != "dead_letter" || job.AttemptCount != 2 {
		t.Fatalf("exhausted retry state = %+v err=%v, want dead_letter", job, err)
	}
}

func TestProjectionJobExpiredLeaseCanBeReclaimed(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("4", 64)
	now := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)
	registerAndEnqueueProjectionJob(t, db, revisionID, "capture-expired", now)

	claimAt := func(at time.Time) ProjectionJob {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		job, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, at, time.Second, 3)
		if err != nil || !found {
			_ = tx.Rollback()
			t.Fatalf("claim expired projection: found=%v err=%v", found, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return job
	}
	first := claimAt(now)
	second := claimAt(now.Add(2 * time.Second))
	if first.LeaseToken.String == second.LeaseToken.String || second.AttemptCount != 2 {
		t.Fatalf("expired lease was not replaced: first=%+v second=%+v", first, second)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeasedProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID, second.LeaseToken.String, second.PayloadSHA256, now.Add(3*time.Second)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("acknowledge reclaimed lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionJobClaimQuarantinesCorruptJobAndContinues(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)
	corruptRevision, validRevision := strings.Repeat("1", 64), strings.Repeat("2", 64)
	registerAndEnqueueProjectionJob(t, db, corruptRevision, "capture-corrupt", now)
	registerAndEnqueueProjectionJob(t, db, validRevision, "capture-valid", now.Add(time.Second))
	if _, err := db.ExecContext(ctx,
		"UPDATE projection_jobs SET payload_sha256=? WHERE revision_id=?", strings.Repeat("f", 64), corruptRevision,
	); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, now.Add(2*time.Second), time.Minute, 3)
	if err != nil || !found || claimed.RevisionID != corruptRevision || claimed.State != "dead_letter" {
		_ = tx.Rollback()
		t.Fatalf("claim should report quarantined job: job=%+v found=%v err=%v", claimed, found, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := db.QueryRowContext(ctx,
		"SELECT state,last_error_code FROM projection_jobs WHERE revision_id=?", corruptRevision,
	).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "dead_letter" || reason != "invalid_job" {
		t.Fatalf("corrupt job state=%q reason=%q, want dead_letter/invalid_job", state, reason)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	valid, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, now.Add(3*time.Second), time.Minute, 3)
	if err != nil || !found || valid.RevisionID != validRevision || valid.State != "leased" {
		_ = tx.Rollback()
		t.Fatalf("claim valid job after quarantine: job=%+v found=%v err=%v", valid, found, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestLeasedProjectionAcknowledgementRequiresPayloadDigest(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("3", 64)
	now := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	registerAndEnqueueProjectionJob(t, db, revisionID, "capture-digest-fence", now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := ClaimNextProjectionJob(ctx, tx, projectionTestProjectKey, now, time.Minute, 3)
	if err != nil || !found {
		_ = tx.Rollback()
		t.Fatalf("claim digest-fenced job: found=%v err=%v", found, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeasedProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID,
		claimed.LeaseToken.String, strings.Repeat("f", 64), now.Add(time.Second)); !errors.Is(err, ErrProjectionJobConflict) {
		_ = tx.Rollback()
		t.Fatalf("wrong payload digest acknowledgement error=%v", err)
	}
	_ = tx.Rollback()
	job, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || job.State != "leased" {
		t.Fatalf("wrong digest changed the leased job: job=%+v err=%v", job, err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE projection_jobs SET payload_json=? WHERE revision_id=?", "{}", revisionID); err != nil {
		t.Fatalf("tamper claimed payload fixture: %v", err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLeasedProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID,
		claimed.LeaseToken.String, claimed.PayloadSHA256, now.Add(2*time.Second)); !errors.Is(err, ErrProjectionJobConflict) {
		_ = tx.Rollback()
		t.Fatalf("changed payload acknowledgement error=%v, want digest conflict", err)
	}
	_ = tx.Rollback()
	if _, err := db.ExecContext(ctx,
		"UPDATE projection_jobs SET payload_json=? WHERE revision_id=?", claimed.PayloadJSON, revisionID); err != nil {
		t.Fatalf("restore claimed payload fixture: %v", err)
	}
	job, err = ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || job.State != "leased" {
		t.Fatalf("changed payload modified the lease: job=%+v err=%v", job, err)
	}
}

func TestReconciledProjectionAcknowledgementRequiresPendingJobDigest(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("4", 64)
	now := time.Date(2026, 10, 23, 18, 30, 0, 0, time.UTC)
	registerAndEnqueueProjectionJob(t, db, revisionID, "capture-reconcile-fence", now)
	job, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkReconciledProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID,
		strings.Repeat("f", 64), now.Add(time.Second)); !errors.Is(err, ErrProjectionJobConflict) {
		_ = tx.Rollback()
		t.Fatalf("wrong digest reconciliation error=%v", err)
	}
	_ = tx.Rollback()
	current, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || current.State != "pending" {
		t.Fatalf("wrong digest changed the job: job=%+v err=%v", current, err)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkReconciledProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID,
		job.PayloadSHA256, now.Add(time.Second)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("mark exact job reconciled: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err = ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil || current.State != "applied" || current.LastErrorCode.Valid || !current.AppliedAt.Valid {
		t.Fatalf("reconciled job state = %+v, err=%v", current, err)
	}
}

func registerAndEnqueueProjectionJob(t *testing.T, db *sql.DB, revisionID, captureID string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, captureID, createdAt)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, captureID, minimalProjectionPayload(projectionTestProjectKey, captureID), createdAt); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkProjectionJobAppliedAcknowledgesPendingAndIsIdempotent(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("d", 64)
	now := time.Date(2026, 10, 23, 13, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-ack", now)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-ack", minimalProjectionPayload(projectionTestProjectKey, "observation-ack"), now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := MarkProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID, now.Add(time.Minute)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("acknowledgement attempt %d: %v", attempt+1, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	job, err := ProjectionJobByRevision(ctx, db, projectionTestProjectKey, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "applied" || !job.AppliedAt.Valid || job.AppliedAt.String != now.Add(time.Minute).Format(projectionTimestampLayout) {
		t.Fatalf("acknowledged job state = %+v", job)
	}
}

func TestMarkProjectionJobAppliedRejectsLeasedJob(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("c", 64)
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-leased", now)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-leased", minimalProjectionPayload(projectionTestProjectKey, "observation-leased"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE projection_jobs SET state='leased',lease_token=?,lease_until=? WHERE revision_id=?", strings.Repeat("a", 32), now.Add(time.Minute).Format(projectionTimestampLayout), revisionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = MarkProjectionJobApplied(ctx, tx, projectionTestProjectKey, revisionID, now.Add(2*time.Minute))
	_ = tx.Rollback()
	if !errors.Is(err, ErrProjectionJobNotPending) {
		t.Fatalf("leased job acknowledgement error = %v, want %v", err, ErrProjectionJobNotPending)
	}
}

func TestProjectionJobProjectKeyMustMatchDatabaseMetadata(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	revisionID := strings.Repeat("8", 64)
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-project-isolation", now)
	otherProjectKey := strings.Repeat("f", 64)
	if _, _, err := EnqueueProjectionJob(ctx, tx, otherProjectKey, revisionID, "observation-project-isolation", minimalProjectionPayload(otherProjectKey, "observation-project-isolation"), now); !errors.Is(err, ErrProjectionProjectMismatch) {
		_ = tx.Rollback()
		t.Fatalf("enqueue accepted a project key that does not match database metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs WHERE revision_id=?", revisionID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatal("mismatched enqueue wrote a projection job")
	}

	payload := minimalProjectionPayload(otherProjectKey, "observation-project-isolation")
	checksum := sha256.Sum256(payload)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projection_jobs(job_id,revision_id,payload_json,payload_sha256,state,attempt_count,
		 next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,'pending',0,?,?,?)`,
		"projection-"+revisionID, revisionID, string(payload), hex.EncodeToString(checksum[:]),
		now.UTC().Format(projectionTimestampLayout), now.UTC().Format(projectionTimestampLayout), now.UTC().Format(projectionTimestampLayout),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectionJobByRevision(ctx, db, otherProjectKey, revisionID); !errors.Is(err, ErrProjectionProjectMismatch) {
		t.Fatalf("read accepted a project key that does not match database metadata: %v", err)
	}
}

func TestProjectionJobTimestampsSortChronologically(t *testing.T) {
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	ctx := context.Background()
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	earlierRevision, laterRevision := strings.Repeat("3", 64), strings.Repeat("4", 64)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	registerProjectionRevision(t, ctx, tx, laterRevision, "observation-4", base)
	registerProjectionRevision(t, ctx, tx, earlierRevision, "observation-5", base)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, laterRevision, "observation-4", minimalProjectionPayload(projectionTestProjectKey, "observation-4"), base.Add(110*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, earlierRevision, "observation-5", minimalProjectionPayload(projectionTestProjectKey, "observation-5"), base.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx,
		"SELECT revision_id FROM projection_jobs ORDER BY next_attempt_at,sequence_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var first string
	if !rows.Next() || rows.Scan(&first) != nil || first != earlierRevision {
		t.Fatalf("earliest due job did not sort first: %q", first)
	}
}

func minimalProjectionPayload(project, captureID string) []byte {
	return projectionPayloadWithMarker(project, captureID, "original")
}

func projectionPayloadWithMarker(project, captureID, marker string) []byte {
	graphObservationID := sha256.Sum256([]byte(captureID + "\x00" + marker))
	source := `{"path":"/tmp/projection-fixture.jsonl","sha256":"` + strings.Repeat("a", 64) + `","record_locator":"line:1"}`
	return []byte(`{"schema_version":1,"program":"` + project + `","sources":[` + source + `],"entities":[{"id":"observation-` + hex.EncodeToString(graphObservationID[:]) + `","kind":"observation","data":{"capture_id":"` + captureID + `","program":"` + project + `","test_marker":"` + marker + `"},"source_refs":[` + source + `]}],"assertions":[],"cards":[]}`)
}

func registerProjectionRevision(t *testing.T, ctx context.Context, tx *sql.Tx, revisionID, observationID string, firstSeen time.Time) {
	t.Helper()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO capture_revision_registry(revision_id,observation_id,source_kind,first_seen_at)
		 VALUES(?,?,'historical',?)`, revisionID, observationID,
		firstSeen.UTC().Format(projectionTimestampLayout),
	); err != nil {
		t.Fatalf("register accepted source revision: %v", err)
	}
}

func openProjectionJobDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS project_meta (
 key TEXT PRIMARY KEY,
 value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS capture_revision_registry (
 revision_id TEXT PRIMARY KEY CHECK(length(revision_id)=64),
 observation_id TEXT NOT NULL,
 source_kind TEXT NOT NULL CHECK(source_kind IN ('historical','live')),
 first_seen_at TEXT NOT NULL,
 UNIQUE(source_kind,observation_id,revision_id)
);
CREATE TABLE IF NOT EXISTS projection_jobs (
 sequence_id INTEGER PRIMARY KEY AUTOINCREMENT,
 job_id TEXT NOT NULL UNIQUE,
 revision_id TEXT NOT NULL UNIQUE REFERENCES capture_revision_registry(revision_id),
 payload_json TEXT NOT NULL,
 payload_sha256 TEXT NOT NULL CHECK(length(payload_sha256)=64),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','leased','applied','dead_letter')),
 attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count>=0),
 next_attempt_at TEXT NOT NULL,
 lease_token TEXT,
 lease_until TEXT,
 last_error_code TEXT,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 applied_at TEXT,
 CHECK((state='leased' AND lease_token IS NOT NULL AND lease_until IS NOT NULL) OR
       (state<>'leased' AND lease_token IS NULL AND lease_until IS NULL))
);
CREATE INDEX IF NOT EXISTS projection_jobs_due_idx ON projection_jobs(state,next_attempt_at,sequence_id);
`)
	if err != nil {
		_ = db.Close()
		t.Fatalf("create projection ledger fixture: %v", err)
	}
	if _, err := db.Exec("INSERT OR IGNORE INTO project_meta(key,value) VALUES('project_key',?)", projectionTestProjectKey); err != nil {
		_ = db.Close()
		t.Fatalf("register projection fixture project key: %v", err)
	}
	return db
}
