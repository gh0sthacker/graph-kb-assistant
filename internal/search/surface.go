package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"bb-graph-assistant/internal/store"
)

var surfaceSeedPattern = regexp.MustCompile(`^observation-[a-f0-9]{64}$`)

const (
	DefaultSurfaceScanSize  = 25
	MaximumSurfaceScanSize  = 40
	MaximumSurfaceFieldRows = 100
	maximumSurfaceSeeds     = 3
)

// SurfaceCursor resumes a bounded row scan in each local structural index.
// It deliberately does not expose an offset or any Caido cursor.
type SurfaceCursor struct {
	HistoricalRowID int64 `json:"historical_row_id"`
	LiveRowID       int64 `json:"live_row_id"`
}

// SurfaceEndpoint summarizes the observations found in this bounded scan
// page. Counts are page-scoped; endpoints can recur on later pages.
type SurfaceEndpoint struct {
	Host                        string   `json:"host"`
	Port                        int      `json:"port"`
	Route                       string   `json:"route"`
	Method                      string   `json:"method"`
	ObservationCount            int64    `json:"observation_count"`
	ObservationCountScope       string   `json:"observation_count_scope"`
	CompleteParseCount          int64    `json:"complete_parse_count"`
	PartialParseCount           int64    `json:"partial_parse_count"`
	UnparsedCount               int64    `json:"unparsed_count"`
	GraphMappedObservationCount int64    `json:"graph_mapped_observation_count"`
	FieldSampleCount            int      `json:"field_sample_count"`
	FieldSampleLimit            int      `json:"field_sample_limit"`
	FieldCoverageState          string   `json:"field_coverage_state"`
	FieldNames                  []string `json:"field_names"`
	HeaderNames                 []string `json:"header_names"`
	FieldNamesTruncated         bool     `json:"field_names_truncated"`
	HeaderNamesTruncated        bool     `json:"header_names_truncated"`
	SampledGraphSeedIDs         []string `json:"sampled_graph_seed_ids"`
	GraphSeedsTruncated         bool     `json:"graph_seeds_truncated"`
}

// SurfacePage is one bounded scan window, rather than a complete endpoint
// aggregation. The same endpoint may appear on more than one page.
type SurfacePage struct {
	ProjectKey         string            `json:"project_key"`
	Endpoints          []SurfaceEndpoint `json:"endpoints"`
	HistoricalRowsRead int               `json:"historical_rows_read"`
	LiveRowsRead       int               `json:"live_rows_read"`
	Truncated          bool              `json:"truncated"`
	NextAfter          *SurfaceCursor    `json:"next_after"`
	Coverage           string            `json:"coverage_state"`
	CoverageGaps       []string          `json:"coverage_gaps"`
}

type surfaceRecord struct {
	rowID       int64
	requestID   string
	parseState  string
	method      string
	host        string
	port        int
	route       string
	fieldNames  string
	headerNames string
	graphSeedID sql.NullString
}

