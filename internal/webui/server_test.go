package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/buildinfo"
	"bb-graph-assistant/internal/consultation"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

type surfaceServiceAPI struct {
	*explorerAPIFixture
	service *appquery.Service
}

func (f *surfaceServiceAPI) GraphExplorerSurface(ctx context.Context, request appquery.SurfaceRequest) (appquery.SurfaceResult, error) {
	return f.service.Surface(ctx, request)
}

type explorerAPIFixture struct {
	direct, full        int
	graphErr            error
	graphPayload        map[string]any
	lastRequest         appquery.GraphRequest
	surfaceErr          error
	lastSurfaceRequest  appquery.SurfaceRequest
	evidenceErr         error
	evidenceResult      appquery.CaptureMessageRange
	lastEvidenceRequest appquery.CaptureMessageRangeRequest
	comparisonErr       error
	comparisonResult    appquery.CaptureComparison
	lastComparison      appquery.CaptureComparisonRequest
	packetErr           error
	packetResult        appquery.EvidencePacket
	lastPacketRequest   appquery.EvidencePacketRequest
}

type consultationAPIFixture struct {
	*explorerAPIFixture
	reviewSession             consultation.Session
	resumed                   consultation.Session
	accepted                  consultation.Session
	acceptErr                 error
	acceptCalls               int
	lastProject               string
	lastSession               string
	lastVersion               int
	lastIdempotency           string
	contextAnnotation         consultation.ContextAnnotation
	contextErr                error
	contextCalls              int
	lastContextRequest        consultation.RecordContextAnnotationRequest
	operationAliases          []consultation.OperationAlias
	operationAlias            consultation.OperationAlias
	operationAliasErr         error
	operationAliasGetCalls    int
	operationAliasSaveCalls   int
	lastOperationAliasRequest consultation.SaveOperationAliasRequest
}

func (f *consultationAPIFixture) CreateSession(_ context.Context, _ appquery.EvidencePacketRequest, _ string) (consultation.Session, error) {
	return consultation.Session{}, nil
}
func (f *consultationAPIFixture) Resume(_ context.Context, _, _ string) (consultation.Session, error) {
	return f.resumed, nil
}
func (f *consultationAPIFixture) ReviewSession(_ context.Context, _, _ string) (consultation.Session, error) {
	return f.reviewSession, nil
}
func (f *consultationAPIFixture) AcceptReview(_ context.Context, projectKey, sessionID string, version int, idempotencyKey string) (consultation.Session, error) {
	f.acceptCalls++
	f.lastProject, f.lastSession, f.lastVersion, f.lastIdempotency = projectKey, sessionID, version, idempotencyKey
	return f.accepted, f.acceptErr
}
func (f *consultationAPIFixture) RecordContextAnnotation(_ context.Context, request consultation.RecordContextAnnotationRequest) (consultation.ContextAnnotation, error) {
	f.contextCalls++
	f.lastContextRequest = request
	return f.contextAnnotation, f.contextErr
}
func (f *consultationAPIFixture) ListOperationAliases(_ context.Context, _ string) ([]consultation.OperationAlias, error) {
	f.operationAliasGetCalls++
	return f.operationAliases, f.operationAliasErr
}
func (f *consultationAPIFixture) SaveOperationAlias(_ context.Context, request consultation.SaveOperationAliasRequest) (consultation.OperationAlias, error) {
	f.operationAliasSaveCalls++
	f.lastOperationAliasRequest = request
	return f.operationAlias, f.operationAliasErr
}
func (f *consultationAPIFixture) Compare(context.Context, string, string, int, string, appquery.CaptureComparisonRequest) (consultation.ComparisonRecord, error) {
	return consultation.ComparisonRecord{}, nil
}
func (f *consultationAPIFixture) RecordProposal(context.Context, consultation.RecordProposalRequest) (consultation.ProposalRecord, error) {
	return consultation.ProposalRecord{}, nil
}
func (f *consultationAPIFixture) ValidateProposal(context.Context, consultation.ValidateProposalRequest) (consultation.ValidationReceipt, error) {
	return consultation.ValidationReceipt{}, nil
}
func (f *consultationAPIFixture) SaveCandidate(context.Context, consultation.SaveCandidateRequest) (consultation.CandidateVersion, error) {
	return consultation.CandidateVersion{}, nil
}

