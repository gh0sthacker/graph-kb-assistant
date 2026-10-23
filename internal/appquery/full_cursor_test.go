package appquery

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFullSliceCursorPagesUniqueBranchesAndRetriesIdempotently(t *testing.T) {
	store := newFullCursorStore()
	binding := fullCursorBinding{
		ProjectKey: strings.Repeat("a", 64), SeedHash: strings.Repeat("b", 64),
		Budget: GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	}
	initial := make([]string, 25)
	for i := range initial {
		initial[i] = fmt.Sprintf("assertion-%064x", i+1)
	}
	token, err := store.create(binding, 7, initial, map[string]any{"page": 0})
	if err != nil {
		t.Fatalf("create cursor: %v", err)
	}
	var calls int
	load := func(seeds []string, cumulative map[string]any, page int) (map[string]any, []string, int64, error) {
		calls++
		if len(seeds) == 0 {
			t.Fatal("continuation page has no branch seeds")
		}
		if page != calls || fmt.Sprint(cumulative["page"]) != fmt.Sprint(page-1) {
			t.Fatalf("page=%d cumulative=%#v calls=%d", page, cumulative, calls)
		}
		if calls == 1 {
			return map[string]any{"page": 1}, []string{initial[0], "assertion-" + strings.Repeat("c", 64)}, 7, nil
		}
		want := map[string]struct{}{initial[0]: {}, "assertion-" + strings.Repeat("c", 64): {}}
		for _, id := range initial[20:] {
			want[id] = struct{}{}
		}
		if len(seeds) != len(want) {
			t.Fatalf("second page seeds=%d, want all preserved branches %d: %v", len(seeds), len(want), seeds)
		}
		for _, id := range seeds {
			if _, exists := want[id]; !exists {
				t.Fatalf("second page includes unexpected branch %s: %v", id, seeds)
			}
		}
		return map[string]any{"page": 2}, nil, 7, nil
	}

	first, next, page, err := store.next(token, binding, load, nil)
	if err != nil || page != 1 || next == "" || first["next_cursor"] != next {
		t.Fatalf("first page = %#v, next %q, page %d, err %v", first, next, page, err)
	}
	if calls != 1 {
		t.Fatalf("loader called %d times, want 1", calls)
	}
	// Replaying the consumed token returns the identical page without advancing
	// the session or issuing a second database read.
	replayed, replayCursor, replayPage, err := store.next(token, binding, load, nil)
	if err != nil || replayPage != page || replayCursor != next || fmt.Sprint(replayed["page"]) != fmt.Sprint(first["page"]) || calls != 1 {
		t.Fatalf("idempotent replay = %#v, next %q, page %d, calls %d, err %v", replayed, replayCursor, replayPage, calls, err)
	}

	second, final, page, err := store.next(next, binding, load, nil)
	if err != nil || page != 2 || final != "" || second["continuation_branches_complete"] != true || second["continuation_state"] != "branches_complete" {
		t.Fatalf("second page = %#v, next %q, page %d, err %v", second, final, page, err)
	}
	if calls != 2 {
		t.Fatalf("loader called %d times, want 2", calls)
	}
}

func TestFullSliceCursorBindsProjectSeedsAndBudget(t *testing.T) {
	store := newFullCursorStore()
	binding := fullCursorBinding{
		ProjectKey: strings.Repeat("a", 64), SeedHash: strings.Repeat("b", 64),
		Budget: GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	}
	token, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []fullCursorBinding{
		{ProjectKey: strings.Repeat("d", 64), SeedHash: binding.SeedHash, Budget: binding.Budget},
		{ProjectKey: binding.ProjectKey, SeedHash: strings.Repeat("e", 64), Budget: binding.Budget},
		{ProjectKey: binding.ProjectKey, SeedHash: binding.SeedHash, Budget: GraphBudget{MaxNodes: 80, MaxEdges: 159, MaxDepth: 3}},
	} {
		if _, _, _, err := store.next(token, changed, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
			t.Fatal("mismatched cursor reached the graph loader")
			return nil, nil, 0, nil
		}, nil); !isQueryError(err, ErrorInvalidInput) {
			t.Fatalf("mismatched binding error = %v, want invalid-input", err)
		}
	}
}

