package appquery

import (
	"context"
	"errors"
	"strings"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/store"
)

const (
	MaximumEvidenceRangeBytes = ingest.MaximumCaptureEvidenceRangeBytes
	maximumEvidenceOffset     = 128 << 20
)

// CaptureMessageRangeRequest identifies one exact message range in a local
// historical capture. RevisionID pins subsequent pages to the same evidence.
type CaptureMessageRangeRequest struct {
	ProjectKey string
	RequestID  string
	RevisionID string
	Side       string
	Offset     int64
	Maximum    int
}

// CaptureMessageRange contains bounded bytes plus provenance and parsing
// limits. Data is intentionally not included in a generic wire-map helper.
type CaptureMessageRange struct {
	ProjectKey        string
	RequestID         string
	RevisionID        string
	Side              string
	Offset            int64
	TotalSize         int64
	Data              []byte
	SHA256            string
	CapturedAt        *string
	ParseState        string
	WireFidelity      string
	IntegrityStatus   string
	RequestTruncated  *bool
	ResponseTruncated *bool
}

// CaptureMessageRange reads one authenticated range from the encrypted vault
// of exactly one registered local project. It never fetches Caido data.
func (s *Service) CaptureMessageRange(ctx context.Context, request CaptureMessageRangeRequest) (CaptureMessageRange, error) {
	if s == nil {
		return CaptureMessageRange{}, queryError(ErrorEvidenceUnavailable, "local capture evidence is unavailable", nil)
	}
	if ctx == nil || validateCaptureMessageRangeRequest(request) != nil {
		return CaptureMessageRange{}, queryError(ErrorInvalidInput, "capture evidence request is invalid", nil)
	}
	ref, ok := s.projectRef(request.ProjectKey)
	if !ok {
		return CaptureMessageRange{}, queryError(ErrorProjectNotRegistered, "selected project is not in the local project catalog", nil)
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, s.dataRoot)
	if err != nil {
		return CaptureMessageRange{}, queryError(ErrorProjectUnavailable, "selected project is unavailable for local evidence reads", err)
	}
	defer selected.Close()

	var raw ingest.CaptureRange
	if request.RevisionID == "" {
		raw, err = ingest.ReadCaptureCurrentRevisionRange(ctx, selected, request.RequestID, request.Side, request.Offset, request.Maximum)
	} else {
		raw, err = ingest.ReadCaptureRevisionRange(ctx, selected, request.RequestID, request.RevisionID, request.Side, request.Offset, request.Maximum)
	}
	if err != nil {
		return CaptureMessageRange{}, captureEvidenceReadError(err)
	}
	if err := ctx.Err(); err != nil {
		clear(raw.Data)
		return CaptureMessageRange{}, queryError(ErrorEvidenceUnavailable, "capture evidence read was interrupted", err)
	}
	if raw.RevisionID == nil || raw.TotalSize < 0 || len(raw.Data) > request.Maximum || len(raw.Data) == 0 && request.Offset < raw.TotalSize {
		clear(raw.Data)
		return CaptureMessageRange{}, queryError(ErrorEvidenceUnavailable, "capture evidence range is incomplete", errors.New("invalid range metadata"))
	}
	if request.RevisionID != "" && *raw.RevisionID != request.RevisionID {
		clear(raw.Data)
		return CaptureMessageRange{}, queryError(ErrorEvidenceUnavailable, "capture revision changed during evidence read", errors.New("revision mismatch"))
	}
	return CaptureMessageRange{
		ProjectKey: request.ProjectKey, RequestID: request.RequestID, RevisionID: *raw.RevisionID,
		Side: request.Side, Offset: request.Offset, TotalSize: raw.TotalSize,
		Data: raw.Data, SHA256: raw.SHA256, CapturedAt: raw.CapturedAt,
		ParseState: raw.ParseState, WireFidelity: raw.WireFidelity, IntegrityStatus: string(raw.IntegrityStatus),
		RequestTruncated: raw.RequestTruncated, ResponseTruncated: raw.ResponseTruncated,
	}, nil
}

func captureEvidenceReadError(err error) *QueryError {
	if errors.Is(err, ingest.ErrVaultKeyUnavailable) {
		return queryError(ErrorEvidenceKeyUnavailable, "local vault key is unavailable or invalid", err)
	}
	return queryError(ErrorEvidenceUnavailable, "exact historical capture bytes are unavailable", err)
}

func validateCaptureMessageRangeRequest(request CaptureMessageRangeRequest) error {
	if !projectKeyPattern.MatchString(request.ProjectKey) || strings.TrimSpace(request.RequestID) == "" || len(request.RequestID) > 512 ||
		(request.Side != "request" && request.Side != "response") || request.Offset < 0 || request.Offset > maximumEvidenceOffset ||
		request.Maximum < 1 || request.Maximum > MaximumEvidenceRangeBytes ||
		(request.RevisionID != "" && (len(request.RevisionID) != 64 || strings.Trim(request.RevisionID, "0123456789abcdef") != "")) {
		return errors.New("capture evidence range is invalid")
	}
	return nil
}
