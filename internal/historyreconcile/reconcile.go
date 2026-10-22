// Package historyreconcile compares a bounded Caido metadata enumeration with
// the request IDs already present in one local project store. It never reads
// request bodies or writes project data.
package historyreconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

const (
	maxRunDuration    = 30 * time.Second
	maxPageSize       = 100
	maxPages          = 1000
	maxRequestIDs     = 100000
	maxRequestIDBytes = 512
	maxCursorBytes    = 4096
)

// InventorySource exposes only the no-switch metadata operations needed by
// reconciliation. Implementations must not fetch request or response details.
type InventorySource interface {
	CurrentProjectContext(context.Context) (caido.ProjectRef, error)
	ListRequestPageNoSwitchContext(context.Context, caido.ProjectRef, *string, int) (caido.RequestMetadataPage, error)
}

type Options struct {
	PageSize      int
	MaxPages      int
	MaxRequestIDs int
}

// Report contains aggregate counts only. It intentionally excludes project
// identities, request IDs, URLs, cursor values, and captured content.
type Report struct {
	State                  string `json:"state"`
	PagesCompleted         int    `json:"pages_completed"`
	CaidoRequestCount      int    `json:"caido_request_count"`
	LocalRequestCount      int    `json:"local_request_count"`
	MatchedCount           int    `json:"matched_count"`
	CaidoOnlyCount         int    `json:"caido_only_count"`
	LocalOnlyCount         int    `json:"local_only_count"`
	DuplicateCount         int    `json:"duplicate_count"`
	LocalInventoryComplete bool   `json:"local_inventory_complete"`
	EnumerationTerminal    bool   `json:"enumeration_terminal"`
	CoverageReconciled     bool   `json:"coverage_reconciled"`
	StopReason             string `json:"stop_reason"`
}

// Run is the command-oriented name for Reconcile.
func Run(ctx context.Context, dataRoot, projectKey string, source InventorySource, options Options) (Report, error) {
	return Reconcile(ctx, dataRoot, projectKey, source, options)
}

// Reconcile enumerates a bounded metadata inventory from the project already
// selected in Caido, then compares it with the union of local historical and
// live-tail request IDs. A terminal cursor is not proof of a stable snapshot,
// so State remains partial and CoverageReconciled remains false.
func Reconcile(ctx context.Context, dataRoot, projectKey string, source InventorySource, options Options) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("Caido reconciliation context is required")
	}
	if source == nil {
		return Report{}, errors.New("Caido reconciliation source is required")
	}
	if err := validateOptions(options); err != nil {
		return Report{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, maxRunDuration)
	defer cancel()

	selected, err := registeredProject(dataRoot, projectKey)
	if err != nil {
		return Report{}, err
	}
	report := Report{State: "partial", StopReason: "not_started"}
	current, err := source.CurrentProjectContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			report.StopReason = "cancelled"
			return report, ctx.Err()
		}
		return report, errors.New("selected Caido project could not be confirmed")
	}
	if !current.SameProject(caido.ProjectRef(*selected)) {
		report.StopReason = "project_mismatch"
		return report, errors.New("the registered project is not already selected in Caido")
	}

	projectStore, err := store.OpenProjectReadOnly(ctx, *selected, dataRoot)
	if err != nil {
		return Report{}, err
	}
	defer projectStore.Close()
	localIDs, err := localRequestIDs(ctx, projectStore.Database.DB(), selected.InstanceID, selected.ProjectID, options.MaxRequestIDs)
	if err != nil {
		if ctx.Err() != nil {
			report.StopReason = "cancelled"
			return report, ctx.Err()
		}
		return Report{}, errors.New("local request inventory could not be read")
	}
	report.LocalRequestCount = len(localIDs)
	if len(localIDs) > options.MaxRequestIDs {
		report.StopReason = "local_request_limit"
		return report, nil
	}
	report.LocalInventoryComplete = true

	caidoIDs := make(map[string]struct{})
	cursors := make(map[[32]byte]struct{})
	var cursor *string
	var runErr error
	report.StopReason = "page_limit"
	for report.PagesCompleted < options.MaxPages {
		if err := ctx.Err(); err != nil {
			report.StopReason = "cancelled"
			runErr = err
			break
		}
		if len(caidoIDs) >= options.MaxRequestIDs {
			report.StopReason = "request_limit"
			break
		}
		pageSize := options.PageSize
		remaining := options.MaxRequestIDs - len(caidoIDs)
		if remaining < pageSize {
			pageSize = remaining
		}
		page, pageErr := source.ListRequestPageNoSwitchContext(ctx, caido.ProjectRef(*selected), cursor, pageSize)
		if pageErr != nil {
			if ctx.Err() != nil {
				report.StopReason = "cancelled"
				runErr = ctx.Err()
				break
			}
			report.StopReason = "source_error"
			runErr = errors.New("Caido metadata page could not be read")
			break
		}
		if len(page.Requests) > pageSize || (page.HasMore && len(page.Requests) == 0) {
			report.StopReason = "source_error"
			runErr = errors.New("Caido metadata page is invalid")
			break
		}
		if page.HasMore && (page.NextCursor == nil || *page.NextCursor == "" || len(*page.NextCursor) > maxCursorBytes) {
			report.StopReason = "source_error"
			runErr = errors.New("Caido metadata page cursor is invalid")
			break
		}
		duplicateFound := false
		pageIDs := make([]string, 0, len(page.Requests))
		pageSeen := make(map[string]struct{}, len(page.Requests))
		for _, row := range page.Requests {
			if row.RequestID == "" || len(row.RequestID) > maxRequestIDBytes {
				report.StopReason = "source_error"
				runErr = errors.New("Caido metadata page contains an invalid request ID")
				break
			}
			_, alreadySeen := caidoIDs[row.RequestID]
			_, duplicatedInPage := pageSeen[row.RequestID]
			if alreadySeen || duplicatedInPage {
				report.DuplicateCount++
				duplicateFound = true
				break
			}
			pageSeen[row.RequestID] = struct{}{}
			pageIDs = append(pageIDs, row.RequestID)
		}
		if report.StopReason == "source_error" {
			break
		}
		if duplicateFound {
			report.StopReason = "duplicate_id"
			break
		}
		for _, requestID := range pageIDs {
			caidoIDs[requestID] = struct{}{}
		}
		report.CaidoRequestCount = len(caidoIDs)
		report.PagesCompleted++
		if !page.HasMore {
			report.EnumerationTerminal = true
			report.StopReason = "terminal_cursor"
			break
		}
		next := *page.NextCursor
		cursorDigest := sha256.Sum256([]byte(next))
		if _, repeated := cursors[cursorDigest]; repeated || (cursor != nil && *cursor == next) {
			report.StopReason = "cursor_cycle"
			runErr = errors.New("Caido metadata pagination repeated a cursor")
			break
		}
		cursors[cursorDigest] = struct{}{}
		cursor = &next
	}

	for requestID := range caidoIDs {
		if _, found := localIDs[requestID]; found {
			report.MatchedCount++
		} else {
			report.CaidoOnlyCount++
		}
	}
	report.LocalOnlyCount = len(localIDs) - report.MatchedCount
	return report, runErr
}

