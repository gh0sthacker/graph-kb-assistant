// Package search provides bounded, project-scoped reads over the local
// historical and live Caido structural indexes.
package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/store"
)

const (
	DefaultLimit       = 100
	MaximumLimit       = 500
	MaximumProjects    = 20
	MaximumNames       = 50
	MaximumNameLength  = 256
	MaximumNamesJSON   = 64 << 10
	maximumSearchTerms = 32
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

var ErrSignatureIndexUnavailable = errors.New("selected graph needs a signature index rebuild before signature-filtered search")

// Filters are applied with AND semantics. Limit must be between 1 and 500.
type Filters struct {
	Text              string
	Method            string
	Host              string
	Route             string
	Status            *int
	FieldName         string
	RequestSignature  string
	ResponseSignature string
	Since             string
	Until             string
	Limit             int
}

type Result struct {
	Hits                []Hit    `json:"hits"`
	Truncated           bool     `json:"truncated"`
	SelectedProjectKeys []string `json:"selected_project_keys"`
}

type CaidoRequestReference struct {
	InstanceID string `json:"instance_id"`
	ProjectID  string `json:"project_id"`
	RequestID  string `json:"request_id"`
}

// Hit contains only structural metadata and provenance; it never contains
// request or response message bytes or values.
type Hit struct {
	ProjectKey           string                 `json:"project_key"`
	ObservationID        string                 `json:"observation_id"`
	GraphSeedID          *string                `json:"graph_seed_id"`
	RequestID            string                 `json:"request_id"`
	CapturedAt           *string                `json:"captured_at"`
	SyncedAt             *string                `json:"synced_at,omitempty"`
	Method               string                 `json:"method"`
	Host                 string                 `json:"host"`
	Port                 int                    `json:"port"`
	Route                string                 `json:"route"`
	Status               *int                   `json:"status"`
	RequestContentType   string                 `json:"request_content_type"`
	ResponseContentType  string                 `json:"response_content_type"`
	FieldNames           []string               `json:"field_names"`
	HeaderNames          []string               `json:"header_names"`
	FieldNamesTruncated  bool                   `json:"field_names_truncated"`
	HeaderNamesTruncated bool                   `json:"header_names_truncated"`
	RequestSHA256        *string                `json:"request_sha256,omitempty"`
	ResponseSHA256       *string                `json:"response_sha256,omitempty"`
	RequestSignature     *string                `json:"request_signature,omitempty"`
	ResponseSignature    *string                `json:"response_signature,omitempty"`
	ResponseSize         *int64                 `json:"response_size,omitempty"`
	CaptureRevision      *string                `json:"capture_revision,omitempty"`
	ParseState           string                 `json:"parse_state"`
	RequestTruncated     *bool                  `json:"request_truncated"`
	ResponseTruncated    *bool                  `json:"response_truncated"`
	RequestWireFidelity  string                 `json:"request_wire_fidelity"`
	ResponseWireFidelity string                 `json:"response_wire_fidelity"`
	RecordSource         string                 `json:"record_source"`
	CaidoRequestRef      *CaidoRequestReference `json:"caido_request_reference,omitempty"`
	RawStoredLocally     bool                   `json:"raw_stored_locally"`
}

// Captures searches only the stores explicitly passed by the caller.
func Captures(ctx context.Context, stores []*store.ProjectStore, filters Filters) (Result, error) {
	return CapturesWithGraphService(ctx, graphaccess.DefaultService(), stores, filters)
}

// CapturesWithGraphService searches only explicitly selected projects and
// resolves signature filters through the caller-owned graph service.
func CapturesWithGraphService(ctx context.Context, graphService *graphaccess.Service, stores []*store.ProjectStore, filters Filters) (Result, error) {
	limit, matchQuery, err := validate(stores, &filters)
	if err != nil {
		return Result{}, err
	}
	keys := make([]string, 0, len(stores))
	for _, projectStore := range stores {
		keys = append(keys, projectStore.ProjectKey)
	}
	result := Result{Hits: []Hit{}, SelectedProjectKeys: keys}

	allHits := make([]Hit, 0, min(limit*len(stores), 10000))
	for _, projectStore := range stores {
		if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil {
			return Result{}, errors.New("selected project store is unavailable")
		}
		historical, truncated, queryErr := searchHistorical(ctx, graphService, projectStore, filters, matchQuery, limit)
		if queryErr != nil {
			return Result{}, queryErr
		}
		result.Truncated = result.Truncated || truncated
		allHits = append(allHits, historical...)
		live, truncated, queryErr := searchLive(ctx, projectStore, filters, matchQuery, limit)
		if queryErr != nil {
			return Result{}, queryErr
		}
		result.Truncated = result.Truncated || truncated
		allHits = append(allHits, live...)
	}

	sort.SliceStable(allHits, func(i, j int) bool {
		leftTime := hitTime(allHits[i])
		rightTime := hitTime(allHits[j])
		if leftTime != rightTime {
			return leftTime > rightTime
		}
		if allHits[i].RequestID != allHits[j].RequestID {
			return allHits[i].RequestID > allHits[j].RequestID
		}
		return allHits[i].ProjectKey < allHits[j].ProjectKey
	})
	seen := make(map[string]struct{}, len(allHits))
	unique := allHits[:0]
	for _, hit := range allHits {
		key := hit.ProjectKey + "\x00" + hit.RequestID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, hit)
	}
	if len(unique) > limit {
		result.Truncated = true
		unique = unique[:limit]
	}
	result.Hits = unique
	return result, nil
}