// ReadSurfaceMap scans no more than limit+1 rows from each local structural
// index in one read-only SQLite snapshot. It uses rowid keysets so the amount
// of materialized metadata is bounded even when a single endpoint is popular.
// Counts and field names describe only this scan window, not project totals.
func ReadSurfaceMap(ctx context.Context, projectStore *store.ProjectStore, after *SurfaceCursor, limit int) (SurfacePage, error) {
	if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil || !projectStore.Database.ReadOnly() {
		return SurfacePage{}, errors.New("surface map requires a read-only selected project store")
	}
	projectKey, err := projectStore.Project.StorageKey()
	if err != nil || projectKey != projectStore.ProjectKey {
		return SurfacePage{}, errors.New("surface map project identity is inconsistent")
	}
	if limit < 1 || limit > MaximumSurfaceScanSize {
		return SurfacePage{}, errors.New("surface scan limit must be between 1 and 40")
	}
	if after != nil && !validSurfaceCursor(*after) {
		return SurfacePage{}, errors.New("surface scan cursor is invalid")
	}
	tx, err := projectStore.Database.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SurfacePage{}, errors.New("surface map snapshot could not be opened")
	}
	defer tx.Rollback()

	page := SurfacePage{
		ProjectKey: projectStore.ProjectKey,
		Endpoints:  []SurfaceEndpoint{},
		Coverage:   "partial",
		CoverageGaps: []string{
			"Counts and field names describe this bounded scan page only; endpoint entries can recur on later pages.",
			"Only local historical/live structural search indexes are scanned; captures without a historical search row and unreconciled Caido history are not covered.",
			"Local indexes may change between page calls; restart the scan after a sync to avoid combining different snapshots.",
			"Browser UI walkthrough and JavaScript endpoint coverage are not included.",
		},
	}
	historyAfter, liveAfter := int64(0), int64(0)
	if after != nil {
		historyAfter, liveAfter = after.HistoricalRowID, after.LiveRowID
	}
	history, historyMore, err := readHistoricalSurfaceRows(ctx, tx, projectStore, historyAfter, limit)
	if err != nil {
		return SurfacePage{}, err
	}
	live, liveMore, err := readLiveSurfaceRows(ctx, tx, projectStore, liveAfter, limit)
	if err != nil {
		return SurfacePage{}, err
	}
	page.HistoricalRowsRead = len(history)
	page.LiveRowsRead = len(live)
	if len(history) > 0 {
		historyAfter = history[len(history)-1].rowID
	}
	if len(live) > 0 {
		liveAfter = live[len(live)-1].rowID
	}
	if historyMore || liveMore {
		page.Truncated = true
		page.NextAfter = &SurfaceCursor{HistoricalRowID: historyAfter, LiveRowID: liveAfter}
	}

	seenRequests := make(map[string]struct{}, len(history)+len(live))
	for _, item := range history {
		seenRequests[item.requestID] = struct{}{}
	}
	for _, item := range live {
		if _, seen := seenRequests[item.requestID]; seen {
			continue
		}
		var exists int
		err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM captures WHERE caido_instance_id=? AND caido_project_id=? AND request_id=? LIMIT 1",
			projectStore.Project.InstanceID, projectStore.Project.ProjectID, item.requestID,
		).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return SurfacePage{}, errors.New("historical/live capture identity could not be reconciled")
		}
		seenRequests[item.requestID] = struct{}{}
		history = append(history, item)
	}

	page.Endpoints = aggregateSurfacePage(history)
	if page.Truncated {
		page.CoverageGaps = append(page.CoverageGaps, "More local structural rows remain; continue from next_after to scan them.")
	}
	for _, endpoint := range page.Endpoints {
		if endpoint.GraphMappedObservationCount < endpoint.ObservationCount {
			page.CoverageGaps = append(page.CoverageGaps, "Some observations in this scan page have no current graph mapping.")
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return SurfacePage{}, errors.New("surface map snapshot could not be completed")
	}
	return page, nil
}

