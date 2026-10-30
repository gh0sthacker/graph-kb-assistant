package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/projectlock"
)

func TestRunLiveTailLargeStructuralBundleReplaysWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	selected, projectStore, writer := newLiveTailFixture(t)
	// A small JSON response expands into revision-bound field occurrences,
	// value references and provenance. This reproduces the product-list response
	// that exceeded the old 4 MiB job limit, without depending on live traffic.
	var body strings.Builder
	body.WriteString(`{"status":"success","data":[`)
	for index := 0; index < 70; index++ {
		if index > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"id":%d,"name":"item-%d","description":"description-%d","price":1.5,"deluxePrice":1,"image":"image-%d.jpg","alternateImages":[],"createdAt":"2026-10-30T10:00:00Z","updatedAt":"2026-10-30T10:00:00Z","deletedAt":null}`, index, index, index, index)
	}
	body.WriteString(`]}`)
	status := 200
	truncated := false
	exchange := caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: selected, RequestID: "large-product-list"},
		RequestRaw:  []byte("GET /api/products HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + body.String()),
		HasResponse: true, ResponseStatus: &status, Method: "GET", Scheme: "https",
		Host: "api.fixture.invalid", Port: 443, Target: "https://api.fixture.invalid/api/products",
		RequestWireFidelity: "reconstructed_text", ResponseWireFidelity: "reconstructed_text",
		RequestTruncated: &truncated, ResponseTruncated: &truncated,
	}
	prepared, err := writer.prepareLiveCaptures([]caido.HttpExchange{exchange})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(prepared[0].bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 4<<20 || len(payload) > behaviorgraph.MaxBundleJSONBytes {
		t.Fatalf("fixture bundle = %d bytes, want above the old 4 MiB limit and within the graph writer limit", len(payload))
	}
	metadata := caido.RequestMetadataPage{Requests: []caido.RequestMetadata{{
		RequestID: exchange.Ref.RequestID, Method: exchange.Method, URL: exchange.Target, StatusCode: &status,
	}}}
	cursor := "large-product-replay"
	firstMetadata := metadata
	firstMetadata.HasMore = true
	firstMetadata.NextCursor = &cursor
	source := &liveTailSourceFixture{
		project: selected, pages: []caido.RequestMetadataPage{firstMetadata, metadata},
		exchanges: map[string]caido.HttpExchange{exchange.Ref.RequestID: exchange},
	}
	options := LiveTailOptions{PageSize: 1, MaxPages: 1, OverlapSeconds: 90, MaxBodyBytes: 64 << 10}
	// Stop at the page cap, then resume the same window with an overlapping row.
	first, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil || first.RequestsAdded != 1 || first.State != "partial" || !first.CursorPending {
		t.Fatalf("first RunLiveTail() = %+v, err=%v; want one accepted capture", first, err)
	}
	second, err := RunLiveTail(ctx, source, writer, selected, options)
	if err != nil || second.RequestsAdded != 0 || second.ResponsesRefreshed != 0 || second.State != "complete" {
		t.Fatalf("repeated RunLiveTail() = %+v, err=%v; want a deduplicated idle window", second, err)
	}
	if source.httpqlFilters[0] != source.httpqlFilters[1] {
		t.Fatalf("replay changed its window: %q != %q", source.httpqlFilters[0], source.httpqlFilters[1])
	}
	// Also replay the detail bytes directly: correctness must not rely solely
	// on the metadata skip optimization.
	now := time.Now().UTC()
	checkpoint, err := writer.BeginCycle(ctx, now, options.OverlapSeconds)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := writer.WritePage(ctx, checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}})
	if err != nil || replay.RequestsAdded != 0 || replay.ResponsesRefreshed != 0 {
		t.Fatalf("detail replay = %+v, err=%v; want no duplicate revision", replay, err)
	}
	for _, table := range []string{"live_capture_index", "live_capture_fts", "capture_revision_registry", "projection_jobs"} {
		var count int
		if err := projectStore.Database.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count = %d, err=%v; want one after replay", table, count, err)
		}
	}
	job, err := behaviorgraph.ProjectionJobByRevision(ctx, projectStore.Database.DB(), projectStore.ProjectKey, prepared[0].revisionID)
	if err != nil || job.GraphObservationID != prepared[0].graphObservationID || len(job.PayloadJSON) <= 4<<20 {
		t.Fatalf("large durable projection job: observation=%q bytes=%d err=%v", job.GraphObservationID, len(job.PayloadJSON), err)
	}
	maintenance, err := projectlock.Acquire(ctx, projectStore.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	result, err := runProjectionWorkerOnce(ctx, projectStore, maintenance, ProjectionWorkerOptions{}, materializeLocalProjection, now)
	if err != nil || result.State != "applied" {
		t.Fatalf("large bundle projection = %+v, err=%v", result, err)
	}
}
