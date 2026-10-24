package consultation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/publicdata"
	projectstore "bb-graph-assistant/internal/store"

	_ "modernc.org/sqlite"
)

const (
	consultationDatabaseName  = "consultation.sqlite3"
	consultationApplicationID = 0x42424353 // ASCII "BBCS".
	consultationSchemaVersion = 2
	maximumStoredSessions     = 5000
)

type sessionDB struct{ db *sql.DB }

func (s *Service) database(projectKey string, create bool) (*sessionDB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreUnavailable
	}
	if existing := s.dbs[projectKey]; existing != nil {
		return existing, nil
	}
	ref, ok := s.projects[projectKey]
	if !ok {
		return nil, ErrProjectNotRegistered
	}
	directory, err := projectstore.ProjectDirectory(ref, s.dataRoot)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o077 != 0 {
		return nil, ErrStoreUnavailable
	}
	path := filepath.Join(directory, consultationDatabaseName)
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || !create {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrSessionNotFound
			}
			return nil, ErrStoreUnavailable
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, ErrStoreUnavailable
		}
		if chmodErr := file.Chmod(0o600); chmodErr != nil {
			_ = file.Close()
			return nil, ErrStoreUnavailable
		}
		if closeErr := file.Close(); closeErr != nil {
			return nil, ErrStoreUnavailable
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, ErrStoreUnavailable
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, ErrStoreUnavailable
	}
	fileURL := (&url.URL{Scheme: "file", Path: path}).String() + "?_txlock=immediate"
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	fail := func() (*sessionDB, error) {
		_ = db.Close()
		return nil, ErrStoreUnavailable
	}
	if err := db.PingContext(context.Background()); err != nil {
		return fail()
	}
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA foreign_keys = ON", "PRAGMA secure_delete = ON", "PRAGMA synchronous = FULL"} {
		if _, err := db.Exec(pragma); err != nil {
			return fail()
		}
	}
	if err := initializeConsultationSchema(db); err != nil {
		return fail()
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fail()
	}
	opened := &sessionDB{db: db}
	s.dbs[projectKey] = opened
	return opened, nil
}

