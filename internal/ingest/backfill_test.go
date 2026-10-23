package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
)

func TestRunBackfillTerminalCursorDoesNotClaimCoverageReconciled(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	t.Cleanup(pageWriter.Close)

	requestID := "synthetic-history-request"
	target := "https://api.fixture.invalid/api/items"
	metadata := caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{
		RequestID: requestID, Method: "GET", URL: target,
	}}}
	history := caido.HistoryPage{Exchanges: []caido.HttpExchange{{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: requestID},
		RequestRaw:           []byte("GET /api/items HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "api.fixture.invalid",
		Port:                 443,
		Target:               target,
		RequestWireFidelity:  "source_bytes",
		ResponseWireFidelity: "unknown",
	}}}
	source := &backfillSourceFixture{project: selected, metadata: metadata, history: history}

	result, err := RunBackfill(ctx, source, pageWriter, selected, BackfillOptions{
		PageSize: 1, MaxBodyBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("RunBackfill() error = %v", err)
	}
	if result.State != "done" || result.CursorState != "complete" || result.LimitReached || result.PagesThisRun != 1 {
		t.Fatalf("RunBackfill() terminal enumeration state = %+v", result)
	}
	if result.CoverageReconciled {
		t.Fatal("RunBackfill() marked coverage reconciled without a stable source snapshot")
	}
	if !strings.Contains(result.CoverageNote, "no stable snapshot token") || !strings.Contains(result.CoverageNote, "expected history total") {
		t.Fatalf("RunBackfill() coverage note does not explain the unresolved gap: %q", result.CoverageNote)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal backfill result: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("decode backfill JSON: %v", err)
	}
	if output["state"] != "done" || output["coverage_reconciled"] != false || output["coverage_note"] != result.CoverageNote {
		t.Fatalf("serialized backfill result obscures partial coverage: %s", encoded)
	}
	if source.pageCalls != 1 || source.fetchCalls != 1 {
		t.Fatalf("Caido calls = list:%d fetch:%d; want one terminal page", source.pageCalls, source.fetchCalls)
	}
}

func TestValidateExchangeAcceptsLongCaidoTargetWithinMessageLimit(t *testing.T) {
	selected := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := selected.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "long-target-fixture"},
		RequestRaw:           []byte("GET / HTTP/1.1\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "fixture.invalid",
		Port:                 443,
		Target:               "https://fixture.invalid/" + strings.Repeat("a", 9_000),
		RequestWireFidelity:  "reconstructed_text",
		ResponseWireFidelity: "unknown",
	}

	if _, err := validateExchange(exchange, projectKey); err != nil {
		t.Fatalf("validateExchange() rejected a target longer than eight KiB but within the message limit: %v", err)
	}
}

func TestValidateExchangeRejectsTargetBeyondMessageLimit(t *testing.T) {
	selected := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectKey, err := selected.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	exchange := caido.HttpExchange{
		Ref:                  caido.ExchangeRef{Project: selected, RequestID: "oversized-target-fixture"},
		RequestRaw:           []byte("GET / HTTP/1.1\r\n\r\n"),
		Method:               "GET",
		Scheme:               "https",
		Host:                 "fixture.invalid",
		Port:                 443,
		Target:               "https://fixture.invalid/" + strings.Repeat("a", caido.TargetURLBytesMaximum+1),
		RequestWireFidelity:  "reconstructed_text",
		ResponseWireFidelity: "unknown",
	}

	if _, err := validateExchange(exchange, projectKey); err == nil {
		t.Fatal("validateExchange() accepted a target beyond the message limit")
	}
}

type backfillSourceFixture struct {
	project    project.CaidoProjectRef
	metadata   caido.RequestMetadataPage
	history    caido.HistoryPage
	pageErr    error
	fetchErr   error
	pageCalls  int
	fetchCalls int
}

func (s *backfillSourceFixture) ListProjects() ([]caido.ProjectRef, error) {
	return []caido.ProjectRef{s.project}, nil
}

func (s *backfillSourceFixture) ListRequestPage(selected project.CaidoProjectRef, cursor *string, _ int, httpql *string) (caido.RequestMetadataPage, error) {
	if !selected.SameProject(s.project) || cursor != nil || httpql != nil || s.pageCalls != 0 {
		return caido.RequestMetadataPage{}, errors.New("fixture received an unexpected backfill page request")
	}
	s.pageCalls++
	if s.pageErr != nil {
		return caido.RequestMetadataPage{}, s.pageErr
	}
	return s.metadata, nil
}

func (s *backfillSourceFixture) FetchHistoryPage(selected project.CaidoProjectRef, page caido.RequestMetadataPage, _ map[string]struct{}, _ int) (caido.HistoryPage, error) {
	if !selected.SameProject(s.project) || s.fetchCalls != 0 || len(page.Requests) != len(s.metadata.Requests) {
		return caido.HistoryPage{}, errors.New("fixture received an unexpected history detail request")
	}
	s.fetchCalls++
	if s.fetchErr != nil {
		return caido.HistoryPage{}, s.fetchErr
	}
	return s.history, nil
}

func (s *backfillSourceFixture) GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error) {
	return nil, errors.New("fixture does not implement direct exchange lookup")
}