func (*explorerAPIFixture) GraphExplorerProjects(context.Context, string, int) (map[string]any, error) {
	return map[string]any{}, nil
}
func (f *explorerAPIFixture) GraphExplorerSurface(_ context.Context, request appquery.SurfaceRequest) (appquery.SurfaceResult, error) {
	f.lastSurfaceRequest = request
	return appquery.SurfaceResult{ProjectKey: request.ProjectKey, Filter: request.Filter, Coverage: "partial", CoverageGaps: []string{}}, f.surfaceErr
}
func (f *explorerAPIFixture) Graph(_ context.Context, request appquery.GraphRequest) (appquery.GraphResult, error) {
	f.lastRequest = request
	if request.View == appquery.GraphViewFull {
		f.full++
	} else {
		f.direct++
	}
	payload := f.graphPayload
	if payload == nil {
		payload = map[string]any{"view": string(request.View)}
	}
	return appquery.GraphResult{View: request.View, Payload: payload}, f.graphErr
}
func (f *explorerAPIFixture) CaptureMessageRange(_ context.Context, request appquery.CaptureMessageRangeRequest) (appquery.CaptureMessageRange, error) {
	f.lastEvidenceRequest = request
	return f.evidenceResult, f.evidenceErr
}
func (f *explorerAPIFixture) CompareCaptureMessages(_ context.Context, request appquery.CaptureComparisonRequest) (appquery.CaptureComparison, error) {
	f.lastComparison = request
	return f.comparisonResult, f.comparisonErr
}
func (f *explorerAPIFixture) BuildEvidencePacket(_ context.Context, request appquery.EvidencePacketRequest) (appquery.EvidencePacket, error) {
	f.lastPacketRequest = request
	return f.packetResult, f.packetErr
}

func TestEvidenceHandlerReadsOnePinnedBoundedLocalMessageRange(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revisionID := strings.Repeat("b", 64)
	api := &explorerAPIFixture{evidenceResult: appquery.CaptureMessageRange{
		ProjectKey: projectKey, RequestID: "239", RevisionID: revisionID, Side: "request",
		Offset: 16, TotalSize: 21, Data: []byte("GET /"), SHA256: strings.Repeat("c", 64),
		ParseState: "complete", WireFidelity: "reconstructed_text",
		IntegrityStatus: "verified",
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/evidence?project_key="+projectKey+"&request_id=239&revision_id="+revisionID+"&side=request&offset=16&max_bytes=4096", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive evidence response is cacheable")
	}
	if got := api.lastEvidenceRequest; got.ProjectKey != projectKey || got.RequestID != "239" || got.RevisionID != revisionID || got.Side != "request" || got.Offset != 16 || got.Maximum != 4096 {
		t.Fatalf("evidence request = %#v", got)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["data"] != "GET /" || result["encoding"] != "utf-8" || result["revision_id"] != revisionID || result["next_offset"] != nil || result["source_integrity_status"] != "verified" {
		t.Fatalf("evidence response = %#v", result)
	}
}

func TestEvidenceHandlerRejectsDuplicateAndUnknownQueryParameters(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	for _, target := range []string{
		"/api/evidence?project_key=" + projectKey + "&project_key=" + projectKey + "&request_id=239&side=request",
		"/api/evidence?project_key=" + projectKey + "&request_id=239&side=request&unexpected=value",
		"/api/evidence?project_key=" + projectKey + "&request_id=239&side=request&max_bytes=1048577",
	} {
		api := &explorerAPIFixture{}
		handler, _ := NewHandler(api)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("target %q status=%d, want 400", target, response.Code)
		}
		if api.lastEvidenceRequest.RequestID != "" {
			t.Errorf("invalid evidence query reached service: %#v", api.lastEvidenceRequest)
		}
	}
}