func validate(stores []*store.ProjectStore, filters *Filters) (int, string, error) {
	if len(stores) == 0 {
		return 0, "", errors.New("select at least one Caido project before searching")
	}
	if len(stores) > MaximumProjects {
		return 0, "", errors.New("select no more than 20 Caido projects")
	}
	seen := make(map[string]struct{}, len(stores))
	for _, projectStore := range stores {
		if projectStore == nil || projectStore.ProjectKey == "" || projectStore.Database == nil || projectStore.Database.DB() == nil {
			return 0, "", errors.New("selected project store is unavailable")
		}
		canonicalKey, err := projectStore.Project.StorageKey()
		if err != nil || canonicalKey != projectStore.ProjectKey {
			return 0, "", errors.New("selected project store identity is inconsistent")
		}
		if !projectStore.Database.ReadOnly() {
			return 0, "", errors.New("capture search requires read-only project stores")
		}
		if _, exists := seen[projectStore.ProjectKey]; exists {
			return 0, "", errors.New("a Caido project may appear only once in the selection")
		}
		seen[projectStore.ProjectKey] = struct{}{}
	}
	if filters.Limit < 1 || filters.Limit > MaximumLimit {
		return 0, "", errors.New("limit must be between 1 and 500")
	}
	if utf8.RuneCountInString(filters.Text) > 4096 {
		return 0, "", errors.New("search text exceeds the 4096-character limit")
	}
	if len(filters.Method) > 16 || len(filters.Host) > 253 || len(filters.Route) > 2048 || len(filters.FieldName) > 256 || len(filters.Since) > 64 || len(filters.Until) > 64 {
		return 0, "", errors.New("a search filter exceeds its maximum length")
	}
	if filters.Status != nil && (*filters.Status < 100 || *filters.Status > 599) {
		return 0, "", errors.New("status must be an HTTP status code")
	}
	var sinceTime, untilTime time.Time
	if filters.Since != "" {
		var err error
		sinceTime, err = parseTimestamp(filters.Since)
		if err != nil {
			return 0, "", errors.New("since must be an RFC 3339 timestamp")
		}
	}
	if filters.Until != "" {
		var err error
		untilTime, err = parseTimestamp(filters.Until)
		if err != nil {
			return 0, "", errors.New("until must be an RFC 3339 timestamp")
		}
	}
	if !sinceTime.IsZero() && !untilTime.IsZero() && sinceTime.After(untilTime) {
		return 0, "", errors.New("since must not be later than until")
	}
	for _, signature := range []string{filters.RequestSignature, filters.ResponseSignature} {
		if signature != "" && !digestPattern.MatchString(signature) {
			return 0, "", errors.New("request and response signatures must be lowercase 64-character digests")
		}
	}
	terms := searchTerms(filters.Text)
	if len(terms) == 0 {
		return filters.Limit, "", nil
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"*`)
	}
	return filters.Limit, strings.Join(quoted, " OR "), nil
}

