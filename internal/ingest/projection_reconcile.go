package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

// DefaultProjectionReconcileJobs bounds the number of queue rows scanned by
// one reconciliation command.
const DefaultProjectionReconcileJobs = 5000

// DefaultProjectionReconcileBytes bounds normalized payload bytes scanned by
// one reconciliation command.
const DefaultProjectionReconcileBytes int64 = 512 << 20

const (
	maxProjectionReconcileJobs  = 50000
	maxProjectionReconcileBytes = int64(1 << 30)
	maxProjectionPayloadBytes   = int64(4 << 20)
	projectionReconcileAckBatch = 100
)

type ProjectionReconcileOptions struct {
	MaxJobs           int
	MaxBytes          int64
	AfterSequenceID   int64
	IncludeDeadLetter bool
	Apply             bool
}

type ProjectionReconcileResult struct {
	ProjectKey          string `json:"project_key"`
	State               string `json:"state"`
	LocalSnapshotSHA256 string `json:"local_snapshot_sha256"`
	Neo4jSnapshotSHA256 string `json:"neo4j_snapshot_sha256"`
	Scanned             int    `json:"scanned"`
	ScannedPayloadBytes int64  `json:"scanned_payload_bytes"`
	ExactMatches        int    `json:"exact_matches"`
	MissingRecords      int    `json:"missing_records"`
	ConflictingRecords  int    `json:"conflicting_records"`
	InvalidJobs         int    `json:"invalid_jobs"`
	Applied             int    `json:"applied"`
	Pending             int64  `json:"pending"`
	DeadLetter          int64  `json:"dead_letter"`
	DeadLetterIncluded  bool   `json:"dead_letter_included"`
	NextSequenceID      int64  `json:"next_sequence_id,omitempty"`
	Truncated           bool   `json:"truncated"`
	ApplyRequested      bool   `json:"apply_requested"`
}

type projectionReconcileRef struct {
	SequenceID   int64
	RevisionID   string
	PayloadSHA   string
	PayloadBytes int64
	State        string
}

type projectionReconcileCandidate struct {
	SequenceID int64
	RevisionID string
	PayloadSHA string
	GraphID    string
	State      string
}