func TestEvidenceHandlerExplainsUnavailableLocalVaultKey(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	api := &explorerAPIFixture{evidenceErr: &appquery.QueryError{
		Code: appquery.ErrorEvidenceKeyUnavailable, Message: "local vault key is unavailable or invalid",
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/evidence?project_key="+projectKey+"&request_id=239&side=request", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "local vault key is unavailable or invalid") {
		t.Fatalf("missing-key response status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRuntimeCapabilitiesReportVaultKeyStateWithoutExposingKey(t *testing.T) {
	api := &explorerAPIFixture{}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	requestState := func() (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/runtime-capabilities", nil))
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode runtime capabilities: %v", err)
		}
		return response, payload
	}

	t.Setenv("BB_ASSISTANT_VAULT_KEY", "")
	response, payload := requestState()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || payload["evidence_vault_key_state"] != "missing_or_invalid" {
		t.Fatalf("missing-key response status=%d cache=%q payload=%#v", response.Code, response.Header().Get("Cache-Control"), payload)
	}

	key := make([]byte, 32)
	encoded := base64.RawURLEncoding.EncodeToString(key)
	t.Setenv("BB_ASSISTANT_VAULT_KEY", encoded)
	response, payload = requestState()
	if response.Code != http.StatusOK || payload["evidence_vault_key_state"] != "configured_not_verified" {
		t.Fatalf("configured-key response status=%d payload=%#v", response.Code, payload)
	}
	if strings.Contains(response.Body.String(), encoded) {
		t.Fatal("runtime capabilities response exposed the configured key")
	}
}

func TestHealthHandlerReportsLivenessWithoutReadingApplicationData(t *testing.T) {
	api := &explorerAPIFixture{}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/health", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health response status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload["status"] != "ok" || payload["version"] != buildinfo.String() {
		t.Fatalf("health payload=%#v", payload)
	}
	if api.direct != 0 || api.full != 0 {
		t.Fatalf("liveness handler queried graph data: direct=%d full=%d", api.direct, api.full)
	}
}

func TestComparisonHandlerReadsExactLocalRevisionsWithoutCaching(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	leftRevision := strings.Repeat("b", 64)
	rightRevision := strings.Repeat("c", 64)
	offset := int64(7)
	api := &explorerAPIFixture{comparisonResult: appquery.CaptureComparison{
		ProjectKey: projectKey, LeftRequestID: "left", LeftRevisionID: leftRevision,
		RightRequestID: "right", RightRevisionID: rightRevision,
		Request: appquery.CaptureSideComparison{Available: true, Equal: false, ComparedBytes: 42, FirstDifferenceOffset: &offset},
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	target := "/api/compare?project_key=" + projectKey + "&left_request_id=left&left_revision_id=" + leftRevision +
		"&right_request_id=right&right_revision_id=" + rightRevision
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("comparison response status=%d cache=%q: %s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if got := api.lastComparison; got.ProjectKey != projectKey || got.LeftRequestID != "left" || got.LeftRevisionID != leftRevision ||
		got.RightRequestID != "right" || got.RightRevisionID != rightRevision {
		t.Fatalf("comparison request = %#v", got)
	}
	var result appquery.CaptureComparison
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode comparison response: %v", err)
	}
	if !result.Request.Available || result.Request.FirstDifferenceOffset == nil || *result.Request.FirstDifferenceOffset != 7 {
		t.Fatalf("comparison result = %#v", result)
	}
}

func TestComparisonHandlerRejectsDuplicateAndUnknownQueryParameters(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revision := strings.Repeat("b", 64)
	base := "/api/compare?project_key=" + projectKey + "&left_request_id=left&left_revision_id=" + revision +
		"&right_request_id=right&right_revision_id=" + revision
	for _, target := range []string{base + "&project_key=" + projectKey, base + "&unexpected=value",
		strings.Replace(base, "left_revision_id="+revision, "left_revision_id="+strings.Repeat("z", 64), 1),
		strings.Replace(base, "left_request_id=left", "left_request_id=%20%20", 1),
	} {
		api := &explorerAPIFixture{}
		handler, _ := NewHandler(api)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
		if response.Code != http.StatusBadRequest || api.lastComparison.LeftRequestID != "" {
			t.Fatalf("invalid comparison query status=%d request=%#v", response.Code, api.lastComparison)
		}
	}
}

func TestComparisonHandlerExplainsUnavailableVaultKey(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revision := strings.Repeat("b", 64)
	api := &explorerAPIFixture{comparisonErr: &appquery.QueryError{
		Code: appquery.ErrorEvidenceKeyUnavailable, Message: "local vault key is unavailable or invalid",
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	target := "/api/compare?project_key=" + projectKey + "&left_request_id=left&left_revision_id=" + revision +
		"&right_request_id=right&right_revision_id=" + strings.Repeat("c", 64)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "local vault key is unavailable or invalid") {
		t.Fatalf("comparison missing-key response status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEvidencePacketPreviewRequiresSameOriginAndReturnsNoStorePacket(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revision := strings.Repeat("b", 64)
	request := appquery.EvidencePacketRequest{
		ProjectKey: projectKey, Question: "Compare these spans.",
		Selections: []appquery.EvidencePacketSelection{{RequestID: "239", RevisionID: revision, Side: "request", Offset: 12, Length: 80}},
	}
	api := &explorerAPIFixture{packetResult: appquery.EvidencePacket{
		Version: "1", PolicyVersion: appquery.EvidencePacketPolicyVersion, ProjectKey: projectKey,
		Question: request.Question, PreviewOnly: true, OperatorReviewNeeded: true,
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/evidence/packet-preview", bytes.NewReader(encoded))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("packet preview status=%d cache=%q: %s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if got := api.lastPacketRequest; got.ProjectKey != request.ProjectKey || got.Question != request.Question || len(got.Selections) != 1 || got.Selections[0] != request.Selections[0] {
		t.Fatalf("packet request = %#v", got)
	}
	var result appquery.EvidencePacket
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.PreviewOnly || !result.OperatorReviewNeeded {
		t.Fatalf("packet response = %#v, error=%v", result, err)
	}
}

func TestEvidencePacketPreviewRejectsUnknownFieldsAndCrossOrigin(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	validBody := `{"project_key":"` + projectKey + `","question":"compare","selections":[{"request_id":"239","revision_id":"` + strings.Repeat("b", 64) + `","side":"request","offset":0,"length":8}]}`
	tests := []struct {
		name, body, origin string
		wantStatus         int
	}{
		{name: "unknown field", body: strings.TrimSuffix(validBody, "}") + `,"raw":"secret"}`, origin: "http://127.0.0.1", wantStatus: http.StatusBadRequest},
		{name: "cross origin", body: validBody, origin: "http://example.test", wantStatus: http.StatusForbidden},
		{name: "missing origin", body: validBody, origin: "", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &explorerAPIFixture{}
			handler, _ := NewHandler(api)
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/evidence/packet-preview", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || api.lastPacketRequest.ProjectKey != "" {
				t.Fatalf("status=%d request=%#v body=%s", response.Code, api.lastPacketRequest, response.Body.String())
			}
		})
	}
}

func TestEvidenceHandlerFallsBackToBase64ForControlHeavyTextWithinResponseLimit(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	revisionID := strings.Repeat("b", 64)
	data := bytes.Repeat([]byte{0}, appquery.MaximumEvidenceRangeBytes)
	api := &explorerAPIFixture{evidenceResult: appquery.CaptureMessageRange{
		ProjectKey: projectKey, RequestID: "239", RevisionID: revisionID, Side: "request",
		TotalSize: int64(len(data)), Data: data,
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/evidence?project_key="+projectKey+"&request_id=239&side=request", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	if response.Body.Len() > 2<<20 {
		t.Fatalf("evidence response size=%d, want at most 2 MiB", response.Body.Len())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["encoding"] != "base64" {
		t.Fatalf("encoding=%v, want base64 fallback", result["encoding"])
	}
	encoded, ok := result["data"].(string)
	if !ok {
		t.Fatalf("data type=%T, want string", result["data"])
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatalf("base64 fallback did not preserve exact evidence bytes: decode error=%v, got %d bytes", err, len(decoded))
	}
}

func TestGraphHandlerLoadsDirectNeighborhoodUnlessFullSliceRequested(t *testing.T) {
	for _, view := range []string{"", "direct", "full", "invalid"} {
		t.Run(view, func(t *testing.T) {
			api := &explorerAPIFixture{}
			handler, err := NewHandler(api)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?project_key="+strings.Repeat("a", 64)+"&seed=endpoint-"+strings.Repeat("b", 64)+"&view="+view, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if view == "invalid" {
				if response.Code != http.StatusBadRequest || api.direct != 0 || api.full != 0 {
					t.Fatalf("invalid view dispatched: status=%d direct=%d full=%d", response.Code, api.direct, api.full)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", response.Code, response.Body.String())
			}
			if view == "full" {
				if api.full != 1 || api.direct != 0 {
					t.Fatal("explicit full view did not use the full reader")
				}
			} else if api.direct != 1 || api.full != 0 {
				t.Fatal("default view loaded the full project reader")
			}
		})
	}
}

func TestGraphHandlerMapsTypedErrorsWithoutLeakingCauses(t *testing.T) {
	tests := []struct {
		name string
		code appquery.ErrorCode
		want int
	}{
		{name: "invalid input", code: appquery.ErrorInvalidInput, want: http.StatusBadRequest},
		{name: "unknown project", code: appquery.ErrorProjectNotRegistered, want: http.StatusNotFound},
		{name: "unavailable project", code: appquery.ErrorProjectUnavailable, want: http.StatusServiceUnavailable},
		{name: "large result", code: appquery.ErrorOutputTooLarge, want: http.StatusRequestEntityTooLarge},
		{name: "stale cursor", code: appquery.ErrorStaleCursor, want: http.StatusConflict},
		{name: "unsupported cursor", code: appquery.ErrorCursorUnsupported, want: http.StatusNotImplemented},
		{name: "graph unavailable", code: appquery.ErrorGraphUnavailable, want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &explorerAPIFixture{graphErr: &appquery.QueryError{
				Code: test.code, Err: errors.New("/private/home/secret/database.sqlite3"),
			}}
			handler, err := NewHandler(api)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?project_key="+strings.Repeat("a", 64)+"&seed=endpoint-"+strings.Repeat("b", 64), nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private/home/secret") {
				t.Fatalf("typed error leaked its internal cause: %s", response.Body.String())
			}
		})
	}
}

func TestGraphHandlerPassesContinuationCursorToTypedQuery(t *testing.T) {
	api := &explorerAPIFixture{}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	cursor := "opaque-cursor-value"
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/graph?project_key="+strings.Repeat("a", 64)+"&seed=endpoint-"+strings.Repeat("b", 64)+"&cursor="+cursor, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || string(api.lastRequest.Cursor) != cursor {
		t.Fatalf("continuation request = %#v, status %d: %s", api.lastRequest, response.Code, response.Body.String())
	}
}

func TestGraphHandlerPreservesProvenanceContinuationPayload(t *testing.T) {
	owner := "endpoint-" + strings.Repeat("c", 64)
	api := &explorerAPIFixture{graphPayload: map[string]any{
		"next_cursor": "opaque-cursor", "provenance_truncated": true,
		"provenance_cursor_truncated": false, "provenance_truncated_entity_ids": []string{owner},
		"missing_branches": []map[string]any{{"frontier_id": owner, "reason": "analytical_provenance_limit"}},
	}}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1/api/graph?view=full&project_key="+strings.Repeat("a", 64)+"&seed="+owner, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode graph payload: %v", err)
	}
	if payload["next_cursor"] != "opaque-cursor" || payload["provenance_truncated"] != true ||
		payload["provenance_cursor_truncated"] != false || payload["provenance_truncated_entity_ids"] == nil || payload["missing_branches"] == nil {
		t.Fatalf("HTTP graph response lost provenance continuation fields: %#v", payload)
	}
}

func TestSurfaceHandlerPassesServerSideFilterAndOpaqueCursor(t *testing.T) {
	api := &explorerAPIFixture{}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	filter := " api.fixture "
	cursor := "opaque-signed-cursor"
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/surface?project_key="+strings.Repeat("a", 64)+"&filter="+url.QueryEscape(filter)+"&cursor="+cursor+"&limit=80", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	if api.lastSurfaceRequest.ProjectKey != strings.Repeat("a", 64) || api.lastSurfaceRequest.Filter != filter || api.lastSurfaceRequest.Cursor != cursor || api.lastSurfaceRequest.Limit != 80 {
		t.Fatalf("surface request = %#v", api.lastSurfaceRequest)
	}
}

func TestSurfaceHTTPReadsWALBackedProjectAndFailsClosedOnRollbackJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "surface-http-instance", ProjectID: "surface-http-project"}
	created, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	if err := created.Close(); err != nil {
		t.Fatalf("close created project: %v", err)
	}
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	service, err := appquery.NewGraphQueryService(root, []project.CaidoProjectRef{ref}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService(): %v", err)
	}
	t.Cleanup(func() { _ = service.Close(ctx) })

	directory, err := store.ProjectDirectory(ref, root)
	if err != nil {
		t.Fatalf("ProjectDirectory(): %v", err)
	}
	lease, err := projectlock.Acquire(ctx, directory)
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	writable, err := store.OpenProjectForWrite(ctx, ref, root)
	if err != nil {
		_ = lease.Close()
		t.Fatalf("OpenProjectForWrite(): %v", err)
	}
	observationID := strings.Repeat("a", 64)
	if _, err := writable.Database.DB().ExecContext(ctx,
		`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,request_size,request_vault_ref,parse_state)
		 VALUES(?,?,?,?,?,0,'fixture-vault-ref','complete')`, observationID, ref.InstanceID, ref.ProjectID, "request-1", strings.Repeat("b", 64)); err != nil {
		_ = writable.Close()
		_ = lease.Close()
		t.Fatalf("insert capture: %v", err)
	}
	if _, err := writable.Database.DB().ExecContext(ctx,
		`INSERT INTO capture_search(observation_id,method,host,port,route,response_status,request_content_type,response_content_type,field_names_json,header_names_json)
		 VALUES(?,'GET','api.fixture',443,'/wal',200,'','application/json','[]','[]')`, observationID); err != nil {
		_ = writable.Close()
		_ = lease.Close()
		t.Fatalf("insert surface index: %v", err)
	}
	if err := lease.Close(); err != nil {
		_ = writable.Close()
		t.Fatalf("release fixture writer lease: %v", err)
	}
	databasePath := filepath.Join(directory, "graph.sqlite3")
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(databasePath + suffix); err != nil {
			_ = writable.Close()
			t.Fatalf("expected synthetic %s sidecar: %v", suffix, err)
		}
	}

	api := &surfaceServiceAPI{explorerAPIFixture: &explorerAPIFixture{}, service: service}
	handler, err := NewHandler(api)
	if err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/surface?project_key="+key, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		_ = writable.Close()
		t.Fatalf("WAL-backed surface status=%d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		_ = writable.Close()
		t.Fatalf("decode surface response: %v", err)
	}
	if payload["total_operations"] != float64(1) || payload["operations"] == nil {
		_ = writable.Close()
		t.Fatalf("surface response did not include committed WAL data: %#v", payload)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close fixture writer: %v", err)
	}

	journal := databasePath + "-journal"
	marker := []byte("synthetic rollback journal")
	if err := os.WriteFile(journal, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/surface?project_key="+key, nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("rollback-journal surface status=%d, want 503: %s", response.Code, response.Body.String())
	}
	got, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(got, marker) {
		t.Fatalf("failed-closed read changed rollback journal: data=%q err=%v", got, err)
	}
}

