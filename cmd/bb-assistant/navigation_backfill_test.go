package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestNavigationBackfillCLIValidatesArgumentsBeforeRuntimeAccess(t *testing.T) {
	key := strings.Repeat("a", 64)
	for _, command := range []string{"graph-navigation-backfill-preview", "graph-navigation-backfill-apply"} {
		t.Run(command, func(t *testing.T) {
			for _, test := range []struct {
				name string
				args []string
			}{
				{name: "missing project"},
				{name: "invalid project", args: []string{"--project-key", "invalid"}},
				{name: "multiple projects", args: []string{"--project-key", key, "--project-key", key}},
				{name: "positional argument", args: []string{"--project-key", key, "unexpected"}},
				{name: "zero captures", args: []string{"--project-key", key, "--max-captures", "0"}},
				{name: "negative captures", args: []string{"--project-key", key, "--max-captures", "-1"}},
				{name: "capture cap", args: []string{"--project-key", key, "--max-captures", "20001"}},
				{name: "zero bytes", args: []string{"--project-key", key, "--max-bytes", "0"}},
				{name: "byte minimum", args: []string{"--project-key", key, "--max-bytes", "1048575"}},
				{name: "byte cap", args: []string{"--project-key", key, "--max-bytes", "1073741825"}},
				{name: "request-only budget unsupported", args: []string{"--project-key", key, "--max-request-bytes", "1048576"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					root := filepath.Join(t.TempDir(), "runtime-must-not-exist")
					args := []string{command, "--data-root", root}
					if strings.HasSuffix(command, "-apply") {
						args = append(args, "--candidate-sha256", key)
					}
					args = append(args, test.args...)
					err := run(args)
					if err == nil || !(strings.Contains(err.Error(), "usage:") || strings.Contains(err.Error(), "limits are invalid") || strings.Contains(err.Error(), "flag provided but not defined")) {
						t.Fatalf("invalid arguments reached runtime access: %v", err)
					}
					if _, err := os.Stat(root); !os.IsNotExist(err) {
						t.Fatalf("argument validation created the runtime root: %v", err)
					}
				})
			}
		})
	}
}

func TestNavigationBackfillApplyCLIRequiresExactSHA256Syntax(t *testing.T) {
	key := strings.Repeat("a", 64)
	for _, digest := range []string{"", "a", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("A", 64), strings.Repeat(" ", 64)} {
		root := filepath.Join(t.TempDir(), "runtime-must-not-exist")
		args := []string{"graph-navigation-backfill-apply", "--project-key", key, "--data-root", root}
		if digest != "" {
			args = append(args, "--candidate-sha256", digest)
		}
		if err := run(args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("digest %q did not fail before runtime access: %v", digest, err)
		}
	}
}

func TestNavigationBackfillCommandsAreListedInUsage(t *testing.T) {
	err := run(nil)
	for _, command := range []string{"graph-navigation-backfill-preview", "graph-navigation-backfill-apply"} {
		if err == nil || !strings.Contains(err.Error(), command) {
			t.Fatalf("usage omitted %s: %v", command, err)
		}
	}
}

func TestNavigationBackfillCLIWaitsForProjectMaintenanceLock(t *testing.T) {
	for _, command := range []string{"graph-navigation-backfill-preview", "graph-navigation-backfill-apply"} {
		t.Run(command, func(t *testing.T) {
			root, key, directory := navigationBackfillCLIProject(t, false)
			maintenance, err := projectlock.Acquire(t.Context(), directory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = maintenance.Close() })
			args := []string{command, "--project-key", key, "--data-root", root}
			if strings.HasSuffix(command, "-apply") {
				args = append(args, "--candidate-sha256", strings.Repeat("0", 64))
			}
			finished := make(chan error, 1)
			output, commandErr := captureNavigationBackfillOutput(t, func() error {
				go func() { finished <- run(args) }()
				select {
				case err := <-finished:
					_ = maintenance.Close()
					t.Fatalf("command passed maintenance boundary before release: %v", err)
				case <-time.After(150 * time.Millisecond):
				}
				if err := maintenance.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-finished:
					return err
				case <-time.After(5 * time.Second):
					t.Fatal("command remained blocked after maintenance lock release")
					return nil
				}
			})
			if commandErr == nil || !strings.Contains(commandErr.Error(), "graph") {
				t.Fatalf("command did not reach synthetic missing-graph boundary: %v; output=%s", commandErr, output)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			lease, err := projectlock.Acquire(ctx, directory)
			if err != nil {
				t.Fatalf("command retained its maintenance lease after returning: %v", err)
			}
			_ = lease.Close()
		})
	}
}

