package mcpserver

import (
	"context"
	"encoding/json"
	"errors"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/consultation"
)

type consultationCreateArgs struct {
	ProjectKey     string                             `json:"project_key"`
	Question       string                             `json:"question"`
	Selections     []appquery.EvidencePacketSelection `json:"selections"`
	IdempotencyKey string                             `json:"idempotency_key"`
}

type consultationCompareArgs struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ConsultationID  string `json:"consultation_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	LeftRequestID   string `json:"left_request_id"`
	LeftRevisionID  string `json:"left_revision_id"`
	RightRequestID  string `json:"right_request_id"`
	RightRevisionID string `json:"right_revision_id"`
}

type consultationProposalArgs struct {
	ProjectKey           string                         `json:"project_key"`
	SessionID            string                         `json:"session_id"`
	ConsultationID       string                         `json:"consultation_id"`
	ExpectedVersion      int                            `json:"expected_version"`
	IdempotencyKey       string                         `json:"idempotency_key"`
	Proposal             consultation.CandidateProposal `json:"proposal"`
	EvidenceReferenceIDs []string                       `json:"evidence_reference_ids"`
	ComparisonIDs        []string                       `json:"comparison_ids"`
}

type consultationValidateArgs struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ConsultationID  string `json:"consultation_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProposalID      string `json:"proposal_id"`
}

