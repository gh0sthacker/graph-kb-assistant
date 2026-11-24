package appquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/publicdata"
	"bb-graph-assistant/internal/store"
)

const (
	EvidencePacketPolicyVersion    = "bb-evidence-redaction-v1"
	MaximumEvidencePacketQuestion  = 2048
	MaximumEvidencePacketSpans     = 4
	MaximumEvidencePacketSpanBytes = 4096
	MaximumEvidencePacketBytes     = 8192
)

type EvidencePacketSelection struct {
	RequestID  string `json:"request_id"`
	RevisionID string `json:"revision_id"`
	Side       string `json:"side"`
	Offset     int64  `json:"offset"`
	Length     int    `json:"length"`
}

type EvidencePacketRequest struct {
	ProjectKey string                    `json:"project_key"`
	Question   string                    `json:"question"`
	Selections []EvidencePacketSelection `json:"selections"`
}

type EvidencePacketEvidence struct {
	Available             bool                           `json:"available"`
	OmittedReason         string                         `json:"omitted_reason,omitempty"`
	RequestID             string                         `json:"request_id"`
	RevisionID            string                         `json:"revision_id"`
	Side                  string                         `json:"side"`
	SpanStart             int64                          `json:"span_start"`
	SpanEnd               int64                          `json:"span_end"`
	MessageSize           *int64                         `json:"message_size,omitempty"`
	SelectedBytes         int                            `json:"selected_bytes"`
	IncludedBytes         int                            `json:"included_bytes"`
	RedactedBytes         int                            `json:"redacted_bytes"`
	OmittedBytes          *int64                         `json:"omitted_bytes,omitempty"`
	SHA256                string                         `json:"sha256,omitempty"`
	SourceIntegrityStatus string                         `json:"source_integrity_status"`
	CapturedAt            *string                        `json:"captured_at,omitempty"`
	ParseState            string                         `json:"parse_state"`
	WireFidelity          string                         `json:"wire_fidelity"`
	Truncated             *bool                          `json:"truncated,omitempty"`
	Excerpt               string                         `json:"excerpt,omitempty"`
	Redactions            []publicdata.TextRedactionSpan `json:"redactions"`
}

type EvidencePacket struct {
	Version              string                         `json:"version"`
	PolicyVersion        string                         `json:"policy_version"`
	ProjectKey           string                         `json:"project_key"`
	Question             string                         `json:"question"`
	QuestionRedactions   []publicdata.TextRedactionSpan `json:"question_redactions"`
	Evidence             []EvidencePacketEvidence       `json:"evidence"`
	SelectedBytes        int                            `json:"selected_bytes"`
	OmittedBytes         int64                          `json:"omitted_bytes"`
	OmittedBytesKnown    bool                           `json:"omitted_bytes_known"`
	PreviewOnly          bool                           `json:"preview_only"`
	OperatorReviewNeeded bool                           `json:"operator_review_needed"`
	CoverageNote         string                         `json:"coverage_note"`
}

