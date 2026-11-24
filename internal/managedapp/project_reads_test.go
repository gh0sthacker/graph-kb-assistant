package managedapp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

// A completed projection must give Explorer readers a turn before the next
// delivery. The old managed loop kept its exclusive lease across the entire
// batch, making Surface fail with ErrorProjectUnavailable despite committed rows.
func TestProjectWorkerAllowsExplorerReadsBetweenDeliveries(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	// Missing Neo4j configuration makes deliveries retryable without network I/O.
	t.Setenv("BB_ASSISTANT_NEO4J_URI", "")
	t.Setenv("BB_ASSISTANT_NEO4J_USERNAME", "")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "")
	t.Setenv("BB_ASSISTANT_VAULT_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	options := DefaultOptions()
	options.DataRoot = t.TempDir()
	options.MaxJobs = 2
	ref := project.CaidoProjectRef{InstanceID: "explorer-worker", ProjectID: "synthetic-project"}
	selected, err := store.CreateProject(t.Context(), ref, options.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	selection := selectedProject{ref: ref, key: selected.ProjectKey, directory: selected.Directory}
	lease, err := projectlock.Acquire(t.Context(), selected.Directory)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := ingest.OpenPageWriter(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := writer.BeginRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	exchanges := make([]caido.HttpExchange, 0, 2)
	for i := range 2 {
		route := fmt.Sprintf("/items/%d", i)
		exchanges = append(exchanges, caido.HttpExchange{
			Ref:        caido.ExchangeRef{Project: ref, RequestID: fmt.Sprintf("request-%d", i)},
			RequestRaw: []byte("GET " + route + " HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"),
			Method:     "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
			Target: "https://api.fixture.invalid" + route, RequestWireFidelity: "source_bytes", ResponseWireFidelity: "unknown",
		})
	}
	accepted, err := writer.WritePage(t.Context(), ingest.HistoryPageInput{
		RunID: checkpoint.RunID, Page: caido.HistoryPage{Exchanges: exchanges},
	})
	writer.Close()
	if err != nil || accepted.Stored != 2 {
		t.Fatalf("synthetic intake: stored=%d err=%v", accepted.Stored, err)
	}
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	queries, err := appquery.NewGraphQueryService(options.DataRoot, []project.CaidoProjectRef{ref}, graphaccess.NewService())
	if err != nil {
		t.Fatal(err)
	}
	defer queries.Close(context.Background())

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	completed := 0
	err = runProjectWorker(ctx, options, selection, func(event Event) {
		if event.State != "pending" {
			return
		}
		completed++
		readCtx, cancelRead := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancelRead()
		result, readErr := queries.Surface(readCtx, appquery.SurfaceRequest{ProjectKey: selection.key, Limit: 40})
		if readErr != nil || result.TotalOperations == 0 || result.TotalObservations != 2 {
			t.Errorf("Explorer surface after delivery %d: operations=%d observations=%d error=%v", completed, result.TotalOperations, result.TotalObservations, readErr)
		}
		// Graph and evidence readers enter through this same read-only store seam.
		opened, openErr := store.OpenProjectReadOnly(readCtx, ref, options.DataRoot)
		if openErr != nil {
			t.Errorf("Explorer graph store after delivery %d: %v", completed, openErr)
		} else if err := opened.Close(); err != nil {
			t.Error(err)
		}
		if completed == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || completed != 2 {
		t.Fatalf("worker completed=%d err=%v; want two retryable deliveries and clean cancellation", completed, err)
	}
	var pending int
	readOnly, err := store.OpenProjectReadOnly(t.Context(), ref, options.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := readOnly.Database.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM projection_jobs WHERE state='pending' AND attempt_count=1").Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("retryable work was lost: pending=%d err=%v", pending, err)
	}
}
