package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bb-graph-assistant/internal/projectlock"
)

func TestRegisterProjectWaitsForCatalogLease(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	projects, err := EnsurePrivateDirectory(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := projectlock.Acquire(context.Background(), projects)
	if err != nil {
		t.Fatal(err)
	}
	ref := CaidoProjectRef{InstanceID: "synthetic-instance", ProjectID: "catalog-lock"}
	done := make(chan error, 1)
	go func() { done <- RegisterProject(ref, root) }()
	select {
	case err := <-done:
		_ = lease.Close()
		t.Fatalf("project registration bypassed the active catalog lease: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("register project after catalog lease released: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("project registration did not continue after catalog lease release")
	}
	refs, err := LoadProjectRefs(root)
	if err != nil || len(refs) != 1 || !refs[0].SameProject(ref) {
		t.Fatalf("registered project refs = %+v, err=%v", refs, err)
	}
}
