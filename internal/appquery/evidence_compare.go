package appquery

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/store"
)

const MaximumComparisonMessageBytes = ingest.MaximumCaptureEvidenceRangeBytes

type CaptureComparisonRequest struct {
	ProjectKey      string
	LeftRequestID   string
	LeftRevisionID  string
	RightRequestID  string
	RightRevisionID string
}

type CaptureRevisionSummary struct {
	RequestID    string  `json:"request_id"`
	RevisionID   string  `json:"revision_id"`
	Size         int64   `json:"size"`
	SHA256       string  `json:"sha256"`
	CapturedAt   *string `json:"captured_at,omitempty"`
	ParseState   string  `json:"parse_state"`
	WireFidelity string  `json:"wire_fidelity"`
	Truncated    *bool   `json:"truncated,omitempty"`
}

type CaptureSideComparison struct {
	Available             bool                   `json:"available"`
	Reason                string                 `json:"reason,omitempty"`
	Equal                 bool                   `json:"equal"`
	ComparedBytes         int64                  `json:"compared_bytes"`
	FirstDifferenceOffset *int64                 `json:"first_difference_offset,omitempty"`
	LeftExcerpt           string                 `json:"left_excerpt,omitempty"`
	RightExcerpt          string                 `json:"right_excerpt,omitempty"`
	Left                  CaptureRevisionSummary `json:"left"`
	Right                 CaptureRevisionSummary `json:"right"`
}

type CaptureComparison struct {
	ProjectKey      string                `json:"project_key"`
	LeftRequestID   string                `json:"left_request_id"`
	LeftRevisionID  string                `json:"left_revision_id"`
	RightRequestID  string                `json:"right_request_id"`
	RightRevisionID string                `json:"right_revision_id"`
	Request         CaptureSideComparison `json:"request"`
	Response        CaptureSideComparison `json:"response"`
	CoverageNote    string                `json:"coverage_note"`
}

// CompareCaptureMessages compares the stored bytes of two exact local
// historical revisions, up to one MiB per side. It never fetches Caido data
// and does not infer completeness from a byte-for-byte match.
func (s *Service) CompareCaptureMessages(ctx context.Context, request CaptureComparisonRequest) (CaptureComparison, error) {
	if s == nil {
		return CaptureComparison{}, queryError(ErrorEvidenceUnavailable, "local capture comparison is unavailable", nil)
	}
	if ctx == nil || validateCaptureComparisonRequest(request) != nil {
		return CaptureComparison{}, queryError(ErrorInvalidInput, "capture comparison request is invalid", nil)
	}
	ref, ok := s.projects[request.ProjectKey]
	if !ok {
		return CaptureComparison{}, queryError(ErrorProjectNotRegistered, "selected project is not in the local project catalog", nil)
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, s.dataRoot)
	if err != nil {
		return CaptureComparison{}, queryError(ErrorProjectUnavailable, "selected project is unavailable for local evidence reads", err)
	}
	defer selected.Close()

	compareSide := func(side string) (CaptureSideComparison, error) {
		left, leftErr := ingest.ReadCaptureRevisionRange(ctx, selected, request.LeftRequestID, request.LeftRevisionID, side, 0, MaximumComparisonMessageBytes)
		if errors.Is(leftErr, ingest.ErrVaultKeyUnavailable) {
			return CaptureSideComparison{}, captureEvidenceReadError(leftErr)
		}
		right, rightErr := ingest.ReadCaptureRevisionRange(ctx, selected, request.RightRequestID, request.RightRevisionID, side, 0, MaximumComparisonMessageBytes)
		if errors.Is(rightErr, ingest.ErrVaultKeyUnavailable) {
			clear(left.Data)
			return CaptureSideComparison{}, captureEvidenceReadError(rightErr)
		}
		comparison := compareCaptureRanges(request, side, left, leftErr, right, rightErr)
		clear(left.Data)
		clear(right.Data)
		return comparison, nil
	}

	requestComparison, err := compareSide("request")
	if err != nil {
		return CaptureComparison{}, err
	}
	responseComparison, err := compareSide("response")
	if err != nil {
		return CaptureComparison{}, err
	}
	if err := ctx.Err(); err != nil {
		return CaptureComparison{}, queryError(ErrorEvidenceUnavailable, "capture comparison was interrupted", err)
	}
	return CaptureComparison{
		ProjectKey: request.ProjectKey, LeftRequestID: request.LeftRequestID, LeftRevisionID: request.LeftRevisionID,
		RightRequestID: request.RightRequestID, RightRevisionID: request.RightRevisionID,
		Request: requestComparison, Response: responseComparison,
		CoverageNote: "Exact byte comparison of locally stored historical messages up to 1 MiB per side; truncation and source fidelity are reported per message. Live data and larger messages are not compared.",
	}, nil
}

