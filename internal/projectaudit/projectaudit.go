// Package projectaudit reports project-local store compatibility and coverage
// without reading captured request or response bodies.
package projectaudit

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"regexp"
	"time"

	"bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/project"
	capturesearch "bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
)

const expectedCaptureSchemaVersion = store.CurrentSchemaVersion
const maxAuditDuration = 30 * time.Second

var projectKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type CaptureStoreStatus struct {
	State                 string `json:"state"`
	SchemaVersion         int    `json:"schema_version,omitempty"`
	ExpectedSchemaVersion int    `json:"expected_schema_version"`
	RevisionCount         int64  `json:"revision_count"`
}

type BehaviorGraphStatus struct {
	State string `json:"state"`
	graph.DatabaseStats
}

type FidelityCounts struct {
	SourceBytes       int64 `json:"source_bytes"`
	ReconstructedText int64 `json:"reconstructed_text"`
	Unknown           int64 `json:"unknown"`
}

type ProjectionQueueStatus struct {
	State                   string `json:"state"`
	Pending                 int64  `json:"pending"`
	PendingDue              int64  `json:"pending_due"`
	PendingScheduled        int64  `json:"pending_scheduled"`
	OldestPendingAgeSeconds int64  `json:"oldest_pending_age_seconds"`
	Leased                  int64  `json:"leased"`
	ExpiredLeases           int64  `json:"expired_leases"`
	Applied                 int64  `json:"applied"`
	DeadLetter              int64  `json:"dead_letter"`
}

type Report struct {
	ProjectKey           string                      `json:"project_key"`
	OverallState         string                      `json:"overall_state"`
	CaptureStore         CaptureStoreStatus          `json:"capture_store"`
	BehaviorGraph        BehaviorGraphStatus         `json:"behavior_graph"`
	ProjectionQueue      ProjectionQueueStatus       `json:"projection_queue"`
	Coverage             capturesearch.ProjectStatus `json:"coverage"`
	RequestWireFidelity  FidelityCounts              `json:"request_wire_fidelity"`
	ResponseWireFidelity FidelityCounts              `json:"response_wire_fidelity"`
	HistoryReconciled    bool                        `json:"history_reconciled"`
	Notes                []string                    `json:"notes"`
}

// AuditProject inspects one explicitly selected, locally registered project.
// All databases are opened read-only. Only schema metadata, allowlisted status
// columns, and aggregate counts are read; capture bodies and vault files are
// never opened. A completed sync cursor is not treated as complete coverage.
func AuditProject(ctx context.Context, dataRoot, projectKey string) (Report, error) {
	return auditProjectAt(ctx, dataRoot, projectKey, time.Now().UTC())
}

func auditProjectAt(ctx context.Context, dataRoot, projectKey string, observedAt time.Time) (Report, error) {
	if ctx == nil || observedAt.IsZero() {
		return Report{}, errors.New("project audit context and observation time are required")
	}
	ctx, cancel := context.WithTimeout(ctx, maxAuditDuration)
	defer cancel()
	if !projectKeyPattern.MatchString(projectKey) {
		return Report{}, errors.New("select one valid registered project key")
	}
	refs, err := project.LoadProjectRefsReadOnly(dataRoot)
	if err != nil {
		return Report{}, err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return Report{}, errors.New("project registry contains an invalid identity")
		}
		if key == projectKey {
			copy := ref
			selected = &copy
			break
		}
	}
	if selected == nil {
		return Report{}, errors.New("selected project is not in the local project registry")
	}

	report := Report{
		ProjectKey:      projectKey,
		OverallState:    "partial",
		ProjectionQueue: ProjectionQueueStatus{State: "not_checked"},
		CaptureStore: CaptureStoreStatus{
			State:                 "unavailable",
			ExpectedSchemaVersion: expectedCaptureSchemaVersion,
		},
		BehaviorGraph: BehaviorGraphStatus{State: "not_checked"},
		Coverage: capturesearch.ProjectStatus{
			CoverageState: "partial",
			CoverageGaps:  []string{"Capture store compatibility could not be confirmed."},
		},
		HistoryReconciled: false,
		Notes: []string{
			"This audit reads local structural metadata and aggregate status fields; it does not contact Caido or inspect captured bodies.",
			"Compatible means the database schema and required tables are supported; individual graph payloads are not fully validated by this audit.",
			"The audit is bounded to 30 seconds.",
			"Coverage remains partial until imported history is reconciled against a stable expected total.",
		},
	}

	projectStore, err := store.OpenProjectReadOnly(ctx, *selected, dataRoot)
	if err != nil {
		if errors.Is(err, store.ErrUnsupportedSchema) {
			report.CaptureStore.State = "incompatible"
			report.Coverage.CoverageGaps = []string{"Capture store schema is unsupported by this Go build; the database was not changed."}
			report.Notes = append(report.Notes, "Capture store schema compatibility failed; no migration was attempted.")
			return report, nil
		}
		return Report{}, err
	}
	defer projectStore.Close()
	report.CaptureStore.State = "compatible"
	report.CaptureStore.SchemaVersion = store.CurrentSchemaVersion

	tx, err := projectStore.Database.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Report{}, errors.New("project audit snapshot could not be opened")
	}
	defer tx.Rollback()
	status, err := capturesearch.ReadProjectStatusTx(ctx, tx, projectStore)
	if err != nil {
		return Report{}, err
	}
	report.Coverage = status
	if err := readProjectionQueueCounts(ctx, tx, &report.ProjectionQueue, observedAt.UTC()); err != nil {
		report.ProjectionQueue.State = "unavailable"
		report.Coverage.CoverageGaps = append(report.Coverage.CoverageGaps, "Projection queue status could not be read.")
	} else {
		report.ProjectionQueue.State = "compatible"
	}
	if err := readFidelityCounts(ctx, tx, &report); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		return Report{}, errors.New("project audit snapshot could not be completed")
	}

	behaviorStats, err := graph.InspectReadOnly(ctx, projectStore.BehaviorGraphPath, projectKey)
	switch {
	case err == nil:
		report.BehaviorGraph = BehaviorGraphStatus{State: "compatible", DatabaseStats: behaviorStats}
	case errors.Is(err, graph.ErrUnsupportedDatabase):
		report.BehaviorGraph.State = "incompatible"
		report.Coverage.CoverageGaps = append(report.Coverage.CoverageGaps, "BehaviorGraph metadata or schema is unsupported by this Go build.")
	case errors.Is(err, graph.ErrDatabaseMissing):
		report.BehaviorGraph.State = "missing"
		report.Coverage.CoverageGaps = append(report.Coverage.CoverageGaps, "BehaviorGraph database is missing for the selected project.")
	default:
		report.BehaviorGraph.State = "unavailable"
		report.Coverage.CoverageGaps = append(report.Coverage.CoverageGaps, "BehaviorGraph compatibility could not be confirmed.")
	}
	return report, nil
}

