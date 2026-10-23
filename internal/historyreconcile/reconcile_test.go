package historyreconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

type inventoryFixture struct {
	project       caido.ProjectRef
	pages         []caido.RequestMetadataPage
	currentErr    error
	pageErrAt     int
	pageErr       error
	currentCalls  int
	pageCalls     int
	seenCursors   []string
	seenPageSizes []int
}

func (s *inventoryFixture) CurrentProjectContext(ctx context.Context) (caido.ProjectRef, error) {
	if err := ctx.Err(); err != nil {
		return caido.ProjectRef{}, err
	}
	s.currentCalls++
	if s.currentErr != nil {
		return caido.ProjectRef{}, s.currentErr
	}
	return s.project, nil
}

func (s *inventoryFixture) ListRequestPageNoSwitchContext(ctx context.Context, selected caido.ProjectRef, cursor *string, pageSize int) (caido.RequestMetadataPage, error) {
	if err := ctx.Err(); err != nil {
		return caido.RequestMetadataPage{}, err
	}
	s.pageCalls++
	s.seenPageSizes = append(s.seenPageSizes, pageSize)
	if cursor == nil {
		s.seenCursors = append(s.seenCursors, "")
	} else {
		s.seenCursors = append(s.seenCursors, *cursor)
	}
	if !selected.SameProject(s.project) {
		return caido.RequestMetadataPage{}, errors.New("fixture rejects a different project")
	}
	if s.pageErrAt == s.pageCalls && s.pageErr != nil {
		return caido.RequestMetadataPage{}, s.pageErr
	}
	index := s.pageCalls - 1
	if index >= len(s.pages) {
		return caido.RequestMetadataPage{}, errors.New("fixture has no page at this cursor")
	}
	return s.pages[index], nil
}

type projectFixture struct {
	root         string
	key          string
	ref          project.CaidoProjectRef
	databasePath string
}

func newProjectFixture(t *testing.T) projectFixture {
	t.Helper()
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
	projectStore, err := store.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	fixture := projectFixture{
		root:         root,
		key:          projectStore.ProjectKey,
		ref:          ref,
		databasePath: filepath.Join(projectStore.Directory, "graph.sqlite3"),
	}
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close fixture project store: %v", err)
	}
	return fixture
}

func (f projectFixture) insertHistorical(t *testing.T, requestIDs ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", f.databasePath)
	if err != nil {
		t.Fatalf("open fixture capture database: %v", err)
	}
	defer db.Close()
	for index, requestID := range requestIDs {
		_, err := db.Exec("INSERT INTO captures (\n"+
			"observation_id, caido_instance_id, caido_project_id, request_id,\n"+
			"request_sha256, request_size, request_vault_ref\n"+
			") VALUES (?, ?, ?, ?, 'fixture-digest', 1, 'fixture-vault-ref')",
			"historical-observation-"+requestID, f.ref.InstanceID, f.ref.ProjectID, requestID)
		if err != nil {
			t.Fatalf("insert historical fixture row %d: %v", index, err)
		}
	}
}

func (f projectFixture) insertLive(t *testing.T, requestIDs ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", f.databasePath)
	if err != nil {
		t.Fatalf("open fixture capture database: %v", err)
	}
	defer db.Close()
	for index, requestID := range requestIDs {
		_, err := db.Exec("INSERT INTO live_capture_index (\n"+
			"observation_id, caido_instance_id, caido_project_id, request_id,\n"+
			"graph_observation_id, synced_at, method, host, port, route,\n"+
			"request_content_type, response_content_type, request_size,\n"+
			"request_signature, request_url_key, field_names_json, header_names_json, parse_state\n"+
			") VALUES (?, ?, ?, ?, ?, 'fixture-time', 'GET', 'fixture.invalid', 443, '/',\n"+
			"'', '', 0, 'fixture-signature', 'fixture-url-key', '[]', '[]', 'complete')",
			"live-observation-"+requestID, f.ref.InstanceID, f.ref.ProjectID, requestID,
			"live-graph-observation-"+requestID)
		if err != nil {
			t.Fatalf("insert live fixture row %d: %v", index, err)
		}
	}
}

