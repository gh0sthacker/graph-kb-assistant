package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/coverage"
)

func TestCoverageCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BB_ASSISTANT_DATA_HOME", root)
	store, err := coverage.Open(filepath.Join(root, coverage.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	m := &coverage.Matrix{Run: "demo"}
	m.SetBudget("xss", 1)
	m.Record("xss", coverage.Confirmed, "req-1")
	m.SetBudget("sqli", 1)
	m.Record("sqli", coverage.RuledOut, "req-2")
	m.SetBudget("idor", 2)
	if err := store.Save(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"coverage", "--run", "demo"}
		if asJSON {
			args = append(args, "--json")
		}
		output, err := captureCoverageCLI(t, args)
		if err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var decoded map[string]any
			if err := json.Unmarshal(output, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["run"] != "demo" || decoded["can_finish"] != false || len(decoded["cells"].([]any)) != 3 {
				t.Fatalf("JSON result = %s", output)
			}
		} else {
			for _, want := range []string{"demo", "xss", "confirmed", "sqli", "ruled_out", "idor", "untested", "can_finish: false", "do not claim"} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("text output missing %q: %s", want, output)
				}
			}
		}
	}
	if err := run([]string{"coverage", "--run", "missing", "--data-root", root, "--json"}); err == nil {
		t.Fatal("missing run succeeded")
	}
	for _, args := range [][]string{{"coverage"}, {"coverage", "--run", ""}, {"coverage", "--run", "demo", "extra"}, {"coverage", "--unknown"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}

func TestCoverageCLIMissingDatabaseDoesNotCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-root")
	if err := run([]string{"coverage", "--run", "demo", "--data-root", root}); err == nil {
		t.Fatal("missing coverage succeeded")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read command created root: %v", err)
	}
}

func captureCoverageCLI(t *testing.T, args []string) ([]byte, error) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "coverage-output-*")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = original }()
	runErr := run(args)
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	return data, runErr
}