// RunProjectionReconciliation acknowledges only pending projection jobs whose
// exact normalized records are already present in a local snapshot that has
// byte-for-byte parity with the selected Neo4j project. It never contacts
// Caido or reads capture bodies.
func RunProjectionReconciliation(
	ctx context.Context,
	selected *store.ProjectStore,
	maintenance *projectlock.Lock,
	localSnapshot behaviorgraph.Record,
	neo4jSnapshot behaviorgraph.Record,
	options ProjectionReconcileOptions,
) (ProjectionReconcileResult, error) {
	if ctx == nil || selected == nil || maintenance == nil ||
		!maintenance.CoversExclusiveDirectory(selected.Directory) {
		return ProjectionReconcileResult{}, errors.New("projection reconciliation requires the matching project maintenance lease")
	}
	validateProject := validateSelectedProjectReadOnly
	if options.Apply {
		validateProject = validateSelectedProject
	}
	if err := validateProject(selected); err != nil {
		return ProjectionReconcileResult{}, err
	}
	if options.MaxJobs == 0 {
		options.MaxJobs = DefaultProjectionReconcileJobs
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultProjectionReconcileBytes
	}
	if options.AfterSequenceID < 0 || options.MaxJobs < 1 || options.MaxJobs > maxProjectionReconcileJobs ||
		options.MaxBytes < maxProjectionPayloadBytes || options.MaxBytes > maxProjectionReconcileBytes {
		return ProjectionReconcileResult{}, errors.New("projection reconciliation limits are invalid")
	}
	result := ProjectionReconcileResult{
		ProjectKey: selected.ProjectKey, State: "partial", ApplyRequested: options.Apply,
		DeadLetterIncluded: options.IncludeDeadLetter,
	}
	if localSnapshot["program"] != selected.ProjectKey || neo4jSnapshot["program"] != selected.ProjectKey {
		result.State = "parity_mismatch"
		pending, countErr := countPendingProjectionJobs(ctx, selected.Database.DB())
		if countErr != nil {
			return result, countErr
		}
		result.Pending = pending
		result.DeadLetter, countErr = countDeadLetterProjectionJobs(ctx, selected.Database.DB())
		if countErr != nil {
			return result, countErr
		}
		return result, nil
	}
	localDigest, err := behaviorgraph.SnapshotDigest(localSnapshot)
	if err != nil {
		return result, errors.New("local projection snapshot is invalid")
	}
	result.LocalSnapshotSHA256 = localDigest
	neo4jDigest, err := behaviorgraph.SnapshotDigest(neo4jSnapshot)
	if err != nil {
		return result, errors.New("Neo4j projection snapshot is invalid")
	}
	result.Neo4jSnapshotSHA256 = neo4jDigest
	if localDigest != neo4jDigest {
		result.State = "parity_mismatch"
		pending, countErr := countPendingProjectionJobs(ctx, selected.Database.DB())
		if countErr != nil {
			return result, countErr
		}
		result.Pending = pending
		result.DeadLetter, countErr = countDeadLetterProjectionJobs(ctx, selected.Database.DB())
		if countErr != nil {
			return result, countErr
		}
		return result, nil
	}
	localRecords, err := indexProjectionSnapshot(localSnapshot)
	if err != nil {
		return result, err
	}
	refs, truncated, err := listProjectionReconcileRefs(
		ctx, selected.Database.DB(), options.AfterSequenceID, options.MaxJobs, options.MaxBytes, options.IncludeDeadLetter,
	)
	if err != nil {
		return result, err
	}
	result.Truncated = truncated
	candidates := make([]projectionReconcileCandidate, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if ref.SequenceID < 1 || ref.PayloadBytes < 0 || ref.PayloadBytes > maxProjectionPayloadBytes {
			result.InvalidJobs++
			result.Scanned++
			result.NextSequenceID = ref.SequenceID
			continue
		}
		job, err := behaviorgraph.ProjectionJobByRevision(ctx, selected.Database.DB(), selected.ProjectKey, ref.RevisionID)
		if err != nil || job.State != ref.State || (job.State != "pending" && (!options.IncludeDeadLetter || job.State != "dead_letter")) || job.PayloadSHA256 != ref.PayloadSHA {
			result.InvalidJobs++
			result.Scanned++
			result.ScannedPayloadBytes += ref.PayloadBytes
			continue
		}
		var bundle behaviorgraph.Record
		if err := json.Unmarshal([]byte(job.PayloadJSON), &bundle); err != nil {
			result.InvalidJobs++
			result.Scanned++
			result.ScannedPayloadBytes += ref.PayloadBytes
			continue
		}
		observationID, err := behaviorgraph.ProjectionObservationID([]byte(job.PayloadJSON), selected.ProjectKey, job.ObservationID)
		if err != nil {
			result.InvalidJobs++
			result.Scanned++
			result.ScannedPayloadBytes += ref.PayloadBytes
			continue
		}
		match, reason := projectionBundleMatchesSnapshot(localRecords, bundle)
		result.Scanned++
		result.ScannedPayloadBytes += ref.PayloadBytes
		switch reason {
		case "exact":
			result.ExactMatches++
			candidates = append(candidates, projectionReconcileCandidate{
				SequenceID: ref.SequenceID, RevisionID: job.RevisionID,
				PayloadSHA: job.PayloadSHA256, GraphID: observationID, State: job.State,
			})
		case "missing_records":
			result.MissingRecords++
		case "conflicting_records":
			result.ConflictingRecords++
		default:
			if !match {
				result.InvalidJobs++
			}
		}
	}
	if truncated && len(refs) > 0 {
		result.NextSequenceID = refs[len(refs)-1].SequenceID
	} else {
		result.NextSequenceID = 0
	}
	if options.Apply && len(candidates) > 0 {
		applied, err := applyReconciledProjectionJobs(ctx, selected, candidates)
		result.Applied = applied
		if err != nil {
			result.State = "partial"
			result.Truncated = true
			result.NextSequenceID = options.AfterSequenceID
			if applied > 0 {
				result.NextSequenceID = candidates[applied-1].SequenceID
			}
			result.Pending, _ = countPendingProjectionJobs(ctx, selected.Database.DB())
			return result, err
		}
	}
	result.Pending, err = countPendingProjectionJobs(ctx, selected.Database.DB())
	if err != nil {
		return result, err
	}
	result.DeadLetter, err = countDeadLetterProjectionJobs(ctx, selected.Database.DB())
	if err != nil {
		return result, err
	}
	if !truncated && result.Pending == 0 && result.DeadLetter == 0 {
		if options.Apply && result.Applied > 0 {
			result.State = "reconciled"
		} else {
			result.State = "verified"
		}
	}
	return result, nil
}

