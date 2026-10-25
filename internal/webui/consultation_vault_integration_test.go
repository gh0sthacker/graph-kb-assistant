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
	secondRequestBytes := []byte("GET /profile?view=compact HTTP/1.1\r\nHost: app.fixture.invalid\r\n\r\n")
	secondResponseBytes := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nsynthetic compact profile")
	status := 200
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: "review-capture"},
		RequestRaw: requestBytes, ResponseRaw: responseBytes, HasResponse: true,
		Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
		Target: "https://app.fixture.invalid/profile", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	secondExchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: "review-capture-two"},
		RequestRaw: secondRequestBytes, ResponseRaw: secondResponseBytes, HasResponse: true,
		Method: "GET", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
		Target: "https://app.fixture.invalid/profile?view=compact", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	if _, err := writer.WritePage(ctx, ingest.HistoryPageInput{RunID: run.RunID, Page: caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange, secondExchange}}}); err != nil {
		writer.Close()
		_ = projectStore.Close()
		t.Fatalf("WritePage: %v", err)
	}
	var revisionID, secondRevisionID string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", exchange.Ref.RequestID,
	).Scan(&revisionID); err != nil {
		writer.Close()
		_ = projectStore.Close()
		t.Fatalf("read exact fixture revision: %v", err)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE request_id=?", secondExchange.Ref.RequestID,
	).Scan(&secondRevisionID); err != nil {
		writer.Close()
		_ = projectStore.Close()
		t.Fatalf("read second exact fixture revision: %v", err)
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
		"selections": []appquery.EvidencePacketSelection{
			{RequestID: exchange.Ref.RequestID, RevisionID: revisionID, Side: "request", Length: len(requestBytes)},
			{RequestID: secondExchange.Ref.RequestID, RevisionID: secondRevisionID, Side: "request", Length: len(secondRequestBytes)},
		},
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
	if len(created.References) != 2 {
		t.Fatalf("created evidence references=%d, want both exact selected revisions", len(created.References))
	}
	annotationBody, err := json.Marshal(map[string]any{
		"project_key": projectKey, "session_id": created.ID, "expected_version": 1,
		"idempotency_key": "operator_http_context_0001", "kind": "actor", "label": "actor-a",
		"evidence_reference_ids": []string{created.References[0].ID},
	})
	if err != nil {
		t.Fatalf("encode analyst context: %v", err)
	}
	annotationResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/context-annotation", annotationBody)
	if annotationResponse.Code != http.StatusOK || annotationResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("context annotation status=%d cache=%q body=%s", annotationResponse.Code, annotationResponse.Header().Get("Cache-Control"), annotationResponse.Body.String())
	}
	var annotation consultation.ContextAnnotation
	if err := json.Unmarshal(annotationResponse.Body.Bytes(), &annotation); err != nil {
		t.Fatalf("decode analyst context: %v", err)
	}
	if annotation.Kind != "actor" || annotation.Label != "actor-a" || annotation.Origin != "analyst" ||
		annotation.EvidenceState != "analyst_provided" || len(annotation.EvidenceReferenceIDs) != 1 ||
		annotation.EvidenceReferenceIDs[0] != created.References[0].ID {
		t.Fatalf("context annotation lost its provenance/reference: %#v", annotation)
	}

	reviewResponse := serveLocalGET(handler, "/api/consultation/review?project_key="+projectKey+"&session_id="+created.ID)
	if reviewResponse.Code != http.StatusOK || reviewResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("review status=%d cache=%q body=%s", reviewResponse.Code, reviewResponse.Header().Get("Cache-Control"), reviewResponse.Body.String())
	}
	var review consultation.Session
	if err := json.Unmarshal(reviewResponse.Body.Bytes(), &review); err != nil {
		t.Fatalf("decode review session: %v", err)
	}
	if review.Question == "" || review.ReviewAccepted || len(review.Evidence.Evidence) != 2 || len(review.ContextAnnotations) != 1 ||
		review.ContextAnnotations[0].Label != "actor-a" || review.ContextAnnotations[0].Origin != "analyst" ||
		!review.Evidence.Evidence[0].Available || review.Evidence.Evidence[0].RedactedBytes == 0 ||
		!strings.Contains(review.Evidence.Evidence[0].Excerpt, "[REDACTED]") ||
		strings.Contains(reviewResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("operator review did not show the redacted exact evidence: %#v", review)
	}

	acceptBody, err := json.Marshal(map[string]any{
		"project_key": projectKey, "session_id": created.ID, "expected_version": 2,
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
	if !accepted.ReviewAccepted || accepted.Version != 3 || len(accepted.ContextAnnotations) != 1 || strings.Contains(acceptedResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("explicit review transition did not persist or returned raw evidence: %#v", accepted)
	}
	compareBody, err := json.Marshal(map[string]any{
		"project_key": projectKey, "session_id": created.ID, "expected_version": accepted.Version,
		"idempotency_key": "operator_http_compare_0001",
		"left_request_id": exchange.Ref.RequestID, "left_revision_id": revisionID,
		"right_request_id": secondExchange.Ref.RequestID, "right_revision_id": secondRevisionID,
	})
	if err != nil {
		t.Fatalf("encode session comparison: %v", err)
	}
	compareResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/compare", compareBody)
	if compareResponse.Code != http.StatusOK || compareResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("consultation comparison status=%d cache=%q body=%s", compareResponse.Code, compareResponse.Header().Get("Cache-Control"), compareResponse.Body.String())
	}
	var comparison consultation.ComparisonRecord
	if err := json.Unmarshal(compareResponse.Body.Bytes(), &comparison); err != nil {
		t.Fatalf("decode session comparison: %v", err)
	}
	if comparison.ID == "" || comparison.SessionVersion != 4 || comparison.Result.LeftRequestID != exchange.Ref.RequestID ||
		comparison.Result.LeftRevisionID != revisionID || comparison.Result.RightRequestID != secondExchange.Ref.RequestID ||
		comparison.Result.RightRevisionID != secondRevisionID || comparison.Result.Request.FirstDifferenceOffset == nil ||
		comparison.Result.Request.LeftExcerpt != "" || comparison.Result.Request.RightExcerpt != "" ||
		strings.Contains(compareResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("persisted comparison lost exact capture identity or exposed excerpts: %#v", comparison)
	}

	proposal := consultation.CandidateProposal{
		Title: "Possible account response difference", Summary: "The two synthetic captures show different response behavior.",
		Rationale: "The exact comparison is tied to both selected revisions.", Impact: "Potentially exposes account-specific information.",
		Alternatives: []string{"The difference may be an expected presentation preference."},
		NextChecks:   []string{"Repeat the comparison with a second authorized actor."},
		Limitations:  []string{"Only two synthetic captures are represented."},
	}
	currentSessionVersion := comparison.SessionVersion
	proposalBody := func(expectedVersion int, idempotencyKey string, evidenceIDs []string) []byte {
		body, marshalErr := json.Marshal(consultation.RecordProposalRequest{
			ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: expectedVersion,
			IdempotencyKey: idempotencyKey, Proposal: proposal,
			EvidenceReferenceIDs: evidenceIDs, ComparisonIDs: []string{comparison.ID},
		})
		if marshalErr != nil {
			t.Fatalf("encode candidate proposal: %v", marshalErr)
		}
		return body
	}
	staleProposalResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/proposal",
		proposalBody(currentSessionVersion-1, "operator_http_proposal_stale_0001", []string{created.References[0].ID}))
	if staleProposalResponse.Code != http.StatusConflict {
		t.Fatalf("stale proposal version status=%d body=%s, want conflict", staleProposalResponse.Code, staleProposalResponse.Body.String())
	}

	outsideReferenceID := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xee}, 32))
	outsideProposalResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/proposal",
		proposalBody(currentSessionVersion, "operator_http_proposal_outside_0001", []string{outsideReferenceID}))
	if outsideProposalResponse.Code != http.StatusOK || strings.Contains(outsideProposalResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("untrusted proposal was not safely recorded for validation: status=%d body=%s", outsideProposalResponse.Code, outsideProposalResponse.Body.String())
	}
	var outsideProposal consultation.ProposalRecord
	if err := json.Unmarshal(outsideProposalResponse.Body.Bytes(), &outsideProposal); err != nil {
		t.Fatalf("decode untrusted proposal: %v", err)
	}
	outsideValidationBody, err := json.Marshal(consultation.ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: outsideProposal.SessionVersion,
		IdempotencyKey: "operator_http_validate_outside_0001", ProposalID: outsideProposal.ID,
	})
	if err != nil {
		t.Fatalf("encode outside-reference validation: %v", err)
	}
	outsideValidationResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/validate", outsideValidationBody)
	if outsideValidationResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("outside-session citation validation status=%d body=%s, want rejection", outsideValidationResponse.Code, outsideValidationResponse.Body.String())
	}

	validProposalResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/proposal",
		proposalBody(outsideProposal.SessionVersion, "operator_http_proposal_valid_0001", []string{created.References[0].ID, created.References[1].ID}))
	if validProposalResponse.Code != http.StatusOK || strings.Contains(validProposalResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("valid proposal status=%d body=%s", validProposalResponse.Code, validProposalResponse.Body.String())
	}
	var savedProposal consultation.ProposalRecord
	if err := json.Unmarshal(validProposalResponse.Body.Bytes(), &savedProposal); err != nil {
		t.Fatalf("decode valid proposal: %v", err)
	}
	interruptedResponse := serveLocalGET(handler, "/api/consultation/review?project_key="+projectKey+"&session_id="+created.ID)
	if interruptedResponse.Code != http.StatusOK {
		t.Fatalf("resume after proposal status=%d body=%s", interruptedResponse.Code, interruptedResponse.Body.String())
	}
	var interrupted consultation.Session
	if err := json.Unmarshal(interruptedResponse.Body.Bytes(), &interrupted); err != nil {
		t.Fatalf("decode resumed proposal session: %v", err)
	}
	if interrupted.Version != savedProposal.SessionVersion || len(interrupted.Proposals) != 2 || len(interrupted.Receipts) != 0 ||
		interrupted.Proposals[1].ID != savedProposal.ID || len(interrupted.Proposals[1].EvidenceReferenceIDs) != 2 {
		t.Fatalf("proposal was not recoverable after an interrupted workflow: %#v", interrupted)
	}
	validationBody, err := json.Marshal(consultation.ValidateProposalRequest{
		ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: interrupted.Version,
		IdempotencyKey: "operator_http_validate_valid_0001", ProposalID: savedProposal.ID,
	})
	if err != nil {
		t.Fatalf("encode proposal validation: %v", err)
	}
	validationResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/validate", validationBody)
	if validationResponse.Code != http.StatusOK || strings.Contains(validationResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("proposal validation status=%d body=%s", validationResponse.Code, validationResponse.Body.String())
	}
	var receipt consultation.ValidationReceipt
	if err := json.Unmarshal(validationResponse.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode validation receipt: %v", err)
	}
	if receipt.Status != consultation.ValidationReferencesValid || receipt.ProposalID != savedProposal.ID ||
		len(receipt.EvidenceReferenceIDs) != 2 || len(receipt.ComparisonIDs) != 1 || receipt.ComparisonIDs[0] != comparison.ID {
		t.Fatalf("validation receipt lost its exact references: %#v", receipt)
	}
	validatedResumeResponse := serveLocalGET(handler, "/api/consultation/session?project_key="+projectKey+"&session_id="+created.ID)
	if validatedResumeResponse.Code != http.StatusOK {
		t.Fatalf("resume after validation status=%d body=%s", validatedResumeResponse.Code, validatedResumeResponse.Body.String())
	}
	var validatedResume consultation.Session
	if err := json.Unmarshal(validatedResumeResponse.Body.Bytes(), &validatedResume); err != nil {
		t.Fatalf("decode resumed validation receipt: %v", err)
	}
	if validatedResume.Version != receipt.SessionVersion || len(validatedResume.Receipts) != 1 ||
		validatedResume.Receipts[0].ID != receipt.ID || validatedResume.Receipts[0].Status != consultation.ValidationReferencesValid {
		t.Fatalf("valid receipt was not recoverable after an interrupted workflow: %#v", validatedResume)
	}

	candidateBody := func(expectedVersion int, idempotencyKey string) []byte {
		body, marshalErr := json.Marshal(consultation.SaveCandidateRequest{
			ProjectKey: projectKey, SessionID: created.ID, ExpectedVersion: expectedVersion,
			IdempotencyKey: idempotencyKey, ProposalID: savedProposal.ID, ReceiptID: receipt.ID,
		})
		if marshalErr != nil {
			t.Fatalf("encode candidate save: %v", marshalErr)
		}
		return body
	}
	staleCandidateResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/candidate",
		candidateBody(validatedResume.Version-1, "operator_http_candidate_stale_0001"))
	if staleCandidateResponse.Code != http.StatusConflict {
		t.Fatalf("stale candidate version status=%d body=%s, want conflict", staleCandidateResponse.Code, staleCandidateResponse.Body.String())
	}
	candidateResponse := serveLocalJSON(handler, http.MethodPost, "/api/consultation/candidate",
		candidateBody(validatedResume.Version, "operator_http_candidate_save_0001"))
	if candidateResponse.Code != http.StatusOK || strings.Contains(candidateResponse.Body.String(), "operator-review-secret") {
		t.Fatalf("candidate save status=%d body=%s", candidateResponse.Code, candidateResponse.Body.String())
	}
	var candidate consultation.CandidateVersion
	if err := json.Unmarshal(candidateResponse.Body.Bytes(), &candidate); err != nil {
		t.Fatalf("decode candidate: %v", err)
	}
	if candidate.State != "candidate" || candidate.ProposalID != savedProposal.ID || candidate.ReceiptID != receipt.ID ||
		len(candidate.Citations) != 2 || len(candidate.ComparisonIDs) != 1 || candidate.ComparisonIDs[0] != comparison.ID ||
		candidate.Citations[0].EvidenceReferenceID != created.References[0].ID || candidate.Citations[1].EvidenceReferenceID != created.References[1].ID {
		t.Fatalf("candidate lost its validated citations or candidate state: %#v", candidate)
	}

	resumeResponse := serveLocalGET(handler, "/api/consultation/session?project_key="+projectKey+"&session_id="+created.ID)
	if resumeResponse.Code != http.StatusOK || resumeResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resume status=%d cache=%q body=%s", resumeResponse.Code, resumeResponse.Header().Get("Cache-Control"), resumeResponse.Body.String())
	}
	var resumed consultation.Session
	if err := json.Unmarshal(resumeResponse.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode resumed session: %v", err)
	}
	if !resumed.ReviewAccepted || resumed.Question == "" || len(resumed.ContextAnnotations) != 1 || len(resumed.Comparisons) != 1 ||
		resumed.ContextAnnotations[0].Label != "actor-a" || resumed.Evidence.Evidence[0].RedactedBytes == 0 ||
		resumed.Comparisons[0].ID != comparison.ID || resumed.Version != candidate.SessionVersion ||
		len(resumed.Proposals) != 2 || len(resumed.Receipts) != 1 || len(resumed.Candidates) != 1 ||
		resumed.Candidates[0].State != "candidate" || resumed.Candidates[0].CandidateID != candidate.CandidateID ||
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