func initializeConsultationSchema(db *sql.DB) error {
	var applicationID, version int
	if err := db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return err
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if applicationID == 0 && version == 0 {
		var userTables int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&userTables); err != nil || userTables != 0 {
			return errors.New("unversioned consultation database is not empty")
		}
		statements := []string{
			`CREATE TABLE consultation_sessions (
				session_id TEXT PRIMARY KEY CHECK(length(session_id) = 43),
				project_key TEXT NOT NULL CHECK(length(project_key) = 64),
				version INTEGER NOT NULL CHECK(version >= 1),
				question TEXT NOT NULL,
				state TEXT NOT NULL CHECK(state = 'open'),
				evidence_json BLOB NOT NULL,
				references_json BLOB NOT NULL,
				created_at_ns INTEGER NOT NULL,
				updated_at_ns INTEGER NOT NULL
			)`,
			`CREATE TABLE consultation_comparisons (
				comparison_id TEXT PRIMARY KEY CHECK(length(comparison_id) = 43),
				session_id TEXT NOT NULL REFERENCES consultation_sessions(session_id),
				session_version INTEGER NOT NULL,
				result_json BLOB NOT NULL,
				created_at_ns INTEGER NOT NULL
			)`,
			`CREATE INDEX consultation_comparisons_session_idx ON consultation_comparisons(session_id, created_at_ns, comparison_id)`,
			`CREATE TABLE consultation_proposals (
				proposal_id TEXT PRIMARY KEY CHECK(length(proposal_id) = 43),
				session_id TEXT NOT NULL REFERENCES consultation_sessions(session_id),
				session_version INTEGER NOT NULL,
				proposal_json BLOB NOT NULL,
				evidence_ids_json BLOB NOT NULL,
				comparison_ids_json BLOB NOT NULL,
				proposal_digest TEXT NOT NULL CHECK(length(proposal_digest) = 64),
				created_at_ns INTEGER NOT NULL
			)`,
			`CREATE INDEX consultation_proposals_session_idx ON consultation_proposals(session_id, created_at_ns, proposal_id)`,
			`CREATE TABLE consultation_receipts (
				receipt_id TEXT PRIMARY KEY CHECK(length(receipt_id) = 43),
				session_id TEXT NOT NULL REFERENCES consultation_sessions(session_id),
				session_version INTEGER NOT NULL,
				proposal_id TEXT NOT NULL REFERENCES consultation_proposals(proposal_id),
				proposal_digest TEXT NOT NULL CHECK(length(proposal_digest) = 64),
				evidence_ids_json BLOB NOT NULL,
				comparison_ids_json BLOB NOT NULL,
				policy_version TEXT NOT NULL,
				status TEXT NOT NULL CHECK(status = 'references_validated'),
				validated_at_ns INTEGER NOT NULL
			)`,
			`CREATE INDEX consultation_receipts_session_idx ON consultation_receipts(session_id, validated_at_ns, receipt_id)`,
			`CREATE TABLE consultation_candidates (
				session_id TEXT NOT NULL REFERENCES consultation_sessions(session_id),
				candidate_id TEXT NOT NULL CHECK(length(candidate_id) = 43),
				candidate_version INTEGER NOT NULL CHECK(candidate_version >= 1),
				session_version INTEGER NOT NULL,
				proposal_id TEXT NOT NULL REFERENCES consultation_proposals(proposal_id),
				receipt_id TEXT NOT NULL UNIQUE REFERENCES consultation_receipts(receipt_id),
				state TEXT NOT NULL CHECK(state = 'candidate'),
				created_at_ns INTEGER NOT NULL,
				PRIMARY KEY(session_id, candidate_id, candidate_version)
			)`,
			`CREATE TABLE consultation_reviews (
				session_id TEXT PRIMARY KEY REFERENCES consultation_sessions(session_id),
				session_version INTEGER NOT NULL,
				accepted_at_ns INTEGER NOT NULL
			)`,
			`CREATE TABLE consultation_idempotency (
				project_key TEXT NOT NULL CHECK(length(project_key) = 64),
				session_id TEXT NOT NULL,
				operation TEXT NOT NULL,
				idempotency_key TEXT NOT NULL,
				request_hash TEXT NOT NULL CHECK(length(request_hash) = 64),
				result_json BLOB NOT NULL,
				created_at_ns INTEGER NOT NULL,
				PRIMARY KEY(project_key, session_id, operation, idempotency_key)
			)`,
			`CREATE TRIGGER consultation_sessions_identity_immutable BEFORE UPDATE ON consultation_sessions
				WHEN old.session_id != new.session_id OR old.project_key != new.project_key OR old.question != new.question OR
				old.state != new.state OR old.evidence_json != new.evidence_json OR old.references_json != new.references_json OR old.created_at_ns != new.created_at_ns
				BEGIN SELECT RAISE(ABORT, 'consultation session identity is immutable'); END`,
		}
		for _, table := range []string{"comparisons", "proposals", "receipts", "candidates", "reviews", "idempotency"} {
			statements = append(statements,
				fmt.Sprintf(`CREATE TRIGGER consultation_%s_no_update BEFORE UPDATE ON consultation_%s BEGIN SELECT RAISE(ABORT, 'consultation records are immutable'); END`, table, table),
				fmt.Sprintf(`CREATE TRIGGER consultation_%s_no_delete BEFORE DELETE ON consultation_%s BEGIN SELECT RAISE(ABORT, 'consultation records are append-only'); END`, table, table),
			)
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", consultationApplicationID)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", consultationSchemaVersion)); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	if applicationID == consultationApplicationID && version == 1 {
		return migrateConsultationV1ToV2(db)
	}
	if applicationID != consultationApplicationID || version != consultationSchemaVersion {
		return errors.New("consultation database identity or schema version is unsupported")
	}
	return nil
}

func migrateConsultationV1ToV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`DROP TRIGGER consultation_candidates_no_update`,
		`ALTER TABLE consultation_candidates ADD COLUMN candidate_id TEXT NOT NULL DEFAULT ''`,
		`UPDATE consultation_candidates SET candidate_id = session_id WHERE candidate_id = ''`,
		`CREATE UNIQUE INDEX consultation_candidates_identity_idx ON consultation_candidates(session_id, candidate_id, candidate_version)`,
		`CREATE TRIGGER consultation_candidates_no_update BEFORE UPDATE ON consultation_candidates BEGIN SELECT RAISE(ABORT, 'consultation records are immutable'); END`,
		`CREATE TABLE consultation_reviews (
			session_id TEXT PRIMARY KEY REFERENCES consultation_sessions(session_id),
			session_version INTEGER NOT NULL,
			accepted_at_ns INTEGER NOT NULL
		)`,
		`CREATE TRIGGER consultation_reviews_no_update BEFORE UPDATE ON consultation_reviews BEGIN SELECT RAISE(ABORT, 'consultation review records are immutable'); END`,
		`CREATE TRIGGER consultation_reviews_no_delete BEFORE DELETE ON consultation_reviews BEGIN SELECT RAISE(ABORT, 'consultation review records are append-only'); END`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("PRAGMA user_version = 2"); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *sessionDB) close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *sessionDB) lookupMutation(ctx context.Context, projectKey, sessionID, operation, idempotencyKey, requestHash string) ([]byte, bool, error) {
	var storedHash string
	var result []byte
	err := d.db.QueryRowContext(ctx, `SELECT request_hash, result_json FROM consultation_idempotency
		WHERE project_key = ? AND session_id = ? AND operation = ? AND idempotency_key = ?`, projectKey, sessionID, operation, idempotencyKey).Scan(&storedHash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrStoreUnavailable
	}
	if storedHash != requestHash {
		return nil, false, ErrIdempotencyConflict
	}
	if publicdataValidate(result) != nil {
		return nil, false, ErrStoreUnavailable
	}
	return append([]byte(nil), result...), true, nil
}