func parseTimestamp(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func searchTerms(input string) []string {
	terms := make([]string, 0, maximumSearchTerms)
	current := make([]rune, 0, 64)
	flush := func() {
		if len(current) >= 2 && len(terms) < maximumSearchTerms {
			terms = append(terms, string(current))
		}
		current = current[:0]
	}
	for _, r := range input {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			current = append(current, r)
			if len(current) == 64 {
				flush()
			}
		} else {
			flush()
			if len(terms) == maximumSearchTerms {
				break
			}
		}
	}
	if len(terms) < maximumSearchTerms {
		flush()
	}
	return terms
}

func searchHistorical(ctx context.Context, graphService *graphaccess.Service, projectStore *store.ProjectStore, filters Filters, matchQuery string, limit int) ([]Hit, bool, error) {
	signatureScoped := filters.RequestSignature != "" || filters.ResponseSignature != ""
	if !signatureScoped {
		hits, truncated, err := queryHistoricalBatch(ctx, projectStore, filters, matchQuery, limit, nil, false)
		return hits, truncated, err
	}
	if graphaccess.ConfiguredBackend() == "sqlite" {
		if _, err := os.Lstat(projectStore.BehaviorGraphPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return []Hit{}, false, nil
			}
			return nil, false, errors.New("historical graph signature index is unavailable")
		}
	}
	variantObservationIDs, variantTruncated, err := graphService.ObservationIDsByVariantSignatures(
		ctx, projectStore, filters.RequestSignature, filters.ResponseSignature, behaviorgraph.MaxVariantObservations,
	)
	if err != nil {
		if errors.Is(err, graphaccess.ErrSignatureIndexUnavailable) {
			return nil, false, ErrSignatureIndexUnavailable
		}
		return nil, false, errors.New("historical graph signatures could not be searched")
	}
	if len(variantObservationIDs) == 0 {
		return []Hit{}, variantTruncated, nil
	}

	// Filter and sort every bounded candidate batch before applying the result
	// limit. Capping graph IDs first can hide later matches behind unrelated
	// observations whose host, route, text, or time does not match.
	const candidateBatchSize = 400
	var hits []Hit
	matchTruncated := false
	for start := 0; start < len(variantObservationIDs); start += candidateBatchSize {
		end := min(start+candidateBatchSize, len(variantObservationIDs))
		batchHits, batchTruncated, err := queryHistoricalBatch(ctx, projectStore, filters, matchQuery, limit, variantObservationIDs[start:end], true)
		if err != nil {
			return nil, false, err
		}
		matchTruncated = matchTruncated || batchTruncated
		hits = retainHistoricalTop(hits, batchHits, limit)
	}
	if len(hits) > limit {
		hits = hits[:limit]
		return hits, true, nil
	}
	return hits, variantTruncated || matchTruncated, nil
}

