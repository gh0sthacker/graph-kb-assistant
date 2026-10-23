package search

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestReadSurfaceCatalogGroupsWholeOperationsAndDeduplicatesSources(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "catalog-project"}
	root := t.TempDir()
	writable, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = writable.Close()
		}
	})
	for i := 0; i < 120; i++ {
		insertCatalogHistorical(t, ctx, writable, ref, fmt.Sprintf("request-%03d", i), fmt.Sprintf("%064x", i+1), "GET", "api.fixture", 443, "/items", "complete", `["id"]`, `["accept"]`, "")
	}
	insertCatalogHistorical(t, ctx, writable, ref, "request-post", fmt.Sprintf("%064x", 121), "POST", "api.fixture", 443, "/items", "partial", `[]`, `[]`, "")
	insertCatalogHistorical(t, ctx, writable, ref, "request-other", fmt.Sprintf("%064x", 122), "GET", "web.fixture", 80, "/", "not_parsed", `[]`, `[]`, "")
	// The historical record wins when the same Caido Request ID also appears in
	// the live index. A distinct live request remains visible in the catalog.
	insertCatalogLive(t, ctx, writable, ref, "request-000", fmt.Sprintf("%064x", 123), "GET", "ignored.fixture", 443, "/ignored", "complete")
	insertCatalogLive(t, ctx, writable, ref, "request-live", fmt.Sprintf("%064x", 124), "GET", "live.fixture", 8080, "/health", "partial")
	if err := writable.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	closed = true
	readonly, err := store.OpenProjectReadOnly(ctx, ref, root)
	if err != nil {
		t.Fatalf("OpenProjectReadOnly(): %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	page, err := ReadSurfaceCatalog(ctx, readonly, SurfaceCatalogRequest{Limit: 1})
	if err != nil {
		t.Fatalf("ReadSurfaceCatalog(): %v", err)
	}
	if len(page.Operations) != 1 || page.Operations[0].Host != "api.fixture" || page.Operations[0].Method != "GET" || page.Operations[0].Route != "/items" {
		t.Fatalf("first operation = %#v", page.Operations)
	}
	operation := page.Operations[0]
	if operation.ObservationCount != 120 || operation.CompleteParseCount != 120 || operation.FieldSampleCount != MaximumSurfaceFieldRows {
		t.Fatalf("whole-operation counts = %#v", operation)
	}
	if len(operation.FieldNames) != 1 || operation.FieldNames[0] != "id" || len(operation.HeaderNames) != 1 || operation.HeaderNames[0] != "accept" || operation.FieldCoverageState != "sample_limit_reached" {
		t.Fatalf("bounded field/header samples = %#v", operation)
	}
	if operation.ObservationCountScope != "local_structural_index" || !page.Truncated || page.NextAfter == nil {
		t.Fatalf("catalog paging metadata = %#v", page)
	}
	if page.TotalOperations != 4 || page.TotalObservations != 123 {
		t.Fatalf("filtered local totals = operations %d, observations %d; want 4, 123", page.TotalOperations, page.TotalObservations)
	}
	if page.RowsRead < 123 || page.SnapshotFingerprint == "" || page.Coverage != "partial" || len(page.CoverageGaps) == 0 {
		t.Fatalf("coverage/fingerprint metadata = %#v", page)
	}

	second, err := ReadSurfaceCatalog(ctx, readonly, SurfaceCatalogRequest{Limit: 1, After: page.NextAfter, SnapshotFingerprint: page.SnapshotFingerprint})
	if err != nil {
		t.Fatalf("second ReadSurfaceCatalog(): %v", err)
	}
	if len(second.Operations) != 1 || second.Operations[0].Method != "POST" || second.Operations[0].Route != "/items" {
		t.Fatalf("second operation = %#v", second.Operations)
	}
}

func TestReadSurfaceCatalogFiltersBeforePagingAndUsesHostPortMethodRouteOrder(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "catalog-filter-project"}
	root := t.TempDir()
	writable, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	for i, row := range []struct {
		method, host string
		port         int
		route        string
	}{
		{"GET", "z.fixture", 443, "/z"}, {"GET", "a.fixture", 443, "/b"},
		{"POST", "a.fixture", 443, "/a"}, {"GET", "a.fixture", 443, "/a"},
	} {
		insertCatalogHistorical(t, ctx, writable, ref, fmt.Sprintf("request-%d", i), fmt.Sprintf("%064x", i+1), row.method, row.host, row.port, row.route, "complete", `[]`, `[]`, "")
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	readonly, err := store.OpenProjectReadOnly(ctx, ref, root)
	if err != nil {
		t.Fatalf("OpenProjectReadOnly(): %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	page, err := ReadSurfaceCatalog(ctx, readonly, SurfaceCatalogRequest{Filter: " A.FIXTURE ", Limit: 1})
	if err != nil {
		t.Fatalf("ReadSurfaceCatalog(): %v", err)
	}
	if page.TotalOperations != 3 || len(page.Operations) != 1 || page.Operations[0].Route != "/a" || page.Operations[0].Method != "GET" {
		t.Fatalf("filtered first page = %#v", page)
	}
	if page.NextAfter == nil {
		t.Fatal("filtered result should have a continuation cursor")
	}
	second, err := ReadSurfaceCatalog(ctx, readonly, SurfaceCatalogRequest{Filter: "a.fixture", Limit: 1, After: page.NextAfter, SnapshotFingerprint: page.SnapshotFingerprint})
	if err != nil {
		t.Fatalf("second ReadSurfaceCatalog(): %v", err)
	}
	if len(second.Operations) != 1 || second.Operations[0].Method != "GET" || second.Operations[0].Route != "/b" {
		t.Fatalf("second filtered operation = %#v", second.Operations)
	}
}

func TestSurfaceCatalogQueryPlanUsesProjectIdentityIndexes(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "catalog-plan-project"}
	root := t.TempDir()
	writable, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	insertCatalogHistorical(t, ctx, writable, ref, "request-1", fmt.Sprintf("%064x", 1), "GET", "plan.fixture", 443, "/", "complete", `[]`, `[]`, "")
	if err := writable.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	readonly, err := store.OpenProjectReadOnly(ctx, ref, root)
	if err != nil {
		t.Fatalf("OpenProjectReadOnly(): %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readonly.Database.DB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+surfaceCatalogQuery, ref.InstanceID, ref.ProjectID, ref.InstanceID, ref.ProjectID, ref.InstanceID, ref.ProjectID)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	t.Logf("project %s query plan:\n%s", key[:12], plan)
	if !strings.Contains(strings.ToLower(plan), "index") || !strings.Contains(strings.ToLower(plan), "captures") || !strings.Contains(strings.ToLower(plan), "live_capture_index") {
		t.Fatalf("query plan does not use the project identity indexes for both local sources:\n%s", plan)
	}
}

func insertCatalogHistorical(t *testing.T, ctx context.Context, projectStore *store.ProjectStore, ref project.CaidoProjectRef, requestID, observationID, method, host string, port int, route, parseState, fields, headers, graphID string) {
	t.Helper()
	if graphID == "" {
		graphID = "observation-" + strings.Repeat("0", 64)
	}
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,request_size,request_vault_ref,parse_state)
		 VALUES(?,?,?,?,?,0,'fixture-vault-ref',?)`, observationID, ref.InstanceID, ref.ProjectID, requestID, strings.Repeat("a", 64), parseState); err != nil {
		t.Fatalf("insert historical capture: %v", err)
	}
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO capture_search(observation_id,method,host,port,route,response_status,request_content_type,response_content_type,field_names_json,header_names_json)
		 VALUES(?,?,?,?,?,200,'','',?,?)`, observationID, method, host, port, route, fields, headers); err != nil {
		t.Fatalf("insert historical search row: %v", err)
	}
	if graphID != "" {
		revision := strings.Repeat("b", 64)
		if _, err := projectStore.Database.DB().ExecContext(ctx, `UPDATE captures SET current_revision_id=? WHERE observation_id=?`, revision, observationID); err != nil {
			t.Fatalf("set revision: %v", err)
		}
		if _, err := projectStore.Database.DB().ExecContext(ctx, `INSERT INTO graph_capture_map(observation_id,revision_id,graph_observation_id,updated_at) VALUES(?,?,?,?)`, observationID, revision, graphID, "2026-10-23T00:00:00Z"); err != nil {
			t.Fatalf("insert graph mapping: %v", err)
		}
	}
}

func insertCatalogLive(t *testing.T, ctx context.Context, projectStore *store.ProjectStore, ref project.CaidoProjectRef, requestID, observationID, method, host string, port int, route, parseState string) {
	t.Helper()
	if _, err := projectStore.Database.DB().ExecContext(ctx,
		`INSERT INTO live_capture_index(observation_id,caido_instance_id,caido_project_id,request_id,graph_observation_id,synced_at,
		 method,host,port,route,response_status,request_content_type,response_content_type,request_size,response_size,
		 request_signature,response_signature,request_url_key,referer_url_key,redirect_url_key,field_names_json,header_names_json,parse_state)
		 VALUES(?,?,?,?,?,'2026-10-23T00:00:00Z',?,?,?,?,200,'','',0,NULL,?,NULL,?,NULL,NULL,'[]','[]',?)`,
		observationID, ref.InstanceID, ref.ProjectID, requestID, "observation-"+strings.Repeat("c", 64), method, host, port, route,
		strings.Repeat("d", 64), strings.Repeat("e", 64), parseState); err != nil {
		t.Fatalf("insert live surface row: %v", err)
	}
}
