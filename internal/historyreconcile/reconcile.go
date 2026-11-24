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
	"net/url"
	"strconv"
	"strings"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

const (
	defaultRunDuration = 30 * time.Second
	maxRunDuration     = 30 * time.Minute
	maxPageSize        = 100
	maxPages           = 10000
	maxRequestIDs      = 1000000
	maxRequestIDBytes  = 512
	maxCursorBytes     = 4096
	minLocalScanRows   = 1024
	maxLocalScanRows   = 4000000
)

// InventorySource exposes only the no-switch metadata operations needed by
// reconciliation. Implementations must not fetch request or response details.
type InventorySource interface {
	CurrentProjectContext(context.Context) (caido.ProjectRef, error)
	ListRequestPageNoSwitchContext(context.Context, caido.ProjectRef, *string, int) (caido.RequestMetadataPage, error)
}

// ScopedInventorySource supports an exact-host metadata filter without
// changing the already selected Caido project.
type ScopedInventorySource interface {
	InventorySource
	ListRequestPageNoSwitchHostContext(context.Context, caido.ProjectRef, *string, int, string) (caido.RequestMetadataPage, error)
}

type Options struct {
	PageSize      int
	MaxPages      int
	MaxRequestIDs int
	MaxDuration   time.Duration
	ScopeHost     string
	ScopePort     int
}

