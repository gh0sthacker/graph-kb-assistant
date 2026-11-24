package mcpserver

import (
	"context"
	"path/filepath"
	"testing"

	"bb-graph-assistant/internal/project"
)

func TestRefreshProjectCatalogTracksCaidoRegistryChanges(t *testing.T) {
	root := t.TempDir()
	tools, err := NewTools(root, filepath.Join(root, "missing-kb.sqlite3"))
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	defer tools.Close()

	first := project.CaidoProjectRef{InstanceID: "refresh-fixture", ProjectID: "first"}
	if err := project.RegisterProject(first, root); err != nil {
		t.Fatalf("RegisterProject(first): %v", err)
	}
	if err := tools.RefreshProjectCatalog(); err != nil {
		t.Fatalf("RefreshProjectCatalog(add): %v", err)
	}
	firstKey, _ := first.StorageKey()
	listed, err := tools.GraphExplorerProjects(context.Background(), "", 10)
	if err != nil || listed["total_project_count"] != 1 {
		t.Fatalf("added project not visible: listed=%v err=%v", listed, err)
	}
	if _, ok := tools.projectRef(firstKey); !ok {
		t.Fatal("project lookup did not refresh after add")
	}

	second := project.CaidoProjectRef{InstanceID: "refresh-fixture", ProjectID: "second"}
	if err := project.RegisterProject(second, root); err != nil {
		t.Fatalf("RegisterProject(second): %v", err)
	}
	if err := tools.RefreshProjectCatalog(); err != nil {
		t.Fatalf("RefreshProjectCatalog(rename/add): %v", err)
	}
	listed, err = tools.GraphExplorerProjects(context.Background(), "", 10)
	if err != nil || listed["total_project_count"] != 2 {
		t.Fatalf("second project not visible: listed=%v err=%v", listed, err)
	}

	if err := project.UnregisterProject(context.Background(), first, root); err != nil {
		t.Fatalf("UnregisterProject(first): %v", err)
	}
	if err := tools.RefreshProjectCatalog(); err != nil {
		t.Fatalf("RefreshProjectCatalog(remove): %v", err)
	}
	listed, err = tools.GraphExplorerProjects(context.Background(), "", 10)
	if err != nil || listed["total_project_count"] != 1 {
		t.Fatalf("removed project remains visible: listed=%v err=%v", listed, err)
	}
	if _, ok := tools.projectRef(firstKey); ok {
		t.Fatal("removed project remains addressable")
	}
}
