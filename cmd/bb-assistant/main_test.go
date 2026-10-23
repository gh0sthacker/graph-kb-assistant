package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

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
		{"--max-pages", "1001"},
		{"--max-requests", "100001"},
	} {
		commandArgs := append([]string{"--project-key", key}, args...)
		if err := reconcileCaidoHistory(commandArgs); err == nil {
			t.Errorf("caido-reconcile accepted invalid bounds %v", args)
		}
	}
}
