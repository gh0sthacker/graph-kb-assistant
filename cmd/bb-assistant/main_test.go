package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestNormalizeTailScope(t *testing.T) {
	valid, err := normalizeTailScope("HTTPS", "API.FIXTURE.INVALID", 443, true)
	if err != nil {
		t.Fatalf("normalizeTailScope(valid): %v", err)
	}
	if valid == nil || valid.Scheme != "https" || valid.Host != "api.fixture.invalid" || valid.Port != 443 {
		t.Fatalf("normalized scope = %+v", valid)
	}
	if scope, err := normalizeTailScope("", "", 0, false); err != nil || scope != nil {
		t.Fatalf("optional empty scope = %+v, %v", scope, err)
	}
	for _, input := range []struct {
		scheme   string
		host     string
		port     int
		required bool
	}{
		{required: true},
		{scheme: "https", host: "api.fixture.invalid", required: true},
		{scheme: "ftp", host: "api.fixture.invalid", port: 443, required: true},
		{scheme: "https", host: "*.fixture.invalid", port: 443, required: true},
		{scheme: "https", host: "api.fixture.invalid", port: 65536, required: true},
	} {
		if _, err := normalizeTailScope(input.scheme, input.host, input.port, input.required); err == nil {
			t.Fatalf("normalizeTailScope(%+v) succeeded; want validation error", input)
		}
	}
}

func TestLiveTailScopePortDefault(t *testing.T) {
	t.Setenv("BB_ASSISTANT_CAIDO_SCOPE_PORT", "8443")
	port, err := liveTailScopePortDefault()
	if err != nil || port != 8443 {
		t.Fatalf("liveTailScopePortDefault() = %d, %v; want 8443", port, err)
	}
	t.Setenv("BB_ASSISTANT_CAIDO_SCOPE_PORT", "not-a-port")
	if _, err := liveTailScopePortDefault(); err == nil {
		t.Fatal("liveTailScopePortDefault accepted malformed environment input")
	}
}

