package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strings"

	"bb-graph-assistant/internal/store"
)

const (
	DefaultSurfaceCatalogLimit = 40
	MaximumSurfaceCatalogLimit = 100
	MaximumSurfaceFilterBytes  = 256
)

// SurfaceCatalogKey identifies one exact host:port, method, route operation.
// Route canonicalization is deliberately left to a later architecture stage.
type SurfaceCatalogKey struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Method string `json:"method"`
	Route  string `json:"route"`
}

type SurfaceCatalogRequest struct {
	Filter              string
	After               *SurfaceCatalogKey
	SnapshotFingerprint string
	Limit               int
}

// SurfaceOperation contains exact counts for every deduplicated observation
// in one operation from the local structural indexes. Field/header names and
// graph seed IDs are bounded samples; request/response values are never read.
type SurfaceOperation struct {
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

type SurfaceCatalogPage struct {
	ProjectKey          string             `json:"project_key"`
	Filter              string             `json:"filter"`
	Operations          []SurfaceOperation `json:"operations"`
	TotalOperations     int                `json:"total_operations"`
	TotalObservations   int64              `json:"total_observations"`
	RowsRead            int                `json:"rows_read"`
	SnapshotFingerprint string             `json:"snapshot_fingerprint"`
	Truncated           bool               `json:"truncated"`
	NextAfter           *SurfaceCatalogKey `json:"next_after"`
	Coverage            string             `json:"coverage_state"`
	CoverageGaps        []string           `json:"coverage_gaps"`
}

type catalogRecord struct {
	requestID   string
	parseState  string
	key         SurfaceCatalogKey
	fieldNames  string
	headerNames string
	graphSeedID sql.NullString
}

type catalogAccumulator struct {
	operation SurfaceOperation
	fields    map[string]struct{}
	headers   map[string]struct{}
	seeds     map[string]struct{}
}

// ReadSurfaceCatalog scans the local structural indexes in one read-only
// SQLite snapshot. It streams the complete index to provide exact filtered
// totals and a fingerprint, while materializing no more than limit+1 complete
// operations. No request/response bodies or value columns are selected.
func ReadSurfaceCatalog(ctx context.Context, projectStore *store.ProjectStore, request SurfaceCatalogRequest) (SurfaceCatalogPage, error) {
	if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil || !projectStore.Database.ReadOnly() {
		return SurfaceCatalogPage{}, errors.New("surface catalog requires a read-only selected project store")
	}
	projectKey, err := projectStore.Project.StorageKey()
	if err != nil || projectKey != projectStore.ProjectKey {
		return SurfaceCatalogPage{}, errors.New("surface catalog project identity is inconsistent")
	}
	if request.Limit < 1 || request.Limit > MaximumSurfaceCatalogLimit {
		return SurfaceCatalogPage{}, errors.New("surface catalog limit must be between 1 and 100")
	}
	filter := normalizeSurfaceFilter(request.Filter)
	if len(filter) > MaximumSurfaceFilterBytes || (request.After == nil) != (request.SnapshotFingerprint == "") {
		return SurfaceCatalogPage{}, errors.New("surface catalog filter or cursor is invalid")
	}
	if request.After != nil && !validCatalogKey(*request.After) {
		return SurfaceCatalogPage{}, errors.New("surface catalog cursor is invalid")
	}
	if len(request.SnapshotFingerprint) > 64 || (request.SnapshotFingerprint != "" && !isHexDigest(request.SnapshotFingerprint)) {
		return SurfaceCatalogPage{}, errors.New("surface catalog snapshot fingerprint is invalid")
	}

	tx, err := projectStore.Database.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SurfaceCatalogPage{}, errors.New("surface catalog snapshot could not be opened")
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, surfaceCatalogQuery, projectStore.Project.InstanceID, projectStore.Project.ProjectID,
		projectStore.Project.InstanceID, projectStore.Project.ProjectID, projectStore.Project.InstanceID, projectStore.Project.ProjectID)
	if err != nil {
		return SurfaceCatalogPage{}, errors.New("surface catalog index could not be read")
	}
	defer rows.Close()

	page := SurfaceCatalogPage{
		ProjectKey: projectKey, Filter: filter, Operations: []SurfaceOperation{},
		Coverage: "partial", CoverageGaps: []string{
			"Counts cover only deduplicated rows present in the local historical/live structural indexes; they are not Caido project totals.",
			"Captures missing from the local structural indexes and unreconciled Caido history are not covered.",
			"Browser UI walkthrough and JavaScript endpoint coverage are not included.",
		},
	}
	hasher := sha256.New()
	var current *catalogAccumulator
	sawIncompleteParse := false
	sawUnmappedCapture := false
	pageCapacity := request.Limit + 1
	flush := func() {
		if current == nil || !catalogMatchesFilter(current.operation, filter) {
			return
		}
		if len(page.Operations) >= pageCapacity {
			return
		}
		current.operation.FieldNames = sortedSurfaceNames(current.fields)
		current.operation.HeaderNames = sortedSurfaceNames(current.headers)
		current.operation.SampledGraphSeedIDs = sortedSurfaceNames(current.seeds)
		page.Operations = append(page.Operations, current.operation)
	}
	groupKey := SurfaceCatalogKey{}
	haveGroup := false
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return SurfaceCatalogPage{}, err
		}
		var record catalogRecord
		if err := rows.Scan(&record.requestID, &record.parseState, &record.key.Host, &record.key.Port,
			&record.key.Method, &record.key.Route, &record.fieldNames, &record.headerNames, &record.graphSeedID); err != nil {
			return SurfaceCatalogPage{}, errors.New("surface catalog row is malformed")
		}
		page.RowsRead++
		writeCatalogFingerprint(hasher, record)
		if catalogMatchesFilterKey(record.key, filter) {
			if record.parseState != "complete" {
				sawIncompleteParse = true
			}
			if !record.graphSeedID.Valid || !surfaceSeedPattern.MatchString(record.graphSeedID.String) {
				sawUnmappedCapture = true
			}
		}
		if !haveGroup || compareCatalogKey(groupKey, record.key) != 0 {
			flush()
			current = nil
			groupKey = record.key
			haveGroup = true
			if catalogMatchesFilterKey(record.key, filter) {
				page.TotalOperations++
				page.TotalObservations++
				if request.After == nil || compareCatalogKey(record.key, *request.After) > 0 {
					if len(page.Operations) < pageCapacity {
						current = newCatalogAccumulator(record.key)
					}
				}
			} else {
				continue
			}
		} else if catalogMatchesFilterKey(record.key, filter) {
			page.TotalObservations++
		}
		if current != nil {
			current.add(record)
		}
	}
	if err := rows.Err(); err != nil {
		return SurfaceCatalogPage{}, errors.New("surface catalog scan could not finish")
	}
	flush()
	page.SnapshotFingerprint = hex.EncodeToString(hasher.Sum(nil))
	if request.SnapshotFingerprint != "" && request.SnapshotFingerprint != page.SnapshotFingerprint {
		return SurfaceCatalogPage{}, ErrSurfaceCatalogStale
	}
	if len(page.Operations) > request.Limit {
		page.Truncated = true
		page.Operations = page.Operations[:request.Limit]
		last := page.Operations[len(page.Operations)-1]
		page.NextAfter = &SurfaceCatalogKey{Host: last.Host, Port: last.Port, Method: last.Method, Route: last.Route}
	}
	if sawIncompleteParse {
		page.CoverageGaps = appendCoverageGap(page.CoverageGaps, "Some local captures matching this filter have incomplete parsing; field/header samples may be incomplete.")
	}
	if sawUnmappedCapture {
		page.CoverageGaps = appendCoverageGap(page.CoverageGaps, "Some local captures matching this filter have no current graph mapping.")
	}
	if page.Truncated {
		page.CoverageGaps = appendCoverageGap(page.CoverageGaps, "More complete operations match this filter; continue with the returned cursor.")
	}
	if err := tx.Commit(); err != nil {
		return SurfaceCatalogPage{}, errors.New("surface catalog snapshot could not be completed")
	}
	return page, nil
}

