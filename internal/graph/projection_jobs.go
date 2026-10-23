package graph

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"bb-graph-assistant/internal/publicdata"
)

const maxProjectionJobPayloadBytes = 4 << 20

var (
	ErrProjectionJobConflict      = errors.New("projection job revision conflicts with its stored payload")
	ErrInvalidProjectionJob       = errors.New("projection job input is invalid")
	ErrProjectionRevisionMissing  = errors.New("projection job revision is not registered")
	ErrProjectionRevisionMismatch = errors.New("projection job revision does not match its capture identity")
	ErrProjectionProjectMismatch  = errors.New("projection project key does not match the selected database")
	ErrProjectionJobNotPending    = errors.New("projection job is not pending acknowledgement")
)

const projectionTimestampLayout = "2006-01-02T15:04:05.000000000Z"

// ProjectionJob is the durable handoff from accepted capture revisions to the
// asynchronous graph projector. PayloadJSON contains a project-bound,
// normalized graph bundle, never a raw HTTP request or response.
type ProjectionJob struct {
	JobID         string
	RevisionID    string
	ObservationID string
	PayloadJSON   string
	PayloadSHA256 string
	State         string
	AttemptCount  int
	NextAttemptAt string
	CreatedAt     string
	UpdatedAt     string
	LastErrorCode sql.NullString
	AppliedAt     sql.NullString
	LeaseToken    sql.NullString
	LeaseUntil    sql.NullString
}

type projectionJobTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// EnqueueProjectionJob inserts an immutable job using the caller's active
// SQLite transaction. The caller can therefore commit the job with its source
// revision and checkpoint. Replaying the same revision and normalized payload
// is idempotent; a different payload for an existing revision fails.
func EnqueueProjectionJob(
	ctx context.Context,
	tx projectionJobTx,
	projectKey string,
	revisionID string,
	observationID string,
	payload []byte,
	createdAt time.Time,
) (ProjectionJob, bool, error) {
	if ctx == nil {
		return ProjectionJob{}, false, ErrInvalidProjectionJob
	}
	if err := ctx.Err(); err != nil {
		return ProjectionJob{}, false, err
	}
	if tx == nil || !graphDigestPattern.MatchString(projectKey) || !validProjectionRevisionID(revisionID) || strings.TrimSpace(observationID) == "" ||
		len(payload) == 0 || len(payload) > maxProjectionJobPayloadBytes || createdAt.IsZero() {
		return ProjectionJob{}, false, ErrInvalidProjectionJob
	}
	if err := validateProjectionProjectKey(ctx, tx, projectKey); err != nil {
		return ProjectionJob{}, false, err
	}
	normalizedPayload, err := validateProjectionBundle(payload, projectKey)
	if err != nil {
		return ProjectionJob{}, false, ErrInvalidProjectionJob
	}
	if _, err := projectionObservationID(normalizedPayload, observationID); err != nil {
		return ProjectionJob{}, false, ErrInvalidProjectionJob
	}
	var registeredObservationID string
	if err := tx.QueryRowContext(ctx,
		"SELECT observation_id FROM capture_revision_registry WHERE revision_id=?", revisionID,
	).Scan(&registeredObservationID); err != nil || registeredObservationID == "" {
		return ProjectionJob{}, false, ErrProjectionRevisionMissing
	}
	if registeredObservationID != observationID {
		return ProjectionJob{}, false, ErrProjectionRevisionMismatch
	}
	jobID := "projection-" + revisionID
	checksum := sha256.Sum256(normalizedPayload)
	checksumHex := hex.EncodeToString(checksum[:])
	now := createdAt.UTC().Format(projectionTimestampLayout)
	result, err := tx.ExecContext(ctx,
		`INSERT INTO projection_jobs
		 (job_id,revision_id,payload_json,payload_sha256,state,attempt_count,next_attempt_at,created_at,updated_at)
		 VALUES(?,?,?,?,'pending',0,?,?,?)
		 ON CONFLICT(revision_id) DO NOTHING`,
		jobID, revisionID, string(normalizedPayload), checksumHex, now, now, now,
	)
	if err != nil {
		return ProjectionJob{}, false, errors.New("projection job could not be added to the active transaction")
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return ProjectionJob{}, false, errors.New("projection job insertion result is unavailable")
	}
	job, err := queryProjectionJobByRevision(ctx, tx, projectKey, revisionID)
	if err != nil {
		return ProjectionJob{}, false, errors.New("projection job could not be verified")
	}
	if job.PayloadSHA256 != checksumHex || job.PayloadJSON != string(normalizedPayload) || job.JobID != jobID {
		return ProjectionJob{}, false, ErrProjectionJobConflict
	}
	return job, inserted == 1, nil
}