func validateCaptureComparisonRequest(request CaptureComparisonRequest) error {
	validRevision := func(value string) bool {
		return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
	}
	validRequestID := func(value string) bool {
		return strings.TrimSpace(value) != "" && len(value) <= 512
	}
	if !projectKeyPattern.MatchString(request.ProjectKey) || !validRequestID(request.LeftRequestID) || !validRevision(request.LeftRevisionID) ||
		!validRequestID(request.RightRequestID) || !validRevision(request.RightRevisionID) {
		return errors.New("capture comparison identity is invalid")
	}
	return nil
}

func compareCaptureRanges(request CaptureComparisonRequest, side string, left ingest.CaptureRange, leftErr error, right ingest.CaptureRange, rightErr error) CaptureSideComparison {
	comparison := CaptureSideComparison{
		Left:  CaptureRevisionSummary{RequestID: request.LeftRequestID, RevisionID: request.LeftRevisionID},
		Right: CaptureRevisionSummary{RequestID: request.RightRequestID, RevisionID: request.RightRevisionID},
	}
	if leftErr == nil {
		comparison.Left = summarizeCaptureRevision(request.LeftRequestID, left, side)
	}
	if rightErr == nil {
		comparison.Right = summarizeCaptureRevision(request.RightRequestID, right, side)
	}
	if leftErr != nil || rightErr != nil {
		comparison.Reason = "Exact local bytes are unavailable for one or both selected revisions."
		return comparison
	}
	if left.TotalSize != int64(len(left.Data)) || right.TotalSize != int64(len(right.Data)) {
		comparison.Reason = "One or both stored messages exceed the 1 MiB comparison limit."
		return comparison
	}
	comparison.Available = true
	comparison.Equal = bytes.Equal(left.Data, right.Data)
	comparison.ComparedBytes = int64(max(len(left.Data), len(right.Data)))
	if comparison.Equal {
		return comparison
	}
	first := 0
	for first < len(left.Data) && first < len(right.Data) && left.Data[first] == right.Data[first] {
		first++
	}
	offset := int64(first)
	comparison.FirstDifferenceOffset = &offset
	const contextBytes = 32
	start := max(0, first-contextBytes)
	comparison.LeftExcerpt = strconv.Quote(string(left.Data[start:min(len(left.Data), first+contextBytes)]))
	comparison.RightExcerpt = strconv.Quote(string(right.Data[start:min(len(right.Data), first+contextBytes)]))
	return comparison
}

func summarizeCaptureRevision(requestID string, capture ingest.CaptureRange, side string) CaptureRevisionSummary {
	summary := CaptureRevisionSummary{
		RequestID: requestID, Size: capture.TotalSize, SHA256: capture.SHA256,
		CapturedAt: capture.CapturedAt, ParseState: capture.ParseState, WireFidelity: capture.WireFidelity,
	}
	if capture.RevisionID != nil {
		summary.RevisionID = *capture.RevisionID
	}
	if side == "request" {
		summary.Truncated = capture.RequestTruncated
	} else {
		summary.Truncated = capture.ResponseTruncated
	}
	return summary
}
