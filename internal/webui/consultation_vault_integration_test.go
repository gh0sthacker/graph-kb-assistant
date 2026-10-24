package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/consultation"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

type consultationServiceHTTPAPI struct {
	*evidenceServiceAPI
	consultations *consultation.Service
}

func (api *consultationServiceHTTPAPI) CreateSession(ctx context.Context, request appquery.EvidencePacketRequest, key string) (consultation.Session, error) {
	return api.consultations.CreateSession(ctx, request, key)
}
func (api *consultationServiceHTTPAPI) Resume(ctx context.Context, projectKey, sessionID string) (consultation.Session, error) {
	return api.consultations.Resume(ctx, projectKey, sessionID)
}
func (api *consultationServiceHTTPAPI) ReviewSession(ctx context.Context, projectKey, sessionID string) (consultation.Session, error) {
	return api.consultations.ReviewSession(ctx, projectKey, sessionID)
}
func (api *consultationServiceHTTPAPI) AcceptReview(ctx context.Context, projectKey, sessionID string, version int, key string) (consultation.Session, error) {
	return api.consultations.AcceptReview(ctx, projectKey, sessionID, version, key)
}
func (api *consultationServiceHTTPAPI) RecordContextAnnotation(ctx context.Context, request consultation.RecordContextAnnotationRequest) (consultation.ContextAnnotation, error) {
	return api.consultations.RecordContextAnnotation(ctx, request)
}
func (api *consultationServiceHTTPAPI) ListOperationAliases(ctx context.Context, projectKey string) ([]consultation.OperationAlias, error) {
	return api.consultations.ListOperationAliases(ctx, projectKey)
}
func (api *consultationServiceHTTPAPI) SaveOperationAlias(ctx context.Context, request consultation.SaveOperationAliasRequest) (consultation.OperationAlias, error) {
	return api.consultations.SaveOperationAlias(ctx, request)
}
func (api *consultationServiceHTTPAPI) Compare(ctx context.Context, projectKey, sessionID string, version int, key string, request appquery.CaptureComparisonRequest) (consultation.ComparisonRecord, error) {
	return api.consultations.Compare(ctx, projectKey, sessionID, version, key, request)
}
func (api *consultationServiceHTTPAPI) RecordProposal(ctx context.Context, request consultation.RecordProposalRequest) (consultation.ProposalRecord, error) {
	return api.consultations.RecordProposal(ctx, request)
}
func (api *consultationServiceHTTPAPI) ValidateProposal(ctx context.Context, request consultation.ValidateProposalRequest) (consultation.ValidationReceipt, error) {
	return api.consultations.ValidateProposal(ctx, request)
}
func (api *consultationServiceHTTPAPI) SaveCandidate(ctx context.Context, request consultation.SaveCandidateRequest) (consultation.CandidateVersion, error) {
	return api.consultations.SaveCandidate(ctx, request)
}

