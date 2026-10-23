package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

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