// BuildEvidencePacket creates a redacted local preview from explicitly chosen
// byte spans in exact historical revisions. It never sends data to a model.
func (s *Service) BuildEvidencePacket(ctx context.Context, request EvidencePacketRequest) (EvidencePacket, error) {
	if s == nil {
		return EvidencePacket{}, queryError(ErrorEvidenceUnavailable, "evidence packet preview is unavailable", nil)
	}
	if ctx == nil || validateEvidencePacketRequest(request) != nil {
		return EvidencePacket{}, queryError(ErrorInvalidInput, "evidence packet request is invalid", nil)
	}
	question, questionRedactions, err := publicdata.RedactText(request.Question)
	if err != nil || strings.TrimSpace(question) == "" || publicdata.Validate(map[string]string{"question": question}) != nil {
		return EvidencePacket{}, queryError(ErrorInvalidInput, "evidence packet question is invalid after redaction", err)
	}
	ref, ok := s.projectRef(request.ProjectKey)
	if !ok {
		return EvidencePacket{}, queryError(ErrorProjectNotRegistered, "selected project is not in the local project catalog", nil)
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, s.dataRoot)
	if err != nil {
		return EvidencePacket{}, queryError(ErrorProjectUnavailable, "selected project is unavailable for local evidence reads", err)
	}
	defer selected.Close()

	packet := EvidencePacket{
		Version: "1", PolicyVersion: EvidencePacketPolicyVersion, ProjectKey: request.ProjectKey,
		Question: question, QuestionRedactions: questionRedactions,
		Evidence:    make([]EvidencePacketEvidence, 0, len(request.Selections)),
		PreviewOnly: true, OperatorReviewNeeded: true,
		OmittedBytesKnown: true,
		CoverageNote:      "Local preview only; no model call is made. Redaction is heuristic and requires operator review. Only selected exact historical spans are included; live data and unselected bytes are omitted.",
	}
	for _, selection := range request.Selections {
		if err := ctx.Err(); err != nil {
			return EvidencePacket{}, queryError(ErrorEvidenceUnavailable, "evidence packet preview was interrupted", err)
		}
		// Scan the complete bounded message before extracting the requested span;
		// otherwise a selection could begin inside an otherwise detectable secret.
		// The reader rejects larger objects before opening their vault files because
		// the current AES-GCM format authenticates and decrypts whole messages.
		raw, readErr := ingest.ReadCaptureRevisionForRedaction(ctx, selected, selection.RequestID, selection.RevisionID, selection.Side)
		if errors.Is(readErr, ingest.ErrVaultKeyUnavailable) {
			clear(raw.Data)
			return EvidencePacket{}, captureEvidenceReadError(readErr)
		}
		if errors.Is(readErr, ingest.ErrCaptureContextLimit) {
			item := buildEvidencePacketEvidence(selection, raw)
			clear(raw.Data)
			packet.appendEvidence(item)
			continue
		}
		if readErr != nil {
			clear(raw.Data)
			packet.appendEvidence(unavailablePacketEvidence(selection, "Exact local historical bytes are unavailable for the selected span."))
			continue
		}
		item := buildEvidencePacketEvidence(selection, raw)
		clear(raw.Data)
		packet.appendEvidence(item)
	}
	if err := publicdata.Validate(packet); err != nil {
		return EvidencePacket{}, queryError(ErrorEvidenceUnavailable, "redacted evidence packet failed the public-data validation gate", err)
	}
	if err := ctx.Err(); err != nil {
		return EvidencePacket{}, queryError(ErrorEvidenceUnavailable, "evidence packet preview was interrupted", err)
	}
	return packet, nil
}

