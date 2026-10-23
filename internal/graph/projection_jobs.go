package graph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	PayloadJSON   string
	PayloadSHA256 string
	State         string
	AttemptCount  int
	NextAttemptAt string
	CreatedAt     string
	UpdatedAt     string
	LastErrorCode sql.NullString
	AppliedAt     sql.NullString
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
	var observationID string
	err := queryer.QueryRowContext(ctx,
		`SELECT j.job_id,j.revision_id,j.payload_json,j.payload_sha256,j.state,j.attempt_count,
		 j.next_attempt_at,j.created_at,j.updated_at,j.last_error_code,j.applied_at,r.observation_id
		 FROM projection_jobs j JOIN capture_revision_registry r ON r.revision_id=j.revision_id
		 WHERE j.revision_id=?`, revisionID,
	).Scan(
		&job.JobID, &job.RevisionID, &job.PayloadJSON, &job.PayloadSHA256, &job.State,
		&job.AttemptCount, &job.NextAttemptAt, &job.CreatedAt, &job.UpdatedAt,
		&job.LastErrorCode, &job.AppliedAt, &observationID,
	)
	if err != nil {
		return ProjectionJob{}, err
	}
	if err := validateStoredProjectionJob(job, projectKey, observationID); err != nil {
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
