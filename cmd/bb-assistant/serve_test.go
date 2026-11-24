package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServeRejectsInvalidOptionsBeforeRuntimeAccess(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"--addr", "0.0.0.0:8765"}, {"--project-key", "bad"}, {"--poll-interval", "0s"}, {"--max-jobs", "0"}, {"--max-attempts", "101"}, {"--lease-duration", "1s"}} {
		root := filepath.Join(t.TempDir(), "must-not-be-created")
		command := append([]string{"serve", "--data-root", root}, args...)
		if err := run(command); err == nil {
			t.Fatalf("serve accepted invalid arguments: %v", args)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("invalid arguments created runtime state: %v", err)
		}
	}
}