func indexProjectionSnapshot(snapshot behaviorgraph.Record) (map[string]map[string][]byte, error) {
	indexed := make(map[string]map[string][]byte, 4)
	for _, table := range []string{"sources", "entities", "assertions", "cards"} {
		records, ok := projectionRecords(snapshot[table])
		if !ok {
			return nil, errors.New("projection snapshot record list is invalid")
		}
		byID := make(map[string][]byte, len(records))
		for _, record := range records {
			encoded, err := json.Marshal(record)
			if err != nil {
				return nil, errors.New("projection snapshot record cannot be encoded")
			}
			id, err := projectionReconcileRecordKey(table, record)
			if err != nil {
				return nil, err
			}
			if current, exists := byID[id]; exists && !bytes.Equal(current, encoded) {
				return nil, errors.New("projection snapshot has conflicting duplicate records")
			}
			byID[id] = encoded
		}
		indexed[table] = byID
	}
	return indexed, nil
}

func projectionBundleMatchesSnapshot(
	snapshot map[string]map[string][]byte,
	bundle behaviorgraph.Record,
) (bool, string) {
	missing := false
	conflicting := false
	for _, table := range []string{"sources", "entities", "assertions", "cards"} {
		records, ok := projectionRecords(bundle[table])
		if !ok {
			return false, "invalid_bundle"
		}
		for _, record := range records {
			encoded, err := json.Marshal(record)
			if err != nil {
				return false, "invalid_bundle"
			}
			id, err := projectionReconcileRecordKey(table, record)
			if err != nil {
				return false, "invalid_bundle"
			}
			current, exists := snapshot[table][id]
			if !exists {
				missing = true
				continue
			}
			if !bytes.Equal(current, encoded) {
				conflicting = true
			}
		}
	}
	if conflicting {
		return false, "conflicting_records"
	}
	if missing {
		return false, "missing_records"
	}
	return true, "exact"
}

func projectionReconcileRecordKey(table string, record behaviorgraph.Record) (string, error) {
	if table == "sources" {
		path, pathOK := record["path"].(string)
		digest, digestOK := record["sha256"].(string)
		if !pathOK || path == "" || !digestOK || digest == "" {
			return "", errors.New("projection source identity is invalid")
		}
		return path + "\x00" + digest + "\x00" + fmt.Sprint(record["record_locator"]), nil
	}
	id, ok := record["id"].(string)
	if !ok || id == "" {
		return "", errors.New("projection record identity is invalid")
	}
	return id, nil
}