func TestGraphSliceCLIUsesSharedQueryServiceAndPreservesContext(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "graph-slice-cli", ProjectID: "fixture-project"}
	projectStore, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	projectKey := projectStore.ProjectKey
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		_ = projectStore.Close()
		t.Fatalf("Acquire(): %v", err)
	}
	requestRaw := []byte("GET /items HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n")
	receiptPath := filepath.Join(projectStore.BehaviorReceiptPath, "cli-fixture.json")
	receipt := []byte(`{"fixture":true}`)
	if err := os.WriteFile(receiptPath, receipt, 0o600); err != nil {
		_ = maintenance.Close()
		_ = projectStore.Close()
		t.Fatalf("WriteFile(receipt): %v", err)
	}
	digest := sha256.Sum256(receipt)
	status := 200
	truncated := false
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: ref, RequestID: "cli-fixture-request"},
		RequestRaw: requestRaw, ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"),
		HasResponse: true, Method: "GET", Scheme: "https", Host: "fixture.invalid", Port: 443,
		Target: "https://fixture.invalid/items", ResponseStatus: &status,
		RequestTruncated: &truncated, ResponseTruncated: &truncated,
		RequestWireFidelity: "reconstructed_text", ResponseWireFidelity: "reconstructed_text",
	}
	bundle, endpointID, err := behaviorgraph.BuildHistoricalBundle(exchange, projectKey, behaviorgraph.Record{
		"path": receiptPath, "sha256": hex.EncodeToString(digest[:]), "record_locator": "$.records[0]",
	})
	if err != nil {
		_ = maintenance.Close()
		_ = projectStore.Close()
		t.Fatalf("BuildHistoricalBundle(): %v", err)
	}
	writer, err := behaviorgraph.OpenWriter(ctx, projectStore)
	if err != nil {
		_ = maintenance.Close()
		_ = projectStore.Close()
		t.Fatalf("OpenWriter(): %v", err)
	}
	if _, err := writer.ImportBundle(ctx, bundle); err != nil {
		_ = maintenance.Close()
		_ = projectStore.Close()
		t.Fatalf("ImportBundle(): %v", err)
	}
	if err := maintenance.Close(); err != nil {
		_ = projectStore.Close()
		t.Fatalf("Close maintenance lock: %v", err)
	}
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close project store: %v", err)
	}

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = writeEnd
	t.Cleanup(func() { os.Stdout = originalStdout })
	if err := readGraphSlice([]string{
		"--data-root", root, "--project-key", projectKey, "--seed", endpointID,
		"--context-json", `{"actor":"synthetic-actor"}`,
	}); err != nil {
		_ = writeEnd.Close()
		t.Fatalf("readGraphSlice(): %v", err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.NewDecoder(readEnd).Decode(&result); err != nil {
		t.Fatalf("graph-slice output is not JSON: %v", err)
	}
	_ = readEnd.Close()
	contextValue, ok := result["context"].(map[string]any)
	if !ok || contextValue["actor"] != "synthetic-actor" || result["graph_version"] != "behavior-graph-source-pinned-v1" {
		t.Fatalf("shared graph query result lost its context or metadata: %#v", result)
	}
	for _, pathMode := range []struct {
		path string
		mode os.FileMode
	}{
		{path: filepath.Join(root, "query-state"), mode: 0o700},
		{path: filepath.Join(root, "query-state", "full-cursors.sqlite3"), mode: 0o600},
	} {
		info, statErr := os.Stat(pathMode.path)
		if statErr != nil {
			t.Fatalf("stat persistent cursor state %q: %v", pathMode.path, statErr)
		}
		if info.Mode().Perm() != pathMode.mode {
			t.Fatalf("persistent cursor state %q permissions = %o, want %o", pathMode.path, info.Mode().Perm(), pathMode.mode)
		}
	}
}

func TestProjectBackupCommandsRequireExplicitResourceBudgets(t *testing.T) {
	if err := run([]string{"project-backup", "--project-key", strings.Repeat("a", 64), "--output", "backup.tar"}); err == nil {
		t.Fatal("project-backup accepted missing byte and file budgets")
	}
	if err := run([]string{"project-restore", "--input", "backup.tar", "--max-bytes", "1024"}); err == nil {
		t.Fatal("project-restore accepted a missing file budget")
	}
}

func TestProjectBackupRestoreCLIEmitsRestoredIdentityAndCapabilitySummary(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	ctx := t.Context()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-cli-fixture", ProjectID: "round-trip"}
	selected, err := store.CreateProject(ctx, ref, sourceRoot)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	projectKey, projectDir := selected.ProjectKey, selected.Directory
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(projectDir, "vault")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "synthetic.enc"), []byte("opaque synthetic payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "round-trip.tar")
	backupOutput := captureCommandStdout(t, func() error {
		return backupProject([]string{"--data-root", sourceRoot, "--project-key", projectKey, "--output", archive, "--max-bytes", "1048576", "--max-files", "32"})
	})
	var backupResult map[string]any
	if err := json.Unmarshal(backupOutput, &backupResult); err != nil || backupResult["state"] != "complete" {
		t.Fatalf("project-backup output=%s err=%v", backupOutput, err)
	}

	targetRoot := filepath.Join(t.TempDir(), "restored")
	restoreOutput := captureCommandStdout(t, func() error {
		return restoreProject([]string{"--input", archive, "--data-root", targetRoot, "--max-bytes", "1048576", "--max-files", "32"})
	})
	var restoreResult map[string]any
	if err := json.Unmarshal(restoreOutput, &restoreResult); err != nil {
		t.Fatalf("project-restore output is not JSON: %s err=%v", restoreOutput, err)
	}
	restoreReport, ok := restoreResult["restore"].(map[string]any)
	if !ok || restoreReport["state"] != "restored" || restoreReport["project_key"] != projectKey {
		t.Fatalf("restore output lost project identity: %#v", restoreResult)
	}
	if _, ok := restoreResult["capability_summary"].(map[string]any); !ok {
		t.Fatalf("restore output lacks the fresh capability summary: %#v", restoreResult)
	}
	if restoreResult["capability_summary_state"] == "unavailable" {
		t.Fatalf("synthetic restored project did not produce a fresh capability summary: %#v", restoreResult)
	}
	if strings.Contains(string(backupOutput)+string(restoreOutput), "opaque synthetic payload") {
		t.Fatal("backup command output disclosed a vault payload")
	}
}

func captureCommandStdout(t *testing.T, invoke func() error) []byte {
	t.Helper()
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writeEnd
	invokeErr := invoke()
	_ = writeEnd.Close()
	os.Stdout = original
	output, readErr := io.ReadAll(readEnd)
	_ = readEnd.Close()
	if invokeErr != nil {
		t.Fatalf("command failed: %v", invokeErr)
	}
	if readErr != nil {
		t.Fatalf("read command output: %v", readErr)
	}
	return output
}

type graphSliceCLIServiceFixture struct {
	graph    *graphaccess.Service
	requests *[]appquery.GraphRequest
	result   appquery.GraphResult
}

func (f *graphSliceCLIServiceFixture) Graph(_ context.Context, request appquery.GraphRequest) (appquery.GraphResult, error) {
	*f.requests = append(*f.requests, request)
	return f.result, nil
}

func (f *graphSliceCLIServiceFixture) Close(ctx context.Context) error {
	return f.graph.Close(ctx)
}

func TestGraphSliceCLIForwardsCursorAcrossFreshServiceInvocations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "graph-slice-cli-cursor", ProjectID: "fixture-project"}
	projectStore, err := store.CreateProject(t.Context(), ref, root)
	if err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	projectKey := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("close project store: %v", err)
	}
	seed := "endpoint-" + strings.Repeat("a", 64)
	token := strings.Repeat("A", 43)
	var requests []appquery.GraphRequest
	created := 0
	factory := func(_ string, _ []project.CaidoProjectRef, graph *graphaccess.Service) (graphSliceQueryService, error) {
		created++
		payload := map[string]any{"page": created}
		if created == 1 {
			payload["next_cursor"] = token
		} else {
			payload["continuation_branches_complete"] = true
		}
		return &graphSliceCLIServiceFixture{
			graph: graph, requests: &requests,
			result: appquery.GraphResult{ProjectKey: projectKey, View: appquery.GraphViewFull, Payload: payload},
		}, nil
	}

	baseArgs := []string{
		"--data-root", root, "--project-key", projectKey, "--seed", seed,
		"--max-nodes", "42", "--max-edges", "73", "--max-depth", "2",
		"--context-json", `{"actor":"synthetic-actor","tenant":"tenant-a"}`,
	}
	readResult := func(args []string) map[string]any {
		t.Helper()
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		originalStdout := os.Stdout
		os.Stdout = writeEnd
		err = readGraphSliceWithFactory(args, factory)
		os.Stdout = originalStdout
		if err != nil {
			_ = readEnd.Close()
			_ = writeEnd.Close()
			t.Fatalf("readGraphSliceWithFactory(): %v", err)
		}
		if err := writeEnd.Close(); err != nil {
			t.Fatalf("close graph-slice output: %v", err)
		}
		var result map[string]any
		if err := json.NewDecoder(readEnd).Decode(&result); err != nil {
			_ = readEnd.Close()
			t.Fatalf("decode graph-slice output: %v", err)
		}
		_ = readEnd.Close()
		return result
	}

	firstResult := readResult(baseArgs)
	if firstResult["next_cursor"] != token {
		t.Fatalf("first CLI page cursor = %#v, want %q", firstResult["next_cursor"], token)
	}
	secondArgs := append(append([]string(nil), baseArgs...), "--cursor", token)
	secondResult := readResult(secondArgs)
	if secondResult["continuation_branches_complete"] != true || created != 2 || len(requests) != 2 {
		t.Fatalf("second CLI invocation = %#v; service creations %d, requests %d", secondResult, created, len(requests))
	}
	wantContext := map[string]any{"actor": "synthetic-actor", "tenant": "tenant-a"}
	for i, request := range requests {
		wantCursor := appquery.GraphCursor("")
		if i == 1 {
			wantCursor = appquery.GraphCursor(token)
		}
		if request.ProjectKey != projectKey || request.View != appquery.GraphViewFull ||
			!reflect.DeepEqual(request.SeedIDs, []string{seed}) ||
			request.Budget != (appquery.GraphBudget{MaxNodes: 42, MaxEdges: 73, MaxDepth: 2}) ||
			!reflect.DeepEqual(request.Context, wantContext) || request.Cursor != wantCursor {
			t.Errorf("CLI request %d = %#v; project/seeds/budget/context/cursor mismatch", i, request)
		}
	}
}

