package runtimeaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestBuildStatusSummarizesLocalCoverageWithoutContactingServices(t *testing.T) {
	t.Setenv("BB_ASSISTANT_NEO4J_URI", "bolt://127.0.0.1:9999")
	t.Setenv("BB_ASSISTANT_NEO4J_USERNAME", "private-user-marker")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "private-password-marker")
	t.Setenv("BB_ASSISTANT_NEO4J_DATABASE", "neo4j")
	root := filepath.Join(t.TempDir(), "data")
	displayName := "private-project-name-marker"
	ref := project.CaidoProjectRef{InstanceID: "status-instance", ProjectID: "status-project", DisplayName: &displayName}
	projectStore, err := store.CreateProject(t.Context(), ref, root)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	key := projectStore.ProjectKey
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	now := time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC)
	report, err := Build(t.Context(), root, "neo4j", now)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if report.GeneratedAt != now || report.OverallState != "partial" || report.ProjectRegistryState != "readable" {
		t.Fatalf("unexpected top-level status: %+v", report)
	}
	if report.GraphBackend.Name != "neo4j" || report.GraphBackend.Configuration != "configured" || report.GraphBackend.Connectivity != "not_checked" {
		t.Fatalf("graph backend status = %+v", report.GraphBackend)
	}
	if report.RegisteredProjects != 1 || report.AuditedProjects != 1 || report.UninspectedProjects != 0 || len(report.Projects) != 1 {
		t.Fatalf("project audit counts are wrong: %+v", report)
	}
	got := report.Projects[0]
	if got.ProjectKey != key || got.CaptureStoreState != "compatible" || got.CoverageState != "partial" || got.HistoryReconciled {
		t.Fatalf("project status is incomplete or overstated: %+v", got)
	}
	if got.ProjectionQueue.State != "compatible" || got.ProjectionQueue.Pending != 0 {
		t.Fatalf("projection queue status = %+v", got.ProjectionQueue)
	}
	if report.Capabilities.CaidoLiveIntake != "not checked; Caido was not contacted" ||
		report.Capabilities.ModelExecution != "not invoked" || report.Capabilities.CapturedBodiesRead {
		t.Fatalf("status overstated unverified capabilities: %+v", report.Capabilities)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, marker := range []string{"private-user-marker", "private-password-marker", "private-project-name-marker", "status-project"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("runtime status exposed private value marker %q", marker)
		}
	}
}

func TestBuildStatusForEmptyRegistryRemainsPartial(t *testing.T) {
	root := t.TempDir()
	report, err := Build(t.Context(), root, "sqlite", time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if report.RegisteredProjects != 0 || report.AuditedProjects != 0 || report.OverallState != "partial" {
		t.Fatalf("empty registry status = %+v", report)
	}
	if report.ProjectRegistryState != "not_initialized" {
		t.Fatalf("missing project registry state = %q, want not_initialized", report.ProjectRegistryState)
	}
	if report.GraphBackend.Configuration != "not_required" || report.GraphBackend.Connectivity != "not_applicable" {
		t.Fatalf("SQLite backend status = %+v", report.GraphBackend)
	}
}

func TestBuildStatusDistinguishesInitializedEmptyRegistry(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "projects"), 0o700); err != nil {
		t.Fatalf("create projects directory: %v", err)
	}
	report, err := Build(t.Context(), root, "sqlite", time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if report.ProjectRegistryState != "empty" {
		t.Fatalf("project registry state = %q, want empty", report.ProjectRegistryState)
	}
}