func readProjectionQueueCounts(ctx context.Context, tx *sql.Tx, status *ProjectionQueueStatus, observedAt time.Time) error {
	if ctx == nil || tx == nil || status == nil {
		return errors.New("projection queue snapshot is unavailable")
	}
	observedAtText := observedAt.UTC().Format(time.RFC3339Nano)
	var oldestPendingJulian sql.NullFloat64
	var unsupportedStates, malformedPendingTimes, malformedLeaseTimes int64
	err := tx.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN state='pending' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='pending' AND julianday(next_attempt_at)<=julianday(?) THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='pending' AND julianday(next_attempt_at)>julianday(?) THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='leased' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='leased' AND julianday(lease_until)<=julianday(?) THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='applied' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='dead_letter' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state NOT IN ('pending','leased','applied','dead_letter') THEN 1 ELSE 0 END),0),
		  MIN(CASE WHEN state='pending' THEN julianday(created_at) END),
		  COALESCE(SUM(CASE WHEN state='pending' AND (julianday(created_at) IS NULL OR julianday(next_attempt_at) IS NULL) THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN state='leased' AND julianday(lease_until) IS NULL THEN 1 ELSE 0 END),0)
		FROM projection_jobs`, observedAtText, observedAtText, observedAtText).Scan(
		&status.Pending, &status.PendingDue, &status.PendingScheduled, &status.Leased, &status.ExpiredLeases,
		&status.Applied, &status.DeadLetter, &unsupportedStates, &oldestPendingJulian,
		&malformedPendingTimes, &malformedLeaseTimes,
	)
	if err != nil {
		return errors.New("projection queue diagnostics could not be read")
	}
	if unsupportedStates != 0 {
		return errors.New("projection queue contains an unsupported state")
	}
	if malformedPendingTimes != 0 || malformedLeaseTimes != 0 {
		return errors.New("projection queue timestamps are malformed")
	}
	if status.Pending < 0 || status.PendingDue < 0 || status.PendingScheduled < 0 || status.Leased < 0 ||
		status.ExpiredLeases < 0 || status.Applied < 0 || status.DeadLetter < 0 {
		return errors.New("projection queue counts are malformed")
	}
	if status.Pending != status.PendingDue+status.PendingScheduled {
		return errors.New("projection queue retry schedule is incomplete")
	}
	if oldestPendingJulian.Valid {
		oldestPendingUnix := (oldestPendingJulian.Float64 - 2440587.5) * 86400
		ageSeconds := float64(observedAt.UTC().Unix()) - oldestPendingUnix
		if ageSeconds > 0 {
			status.OldestPendingAgeSeconds = int64(math.Round(ageSeconds))
		}
	}
	return nil
}

func readFidelityCounts(ctx context.Context, tx *sql.Tx, report *Report) error {
	var requestTotal, requestSource, requestReconstructed int64
	var responseSource, responseReconstructed int64
	err := tx.QueryRowContext(ctx, `
		SELECT count(*),
		       COALESCE(sum(CASE WHEN request_wire_fidelity='source_bytes' THEN 1 ELSE 0 END),0),
		       COALESCE(sum(CASE WHEN request_wire_fidelity='reconstructed_text' THEN 1 ELSE 0 END),0),
	       COALESCE(sum(CASE WHEN response_wire_fidelity='source_bytes' THEN 1 ELSE 0 END),0),
		       COALESCE(sum(CASE WHEN response_wire_fidelity='reconstructed_text' THEN 1 ELSE 0 END),0)
		FROM capture_revisions`).Scan(&requestTotal, &requestSource, &requestReconstructed, &responseSource, &responseReconstructed)
	if err != nil {
		return errors.New("capture fidelity counts could not be read")
	}
	report.CaptureStore.RevisionCount = requestTotal
	report.RequestWireFidelity = FidelityCounts{
		SourceBytes:       requestSource,
		ReconstructedText: requestReconstructed,
		Unknown:           requestTotal - requestSource - requestReconstructed,
	}
	report.ResponseWireFidelity = FidelityCounts{
		SourceBytes:       responseSource,
		ReconstructedText: responseReconstructed,
		Unknown:           requestTotal - responseSource - responseReconstructed,
	}
	return nil
}