func (f projectFixture) digest(t *testing.T) [32]byte {
	t.Helper()
	contents, err := os.ReadFile(f.databasePath)
	if err != nil {
		t.Fatalf("read fixture database for digest: %v", err)
	}
	return sha256.Sum256(contents)
}

func fixtureRequest(id string) caido.RequestMetadata {
	return caido.RequestMetadata{RequestID: id, Method: "GET", URL: "https://fixture.invalid/"}
}

func page(requestIDs []string, next *string, hasMore bool) caido.RequestMetadataPage {
	requests := make([]caido.RequestMetadata, 0, len(requestIDs))
	for _, id := range requestIDs {
		requests = append(requests, fixtureRequest(id))
	}
	return caido.RequestMetadataPage{Requests: requests, NextCursor: next, HasMore: hasMore}
}

func fixtureOptions() Options {
	return Options{PageSize: 3, MaxPages: 10, MaxRequestIDs: 100}
}

func TestReconciliationDurationUsesBoundedOverride(t *testing.T) {
	if got := reconciliationDuration(0); got != 30*time.Second {
		t.Fatalf("default reconciliation deadline = %s, want 30s", got)
	}
	if got := reconciliationDuration(5 * time.Minute); got != 5*time.Minute {
		t.Fatalf("configured reconciliation deadline = %s, want 5m", got)
	}
	if err := validateOptions(Options{PageSize: 1, MaxPages: 1, MaxRequestIDs: 1, MaxDuration: 31 * time.Minute}); err == nil {
		t.Fatal("accepted reconciliation deadline above the 30-minute limit")
	}
}

func TestReconciliationBoundsSupportLargeInventoriesWithinCaps(t *testing.T) {
	options := fixtureOptions()
	options.MaxPages = 10000
	options.MaxRequestIDs = 1000000
	if err := validateOptions(options); err != nil {
		t.Fatalf("validateOptions() rejected supported bounded inventory: %v", err)
	}

	options.MaxPages++
	if err := validateOptions(options); err == nil {
		t.Fatal("validateOptions() accepted page count above 10000")
	}
	options = fixtureOptions()
	options.MaxRequestIDs = 1000001
	if err := validateOptions(options); err == nil {
		t.Fatal("validateOptions() accepted request count above 1000000")
	}
}

func runFixture(t *testing.T, fixture projectFixture, source *inventoryFixture, options Options) (Report, error) {
	t.Helper()
	return Run(context.Background(), fixture.root, fixture.key, source, options)
}

func TestRunComparesPagesAndHistoricalLiveUnionWithoutChangingDatabase(t *testing.T) {
	fixture := newProjectFixture(t)
	fixture.insertHistorical(t, "fixture-historical-match", "fixture-local-only", "fixture-overlap")
	fixture.insertLive(t, "fixture-live-match", "fixture-overlap")
	before := fixture.digest(t)
	cursor := "fixture-cursor-1"
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages: []caido.RequestMetadataPage{
			page([]string{"fixture-historical-match", "fixture-caido-only-1", "fixture-live-match"}, &cursor, true),
			page([]string{"fixture-overlap", "fixture-caido-only-2"}, nil, false),
		},
	}

	report, err := runFixture(t, fixture, source, fixtureOptions())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.State != "partial" || report.CoverageReconciled || !report.EnumerationTerminal {
		t.Errorf("terminal inventory report = %+v; want terminal enumeration with unreconciled PARTIAL coverage", report)
	}
	if report.PagesCompleted != 2 || report.CaidoRequestCount != 5 || report.LocalRequestCount != 4 ||
		report.MatchedCount != 3 || report.CaidoOnlyCount != 2 || report.LocalOnlyCount != 1 {
		t.Errorf("inventory counts = %+v; want pages=2 Caido=5 local=4 matched=3 Caido-only=2 local-only=1", report)
	}
	if report.DuplicateCount != 0 {
		t.Errorf("duplicate count = %d; want 0", report.DuplicateCount)
	}
	if !reflectEqualStrings(source.seenCursors, []string{"", "fixture-cursor-1"}) {
		t.Errorf("page cursors = %#v; want first page and advancing cursor", source.seenCursors)
	}
	for _, pageSize := range source.seenPageSizes {
		if pageSize != fixtureOptions().PageSize {
			t.Errorf("page size = %d; want %d", pageSize, fixtureOptions().PageSize)
		}
	}
	assertNoRequestIDsInReport(t, report,
		"fixture-historical-match", "fixture-local-only", "fixture-overlap",
		"fixture-live-match", "fixture-caido-only-1", "fixture-caido-only-2")
	if after := fixture.digest(t); after != before {
		t.Fatal("Run() changed the local capture database")
	}
}