// Report contains aggregate counts only. It intentionally excludes project
// identities, request IDs, URLs, cursor values, and captured content.
type Report struct {
	State                   string `json:"state"`
	PagesCompleted          int    `json:"pages_completed"`
	CaidoRequestCount       int    `json:"caido_request_count"`
	LocalRequestCount       int    `json:"local_request_count"`
	MatchedCount            int    `json:"matched_count"`
	CaidoOnlyCount          int    `json:"caido_only_count"`
	LocalOnlyCount          int    `json:"local_only_count"`
	DuplicateCount          int    `json:"duplicate_count"`
	LocalInventoryComplete  bool   `json:"local_inventory_complete"`
	ScopeHost               string `json:"scope_host,omitempty"`
	ScopePort               int    `json:"scope_port,omitempty"`
	CaidoHostFilterCount    int    `json:"caido_host_filter_count"`
	CaidoScopeExcludedCount int    `json:"caido_scope_excluded_count"`
	CaidoUnclassifiedCount  int    `json:"caido_unclassified_count"`
	LocalUnclassifiedCount  int    `json:"local_unclassified_count"`
	EnumerationTerminal     bool   `json:"enumeration_terminal"`
	CoverageReconciled      bool   `json:"coverage_reconciled"`
	StopReason              string `json:"stop_reason"`
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
	scopeHost := ""
	if options.ScopeHost != "" {
		var err error
		scopeHost, err = caido.NormalizeRequestHost(options.ScopeHost)
		if err != nil {
			return Report{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, reconciliationDuration(options.MaxDuration))
	defer cancel()

	selected, err := registeredProject(dataRoot, projectKey)
	if err != nil {
		return Report{}, err
	}
	report := Report{State: "partial", StopReason: "not_started"}
	if scopeHost != "" {
		report.ScopeHost = scopeHost
		report.ScopePort = options.ScopePort
	}
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
	var scopedSource ScopedInventorySource
	if scopeHost != "" {
		var ok bool
		scopedSource, ok = source.(ScopedInventorySource)
		if !ok {
			report.StopReason = "scoped_source_unavailable"
			return report, errors.New("Caido source does not support exact-host metadata filters")
		}
	}

	projectStore, err := store.OpenProjectReadOnly(ctx, *selected, dataRoot)
	if err != nil {
		return Report{}, err
	}
	defer projectStore.Close()
	var localIDs map[string]struct{}
	localUnclassified := 0
	localLimitReason := ""
	if scopeHost != "" {
		localIDs, localUnclassified, localLimitReason, err = localScopedRequestIDs(ctx, projectStore.Database.DB(), selected.InstanceID, selected.ProjectID, scopeHost, options.ScopePort, options.MaxRequestIDs)
	} else {
		localIDs, err = localRequestIDs(ctx, projectStore.Database.DB(), selected.InstanceID, selected.ProjectID, options.MaxRequestIDs)
	}
	if err != nil {
		if ctx.Err() != nil {
			report.StopReason = "cancelled"
			return report, ctx.Err()
		}
		return Report{}, errors.New("local request inventory could not be read")
	}
	report.LocalRequestCount = len(localIDs)
	report.LocalUnclassifiedCount = localUnclassified
	if len(localIDs) > options.MaxRequestIDs || localLimitReason != "" {
		if localLimitReason != "" {
			report.StopReason = localLimitReason
		} else {
			report.StopReason = "local_request_limit"
		}
		return report, nil
	}
	report.LocalInventoryComplete = localUnclassified == 0

	caidoIDs := make(map[string]struct{})
	seenCaidoIDs := make(map[string]struct{})
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
		enumeratedCount := len(caidoIDs)
		if scopeHost != "" {
			enumeratedCount = report.CaidoHostFilterCount
		}
		if enumeratedCount >= options.MaxRequestIDs {
			report.StopReason = "request_limit"
			break
		}
		pageSize := options.PageSize
		remaining := options.MaxRequestIDs - enumeratedCount
		if remaining < pageSize {
			pageSize = remaining
		}
		var page caido.RequestMetadataPage
		var pageErr error
		if scopedSource != nil {
			page, pageErr = scopedSource.ListRequestPageNoSwitchHostContext(ctx, caido.ProjectRef(*selected), cursor, pageSize, scopeHost)
		} else {
			page, pageErr = source.ListRequestPageNoSwitchContext(ctx, caido.ProjectRef(*selected), cursor, pageSize)
		}
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
		if scopeHost != "" {
			report.CaidoHostFilterCount += len(page.Requests)
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
			_, alreadySeen := seenCaidoIDs[row.RequestID]
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
			seenCaidoIDs[requestID] = struct{}{}
		}
		for _, row := range page.Requests {
			if scopeHost != "" {
				host, port, hostErr := requestHostPort(row.URL)
				if hostErr != nil {
					report.CaidoUnclassifiedCount++
					continue
				}
				if host != scopeHost || port != options.ScopePort {
					report.CaidoScopeExcludedCount++
					continue
				}
			}
			caidoIDs[row.RequestID] = struct{}{}
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
		options.MaxRequestIDs < 1 || options.MaxRequestIDs > maxRequestIDs ||
		options.MaxDuration < 0 || options.MaxDuration > maxRunDuration {
		return fmt.Errorf("Caido reconciliation bounds must be page-size 1-%d, pages 1-%d, IDs 1-%d", maxPageSize, maxPages, maxRequestIDs)
	}
	if err := ValidateScope(options.ScopeHost, options.ScopePort); err != nil {
		return err
	}
	return nil
}

// ValidateScope requires an exact host and port together. An empty scope keeps
// the existing whole-project inventory behavior.
func ValidateScope(host string, port int) error {
	if host == "" && port == 0 {
		return nil
	}
	if host == "" || port < 1 || port > 65535 {
		return errors.New("Caido scope requires one valid host and port")
	}
	if _, err := caido.NormalizeRequestHost(host); err != nil {
		return err
	}
	return nil
}

func requestHostPort(rawURL string) (string, int, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" ||
		(strings.ToLower(parsed.Scheme) != "http" && strings.ToLower(parsed.Scheme) != "https") {
		return "", 0, errors.New("Caido URL is not an absolute HTTP URL")
	}
	host, err := caido.NormalizeRequestHost(parsed.Hostname())
	if err != nil {
		return "", 0, err
	}
	port := 80
	if strings.EqualFold(parsed.Scheme, "https") {
		port = 443
	}
	if value := parsed.Port(); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return "", 0, errors.New("Caido URL port is invalid")
		}
	}
	return host, port, nil
}

func reconciliationDuration(configured time.Duration) time.Duration {
	if configured <= 0 {
		return defaultRunDuration
	}
	return configured
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

func localScopedRequestIDs(ctx context.Context, db *sql.DB, instanceID, projectID, host string, port, maxIDs int) (map[string]struct{}, int, string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, "", err
	}
	defer tx.Rollback()
	ids := make(map[string]struct{})
	queries := []struct {
		query string
		args  []any
	}{
		{
			query: `SELECT c.request_id, s.host, s.port FROM captures c
				LEFT JOIN capture_search s ON s.observation_id=c.observation_id
				WHERE c.caido_instance_id=? AND c.caido_project_id=?
				ORDER BY c.request_id LIMIT ?`,
			args: []any{instanceID, projectID},
		},
		{
			query: `SELECT request_id, host, port FROM live_capture_index
				WHERE caido_instance_id=? AND caido_project_id=?
				ORDER BY request_id LIMIT ?`,
			args: []any{instanceID, projectID},
		},
	}
	localScanLimit := localScanRowLimit(maxIDs)
	rowsScanned := 0
	unclassified := 0
	for _, item := range queries {
		remaining := localScanLimit - rowsScanned
		args := append(item.args, remaining+1)
		rows, err := tx.QueryContext(ctx, item.query, args...)
		if err != nil {
			return nil, 0, "", err
		}
		for rows.Next() {
			var requestID string
			var indexedHost sql.NullString
			var indexedPort sql.NullInt64
			if err := rows.Scan(&requestID, &indexedHost, &indexedPort); err != nil {
				_ = rows.Close()
				return nil, 0, "", err
			}
			if requestID == "" || len(requestID) > maxRequestIDBytes {
				_ = rows.Close()
				return nil, 0, "", errors.New("local scoped inventory contains an invalid ID")
			}
			rowsScanned++
			if rowsScanned > localScanLimit {
				if err := rows.Close(); err != nil {
					return nil, 0, "", err
				}
				return ids, unclassified, "local_scan_limit", nil
			}
			valid := indexedHost.Valid && strings.ToLower(strings.TrimSpace(indexedHost.String)) != "unknown" && indexedPort.Valid && indexedPort.Int64 >= 1 && indexedPort.Int64 <= 65535
			normalizedHost := ""
			if valid {
				var hostErr error
				normalizedHost, hostErr = caido.NormalizeRequestHost(indexedHost.String)
				valid = hostErr == nil
			}
			if !valid {
				unclassified++
				continue
			}
			if normalizedHost != host || int(indexedPort.Int64) != port {
				continue
			}
			ids[requestID] = struct{}{}
			if len(ids) > maxIDs {
				if err := rows.Close(); err != nil {
					return nil, 0, "", err
				}
				return ids, unclassified, "local_request_limit", nil
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, 0, "", err
		}
		if err := rows.Close(); err != nil {
			return nil, 0, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, "", err
	}
	return ids, unclassified, "", nil
}

func localScanRowLimit(maxIDs int) int {
	limit := maxIDs * 4
	if limit < minLocalScanRows {
		return minLocalScanRows
	}
	if limit > maxLocalScanRows {
		return maxLocalScanRows
	}
	return limit
}