var ErrSurfaceCatalogStale = errors.New("surface catalog snapshot changed; reload the first page")

const surfaceCatalogQuery = `SELECT request_id,parse_state,host,port,method,route,field_names_json,header_names_json,graph_seed_id FROM (
	SELECT c.request_id AS request_id,c.parse_state AS parse_state,s.host AS host,s.port AS port,s.method AS method,s.route AS route,
		CAST(substr(CAST(s.field_names_json AS BLOB),1,65537) AS TEXT) AS field_names_json,
		CAST(substr(CAST(s.header_names_json AS BLOB),1,65537) AS TEXT) AS header_names_json,m.graph_observation_id AS graph_seed_id
	FROM captures AS c JOIN capture_search AS s ON s.observation_id=c.observation_id
	LEFT JOIN graph_capture_map AS m ON m.observation_id=c.observation_id AND m.revision_id=c.current_revision_id
	WHERE c.caido_instance_id=? AND c.caido_project_id=?
	UNION ALL
	SELECT l.request_id,l.parse_state,l.host,l.port,l.method,l.route,
		CAST(substr(CAST(l.field_names_json AS BLOB),1,65537) AS TEXT),
		CAST(substr(CAST(l.header_names_json AS BLOB),1,65537) AS TEXT),l.graph_observation_id
	FROM live_capture_index AS l
	WHERE l.caido_instance_id=? AND l.caido_project_id=?
		AND NOT EXISTS (SELECT 1 FROM captures AS c WHERE c.caido_instance_id=? AND c.caido_project_id=? AND c.request_id=l.request_id)
) AS local_rows
ORDER BY host COLLATE BINARY,port,method COLLATE BINARY,route COLLATE BINARY,request_id COLLATE BINARY`