type candidateSaveArgs struct {
	ProjectKey      string `json:"project_key"`
	SessionID       string `json:"session_id"`
	ConsultationID  string `json:"consultation_id"`
	ExpectedVersion int    `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProposalID      string `json:"proposal_id"`
	ReceiptID       string `json:"receipt_id"`
	CandidateID     string `json:"candidate_id,omitempty"`
}

type consultationResumeArgs struct {
	ProjectKey     string `json:"project_key"`
	SessionID      string `json:"session_id"`
	ConsultationID string `json:"consultation_id"`
}

func resolveConsultationSessionID(sessionID, consultationID string) (string, error) {
	if sessionID != "" && consultationID != "" && sessionID != consultationID {
		return "", errors.New("session_id and consultation_id must match when both are provided")
	}
	if sessionID != "" {
		return sessionID, nil
	}
	if consultationID != "" {
		return consultationID, nil
	}
	return "", errors.New("either session_id or consultation_id is required")
}

func (t *Tools) CreateSession(ctx context.Context, request appquery.EvidencePacketRequest, idempotencyKey string) (consultation.Session, error) {
	if t == nil || t.consultations == nil {
		return consultation.Session{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.CreateSession(ctx, request, idempotencyKey)
}

func (t *Tools) Resume(ctx context.Context, projectKey, sessionID string) (consultation.Session, error) {
	if t == nil || t.consultations == nil {
		return consultation.Session{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.Resume(ctx, projectKey, sessionID)
}

// ReviewSession is consumed only by the same-process loopback web UI. It is
// intentionally absent from the MCP tool catalog.
func (t *Tools) ReviewSession(ctx context.Context, projectKey, sessionID string) (consultation.Session, error) {
	if t == nil || t.consultations == nil {
		return consultation.Session{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.ReviewSession(ctx, projectKey, sessionID)
}

// AcceptReview is a local-operator transition exposed to the web UI only.
func (t *Tools) AcceptReview(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey string) (consultation.Session, error) {
	if t == nil || t.consultations == nil {
		return consultation.Session{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.AcceptReview(ctx, projectKey, sessionID, expectedVersion, idempotencyKey)
}

// RecordContextAnnotation is available to the same-process local web UI; the
// annotation remains contextual metadata and is not exposed as an MCP write.
func (t *Tools) RecordContextAnnotation(ctx context.Context, request consultation.RecordContextAnnotationRequest) (consultation.ContextAnnotation, error) {
	if t == nil || t.consultations == nil {
		return consultation.ContextAnnotation{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.RecordContextAnnotation(ctx, request)
}

// ListOperationAliases exposes project-local analyst labels to the web UI.
func (t *Tools) ListOperationAliases(ctx context.Context, projectKey string) ([]consultation.OperationAlias, error) {
	if t == nil || t.consultations == nil {
		return nil, errors.New("consultation service is unavailable")
	}
	return t.consultations.ListOperationAliases(ctx, projectKey)
}

// SaveOperationAlias is a project-local web UI write; it is intentionally not
// part of the default MCP tool catalog.
func (t *Tools) SaveOperationAlias(ctx context.Context, request consultation.SaveOperationAliasRequest) (consultation.OperationAlias, error) {
	if t == nil || t.consultations == nil {
		return consultation.OperationAlias{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.SaveOperationAlias(ctx, request)
}

func (t *Tools) Compare(ctx context.Context, projectKey, sessionID string, expectedVersion int, idempotencyKey string, request appquery.CaptureComparisonRequest) (consultation.ComparisonRecord, error) {
	if t == nil || t.consultations == nil {
		return consultation.ComparisonRecord{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.Compare(ctx, projectKey, sessionID, expectedVersion, idempotencyKey, request)
}

func (t *Tools) RecordProposal(ctx context.Context, request consultation.RecordProposalRequest) (consultation.ProposalRecord, error) {
	if t == nil || t.consultations == nil {
		return consultation.ProposalRecord{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.RecordProposal(ctx, request)
}

func (t *Tools) ValidateProposal(ctx context.Context, request consultation.ValidateProposalRequest) (consultation.ValidationReceipt, error) {
	if t == nil || t.consultations == nil {
		return consultation.ValidationReceipt{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.ValidateProposal(ctx, request)
}

func (t *Tools) SaveCandidate(ctx context.Context, request consultation.SaveCandidateRequest) (consultation.CandidateVersion, error) {
	if t == nil || t.consultations == nil {
		return consultation.CandidateVersion{}, errors.New("consultation service is unavailable")
	}
	return t.consultations.SaveCandidate(ctx, request)
}

func (t *Tools) callConsultation(ctx context.Context, name string, encoded json.RawMessage) (any, error) {
	if t == nil || t.consultations == nil {
		return nil, errors.New("consultation service is unavailable")
	}
	switch name {
	case "bb_consult_create_session":
		var args consultationCreateArgs
		if err := decodeObject(encoded, &args); err != nil {
			return nil, err
		}
		return t.CreateSession(ctx, appquery.EvidencePacketRequest{
			ProjectKey: args.ProjectKey, Question: args.Question, Selections: args.Selections,
		}, args.IdempotencyKey)
	case "bb_consult_compare":
		var args consultationCompareArgs
		if err := decodeObject(encoded, &args); err != nil {
			return nil, err
		}
		sessionID, err := resolveConsultationSessionID(args.SessionID, args.ConsultationID)
		if err != nil {
			return nil, err
		}
		return t.Compare(ctx, args.ProjectKey, sessionID, args.ExpectedVersion, args.IdempotencyKey, appquery.CaptureComparisonRequest{
			ProjectKey: args.ProjectKey, LeftRequestID: args.LeftRequestID, LeftRevisionID: args.LeftRevisionID,
			RightRequestID: args.RightRequestID, RightRevisionID: args.RightRevisionID,
		})
	case "bb_consult_record_proposal":
		var args consultationProposalArgs
		if err := decodeObject(encoded, &args); err != nil {
			return nil, err
		}
		sessionID, err := resolveConsultationSessionID(args.SessionID, args.ConsultationID)
		if err != nil {
			return nil, err
		}
		return t.RecordProposal(ctx, consultation.RecordProposalRequest{
			ProjectKey: args.ProjectKey, SessionID: sessionID, ExpectedVersion: args.ExpectedVersion,
			IdempotencyKey: args.IdempotencyKey, Proposal: args.Proposal,
			EvidenceReferenceIDs: args.EvidenceReferenceIDs, ComparisonIDs: args.ComparisonIDs,
		})
	case "bb_consult_validate_proposal":
		var args consultationValidateArgs
		if err := decodeObject(encoded, &args); err != nil {
			return nil, err
		}
		sessionID, err := resolveConsultationSessionID(args.SessionID, args.ConsultationID)
		if err != nil {
			return nil, err
		}
		return t.ValidateProposal(ctx, consultation.ValidateProposalRequest{
			ProjectKey: args.ProjectKey, SessionID: sessionID, ExpectedVersion: args.ExpectedVersion,
			IdempotencyKey: args.IdempotencyKey, ProposalID: args.ProposalID,
		})
	case "bb_candidate_save":
		var args candidateSaveArgs
		if err := decodeObject(encoded, &args); err != nil {
			return nil, err
		}
		sessionID, err := resolveConsultationSessionID(args.SessionID, args.ConsultationID)
		if err != nil {
			return nil, err
		}
		return t.SaveCandidate(ctx, consultation.SaveCandidateRequest{
			ProjectKey: args.ProjectKey, SessionID: sessionID, ExpectedVersion: args.ExpectedVersion,
			IdempotencyKey: args.IdempotencyKey, ProposalID: args.ProposalID, ReceiptID: args.ReceiptID, CandidateID: args.CandidateID,
		})
	case "bb_consult_resume":
		var args consultationResumeArgs
		if err := decodeObject(encoded, &args); err != nil {
			return nil, err
		}
		sessionID, err := resolveConsultationSessionID(args.SessionID, args.ConsultationID)
		if err != nil {
			return nil, err
		}
		return t.Resume(ctx, args.ProjectKey, sessionID)
	default:
		return nil, errors.New("unknown consultation tool")
	}
}

func consultationToolCatalog() []toolSpec {
	objectSchema := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	stringSchema := func(max int, extra map[string]any) map[string]any {
		value := map[string]any{"type": "string", "maxLength": max}
		for key, item := range extra {
			value[key] = item
		}
		return value
	}
	projectKey := stringSchema(64, map[string]any{"minLength": 64, "pattern": "^[a-f0-9]{64}$"})
	opaqueID := stringSchema(43, map[string]any{"minLength": 43, "pattern": "^[A-Za-z0-9_-]{43}$"})
	revisionID := stringSchema(64, map[string]any{"minLength": 64, "pattern": "^[a-f0-9]{64}$"})
	idempotencyKey := stringSchema(128, map[string]any{"minLength": 16, "pattern": "^[A-Za-z0-9_-]{16,128}$"})
	version := map[string]any{"type": "integer", "minimum": 1}
	sessionIDSchema := func(properties map[string]any, required ...string) map[string]any {
		properties["session_id"] = opaqueID
		properties["consultation_id"] = opaqueID
		schema := objectSchema(properties, required...)
		schema["anyOf"] = []any{
			map[string]any{"required": []string{"session_id"}},
			map[string]any{"required": []string{"consultation_id"}},
		}
		return schema
	}
	sessionAliasDescription := " Use either `session_id` or `consultation_id`; if both are supplied, they must match."
	selection := objectSchema(map[string]any{
		"request_id":  stringSchema(512, map[string]any{"minLength": 1}),
		"revision_id": revisionID,
		"side":        map[string]any{"type": "string", "enum": []string{"request", "response"}},
		"offset":      map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483648},
		"length":      map[string]any{"type": "integer", "minimum": 1, "maximum": appquery.MaximumEvidencePacketSpanBytes},
	}, "request_id", "revision_id", "side", "offset", "length")
	proposal := objectSchema(map[string]any{
		"title":        stringSchema(160, map[string]any{"minLength": 1}),
		"summary":      stringSchema(2048, map[string]any{"minLength": 1}),
		"rationale":    stringSchema(4096, map[string]any{"minLength": 1}),
		"impact":       stringSchema(1024, nil),
		"alternatives": map[string]any{"type": "array", "maxItems": 5, "items": stringSchema(1024, nil)},
		"next_checks":  map[string]any{"type": "array", "maxItems": 8, "items": stringSchema(512, nil)},
		"limitations":  map[string]any{"type": "array", "maxItems": 8, "items": stringSchema(512, nil)},
	}, "title", "summary", "rationale")
	write := func() map[string]any {
		return map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	}
	read := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	return []toolSpec{
		{Name: "bb_consult_create_session", Title: "Start a local consultation",
			Description: "Create or idempotently resume a project-scoped consultation from one question and up to four selected historical evidence spans. The response includes the same session identifier as `consultation_id` and `session_id`; `content_withheld` and `withheld_fields` indicate hidden content until the operator reviews and accepts it in the local web UI. No model call is made.",
			InputSchema: objectSchema(map[string]any{
				"project_key": projectKey, "question": stringSchema(appquery.MaximumEvidencePacketQuestion, map[string]any{"minLength": 1}),
				"selections":      map[string]any{"type": "array", "minItems": 1, "maxItems": appquery.MaximumEvidencePacketSpans, "items": selection},
				"idempotency_key": idempotencyKey,
			}, "project_key", "question", "selections", "idempotency_key"), Annotations: write()},
		{Name: "bb_consult_compare", Title: "Compare exact captures in a consultation",
			Description: "Append a deterministic comparison for two exact revisions already represented by verified evidence in this consultation. Raw request/response excerpts are removed before returning or persisting the result." + sessionAliasDescription,
			InputSchema: sessionIDSchema(map[string]any{
				"project_key": projectKey, "expected_version": version, "idempotency_key": idempotencyKey,
				"left_request_id": stringSchema(512, map[string]any{"minLength": 1}), "left_revision_id": revisionID,
				"right_request_id": stringSchema(512, map[string]any{"minLength": 1}), "right_revision_id": revisionID,
			}, "project_key", "expected_version", "idempotency_key", "left_request_id", "left_revision_id", "right_request_id", "right_revision_id"), Annotations: write()},
		{Name: "bb_consult_record_proposal", Title: "Save an unverified consultation proposal",
			Description: "Persist a redacted, untrusted interpretation with opaque evidence/comparison IDs. The proposal remains a candidate draft until a separate exact-reference validation receipt is created; recording it does not confirm a vulnerability." + sessionAliasDescription,
			InputSchema: sessionIDSchema(map[string]any{
				"project_key": projectKey, "expected_version": version, "idempotency_key": idempotencyKey, "proposal": proposal,
				"evidence_reference_ids": map[string]any{"type": "array", "maxItems": 8, "uniqueItems": true, "items": opaqueID},
				"comparison_ids":         map[string]any{"type": "array", "maxItems": 16, "uniqueItems": true, "items": opaqueID},
			}, "project_key", "expected_version", "idempotency_key", "proposal", "evidence_reference_ids", "comparison_ids"), Annotations: write()},
		{Name: "bb_consult_validate_proposal", Title: "Validate exact proposal references",
			Description: "Re-resolve the cited exact redacted spans and re-run cited comparisons against their same request/revision IDs. Returns a receipt for reference identity and integrity only; it does not prove semantic support, impact, or vulnerability." + sessionAliasDescription,
			InputSchema: sessionIDSchema(map[string]any{
				"project_key": projectKey, "expected_version": version, "idempotency_key": idempotencyKey, "proposal_id": opaqueID,
			}, "project_key", "expected_version", "idempotency_key", "proposal_id"), Annotations: write()},
		{Name: "bb_candidate_save", Title: "Save an immutable candidate version",
			Description: "Create a candidate or append an immutable version to the supplied candidate_id only when a matching citation-validation receipt exists. Each new candidate starts in candidate state; this operation never marks a candidate reproduced or verified." + sessionAliasDescription,
			InputSchema: sessionIDSchema(map[string]any{
				"project_key": projectKey, "expected_version": version, "idempotency_key": idempotencyKey,
				"proposal_id": opaqueID, "receipt_id": opaqueID, "candidate_id": opaqueID,
			}, "project_key", "expected_version", "idempotency_key", "proposal_id", "receipt_id"), Annotations: write()},
		{Name: "bb_consult_resume", Title: "Resume a local consultation",
			Description: "Read one saved project-scoped consultation and its immutable comparisons, proposals, validation receipts, and candidate versions after an application restart. Before review, `content_withheld` and `withheld_fields` identify hidden content; stored evidence remains redacted." + sessionAliasDescription,
			InputSchema: sessionIDSchema(map[string]any{"project_key": projectKey}, "project_key"), Annotations: read},
	}
}
