package appquery

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPersistentFullCursorSurvivesStoreRestartAndReplaysAcceptedPage(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "query-state")
	binding := testFullCursorBinding()
	branch := "assertion-" + strings.Repeat("c", 64)
	continuationBranch := "assertion-" + strings.Repeat("d", 64)

	firstStore, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatalf("open persistent cursor store: %v", err)
	}
	firstToken, err := firstStore.create(binding, 7, []string{branch}, map[string]any{"page": 0})
	if err != nil {
		_ = firstStore.close()
		t.Fatalf("create persistent cursor: %v", err)
	}
	firstPage, nextToken, pageNo, err := firstStore.next(firstToken, binding, func(_ []string, _ map[string]any, page int) (map[string]any, []string, int64, error) {
		return map[string]any{"page": page}, []string{continuationBranch}, 7, nil
	}, nil)
	if err != nil || pageNo != 1 || nextToken == "" {
		_ = firstStore.close()
		t.Fatalf("first page = %#v, next %q, page %d, err %v", firstPage, nextToken, pageNo, err)
	}
	if err := firstStore.close(); err != nil {
		t.Fatalf("close first cursor store: %v", err)
	}

	restartedStore, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatalf("reopen persistent cursor store: %v", err)
	}
	defer restartedStore.close()
	replayed, replayNext, replayPage, err := restartedStore.next(firstToken, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		t.Fatal("replaying an accepted cursor must not read the graph again")
		return nil, nil, 0, nil
	}, nil)
	if err != nil || replayNext != nextToken || replayPage != pageNo || fmt.Sprint(replayed) != fmt.Sprint(firstPage) {
		t.Fatalf("replayed page = %#v, next %q, page %d, err %v; want %#v", replayed, replayNext, replayPage, err, firstPage)
	}

	secondPage, finalToken, secondPageNo, err := restartedStore.next(nextToken, binding, func(_ []string, _ map[string]any, page int) (map[string]any, []string, int64, error) {
		return map[string]any{"page": page}, nil, 7, nil
	}, nil)
	if err != nil || secondPageNo != 2 || finalToken != "" || secondPage["continuation_branches_complete"] != true {
		t.Fatalf("continued page = %#v, next %q, page %d, err %v", secondPage, finalToken, secondPageNo, err)
	}
	if err := restartedStore.close(); err != nil {
		t.Fatalf("close restarted cursor store: %v", err)
	}

	thirdStore, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatalf("open cursor store for final replay: %v", err)
	}
	defer thirdStore.close()
	finalReplay, _, replayPage, err := thirdStore.next(nextToken, binding, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		t.Fatal("replaying the final accepted cursor must not read the graph again")
		return nil, nil, 0, nil
	}, nil)
	if err != nil || replayPage != 2 || fmt.Sprint(finalReplay) != fmt.Sprint(secondPage) {
		t.Fatalf("final replay = %#v, page %d, err %v; want %#v", finalReplay, replayPage, err, secondPage)
	}
}

func TestPersistentFullCursorPrunesExpiredStateWhenOpeningStore(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "query-state")
	store, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatalf("open cursor store: %v", err)
	}
	store.now = func() time.Time { return time.Now().Add(-2 * fullCursorTTL) }
	if _, err := store.create(testFullCursorBinding(), 7, []string{"assertion-" + strings.Repeat("c", 64)}, map[string]any{"private_path": "/tmp/expired-evidence.json"}); err != nil {
		_ = store.close()
		t.Fatalf("create expired cursor state: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("close cursor store: %v", err)
	}

	reopened, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatalf("reopen cursor store: %v", err)
	}
	defer reopened.close()
	var sessions, tokens, secureDelete int
	if err := reopened.db.QueryRow("SELECT count(*) FROM full_cursor_sessions").Scan(&sessions); err != nil {
		t.Fatalf("count persisted sessions: %v", err)
	}
	if err := reopened.db.QueryRow("SELECT count(*) FROM full_cursor_tokens").Scan(&tokens); err != nil {
		t.Fatalf("count persisted tokens: %v", err)
	}
	if err := reopened.db.QueryRow("PRAGMA secure_delete").Scan(&secureDelete); err != nil {
		t.Fatalf("read secure_delete setting: %v", err)
	}
	if sessions != 0 || tokens != 0 || secureDelete != 1 {
		t.Fatalf("expired cursor cleanup = sessions:%d tokens:%d secure_delete:%d; want 0, 0, 1", sessions, tokens, secureDelete)
	}
}