func TestFullSliceCursorRejectsMalformedExpiredAndStaleGeneration(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := newFullCursorStore()
	store.now = func() time.Time { return now }
	binding := fullCursorBinding{
		ProjectKey: strings.Repeat("a", 64), SeedHash: strings.Repeat("b", 64),
		Budget: GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	}
	if _, _, _, err := store.next("not-a-token", binding, nil, nil); !isQueryError(err, ErrorInvalidInput) {
		t.Fatalf("malformed cursor error = %v", err)
	}
	token, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		return map[string]any{"page": 1}, nil, 8, nil
	}, nil); !isQueryError(err, ErrorStaleCursor) {
		t.Fatalf("changed generation error = %v, want stale cursor", err)
	}
	store.mu.Lock()
	activeAfterGenerationChange, sessionsAfterGenerationChange := store.active, len(store.sessions)
	store.mu.Unlock()
	if activeAfterGenerationChange != 0 || sessionsAfterGenerationChange != 0 {
		t.Fatalf("generation change retained cursor capacity: active=%d sessions=%d", activeAfterGenerationChange, sessionsAfterGenerationChange)
	}
	token, err = store.create(binding, 7, []string{"assertion-" + strings.Repeat("d", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(fullCursorTTL + time.Second)
	if _, _, _, err := store.next(token, binding, nil, nil); !isQueryError(err, ErrorStaleCursor) {
		t.Fatalf("expired cursor error = %v, want stale cursor", err)
	}
}

func TestFullSliceCursorExpiresWhileLoaderIsRunning(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := newFullCursorStore()
	store.now = func() time.Time { return now }
	binding := testFullCursorBinding()
	token, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	_, next, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		now = now.Add(fullCursorTTL + time.Second)
		return map[string]any{"page": 1}, nil, 7, nil
	}, nil)
	if !isQueryError(err, ErrorStaleCursor) || next != "" {
		t.Fatalf("expired in-flight cursor next=%q err=%v, want stale cursor without token", next, err)
	}
	store.mu.Lock()
	active, sessions := store.active, len(store.sessions)
	store.mu.Unlock()
	if active != 0 || sessions != 0 {
		t.Fatalf("expired in-flight cursor retained capacity: active=%d sessions=%d", active, sessions)
	}
}

func TestFullSliceCursorDoesNotCommitAfterStoreClear(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	token, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	_, next, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		store.clear()
		return map[string]any{"page": 1}, nil, 7, nil
	}, nil)
	if !isQueryError(err, ErrorStaleCursor) || next != "" {
		t.Fatalf("cleared in-flight cursor next=%q err=%v, want stale cursor without token", next, err)
	}
	store.mu.Lock()
	active, sessions := store.active, len(store.sessions)
	store.mu.Unlock()
	if active != 0 || sessions != 0 {
		t.Fatalf("cleared in-flight cursor was reinserted: active=%d sessions=%d", active, sessions)
	}
}

func TestFullSliceCursorRemovesSessionOnTypedStaleLoadError(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	token, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		return nil, nil, 0, queryError(ErrorStaleCursor, "generation changed", nil)
	}, nil); !isQueryError(err, ErrorStaleCursor) {
		t.Fatalf("stale loader error = %v", err)
	}
	store.mu.Lock()
	active, sessions := store.active, len(store.sessions)
	store.mu.Unlock()
	if active != 0 || sessions != 0 {
		t.Fatalf("stale loader retained cursor capacity: active=%d sessions=%d", active, sessions)
	}
}

func TestFullSliceCursorMarksOverflowedBranchQueuePartial(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	initial := make([]string, maxFullCursorPendingBranches)
	newBranches := make([]string, maxFullCursorPendingBranches)
	for i := range initial {
		initial[i] = fmt.Sprintf("assertion-%064x", i+1)
		newBranches[i] = fmt.Sprintf("assertion-%064x", i+10_000)
	}
	token, err := store.create(binding, 7, initial, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	page, next, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		return map[string]any{"page": 1}, newBranches, 7, nil
	}, nil)
	if err != nil || next == "" || page["continuation_state"] != "available_with_truncated_branch_queue" ||
		page["continuation_branch_queue_truncated"] != true || fmt.Sprint(page["continuation_dropped_branch_count"]) != fmt.Sprint(maxFullCursorPendingBranches-20) {
		t.Fatalf("overflowed branch queue = %#v next=%q err=%v", page, next, err)
	}
}

func TestFullSliceCursorKeepsPageRetryableAfterReadFailure(t *testing.T) {
	store := newFullCursorStore()
	binding := fullCursorBinding{
		ProjectKey: strings.Repeat("a", 64), SeedHash: strings.Repeat("b", 64),
		Budget: GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	}
	token, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("database unavailable")
	if _, _, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		return nil, nil, 0, readErr
	}, nil); !errors.Is(err, readErr) {
		t.Fatalf("failed read error = %v, want wrapped database error", err)
	}
	if _, _, page, err := store.next(token, binding, func(seeds []string, _ map[string]any, _ int) (map[string]any, []string, int64, error) {
		if len(seeds) != 1 || seeds[0] != "assertion-"+strings.Repeat("c", 64) {
			t.Fatalf("retry seeds = %v", seeds)
		}
		return map[string]any{"page": 1}, nil, 7, nil
	}, nil); err != nil || page != 1 {
		t.Fatalf("retry page=%d err=%v", page, err)
	}
}