// ProjectionJobByRevision returns and revalidates the durable job for one
// source revision. The project key binds its payload to the selected project.
func ProjectionJobByRevision(
	ctx context.Context,
	queryer projectionJobQueryer,
	projectKey string,
	revisionID string,
) (ProjectionJob, error) {
	if ctx == nil || queryer == nil || !graphDigestPattern.MatchString(projectKey) || !validProjectionRevisionID(revisionID) {
		return ProjectionJob{}, ErrInvalidProjectionJob
	}
	if err := validateProjectionProjectKey(ctx, queryer, projectKey); err != nil {
		return ProjectionJob{}, err
	}
	return queryProjectionJobByRevision(ctx, queryer, projectKey, revisionID)
}

// MarkProjectionJobApplied acknowledges a pending job only after its derived
// graph projections have completed. The caller's transaction keeps the state
// transition durable and project-scoped.
func MarkProjectionJobApplied(
	ctx context.Context,
	tx projectionJobTx,
	projectKey string,
	revisionID string,
	appliedAt time.Time,
) error {
	if ctx == nil || tx == nil || !graphDigestPattern.MatchString(projectKey) ||
		!validProjectionRevisionID(revisionID) || appliedAt.IsZero() {
		return ErrInvalidProjectionJob
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateProjectionProjectKey(ctx, tx, projectKey); err != nil {
		return err
	}
	job, err := queryProjectionJobByRevision(ctx, tx, projectKey, revisionID)
	if err != nil {
		return err
	}
	if job.State == "applied" {
		return nil
	}
	if job.State != "pending" {
		return ErrProjectionJobNotPending
	}
	now := appliedAt.UTC().Format(projectionTimestampLayout)
	result, err := tx.ExecContext(ctx,
		`UPDATE projection_jobs SET state='applied',lease_token=NULL,lease_until=NULL,
		 applied_at=?,updated_at=? WHERE revision_id=? AND state='pending'`,
		now, now, revisionID,
	)
	if err != nil {
		return errors.New("projection job acknowledgement could not be stored")
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return ErrProjectionJobNotPending
	}
	return nil
}

// ClaimNextProjectionJob leases the earliest due job inside the caller's
// transaction. Expired leases are returned to the queue or dead-lettered when
// their attempt budget is exhausted. Commit before starting projection I/O.
func ClaimNextProjectionJob(
	ctx context.Context,
	tx projectionJobTx,
	projectKey string,
	now time.Time,
	leaseDuration time.Duration,
	maxAttempts int,
) (ProjectionJob, bool, error) {
	if ctx == nil || tx == nil || !graphDigestPattern.MatchString(projectKey) || now.IsZero() ||
		leaseDuration < time.Second || leaseDuration > 30*time.Minute || maxAttempts < 1 || maxAttempts > 100 {
		return ProjectionJob{}, false, ErrInvalidProjectionJob
	}
	if err := ctx.Err(); err != nil {
		return ProjectionJob{}, false, err
	}
	if err := validateProjectionProjectKey(ctx, tx, projectKey); err != nil {
		return ProjectionJob{}, false, err
	}
	nowText := now.UTC().Format(projectionTimestampLayout)
	if _, err := tx.ExecContext(ctx,
		`UPDATE projection_jobs
		 SET state=CASE WHEN attempt_count>=? THEN 'dead_letter' ELSE 'pending' END,
		  next_attempt_at=CASE WHEN attempt_count>=? THEN next_attempt_at ELSE ? END,
		  last_error_code=CASE WHEN attempt_count>=? THEN 'lease_expired' ELSE last_error_code END,
		  lease_token=NULL,lease_until=NULL,updated_at=?
		 WHERE state='leased' AND lease_until<=?`,
		maxAttempts, maxAttempts, nowText, maxAttempts, nowText, nowText,
	); err != nil {
		return ProjectionJob{}, false, errors.New("expired projection leases could not be recovered")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE projection_jobs SET state='dead_letter',last_error_code=COALESCE(last_error_code,'attempts_exhausted'),updated_at=?
		 WHERE state='pending' AND attempt_count>=?`, nowText, maxAttempts,
	); err != nil {
		return ProjectionJob{}, false, errors.New("exhausted projection jobs could not be closed")
	}
	for rejected := 0; rejected < 100; rejected++ {
		var revisionID string
		err := tx.QueryRowContext(ctx,
			`SELECT revision_id FROM projection_jobs
			 WHERE state='pending' AND next_attempt_at<=? AND attempt_count<?
			 ORDER BY sequence_id LIMIT 1`, nowText, maxAttempts,
		).Scan(&revisionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectionJob{}, false, nil
		}
		if err != nil {
			return ProjectionJob{}, false, errors.New("due projection job could not be selected")
		}
		var tokenBytes [16]byte
		if _, err := rand.Read(tokenBytes[:]); err != nil {
			return ProjectionJob{}, false, errors.New("projection lease token could not be generated")
		}
		leaseToken := hex.EncodeToString(tokenBytes[:])
		leaseUntil := now.Add(leaseDuration).UTC().Format(projectionTimestampLayout)
		result, err := tx.ExecContext(ctx,
			`UPDATE projection_jobs SET state='leased',attempt_count=attempt_count+1,
			 lease_token=?,lease_until=?,updated_at=?
			 WHERE revision_id=? AND state='pending' AND next_attempt_at<=? AND attempt_count<?`,
			leaseToken, leaseUntil, nowText, revisionID, nowText, maxAttempts,
		)
		if err != nil {
			return ProjectionJob{}, false, errors.New("projection job could not be leased")
		}
		updated, err := result.RowsAffected()
		if err != nil || updated != 1 {
			return ProjectionJob{}, false, errors.New("selected projection job was claimed concurrently")
		}
		job, err := queryProjectionJobByRevision(ctx, tx, projectKey, revisionID)
		if err != nil {
			// Quarantine corrupt queue data with a fixed reason. Do not expose its
			// payload or error text, and continue to the next due job.
			result, quarantineErr := tx.ExecContext(ctx,
				`UPDATE projection_jobs SET state='dead_letter',lease_token=NULL,lease_until=NULL,
				 last_error_code='invalid_job',updated_at=?
				 WHERE revision_id=? AND state='leased' AND lease_token=?`, nowText, revisionID, leaseToken,
			)
			if quarantineErr != nil {
				return ProjectionJob{}, false, errors.New("invalid projection job could not be quarantined")
			}
			if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
				return ProjectionJob{}, false, errors.New("invalid projection job quarantine could not be verified")
			}
			var invalidJob ProjectionJob
			if err := tx.QueryRowContext(ctx,
				"SELECT job_id,revision_id,state,attempt_count FROM projection_jobs WHERE revision_id=?", revisionID,
			).Scan(&invalidJob.JobID, &invalidJob.RevisionID, &invalidJob.State, &invalidJob.AttemptCount); err != nil {
				return ProjectionJob{}, false, errors.New("quarantined projection job could not be read")
			}
			invalidJob.LastErrorCode = sql.NullString{String: "invalid_job", Valid: true}
			return invalidJob, true, nil
		}
		if !job.LeaseToken.Valid || job.LeaseToken.String != leaseToken || !job.LeaseUntil.Valid {
			return ProjectionJob{}, false, errors.New("claimed projection job could not be verified")
		}
		return job, true, nil
	}
	return ProjectionJob{}, false, nil
}

// MarkLeasedProjectionJobApplied acknowledges only the lease currently owned
// by the caller. A stale worker cannot acknowledge a reclaimed job.
func MarkLeasedProjectionJobApplied(
	ctx context.Context,
	tx projectionJobTx,
	projectKey string,
	revisionID string,
	leaseToken string,
	payloadSHA256 string,
	appliedAt time.Time,
) error {
	if ctx == nil || tx == nil || !graphDigestPattern.MatchString(projectKey) ||
		!validProjectionRevisionID(revisionID) || !validProjectionLeaseToken(leaseToken) ||
		!validProjectionRevisionID(payloadSHA256) || appliedAt.IsZero() {
		return ErrInvalidProjectionJob
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateProjectionProjectKey(ctx, tx, projectKey); err != nil {
		return err
	}
	job, err := queryProjectionJobByRevision(ctx, tx, projectKey, revisionID)
	if err != nil {
		return err
	}
	if job.State != "leased" || !job.LeaseToken.Valid || job.LeaseToken.String != leaseToken {
		return ErrProjectionJobNotPending
	}
	if job.PayloadSHA256 != payloadSHA256 {
		return ErrProjectionJobConflict
	}
	now := appliedAt.UTC().Format(projectionTimestampLayout)
	result, err := tx.ExecContext(ctx,
		`UPDATE projection_jobs SET state='applied',lease_token=NULL,lease_until=NULL,
		 applied_at=?,updated_at=? WHERE revision_id=? AND state='leased' AND lease_token=? AND payload_sha256=?`,
		now, now, revisionID, leaseToken, payloadSHA256,
	)
	if err != nil {
		return errors.New("leased projection job acknowledgement could not be stored")
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return ErrProjectionJobNotPending
	}
	return nil
}

// FailLeasedProjectionJob releases a failed delivery for retry or moves it to
// dead_letter after maxAttempts. Only the worker presenting the current lease
// token can change the job.
func FailLeasedProjectionJob(
	ctx context.Context,
	tx projectionJobTx,
	projectKey string,
	revisionID string,
	leaseToken string,
	payloadSHA256 string,
	errorCode string,
	now time.Time,
	nextAttemptAt time.Time,
	maxAttempts int,
) error {
	if ctx == nil || tx == nil || !graphDigestPattern.MatchString(projectKey) || !validProjectionRevisionID(revisionID) ||
		!validProjectionLeaseToken(leaseToken) || !validProjectionRevisionID(payloadSHA256) || !projectionErrorCodePattern.MatchString(errorCode) ||
		now.IsZero() || nextAttemptAt.Before(now) || maxAttempts < 1 || maxAttempts > 100 {
		return ErrInvalidProjectionJob
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateProjectionProjectKey(ctx, tx, projectKey); err != nil {
		return err
	}
	job, err := queryProjectionJobByRevision(ctx, tx, projectKey, revisionID)
	if err != nil {
		return err
	}
	if job.State != "leased" || !job.LeaseToken.Valid || job.LeaseToken.String != leaseToken {
		return ErrProjectionJobNotPending
	}
	if job.PayloadSHA256 != payloadSHA256 {
		return ErrProjectionJobConflict
	}
	state := "pending"
	if job.AttemptCount >= maxAttempts {
		state = "dead_letter"
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE projection_jobs SET state=?,next_attempt_at=?,lease_token=NULL,lease_until=NULL,
		 last_error_code=?,updated_at=? WHERE revision_id=? AND state='leased' AND lease_token=? AND payload_sha256=?`,
		state, nextAttemptAt.UTC().Format(projectionTimestampLayout), errorCode,
		now.UTC().Format(projectionTimestampLayout), revisionID, leaseToken, payloadSHA256,
	)
	if err != nil {
		return errors.New("failed projection job could not be rescheduled")
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrProjectionJobNotPending
	}
	return nil
}

var projectionErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validProjectionLeaseToken(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

type projectionJobQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateProjectionProjectKey(ctx context.Context, queryer projectionJobQueryer, projectKey string) error {
	var registeredProjectKey string
	err := queryer.QueryRowContext(ctx,
		"SELECT value FROM project_meta WHERE key='project_key'",
	).Scan(&registeredProjectKey)
	if err != nil {
		return ErrProjectionProjectMismatch
	}
	if registeredProjectKey != projectKey {
		return ErrProjectionProjectMismatch
	}
	return nil
}

func queryProjectionJobByRevision(
	ctx context.Context,
	queryer projectionJobQueryer,
	projectKey string,
	revisionID string,
) (ProjectionJob, error) {
	var job ProjectionJob
	err := queryer.QueryRowContext(ctx,
		`SELECT j.job_id,j.revision_id,j.payload_json,j.payload_sha256,j.state,j.attempt_count,
		 j.next_attempt_at,j.created_at,j.updated_at,j.last_error_code,j.applied_at,
		 j.lease_token,j.lease_until,r.observation_id
		 FROM projection_jobs j JOIN capture_revision_registry r ON r.revision_id=j.revision_id
		 WHERE j.revision_id=?`, revisionID,
	).Scan(
		&job.JobID, &job.RevisionID, &job.PayloadJSON, &job.PayloadSHA256, &job.State,
		&job.AttemptCount, &job.NextAttemptAt, &job.CreatedAt, &job.UpdatedAt,
		&job.LastErrorCode, &job.AppliedAt, &job.LeaseToken, &job.LeaseUntil, &job.ObservationID,
	)
	if err != nil {
		return ProjectionJob{}, err
	}
	if err := validateStoredProjectionJob(job, projectKey, job.ObservationID); err != nil {
		return ProjectionJob{}, err
	}
	return job, nil
}

func validateStoredProjectionJob(job ProjectionJob, projectKey, observationID string) error {
	if !validProjectionRevisionID(job.RevisionID) || job.JobID != "projection-"+job.RevisionID ||
		len(job.PayloadJSON) == 0 || len(job.PayloadJSON) > maxProjectionJobPayloadBytes {
		return errors.New("stored projection job identity or payload is invalid")
	}
	checksum := sha256.Sum256([]byte(job.PayloadJSON))
	if job.PayloadSHA256 != hex.EncodeToString(checksum[:]) {
		return errors.New("stored projection job checksum does not match its payload")
	}
	normalized, err := validateProjectionBundle([]byte(job.PayloadJSON), projectKey)
	if err != nil || !bytes.Equal(normalized, []byte(job.PayloadJSON)) {
		return errors.New("stored projection job payload is invalid")
	}
	if _, err := projectionObservationID(normalized, observationID); err != nil {
		return errors.New("stored projection job does not match its capture revision")
	}
	switch job.State {
	case "pending", "leased", "applied", "dead_letter":
	default:
		return errors.New("stored projection job state is invalid")
	}
	if job.State == "leased" {
		if !job.LeaseToken.Valid || !validProjectionLeaseToken(job.LeaseToken.String) || !job.LeaseUntil.Valid {
			return errors.New("stored projection job lease is invalid")
		}
	} else if job.LeaseToken.Valid || job.LeaseUntil.Valid {
		return errors.New("stored non-leased projection job has lease metadata")
	}
	return nil
}

// ProjectionObservationID validates a project-bound graph bundle and returns
// the one observation entity whose capture_id matches the accepted capture.
// A bundle for a single capture revision must contain exactly one observation.
func ProjectionObservationID(payload []byte, projectKey, captureID string) (string, error) {
	if strings.TrimSpace(captureID) == "" {
		return "", ErrInvalidProjectionJob
	}
	normalized, err := validateProjectionBundle(payload, projectKey)
	if err != nil {
		return "", ErrInvalidProjectionJob
	}
	return projectionObservationID(normalized, captureID)
}

func projectionObservationID(normalizedPayload []byte, captureID string) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(normalizedPayload))
	decoder.UseNumber()
	var bundle map[string]any
	if err := decoder.Decode(&bundle); err != nil || bundle == nil {
		return "", ErrInvalidProjectionJob
	}
	entities, err := recordList(bundle["entities"], maxWriterRecords)
	if err != nil {
		return "", ErrInvalidProjectionJob
	}
	var graphObservationID string
	observationCount := 0
	for _, entity := range entities {
		if entity["kind"] != "observation" {
			continue
		}
		observationCount++
		data, ok := entity["data"].(map[string]any)
		id, okID := entity["id"].(string)
		entityCaptureID, okCaptureID := data["capture_id"].(string)
		if !ok || !okID || !okCaptureID || entityCaptureID != captureID || !graphIDPattern.MatchString(id) {
			return "", ErrInvalidProjectionJob
		}
		graphObservationID = id
	}
	if observationCount != 1 || graphObservationID == "" {
		return "", ErrInvalidProjectionJob
	}
	return graphObservationID, nil
}

func validateProjectionBundle(payload []byte, projectKey string) ([]byte, error) {
	if err := publicdata.ValidateJSON(payload); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var bundle Record
	if err := decoder.Decode(&bundle); err != nil || bundle == nil {
		return nil, errors.New("projection bundle is not a JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("projection bundle has trailing content")
	}
	normalized, err := (&Writer{program: projectKey}).normalizeBundle(bundle)
	if err != nil {
		return nil, err
	}
	encoded, err := canonicalJSON(normalized)
	if err != nil || len(encoded) == 0 || len(encoded) > maxProjectionJobPayloadBytes {
		return nil, errors.New("normalized projection bundle exceeds the size limit")
	}
	return encoded, nil
}

func validProjectionRevisionID(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