func queryHistoricalBatch(ctx context.Context, projectStore *store.ProjectStore, filters Filters, matchQuery string, limit int, variantObservationIDs []string, signatureScoped bool) ([]Hit, bool, error) {
	query := `SELECT x.observation_id,x.request_id,x.captured_at,x.request_sha256,x.response_sha256,x.parse_state,
	                 x.current_revision_id,cr.request_truncated,cr.response_truncated,
	                 cr.request_wire_fidelity,cr.response_wire_fidelity,
	                 s.method,s.host,s.port,s.route,s.response_status,
	                 s.request_content_type,s.response_content_type,
	                 CAST(substr(CAST(s.field_names_json AS BLOB),1,65537) AS TEXT),
	                 CAST(substr(CAST(s.header_names_json AS BLOB),1,65537) AS TEXT),
	                 m.graph_observation_id
	          FROM capture_search s
	          JOIN captures x ON x.observation_id=s.observation_id
	          LEFT JOIN capture_revisions cr ON cr.revision_id=x.current_revision_id
	          LEFT JOIN graph_capture_map m ON m.observation_id=x.observation_id
	                AND m.revision_id=x.current_revision_id`
	if matchQuery != "" {
		query += ` JOIN capture_search_fts f ON f.observation_id=s.observation_id`
	}
	clauses, args := historicalClauses(filters, matchQuery)
	clauses = append(clauses, "x.caido_instance_id=?", "x.caido_project_id=?")
	args = append(args, projectStore.Project.InstanceID, projectStore.Project.ProjectID)
	if signatureScoped {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(variantObservationIDs)), ",")
		if len(variantObservationIDs) == 0 {
			return []Hit{}, false, nil
		}
		clauses = append(clauses, "m.graph_observation_id IN ("+placeholders+")")
		for _, observationID := range variantObservationIDs {
			args = append(args, observationID)
		}
	}
	if len(clauses) != 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY x.captured_at DESC,x.request_id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := projectStore.Database.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, errors.New("historical capture search failed")
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit+1)
	for rows.Next() {
		var r historicalRow
		if err := rows.Scan(
			&r.observationID, &r.requestID, &r.capturedAt, &r.requestSHA256, &r.responseSHA256, &r.parseState,
			&r.revisionID, &r.requestTruncated, &r.responseTruncated,
			&r.requestFidelity, &r.responseFidelity,
			&r.method, &r.host, &r.port, &r.route, &r.status,
			&r.requestContentType, &r.responseContentType, &r.fieldNamesJSON, &r.headerNamesJSON,
			&r.graphSeedID,
		); err != nil {
			return nil, false, errors.New("historical capture search returned an invalid row")
		}
		hit, err := r.hit(projectStore)
		if err != nil {
			return nil, false, err
		}
		if filters.RequestSignature != "" {
			hit.RequestSignature = stringPointer(filters.RequestSignature)
		}
		if filters.ResponseSignature != "" {
			hit.ResponseSignature = stringPointer(filters.ResponseSignature)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.New("historical capture search could not finish")
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	return hits, truncated, nil
}

func retainHistoricalTop(existing, incoming []Hit, limit int) []Hit {
	combined := append(existing, incoming...)
	sort.SliceStable(combined, func(i, j int) bool {
		leftTime, rightTime := hitTime(combined[i]), hitTime(combined[j])
		if leftTime != rightTime {
			return leftTime > rightTime
		}
		return combined[i].RequestID > combined[j].RequestID
	})
	seen := make(map[string]struct{}, len(combined))
	unique := combined[:0]
	for _, hit := range combined {
		key := hit.ProjectKey + "\x00" + hit.RequestID
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, hit)
		if len(unique) == limit+1 {
			break
		}
	}
	return unique
}

func historicalClauses(filters Filters, matchQuery string) ([]string, []any) {
	clauses := make([]string, 0, 9)
	args := make([]any, 0, 9)
	if matchQuery != "" {
		clauses = append(clauses, "capture_search_fts MATCH ?")
		args = append(args, matchQuery)
	}
	if filters.Method != "" {
		clauses = append(clauses, "s.method=?")
		args = append(args, strings.ToUpper(filters.Method))
	}
	if filters.Host != "" {
		clauses = append(clauses, "instr(lower(s.host),lower(?))>0")
		args = append(args, filters.Host)
	}
	if filters.Route != "" {
		clauses = append(clauses, "instr(lower(s.route),lower(?))>0")
		args = append(args, strings.ToLower(filters.Route))
	}
	if filters.Status != nil {
		clauses = append(clauses, "s.response_status=?")
		args = append(args, *filters.Status)
	}
	if filters.FieldName != "" {
		clauses = append(clauses, "instr(lower(s.field_names_json),lower(?))>0")
		args = append(args, strings.ToLower(filters.FieldName))
	}
	if filters.Since != "" {
		clauses = append(clauses, "x.captured_at>=?")
		args = append(args, filters.Since)
	}
	if filters.Until != "" {
		clauses = append(clauses, "x.captured_at<=?")
		args = append(args, filters.Until)
	}
	return clauses, args
}

type historicalRow struct {
	observationID       string
	requestID           string
	capturedAt          sql.NullString
	requestSHA256       string
	responseSHA256      sql.NullString
	parseState          string
	revisionID          sql.NullString
	requestTruncated    sql.NullInt64
	responseTruncated   sql.NullInt64
	requestFidelity     sql.NullString
	responseFidelity    sql.NullString
	method              string
	host                string
	port                int
	route               string
	status              sql.NullInt64
	requestContentType  string
	responseContentType string
	fieldNamesJSON      string
	headerNamesJSON     string
	graphSeedID         sql.NullString
}