func (d *sessionDB) createSession(ctx context.Context, session Session, idempotencyKey, requestHash string) (Session, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	defer tx.Rollback()
	if encoded, found, err := lookupMutationTx(ctx, tx, session.ProjectKey, "", "create_session", idempotencyKey, requestHash); err != nil {
		return Session{}, err
	} else if found {
		var replay Session
		if decodeSafe(encoded, &replay) != nil {
			return Session{}, ErrStoreUnavailable
		}
		return replay, nil
	}
	var total int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM consultation_sessions").Scan(&total); err != nil {
		return Session{}, ErrStoreUnavailable
	}
	if total >= maximumStoredSessions {
		return Session{}, ErrLimitReached
	}
	evidenceJSON, err := encodeSafe(session.Evidence)
	if err != nil {
		return Session{}, ErrEvidenceUnavailable
	}
	referencesJSON, err := encodeSafe(session.References)
	if err != nil {
		return Session{}, ErrEvidenceUnavailable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_sessions
		(session_id, project_key, version, question, state, evidence_json, references_json, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, session.ProjectKey, session.Version, session.Question, session.State,
		evidenceJSON, referencesJSON, session.CreatedAt.UnixNano(), session.UpdatedAt.UnixNano()); err != nil {
		return Session{}, ErrStoreUnavailable
	}
	encoded, err := encodeSafe(session)
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	if err := insertMutation(ctx, tx, session.ProjectKey, "", "create_session", idempotencyKey, requestHash, encoded, session.CreatedAt); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, ErrStoreUnavailable
	}
	return session, nil
}