func TestNavigationBackfillCLIPreviewAndExactDigestApply(t *testing.T) {
	root, key, directory := navigationBackfillCLIProject(t, true)
	graphPath := filepath.Join(directory, "behavior.sqlite3")
	before, err := os.ReadFile(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	previewOutput, err := captureNavigationBackfillOutput(t, func() error {
		return run([]string{"graph-navigation-backfill-preview", "--project-key", key, "--data-root", root,
			"--max-captures", "20000", "--max-bytes", "1073741824"})
	})
	if err != nil {
		t.Fatalf("preview: %v; output=%s", err, previewOutput)
	}
	var preview ingest.NavigationBackfillPreview
	if err := json.Unmarshal(previewOutput, &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.ProjectKey != key || preview.State != "ready" || preview.WriteMode != "preview_only" || !validProjectKey(preview.CandidateSHA256) {
		t.Fatalf("preview did not return a ready scoped digest: %s", previewOutput)
	}
	afterPreview, err := os.ReadFile(graphPath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(afterPreview) {
		t.Fatalf("preview modified graph storage: %v", err)
	}

	rejectedOutput, err := captureNavigationBackfillOutput(t, func() error {
		return run([]string{"graph-navigation-backfill-apply", "--project-key", key, "--data-root", root,
			"--candidate-sha256", strings.Repeat("0", 64)})
	})
	if err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("apply accepted a different digest: %v; output=%s", err, rejectedOutput)
	}
	afterRejected, err := os.ReadFile(graphPath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(afterRejected) {
		t.Fatalf("rejected apply modified graph storage: %v", err)
	}
	applyOutput, err := captureNavigationBackfillOutput(t, func() error {
		return run([]string{"graph-navigation-backfill-apply", "--project-key", key, "--data-root", root,
			"--candidate-sha256", preview.CandidateSHA256, "--max-captures", "1", "--max-bytes", "1048576"})
	})
	if err != nil {
		t.Fatalf("apply exact preview: %v; output=%s", err, applyOutput)
	}
	var applied map[string]any
	if err := json.Unmarshal(applyOutput, &applied); err != nil {
		t.Fatal(err)
	}
	if applied["project_key"] != key || applied["candidate_sha256"] != preview.CandidateSHA256 || applied["neo4j_drain"] != "not_attempted" {
		t.Fatalf("apply omitted scoped safe result fields: %s", applyOutput)
	}
	for _, output := range [][]byte{previewOutput, rejectedOutput, applyOutput} {
		for _, forbidden := range []string{"\"bundles\"", "raw_request", "raw_response", "authorization", "cookie"} {
			if strings.Contains(strings.ToLower(string(output)), forbidden) {
				t.Fatalf("CLI output exposed forbidden data %q: %s", forbidden, output)
			}
		}
	}
}

func TestNavigationBackfillPreviewJSONOmitsBundles(t *testing.T) {
	encoded, err := json.Marshal(ingest.NavigationBackfillPreview{
		ProjectKey: strings.Repeat("a", 64), State: "ready", WriteMode: "preview_only",
		Bundles: []behaviorgraph.Record{{"raw_request": "synthetic-private-value"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private-value") || strings.Contains(string(encoded), "raw_request") || strings.Contains(string(encoded), "\"bundles\"") {
		t.Fatalf("preview serialization disclosed an in-memory bundle: %s", encoded)
	}
}

func navigationBackfillCLIProject(t *testing.T, createGraph bool) (root, key, directory string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "private-data")
	ref := project.CaidoProjectRef{InstanceID: "navigation-cli-instance", ProjectID: "synthetic-navigation-project"}
	selected, err := store.CreateProject(t.Context(), ref, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = selected.Close() })
	key, directory = selected.ProjectKey, selected.Directory
	if createGraph {
		maintenance, err := projectlock.Acquire(t.Context(), directory)
		if err != nil {
			t.Fatal(err)
		}
		defer maintenance.Close()
		writer, err := behaviorgraph.OpenWriter(t.Context(), selected)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ImportBundle(t.Context(), behaviorgraph.Record{
			"schema_version": 1, "program": key, "sources": []behaviorgraph.Record{},
			"entities": []behaviorgraph.Record{}, "assertions": []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	return root, key, directory
}

func captureNavigationBackfillOutput(t *testing.T, invoke func() error) ([]byte, error) {
	t.Helper()
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()
	original := os.Stdout
	os.Stdout = writeEnd
	defer func() { os.Stdout = original }()
	invokeErr := invoke()
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	return output, invokeErr
}