func validateEvidencePacketRequest(request EvidencePacketRequest) error {
	if !projectKeyPattern.MatchString(request.ProjectKey) || !utf8.ValidString(request.Question) || strings.TrimSpace(request.Question) == "" ||
		len(request.Question) > MaximumEvidencePacketQuestion || len(request.Selections) < 1 || len(request.Selections) > MaximumEvidencePacketSpans {
		return errors.New("evidence packet request identity or size is invalid")
	}
	seen := make(map[string]struct{}, len(request.Selections))
	total := 0
	for _, selection := range request.Selections {
		if strings.TrimSpace(selection.RequestID) == "" || len(selection.RequestID) > 512 ||
			len(selection.RevisionID) != 64 || strings.Trim(selection.RevisionID, "0123456789abcdef") != "" ||
			(selection.Side != "request" && selection.Side != "response") || selection.Offset < 0 || selection.Offset > maximumEvidenceOffset ||
			selection.Length < 1 || selection.Length > MaximumEvidencePacketSpanBytes {
			return errors.New("evidence packet span is invalid")
		}
		total += selection.Length
		if total > MaximumEvidencePacketBytes {
			return errors.New("evidence packet exceeds the selected-byte budget")
		}
		identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", selection.RequestID, selection.RevisionID, selection.Side, selection.Offset, selection.Length)
		if _, exists := seen[identity]; exists {
			return errors.New("evidence packet contains a duplicate span")
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func buildEvidencePacketEvidence(selection EvidencePacketSelection, raw ingest.CaptureRange) EvidencePacketEvidence {
	item := EvidencePacketEvidence{
		RequestID: selection.RequestID, RevisionID: selection.RevisionID, Side: selection.Side,
		SpanStart: selection.Offset, SpanEnd: selection.Offset + int64(selection.Length),
		SelectedBytes: selection.Length, SourceIntegrityStatus: string(ingest.CaptureIntegrityUnavailable),
		Redactions: []publicdata.TextRedactionSpan{},
	}
	if raw.RevisionID == nil || *raw.RevisionID != selection.RevisionID {
		item.OmittedReason = "The exact selected revision could not be resolved."
		return item
	}
	item.MessageSize = int64Pointer(raw.TotalSize)
	item.SHA256 = raw.SHA256
	item.SourceIntegrityStatus = string(raw.IntegrityStatus)
	if item.SourceIntegrityStatus == "" {
		item.SourceIntegrityStatus = string(ingest.CaptureIntegrityUnavailable)
	}
	item.CapturedAt = raw.CapturedAt
	item.ParseState = raw.ParseState
	item.WireFidelity = raw.WireFidelity
	if selection.Side == "request" {
		item.Truncated = raw.RequestTruncated
	} else {
		item.Truncated = raw.ResponseTruncated
	}
	item.OmittedBytes = int64Pointer(raw.TotalSize)
	if raw.TotalSize > int64(len(raw.Data)) {
		item.OmittedReason = "The full message exceeds the local redaction context limit; the selected span was omitted."
		return item
	}
	if selection.Offset+int64(selection.Length) > raw.TotalSize || len(raw.Data) != int(raw.TotalSize) {
		item.OmittedReason = "The exact selected span could not be resolved at the requested byte bounds."
		return item
	}
	if !utf8.Valid(raw.Data) {
		item.OmittedReason = "The full message is not valid UTF-8; the selected span was omitted from the model packet."
		return item
	}
	start := int(selection.Offset)
	end := start + selection.Length
	selectedSource := raw.Data[start:end]
	if !utf8.Valid(selectedSource) {
		item.OmittedReason = "The selected span is not valid UTF-8; its bytes were omitted from the model packet."
		return item
	}
	_, spans, err := publicdata.RedactText(string(raw.Data))
	if err != nil {
		item.OmittedReason = "The selected span did not pass the redaction validation gate; its text was omitted."
		return item
	}
	selectedExcerpt, selectedRedactions, redactedBytes := redactSelectedSpan(string(raw.Data), spans, start, end)
	if err := publicdata.Validate(map[string]string{"excerpt": selectedExcerpt}); err != nil {
		item.OmittedReason = "The selected span did not pass the redaction validation gate; its text was omitted."
		return item
	}
	item.Available = true
	item.Excerpt = selectedExcerpt
	item.Redactions = selectedRedactions
	item.RedactedBytes = redactedBytes
	item.IncludedBytes = selection.Length - item.RedactedBytes
	item.OmittedBytes = int64Pointer(raw.TotalSize - int64(item.IncludedBytes))
	return item
}

func redactSelectedSpan(input string, redactions []publicdata.TextRedactionSpan, start, end int) (string, []publicdata.TextRedactionSpan, int) {
	var excerpt strings.Builder
	excerpt.Grow(end - start)
	selectedRedactions := make([]publicdata.TextRedactionSpan, 0)
	cursor := start
	redactedBytes := 0
	for _, span := range redactions {
		if span.End <= start || span.Start >= end {
			continue
		}
		overlapStart := span.Start
		if overlapStart < start {
			overlapStart = start
		}
		overlapEnd := span.End
		if overlapEnd > end {
			overlapEnd = end
		}
		if overlapStart > cursor {
			excerpt.WriteString(input[cursor:overlapStart])
		}
		excerpt.WriteString("[REDACTED]")
		if overlapEnd > cursor {
			cursor = overlapEnd
		}
		selectedRedactions = append(selectedRedactions, span)
		redactedBytes += overlapEnd - overlapStart
	}
	if cursor < end {
		excerpt.WriteString(input[cursor:end])
	}
	return excerpt.String(), selectedRedactions, redactedBytes
}

func unavailablePacketEvidence(selection EvidencePacketSelection, reason string) EvidencePacketEvidence {
	return EvidencePacketEvidence{
		RequestID: selection.RequestID, RevisionID: selection.RevisionID, Side: selection.Side,
		SpanStart: selection.Offset, SpanEnd: selection.Offset + int64(selection.Length), SelectedBytes: selection.Length,
		SourceIntegrityStatus: string(ingest.CaptureIntegrityUnavailable),
		OmittedReason:         reason, Redactions: []publicdata.TextRedactionSpan{},
	}
}

func (packet *EvidencePacket) appendEvidence(item EvidencePacketEvidence) {
	packet.SelectedBytes += item.SelectedBytes
	if item.OmittedBytes == nil {
		packet.OmittedBytesKnown = false
	} else {
		packet.OmittedBytes += *item.OmittedBytes
	}
	packet.Evidence = append(packet.Evidence, item)
}

func int64Pointer(value int64) *int64 { return &value }