func (d *sessionDB) readSession(ctx context.Context, projectKey, sessionID string) (Session, error) {
	var session Session
	var evidenceJSON, referencesJSON []byte
	var createdAt, updatedAt int64
	err := d.db.QueryRowContext(ctx, `SELECT session_id, project_key, version, question, state, evidence_json, references_json, created_at_ns, updated_at_ns
		FROM consultation_sessions WHERE project_key = ? AND session_id = ?`, projectKey, sessionID).Scan(
		&session.ID, &session.ProjectKey, &session.Version, &session.Question, &session.State, &evidenceJSON, &referencesJSON, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	if decodeSafe(evidenceJSON, &session.Evidence) != nil || decodeSafe(referencesJSON, &session.References) != nil {
		return Session{}, ErrStoreUnavailable
	}
	session.CreatedAt = time.Unix(0, createdAt).UTC()
	session.UpdatedAt = time.Unix(0, updatedAt).UTC()
	var acceptedVersion, acceptedAt int64
	err = d.db.QueryRowContext(ctx, `SELECT session_version, accepted_at_ns FROM consultation_reviews WHERE session_id = ?`, sessionID).Scan(&acceptedVersion, &acceptedAt)
	if err == nil {
		session.ReviewAccepted = true
		session.ReviewAcceptedVersion = int(acceptedVersion)
		session.ReviewAcceptedAt = time.Unix(0, acceptedAt).UTC()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrStoreUnavailable
	}
	session.Comparisons = []ComparisonRecord{}
	session.Proposals = []ProposalRecord{}
	session.Receipts = []ValidationReceipt{}
	session.Candidates = []CandidateVersion{}
	rows, err := d.db.QueryContext(ctx, `SELECT comparison_id, session_version, result_json, created_at_ns FROM consultation_comparisons
		WHERE session_id = ? ORDER BY created_at_ns, comparison_id`, sessionID)
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	for rows.Next() {
		var item ComparisonRecord
		var resultJSON []byte
		var created int64
		if err := rows.Scan(&item.ID, &item.SessionVersion, &resultJSON, &created); err != nil || decodeSafe(resultJSON, &item.Result) != nil {
			_ = rows.Close()
			return Session{}, ErrStoreUnavailable
		}
		item.CreatedAt = time.Unix(0, created).UTC()
		session.Comparisons = append(session.Comparisons, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Session{}, ErrStoreUnavailable
	}
	_ = rows.Close()
	rows, err = d.db.QueryContext(ctx, `SELECT proposal_id, session_version, proposal_json, evidence_ids_json, comparison_ids_json, proposal_digest, created_at_ns FROM consultation_proposals
		WHERE session_id = ? ORDER BY created_at_ns, proposal_id`, sessionID)
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	for rows.Next() {
		var item ProposalRecord
		var proposalJSON, evidenceIDsJSON, comparisonIDsJSON []byte
		var created int64
		if err := rows.Scan(&item.ID, &item.SessionVersion, &proposalJSON, &evidenceIDsJSON, &comparisonIDsJSON, &item.Digest, &created); err != nil ||
			decodeSafe(proposalJSON, &item.Proposal) != nil || decodeSafe(evidenceIDsJSON, &item.EvidenceReferenceIDs) != nil || decodeSafe(comparisonIDsJSON, &item.ComparisonIDs) != nil {
			_ = rows.Close()
			return Session{}, ErrStoreUnavailable
		}
		item.CreatedAt = time.Unix(0, created).UTC()
		session.Proposals = append(session.Proposals, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Session{}, ErrStoreUnavailable
	}
	_ = rows.Close()
	rows, err = d.db.QueryContext(ctx, `SELECT receipt_id, session_version, proposal_id, proposal_digest, evidence_ids_json, comparison_ids_json, policy_version, status, validated_at_ns FROM consultation_receipts
		WHERE session_id = ? ORDER BY validated_at_ns, receipt_id`, sessionID)
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	for rows.Next() {
		var item ValidationReceipt
		var evidenceIDsJSON, comparisonIDsJSON []byte
		var validated int64
		if err := rows.Scan(&item.ID, &item.SessionVersion, &item.ProposalID, &item.ProposalDigest, &evidenceIDsJSON, &comparisonIDsJSON, &item.PolicyVersion, &item.Status, &validated); err != nil ||
			decodeSafe(evidenceIDsJSON, &item.EvidenceReferenceIDs) != nil || decodeSafe(comparisonIDsJSON, &item.ComparisonIDs) != nil {
			_ = rows.Close()
			return Session{}, ErrStoreUnavailable
		}
		item.ValidatedAt = time.Unix(0, validated).UTC()
		session.Receipts = append(session.Receipts, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Session{}, ErrStoreUnavailable
	}
	_ = rows.Close()
	rows, err = d.db.QueryContext(ctx, `SELECT candidate_id, candidate_version, session_version, proposal_id, receipt_id, state, created_at_ns FROM consultation_candidates
		WHERE session_id = ? ORDER BY candidate_id, candidate_version`, sessionID)
	if err != nil {
		return Session{}, ErrStoreUnavailable
	}
	proposalMap := make(map[string]ProposalRecord)
	for _, proposal := range session.Proposals {
		proposalMap[proposal.ID] = proposal
	}
	receiptMap := make(map[string]ValidationReceipt)
	for _, receipt := range session.Receipts {
		receiptMap[receipt.ID] = receipt
	}
	refs := referenceMap(session)
	for rows.Next() {
		var item CandidateVersion
		var created int64
		if err := rows.Scan(&item.CandidateID, &item.Version, &item.SessionVersion, &item.ProposalID, &item.ReceiptID, &item.State, &created); err != nil {
			_ = rows.Close()
			return Session{}, ErrStoreUnavailable
		}
		proposal, proposalOK := proposalMap[item.ProposalID]
		receipt, receiptOK := receiptMap[item.ReceiptID]
		if !proposalOK || !receiptOK || receipt.ProposalID != proposal.ID || receipt.ProposalDigest != proposal.Digest || item.State != CandidateStateCandidate {
			_ = rows.Close()
			return Session{}, ErrStoreUnavailable
		}
		item.Proposal = proposal.Proposal
		item.ComparisonIDs = append([]string(nil), proposal.ComparisonIDs...)
		item.Citations = make([]EvidenceCitation, 0, len(receipt.EvidenceReferenceIDs))
		for _, referenceID := range receipt.EvidenceReferenceIDs {
			ref, ok := refs[referenceID]
			if !ok || ref.SourceIntegrityStatus != "verified" || !revisionPattern.MatchString(ref.SHA256) {
				_ = rows.Close()
				return Session{}, ErrStoreUnavailable
			}
			item.Citations = append(item.Citations, EvidenceCitation{
				EvidenceReferenceID: ref.ID, RequestID: ref.RequestID, RevisionID: ref.RevisionID, Side: ref.Side,
				SpanStart: ref.SpanStart, SpanEnd: ref.SpanEnd, SHA256: ref.SHA256,
			})
		}
		item.CreatedAt = time.Unix(0, created).UTC()
		session.Candidates = append(session.Candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Session{}, ErrStoreUnavailable
	}
	_ = rows.Close()
	if err := validateStoredSession(session); err != nil {
		return Session{}, ErrStoreUnavailable
	}
	return session, nil
}

func (d *sessionDB) appendReview(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey, requestHash string, session Session) (Session, error) {
	newVersion := expectedVersion + 1
	acceptedAt := time.Now().UTC()
	session.ReviewAccepted = true
	session.ReviewAcceptedVersion = newVersion
	session.ReviewAcceptedAt = acceptedAt
	session.Version = newVersion
	session.UpdatedAt = acceptedAt
	encoded, err := d.mutateSession(ctx, projectKey, sessionID, expectedVersion, "accept_review", idempotencyKey, requestHash, newVersion, func(tx *sql.Tx) (any, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_reviews(session_id, session_version, accepted_at_ns) VALUES (?, ?, ?)`, sessionID, newVersion, acceptedAt.UnixNano()); err != nil {
			return nil, ErrSessionVersionConflict
		}
		return session, nil
	})
	if err != nil {
		return Session{}, err
	}
	var result Session
	if decodeSafe(encoded, &result) != nil {
		return Session{}, ErrStoreUnavailable
	}
	return result, nil
}

func (d *sessionDB) appendComparison(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey, requestHash string, record ComparisonRecord) (ComparisonRecord, error) {
	encoded, err := d.mutateSession(ctx, projectKey, sessionID, expectedVersion, "compare", idempotencyKey, requestHash, record.SessionVersion, func(tx *sql.Tx) (any, error) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM consultation_comparisons WHERE session_id = ?", sessionID).Scan(&count); err != nil || count >= maximumSessionComparisons {
			return nil, ErrLimitReached
		}
		resultJSON, err := encodeSafe(record.Result)
		if err != nil {
			return nil, ErrComparisonUnavailable
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_comparisons(comparison_id, session_id, session_version, result_json, created_at_ns) VALUES (?, ?, ?, ?, ?)`,
			record.ID, sessionID, record.SessionVersion, resultJSON, record.CreatedAt.UnixNano()); err != nil {
			return nil, ErrStoreUnavailable
		}
		return record, nil
	})
	if err != nil {
		return ComparisonRecord{}, err
	}
	var result ComparisonRecord
	if decodeSafe(encoded, &result) != nil {
		return ComparisonRecord{}, ErrStoreUnavailable
	}
	return result, nil
}

func (d *sessionDB) appendProposal(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey, requestHash string, record ProposalRecord) (ProposalRecord, error) {
	encoded, err := d.mutateSession(ctx, projectKey, sessionID, expectedVersion, "record_proposal", idempotencyKey, requestHash, record.SessionVersion, func(tx *sql.Tx) (any, error) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM consultation_proposals WHERE session_id = ?", sessionID).Scan(&count); err != nil || count >= maximumProposals {
			return nil, ErrLimitReached
		}
		proposalJSON, err := encodeSafe(record.Proposal)
		if err != nil {
			return nil, ErrInvalidInput
		}
		evidenceIDsJSON, err := encodeSafe(record.EvidenceReferenceIDs)
		if err != nil {
			return nil, ErrInvalidInput
		}
		comparisonIDsJSON, err := encodeSafe(record.ComparisonIDs)
		if err != nil {
			return nil, ErrInvalidInput
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_proposals(proposal_id, session_id, session_version, proposal_json, evidence_ids_json, comparison_ids_json, proposal_digest, created_at_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, record.ID, sessionID, record.SessionVersion, proposalJSON, evidenceIDsJSON, comparisonIDsJSON, record.Digest, record.CreatedAt.UnixNano()); err != nil {
			return nil, ErrStoreUnavailable
		}
		return record, nil
	})
	if err != nil {
		return ProposalRecord{}, err
	}
	var result ProposalRecord
	if decodeSafe(encoded, &result) != nil {
		return ProposalRecord{}, ErrStoreUnavailable
	}
	return result, nil
}

func (d *sessionDB) appendReceipt(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey, requestHash string, record ValidationReceipt) (ValidationReceipt, error) {
	encoded, err := d.mutateSession(ctx, projectKey, sessionID, expectedVersion, "validate_proposal", idempotencyKey, requestHash, record.SessionVersion, func(tx *sql.Tx) (any, error) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM consultation_receipts WHERE session_id = ?", sessionID).Scan(&count); err != nil {
			return nil, ErrStoreUnavailable
		}
		if count >= maximumValidationReceipts {
			return nil, ErrLimitReached
		}
		evidenceIDsJSON, err := encodeSafe(record.EvidenceReferenceIDs)
		if err != nil {
			return nil, ErrCitationInvalid
		}
		comparisonIDsJSON, err := encodeSafe(record.ComparisonIDs)
		if err != nil {
			return nil, ErrCitationInvalid
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_receipts(receipt_id, session_id, session_version, proposal_id, proposal_digest, evidence_ids_json, comparison_ids_json, policy_version, status, validated_at_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.ID, sessionID, record.SessionVersion, record.ProposalID, record.ProposalDigest, evidenceIDsJSON, comparisonIDsJSON, record.PolicyVersion, record.Status, record.ValidatedAt.UnixNano()); err != nil {
			return nil, ErrStoreUnavailable
		}
		return record, nil
	})
	if err != nil {
		return ValidationReceipt{}, err
	}
	var result ValidationReceipt
	if decodeSafe(encoded, &result) != nil {
		return ValidationReceipt{}, ErrStoreUnavailable
	}
	return result, nil
}

func (d *sessionDB) appendCandidate(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey, requestHash, candidateID string, proposal ProposalRecord, receipt ValidationReceipt, citations []EvidenceCitation) (CandidateVersion, error) {
	newVersion := expectedVersion + 1
	encoded, err := d.mutateSession(ctx, projectKey, sessionID, expectedVersion, "save_candidate", idempotencyKey, requestHash, newVersion, func(tx *sql.Tx) (any, error) {
		var total int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM consultation_candidates WHERE session_id = ?", sessionID).Scan(&total); err != nil {
			return nil, ErrStoreUnavailable
		}
		if total >= maximumCandidateVersions {
			return nil, ErrLimitReached
		}
		var version int
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(candidate_version), 0) + 1 FROM consultation_candidates WHERE session_id = ? AND candidate_id = ?", sessionID, candidateID).Scan(&version); err != nil {
			return nil, ErrStoreUnavailable
		}
		if version > maximumCandidateVersions {
			return nil, ErrLimitReached
		}
		candidate := CandidateVersion{
			CandidateID: candidateID, Version: version, SessionVersion: newVersion, State: CandidateStateCandidate, ProposalID: proposal.ID,
			ReceiptID: receipt.ID, Proposal: proposal.Proposal, Citations: append([]EvidenceCitation(nil), citations...),
			ComparisonIDs: append([]string(nil), proposal.ComparisonIDs...), CreatedAt: time.Now().UTC(),
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_candidates(session_id, candidate_id, candidate_version, session_version, proposal_id, receipt_id, state, created_at_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, candidate.CandidateID, candidate.Version, candidate.SessionVersion, candidate.ProposalID, candidate.ReceiptID, candidate.State, candidate.CreatedAt.UnixNano()); err != nil {
			return nil, ErrReceiptInvalid
		}
		return candidate, nil
	})
	if err != nil {
		return CandidateVersion{}, err
	}
	var result CandidateVersion
	if decodeSafe(encoded, &result) != nil {
		return CandidateVersion{}, ErrStoreUnavailable
	}
	return result, nil
}