func validateOptions(options Options) error {
	if options.PageSize < 1 || options.PageSize > maxPageSize ||
		options.MaxPages < 1 || options.MaxPages > maxPages ||
		options.MaxRequestIDs < 1 || options.MaxRequestIDs > maxRequestIDs {
		return fmt.Errorf("Caido reconciliation bounds must be page-size 1-%d, pages 1-%d, IDs 1-%d", maxPageSize, maxPages, maxRequestIDs)
	}
	return nil
}

func registeredProject(dataRoot, projectKey string) (*project.CaidoProjectRef, error) {
	refs, err := project.LoadProjectRefsReadOnly(dataRoot)
	if err != nil {
		return nil, err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return nil, errors.New("project registry contains an invalid identity")
		}
		if key == projectKey {
			copy := ref
			selected = &copy
			break
		}
	}
	if selected == nil {
		return nil, errors.New("selected project is not in the local project registry")
	}
	return selected, nil
}

func localRequestIDs(ctx context.Context, db *sql.DB, instanceID, projectID string, maxIDs int) (map[string]struct{}, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make(map[string]struct{})
	// Each source table enforces uniqueness by project and request ID. Read
	// them separately so SQLite does not need to materialize an unbounded UNION
	// sort before applying LIMIT. The Go map retains at most maxIDs+1 values.
	queries := []string{
		"SELECT request_id FROM captures WHERE caido_instance_id=? AND caido_project_id=? LIMIT ?",
		"SELECT request_id FROM live_capture_index WHERE caido_instance_id=? AND caido_project_id=? LIMIT ?",
	}
	for _, query := range queries {
		rows, err := tx.QueryContext(ctx, query, instanceID, projectID, maxIDs+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var requestID string
			if err := rows.Scan(&requestID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if requestID == "" || len(requestID) > maxRequestIDBytes {
				_ = rows.Close()
				return nil, errors.New("local request inventory contains an invalid ID")
			}
			ids[requestID] = struct{}{}
			if len(ids) > maxIDs {
				if err := rows.Close(); err != nil {
					return nil, err
				}
				return ids, nil
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}