func (r historicalRow) hit(projectStore *store.ProjectStore) (Hit, error) {
	fields, fieldsTruncated, err := decodeNames(r.fieldNamesJSON)
	if err != nil {
		return Hit{}, err
	}
	headers, headersTruncated, err := decodeNames(r.headerNamesJSON)
	if err != nil {
		return Hit{}, err
	}
	return Hit{
		ProjectKey: projectStore.ProjectKey, ObservationID: r.observationID, GraphSeedID: nullableString(r.graphSeedID),
		RequestID: r.requestID, CapturedAt: nullableString(r.capturedAt),
		Method: r.method, Host: r.host, Port: r.port, Route: r.route, Status: nullableInt(r.status),
		RequestContentType: r.requestContentType, ResponseContentType: r.responseContentType,
		FieldNames: fields, HeaderNames: headers, FieldNamesTruncated: fieldsTruncated,
		HeaderNamesTruncated: headersTruncated, RequestSHA256: stringPointer(r.requestSHA256),
		ResponseSHA256: nullableString(r.responseSHA256), CaptureRevision: nullableString(r.revisionID),
		ParseState: r.parseState, RequestTruncated: nullableBool(r.requestTruncated),
		ResponseTruncated:   nullableBool(r.responseTruncated),
		RequestWireFidelity: fidelity(r.requestFidelity), ResponseWireFidelity: fidelity(r.responseFidelity),
		RecordSource: "encrypted_local_capture", RawStoredLocally: true,
		CaidoRequestRef: &CaidoRequestReference{
			InstanceID: projectStore.Project.InstanceID, ProjectID: projectStore.Project.ProjectID,
			RequestID: r.requestID,
		},
	}, nil
}