func TestPersistentFullCursorConcurrentAdvanceReturnsOneAcceptedPage(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "query-state")
	binding := testFullCursorBinding()
	branch := "assertion-" + strings.Repeat("c", 64)
	storeA, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.close()
	storeB, err := openPersistentFullCursorStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.close()
	token, err := storeA.create(binding, 7, []string{branch}, map[string]any{"page": 0})
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	type result struct {
		payload map[string]any
		token   string
		page    int
		err     error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(2)
	load := func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
		ready <- struct{}{}
		<-release
		return map[string]any{"accepted": true}, nil, 7, nil
	}
	for _, cursorStore := range []*fullCursorStore{storeA, storeB} {
		go func(cursorStore *fullCursorStore) {
			defer start.Done()
			payload, next, page, err := cursorStore.next(token, binding, load, nil)
			results <- result{payload: payload, token: next, page: page, err: err}
		}(cursorStore)
	}
	<-ready
	<-ready
	close(release)
	start.Wait()
	first, second := <-results, <-results
	for i, got := range []result{first, second} {
		if got.err != nil || got.page != 1 || got.token != "" || got.payload["accepted"] != true {
			t.Fatalf("concurrent result %d = %#v, want the same accepted page; err=%v", i, got.payload, got.err)
		}
	}
	if fmt.Sprint(first.payload) != fmt.Sprint(second.payload) {
		t.Fatalf("concurrent callers received different page payloads: %#v and %#v", first.payload, second.payload)
	}
}

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
		{ProjectKey: binding.ProjectKey, SeedHash: binding.SeedHash, ContextHash: strings.Repeat("f", 64), Budget: binding.Budget},
	} {
		if _, _, _, err := store.next(token, changed, func([]string, map[string]any, int) (map[string]any, []string, int64, error) {
			t.Fatal("mismatched cursor reached the graph loader")
			return nil, nil, 0, nil
		}, nil); !isQueryError(err, ErrorInvalidInput) {
			t.Fatalf("mismatched binding error = %v, want invalid-input", err)
		}
	}
}

func TestFullSliceCursorAcceptsNativeFrontierEntitySeeds(t *testing.T) {
	store := newFullCursorStore()
	binding := fullCursorBinding{
		ProjectKey: strings.Repeat("a", 64), SeedHash: strings.Repeat("b", 64),
		Budget: GraphBudget{MaxNodes: 80, MaxEdges: 160, MaxDepth: 3},
	}
	endpoint := "endpoint-" + strings.Repeat("f", 64)
	token, err := store.create(binding, 7, []string{endpoint}, map[string]any{"page": 0})
	if err != nil {
		t.Fatalf("create native frontier continuation: %v", err)
	}
	page, next, pageNo, err := store.next(token, binding, func(seeds []string, _ map[string]any, page int) (map[string]any, []string, int64, error) {
		if len(seeds) != 1 || seeds[0] != endpoint {
			t.Fatalf("native frontier seeds = %v, want [%s]", seeds, endpoint)
		}
		return map[string]any{"page": page}, nil, 7, nil
	}, nil)
	if err != nil || next != "" || pageNo != 1 || page["continuation_branches_complete"] != true {
		t.Fatalf("native frontier continuation = %#v, next %q, page %d, err %v", page, next, pageNo, err)
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