func (d *sessionDB) mutateSession(ctx context.Context, projectKey, sessionID string, expectedVersion int, operation, idempotencyKey, requestHash string, newVersion int, change func(*sql.Tx) (any, error)) ([]byte, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	defer tx.Rollback()
	if encoded, found, err := lookupMutationTx(ctx, tx, projectKey, sessionID, operation, idempotencyKey, requestHash); err != nil {
		return nil, err
	} else if found {
		return encoded, nil
	}
	var currentVersion int
	err = tx.QueryRowContext(ctx, "SELECT version FROM consultation_sessions WHERE project_key = ? AND session_id = ?", projectKey, sessionID).Scan(&currentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	if currentVersion != expectedVersion || newVersion != expectedVersion+1 {
		return nil, ErrSessionVersionConflict
	}
	result, err := change(tx)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeSafe(result)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	now := time.Now().UTC()
	updated, err := tx.ExecContext(ctx, "UPDATE consultation_sessions SET version = ?, updated_at_ns = ? WHERE project_key = ? AND session_id = ? AND version = ?",
		newVersion, now.UnixNano(), projectKey, sessionID, expectedVersion)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	rows, err := updated.RowsAffected()
	if err != nil || rows != 1 {
		return nil, ErrSessionVersionConflict
	}
	if err := insertMutation(ctx, tx, projectKey, sessionID, operation, idempotencyKey, requestHash, encoded, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrStoreUnavailable
	}
	return encoded, nil
}

func lookupMutationTx(ctx context.Context, tx *sql.Tx, projectKey, sessionID, operation, idempotencyKey, requestHash string) ([]byte, bool, error) {
	var storedHash string
	var result []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash, result_json FROM consultation_idempotency
		WHERE project_key = ? AND session_id = ? AND operation = ? AND idempotency_key = ?`, projectKey, sessionID, operation, idempotencyKey).Scan(&storedHash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrStoreUnavailable
	}
	if storedHash != requestHash {
		return nil, false, ErrIdempotencyConflict
	}
	if publicdataValidate(result) != nil {
		return nil, false, ErrStoreUnavailable
	}
	return append([]byte(nil), result...), true, nil
}

func insertMutation(ctx context.Context, tx *sql.Tx, projectKey, sessionID, operation, idempotencyKey, requestHash string, result []byte, created time.Time) error {
	if !validIdempotencyKey(idempotencyKey) || len(requestHash) != 64 || publicdataValidate(result) != nil {
		return ErrInvalidInput
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO consultation_idempotency(project_key, session_id, operation, idempotency_key, request_hash, result_json, created_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, projectKey, sessionID, operation, idempotencyKey, requestHash, result, created.UnixNano()); err != nil {
		return ErrStoreUnavailable
	}
	return nil
}

func validateStoredSession(session Session) error {
	maximumSessionVersion := 1 + 1 + maximumSessionComparisons + maximumProposals + maximumValidationReceipts + maximumCandidateVersions
	if !sessionIDPattern.MatchString(session.ID) || !projectKeyPattern.MatchString(session.ProjectKey) || session.Version < 1 || session.Version > maximumSessionVersion || session.State != SessionOpen ||
		session.Question != session.Evidence.Question || session.Evidence.ProjectKey != session.ProjectKey || len(session.References) != len(session.Evidence.Evidence) ||
		len(session.References) > maximumEvidenceReferences || len(session.Comparisons) > maximumSessionComparisons || len(session.Proposals) > maximumProposals ||
		len(session.Receipts) > maximumValidationReceipts || len(session.Candidates) > maximumCandidateVersions ||
		session.CreatedAt.IsZero() || session.UpdatedAt.Before(session.CreatedAt) {
		return errors.New("consultation session shape is inconsistent")
	}
	if session.ReviewAccepted != !session.ReviewAcceptedAt.IsZero() || session.ReviewAccepted != (session.ReviewAcceptedVersion > 0) {
		return errors.New("consultation review state is inconsistent")
	}
	if err := publicdata.Validate(session); err != nil {
		return err
	}
	selections := make([]appquery.EvidencePacketSelection, 0, len(session.Evidence.Evidence))
	for _, item := range session.Evidence.Evidence {
		if item.SpanEnd <= item.SpanStart || item.SpanEnd-item.SpanStart > appquery.MaximumEvidencePacketSpanBytes {
			return errors.New("consultation evidence span is invalid")
		}
		selections = append(selections, appquery.EvidencePacketSelection{
			RequestID: item.RequestID, RevisionID: item.RevisionID, Side: item.Side,
			Offset: item.SpanStart, Length: int(item.SpanEnd - item.SpanStart),
		})
	}
	if err := validatePacket(appquery.EvidencePacketRequest{ProjectKey: session.ProjectKey, Question: session.Question, Selections: selections}, session.Evidence); err != nil {
		return errors.New("consultation evidence packet is inconsistent")
	}
	seen := make(map[string]struct{}, len(session.References))
	references := referenceMap(session)
	for index, ref := range session.References {
		item := session.Evidence.Evidence[index]
		if !sessionIDPattern.MatchString(ref.ID) || ref.EvidenceIndex != index || ref.RequestID != item.RequestID || ref.RevisionID != item.RevisionID ||
			ref.Side != item.Side || ref.SpanStart != item.SpanStart || ref.SpanEnd != item.SpanEnd || ref.SHA256 != item.SHA256 || ref.PolicyVersion != session.Evidence.PolicyVersion {
			return errors.New("consultation evidence reference is inconsistent")
		}
		if _, exists := seen[ref.ID]; exists {
			return errors.New("consultation evidence reference is duplicated")
		}
		seen[ref.ID] = struct{}{}
	}
	versions := make(map[int]struct{}, session.Version-1)
	addVersion := func(version int) bool {
		if version < 2 || version > session.Version {
			return false
		}
		if _, exists := versions[version]; exists {
			return false
		}
		versions[version] = struct{}{}
		return true
	}
	if session.ReviewAccepted && (!addVersion(session.ReviewAcceptedVersion) || session.ReviewAcceptedAt.Before(session.CreatedAt)) {
		return errors.New("consultation review transition is inconsistent")
	}
	comparisons := comparisonMap(session)
	for _, comparison := range session.Comparisons {
		if !sessionIDPattern.MatchString(comparison.ID) || !addVersion(comparison.SessionVersion) || comparison.CreatedAt.IsZero() ||
			comparison.Result.ProjectKey != session.ProjectKey || !hasAvailableEvidence(session, comparison.Result.LeftRequestID, comparison.Result.LeftRevisionID) ||
			!hasAvailableEvidence(session, comparison.Result.RightRequestID, comparison.Result.RightRevisionID) ||
			!comparisonSideMatches(comparison.Result.Request, appquery.CaptureComparisonRequest{ProjectKey: session.ProjectKey, LeftRequestID: comparison.Result.LeftRequestID, LeftRevisionID: comparison.Result.LeftRevisionID, RightRequestID: comparison.Result.RightRequestID, RightRevisionID: comparison.Result.RightRevisionID}) ||
			!comparisonSideMatches(comparison.Result.Response, appquery.CaptureComparisonRequest{ProjectKey: session.ProjectKey, LeftRequestID: comparison.Result.LeftRequestID, LeftRevisionID: comparison.Result.LeftRevisionID, RightRequestID: comparison.Result.RightRequestID, RightRevisionID: comparison.Result.RightRevisionID}) ||
			comparison.Result.Request.LeftExcerpt != "" ||
			comparison.Result.Request.RightExcerpt != "" || comparison.Result.Response.LeftExcerpt != "" || comparison.Result.Response.RightExcerpt != "" {
			return errors.New("consultation comparison is inconsistent or contains raw excerpts")
		}
	}
	proposals := make(map[string]ProposalRecord, len(session.Proposals))
	for _, proposal := range session.Proposals {
		cleaned, cleanErr := sanitizeProposal(proposal.Proposal)
		digest, digestErr := digestJSON(struct {
			Proposal    CandidateProposal `json:"proposal"`
			EvidenceIDs []string          `json:"evidence_reference_ids"`
			CompareIDs  []string          `json:"comparison_ids"`
			Policy      string            `json:"policy_version"`
		}{proposal.Proposal, proposal.EvidenceReferenceIDs, proposal.ComparisonIDs, session.Evidence.PolicyVersion})
		if !sessionIDPattern.MatchString(proposal.ID) || !addVersion(proposal.SessionVersion) || proposal.CreatedAt.IsZero() ||
			!revisionPattern.MatchString(proposal.Digest) || digestErr != nil || digest != proposal.Digest || cleanErr != nil || !reflect.DeepEqual(cleaned, proposal.Proposal) ||
			len(proposal.EvidenceReferenceIDs) > maximumEvidenceCitations || len(proposal.ComparisonIDs) > maximumComparisonCitations ||
			!validOpaqueIDs(proposal.EvidenceReferenceIDs) || !validOpaqueIDs(proposal.ComparisonIDs) {
			return errors.New("consultation proposal is invalid")
		}
		proposals[proposal.ID] = proposal
	}
	for _, receipt := range session.Receipts {
		proposal, ok := proposals[receipt.ProposalID]
		if !sessionIDPattern.MatchString(receipt.ID) || !addVersion(receipt.SessionVersion) || receipt.ValidatedAt.IsZero() || !ok ||
			receipt.SessionVersion <= proposal.SessionVersion || receipt.ProposalDigest != proposal.Digest || receipt.Status != ValidationReferencesValid ||
			receipt.PolicyVersion != session.Evidence.PolicyVersion || !reflect.DeepEqual(receipt.EvidenceReferenceIDs, proposal.EvidenceReferenceIDs) ||
			!reflect.DeepEqual(receipt.ComparisonIDs, proposal.ComparisonIDs) || len(receipt.EvidenceReferenceIDs) == 0 ||
			len(receipt.EvidenceReferenceIDs) > maximumEvidenceCitations || len(receipt.ComparisonIDs) > maximumComparisonCitations {
			return errors.New("consultation citation receipt is inconsistent")
		}
		for _, id := range receipt.EvidenceReferenceIDs {
			ref, exists := references[id]
			if !exists || ref.SourceIntegrityStatus != "verified" || !revisionPattern.MatchString(ref.SHA256) || ref.PolicyVersion != receipt.PolicyVersion {
				return errors.New("consultation citation receipt has an invalid evidence reference")
			}
		}
		for _, id := range receipt.ComparisonIDs {
			if _, exists := comparisons[id]; !exists {
				return errors.New("consultation citation receipt has an invalid comparison reference")
			}
		}
	}
	candidateVersions := make(map[string]map[int]struct{}, len(session.Candidates))
	for _, candidate := range session.Candidates {
		proposal, proposalOK := proposals[candidate.ProposalID]
		receipt, receiptOK := findReceipt(session, candidate.ReceiptID)
		if !sessionIDPattern.MatchString(candidate.CandidateID) || candidate.Version < 1 || candidate.Version > maximumCandidateVersions ||
			candidate.State != CandidateStateCandidate || !addVersion(candidate.SessionVersion) || candidate.CreatedAt.IsZero() ||
			!proposalOK || !receiptOK || receipt.ProposalID != proposal.ID || receipt.ProposalDigest != proposal.Digest ||
			candidate.SessionVersion <= receipt.SessionVersion || !reflect.DeepEqual(candidate.Proposal, proposal.Proposal) ||
			!reflect.DeepEqual(candidate.ComparisonIDs, receipt.ComparisonIDs) || len(candidate.Citations) != len(receipt.EvidenceReferenceIDs) {
			return errors.New("consultation candidate is inconsistent")
		}
		if candidateVersions[candidate.CandidateID] == nil {
			candidateVersions[candidate.CandidateID] = make(map[int]struct{})
		}
		if _, duplicate := candidateVersions[candidate.CandidateID][candidate.Version]; duplicate {
			return errors.New("consultation candidate version is duplicated")
		}
		candidateVersions[candidate.CandidateID][candidate.Version] = struct{}{}
		for citationIndex, id := range receipt.EvidenceReferenceIDs {
			ref := references[id]
			citation := candidate.Citations[citationIndex]
			if citation.EvidenceReferenceID != ref.ID || citation.RequestID != ref.RequestID || citation.RevisionID != ref.RevisionID ||
				citation.Side != ref.Side || citation.SpanStart != ref.SpanStart || citation.SpanEnd != ref.SpanEnd || citation.SHA256 != ref.SHA256 {
				return errors.New("consultation candidate citation is inconsistent")
			}
		}
	}
	for _, versionsForCandidate := range candidateVersions {
		for version := 1; version <= len(versionsForCandidate); version++ {
			if _, exists := versionsForCandidate[version]; !exists {
				return errors.New("consultation candidate version sequence has a gap")
			}
		}
	}
	if len(versions) != session.Version-1 {
		return errors.New("consultation session version has no matching append-only record")
	}
	return nil
}

func publicdataValidate(encoded []byte) error {
	return publicdata.ValidateJSON(encoded)
}