func searchLive(ctx context.Context, projectStore *store.ProjectStore, filters Filters, matchQuery string, limit int) ([]Hit, bool, error) {
	query := `SELECT l.observation_id,l.request_id,l.synced_at,l.method,l.host,l.port,l.route,l.response_status,
	                 l.request_content_type,l.response_content_type,
	                 CAST(substr(CAST(l.field_names_json AS BLOB),1,65537) AS TEXT),
	                 CAST(substr(CAST(l.header_names_json AS BLOB),1,65537) AS TEXT),
	                 l.request_signature,l.response_signature,l.response_size,l.parse_state,
	                 l.graph_observation_id,l.caido_instance_id,l.caido_project_id
	          FROM live_capture_index l`
	if matchQuery != "" {
		query += ` JOIN live_capture_fts f ON f.observation_id=l.observation_id`
	}
	clauses, args := liveClauses(filters, matchQuery)
	clauses = append(clauses, "l.caido_instance_id=?", "l.caido_project_id=?")
	args = append(args, projectStore.Project.InstanceID, projectStore.Project.ProjectID)
	if len(clauses) != 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY l.synced_at DESC,l.request_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := projectStore.Database.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, errors.New("live capture search failed")
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit+1)
	for rows.Next() {
		var r liveRow
		if err := rows.Scan(
			&r.observationID, &r.requestID, &r.syncedAt, &r.method, &r.host, &r.port, &r.route, &r.status,
			&r.requestContentType, &r.responseContentType, &r.fieldNamesJSON, &r.headerNamesJSON,
			&r.requestSignature, &r.responseSignature, &r.responseSize, &r.parseState,
			&r.graphSeedID, &r.instanceID, &r.projectID,
		); err != nil {
			return nil, false, errors.New("live capture search returned an invalid row")
		}
		hit, err := r.hit(projectStore.ProjectKey)
		if err != nil {
			return nil, false, err
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.New("live capture search could not finish")
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	return hits, truncated, nil
}

func liveClauses(filters Filters, matchQuery string) ([]string, []any) {
	clauses := make([]string, 0, 10)
	args := make([]any, 0, 10)
	if filters.RequestSignature != "" {
		clauses = append(clauses, "l.request_signature=?")
		args = append(args, filters.RequestSignature)
	}
	if filters.ResponseSignature != "" {
		clauses = append(clauses, "l.response_signature=?")
		args = append(args, filters.ResponseSignature)
	}
	if matchQuery != "" {
		clauses = append(clauses, "live_capture_fts MATCH ?")
		args = append(args, matchQuery)
	}
	if filters.Method != "" {
		clauses = append(clauses, "l.method=?")
		args = append(args, strings.ToUpper(filters.Method))
	}
	if filters.Host != "" {
		clauses = append(clauses, "instr(lower(l.host),lower(?))>0")
		args = append(args, filters.Host)
	}
	if filters.Route != "" {
		clauses = append(clauses, "instr(lower(l.route),lower(?))>0")
		args = append(args, strings.ToLower(filters.Route))
	}
	if filters.Status != nil {
		clauses = append(clauses, "l.response_status=?")
		args = append(args, *filters.Status)
	}
	if filters.FieldName != "" {
		clauses = append(clauses, "instr(lower(l.field_names_json),lower(?))>0")
		args = append(args, strings.ToLower(filters.FieldName))
	}
	if filters.Since != "" {
		clauses = append(clauses, "l.synced_at>=?")
		args = append(args, filters.Since)
	}
	if filters.Until != "" {
		clauses = append(clauses, "l.synced_at<=?")
		args = append(args, filters.Until)
	}
	return clauses, args
}

type liveRow struct {
	observationID       string
	requestID           string
	syncedAt            string
	method              string
	host                string
	port                int
	route               string
	status              sql.NullInt64
	requestContentType  string
	responseContentType string
	fieldNamesJSON      string
	headerNamesJSON     string
	requestSignature    string
	responseSignature   sql.NullString
	responseSize        sql.NullInt64
	parseState          string
	graphSeedID         string
	instanceID          string
	projectID           string
}

func (r liveRow) hit(projectKey string) (Hit, error) {
	fields, fieldsTruncated, err := decodeNames(r.fieldNamesJSON)
	if err != nil {
		return Hit{}, err
	}
	headers, headersTruncated, err := decodeNames(r.headerNamesJSON)
	if err != nil {
		return Hit{}, err
	}
	return Hit{
		ProjectKey: projectKey, ObservationID: r.observationID, GraphSeedID: stringPointer(r.graphSeedID),
		RequestID: r.requestID, SyncedAt: stringPointer(r.syncedAt), Method: r.method,
		Host: r.host, Port: r.port, Route: r.route, Status: nullableInt(r.status),
		RequestContentType: r.requestContentType, ResponseContentType: r.responseContentType,
		FieldNames: fields, HeaderNames: headers, FieldNamesTruncated: fieldsTruncated,
		HeaderNamesTruncated: headersTruncated, RequestSignature: stringPointer(r.requestSignature),
		ResponseSignature: nullableString(r.responseSignature), ResponseSize: nullableInt64(r.responseSize),
		ParseState: r.parseState, RecordSource: "caido_live_tail", RawStoredLocally: false,
		CaidoRequestRef: &CaidoRequestReference{InstanceID: r.instanceID, ProjectID: r.projectID, RequestID: r.requestID},
	}, nil
}

func decodeNames(encoded string) ([]string, bool, error) {
	if len(encoded) > MaximumNamesJSON {
		return []string{}, true, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(encoded), &names); err != nil || names == nil {
		return nil, false, errors.New("capture metadata contains an invalid name list")
	}
	truncated := len(names) > MaximumNames
	if truncated {
		names = names[:MaximumNames]
	}
	for i, name := range names {
		if utf8.RuneCountInString(name) > MaximumNameLength {
			names[i] = string([]rune(name)[:MaximumNameLength])
			truncated = true
		}
	}
	return names, truncated, nil
}

func hitTime(hit Hit) string {
	if hit.CapturedAt != nil {
		return *hit.CapturedAt
	}
	if hit.SyncedAt != nil {
		return *hit.SyncedAt
	}
	return ""
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func stringPointer(value string) *string { return &value }

func nullableInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func nullableBool(value sql.NullInt64) *bool {
	if !value.Valid {
		return nil
	}
	converted := value.Int64 != 0
	return &converted
}

func fidelity(value sql.NullString) string {
	if !value.Valid || value.String == "" {
		return "unknown"
	}
	return value.String
}