func TestFullCursorDoesNotAdvanceWhenFinalResponseExceedsWireLimit(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	branch := "assertion-" + strings.Repeat("c", 64)
	token, err := store.create(binding, 7, []string{branch}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	load := func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		calls++
		return map[string]any{"page": 1}, nil, 7, nil
	}
	tooLarge := func(map[string]any) (map[string]any, error) {
		return nil, queryError(ErrorOutputTooLarge, "oversized page", nil)
	}
	if _, _, _, err := store.next(token, binding, load, tooLarge); !isQueryError(err, ErrorOutputTooLarge) {
		t.Fatalf("oversized response error = %v", err)
	}
	page, next, number, err := store.next(token, binding, load, func(value map[string]any) (map[string]any, error) {
		value["prepared"] = true
		return value, nil
	})
	if err != nil || number != 1 || next != "" || page["prepared"] != true || calls != 2 {
		t.Fatalf("retry after oversized response = %#v next=%q page=%d calls=%d err=%v", page, next, number, calls, err)
	}
}

func TestFullCursorReplaysEveryAcceptedWirePage(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	branch := "assertion-" + strings.Repeat("c", 64)
	token, err := store.create(binding, 7, []string{branch}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", (1<<20)+10)
	var calls int
	load := func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		calls++
		return map[string]any{"page": 1, "payload": large}, nil, 7, nil
	}
	finalize := func(value map[string]any) (map[string]any, error) {
		value["prepared"] = true
		return value, nil
	}
	first, _, _, err := store.next(token, binding, load, finalize)
	if err != nil {
		t.Fatal(err)
	}
	replay, _, _, err := store.next(token, binding, load, finalize)
	if err != nil || replay["payload"] != first["payload"] || replay["prepared"] != true || calls != 1 {
		t.Fatalf("large response replay = calls:%d err:%v", calls, err)
	}
}

func TestFullCursorPreservesTruncatedBranchCatalogAndReclaimsCompletedCapacity(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	for i := 0; i < maxFullCursorSessions; i++ {
		branch := fmt.Sprintf("assertion-%064x", i+1)
		token, err := store.create(binding, 7, []string{branch}, map[string]any{"page": 0})
		if err != nil {
			t.Fatalf("create cursor %d: %v", i, err)
		}
		page, _, _, err := store.next(token, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
			return map[string]any{"page": 1}, nil, 7, nil
		}, nil)
		if err != nil || page["continuation_branches_complete"] != true {
			t.Fatalf("complete cursor %d = %#v, err %v", i, page, err)
		}
	}
	last, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("f", 64)}, map[string]any{"page": 0})
	if err != nil || last == "" {
		t.Fatalf("new cursor after completed sessions = %q, err %v", last, err)
	}

	truncatedToken, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("e", 64)}, map[string]any{"missing_branches_truncated": true})
	if err != nil {
		t.Fatal(err)
	}
	page, _, _, err := store.next(truncatedToken, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		return map[string]any{"missing_branches_truncated": true}, nil, 7, nil
	}, nil)
	if err != nil || page["continuation_state"] != "branch_list_truncated" || page["continuation_branches_truncated"] != true || page["continuation_branches_complete"] == true {
		t.Fatalf("truncated branch catalog state = %#v, err %v", page, err)
	}
}

func TestFullCursorCapacityStillBoundsUnfinishedSessions(t *testing.T) {
	store := newFullCursorStore()
	binding := testFullCursorBinding()
	for i := 0; i < maxFullCursorSessions; i++ {
		branch := fmt.Sprintf("assertion-%064x", i+1)
		if _, err := store.create(binding, 7, []string{branch}, map[string]any{"page": 0}); err != nil {
			t.Fatalf("create active cursor %d: %v", i, err)
		}
	}
	if _, err := store.create(binding, 7, []string{"assertion-" + strings.Repeat("f", 64)}, map[string]any{"page": 0}); err == nil {
		t.Fatal("active cursor capacity did not reject an additional session")
	}
}

func testFullCursorBinding() fullCursorBinding {
	return fullCursorBinding{
		ProjectKey: strings.Repeat("a", 64), SeedHash: strings.Repeat("b", 64),
		Budget: GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	}
}

func isQueryError(err error, code ErrorCode) bool {
	var queryErr *QueryError
	return errors.As(err, &queryErr) && queryErr.Code == code
}
