package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
)

func TestPurgeProjectFilesRequiresLeaseAndPreservesLockInode(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := project.CaidoProjectRef{InstanceID: "test", ProjectID: "purge"}
	selected, err := CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatal(err)
	}
	directory := selected.Directory
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "extra.data"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PurgeProjectFilesUnderMaintenance(ref, root, nil); err == nil {
		t.Fatal("purge accepted a missing maintenance lease")
	}
	maintenance, err := projectlock.Acquire(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := PurgeProjectFilesUnderMaintenance(ref, root, maintenance); err != nil {
		_ = maintenance.Close()
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".maintenance.lock")); err != nil {
		_ = maintenance.Close()
		t.Fatalf("maintenance lock was removed: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".maintenance.lock" {
		_ = maintenance.Close()
		t.Fatalf("remaining project entries=%v err=%v", entries, err)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatal(err)
	}
}