func listProjectionReconcileRefs(
	ctx context.Context,
	db *sql.DB,
	afterSequenceID int64,
	maxJobs int,
	maxBytes int64,
	includeDeadLetter bool,
) ([]projectionReconcileRef, bool, error) {
	if ctx == nil || db == nil || afterSequenceID < 0 || maxJobs < 1 || maxJobs > maxProjectionReconcileJobs ||
		maxBytes < maxProjectionPayloadBytes || maxBytes > maxProjectionReconcileBytes {
		return nil, false, errors.New("projection reconciliation query is invalid")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT sequence_id,revision_id,payload_sha256,length(payload_json),state
		 FROM projection_jobs
		 WHERE sequence_id>? AND (state='pending' OR (?=1 AND state='dead_letter'))
		 ORDER BY sequence_id LIMIT ?`, afterSequenceID, includeDeadLetter, maxJobs+1,
	)
	if err != nil {
		return nil, false, errors.New("projection jobs could not be listed")
	}
	defer rows.Close()
	refs := make([]projectionReconcileRef, 0, min(maxJobs, 256))
	var payloadBytes int64
	truncated := false
	for rows.Next() {
		var ref projectionReconcileRef
		if err := rows.Scan(&ref.SequenceID, &ref.RevisionID, &ref.PayloadSHA, &ref.PayloadBytes, &ref.State); err != nil {
			return nil, false, errors.New("projection job metadata is invalid")
		}
		if len(refs) >= maxJobs {
			truncated = true
			break
		}
		if ref.PayloadBytes >= 0 && ref.PayloadBytes <= maxProjectionPayloadBytes && payloadBytes+ref.PayloadBytes > maxBytes {
			truncated = true
			break
		}
		refs = append(refs, ref)
		if ref.PayloadBytes > 0 && ref.PayloadBytes <= maxProjectionPayloadBytes {
			payloadBytes += ref.PayloadBytes
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.New("pending projection job metadata could not be read")
	}
	if err := rows.Close(); err != nil {
		return nil, false, errors.New("pending projection job metadata cursor could not be closed")
	}
	return refs, truncated, nil
}

func applyReconciledProjectionJobs(
	ctx context.Context,
	selected *store.ProjectStore,
	candidates []projectionReconcileCandidate,
) (int, error) {
	applied := 0
	for start := 0; start < len(candidates); start += projectionReconcileAckBatch {
		end := min(start+projectionReconcileAckBatch, len(candidates))
		conn, err := beginSelectedImmediate(ctx, selected)
		if err != nil {
			return applied, errors.New("projection reconciliation transaction could not begin")
		}
		for _, candidate := range candidates[start:end] {
			job, err := behaviorgraph.ProjectionJobByRevision(ctx, conn, selected.ProjectKey, candidate.RevisionID)
			if err != nil || job.State != candidate.State || job.PayloadSHA256 != candidate.PayloadSHA {
				rollbackProjectionConn(conn)
				return applied, errors.New("projection reconciliation candidate changed before acknowledgment")
			}
			observationID, err := behaviorgraph.ProjectionObservationID([]byte(job.PayloadJSON), selected.ProjectKey, job.ObservationID)
			if err != nil || observationID != candidate.GraphID {
				rollbackProjectionConn(conn)
				return applied, errors.New("projection reconciliation observation identity changed")
			}
			now := time.Now().UTC()
			if err := persistProjectionJobMapping(ctx, conn, job, observationID, now); err != nil {
				rollbackProjectionConn(conn)
				return applied, errors.New("reconciled projection mapping could not be persisted")
			}
			var markErr error
			if candidate.State == "dead_letter" {
				markErr = behaviorgraph.MarkReconciledDeadLetterJobApplied(
					ctx, conn, selected.ProjectKey, candidate.RevisionID, candidate.PayloadSHA, now,
				)
			} else {
				markErr = behaviorgraph.MarkReconciledProjectionJobApplied(
					ctx, conn, selected.ProjectKey, candidate.RevisionID, candidate.PayloadSHA, now,
				)
			}
			if markErr != nil {
				rollbackProjectionConn(conn)
				return applied, errors.New("reconciled projection acknowledgment could not be stored")
			}
		}
		if err := commitImmediate(ctx, conn); err != nil {
			rollbackProjectionConn(conn)
			return applied, errors.New("projection reconciliation batch could not be committed")
		}
		_ = conn.Close()
		applied += end - start
	}
	return applied, nil
}

func countPendingProjectionJobs(ctx context.Context, db *sql.DB) (int64, error) {
	if ctx == nil || db == nil {
		return 0, errors.New("projection queue count is unavailable")
	}
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs WHERE state='pending'").Scan(&count); err != nil {
		return 0, errors.New("pending projection queue count could not be read")
	}
	return count, nil
}

func countDeadLetterProjectionJobs(ctx context.Context, db *sql.DB) (int64, error) {
	if ctx == nil || db == nil {
		return 0, errors.New("dead-letter projection count is unavailable")
	}
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs WHERE state='dead_letter'").Scan(&count); err != nil {
		return 0, errors.New("dead-letter projection count could not be read")
	}
	return count, nil
}