func normalizeSurfaceFilter(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func catalogMatchesFilter(operation SurfaceOperation, filter string) bool {
	return catalogMatchesFilterKey(SurfaceCatalogKey{Host: operation.Host, Port: operation.Port, Method: operation.Method, Route: operation.Route}, filter)
}

func catalogMatchesFilterKey(key SurfaceCatalogKey, filter string) bool {
	if filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(key.Host), filter) || strings.Contains(strings.ToLower(key.Method), filter) || strings.Contains(strings.ToLower(key.Route), filter)
}

func validCatalogKey(key SurfaceCatalogKey) bool {
	return key.Host != "" && len(key.Host) <= 512 && key.Port >= 0 && key.Port <= 65535 && key.Method != "" && len(key.Method) <= 32 && key.Route != "" && len(key.Route) <= 8192
}

func compareCatalogKey(left, right SurfaceCatalogKey) int {
	if left.Host != right.Host {
		if left.Host < right.Host {
			return -1
		}
		return 1
	}
	if left.Port != right.Port {
		if left.Port < right.Port {
			return -1
		}
		return 1
	}
	if left.Method != right.Method {
		if left.Method < right.Method {
			return -1
		}
		return 1
	}
	if left.Route != right.Route {
		if left.Route < right.Route {
			return -1
		}
		return 1
	}
	return 0
}

func newCatalogAccumulator(key SurfaceCatalogKey) *catalogAccumulator {
	return &catalogAccumulator{
		operation: SurfaceOperation{Host: key.Host, Port: key.Port, Method: key.Method, Route: key.Route,
			ObservationCountScope: "local_structural_index", FieldSampleLimit: MaximumSurfaceFieldRows,
			FieldCoverageState: "bounded_sample", FieldNames: []string{}, HeaderNames: []string{}, SampledGraphSeedIDs: []string{}},
		fields: make(map[string]struct{}), headers: make(map[string]struct{}), seeds: make(map[string]struct{}),
	}
}

func (a *catalogAccumulator) add(record catalogRecord) {
	a.operation.ObservationCount++
	if a.operation.FieldSampleCount < MaximumSurfaceFieldRows {
		a.operation.FieldSampleCount++
		fields, fieldsTruncated, err := decodeNames(record.fieldNames)
		if err != nil {
			a.operation.FieldNamesTruncated = true
		} else {
			a.operation.FieldNamesTruncated = a.operation.FieldNamesTruncated || fieldsTruncated
			addSurfaceNames(a.fields, fields, &a.operation.FieldNamesTruncated)
		}
		headers, headersTruncated, err := decodeNames(record.headerNames)
		if err != nil {
			a.operation.HeaderNamesTruncated = true
		} else {
			a.operation.HeaderNamesTruncated = a.operation.HeaderNamesTruncated || headersTruncated
			addSurfaceNames(a.headers, headers, &a.operation.HeaderNamesTruncated)
		}
	} else {
		a.operation.FieldCoverageState = "sample_limit_reached"
	}
	switch record.parseState {
	case "complete":
		a.operation.CompleteParseCount++
	case "partial":
		a.operation.PartialParseCount++
	default:
		a.operation.UnparsedCount++
	}
	if record.graphSeedID.Valid && surfaceSeedPattern.MatchString(record.graphSeedID.String) {
		a.operation.GraphMappedObservationCount++
		if _, exists := a.seeds[record.graphSeedID.String]; exists {
			return
		}
		if len(a.seeds) < maximumSurfaceSeeds {
			a.seeds[record.graphSeedID.String] = struct{}{}
		} else {
			a.operation.GraphSeedsTruncated = true
		}
	}
}

func writeCatalogFingerprint(hasher hash.Hash, record catalogRecord) {
	values := []string{record.requestID, record.key.Host, record.key.Method, record.key.Route, record.parseState, record.fieldNames, record.headerNames}
	port := make([]byte, 8)
	binary.BigEndian.PutUint64(port, uint64(record.key.Port))
	_, _ = hasher.Write(port)
	if record.graphSeedID.Valid {
		values = append(values, record.graphSeedID.String)
	} else {
		values = append(values, "")
	}
	for _, value := range values {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write([]byte(value))
	}
}

func appendCoverageGap(gaps []string, value string) []string {
	for _, existing := range gaps {
		if existing == value {
			return gaps
		}
	}
	return append(gaps, value)
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
