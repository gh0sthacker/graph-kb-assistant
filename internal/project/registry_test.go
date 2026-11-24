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

func TestObserveProjectInventoryRequiresRepeatedSuccessfulOmissions(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	removed := CaidoProjectRef{InstanceID: "caido", ProjectID: "removed"}
	other := CaidoProjectRef{InstanceID: "other-instance", ProjectID: "keep"}
	if err := RegisterProject(removed, root); err != nil {
		t.Fatal(err)
	}
	if err := RegisterProject(other, root); err != nil {
		t.Fatal(err)
	}

	for confirmation := 1; confirmation < 3; confirmation++ {
		missing, err := ObserveProjectInventory(context.Background(), root, "caido", nil, 3)
		if err != nil || len(missing) != 0 {
			t.Fatalf("confirmation %d: missing=%v err=%v", confirmation, missing, err)
		}
	}
	missing, err := ObserveProjectInventory(context.Background(), root, "caido", nil, 3)
	if err != nil || len(missing) != 1 || !missing[0].SameProject(removed) {
		t.Fatalf("third omission: missing=%v err=%v", missing, err)
	}
	missing, err = ObserveProjectInventory(context.Background(), root, "caido", nil, 3)
	if err != nil || len(missing) != 0 {
		t.Fatalf("continued omission re-emitted absent transition: missing=%v err=%v", missing, err)
	}
	refs, err := LoadProjectRefs(root)
	if err != nil || len(refs) != 2 {
		t.Fatalf("source absence must retain both projects in the local history catalog: refs=%v err=%v", refs, err)
	}

	if err := UnregisterProject(context.Background(), removed, root); err != nil {
		t.Fatal(err)
	}
	refs, err = LoadProjectRefs(root)
	if err != nil || len(refs) != 1 || !refs[0].SameProject(other) {
		t.Fatalf("registry after purge: refs=%v err=%v", refs, err)
	}
}

func TestObserveProjectInventoryResetsWhenProjectReturnsAndRejectsIncompleteRows(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := CaidoProjectRef{InstanceID: "caido", ProjectID: "flapping"}
	if err := RegisterProject(ref, root); err != nil {
		t.Fatal(err)
	}
	if missing, err := ObserveProjectInventory(context.Background(), root, "caido", nil, 3); err != nil || len(missing) != 0 {
		t.Fatalf("first omission: missing=%v err=%v", missing, err)
	}
	if _, err := ObserveProjectInventory(context.Background(), root, "caido", []CaidoProjectRef{{InstanceID: "wrong", ProjectID: "x"}}, 3); err == nil {
		t.Fatal("inventory from a different instance was accepted")
	}
	if missing, err := ObserveProjectInventory(context.Background(), root, "caido", []CaidoProjectRef{ref}, 3); err != nil || len(missing) != 0 {
		t.Fatalf("project return: missing=%v err=%v", missing, err)
	}
	for i := 0; i < 2; i++ {
		if missing, err := ObserveProjectInventory(context.Background(), root, "caido", nil, 3); err != nil || len(missing) != 0 {
			t.Fatalf("post-return omission %d: missing=%v err=%v", i+1, missing, err)
		}
	}
	missing, err := ObserveProjectInventory(context.Background(), root, "caido", nil, 3)
	if err != nil || len(missing) != 1 || !missing[0].SameProject(ref) {
		t.Fatalf("post-return third omission: missing=%v err=%v", missing, err)
	}
}
