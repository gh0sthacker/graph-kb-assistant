package mcpserver

import (
	"context"
	"strings"
	"testing"

	"bb-graph-assistant/internal/graphsync"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
)

func TestOperationBackfillRequiresRegisteredProjectBeforeOpeningStorage(t *testing.T) {
	tools := &Tools{projectsByKey: map[string]project.CaidoProjectRef{}}
	key := strings.Repeat("a", 64)
	if _, err := tools.PreviewOperationBackfill(context.Background(), key, ingest.OperationBackfillOptions{}); err == nil || err.Error() != "selected project is not registered" {
		t.Fatalf("unregistered project preview error = %v", err)
	}
	if _, err := tools.ApplyOperationBackfill(context.Background(), key, strings.Repeat("b", 64), ingest.OperationBackfillOptions{}); err == nil || err.Error() != "selected project is not registered" {
		t.Fatalf("unregistered project apply error = %v", err)
	}
}

func TestOperationBackfillSyncsOnlyItsOrderedBundleTargetsAndStopsOnPending(t *testing.T) {
	ids := []string{"bundle-first", "bundle-second", "bundle-third"}
	var synced []string
	state := operationBackfillBundleSyncStatus(ids, func(id string) (graphsync.Result, error) {
		synced = append(synced, id)
		if id == "bundle-second" {
			return graphsync.Result{State: "partial"}, nil
		}
		return graphsync.Result{State: "ready"}, nil
	})
	if state != "pending" || len(synced) != 2 || synced[0] != ids[0] || synced[1] != ids[1] {
		t.Fatalf("sync state=%q targets=%v", state, synced)
	}
}

func TestOperationBackfillSyncReportsDisabledAndNoOpStates(t *testing.T) {
	disabled := operationBackfillBundleSyncStatus([]string{"bundle-one"}, func(string) (graphsync.Result, error) {
		return graphsync.Result{State: "disabled"}, nil
	})
	if disabled != "disabled" {
		t.Fatalf("disabled sync state = %q", disabled)
	}
	if noOp := operationBackfillBundleSyncStatus(nil, func(string) (graphsync.Result, error) {
		t.Fatal("no-op sync invoked callback")
		return graphsync.Result{}, nil
	}); noOp != "not_applicable" {
		t.Fatalf("empty sync state = %q", noOp)
	}
}