func TestConsultationOperatorReviewUsesRealHTTPAndEncryptedEvidence(t *testing.T) {
	ctx := context.Background()
	vaultKey := make([]byte, 32)
	for index := range vaultKey {
		vaultKey[index] = byte(index + 1)
	}
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(vaultKey))

	root := filepath.Join(t.TempDir(), "runtime")
	ref := project.CaidoProjectRef{InstanceID: "webui-review-fixture", ProjectID: "operator-review"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	writer, err := ingest.OpenPageWriter(ctx, projectStore)
	if err != nil {
		_ = projectStore.Close()
		t.Fatalf("OpenPageWriter: %v", err)
	}
	run, err := writer.BeginRun(ctx)
	if err != nil {
		writer.Close()
		_ = projectStore.Close()
		t.Fatalf("BeginRun: %v", err)
	}
	requestBytes := []byte("GET /profile HTTP/1.1\r\nHost: app.fixture.invalid\r\nAuthorization: Bearer operator-review-secret\r\n\r\n")
	responseBytes := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nsynthetic profile")
	status := 200
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: "review-capture"},
		RequestRaw: requestBytes, ResponseRaw: responseBytes, HasResponse: true,
		Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
		Target: "https://app.fixture.invalid/profile", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	if _, err := writer.WritePage(ctx, ingest.HistoryPageInput{RunID: run.RunID, Page: caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}}}); err != nil {
		writer.Close()
		_ = projectStore.Close()
		t.Fatalf("WritePage: %v", err)
	}
	var revisionID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", exchange.Ref.RequestID,
	).Scan(&revisionID); err != nil {
		writer.Close()
		_ = projectStore.Close()
		t.Fatalf("read exact fixture revision: %v", err)
	}
	writer.Close()
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close fixture project: %v", err)
	}
	projectKey, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	queries, err := appquery.NewGraphQueryService(root, []project.CaidoProjectRef{ref}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer queries.Close(ctx)
	consultations, err := consultation.NewService(root, []project.CaidoProjectRef{ref}, queries, queries)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer consultations.Close()
	api := &consultationServiceHTTPAPI{
		evidenceServiceAPI: &evidenceServiceAPI{explorerAPIFixture: &explorerAPIFixture{}, service: queries},
		consultations:      consultations,
	}
	handler, err := NewHandler(api)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	createBody, err := json.Marshal(map[string]any{
		"project_key": projectKey, "question": "Review this selected synthetic exchange.",
		"selections": []appquery.EvidencePacketSelection{{
			RequestID: exchange.Ref.RequestID, RevisionID: revisionID, Side: "request", Length: len(requestBytes),
		}},
		"idempotency_key": "operator_http_create_0001",
	})
	if err != nil {
		t.Fatalf("encode session creation: %v", err)
	}
	createdResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/session", createBody)
	if createdResponse.Code != http.StatusOK || createdResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create session status=%d cache=%q body=%s", createdResponse.Code, createdResponse.Header().Get("Cache-Control"), createdResponse.Body.String())
	}
	var created consultation.Session
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create session: %v", err)
	}
	if created.Question != "" || created.Evidence.Evidence[0].Excerpt != "" {
		t.Fatal("creation response exposed unreviewed question or evidence")
	}

	reviewResponse := serveLocalGET(handler, "/api/consultation/review?project_key="+projectKey+"&session_id="+created.ID)
	if reviewResponse.Code != http.StatusOK || reviewResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("review status=%d cache=%q body=%s", reviewResponse.Code, reviewResponse.Header().Get("Cache-Control"), reviewResponse.Body.String())
	}
	var review consultation.Session
	if err := json.Unmarshal(reviewResponse.Body.Bytes(), &review); err != nil {
		t.Fatalf("decode review session: %v", err)
	}
	if review.Question == "" || review.ReviewAccepted || len(review.Evidence.Evidence) != 1 ||
		!review.Evidence.Evidence[0].Available || review.Evidence.Evidence[0].RedactedBytes == 0 ||
		!strings.Contains(review.Evidence.Evidence[0].Excerpt, "[REDACTED]") ||
		strings.Contains(reviewResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("operator review did not show the redacted exact evidence: %#v", review)
	}

	acceptBody, err := json.Marshal(map[string]any{
		"project_key": projectKey, "session_id": created.ID, "expected_version": 1,
		"idempotency_key": "operator_http_accept_0001",
	})
	if err != nil {
		t.Fatalf("encode review acceptance: %v", err)
	}
	acceptedResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/accept-review", acceptBody)
	if acceptedResponse.Code != http.StatusOK || acceptedResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("accept review status=%d cache=%q body=%s", acceptedResponse.Code, acceptedResponse.Header().Get("Cache-Control"), acceptedResponse.Body.String())
	}
	var accepted consultation.Session
	if err := json.Unmarshal(acceptedResponse.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accepted session: %v", err)
	}
	if !accepted.ReviewAccepted || accepted.Version != 2 || strings.Contains(acceptedResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("explicit review transition did not persist or returned raw evidence: %#v", accepted)
	}

	resumeResponse := serveLocalGET(handler, "/api/consultation/session?project_key="+projectKey+"&session_id="+created.ID)
	if resumeResponse.Code != http.StatusOK || resumeResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resume status=%d cache=%q body=%s", resumeResponse.Code, resumeResponse.Header().Get("Cache-Control"), resumeResponse.Body.String())
	}
	var resumed consultation.Session
	if err := json.Unmarshal(resumeResponse.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode resumed session: %v", err)
	}
	if !resumed.ReviewAccepted || resumed.Question == "" || resumed.Evidence.Evidence[0].RedactedBytes == 0 ||
		strings.Contains(resumeResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("accepted session did not resume with redacted exact evidence: %#v", resumed)
	}
}

func serveLocalJSON(handler http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1"+target, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
