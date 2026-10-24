package appquery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
)

func TestCaptureMessageRangeValidatesIdentityBoundsAndRegistration(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer service.Close(context.Background())

	valid := CaptureMessageRangeRequest{
		ProjectKey: strings.Repeat("a", 64), RequestID: "239", RevisionID: strings.Repeat("b", 64),
		Side: "request", Offset: 0, Maximum: MaximumEvidenceRangeBytes,
	}
	invalid := []CaptureMessageRangeRequest{
		{ProjectKey: "../" + strings.Repeat("a", 64), RequestID: "239", Side: "request", Maximum: 1},
		{ProjectKey: valid.ProjectKey, Side: "request", Maximum: 1},
		{ProjectKey: valid.ProjectKey, RequestID: "239", Side: "body", Maximum: 1},
		{ProjectKey: valid.ProjectKey, RequestID: "239", Side: "request", Offset: -1, Maximum: 1},
		{ProjectKey: valid.ProjectKey, RequestID: "239", Side: "request", Maximum: 0},
		{ProjectKey: valid.ProjectKey, RequestID: "239", Side: "request", Maximum: MaximumEvidenceRangeBytes + 1},
		{ProjectKey: valid.ProjectKey, RequestID: "239", RevisionID: "../" + strings.Repeat("b", 64), Side: "request", Maximum: 1},
	}
	for index, request := range invalid {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			_, err := service.CaptureMessageRange(context.Background(), request)
			var queryErr *QueryError
			if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
				t.Fatalf("CaptureMessageRange() error = %v, want typed invalid-input error", err)
			}
		})
	}

	_, err = service.CaptureMessageRange(context.Background(), valid)
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorProjectNotRegistered {
		t.Fatalf("unregistered project error = %v, want project-not-registered", err)
	}
}

func TestCaptureMessageRangeRejectsMissingContext(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer service.Close(context.Background())

	_, err = service.CaptureMessageRange(nil, CaptureMessageRangeRequest{
		ProjectKey: strings.Repeat("a", 64), RequestID: "239", Side: "request", Maximum: 1,
	})
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("nil context error = %v, want typed invalid-input error", err)
	}
}

func TestCaptureEvidenceReadErrorClassifiesUnavailableVaultKey(t *testing.T) {
	queryErr := captureEvidenceReadError(ingest.ErrVaultKeyUnavailable)
	if queryErr == nil || queryErr.Code != ErrorEvidenceKeyUnavailable || queryErr.Message != "local vault key is unavailable or invalid" {
		t.Fatalf("captureEvidenceReadError() = %#v, want safe unavailable-key error", queryErr)
	}
}
