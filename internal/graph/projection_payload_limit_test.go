package graph

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectionJobPayloadLimitMatchesGraphWriterAndRemainsBounded(t *testing.T) {
	if maxProjectionJobPayloadBytes != MaxBundleJSONBytes {
		t.Fatalf("job payload limit = %d, graph bundle limit = %d", maxProjectionJobPayloadBytes, MaxBundleJSONBytes)
	}
	ctx := context.Background()
	db := openProjectionJobDB(t, filepath.Join(t.TempDir(), "graph.sqlite3"))
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	revisionID := strings.Repeat("a", 64)
	now := time.Now().UTC()
	registerProjectionRevision(t, ctx, tx, revisionID, "observation-limit", now)
	payload := minimalProjectionPayload(projectionTestProjectKey, "observation-limit")
	// Valid JSON with whitespace must still be rejected at the input boundary;
	// accepting a larger graph bundle does not mean accepting unbounded jobs.
	payload = append(payload, bytes.Repeat([]byte(" "), MaxBundleJSONBytes+1-len(payload))...)
	if _, _, err := EnqueueProjectionJob(ctx, tx, projectionTestProjectKey, revisionID, "observation-limit", payload, now); !errors.Is(err, ErrInvalidProjectionJob) {
		t.Fatalf("oversized projection job error = %v, want ErrInvalidProjectionJob", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM projection_jobs").Scan(&count); err != nil || count != 0 {
		t.Fatalf("oversized job count = %d, err=%v; want zero", count, err)
	}
}
