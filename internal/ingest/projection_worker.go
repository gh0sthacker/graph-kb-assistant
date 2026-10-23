package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

const (
	projectionWorkerTimestampLayout = "2006-01-02T15:04:05.000000000Z"
	defaultProjectionLeaseDuration  = 10 * time.Minute
	defaultProjectionMaxAttempts    = 8
	baseProjectionRetryDelay        = 5 * time.Second
	maxProjectionRetryDelay         = 15 * time.Minute
)

type ProjectionWorkerOptions struct {
	LeaseDuration time.Duration
	MaxAttempts   int
}

type ProjectionWorkerResult struct {
	ProjectKey   string `json:"project_key"`
	State        string `json:"state"`
	JobID        string `json:"job_id,omitempty"`
	RevisionID   string `json:"revision_id,omitempty"`
	AttemptCount int    `json:"attempt_count,omitempty"`
	NextAttempt  string `json:"next_attempt_at,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
}

// RunProjectionWorkerOnce claims and processes at most one durable projection
// job. The caller must hold the selected project's exclusive maintenance
// lease for the complete operation, including graph and Neo4j writes.
func RunProjectionWorkerOnce(
	ctx context.Context,
	selected *store.ProjectStore,
	maintenance *projectlock.Lock,
	options ProjectionWorkerOptions,
) (ProjectionWorkerResult, error) {
	if ctx == nil || selected == nil || maintenance == nil || !maintenance.CoversExclusiveDirectory(selected.Directory) {
		return ProjectionWorkerResult{}, errors.New("projection worker requires the matching project maintenance lease")
	}
	if err := validateSelectedProject(selected); err != nil {
		return ProjectionWorkerResult{}, err
	}
	if graphaccess.ConfiguredBackend() != "neo4j" {
		return ProjectionWorkerResult{ProjectKey: selected.ProjectKey, State: "backend_disabled"}, nil
	}
	return runProjectionWorkerOnce(ctx, selected, maintenance, options, projectGraphBundles, time.Now().UTC())
}

func runProjectionWorkerOnce(
	ctx context.Context,
	selected *store.ProjectStore,
	maintenance *projectlock.Lock,
	options ProjectionWorkerOptions,
	projector projectionExecutor,
	now time.Time,
) (ProjectionWorkerResult, error) {
	if ctx == nil || projector == nil || now.IsZero() || selected == nil ||
		maintenance == nil || !maintenance.CoversExclusiveDirectory(selected.Directory) {
		return ProjectionWorkerResult{}, errors.New("projection worker requires the matching project maintenance lease")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = defaultProjectionLeaseDuration
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = defaultProjectionMaxAttempts
	}
	if options.LeaseDuration <= time.Second || options.LeaseDuration > 30*time.Minute ||
		options.MaxAttempts < 1 || options.MaxAttempts > 100 {
		return ProjectionWorkerResult{}, errors.New("projection worker limits are invalid")
	}
	if err := validateSelectedProject(selected); err != nil {
		return ProjectionWorkerResult{}, err
	}
	result := ProjectionWorkerResult{ProjectKey: selected.ProjectKey}

	claimConn, err := beginSelectedImmediate(ctx, selected)
	if err != nil {
		return ProjectionWorkerResult{}, err
	}
	job, found, claimErr := behaviorgraph.ClaimNextProjectionJob(
		ctx, claimConn, selected.ProjectKey, now, options.LeaseDuration, options.MaxAttempts,
	)
	if claimErr != nil {
		rollbackProjectionConn(claimConn)
		return ProjectionWorkerResult{}, claimErr
	}
	if err := commitImmediate(ctx, claimConn); err != nil {
		rollbackProjectionConn(claimConn)
		return ProjectionWorkerResult{}, err
	}
	_ = claimConn.Close()
	if !found {
		result.State = "idle"
		return result, nil
	}
	result.JobID = job.JobID
	result.RevisionID = job.RevisionID
	result.AttemptCount = job.AttemptCount
	if job.State == "dead_letter" {
		result.State = "dead_letter"
		if job.LastErrorCode.Valid {
			result.ErrorCode = job.LastErrorCode.String
		}
		return result, nil
	}
	deliveryCtx, cancelDelivery := context.WithTimeout(ctx, options.LeaseDuration-time.Second)
	defer cancelDelivery()

	var bundle behaviorgraph.Record
	if err := json.Unmarshal([]byte(job.PayloadJSON), &bundle); err != nil || bundle == nil {
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "invalid_job", result)
	}
	if _, err := behaviorgraph.ProjectionObservationID([]byte(job.PayloadJSON), selected.ProjectKey, job.ObservationID); err != nil {
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "invalid_job", result)
	}
	materialized, neo4jReady, projectionErr := projector(deliveryCtx, selected, []behaviorgraph.Record{bundle})
	if err := deliveryCtx.Err(); err != nil {
		return result, err
	}
	if projectionErr != nil || !materialized || !neo4jReady {
		code := "projection_unavailable"
		if materialized && !neo4jReady {
			code = "neo4j_unavailable"
		}
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, code, result)
	}

	ackConn, err := beginSelectedImmediate(deliveryCtx, selected)
	if err != nil {
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "mapping_unavailable", result)
	}
	committed := false
	ackClosed := false
	releaseAckConn := func() {
		if ackClosed {
			return
		}
		if !committed {
			_, _ = ackConn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = ackConn.Close()
		ackClosed = true
	}
	defer func() {
		releaseAckConn()
	}()
	current, err := behaviorgraph.ProjectionJobByRevision(deliveryCtx, ackConn, selected.ProjectKey, job.RevisionID)
	if err != nil || !sameProjectionLease(current, job) {
		releaseAckConn()
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "projection_fence_mismatch", result)
	}
	graphObservationID, err := behaviorgraph.ProjectionObservationID([]byte(current.PayloadJSON), selected.ProjectKey, current.ObservationID)
	if err != nil {
		releaseAckConn()
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "invalid_job", result)
	}
	appliedAt := time.Now().UTC()
	if appliedAt.Before(now) {
		appliedAt = now
	}
	if err := persistProjectionJobMapping(deliveryCtx, ackConn, current, graphObservationID, appliedAt); err != nil {
		releaseAckConn()
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "mapping_invalid", result)
	}
	if err := behaviorgraph.MarkLeasedProjectionJobApplied(
		deliveryCtx, ackConn, selected.ProjectKey, current.RevisionID, job.LeaseToken.String, job.PayloadSHA256, appliedAt,
	); err != nil {
		releaseAckConn()
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "projection_fence_mismatch", result)
	}
	if err := commitImmediate(deliveryCtx, ackConn); err != nil {
		releaseAckConn()
		return failProjectionDelivery(deliveryCtx, selected, job, options.MaxAttempts, now, "ack_unavailable", result)
	}
	committed = true
	result.State = "applied"
	return result, nil
}

func sameProjectionLease(current, claimed behaviorgraph.ProjectionJob) bool {
	return current.State == "leased" && current.LeaseToken.Valid && claimed.LeaseToken.Valid &&
		current.LeaseToken.String == claimed.LeaseToken.String &&
		current.PayloadSHA256 == claimed.PayloadSHA256 && current.PayloadJSON == claimed.PayloadJSON &&
		current.ObservationID == claimed.ObservationID
}

func persistProjectionJobMapping(
	ctx context.Context,
	conn *sql.Conn,
	job behaviorgraph.ProjectionJob,
	graphObservationID string,
	now time.Time,
) error {
	var observationID, sourceKind string
	if err := conn.QueryRowContext(ctx,
		"SELECT observation_id,source_kind FROM capture_revision_registry WHERE revision_id=?", job.RevisionID,
	).Scan(&observationID, &sourceKind); err != nil || observationID != job.ObservationID {
		return errors.New("projection source revision identity is unavailable")
	}
	if sourceKind == "live" {
		var storedGraphObservationID string
		if err := conn.QueryRowContext(ctx,
			"SELECT graph_observation_id FROM live_capture_index WHERE observation_id=?", observationID,
		).Scan(&storedGraphObservationID); err != nil || storedGraphObservationID != graphObservationID {
			return errors.New("live projection mapping does not match the accepted job")
		}
		return nil
	}
	if sourceKind != "historical" {
		return errors.New("projection source kind is invalid")
	}
	var currentRevision sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT c.current_revision_id FROM captures c
		 JOIN capture_revisions r ON r.observation_id=c.observation_id
		 WHERE c.observation_id=? AND r.revision_id=?`, observationID, job.RevisionID,
	).Scan(&currentRevision); err != nil {
		return errors.New("historical capture revision is unavailable")
	}
	updatedAt := now.UTC().Format(projectionWorkerTimestampLayout)
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO graph_capture_revisions(revision_id,graph_observation_id,updated_at) VALUES(?,?,?)
		 ON CONFLICT(revision_id) DO NOTHING`, job.RevisionID, graphObservationID, updatedAt,
	); err != nil {
		return errors.New("revision graph mapping could not be stored")
	}
	var storedRevisionGraphID string
	if err := conn.QueryRowContext(ctx,
		"SELECT graph_observation_id FROM graph_capture_revisions WHERE revision_id=?", job.RevisionID,
	).Scan(&storedRevisionGraphID); err != nil || storedRevisionGraphID != graphObservationID {
		return errors.New("revision graph mapping conflicts with the accepted job")
	}
	if currentRevision.Valid && currentRevision.String == job.RevisionID {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO graph_capture_map(observation_id,revision_id,graph_observation_id,updated_at)
			 VALUES(?,?,?,?) ON CONFLICT(observation_id) DO UPDATE SET revision_id=excluded.revision_id,
			 graph_observation_id=excluded.graph_observation_id,updated_at=excluded.updated_at`,
			observationID, job.RevisionID, graphObservationID, updatedAt,
		); err != nil {
			return errors.New("current graph mapping could not be stored")
		}
	}
	return nil
}

