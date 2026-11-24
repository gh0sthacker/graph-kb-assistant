package ingest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
)

func TestRunLiveTailEmptyAdvancingPagesComplete(t *testing.T) {
	for _, phase := range []string{"intake", "response_refresh", "both"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			selected, selectedStore, writer := newLiveTailFixture(t)
			// Resume a persisted partial window, including an independent refresh cursor.
			checkpoint, err := writer.BeginCycle(ctx, time.Now().UTC(), 90)
			if err != nil {
				t.Fatal(err)
			}
			cursor := "previous-page"
			written, err := writer.WritePage(ctx, checkpoint, caido.HistoryPage{HasMore: true, NextCursor: &cursor})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selectedStore.Database.DB().ExecContext(ctx,
				"UPDATE live_sync_state SET response_refresh_cursor=? WHERE source_key=?", cursor, writer.sourceKey); err != nil {
				t.Fatal(err)
			}
			next := "empty-next-page"
			empty := caido.RequestMetadataPage{HasMore: true, NextCursor: &next}
			intake, refresh := caido.RequestMetadataPage{}, caido.RequestMetadataPage{}
			if phase != "response_refresh" {
				intake = empty
			}
			if phase != "intake" {
				refresh = empty
			}
			source := &liveTailSourceFixture{project: selected, pages: []caido.RequestMetadataPage{intake}, refreshPages: []caido.RequestMetadataPage{refresh}}
			result, err := RunLiveTail(ctx, source, writer, selected, LiveTailOptions{MaxPages: 10})
			if err != nil || result.State != "complete" || result.CursorPending || result.ResponseRefreshPending || result.LimitReached {
				t.Fatalf("empty page must complete successfully: result=%+v err=%v", result, err)
			}
			if result.RequestsAdded != 0 || source.pageCalls != 1 || source.refreshCalls != 1 {
				t.Fatalf("empty page must stop enumeration: result=%+v source=%+v", result, source)
			}
			var state string
			var savedCursor, refreshCursor, since, until sql.NullString
			var watermark string
			if err := selectedStore.Database.DB().QueryRowContext(ctx,
				"SELECT state,cursor,response_refresh_cursor,window_since,window_until,watermark FROM live_sync_state WHERE source_key=?", writer.sourceKey,
			).Scan(&state, &savedCursor, &refreshCursor, &since, &until, &watermark); err != nil {
				t.Fatal(err)
			}
			if state != "idle" || savedCursor.Valid || refreshCursor.Valid || since.Valid || until.Valid || watermark != written.Checkpoint.WindowUntil {
				t.Fatalf("checkpoint not synchronized: state=%s cursor=%v refresh=%v since=%v until=%v watermark=%s", state, savedCursor, refreshCursor, since, until, watermark)
			}
		})
	}
}

func TestRunLiveTailEmptyInvalidBoundaryStillFails(t *testing.T) {
	selected, _, writer := newLiveTailFixture(t)
	result, err := RunLiveTail(context.Background(), &liveTailSourceFixture{
		project: selected, pages: []caido.RequestMetadataPage{{HasMore: true}},
	}, writer, selected, LiveTailOptions{})
	if err == nil || result.State != "complete" {
		t.Fatalf("malformed empty page must fail without reporting unfinished work: result=%+v err=%v", result, err)
	}
}