func readHistoricalSurfaceRows(ctx context.Context, tx *sql.Tx, projectStore *store.ProjectStore, after int64, limit int) ([]surfaceRecord, bool, error) {
	query := `SELECT s.rowid,c.request_id,c.parse_state,s.method,s.host,s.port,s.route,
		CAST(substr(CAST(s.field_names_json AS BLOB),1,65537) AS TEXT),
		CAST(substr(CAST(s.header_names_json AS BLOB),1,65537) AS TEXT),
		m.graph_observation_id
		FROM capture_search AS s
		JOIN captures AS c ON c.observation_id=s.observation_id
		LEFT JOIN graph_capture_map AS m
		  ON m.observation_id=c.observation_id AND m.revision_id=c.current_revision_id
		WHERE s.rowid>? AND c.caido_instance_id=? AND c.caido_project_id=?
		ORDER BY s.rowid LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, after, projectStore.Project.InstanceID, projectStore.Project.ProjectID, limit+1)
	if err != nil {
		return nil, false, errors.New("historical surface rows could not be read")
	}
	defer rows.Close()
	items := make([]surfaceRecord, 0, limit+1)
	for rows.Next() {
		var item surfaceRecord
		if err := rows.Scan(&item.rowID, &item.requestID, &item.parseState, &item.method, &item.host, &item.port,
			&item.route, &item.fieldNames, &item.headerNames, &item.graphSeedID); err != nil {
			return nil, false, errors.New("historical surface row is malformed")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.New("historical surface scan could not finish")
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

func readLiveSurfaceRows(ctx context.Context, tx *sql.Tx, projectStore *store.ProjectStore, after int64, limit int) ([]surfaceRecord, bool, error) {
	query := `SELECT rowid,request_id,parse_state,method,host,port,route,
		CAST(substr(CAST(field_names_json AS BLOB),1,65537) AS TEXT),
		CAST(substr(CAST(header_names_json AS BLOB),1,65537) AS TEXT),graph_observation_id
		FROM live_capture_index WHERE rowid>? AND caido_instance_id=? AND caido_project_id=?
		ORDER BY rowid LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, after, projectStore.Project.InstanceID, projectStore.Project.ProjectID, limit+1)
	if err != nil {
		return nil, false, errors.New("live surface rows could not be read")
	}
	defer rows.Close()
	items := make([]surfaceRecord, 0, limit+1)
	for rows.Next() {
		var item surfaceRecord
		if err := rows.Scan(&item.rowID, &item.requestID, &item.parseState, &item.method, &item.host, &item.port,
			&item.route, &item.fieldNames, &item.headerNames, &item.graphSeedID); err != nil {
			return nil, false, errors.New("live surface row is malformed")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.New("live surface scan could not finish")
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

func aggregateSurfacePage(records []surfaceRecord) []SurfaceEndpoint {
	byKey := make(map[string]*SurfaceEndpoint, len(records))
	fieldSets := make(map[string]map[string]struct{}, len(records))
	headerSets := make(map[string]map[string]struct{}, len(records))
	seedSets := make(map[string]map[string]struct{}, len(records))
	for _, record := range records {
		key := surfaceEntryKey(record.host, record.port, record.route, record.method)
		entry := byKey[key]
		if entry == nil {
			entry = &SurfaceEndpoint{
				Host: record.host, Port: record.port, Route: record.route, Method: record.method,
				FieldNames: []string{}, HeaderNames: []string{}, SampledGraphSeedIDs: []string{},
				ObservationCountScope: "scan_page", FieldSampleLimit: MaximumSurfaceFieldRows,
				FieldCoverageState: "sampled",
			}
			byKey[key] = entry
			fieldSets[key] = make(map[string]struct{})
			headerSets[key] = make(map[string]struct{})
			seedSets[key] = make(map[string]struct{})
		}
		entry.ObservationCount++
		entry.FieldSampleCount++
		switch record.parseState {
		case "complete":
			entry.CompleteParseCount++
		case "partial":
			entry.PartialParseCount++
		default:
			entry.UnparsedCount++
		}
		if record.graphSeedID.Valid && surfaceSeedPattern.MatchString(record.graphSeedID.String) {
			entry.GraphMappedObservationCount++
			if _, exists := seedSets[key][record.graphSeedID.String]; exists {
			} else if len(seedSets[key]) < maximumSurfaceSeeds {
				seedSets[key][record.graphSeedID.String] = struct{}{}
			} else {
				entry.GraphSeedsTruncated = true
			}
		}
		fieldNames, fieldsTruncated, err := decodeNames(record.fieldNames)
		if err != nil {
			entry.FieldNamesTruncated = true
		} else {
			entry.FieldNamesTruncated = entry.FieldNamesTruncated || fieldsTruncated
			addSurfaceNames(fieldSets[key], fieldNames, &entry.FieldNamesTruncated)
		}
		headerNames, headersTruncated, err := decodeNames(record.headerNames)
		if err != nil {
			entry.HeaderNamesTruncated = true
		} else {
			entry.HeaderNamesTruncated = entry.HeaderNamesTruncated || headersTruncated
			addSurfaceNames(headerSets[key], headerNames, &entry.HeaderNamesTruncated)
		}
	}
	result := make([]SurfaceEndpoint, 0, len(byKey))
	for key, pointer := range byKey {
		pointer.FieldNames = sortedSurfaceNames(fieldSets[key])
		pointer.HeaderNames = sortedSurfaceNames(headerSets[key])
		pointer.SampledGraphSeedIDs = sortedSurfaceNames(seedSets[key])
		entry := *pointer
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Host != right.Host {
			return left.Host < right.Host
		}
		if left.Port != right.Port {
			return left.Port < right.Port
		}
		if left.Route != right.Route {
			return left.Route < right.Route
		}
		return left.Method < right.Method
	})
	return result
}

func addSurfaceNames(destination map[string]struct{}, names []string, truncated *bool) {
	for _, name := range names {
		if _, exists := destination[name]; exists {
			continue
		}
		if len(destination) >= MaximumNames {
			*truncated = true
			continue
		}
		destination[name] = struct{}{}
	}
}

func sortedSurfaceNames(names map[string]struct{}) []string {
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func surfaceEntryKey(host string, port int, route, method string) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", host, port, route, method)
}

func validSurfaceCursor(cursor SurfaceCursor) bool {
	return cursor.HistoricalRowID >= 0 && cursor.LiveRowID >= 0
}