func TestSurfaceHandlerRejectsDuplicateOrOversizedInputsAndMapsStaleCursor(t *testing.T) {
	for _, target := range []string{
		"/api/surface?project_key=" + strings.Repeat("a", 64) + "&filter=x&filter=y",
		"/api/surface?project_key=" + strings.Repeat("a", 64) + "&limit=101",
		"/api/surface?project_key=" + strings.Repeat("a", 64) + "&filter=" + strings.Repeat("x", 257),
	} {
		api := &explorerAPIFixture{}
		handler, _ := NewHandler(api)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("target %q status=%d, want 400", target, response.Code)
		}
		if api.lastSurfaceRequest.ProjectKey != "" {
			t.Errorf("invalid request reached API: %#v", api.lastSurfaceRequest)
		}
	}
	api := &explorerAPIFixture{surfaceErr: &appquery.QueryError{Code: appquery.ErrorStaleCursor}}
	handler, _ := NewHandler(api)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/surface?project_key="+strings.Repeat("a", 64)+"&cursor=stale", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale surface status=%d, want 409: %s", response.Code, response.Body.String())
	}
}

func TestConsultationReviewAndAcceptanceAreLocalNoStoreAndSameOrigin(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	sessionID := strings.Repeat("b", 43)
	preview := consultation.Session{
		ID: sessionID, ProjectKey: projectKey, Version: 1, Question: "Compare two responses", State: consultation.SessionOpen,
		Evidence: appquery.EvidencePacket{ProjectKey: projectKey, Question: "Compare two responses", Evidence: []appquery.EvidencePacketEvidence{{Excerpt: "HTTP/1.1 200 OK"}}},
	}
	accepted := preview
	accepted.Version = 2
	accepted.ReviewAccepted = true
	api := &consultationAPIFixture{
		explorerAPIFixture: &explorerAPIFixture{}, reviewSession: preview,
		accepted: accepted,
	}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	target := "/api/consultation/review?project_key=" + projectKey + "&session_id=" + sessionID
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "HTTP/1.1 200 OK") {
		t.Fatalf("review response status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}

	body := `{"project_key":"` + projectKey + `","session_id":"` + sessionID + `","expected_version":1,"idempotency_key":"accept-review-0123456789"}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/consultation/accept-review", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"review_accepted":true`) {
		t.Fatalf("accept response status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if api.acceptCalls != 1 || api.lastProject != projectKey || api.lastSession != sessionID || api.lastVersion != 1 || api.lastIdempotency != "accept-review-0123456789" {
		t.Fatalf("accept call was not routed exactly: %#v", api)
	}
}

func TestConsultationContextAnnotationRequiresSameOriginAndUsesSelectedReferenceIDs(t *testing.T) {
	projectKey, sessionID, referenceID := strings.Repeat("a", 64), strings.Repeat("b", 43), strings.Repeat("c", 43)
	api := &consultationAPIFixture{
		explorerAPIFixture: &explorerAPIFixture{},
		contextAnnotation: consultation.ContextAnnotation{
			ID: strings.Repeat("d", 43), SessionVersion: 2, Kind: "actor", Label: "actor-a",
			EvidenceReferenceIDs: []string{referenceID}, Origin: "analyst", EvidenceState: "analyst_provided",
		},
	}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project_key":"` + projectKey + `","session_id":"` + sessionID + `","expected_version":1,"idempotency_key":"context-label-0123456789","kind":"actor","label":"actor-a","evidence_reference_ids":["` + referenceID + `"]}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/consultation/context-annotation", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"evidence_state":"analyst_provided"`) {
		t.Fatalf("annotation response status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	got := api.lastContextRequest
	if api.contextCalls != 1 || got.ProjectKey != projectKey || got.SessionID != sessionID || got.ExpectedVersion != 1 || got.Kind != "actor" || got.Label != "actor-a" || len(got.EvidenceReferenceIDs) != 1 || got.EvidenceReferenceIDs[0] != referenceID {
		t.Fatalf("context annotation was not routed exactly: calls=%d request=%#v", api.contextCalls, got)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/consultation/context-annotation", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.example")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || api.contextCalls != 1 {
		t.Fatalf("foreign origin status=%d calls=%d", response.Code, api.contextCalls)
	}
}

func TestOperationAliasAPIIsProjectScopedAndRequiresAnExistingOperationNode(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	operationID := "operation-" + strings.Repeat("b", 64)
	api := &consultationAPIFixture{
		explorerAPIFixture: &explorerAPIFixture{graphPayload: map[string]any{
			"entities": []any{map[string]any{"id": operationID, "kind": "operation"}},
		}},
		operationAliases: []consultation.OperationAlias{{ProjectKey: projectKey, OperationID: operationID, Version: 1, Alias: "Account settings", Origin: "analyst"}},
		operationAlias:   consultation.OperationAlias{ProjectKey: projectKey, OperationID: operationID, Version: 2, Alias: "Profile update", Origin: "analyst"},
	}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/consultation/operation-aliases?project_key="+projectKey, nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "Account settings") || api.operationAliasGetCalls != 1 {
		t.Fatalf("list response status=%d calls=%d body=%s", get.Code, api.operationAliasGetCalls, get.Body.String())
	}

	body := `{"project_key":"` + projectKey + `","operation_id":"` + operationID + `","expected_version":1,"idempotency_key":"test-idem","alias":"Profile update"}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/consultation/operation-alias", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, request)
	if post.Code != http.StatusOK || !strings.Contains(post.Body.String(), `"version":2`) || api.operationAliasSaveCalls != 1 {
		t.Fatalf("save response status=%d calls=%d body=%s", post.Code, api.operationAliasSaveCalls, post.Body.String())
	}
	if got := api.lastOperationAliasRequest; got.ProjectKey != projectKey || got.OperationID != operationID || got.ExpectedVersion != 1 || got.Alias != "Profile update" {
		t.Fatalf("save request = %#v", got)
	}
	if graph := api.lastRequest; graph.View != appquery.GraphViewDirect || len(graph.SeedIDs) != 1 || graph.SeedIDs[0] != operationID {
		t.Fatalf("operation existence query = %#v", graph)
	}

	api.graphPayload = map[string]any{"entities": []any{map[string]any{"id": operationID, "kind": "endpoint"}}}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/consultation/operation-alias", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, request)
	if missing.Code != http.StatusNotFound || api.operationAliasSaveCalls != 1 {
		t.Fatalf("non-operation graph node status=%d save calls=%d body=%s", missing.Code, api.operationAliasSaveCalls, missing.Body.String())
	}
}

func TestConsultationMutationsRejectMissingOrForeignOriginAndMalformedBodies(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	sessionID := strings.Repeat("b", 43)
	body := `{"project_key":"` + projectKey + `","session_id":"` + sessionID + `","expected_version":1,"idempotency_key":"accept-review-0123456789"}`
	for _, test := range []struct {
		name        string
		origin      string
		contentType string
		body        string
		want        int
	}{
		{name: "missing origin", contentType: "application/json", body: body, want: http.StatusMethodNotAllowed},
		{name: "foreign origin", origin: "http://evil.example", contentType: "application/json", body: body, want: http.StatusForbidden},
		{name: "wrong media type", origin: "http://127.0.0.1", contentType: "text/plain", body: body, want: http.StatusUnsupportedMediaType},
		{name: "unknown field", origin: "http://127.0.0.1", contentType: "application/json", body: strings.TrimSuffix(body, "}") + `,"unexpected":true}`, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &consultationAPIFixture{explorerAPIFixture: &explorerAPIFixture{}}
			handler, _ := NewHandler(api)
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/consultation/accept-review", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || api.acceptCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, api.acceptCalls, response.Body.String())
			}
		})
	}
}
