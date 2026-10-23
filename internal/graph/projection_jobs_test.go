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
	first, inserted, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, minimalProjectionPayload(projectionTestProjectKey), now)
	if err != nil || !inserted {
		t.Fatalf("first enqueue: inserted=%v err=%v", inserted, err)
	}
	second, inserted, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, minimalProjectionPayload(projectionTestProjectKey), now.Add(time.Second))
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
	firstPayload := minimalProjectionPayload(projectionTestProjectKey)
	first, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, firstPayload, now)
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
	changedPayload := []byte(`{"schema_version":1,"program":"` + projectionTestProjectKey + `","sources":[{"path":"/tmp/other.jsonl","sha256":"` + strings.Repeat("a", 64) + `","record_locator":"line:2"}],"entities":[],"assertions":[],"cards":[]}`)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, changedPayload, now); !errors.Is(err, ErrProjectionJobConflict) {
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
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, minimalProjectionPayload(projectionTestProjectKey), now); err != nil {
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
	_, _, err = EnqueueProjectionJob(context.Background(), tx, projectionTestProjectKey, strings.Repeat("9", 64), minimalProjectionPayload(projectionTestProjectKey), time.Now().UTC())
	_ = tx.Rollback()
	if !errors.Is(err, ErrProjectionRevisionMissing) {
		t.Fatalf("expected unregistered source revision rejection, got %v", err)
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
	if _, _, err := EnqueueProjectionJob(ctx, tx, otherProjectKey, revisionID, minimalProjectionPayload(otherProjectKey), now); !errors.Is(err, ErrProjectionProjectMismatch) {
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

	payload := minimalProjectionPayload(otherProjectKey)
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
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, laterRevision, minimalProjectionPayload(projectionTestProjectKey), base.Add(110*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, earlierRevision, minimalProjectionPayload(projectionTestProjectKey), base.Add(100*time.Millisecond)); err != nil {
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

func minimalProjectionPayload(project string) []byte {
	return []byte(`{"schema_version":1,"program":"` + project + `","sources":[],"entities":[],"assertions":[],"cards":[]}`)
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