func TestRunReportsDuplicateRequestIDsAcrossPages(t *testing.T) {
	fixture := newProjectFixture(t)
	cursor := "fixture-cursor-repeat"
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages: []caido.RequestMetadataPage{
			page([]string{"fixture-duplicate-id"}, &cursor, true),
			page([]string{"fixture-new-before-duplicate", "fixture-duplicate-id"}, nil, false),
		},
	}

	report, err := runFixture(t, fixture, source, fixtureOptions())
	if err != nil {
		t.Fatalf("Run() error = %v; duplicate inventory should be reported as partial coverage", err)
	}
	if report.State != "partial" || report.CoverageReconciled || report.EnumerationTerminal ||
		report.DuplicateCount != 1 || report.PagesCompleted != 1 || report.CaidoRequestCount != 1 {
		t.Errorf("duplicate inventory report = %+v; want a partial report with one duplicate and no reconciliation", report)
	}
	assertNoRequestIDsInReport(t, report, "fixture-duplicate-id", "fixture-new-before-duplicate")
}

func TestRunRejectsCursorCycle(t *testing.T) {
	fixture := newProjectFixture(t)
	cursorOne := "fixture-cursor-one"
	cursorTwo := "fixture-cursor-two"
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages: []caido.RequestMetadataPage{
			page([]string{"fixture-request-one"}, &cursorOne, true),
			page([]string{"fixture-request-two"}, &cursorTwo, true),
			page([]string{"fixture-request-three"}, &cursorOne, true),
		},
	}

	report, err := runFixture(t, fixture, source, fixtureOptions())
	if err == nil {
		t.Fatal("Run() accepted a repeated cursor cycle")
	}
	if report.State != "partial" || report.EnumerationTerminal || report.CoverageReconciled {
		t.Errorf("cursor-cycle report = %+v; want partial and unreconciled", report)
	}
	if report.PagesCompleted > 3 {
		t.Errorf("pages completed = %d; cycle detection should stop within three pages", report.PagesCompleted)
	}
	assertNoRequestIDsInReport(t, report, "fixture-request-one", "fixture-request-two", "fixture-request-three")
}

func TestRunStopsAtPageLimitAndKeepsCoveragePartial(t *testing.T) {
	fixture := newProjectFixture(t)
	cursor := "fixture-next-page"
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages: []caido.RequestMetadataPage{
			page([]string{"fixture-page-limit-id"}, &cursor, true),
		},
	}
	options := fixtureOptions()
	options.MaxPages = 1

	report, err := runFixture(t, fixture, source, options)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.State != "partial" || report.PagesCompleted != 1 || report.EnumerationTerminal || report.CoverageReconciled {
		t.Errorf("page-limit report = %+v; want one page and partial unreconciled coverage", report)
	}
	if report.CaidoRequestCount != 1 {
		t.Errorf("Caido request count = %d; want only the bounded page", report.CaidoRequestCount)
	}
	assertNoRequestIDsInReport(t, report, "fixture-page-limit-id")
}

