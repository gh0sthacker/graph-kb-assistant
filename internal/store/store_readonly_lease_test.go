package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
)

func readonlyFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "graph.sqlite3")
	if err := initializeEmptyDatabase(context.Background(), path, "synthetic-project"); err != nil {
		t.Fatalf("initialize synthetic database: %v", err)
	}
	return dir, path
}

func directoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestOpenReadOnlyRejectsSQLiteSidecarsWithoutChangingDirectory(t *testing.T) {
	for _, suffix := range []string{"-wal", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			dir, path := readonlyFixture(t)
			sidecar := path + suffix
			marker := []byte("synthetic uncheckpointed SQLite sidecar")
			if err := os.WriteFile(sidecar, marker, 0o600); err != nil {
				t.Fatal(err)
			}
			before := directoryNames(t, dir)

			opened, err := OpenReadOnly(context.Background(), path)
			if opened != nil {
				_ = opened.Close()
				t.Fatal("opened database while a SQLite sidecar was present")
			}
			if err == nil {
				t.Fatal("expected fail-closed rejection for a SQLite sidecar")
			}
			if after := directoryNames(t, dir); !reflect.DeepEqual(after, before) {
				t.Fatalf("read-only rejection changed directory entries: before=%v after=%v", before, after)
			}
			got, readErr := os.ReadFile(sidecar)
			if readErr != nil || !reflect.DeepEqual(got, marker) {
				t.Fatalf("sidecar changed during rejected open: data=%q err=%v", got, readErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			writer, lockErr := projectlock.Acquire(ctx, dir)
			if lockErr != nil {
				t.Fatalf("failed open leaked its read lease: %v", lockErr)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenReadOnlyReleasesLeaseWhenDatabaseIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := OpenReadOnly(context.Background(), filepath.Join(dir, "missing.sqlite3"))
	if err == nil {
		t.Fatal("opened a missing database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	writer, err := projectlock.Acquire(ctx, dir)
	if err != nil {
		t.Fatalf("failed open leaked its read lease: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyHoldsSharedProjectLeaseUntilClose(t *testing.T) {
	dir, path := readonlyFixture(t)
	opened, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open read-only database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	writer, err := projectlock.Acquire(ctx, dir)
	if writer != nil {
		_ = writer.Close()
		t.Fatal("writer acquired project lease while read-only store remained open")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer did not wait for read lease: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close read-only store: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	writer, err = projectlock.Acquire(ctx, dir)
	if err != nil {
		t.Fatalf("writer did not acquire lease after store close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyUnderMaintenanceRequiresMatchingProjectLease(t *testing.T) {
	lockedDir, _ := readonlyFixture(t)
	_, otherDatabase := readonlyFixture(t)
	lease, err := projectlock.Acquire(context.Background(), lockedDir)
	if err != nil {
		t.Fatalf("acquire unrelated project lease: %v", err)
	}
	defer lease.Close()

	opened, err := OpenReadOnlyUnderMaintenance(context.Background(), otherDatabase, lease)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("opened database under a lease for a different project")
	}
	if err == nil {
		t.Fatal("expected a mismatched project lease to be rejected")
	}
}

func TestOpenReadOnlyDoesNotCreateSQLiteOrLockSidecars(t *testing.T) {
	dir, path := readonlyFixture(t)
	before := directoryNames(t, dir)
	opened, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open read-only database: %v", err)
	}
	if during := directoryNames(t, dir); !reflect.DeepEqual(during, before) {
		_ = opened.Close()
		t.Fatalf("read-only open created files: before=%v during=%v", before, during)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if after := directoryNames(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only close created files: before=%v after=%v", before, after)
	}
}

func TestWriteStoreCloseCleansWALSidecarsForReadOnlyReaders(t *testing.T) {
	dir, path := readonlyFixture(t)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := OpenForWrite(context.Background(), path)
	if err != nil {
		t.Fatalf("open writable store: %v", err)
	}
	if err := writer.DB().PingContext(context.Background()); err != nil {
		_ = writer.Close()
		t.Fatalf("initialize writable connection: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writable store: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("writable store left %s after close: %v", suffix, err)
		}
	}
	opened, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("read-only open after writer close: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if names := directoryNames(t, dir); len(names) != 1 || names[0] != "graph.sqlite3" {
		t.Fatalf("writer close left unexpected files: %v", names)
	}
}

func TestOpenProjectReadOnlyUnderMaintenanceUsesExistingExclusiveLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := project.CaidoProjectRef{InstanceID: "synthetic-instance", ProjectID: "maintenance-open"}
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	projectsDir := filepath.Join(root, "projects")
	projectDir := filepath.Join(projectsDir, key)
	for _, directory := range []string{
		projectsDir,
		projectDir,
		filepath.Join(projectDir, "vault"),
		filepath.Join(projectDir, "behavior-receipts"),
	} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := initializeEmptyDatabase(ctx, filepath.Join(projectDir, "graph.sqlite3"), key); err != nil {
		t.Fatal(err)
	}
	directory, err := ProjectDirectory(ref, root)
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := projectlock.Acquire(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	opened, err := OpenProjectReadOnlyUnderMaintenance(ctx, ref, root, maintenance)
	if err != nil {
		t.Fatalf("open under existing maintenance lease: %v", err)
	}
	if !opened.Database.ReadOnly() {
		t.Fatal("database opened writable under a maintenance read")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}
