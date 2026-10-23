package ingest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func TestReadCaptureMessageForGraphRebuildIsAuthenticatedAndBounded(t *testing.T) {
	ctx := context.Background()
	selected, writable, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, writable)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	t.Cleanup(pageWriter.Close)
	checkpoint, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatalf("BeginRun() error = %v", err)
	}
	requestID := "bounded-rebuild-read"
	request := []byte("POST /api/items?id=one&id=two HTTP/1.1\r\nHost: api.fixture.invalid\r\nContent-Type: application/json\r\n\r\n{\"id\":\"fixture-value\"}")
	response := append([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n"), bytes.Repeat([]byte("x"), maxGraphReprojectionMessageBytes)...)
	status := 200
	exchange := caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: selected, RequestID: requestID},
		RequestRaw: request, ResponseRaw: response, HasResponse: true,
		Method: "POST", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/api/items?id=one&id=two", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	if _, err := pageWriter.WritePage(ctx, HistoryPageInput{
		RunID: checkpoint.RunID,
		Page:  caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}},
	}); err != nil {
		t.Fatalf("WritePage() error = %v", err)
	}
	var busy, logFrames, checkpointed int
	if err := writable.Database.DB().QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil || busy != 0 {
		t.Fatal("fixture capture database could not be checkpointed")
	}
	pageWriter.Close()
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable fixture: %v", err)
	}

	projectDir, err := store.ProjectDirectory(selected, filepath.Dir(filepath.Dir(writable.Directory)))
	if err != nil {
		t.Fatalf("ProjectDirectory() error = %v", err)
	}
	lease, err := projectlock.Acquire(ctx, projectDir)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if err := removeClosedFixtureSQLiteSidecars(filepath.Join(projectDir, "graph.sqlite3")); err != nil {
		t.Fatalf("remove closed fixture sidecars: %v", err)
	}
	readonly, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected, filepath.Dir(filepath.Dir(projectDir)), lease)
	if err != nil {
		t.Fatalf("OpenProjectReadOnlyUnderMaintenance() error = %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	full, err := ReadCaptureMessageForGraphRebuild(ctx, readonly, requestID, "request")
	if err != nil {
		t.Fatalf("ReadCaptureMessageForGraphRebuild(request) error = %v", err)
	}
	if !bytes.Equal(full.Data, request) || int64(len(full.Data)) != full.TotalSize || full.SHA256 != sha256Hex(request) {
		t.Fatal("full historical request did not match its authenticated fixture")
	}
	clear(full.Data)

	if _, err := ReadCaptureMessageForGraphRebuild(ctx, readonly, requestID, "response"); err == nil {
		t.Fatal("full graph-rebuild read accepted a response over its 1 MiB bound")
	}
	segment, err := ReadCaptureRange(ctx, readonly, requestID, "response", 0, 16)
	if err != nil {
		t.Fatalf("ReadCaptureRange(response) error = %v", err)
	}
	if len(segment.Data) != 16 || !bytes.Equal(segment.Data, response[:16]) || segment.TotalSize != int64(len(response)) {
		t.Fatal("bounded range read did not return the requested authenticated response prefix")
	}
	clear(segment.Data)
}

func removeClosedFixtureSQLiteSidecars(databasePath string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := databasePath + suffix
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("fixture SQLite sidecar is unavailable")
		}
		if suffix == "-wal" && info.Size() != 0 {
			return errors.New("fixture SQLite WAL is not empty")
		}
		if err := os.Remove(path); err != nil {
			return errors.New("fixture SQLite sidecar could not be removed")
		}
	}
	return nil
}
