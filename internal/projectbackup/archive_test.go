package projectbackup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestBackupRestorePreservesProjectFilesAndIdentity(t *testing.T) {
	ctx := context.Background()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "round-trip"}
	selected, err := store.CreateProject(ctx, ref, sourceRoot)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	sourceDir := selected.Directory
	if err := selected.Close(); err != nil {
		t.Fatalf("close source project: %v", err)
	}
	files := map[string]string{
		"vault/request.enc":                  "synthetic-encrypted-payload-v1",
		"behavior.sqlite3":                   "synthetic graph projection",
		"behavior-receipts/receipt-001.json": `{"receipt":"synthetic"}`,
		"consultation.sqlite3":               "synthetic analyst session store",
	}
	for relative, content := range files {
		writeFixtureFile(t, sourceDir, relative, []byte(content))
	}
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "project.tar")
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 16}
	backup, err := Backup(ctx, sourceRoot, key, archivePath, limits)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backup.State != "complete" || backup.FileCount != len(files)+1 || backup.TotalBytes <= 0 {
		t.Fatalf("backup report = %#v", backup)
	}
	archiveInfo, err := os.Stat(archivePath)
	if err != nil || archiveInfo.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode info=%v err=%v; want 0600", archiveInfo, err)
	}

	targetRoot := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(ctx, archivePath, targetRoot, limits)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.State != "restored" || restored.ProjectKey != key || restored.FileCount != backup.FileCount || restored.TotalBytes != backup.TotalBytes {
		t.Fatalf("restore report = %#v; backup=%#v", restored, backup)
	}
	refs, err := project.LoadProjectRefsReadOnly(targetRoot)
	if err != nil || len(refs) != 1 || !refs[0].SameProject(ref) {
		t.Fatalf("restored project registry refs=%#v err=%v", refs, err)
	}
	restoredDir, err := store.ProjectDirectory(ref, targetRoot)
	if err != nil {
		t.Fatalf("ProjectDirectory after restore: %v", err)
	}
	for relative, want := range files {
		got, err := os.ReadFile(filepath.Join(restoredDir, filepath.FromSlash(relative)))
		if err != nil || string(got) != want {
			t.Errorf("restored %s = %q err=%v; want %q", relative, got, err, want)
		}
		info, statErr := os.Stat(filepath.Join(restoredDir, filepath.FromSlash(relative)))
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("restored %s mode info=%v err=%v; want 0600", relative, info, statErr)
		}
	}
	if _, err := os.Lstat(filepath.Join(restoredDir, ".maintenance.lock")); !os.IsNotExist(err) {
		t.Errorf("project backup restored a process lock file: err=%v", err)
	}
	opened, err := store.OpenProjectReadOnly(ctx, refs[0], targetRoot)
	if err != nil {
		t.Fatalf("restored capture store is not readable: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close restored capture store: %v", err)
	}
}

func TestRestoreRejectsTamperedMemberWithoutRegisteringProject(t *testing.T) {
	ctx := context.Background()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "tampered"}
	selected, err := store.CreateProject(ctx, ref, sourceRoot)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	sourceDir := selected.Directory
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, sourceDir, "vault/payload.enc", []byte("payload-integrity-sentinel"))
	key, _ := ref.StorageKey()
	archivePath := filepath.Join(t.TempDir(), "original.tar")
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 16}
	if _, err := Backup(ctx, sourceRoot, key, archivePath, limits); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	encoded, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	index := bytes.Index(encoded, []byte("payload-integrity-sentinel"))
	if index < 0 {
		t.Fatal("fixture payload is absent from archive")
	}
	encoded[index] ^= 1
	tamperedPath := filepath.Join(t.TempDir(), "tampered.tar")
	if err := os.WriteFile(tamperedPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := filepath.Join(t.TempDir(), "target")
	if _, err := Restore(ctx, tamperedPath, targetRoot, limits); err == nil {
		t.Fatal("Restore accepted a member whose digest differs from the manifest")
	}
	refs, err := project.LoadProjectRefsReadOnly(targetRoot)
	if err != nil || len(refs) != 0 {
		t.Fatalf("failed restore registered a project: refs=%#v err=%v", refs, err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "projects", key)); !os.IsNotExist(err) {
		t.Fatalf("failed restore left project files behind: %v", err)
	}
}

func TestRestoreRejectsTraversalAndDuplicateArchiveMembers(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "unsafe-archive"}
	key, _ := ref.StorageKey()
	for _, name := range []string{"files/../escape", "files/CON.txt", "files/COM¹.txt", "files/vault/duplicate"} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "unsafe.tar")
			writeUnsafeFixtureArchive(t, archivePath, ref, name)
			targetRoot := filepath.Join(t.TempDir(), "target")
			if _, err := Restore(ctx, archivePath, targetRoot, Limits{MaxBytes: 1 << 20, MaxFiles: 16}); err == nil {
				t.Fatalf("Restore accepted unsafe archive member %q", name)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(targetRoot), "escape")); !os.IsNotExist(err) {
				t.Fatalf("unsafe archive wrote outside the project root: %v", err)
			}
			if _, err := os.Stat(filepath.Join(targetRoot, "projects", key)); !os.IsNotExist(err) {
				t.Fatalf("unsafe restore left a project namespace behind: %v", err)
			}
		})
	}
}

