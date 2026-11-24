package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestWatchLeaseContentionRetriesOnlyForLocalTimeout(t *testing.T) {
	active := context.Background()
	if !isWatchLeaseContention(true, active, context.DeadlineExceeded) {
		t.Fatal("watch should retry after its maintenance wait deadline")
	}
	if isWatchLeaseContention(false, active, context.DeadlineExceeded) {
		t.Fatal("one-shot tail should still return a maintenance wait timeout")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if isWatchLeaseContention(true, canceled, context.DeadlineExceeded) {
		t.Fatal("watch should stop when its parent context is canceled")
	}
	if isWatchLeaseContention(true, active, context.Canceled) {
		t.Fatal("watch should not retry a cancellation as lock contention")
	}
}

func TestNeo4jBackfillWaitsForProjectMaintenanceLock(t *testing.T) {
	dataRoot := t.TempDir() + "/private-data"
	ref := project.CaidoProjectRef{InstanceID: "maintenance-test-instance", ProjectID: "maintenance-test-project"}
	projectStore, err := store.CreateProject(t.Context(), ref, dataRoot)
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	projectKey, projectDir := projectStore.ProjectKey, projectStore.Directory
	if err := projectStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lock, err := projectlock.Acquire(t.Context(), projectDir)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- backfillNeo4j([]string{
			"--project-key", projectKey,
			"--data-root", dataRoot,
			"--verify-only",
		})
	}()

	select {
	case err := <-finished:
		_ = lock.Close()
		t.Fatalf("backfill passed the maintenance boundary before lock release: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "BehaviorGraph") {
			t.Fatalf("backfill did not continue to the expected synthetic missing-graph error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backfill remained blocked after maintenance lock release")
	}
}
