package ingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestRunLiveTailImportsOnceAndDoesNotPersistRawMessages(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)

	requestRaw := []byte("GET /api/items?query=fixture-query-marker HTTP/1.1\r\n" +
		"Host: api.fixture.invalid\r\n" +
		"X-Fixture-Header: fixture-header-marker\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"name":"item","api_key":"fixture-request-body-marker"}`)
	responseRaw := []byte("HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"result":"ok","token":"fixture-response-body-marker"}`)
	status := 200
	capturedAt := "2026-10-22T12:00:00Z"
	requestID := "fixture-request-1"
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: selected, RequestID: requestID},
		CapturedAt: &capturedAt, RequestRaw: requestRaw, ResponseRaw: responseRaw,
		HasResponse: true, Method: "GET", Scheme: "https", Host: "api.fixture.invalid",
		Port: 443, Target: "https://api.fixture.invalid/api/items?query=fixture-query-marker",
		ResponseStatus: &status, RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	source := &liveTailSourceFixture{
		project: selected,
		pages: []caido.RequestMetadataPage{{Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET",
			URL: "https://api.fixture.invalid/api/items?query=fixture-query-marker", StatusCode: &status,
		}}}, {Requests: []caido.RequestMetadata{{
			RequestID: requestID, Method: "GET",
			URL: "https://api.fixture.invalid/api/items?query=fixture-query-marker", StatusCode: &status,
		}}}},
		exchanges: map[string]caido.HttpExchange{requestID: exchange},
	}
	options := LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}

	first, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("first RunLiveTail() error = %v", err)
	}
	if first.State != "idle" || first.RequestsSeen != 1 || first.RequestsAdded != 1 || first.PagesCompleted != 1 || first.CursorPending || first.LimitReached {
		t.Fatalf("first RunLiveTail() result = %+v, want one imported request and an idle checkpoint", first)
	}
	if !allBytesZero(requestRaw) || !allBytesZero(responseRaw) {
		t.Fatal("RunLiveTail() retained raw request or response bytes after processing")
	}

	second, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("second RunLiveTail() error = %v", err)
	}
	if second.State != "idle" || second.RequestsSeen != 1 || second.RequestsAdded != 0 || second.PagesCompleted != 1 {
		t.Fatalf("second RunLiveTail() result = %+v, want an idle deduplicated page", second)
	}
	if source.fetchCalls != 2 || source.skippedCounts[1] != 1 {
		t.Fatalf("detail fetches = %d, skipped IDs on replay = %d; want 2 fetches and 1 skipped ID", source.fetchCalls, source.skippedCounts[1])
	}

	var count int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_index").Scan(&count); err != nil {
		t.Fatalf("count live captures: %v", err)
	}
	if count != 1 {
		t.Fatalf("live capture count = %d, want 1 after replay", count)
	}
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_fts").Scan(&count); err != nil {
		t.Fatalf("count live full-text rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("live full-text row count = %d, want 1 after replay", count)
	}
	var checkpointState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&checkpointState); err != nil {
		t.Fatalf("read live-tail checkpoint: %v", err)
	}
	if checkpointState != "idle" {
		t.Fatalf("checkpoint state = %q, want idle", checkpointState)
	}
	if _, err := os.Stat(projectStore.BehaviorGraphPath); err != nil {
		t.Fatalf("BehaviorGraph was not created: %v", err)
	}

	for _, marker := range []string{
		"fixture-query-marker", "fixture-header-marker", "fixture-request-body-marker", "fixture-response-body-marker",
	} {
		assertAbsentFromProjectFiles(t, projectStore.Directory, marker)
	}
}

func TestRunLiveTailMarksCheckpointPartialWhenCaidoReadFails(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	readErr := errors.New("fixture Caido read failure")
	source := &liveTailSourceFixture{project: selected, pageErr: readErr}

	result, err := RunLiveTail(ctx, source, writer, selected,
		LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10},
	)
	if !errors.Is(err, readErr) {
		t.Fatalf("RunLiveTail() error = %v, want fixture read error", err)
	}
	if result.State != "partial" || result.PagesCompleted != 0 || result.RequestsAdded != 0 {
		t.Fatalf("RunLiveTail() result = %+v, want a partial checkpoint with no imported requests", result)
	}
	var checkpointState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&checkpointState); err != nil {
		t.Fatalf("read live-tail checkpoint: %v", err)
	}
	if checkpointState != "partial" {
		t.Fatalf("checkpoint state = %q, want partial", checkpointState)
	}
}