func TestRestoreRejectsAFileUsedAsAnotherMemberParent(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "parent-conflict"}
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	manifest := archiveManifest{Format: archiveFormat, Version: archiveVersion, Project: ref, ProjectKey: key,
		Files: []fileEntry{
			{Path: "vault", Size: 0, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			{Path: "vault/child", Size: 0, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "parent-conflict.tar")
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for _, entry := range []struct {
		name string
		body []byte
	}{{manifestName, manifestBytes}, {"files/vault", nil}, {"files/vault/child", nil}} {
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	targetRoot := filepath.Join(t.TempDir(), "target")
	if _, err := Restore(ctx, archive, targetRoot, Limits{MaxBytes: 1 << 20, MaxFiles: 16}); err == nil {
		t.Fatal("Restore accepted a file path that is also a parent directory")
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "projects")); !os.IsNotExist(err) {
		t.Fatalf("invalid archive created restore data: %v", err)
	}
}

func TestBackupRequiresExplicitPositiveLimitsAndDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "limits"}
	selected, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatal(err)
	}
	directory := selected.Directory
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	key, _ := ref.StorageKey()
	for _, limits := range []Limits{{}, {MaxBytes: -1, MaxFiles: 1}, {MaxBytes: 1, MaxFiles: 0}} {
		path := filepath.Join(t.TempDir(), "unbounded.tar")
		if _, err := Backup(ctx, root, key, path, limits); err == nil {
			t.Errorf("Backup accepted invalid limits %#v", limits)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("failed bounded backup created an archive: %v", err)
		}
	}
	writeFixtureFile(t, directory, "vault/oversize.bin", bytes.Repeat([]byte("x"), 128))
	path := filepath.Join(t.TempDir(), "limited.tar")
	if _, err := Backup(ctx, root, key, path, Limits{MaxBytes: 64, MaxFiles: 16}); err == nil {
		t.Fatal("Backup exceeded the explicit byte limit")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("over-limit backup created an archive: %v", err)
	}

	existingPath := filepath.Join(t.TempDir(), "existing.tar")
	if err := os.WriteFile(existingPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(ctx, root, key, existingPath, Limits{MaxBytes: 1 << 20, MaxFiles: 16}); err == nil {
		t.Fatal("Backup overwrote an existing archive")
	}
	contents, err := os.ReadFile(existingPath)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("existing archive changed: %q err=%v", contents, err)
	}
}

func TestBackupRejectsProjectSymlinksWithoutPublishingArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated permissions on Windows")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "symlink"}
	selected, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatal(err)
	}
	directory, key := selected.Directory, selected.ProjectKey
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "vault", "outside.txt")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "must-not-exist.tar")
	if _, err := Backup(ctx, root, key, archive, Limits{MaxBytes: 1 << 20, MaxFiles: 16}); err == nil {
		t.Fatal("Backup accepted a symlink inside the project directory")
	}
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatalf("failed symlink backup published an archive: %v", err)
	}
}

func TestRestoreRefusesExistingProjectWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "restore-no-overwrite"}
	selected, err := store.CreateProject(ctx, ref, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	key := selected.ProjectKey
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, selected.Directory, "vault/payload.enc", []byte("keep-me"))
	archive := filepath.Join(t.TempDir(), "project.tar")
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 16}
	if _, err := Backup(ctx, sourceRoot, key, archive, limits); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	targetRoot := filepath.Join(t.TempDir(), "target")
	if _, err := Restore(ctx, archive, targetRoot, limits); err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	if _, err := Restore(ctx, archive, targetRoot, limits); err == nil {
		t.Fatal("Restore replaced an already registered project")
	}
	restoredDir, err := store.ProjectDirectory(ref, targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(restoredDir, "vault", "payload.enc"))
	if err != nil || string(content) != "keep-me" {
		t.Fatalf("existing project changed after rejected restore: %q err=%v", content, err)
	}
}

func TestRestoreRejectsSymlinkedProjectsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated permissions on Windows")
	}
	ctx := context.Background()
	sourceRoot := filepath.Join(t.TempDir(), "source")
	ref := project.CaidoProjectRef{InstanceID: "backup-fixture", ProjectID: "restore-symlink"}
	selected, err := store.CreateProject(ctx, ref, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	key := selected.ProjectKey
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "project.tar")
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 16}
	if _, err := Backup(ctx, sourceRoot, key, archive, limits); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	targetRoot := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(targetRoot, "projects")); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, archive, targetRoot, limits); err == nil {
		t.Fatal("Restore followed a symlink at the projects directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink restore wrote into the outside directory: entries=%v err=%v", entries, err)
	}
}

func writeFixtureFile(t *testing.T, root, relative string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", relative, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", relative, err)
	}
}

func writeUnsafeFixtureArchive(t *testing.T, path string, ref project.CaidoProjectRef, member string) {
	t.Helper()
	key, err := ref.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	relative := strings.TrimPrefix(member, "files/")
	manifest := archiveManifest{Format: archiveFormat, Version: archiveVersion, Project: ref, ProjectKey: key,
		Files: []fileEntry{{Path: relative, Size: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
	encodedManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	entries := []struct {
		name string
		body []byte
	}{{name: "manifest.json", body: encodedManifest}, {name: member, body: []byte("x")}}
	if member == "files/vault/duplicate" {
		entries = append(entries, struct {
			name string
			body []byte
		}{name: member, body: []byte("x")})
	}
	for _, entry := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
