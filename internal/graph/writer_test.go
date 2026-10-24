package graph

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestOpenShadowWriterRequiresNewPrivatePathOutsideSelectedProject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "shadow-writer"}
	selected, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()

	shadowDir := filepath.Join(root, "shadow")
	if err := os.Mkdir(shadowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shadowPath := filepath.Join(shadowDir, "behavior.sqlite3")
	writer, err := OpenShadowWriter(ctx, selected, shadowPath)
	if err != nil {
		t.Fatalf("OpenShadowWriter() error = %v", err)
	}
	if writer.graphPath != shadowPath || writer.receiptRoot != selected.BehaviorReceiptPath {
		t.Fatal("shadow writer did not preserve the selected provenance root")
	}
	if _, err := os.Lstat(shadowPath); !os.IsNotExist(err) {
		t.Fatal("opening a shadow writer created or replaced the target graph")
	}

	if _, err := OpenShadowWriter(ctx, selected, selected.BehaviorGraphPath+".shadow"); err == nil {
		t.Fatal("OpenShadowWriter() accepted a path inside the selected project")
	}
	if err := os.WriteFile(filepath.Join(shadowDir, "existing.sqlite3"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	existingPath := filepath.Join(shadowDir, "existing.sqlite3")
	if _, err := OpenShadowWriter(ctx, selected, existingPath); err == nil {
		t.Fatal("OpenShadowWriter() accepted an existing output file")
	}
	contents, err := os.ReadFile(existingPath)
	if err != nil || string(contents) != "keep" {
		t.Fatal("existing output file was changed")
	}

	sharedDir := filepath.Join(root, "shared")
	if err := os.Mkdir(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenShadowWriter(ctx, selected, filepath.Join(sharedDir, "behavior.sqlite3")); err == nil {
		t.Fatal("OpenShadowWriter() accepted a non-private output directory")
	}
}

func TestWriterMatchesProjectStoreChecksStorageDirectory(t *testing.T) {
	ctx := context.Background()
	ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "writer-storage-binding"}
	first, err := store.CreateProject(ctx, ref, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.CreateProject(ctx, ref, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	writer, err := OpenWriter(ctx, first)
	if err != nil {
		t.Fatalf("OpenWriter() error = %v", err)
	}
	if !writer.MatchesProjectStore(first) {
		t.Fatal("writer did not match the store it was opened from")
	}
	if writer.MatchesProjectStore(second) {
		t.Fatal("writer matched a different storage root with the same project key")
	}
}

func TestRecordDifferencePathsReportsSchemaPathsWithoutValues(t *testing.T) {
	left := Record{
		"id":   "observation-id",
		"kind": "observation",
		"data": Record{
			"context": Record{
				"origin":      "https://private.example.invalid",
				"observed_at": "2026-10-22T10:00:00Z",
			},
		},
	}
	right := Record{
		"id":   "observation-id",
		"kind": "observation",
		"data": Record{
			"context": Record{
				"origin":      "https://other.example.invalid",
				"observed_at": "2026-10-22T10:01:00Z",
			},
		},
	}

	got := recordDifferencePaths(left, right, "")
	want := []string{"data.context.observed_at", "data.context.origin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recordDifferencePaths() = %v, want %v", got, want)
	}
	joined := strings.Join(got, ",")
	for _, sensitive := range []string{"private.example", "other.example", "10:00:00", "10:01:00"} {
		if strings.Contains(joined, sensitive) {
			t.Fatalf("field diagnostics exposed a value: %q", joined)
		}
	}
}

func TestRecordDifferencePathsBoundsReportedFields(t *testing.T) {
	left, right := Record{}, Record{}
	for index := 0; index < 24; index++ {
		key := string(rune('a' + index))
		left[key] = "old"
		right[key] = "new"
	}

	got := recordDifferencePaths(left, right, "")
	if len(got) != 16 {
		t.Fatalf("recordDifferencePaths() returned %d paths, want 16", len(got))
	}
}