func failProjectionDelivery(
	ctx context.Context,
	selected *store.ProjectStore,
	job behaviorgraph.ProjectionJob,
	maxAttempts int,
	now time.Time,
	errorCode string,
	result ProjectionWorkerResult,
) (ProjectionWorkerResult, error) {
	failedAt := time.Now().UTC()
	if failedAt.Before(now) {
		failedAt = now
	}
	nextAttempt := failedAt.Add(projectionRetryDelay(job.RevisionID, job.AttemptCount))
	conn, err := beginSelectedImmediate(ctx, selected)
	if err != nil {
		return result, errors.New("projection retry state could not be opened; lease will expire for recovery")
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	if err := behaviorgraph.FailLeasedProjectionJob(
		ctx, conn, selected.ProjectKey, job.RevisionID, job.LeaseToken.String,
		job.PayloadSHA256, errorCode, failedAt, nextAttempt, maxAttempts,
	); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return result, errors.New("projection retry state could not be recorded; lease will expire for recovery")
	}
	if err := commitImmediate(ctx, conn); err != nil {
		return result, errors.New("projection retry state could not be committed; lease will expire for recovery")
	}
	committed = true
	result.State = "pending"
	if job.AttemptCount >= maxAttempts {
		result.State = "dead_letter"
	}
	result.NextAttempt = nextAttempt.UTC().Format(projectionWorkerTimestampLayout)
	result.ErrorCode = errorCode
	return result, nil
}

func projectionRetryDelay(revisionID string, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := attempt - 1
	if exponent > 12 {
		exponent = 12
	}
	delay := baseProjectionRetryDelay * time.Duration(1<<exponent)
	if delay > maxProjectionRetryDelay {
		delay = maxProjectionRetryDelay
	}
	digest := sha256.Sum256([]byte(revisionID + "\x00" + strconv.Itoa(attempt)))
	spread := delay / 5
	if spread == 0 {
		return delay
	}
	window := uint64(2*spread + 1)
	shift := time.Duration(binary.BigEndian.Uint64(digest[:8])%window) - spread
	delay += shift
	if delay > maxProjectionRetryDelay {
		return maxProjectionRetryDelay
	}
	return delay
}

func rollbackProjectionConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Close()
}
