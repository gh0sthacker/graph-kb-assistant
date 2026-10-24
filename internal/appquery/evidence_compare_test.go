package appquery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
)

func TestCompareCaptureRangesReportsEqualityAndFirstDifference(t *testing.T) {
	leftRevision := strings.Repeat("a", 64)
	rightRevision := strings.Repeat("b", 64)
	truncated := true
	leftBytes := []byte("GET /items?id=1 HTTP/1.1\r\n\r\n")
	rightBytes := []byte("GET /items?id=2 HTTP/1.1\r\n\r\n")
	left := ingest.CaptureRange{
		Data: leftBytes, TotalSize: int64(len(leftBytes)), SHA256: strings.Repeat("c", 64),
		RevisionID: &leftRevision, ParseState: "complete", WireFidelity: "source_bytes",
		RequestTruncated: &truncated,
	}
	right := ingest.CaptureRange{
		Data: rightBytes, TotalSize: int64(len(rightBytes)), SHA256: strings.Repeat("d", 64),
		RevisionID: &rightRevision, ParseState: "complete", WireFidelity: "source_bytes",
	}
	request := CaptureComparisonRequest{
		LeftRequestID: "first", LeftRevisionID: leftRevision,
		RightRequestID: "second", RightRevisionID: rightRevision,
	}
	comparison := compareCaptureRanges(request, "request", left, nil, right, nil)
	if !comparison.Available || comparison.Equal || comparison.FirstDifferenceOffset == nil || *comparison.FirstDifferenceOffset != 14 {
		t.Fatalf("comparison = %#v, want a complete difference at byte 14", comparison)
	}
	if comparison.ComparedBytes != int64(len(leftBytes)) || comparison.Left.SHA256 == comparison.Right.SHA256 {
		t.Fatalf("comparison summaries = %#v", comparison)
	}
	if comparison.Left.Truncated == nil || !*comparison.Left.Truncated {
		t.Fatalf("comparison lost the source truncation flag: %#v", comparison.Left)
	}
	if !strings.Contains(comparison.LeftExcerpt, "id=1") || !strings.Contains(comparison.RightExcerpt, "id=2") {
		t.Fatalf("difference excerpts = %q / %q", comparison.LeftExcerpt, comparison.RightExcerpt)
	}

	identical := compareCaptureRanges(request, "request", left, nil, left, nil)
	if !identical.Available || !identical.Equal || identical.FirstDifferenceOffset != nil {
		t.Fatalf("identical comparison = %#v", identical)
	}
}

func TestCompareCaptureRangesMarksOversizedMessagesIncomplete(t *testing.T) {
	request := CaptureComparisonRequest{
		LeftRequestID: "first", LeftRevisionID: strings.Repeat("a", 64),
		RightRequestID: "second", RightRevisionID: strings.Repeat("b", 64),
	}
	partial := ingest.CaptureRange{
		Data: make([]byte, MaximumComparisonMessageBytes), TotalSize: MaximumComparisonMessageBytes + 1,
		SHA256: strings.Repeat("c", 64),
	}
	comparison := compareCaptureRanges(request, "response", partial, nil, partial, nil)
	if comparison.Available || !strings.Contains(comparison.Reason, "1 MiB") {
		t.Fatalf("oversized comparison = %#v, want explicit incomplete state", comparison)
	}
}

func TestCaptureComparisonRequestValidation(t *testing.T) {
	valid := CaptureComparisonRequest{
		ProjectKey: strings.Repeat("a", 64), LeftRequestID: "first", LeftRevisionID: strings.Repeat("b", 64),
		RightRequestID: "second", RightRevisionID: strings.Repeat("c", 64),
	}
	if err := validateCaptureComparisonRequest(valid); err != nil {
		t.Fatalf("valid comparison request rejected: %v", err)
	}
	invalid := valid
	invalid.RightRevisionID = "../" + strings.Repeat("c", 64)
	if err := validateCaptureComparisonRequest(invalid); err == nil {
		t.Fatal("comparison request accepted an invalid right revision")
	}
}

func TestCompareCaptureMessagesValidatesIdentityAndProjectRegistration(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService: %v", err)
	}
	defer service.Close(context.Background())

	valid := CaptureComparisonRequest{
		ProjectKey: strings.Repeat("a", 64), LeftRequestID: "left", LeftRevisionID: strings.Repeat("b", 64),
		RightRequestID: "right", RightRevisionID: strings.Repeat("c", 64),
	}
	invalid := valid
	invalid.RightRequestID = "  "
	_, err = service.CompareCaptureMessages(context.Background(), invalid)
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("invalid comparison error = %v, want typed invalid-input error", err)
	}
	_, err = service.CompareCaptureMessages(nil, valid)
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorInvalidInput {
		t.Fatalf("nil context error = %v, want typed invalid-input error", err)
	}
	_, err = service.CompareCaptureMessages(context.Background(), valid)
	if !errors.As(err, &queryErr) || queryErr.Code != ErrorProjectNotRegistered {
		t.Fatalf("unregistered project error = %v, want project-not-registered", err)
	}
}
