package ingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
)

func TestHistoricalReplayPreservesAcceptedProjectionBeforeURLReferenceEnrichment(t *testing.T) {
	for _, state := range []string{"pending", "applied"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			selected, projectStore, _ := newLiveTailFixture(t)
			writer, err := OpenPageWriter(ctx, projectStore)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			exchange := projectionWorkerExchange(selected, "historical-old-projection", "HTTP/1.1 200 OK\r\n\r\n")
			if _, err := writePendingHistoryRevision(ctx, writer, exchange); err != nil {
				t.Fatal(err)
			}
			var revisionID string
			if err := projectStore.Database.DB().QueryRowContext(ctx,
				"SELECT current_revision_id FROM captures WHERE request_id=?", exchange.Ref.RequestID,
			).Scan(&revisionID); err != nil {
				t.Fatal(err)
			}
			current, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, revisionID)
			if err != nil {
				t.Fatal(err)
			}
			legacyBundle := current.PayloadBundle
			entities := []behaviorgraph.Record{}
			removed := 0
			for _, entity := range projectionRecordsMust(t, legacyBundle["entities"]) {
				if entity["kind"] == "url_reference" {
					removed++
					continue
				}
				entities = append(entities, entity)
			}
			assertions := []behaviorgraph.Record{}
			for _, assertion := range projectionRecordsMust(t, legacyBundle["assertions"]) {
				if assertion["predicate"] == "uses_request_url" || assertion["predicate"] == "referrer_url_matches" || assertion["predicate"] == "redirect_url_matches" {
					continue
				}
				assertions = append(assertions, assertion)
			}
			if removed == 0 {
				t.Fatal("fixture has no additive URL projection to remove")
			}
			legacyBundle["entities"], legacyBundle["assertions"] = entities, assertions
			legacyPayload, err := json.Marshal(legacyBundle)
			if err != nil {
				t.Fatal(err)
			}
			// Construct a disposable ledger as it existed before URL enrichment.
			// Production code must retain this accepted immutable payload.
			tx, err := projectStore.Database.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "DELETE FROM projection_jobs WHERE revision_id=?", revisionID); err != nil {
				t.Fatal(err)
			}
			legacy, inserted, err := behaviorgraph.EnqueueProjectionJob(ctx, tx, projectStore.ProjectKey,
				revisionID, current.ObservationID, legacyPayload, time.Now().UTC())
			if err != nil || !inserted {
				t.Fatalf("construct previously accepted job: inserted=%t err=%v", inserted, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if state == "applied" {
				maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
				if err != nil {
					t.Fatal(err)
				}
				result, projectErr := runProjectionWorkerOnce(ctx, projectStore, maintenance,
					ProjectionWorkerOptions{}, materializeLocalProjection, time.Now().UTC().Add(time.Minute))
				_ = maintenance.Close()
				if projectErr != nil || result.State != "applied" {
					t.Fatalf("apply legacy local graph: result=%+v err=%v", result, projectErr)
				}
			}
			replayed, err := writePendingHistoryRevision(ctx, writer, exchange)
			if err != nil || replayed.Replayed != 1 || replayed.ProjectionState != state {
				t.Fatalf("replay old accepted projection: result=%+v err=%v; want retained %s state", replayed, err, state)
			}
			after, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, revisionID)
			if err != nil || after.PayloadJSON != legacy.PayloadJSON || after.PayloadSHA256 != legacy.PayloadSHA256 || after.State != state {
				t.Fatalf("replay replaced the accepted projection or its delivery state: err=%v", err)
			}
			var count int
			if err := projectStore.Database.DB().QueryRowContext(ctx,
				"SELECT count(*) FROM capture_revisions WHERE observation_id=?", current.ObservationID,
			).Scan(&count); err != nil || count != 1 {
				t.Fatalf("replay created %d capture revisions (err=%v); want one", count, err)
			}
		})
	}
}