func TestWriteCaidoImportProgressProducesOnlyBoundedSummary(t *testing.T) {
	var output bytes.Buffer
	progress := ingest.BackfillProgress{
		PagesThisRun: 2, PageCount: 12, Stored: 84, Replayed: 16,
		ProjectionPendingPages: 1, State: "running", CursorState: "continuing",
		MetadataDurationMS: 3, DetailDurationMS: 120, PageProcessingDurationMS: 450,
		GraphProjectionDurationMS: 210,
	}
	if err := writeCaidoImportProgress(&output, progress); err != nil {
		t.Fatalf("writeCaidoImportProgress() error = %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatalf("decode progress event: %v", err)
	}
	if event["event"] != "caido_import_page" || event["page_count"] != float64(12) || event["detail_duration_ms"] != float64(120) || event["page_processing_duration_ms"] != float64(450) || event["graph_projection_duration_ms"] != float64(210) {
		t.Fatalf("progress event omitted operational summary fields: %s", output.String())
	}
	if _, ok := event["cursor"]; ok {
		t.Fatalf("progress event exposes a raw cursor: %s", output.String())
	}
	for _, forbidden := range []string{"request_id", "request_raw", "response_raw", "authorization", "cookie"} {
		if strings.Contains(strings.ToLower(output.String()), forbidden) {
			t.Fatalf("progress event exposes a forbidden field %q: %s", forbidden, output.String())
		}
	}
}

func TestRegisterCaidoProjectAddsOnlyExplicitIdentity(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	if err := registerCaidoProject([]string{
		"--data-root", dataRoot,
		"--instance-id", "desktop-caido",
		"--project-id", "project-123",
		"--name", "Selected project",
	}); err != nil {
		t.Fatalf("registerCaidoProject() error = %v", err)
	}
	refs, err := project.LoadProjectRefs(dataRoot)
	if err != nil {
		t.Fatalf("LoadProjectRefs() error = %v", err)
	}
	if len(refs) != 1 || refs[0].InstanceID != "desktop-caido" || refs[0].ProjectID != "project-123" {
		t.Fatalf("registered project identity was not preserved")
	}
	if refs[0].DisplayName == nil || *refs[0].DisplayName != "Selected project" {
		t.Fatalf("registered display name was not preserved")
	}
	projectKey, err := refs[0].StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, filePath := range []string{
		filepath.Join(dataRoot, "projects", "registry.json"),
		filepath.Join(dataRoot, "projects", projectKey, "graph.sqlite3"),
	} {
		info, statErr := os.Stat(filePath)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("private file mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestRegisterCaidoProjectRejectsMissingIdentity(t *testing.T) {
	if err := registerCaidoProject([]string{"--instance-id", "desktop-caido"}); err == nil {
		t.Fatal("registerCaidoProject() accepted a missing project ID")
	}
}

func TestRegisterCaidoProjectDoesNotOverwriteExistingStore(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	args := []string{
		"--data-root", dataRoot,
		"--instance-id", "desktop-caido",
		"--project-id", "project-789",
	}
	if err := registerCaidoProject(args); err != nil {
		t.Fatalf("initial project registration failed: %v", err)
	}
	if err := registerCaidoProject(args); err == nil {
		t.Fatal("registerCaidoProject() overwrote an existing store")
	}
}

func TestRegisterCaidoProjectOutputIsJSON(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	originalStdout := os.Stdout
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writeEnd
	t.Cleanup(func() { os.Stdout = originalStdout })
	if err := registerCaidoProject([]string{
		"--data-root", dataRoot,
		"--instance-id", "desktop-caido",
		"--project-id", "project-456",
	}); err != nil {
		t.Fatalf("registerCaidoProject() error = %v", err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.NewDecoder(readEnd).Decode(&output); err != nil {
		t.Fatalf("register output is not JSON: %v", err)
	}
	_ = readEnd.Close()
	if output["project_id"] != "project-456" || output["instance_id"] != "desktop-caido" {
		t.Fatal("register output omitted the project identity")
	}
}

func TestProjectAuditCLIRequiresExactlyOneProjectKey(t *testing.T) {
	if err := auditProject([]string{"--project-key", "invalid"}); err == nil {
		t.Fatal("project-audit accepted an invalid project key")
	}
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := auditProject([]string{"--project-key", key, "--project-key", key}); err == nil {
		t.Fatal("project-audit accepted multiple project keys")
	}
}

func TestProjectAuditCLIOutputIsJSON(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "audit-cli-instance", ProjectID: "audit-cli-project"}
	projectStore, err := store.CreateProject(t.Context(), ref, dataRoot)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	key := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	originalStdout := os.Stdout
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writeEnd
	t.Cleanup(func() { os.Stdout = originalStdout })
	if err := auditProject([]string{"--project-key", key, "--data-root", dataRoot}); err != nil {
		t.Fatalf("auditProject() error = %v", err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.NewDecoder(readEnd).Decode(&output); err != nil {
		t.Fatalf("project-audit output is not JSON: %v", err)
	}
	_ = readEnd.Close()
	if output["project_key"] != key || output["overall_state"] != "partial" {
		t.Fatalf("unexpected project-audit output: %+v", output)
	}
}

func TestRuntimeStatusCLIOutputsPartialLocalCapabilitySummary(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "runtime-status-instance", ProjectID: "runtime-status-project"}
	projectStore, err := store.CreateProject(t.Context(), ref, dataRoot)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	key := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	originalStdout := os.Stdout
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writeEnd
	t.Cleanup(func() { os.Stdout = originalStdout })
	if err := runtimeStatus([]string{"--data-root", dataRoot}); err != nil {
		_ = writeEnd.Close()
		t.Fatalf("runtimeStatus() error = %v", err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.NewDecoder(readEnd).Decode(&output); err != nil {
		t.Fatalf("status output is not JSON: %v", err)
	}
	_ = readEnd.Close()
	if output["overall_state"] != "partial" || output["registered_projects"] != float64(1) || output["audited_projects"] != float64(1) {
		t.Fatalf("unexpected status output: %+v", output)
	}
	projects, ok := output["projects"].([]any)
	if !ok || len(projects) != 1 || projects[0].(map[string]any)["project_key"] != key {
		t.Fatalf("status omitted the local project audit: %+v", output["projects"])
	}
	capabilities, ok := output["capabilities"].(map[string]any)
	if !ok || capabilities["caido_live_intake"] != "not checked; Caido was not contacted" || capabilities["model_execution"] != "not invoked" {
		t.Fatalf("status overstated unverified capabilities: %+v", output["capabilities"])
	}
}

func TestRuntimeStatusCLIRejectsUnexpectedArguments(t *testing.T) {
	if err := runtimeStatus([]string{"unexpected"}); err == nil {
		t.Fatal("status accepted an unexpected positional argument")
	}
}

func TestProjectionWorkerCLIReportsEmptyQueueAsJSON(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "projection-worker-cli", ProjectID: "empty-queue"}
	projectStore, err := store.CreateProject(t.Context(), ref, dataRoot)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	key := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	originalStdout := os.Stdout
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writeEnd
	t.Cleanup(func() { os.Stdout = originalStdout })
	if err := runProjectionWorker([]string{"--project-key", key, "--data-root", dataRoot}); err != nil {
		t.Fatalf("runProjectionWorker() error = %v", err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	var output projectionWorkerDrainOutput
	if err := json.NewDecoder(readEnd).Decode(&output); err != nil {
		t.Fatalf("projection-worker output is not JSON: %v", err)
	}
	_ = readEnd.Close()
	if output.ProjectKey != key || !output.Idle || output.Processed != 0 || len(output.Jobs) != 1 || output.Jobs[0].State != "idle" {
		t.Fatalf("unexpected projection-worker output: %+v", output)
	}
}

func TestProjectionWorkerCLIAccountsCompletedResultsFromInterruptedBatch(t *testing.T) {
	output := projectionWorkerDrainOutput{Jobs: make([]ingest.ProjectionWorkerResult, 0)}
	results := []ingest.ProjectionWorkerResult{
		{State: "applied"},
		{State: "pending", ErrorCode: "worker_interrupted"},
		{State: "dead_letter", ErrorCode: "invalid_job"},
		{State: "error"},
	}

	batchIdle := recordProjectionWorkerResults(&output, results, true)
	if batchIdle || output.Processed != 4 || output.Applied != 1 || output.Retried != 1 ||
		output.DeadLetter != 1 || output.Failed != 1 || len(output.Jobs) != 0 {
		t.Fatalf("partial watch batch was not accounted for: idle=%t output=%+v", batchIdle, output)
	}
}

func TestProjectionWorkerCLIRejectsInvalidPollIntervalBeforeRegistryAccess(t *testing.T) {
	key := strings.Repeat("a", 64)
	for _, interval := range []string{"99ms", "10m1s"} {
		err := runProjectionWorker([]string{"--project-key", key, "--poll-interval", interval})
		if err == nil {
			t.Errorf("projection-worker accepted out-of-range poll interval %s", interval)
		}
	}
}

func TestWaitProjectionWorkerPollStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitProjectionWorkerPoll(ctx, time.Hour) {
		t.Fatal("projection-worker poll continued after cancellation")
	}
}

func TestIsWatchLeaseContentionOnlyRetriesLockWaitFailures(t *testing.T) {
	ctx := context.Background()
	for _, err := range []error{context.DeadlineExceeded, projectlock.ErrWaitTimeout} {
		if !isWatchLeaseContention(true, ctx, err) {
			t.Errorf("watch worker did not recognize lock contention %v", err)
		}
	}
	if isWatchLeaseContention(true, ctx, errors.New("project database is invalid")) {
		t.Fatal("watch worker treated a permanent project error as lock contention")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if isWatchLeaseContention(true, cancelled, context.DeadlineExceeded) {
		t.Fatal("watch worker retried lock contention after shutdown")
	}
}

func TestProjectionWorkerWatchRetriesProjectLockAndStopsOnCancellation(t *testing.T) {
	ctx := context.Background()
	dataRoot := filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "projection-watch", ProjectID: "lock-retry"}
	projectStore, err := store.CreateProject(ctx, ref, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	projectDirectory := projectStore.Directory
	if err := projectStore.Close(); err != nil {
		t.Fatal(err)
	}

	held, err := projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	busy := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		lock, err := acquireProjectionWorkerMaintenance(ctx, projectDirectory, true, 100*time.Millisecond, time.Minute, func() {
			select {
			case busy <- struct{}{}:
			default:
			}
		})
		if lock != nil {
			_ = lock.Close()
		}
		result <- err
	}()
	select {
	case <-busy:
	case <-time.After(2 * time.Second):
		t.Fatal("watch worker did not observe the held project lock")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("watch worker did not acquire the released project lock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch worker did not retry after the project lock was released")
	}

	held, err = projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancelBusy := make(chan struct{}, 1)
	cancelResult := make(chan error, 1)
	go func() {
		lock, err := acquireProjectionWorkerMaintenance(cancelCtx, projectDirectory, true, 100*time.Millisecond, time.Minute, func() {
			select {
			case cancelBusy <- struct{}{}:
			default:
			}
		})
		if lock != nil {
			_ = lock.Close()
		}
		cancelResult <- err
	}()
	select {
	case <-cancelBusy:
	case <-time.After(2 * time.Second):
		_ = held.Close()
		t.Fatal("watch worker did not enter its cancellable lock wait")
	}
	cancel()
	select {
	case err := <-cancelResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("watch lock wait cancellation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch worker did not stop after cancellation")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCaidoReconcileCLIValidatesProjectAndBoundsBeforeStartingBridge(t *testing.T) {
	if err := reconcileCaidoHistory([]string{"--project-key", "invalid"}); err == nil {
		t.Fatal("caido-reconcile accepted an invalid project key")
	}
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := reconcileCaidoHistory([]string{"--project-key", key, "--project-key", key}); err == nil {
		t.Fatal("caido-reconcile accepted multiple project keys")
	}
	for _, args := range [][]string{
		{"--page-size", "0"},
		{"--max-pages", "10001"},
		{"--max-requests", "1000001"},
	} {
		commandArgs := append([]string{"--project-key", key}, args...)
		if err := reconcileCaidoHistory(commandArgs); err == nil {
			t.Errorf("caido-reconcile accepted invalid bounds %v", args)
		}
	}
}

func TestParseCaidoReconcileOptionsAllowsObservedCatalogScale(t *testing.T) {
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	options, err := parseCaidoReconcileOptions([]string{
		"--project-key", key, "--max-pages", "10000", "--max-requests", "1000000", "--timeout", "30m",
	})
	if err != nil {
		t.Fatalf("parseCaidoReconcileOptions() rejected large bounded inventory: %v", err)
	}
	if options.maxPages != 10000 || options.maxRequests != 1000000 || options.timeout != 30*time.Minute {
		t.Fatalf("parsed inventory bounds = pages:%d requests:%d timeout:%s", options.maxPages, options.maxRequests, options.timeout)
	}
	for _, args := range [][]string{{"--max-pages", "10001"}, {"--max-requests", "1000001"}} {
		commandArgs := append([]string{"--project-key", key}, args...)
		if _, err := parseCaidoReconcileOptions(commandArgs); err == nil {
			t.Errorf("accepted inventory bound above the new cap: %v", args)
		}
	}
}

func TestParseCaidoReconcileOptionsRequiresExactHostAndPortTogether(t *testing.T) {
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	options, err := parseCaidoReconcileOptions([]string{
		"--project-key", key, "--scope-host", "192.168.1.149", "--scope-port", "3000",
	})
	if err != nil {
		t.Fatalf("parseCaidoReconcileOptions() rejected exact host/port scope: %v", err)
	}
	if options.scopeHost != "192.168.1.149" || options.scopePort != 3000 {
		t.Fatalf("parsed scope = %q:%d; want 192.168.1.149:3000", options.scopeHost, options.scopePort)
	}
	for _, args := range [][]string{
		{"--scope-host", "192.168.1.149"},
		{"--scope-port", "3000"},
		{"--scope-host", `192.168.1.149" OR req.method.eq:"POST`, "--scope-port", "3000"},
		{"--scope-host", "192.168.1.149", "--scope-port", "65536"},
	} {
		commandArgs := append([]string{"--project-key", key}, args...)
		if _, err := parseCaidoReconcileOptions(commandArgs); err == nil {
			t.Errorf("parseCaidoReconcileOptions() accepted invalid scope %v", args)
		}
	}
}

func TestParseCaidoReconcileOptionsAllowsBoundedLongerDeadline(t *testing.T) {
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	options, err := parseCaidoReconcileOptions([]string{"--project-key", key, "--timeout", "5m"})
	if err != nil {
		t.Fatalf("parseCaidoReconcileOptions() error = %v", err)
	}
	if options.timeout != 5*time.Minute {
		t.Fatalf("reconciliation timeout = %s, want 5m", options.timeout)
	}
}

func TestParseCaidoReconcileOptionsRejectsUnboundedDeadlines(t *testing.T) {
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, value := range []string{"0s", "500ms", "31m"} {
		if _, err := parseCaidoReconcileOptions([]string{"--project-key", key, "--timeout", value}); err == nil {
			t.Errorf("accepted reconciliation timeout %q", value)
		}
	}
}
