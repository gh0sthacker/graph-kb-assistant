package consultation

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateConsultationV1PreservesCandidateHistory(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), consultationDatabaseName)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE consultation_sessions(session_id TEXT PRIMARY KEY)`,
		`CREATE TABLE consultation_candidates(
			session_id TEXT NOT NULL,
			candidate_version INTEGER NOT NULL,
			session_version INTEGER NOT NULL,
			proposal_id TEXT NOT NULL,
			receipt_id TEXT NOT NULL,
			state TEXT NOT NULL,
			created_at_ns INTEGER NOT NULL,
			PRIMARY KEY(session_id,candidate_version)
		)`,
		`CREATE TRIGGER consultation_candidates_no_update BEFORE UPDATE ON consultation_candidates BEGIN SELECT RAISE(ABORT, 'consultation records are immutable'); END`,
		`CREATE TRIGGER consultation_candidates_no_delete BEFORE DELETE ON consultation_candidates BEGIN SELECT RAISE(ABORT, 'consultation records are append-only'); END`,
		`CREATE TABLE consultation_idempotency(
			project_key TEXT NOT NULL, session_id TEXT NOT NULL, operation TEXT NOT NULL, idempotency_key TEXT NOT NULL,
			request_hash TEXT NOT NULL, result_json BLOB NOT NULL, created_at_ns INTEGER NOT NULL,
			PRIMARY KEY(project_key,session_id,operation,idempotency_key)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture SQL failed: %v", err)
		}
	}
	sessionID := testOpaqueID('s')
	if _, err := db.Exec("INSERT INTO consultation_sessions(session_id) VALUES (?)", sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO consultation_candidates(session_id,candidate_version,session_version,proposal_id,receipt_id,state,created_at_ns)
		VALUES (?,1,2,?,?, 'candidate',1)`, sessionID, testOpaqueID('p'), testOpaqueID('r')); err != nil {
		t.Fatal(err)
	}
	legacyReplay := []byte(`{"version":1,"session_version":2,"state":"candidate","proposal_id":"` + testOpaqueID('p') + `","receipt_id":"` + testOpaqueID('r') + `"}`)
	projectKey, requestHash, idempotencyKey := strings.Repeat("a", 64), strings.Repeat("b", 64), testIdempotencyKey("legacy-candidate-save")
	if _, err := db.Exec(`INSERT INTO consultation_idempotency(project_key,session_id,operation,idempotency_key,request_hash,result_json,created_at_ns)
		VALUES (?,?,'save_candidate',?,?,?,1)`, projectKey, sessionID, idempotencyKey, requestHash, legacyReplay); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA application_id = %d", consultationApplicationID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := initializeConsultationSchema(db); err != nil {
		t.Fatalf("initializeConsultationSchema migration: %v", err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var candidateID string
	if err := db.QueryRow("SELECT candidate_id FROM consultation_candidates WHERE session_id=?", sessionID).Scan(&candidateID); err != nil || candidateID != sessionID {
		t.Fatalf("migrated candidate ID=%q err=%v", candidateID, err)
	}
	if _, err := db.Exec("UPDATE consultation_candidates SET state='reproduced' WHERE session_id=?", sessionID); err == nil {
		t.Fatal("migration did not restore the append-only candidate trigger")
	}
	var migratedReplay []byte
	if err := db.QueryRow(`SELECT result_json FROM consultation_idempotency WHERE project_key=? AND session_id=? AND operation='save_candidate' AND idempotency_key=?`, projectKey, sessionID, idempotencyKey).Scan(&migratedReplay); err != nil || string(migratedReplay) != string(legacyReplay) {
		t.Fatalf("migration changed legacy idempotency result=%s err=%v", migratedReplay, err)
	}
	var acceptedReviews int
	if err := db.QueryRow("SELECT count(*) FROM consultation_reviews").Scan(&acceptedReviews); err != nil || acceptedReviews != 0 {
		t.Fatalf("migrated legacy sessions were automatically reviewed: count=%d err=%v", acceptedReviews, err)
	}
}

func testOpaqueID(char byte) string {
	return strings.Repeat(string(char), 43)
}

func downgradeConsultationV2ToV1ForTest(t *testing.T, databasePath, projectKey, sessionID, idempotencyKey string, legacyReplay []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	statements := []string{
		`DROP TRIGGER consultation_context_annotations_no_update`,
		`DROP TRIGGER consultation_context_annotations_no_delete`,
		`DROP INDEX consultation_context_annotations_session_idx`,
		`DROP TABLE consultation_context_annotations`,
		`DROP TRIGGER consultation_candidates_no_update`,
		`DROP INDEX IF EXISTS consultation_candidates_identity_idx`,
		`CREATE TABLE consultation_candidates_legacy_copy AS
			SELECT session_id,candidate_version,session_version,proposal_id,receipt_id,state,created_at_ns FROM consultation_candidates`,
		`DROP TABLE consultation_candidates`,
		`CREATE TABLE consultation_candidates (
			session_id TEXT NOT NULL REFERENCES consultation_sessions(session_id),
			candidate_version INTEGER NOT NULL CHECK(candidate_version >= 1),
			session_version INTEGER NOT NULL,
			proposal_id TEXT NOT NULL REFERENCES consultation_proposals(proposal_id),
			receipt_id TEXT NOT NULL UNIQUE REFERENCES consultation_receipts(receipt_id),
			state TEXT NOT NULL CHECK(state = 'candidate'),
			created_at_ns INTEGER NOT NULL,
			PRIMARY KEY(session_id,candidate_version)
		)`,
		`INSERT INTO consultation_candidates(session_id,candidate_version,session_version,proposal_id,receipt_id,state,created_at_ns)
			SELECT session_id,candidate_version,session_version,proposal_id,receipt_id,state,created_at_ns FROM consultation_candidates_legacy_copy`,
		`DROP TABLE consultation_candidates_legacy_copy`,
		`CREATE TRIGGER consultation_candidates_no_update BEFORE UPDATE ON consultation_candidates BEGIN SELECT RAISE(ABORT, 'consultation records are immutable'); END`,
		`CREATE TRIGGER consultation_candidates_no_delete BEFORE DELETE ON consultation_candidates BEGIN SELECT RAISE(ABORT, 'consultation records are append-only'); END`,
		`DROP TRIGGER consultation_reviews_no_update`,
		`DROP TRIGGER consultation_reviews_no_delete`,
		`DROP TABLE consultation_reviews`,
		`DROP TRIGGER consultation_idempotency_no_update`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("downgrade fixture SQL failed: %v", err)
		}
	}
	if _, err := db.Exec(`UPDATE consultation_idempotency SET result_json=?
		WHERE project_key=? AND session_id=? AND operation='save_candidate' AND idempotency_key=?`, legacyReplay, projectKey, sessionID, idempotencyKey); err != nil {
		t.Fatalf("write legacy candidate replay fixture: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER consultation_idempotency_no_update BEFORE UPDATE ON consultation_idempotency BEGIN SELECT RAISE(ABORT, 'consultation records are immutable'); END`); err != nil {
		t.Fatalf("restore legacy idempotency trigger: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("mark fixture as schema v1: %v", err)
	}
}
