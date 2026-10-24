package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/consultation"
	"bb-graph-assistant/internal/publicdata"
)

const maximumConsultationRequestBytes = 96 << 10

var consultationSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type consultationCreateRequest struct {
	ProjectKey     string                             `json:"project_key"`
	Question       string                             `json:"question"`
	Selections     []appquery.EvidencePacketSelection `json:"selections"`
	IdempotencyKey string                             `json:"idempotency_key"`
}

type consultationCompareRequest struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	LeftRequestID   string `json:"left_request_id"`
	LeftRevisionID  string `json:"left_revision_id"`
	RightRequestID  string `json:"right_request_id"`
	RightRevisionID string `json:"right_revision_id"`
}

type consultationProposalRequest struct {
	ProjectKey           string                         `json:"project_key"`
	SessionID            string                         `json:"session_id"`
	ExpectedVersion      int                            `json:"expected_version"`
	IdempotencyKey       string                         `json:"idempotency_key"`
	Proposal             consultation.CandidateProposal `json:"proposal"`
	EvidenceReferenceIDs []string                       `json:"evidence_reference_ids"`
	ComparisonIDs        []string                       `json:"comparison_ids"`
}

type consultationValidateRequest struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProposalID      string `json:"proposal_id"`
}

type consultationCandidateRequest struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProposalID      string `json:"proposal_id"`
	ReceiptID       string `json:"receipt_id"`
}

type consultationAcceptReviewRequest struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type consultationContextAnnotationRequest struct {
	ProjectKey           string   `json:"project_key"`
	SessionID            string   `json:"session_id"`
	ExpectedVersion      int      `json:"expected_version"`
	IdempotencyKey       string   `json:"idempotency_key"`
	Kind                 string   `json:"kind"`
	Label                string   `json:"label"`
	EvidenceReferenceIDs []string `json:"evidence_reference_ids"`
}

func consultationResumeHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if len(query) != 2 || len(query["project_key"]) != 1 || len(query["session_id"]) != 1 {
			writeError(w, http.StatusBadRequest, "consultation project and session identifiers are required")
			return
		}
		projectKey, sessionID := query.Get("project_key"), query.Get("session_id")
		if !projectKeyPattern.MatchString(projectKey) || !consultationSessionIDPattern.MatchString(sessionID) {
			writeError(w, http.StatusBadRequest, "consultation identifiers are invalid")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.Resume(ctx, projectKey, sessionID)
		writeConsultationResult(w, result, err)
	}
}

func consultationReviewHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if len(query) != 2 || len(query["project_key"]) != 1 || len(query["session_id"]) != 1 {
			writeError(w, http.StatusBadRequest, "consultation project and session identifiers are required")
			return
		}
		projectKey, sessionID := query.Get("project_key"), query.Get("session_id")
		if !projectKeyPattern.MatchString(projectKey) || !consultationSessionIDPattern.MatchString(sessionID) {
			writeError(w, http.StatusBadRequest, "consultation identifiers are invalid")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.ReviewSession(ctx, projectKey, sessionID)
		writeConsultationResult(w, result, err)
	}
}

func consultationCreateHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationCreateRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), graphTimeout)
		defer cancel()
		result, err := api.CreateSession(ctx, appquery.EvidencePacketRequest{
			ProjectKey: body.ProjectKey, Question: body.Question, Selections: body.Selections,
		}, body.IdempotencyKey)
		writeConsultationResult(w, result, err)
	}
}

func consultationAcceptReviewHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationAcceptReviewRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.AcceptReview(ctx, body.ProjectKey, body.SessionID, body.ExpectedVersion, body.IdempotencyKey)
		writeConsultationResult(w, result, err)
	}
}

func consultationContextAnnotationHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationContextAnnotationRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.RecordContextAnnotation(ctx, consultation.RecordContextAnnotationRequest{
			ProjectKey: body.ProjectKey, SessionID: body.SessionID, ExpectedVersion: body.ExpectedVersion,
			IdempotencyKey: body.IdempotencyKey, Kind: body.Kind, Label: body.Label,
			EvidenceReferenceIDs: body.EvidenceReferenceIDs,
		})
		writeConsultationResult(w, result, err)
	}
}

func consultationCompareHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationCompareRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), graphTimeout)
		defer cancel()
		result, err := api.Compare(ctx, body.ProjectKey, body.SessionID, body.ExpectedVersion, body.IdempotencyKey, appquery.CaptureComparisonRequest{
			ProjectKey: body.ProjectKey, LeftRequestID: body.LeftRequestID, LeftRevisionID: body.LeftRevisionID,
			RightRequestID: body.RightRequestID, RightRevisionID: body.RightRevisionID,
		})
		writeConsultationResult(w, result, err)
	}
}

func consultationProposalHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationProposalRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.RecordProposal(ctx, consultation.RecordProposalRequest{
			ProjectKey: body.ProjectKey, SessionID: body.SessionID, ExpectedVersion: body.ExpectedVersion,
			IdempotencyKey: body.IdempotencyKey, Proposal: body.Proposal,
			EvidenceReferenceIDs: body.EvidenceReferenceIDs, ComparisonIDs: body.ComparisonIDs,
		})
		writeConsultationResult(w, result, err)
	}
}

func consultationValidateHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationValidateRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), graphTimeout)
		defer cancel()
		result, err := api.ValidateProposal(ctx, consultation.ValidateProposalRequest{
			ProjectKey: body.ProjectKey, SessionID: body.SessionID, ExpectedVersion: body.ExpectedVersion,
			IdempotencyKey: body.IdempotencyKey, ProposalID: body.ProposalID,
		})
		writeConsultationResult(w, result, err)
	}
}

func consultationCandidateHandler(api consultationAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body consultationCandidateRequest
		if !decodeConsultationJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.SaveCandidate(ctx, consultation.SaveCandidateRequest{
			ProjectKey: body.ProjectKey, SessionID: body.SessionID, ExpectedVersion: body.ExpectedVersion,
			IdempotencyKey: body.IdempotencyKey, ProposalID: body.ProposalID, ReceiptID: body.ReceiptID,
		})
		writeConsultationResult(w, result, err)
	}
}

func decodeConsultationJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "consultation mutations require application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximumConsultationRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "consultation request is malformed or too large")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "consultation request must contain one JSON object")
		return false
	}
	return true
}

func writeConsultationResult(w http.ResponseWriter, result any, operationErr error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if operationErr != nil {
		writeConsultationError(w, operationErr)
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxGraphResponse || publicdata.ValidateJSON(encoded) != nil {
		writeError(w, http.StatusServiceUnavailable, "consultation response failed the local data validation gate")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func writeConsultationError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "consultation data is unavailable"
	switch {
	case errors.Is(err, consultation.ErrInvalidInput):
		status, message = http.StatusBadRequest, "consultation request is invalid"
	case errors.Is(err, consultation.ErrProjectNotRegistered), errors.Is(err, consultation.ErrSessionNotFound):
		status, message = http.StatusNotFound, "the selected consultation project or session was not found"
	case errors.Is(err, consultation.ErrSessionVersionConflict), errors.Is(err, consultation.ErrIdempotencyConflict):
		status, message = http.StatusConflict, "consultation state changed; reload the saved session before retrying"
	case errors.Is(err, consultation.ErrSessionAlreadyAccepted):
		status, message = http.StatusConflict, "accepted consultation sessions cannot be changed"
	case errors.Is(err, consultation.ErrReviewNotAccepted):
		status, message = http.StatusConflict, "review the redacted evidence locally before allowing assistant use"
	case errors.Is(err, consultation.ErrCitationInvalid), errors.Is(err, consultation.ErrReceiptInvalid):
		status, message = http.StatusUnprocessableEntity, "consultation references could not be validated"
	case errors.Is(err, consultation.ErrLimitReached):
		status, message = http.StatusRequestEntityTooLarge, "consultation storage limit was reached"
	case errors.Is(err, consultation.ErrEvidenceUnavailable), errors.Is(err, consultation.ErrComparisonUnavailable), errors.Is(err, consultation.ErrStoreUnavailable):
		status, message = http.StatusServiceUnavailable, "consultation evidence or local storage is unavailable"
	}
	writeError(w, status, message)
}