func TestRunStopsAtRequestIDLimit(t *testing.T) {
	fixture := newProjectFixture(t)
	cursor := "fixture-next-page"
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages: []caido.RequestMetadataPage{
			page([]string{"fixture-request-limit-id"}, &cursor, true),
		},
	}
	options := fixtureOptions()
	options.MaxRequestIDs = 1

	report, err := runFixture(t, fixture, source, options)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.State != "partial" || report.PagesCompleted != 1 || report.CaidoRequestCount != 1 ||
		report.EnumerationTerminal || report.CoverageReconciled {
		t.Errorf("request-limit report = %+v; want one request and partial unreconciled coverage", report)
	}
	assertNoRequestIDsInReport(t, report, "fixture-request-limit-id")
}

func TestRunStopsAtLocalRequestIDLimitWithoutListingCaidoPages(t *testing.T) {
	fixture := newProjectFixture(t)
	fixture.insertHistorical(t, "fixture-local-cap-one", "fixture-local-cap-two", "fixture-local-cap-three")
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages:   []caido.RequestMetadataPage{page([]string{"fixture-should-not-be-read"}, nil, false)},
	}
	options := fixtureOptions()
	options.MaxRequestIDs = 2

	report, err := runFixture(t, fixture, source, options)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.State != "partial" || report.StopReason != "local_request_limit" ||
		report.LocalInventoryComplete || report.LocalRequestCount != 3 || source.pageCalls != 0 {
		t.Errorf("local-limit report = %+v; page calls=%d; want capped local inventory without Caido page reads", report, source.pageCalls)
	}
	assertNoRequestIDsInReport(t, report,
		"fixture-local-cap-one", "fixture-local-cap-two", "fixture-local-cap-three", "fixture-should-not-be-read")
}

func TestRunHonorsCanceledContextWithoutListingPages(t *testing.T) {
	fixture := newProjectFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages:   []caido.RequestMetadataPage{page([]string{"fixture-canceled-id"}, nil, false)},
	}

	report, err := Run(ctx, fixture.root, fixture.key, source, fixtureOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v; want context.Canceled", err)
	}
	if report.State != "partial" || report.CoverageReconciled || report.PagesCompleted != 0 || source.pageCalls != 0 {
		t.Errorf("canceled report = %+v; page calls=%d; want partial with no page reads", report, source.pageCalls)
	}
	assertNoRequestIDsInReport(t, report, "fixture-canceled-id")
}

func TestRunRequiresCurrentProjectToMatchRegisteredProject(t *testing.T) {
	fixture := newProjectFixture(t)
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: "another-fixture-project"},
		pages:   []caido.RequestMetadataPage{page([]string{"fixture-wrong-project-id"}, nil, false)},
	}

	report, err := runFixture(t, fixture, source, fixtureOptions())
	if err == nil {
		t.Fatal("Run() accepted an inventory from a different current Caido project")
	}
	if report.State != "partial" || report.CoverageReconciled || source.pageCalls != 0 {
		t.Errorf("wrong-project report = %+v; page calls=%d; want partial without request listing", report, source.pageCalls)
	}
	assertNoRequestIDsInReport(t, report, "fixture-wrong-project-id")
}

func TestRunDoesNotPutRequestIDsIntoReport(t *testing.T) {
	fixture := newProjectFixture(t)
	fixture.insertHistorical(t, "raw-id-historical-secret-marker")
	source := &inventoryFixture{
		project: caido.ProjectRef{InstanceID: fixture.ref.InstanceID, ProjectID: fixture.ref.ProjectID},
		pages: []caido.RequestMetadataPage{
			page([]string{"raw-id-caido-secret-marker"}, nil, false),
		},
	}

	report, err := runFixture(t, fixture, source, fixtureOptions())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertNoRequestIDsInReport(t, report, "raw-id-historical-secret-marker", "raw-id-caido-secret-marker")
}

func assertNoRequestIDsInReport(t *testing.T, report Report, requestIDs ...string) {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal reconciliation report: %v", err)
	}
	for _, requestID := range requestIDs {
		if strings.Contains(string(encoded), requestID) {
			t.Errorf("reconciliation report contains raw request ID %q", requestID)
		}
	}
}

func reflectEqualStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
