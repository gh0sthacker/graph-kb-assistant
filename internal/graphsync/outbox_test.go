package graphsync

import (
	"context"
	"errors"
	"testing"
)

func TestDrainUntilBundleAppliedDoesNotTreatUnrelatedBacklogAsFailure(t *testing.T) {
	ctx := context.Background()
	drainedPages := 0
	result, err := drainUntilBundleApplied(ctx, "project", "bundle-target",
		func(context.Context, string) (bool, error) { return drainedPages == 3, nil },
		func(context.Context) (Result, error) {
			drainedPages++
			return Result{State: "partial", Applied: 100, Pending: 300 - drainedPages*100, Backend: "neo4j"}, nil
		},
	)
	if err != nil || result.State != "ready" || result.Applied != 300 || drainedPages != 3 {
		t.Fatalf("drain through unrelated backlog: result=%+v pages=%d err=%v", result, drainedPages, err)
	}
}

func TestDrainUntilBundleAppliedStopsWhenNoProgressIsPossible(t *testing.T) {
	ctx := context.Background()
	_, err := drainUntilBundleApplied(ctx, "project", "bundle-target",
		func(context.Context, string) (bool, error) { return false, nil },
		func(context.Context) (Result, error) { return Result{State: "partial", Backend: "neo4j"}, nil },
	)
	if err == nil {
		t.Fatal("targeted drain accepted a pending target without making progress")
	}
}

func TestDrainUntilBundleAppliedAcceptsTargetBeforeLaterPageError(t *testing.T) {
	ctx := context.Background()
	targetApplied := false
	drainCalls := 0
	result, err := drainUntilBundleApplied(ctx, "project", "bundle-target",
		func(context.Context, string) (bool, error) { return targetApplied, nil },
		func(context.Context) (Result, error) {
			drainCalls++
			targetApplied = true
			return Result{State: "partial", Applied: 1, Backend: "neo4j"}, errors.New("synthetic later outbox item failed")
		},
	)
	if err != nil || result.State != "ready" || drainCalls != 1 || result.Applied != 1 {
		t.Fatalf("already-applied target was not acknowledged: result=%+v err=%v", result, err)
	}
}
