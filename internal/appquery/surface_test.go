package appquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestSurfaceCursorBindsProjectFilterAndLocalIndexSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "surface-instance", ProjectID: "surface-project"}
	otherRef := project.CaidoProjectRef{InstanceID: "surface-instance", ProjectID: "other-surface-project"}
	createSurfaceFixtureProject(t, ctx, root, ref, 3)
	refKey, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := otherRef.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewGraphQueryService(root, []project.CaidoProjectRef{ref, otherRef}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService(): %v", err)
	}
	t.Cleanup(func() { _ = service.Close(ctx) })

	first, err := service.Surface(ctx, SurfaceRequest{ProjectKey: refKey, Filter: " API.FIXTURE ", Limit: 1})
	if err != nil {
		t.Fatalf("Surface(first): %v", err)
	}
	if first.Filter != "api.fixture" || first.TotalOperations != 3 || first.TotalObservations != 3 || len(first.Operations) != 1 || first.NextCursor == "" || !first.Truncated {
		t.Fatalf("first surface result = %#v", first)
	}
	wire := first.WireMap()
	if wire["next_after"] != nil || wire["next_cursor"] != first.NextCursor || wire["coverage_state"] != "partial" {
		t.Fatalf("surface wire contract = %#v", wire)
	}
	secondPage, err := service.Surface(ctx, SurfaceRequest{ProjectKey: refKey, Filter: "api.fixture", Cursor: first.NextCursor, Limit: 1})
	if err != nil {
		t.Fatalf("Surface(second page): %v", err)
	}
	if len(secondPage.Operations) != 1 || secondPage.Operations[0].Route != "/b" || secondPage.NextCursor == "" {
		t.Fatalf("second surface page = %#v", secondPage)
	}

	_, err = service.Surface(ctx, SurfaceRequest{ProjectKey: refKey, Filter: "different", Cursor: first.NextCursor, Limit: 1})
	assertSurfaceQueryError(t, err, ErrorStaleCursor)
	_, err = service.Surface(ctx, SurfaceRequest{ProjectKey: otherKey, Filter: "api.fixture", Cursor: first.NextCursor, Limit: 1})
	assertSurfaceQueryError(t, err, ErrorStaleCursor)

	writable, err := store.OpenProjectForWrite(ctx, ref, root)
	if err != nil {
		t.Fatalf("OpenProjectForWrite(): %v", err)
	}
	insertSurfaceIndexRow(t, ctx, writable, ref, "request-4", fmt.Sprintf("%064x", 4), "GET", "api.fixture", 443, "/c")
	if err := writable.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	_, err = service.Surface(ctx, SurfaceRequest{ProjectKey: refKey, Filter: "api.fixture", Cursor: first.NextCursor, Limit: 1})
	assertSurfaceQueryError(t, err, ErrorStaleCursor)
	restartedService, err := NewGraphQueryService(root, []project.CaidoProjectRef{ref, otherRef}, graphaccess.NewService())
	if err != nil {
		t.Fatalf("NewGraphQueryService(restarted): %v", err)
	}
	t.Cleanup(func() { _ = restartedService.Close(ctx) })
	_, err = restartedService.Surface(ctx, SurfaceRequest{ProjectKey: refKey, Filter: "api.fixture", Cursor: first.NextCursor, Limit: 1})
	assertSurfaceQueryError(t, err, ErrorStaleCursor)

	_, err = service.Surface(ctx, SurfaceRequest{ProjectKey: refKey, Filter: "api.fixture", Cursor: first.NextCursor + "x", Limit: 1})
	assertSurfaceQueryError(t, err, ErrorStaleCursor)
}

func TestSurfaceValidatesPageLimitsAndProjectBeforeOpeningDatabase(t *testing.T) {
	service, err := NewGraphQueryService(t.TempDir(), nil, graphaccess.NewService())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	for _, request := range []SurfaceRequest{
		{ProjectKey: "bad", Limit: 1},
		{ProjectKey: strings.Repeat("a", 64), Limit: 101},
		{ProjectKey: strings.Repeat("a", 64), Filter: strings.Repeat("x", 257), Limit: 1},
	} {
		_, err := service.Surface(context.Background(), request)
		assertSurfaceQueryError(t, err, ErrorInvalidInput)
	}
}

func createSurfaceFixtureProject(t *testing.T, ctx context.Context, root string, ref project.CaidoProjectRef, rows int) {
	t.Helper()
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	for i, route := range []string{"/a", "/b", "/c"}[:rows] {
		insertSurfaceIndexRow(t, ctx, projectStore, ref, fmt.Sprintf("request-%d", i+1), fmt.Sprintf("%064x", i+1), "GET", "api.fixture", 443, route)
	}
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
}

func insertSurfaceIndexRow(t *testing.T, ctx context.Context, projectStore *store.ProjectStore, ref project.CaidoProjectRef, requestID, observationID, method, host string, port int, route string) {
	t.Helper()
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,request_size,request_vault_ref,parse_state)
		 VALUES(?,?,?,?,?,0,'fixture-vault-ref','complete')`, observationID, ref.InstanceID, ref.ProjectID, requestID, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("insert capture: %v", err)
	}
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO capture_search(observation_id,method,host,port,route,response_status,request_content_type,response_content_type,field_names_json,header_names_json)
		 VALUES(?,?,?,?,?,200,'','application/json','["id"]','["accept"]')`, observationID, method, host, port, route); err != nil {
		t.Fatalf("insert capture search row: %v", err)
	}
}

func assertSurfaceQueryError(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != code {
		t.Fatalf("error = %v, want typed %s", err, code)
	}
}