func TestRunLiveTailResumesFromCommittedCursor(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)

	firstRequestRaw := []byte("GET /api/first HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\nfirst-page-marker")
	secondRequestRaw := []byte("GET /api/second HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\nsecond-page-marker")
	firstID, secondID := "fixture-request-first", "fixture-request-second"
	firstExchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: selected, RequestID: firstID}, RequestRaw: firstRequestRaw,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/api/first", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}
	secondExchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: selected, RequestID: secondID}, RequestRaw: secondRequestRaw,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/api/second", RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
	}
	nextCursor := "fixture-cursor-next-page"
	firstPage := caido.RequestMetadataPage{
		Requests:   []caido.RequestMetadata{{RequestID: firstID, Method: "GET", URL: firstExchange.Target}},
		NextCursor: &nextCursor, HasMore: true,
	}
	secondPage := caido.RequestMetadataPage{
		Requests: []caido.RequestMetadata{{RequestID: secondID, Method: "GET", URL: secondExchange.Target}},
	}
	source := &liveTailSourceFixture{
		project: selected, pages: []caido.RequestMetadataPage{firstPage, secondPage},
		exchanges: map[string]caido.HttpExchange{firstID: firstExchange, secondID: secondExchange},
	}
	options := LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}

	firstRun, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("first RunLiveTail() error = %v", err)
	}
	if firstRun.State != "partial" || firstRun.RequestsAdded != 1 || firstRun.PagesCompleted != 1 || !firstRun.CursorPending || !firstRun.LimitReached {
		t.Fatalf("first RunLiveTail() result = %+v, want a committed partial page with a pending cursor", firstRun)
	}

	secondRun, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil {
		t.Fatalf("resumed RunLiveTail() error = %v", err)
	}
	if secondRun.State != "idle" || secondRun.RequestsSeen != 1 || secondRun.RequestsAdded != 1 || secondRun.PagesCompleted != 1 || secondRun.CursorPending {
		t.Fatalf("resumed RunLiveTail() result = %+v, want one final page and an idle checkpoint", secondRun)
	}
	if len(source.cursors) != 2 || source.cursors[0] != "" || source.cursors[1] != nextCursor {
		t.Fatalf("metadata cursors = %v, want [empty %q]", source.cursors, nextCursor)
	}
	if len(source.httpqlFilters) != 2 || source.httpqlFilters[0] != source.httpqlFilters[1] {
		t.Fatalf("resumed live-tail time filter changed: %v", source.httpqlFilters)
	}
	if !allBytesZero(firstRequestRaw) || !allBytesZero(secondRequestRaw) {
		t.Fatal("resumed RunLiveTail() retained raw request bytes after processing")
	}
	var count int
	if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM live_capture_index").Scan(&count); err != nil {
		t.Fatalf("count live captures: %v", err)
	}
	if count != 2 {
		t.Fatalf("live capture count after cursor resume = %d, want 2", count)
	}
	var checkpointState string
	if err := projectStore.Database.DB().QueryRowContext(ctx,
		"SELECT state FROM live_sync_state WHERE source_key=?", liveTailSourceKey,
	).Scan(&checkpointState); err != nil {
		t.Fatalf("read live-tail checkpoint: %v", err)
	}
	if checkpointState != "idle" {
		t.Fatalf("checkpoint state after cursor resume = %q, want idle", checkpointState)
	}
	for _, marker := range []string{"first-page-marker", "second-page-marker"} {
		assertAbsentFromProjectFiles(t, projectStore.Directory, marker)
	}
}

type liveTailSourceFixture struct {
	project       project.CaidoProjectRef
	pages         []caido.RequestMetadataPage
	exchanges     map[string]caido.HttpExchange
	pageErr       error
	pageCalls     int
	fetchCalls    int
	cursors       []string
	httpqlFilters []string
	skippedCounts []int
}

func (s *liveTailSourceFixture) ListProjects() ([]caido.ProjectRef, error) {
	return []caido.ProjectRef{s.project}, nil
}

func (s *liveTailSourceFixture) ListRequestPage(selected project.CaidoProjectRef, cursor *string, _ int, httpql *string) (caido.RequestMetadataPage, error) {
	if !selected.SameProject(s.project) || httpql == nil || !strings.Contains(*httpql, "req.created_at.gt:") {
		return caido.RequestMetadataPage{}, errors.New("fixture received an invalid project or live-tail time filter")
	}
	if s.pageErr != nil {
		return caido.RequestMetadataPage{}, s.pageErr
	}
	cursorValue := ""
	if cursor != nil {
		cursorValue = *cursor
	}
	s.cursors = append(s.cursors, cursorValue)
	s.httpqlFilters = append(s.httpqlFilters, *httpql)
	if s.pageCalls >= len(s.pages) {
		return caido.RequestMetadataPage{}, errors.New("fixture has no metadata page for the requested cursor")
	}
	page := s.pages[s.pageCalls]
	s.pageCalls++
	return page, nil
}

func (s *liveTailSourceFixture) FetchHistoryPage(selected project.CaidoProjectRef, page caido.RequestMetadataPage, skipIDs map[string]struct{}, _ int) (caido.HistoryPage, error) {
	if !selected.SameProject(s.project) {
		return caido.HistoryPage{}, errors.New("fixture received a different selected project")
	}
	s.fetchCalls++
	s.skippedCounts = append(s.skippedCounts, len(skipIDs))
	exchanges := make([]caido.HttpExchange, 0, len(page.Requests))
	for _, request := range page.Requests {
		if _, skip := skipIDs[request.RequestID]; skip {
			continue
		}
		exchange, ok := s.exchanges[request.RequestID]
		if !ok {
			return caido.HistoryPage{}, errors.New("fixture metadata identity has no exchange")
		}
		exchanges = append(exchanges, exchange)
	}
	return caido.HistoryPage{Exchanges: exchanges, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
}

func (s *liveTailSourceFixture) GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error) {
	return nil, errors.New("fixture does not implement direct exchange lookup")
}

func newLiveTailFixture(t *testing.T) (project.CaidoProjectRef, *store.ProjectStore, *LiveTailWriter) {
	t.Helper()
	ctx := context.Background()
	selected := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "live-tail-project"}
	projectStore, err := store.CreateProject(ctx, selected, t.TempDir())
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	fixtureKey := bytes.Repeat([]byte("k"), 32)
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(fixtureKey))
	clear(fixtureKey)
	writer, err := OpenLiveTailWriter(ctx, projectStore)
	if err != nil {
		t.Fatalf("OpenLiveTailWriter() error = %v", err)
	}
	t.Cleanup(writer.Close)
	return selected, projectStore, writer
}

func allBytesZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

func assertAbsentFromProjectFiles(t *testing.T, root, marker string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, []byte(marker)) {
			t.Errorf("raw fixture value was persisted in %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect project fixture files: %v", err)
	}
}
