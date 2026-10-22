package search

import (
	"context"
	"database/sql"
	"errors"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

// CaptureDetail is the redacted structural summary of one exact Caido ID.
// Raw message values are never included in this type.
type CaptureDetail struct {
	ProjectKey                 string                 `json:"project_key"`
	CaidoInstanceID            string                 `json:"caido_instance_id"`
	CaidoProjectID             string                 `json:"caido_project_id"`
	RequestID                  string                 `json:"request_id"`
	CaptureRevision            *string                `json:"capture_revision,omitempty"`
	CapturedAt                 *string                `json:"captured_at"`
	SyncedAt                   *string                `json:"synced_at,omitempty"`
	Method                     string                 `json:"method"`
	Host                       string                 `json:"host"`
	Port                       int                    `json:"port"`
	Route                      string                 `json:"route"`
	Status                     *int                   `json:"status"`
	RequestContentType         string                 `json:"request_content_type"`
	ResponseContentType        string                 `json:"response_content_type"`
	FieldNames                 []string               `json:"field_names"`
	HeaderNames                []string               `json:"header_names"`
	FieldNamesTruncated        bool                   `json:"field_names_truncated"`
	HeaderNamesTruncated       bool                   `json:"header_names_truncated"`
	RequestSHA256              *string                `json:"request_sha256,omitempty"`
	ResponseSHA256             *string                `json:"response_sha256,omitempty"`
	RequestSignature           *string                `json:"request_signature,omitempty"`
	ResponseSignature          *string                `json:"response_signature,omitempty"`
	RequestSize                int64                  `json:"request_size"`
	ResponseSize               *int64                 `json:"response_size"`
	RequestTruncated           *bool                  `json:"request_truncated"`
	ResponseTruncated          *bool                  `json:"response_truncated"`
	RequestWireFidelity        string                 `json:"request_wire_fidelity,omitempty"`
	ResponseWireFidelity       string                 `json:"response_wire_fidelity,omitempty"`
	ParseState                 string                 `json:"parse_state"`
	GraphObservationID         *string                `json:"graph_observation_id"`
	CaidoRequestReference      *CaidoRequestReference `json:"caido_request_reference,omitempty"`
	RawValuesReturned          bool                   `json:"raw_values_returned"`
	RawCaptureAvailableLocally bool                   `json:"raw_capture_available_locally"`
}

type historicalDetailRow struct {
	instanceID, projectID, requestID    string
	requestHash, responseHash           sql.NullString
	requestSize                         int64
	responseSize                        sql.NullInt64
	revision, capturedAt, parseState    sql.NullString
	requestTruncated, responseTruncated sql.NullInt64
	requestFidelity, responseFidelity   sql.NullString
	method, host, route                 sql.NullString
	port                                sql.NullInt64
	status                              sql.NullInt64
	requestType, responseType           sql.NullString
	fieldNames, headerNames             sql.NullString
	graphObservationID                  sql.NullString
	requestVaultRef, responseVaultRef   sql.NullString
}

// ReadCaptureDetail reads one exact capture ID from one already-selected
// project. Live rows win when the same request is present in both indexes.
func ReadCaptureDetail(ctx context.Context, projectStore *store.ProjectStore, requestID string) (CaptureDetail, error) {
	if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil || !projectStore.Database.ReadOnly() {
		return CaptureDetail{}, errors.New("capture detail requires a read-only selected project store")
	}
	projectKey, keyErr := projectStore.Project.StorageKey()
	if keyErr != nil || projectKey != projectStore.ProjectKey {
		return CaptureDetail{}, errors.New("capture detail project namespace is inconsistent")
	}
	if _, err := (project.CaidoExchangeRef{Project: projectStore.Project, RequestID: requestID}).ObservationID(); err != nil {
		return CaptureDetail{}, errors.New("capture request ID is invalid")
	}
	var row historicalDetailRow
	err := projectStore.Database.DB().QueryRowContext(ctx, `
		SELECT c.caido_instance_id,c.caido_project_id,c.request_id,
		       c.request_sha256,c.response_sha256,c.request_size,c.response_size,
		       c.current_revision_id,c.captured_at,c.parse_state,
	       cr.request_truncated,cr.response_truncated,cr.request_wire_fidelity,cr.response_wire_fidelity,
	       s.method,s.host,s.port,s.route,s.response_status,s.request_content_type,s.response_content_type,
	       CAST(substr(CAST(s.field_names_json AS BLOB),1,65537) AS TEXT),
	       CAST(substr(CAST(s.header_names_json AS BLOB),1,65537) AS TEXT),m.graph_observation_id,
	       c.request_vault_ref,c.response_vault_ref
		FROM captures AS c
		LEFT JOIN capture_revisions AS cr ON cr.revision_id=c.current_revision_id
		LEFT JOIN capture_search AS s ON s.observation_id=c.observation_id
		LEFT JOIN graph_capture_map AS m ON m.observation_id=c.observation_id
		  AND m.revision_id=c.current_revision_id
		WHERE c.caido_instance_id=? AND c.caido_project_id=? AND c.request_id=?`,
		projectStore.Project.InstanceID, projectStore.Project.ProjectID, requestID,
	).Scan(
		&row.instanceID, &row.projectID, &row.requestID,
		&row.requestHash, &row.responseHash, &row.requestSize, &row.responseSize,
		&row.revision, &row.capturedAt, &row.parseState,
		&row.requestTruncated, &row.responseTruncated, &row.requestFidelity, &row.responseFidelity,
		&row.method, &row.host, &row.port, &row.route, &row.status, &row.requestType, &row.responseType,
		&row.fieldNames, &row.headerNames, &row.graphObservationID, &row.requestVaultRef, &row.responseVaultRef,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return readLiveCaptureDetail(ctx, projectStore, requestID)
	}
	if err != nil {
		return CaptureDetail{}, errors.New("capture detail could not be read")
	}
	fields, fieldsTruncated, err := decodeOptionalNames(row.fieldNames)
	if err != nil {
		return CaptureDetail{}, errors.New("capture field metadata is malformed")
	}
	headers, headersTruncated, err := decodeOptionalNames(row.headerNames)
	if err != nil {
		return CaptureDetail{}, errors.New("capture header metadata is malformed")
	}
	return CaptureDetail{
		ProjectKey: projectStore.ProjectKey, CaidoInstanceID: row.instanceID, CaidoProjectID: row.projectID,
		RequestID: row.requestID, CaptureRevision: nullableString(row.revision), CapturedAt: nullableString(row.capturedAt),
		Method: row.method.String, Host: row.host.String, Port: int(row.port.Int64), Route: row.route.String,
		Status: nullableInt(row.status), RequestContentType: row.requestType.String, ResponseContentType: row.responseType.String,
		FieldNames: fields, HeaderNames: headers, FieldNamesTruncated: fieldsTruncated, HeaderNamesTruncated: headersTruncated,
		RequestSHA256: nullableString(row.requestHash), ResponseSHA256: nullableString(row.responseHash),
		RequestSize: row.requestSize, ResponseSize: nullableInt64(row.responseSize),
		RequestTruncated: nullableBool(row.requestTruncated), ResponseTruncated: nullableBool(row.responseTruncated),
		RequestWireFidelity:  defaultText(row.requestFidelity.String, "unknown"),
		ResponseWireFidelity: defaultText(row.responseFidelity.String, "unknown"), ParseState: defaultText(row.parseState.String, "not_parsed"),
		GraphObservationID: nullableString(row.graphObservationID), RawValuesReturned: false,
		RawCaptureAvailableLocally: row.requestVaultRef.Valid && row.requestVaultRef.String != "",
	}, nil
}

func readLiveCaptureDetail(ctx context.Context, projectStore *store.ProjectStore, requestID string) (CaptureDetail, error) {
	var row struct {
		instanceID, projectID, requestID                                                 string
		syncedAt, method, host, route                                                    string
		port                                                                             int
		status                                                                           sql.NullInt64
		requestType, responseType, fields, headers, requestSignature, graphObservationID string
		responseSignature                                                                sql.NullString
		requestSize                                                                      int64
		responseSize                                                                     sql.NullInt64
		parseState                                                                       string
	}
	err := projectStore.Database.DB().QueryRowContext(ctx, `
		SELECT caido_instance_id,caido_project_id,request_id,synced_at,method,host,port,route,response_status,
		       request_content_type,response_content_type,
		       CAST(substr(CAST(field_names_json AS BLOB),1,65537) AS TEXT),
		       CAST(substr(CAST(header_names_json AS BLOB),1,65537) AS TEXT),
	       request_signature,response_signature,request_size,response_size,parse_state,graph_observation_id
		FROM live_capture_index WHERE caido_instance_id=? AND caido_project_id=? AND request_id=?`,
		projectStore.Project.InstanceID, projectStore.Project.ProjectID, requestID,
	).Scan(&row.instanceID, &row.projectID, &row.requestID, &row.syncedAt, &row.method, &row.host, &row.port,
		&row.route, &row.status, &row.requestType, &row.responseType, &row.fields, &row.headers,
		&row.requestSignature, &row.responseSignature, &row.requestSize, &row.responseSize, &row.parseState, &row.graphObservationID)
	if err != nil {
		return CaptureDetail{}, errors.New("capture was not found in the selected project")
	}
	fields, fieldsTruncated, err := decodeNames(row.fields)
	if err != nil {
		return CaptureDetail{}, errors.New("capture field metadata is malformed")
	}
	headers, headersTruncated, err := decodeNames(row.headers)
	if err != nil {
		return CaptureDetail{}, errors.New("capture header metadata is malformed")
	}
	return CaptureDetail{
		ProjectKey: projectStore.ProjectKey, CaidoInstanceID: row.instanceID, CaidoProjectID: row.projectID,
		RequestID: row.requestID, SyncedAt: stringPointer(row.syncedAt), Method: row.method, Host: row.host,
		Port: row.port, Route: row.route, Status: nullableInt(row.status), RequestContentType: row.requestType,
		ResponseContentType: row.responseType, FieldNames: fields, HeaderNames: headers,
		FieldNamesTruncated: fieldsTruncated, HeaderNamesTruncated: headersTruncated,
		RequestSignature: stringPointer(row.requestSignature), ResponseSignature: nullableString(row.responseSignature),
		RequestSize: row.requestSize, ResponseSize: nullableInt64(row.responseSize), ParseState: row.parseState,
		GraphObservationID:    stringPointer(row.graphObservationID),
		CaidoRequestReference: &CaidoRequestReference{InstanceID: row.instanceID, ProjectID: row.projectID, RequestID: row.requestID},
		RawValuesReturned:     false, RawCaptureAvailableLocally: false,
	}, nil
}

func decodeOptionalNames(value sql.NullString) ([]string, bool, error) {
	if !value.Valid || value.String == "" {
		return []string{}, true, nil
	}
	return decodeNames(value.String)
}

func defaultText(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
