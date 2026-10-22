package search

import (
	"context"
	"database/sql"
	"errors"

	"bb-graph-assistant/internal/store"
)

const (
	historicalCaidoSourceKey = "caido-http-history-v1"
	liveCaidoSourceKey       = "caido-http-live-tail-v1"
)

// ProjectStatus reports local import and structural-index coverage. It never
// treats a terminal cursor as proof that Caido history was fully reconciled.
type ProjectStatus struct {
	CaptureCount            int64    `json:"capture_count"`
	SearchIndexedCount      int64    `json:"search_indexed_count"`
	LiveIndexedCount        int64    `json:"live_indexed_count"`
	GraphMappedCaptureCount int64    `json:"graph_mapped_capture_count"`
	UnmappedCaptureCount    int64    `json:"unmapped_capture_count"`
	CompleteParseCount      int64    `json:"complete_parse_count"`
	PartialParseCount       int64    `json:"partial_parse_count"`
	UnparsedCount           int64    `json:"unparsed_count"`
	LiveCompleteParseCount  int64    `json:"live_complete_parse_count"`
	LivePartialParseCount   int64    `json:"live_partial_parse_count"`
	HistoricalSyncState     string   `json:"historical_sync_state"`
	HistoricalSyncUpdatedAt *string  `json:"historical_sync_updated_at"`
	LiveSyncState           string   `json:"live_sync_state"`
	LiveSyncUpdatedAt       *string  `json:"live_sync_updated_at"`
	CoverageState           string   `json:"coverage_state"`
	CoverageGaps            []string `json:"coverage_gaps"`
}

// ReadProjectStatus takes one read-only SQLite snapshot for local counts and
// sync checkpoints. The result describes imported local data, not completeness
// of Caido history or browser/JavaScript coverage.
func ReadProjectStatus(ctx context.Context, projectStore *store.ProjectStore) (ProjectStatus, error) {
	if !validProjectStatusStore(projectStore) {
		return ProjectStatus{}, errors.New("project status identity is inconsistent")
	}
	tx, err := projectStore.Database.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProjectStatus{}, errors.New("project status snapshot could not be opened")
	}
	defer tx.Rollback()
	status, err := ReadProjectStatusTx(ctx, tx, projectStore)
	if err != nil {
		return ProjectStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectStatus{}, errors.New("project status snapshot could not be completed")
	}
	return status, nil
}

// ReadProjectStatusTx reads status rows from a caller-owned read-only snapshot.
// The caller is responsible for committing or rolling back the transaction.
func ReadProjectStatusTx(ctx context.Context, tx *sql.Tx, projectStore *store.ProjectStore) (ProjectStatus, error) {
	if tx == nil || !validProjectStatusStore(projectStore) {
		return ProjectStatus{}, errors.New("project status requires a read-only selected project store")
	}

	status := ProjectStatus{
		HistoricalSyncState: "not_started",
		LiveSyncState:       "not_started",
		CoverageState:       "partial",
		CoverageGaps: []string{
			"Caido does not provide a reconciled expected total or snapshot token for imported history.",
			"This index does not include browser UI walkthrough or JavaScript endpoint coverage.",
		},
	}
	var complete, partial, unparsed int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*),
		       COALESCE(sum(CASE WHEN parse_state='complete' THEN 1 ELSE 0 END),0),
		       COALESCE(sum(CASE WHEN parse_state='partial' THEN 1 ELSE 0 END),0),
		       COALESCE(sum(CASE WHEN parse_state NOT IN ('complete','partial') THEN 1 ELSE 0 END),0)
		FROM captures`).Scan(&status.CaptureCount, &complete, &partial, &unparsed); err != nil {
		return ProjectStatus{}, errors.New("capture coverage could not be read")
	}
	status.CompleteParseCount = complete
	status.PartialParseCount = partial
	status.UnparsedCount = unparsed
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM capture_search").Scan(&status.SearchIndexedCount); err != nil {
		return ProjectStatus{}, errors.New("historical search coverage could not be read")
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*),
		       COALESCE(sum(CASE WHEN parse_state='complete' THEN 1 ELSE 0 END),0),
		       COALESCE(sum(CASE WHEN parse_state='partial' THEN 1 ELSE 0 END),0)
		FROM live_capture_index`).Scan(&status.LiveIndexedCount, &status.LiveCompleteParseCount, &status.LivePartialParseCount); err != nil {
		return ProjectStatus{}, errors.New("live search coverage could not be read")
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT count(DISTINCT g.observation_id)
		FROM graph_capture_map AS g
		JOIN captures AS c
		  ON c.observation_id=g.observation_id
		 AND c.current_revision_id=g.revision_id`).Scan(&status.GraphMappedCaptureCount); err != nil {
		return ProjectStatus{}, errors.New("graph mapping coverage could not be read")
	}
	if status.CaptureCount > status.GraphMappedCaptureCount {
		status.UnmappedCaptureCount = status.CaptureCount - status.GraphMappedCaptureCount
	}
	var state, historicalUpdatedAt, liveUpdatedAt string
	err := tx.QueryRowContext(ctx,
		"SELECT state,updated_at FROM sync_state WHERE source_key=?", historicalCaidoSourceKey,
	).Scan(&state, &historicalUpdatedAt)
	if err == nil {
		status.HistoricalSyncState = state
		status.HistoricalSyncUpdatedAt = &historicalUpdatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProjectStatus{}, errors.New("historical sync state could not be read")
	}
	err = tx.QueryRowContext(ctx,
		"SELECT state,updated_at FROM live_sync_state WHERE source_key=?", liveCaidoSourceKey,
	).Scan(&status.LiveSyncState, &liveUpdatedAt)
	if err == nil {
		status.LiveSyncUpdatedAt = &liveUpdatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProjectStatus{}, errors.New("live sync state could not be read")
	}
	if status.SearchIndexedCount < status.CaptureCount {
		status.CoverageGaps = append(status.CoverageGaps, "Some stored captures are missing a structural search row.")
	}
	if status.HistoricalSyncState != "done" {
		status.CoverageGaps = append(status.CoverageGaps, "Historical import checkpoint is "+status.HistoricalSyncState+"; the import is not known to be complete.")
	}
	if status.UnmappedCaptureCount > 0 {
		status.CoverageGaps = append(status.CoverageGaps, "Some stored captures are not mapped to a BehaviorGraph observation.")
	}
	if status.PartialParseCount > 0 || status.UnparsedCount > 0 || status.LivePartialParseCount > 0 {
		status.CoverageGaps = append(status.CoverageGaps, "Some stored captures are partial or not parsed.")
	}
	if status.LiveIndexedCount > 0 {
		status.CoverageGaps = append(status.CoverageGaps, "Live-tail rows are not reconciled against a stable Caido history watermark.")
	}
	return status, nil
}

func validProjectStatusStore(projectStore *store.ProjectStore) bool {
	if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil || !projectStore.Database.ReadOnly() {
		return false
	}
	projectKey, err := projectStore.Project.StorageKey()
	return err == nil && projectKey == projectStore.ProjectKey
}
