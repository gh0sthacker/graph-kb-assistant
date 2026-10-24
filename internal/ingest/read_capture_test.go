package ingest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
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
	if got, want := vaultCiphertextSize(int64(len(request))), int64(len(request)+len(vaultMagic)+12+16); got != want {
		t.Fatalf("vault ciphertext bound = %d, want %d", got, want)
	}
	var requestVaultRef string
	if err := readonly.Database.DB().QueryRowContext(ctx,
		"SELECT request_vault_ref FROM capture_revisions WHERE revision_id=(SELECT current_revision_id FROM captures WHERE request_id=?)",
		requestID,
	).Scan(&requestVaultRef); err != nil {
		t.Fatalf("load request vault reference: %v", err)
	}
	requestVaultPath := filepath.Join(readonly.VaultDirectory, requestVaultRef)
	file, err := os.OpenFile(requestVaultPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open request vault object: %v", err)
	}
	if _, err := file.Write(bytes.Repeat([]byte("x"), 2<<20)); err != nil {
		_ = file.Close()
		t.Fatalf("append oversized ciphertext fixture: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close oversized ciphertext fixture: %v", err)
	}
	if _, err := ReadCaptureMessageForGraphRebuild(ctx, readonly, requestID, "request"); err == nil {
		t.Fatal("full graph-rebuild read accepted ciphertext larger than the indexed message plus AEAD overhead")
	}

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

	var oversizedRevision, oversizedVaultRef string
	if err := readonly.Database.DB().QueryRowContext(ctx, `
		SELECT cr.revision_id,cr.response_vault_ref
		FROM captures AS c JOIN capture_revisions AS cr ON cr.observation_id=c.observation_id
		WHERE c.request_id=? AND cr.revision_id=c.current_revision_id`, requestID,
	).Scan(&oversizedRevision, &oversizedVaultRef); err != nil {
		t.Fatalf("load oversized response reference: %v", err)
	}
	if err := os.Remove(filepath.Join(readonly.VaultDirectory, oversizedVaultRef)); err != nil {
		t.Fatalf("remove oversized response fixture to prove preflight ordering: %v", err)
	}
	oversized, err := ReadCaptureRevisionForRedaction(ctx, readonly, requestID, oversizedRevision, "response")
	if !errors.Is(err, ErrCaptureContextLimit) {
		t.Fatalf("oversized redaction-context read error = %v, want context-limit before vault access", err)
	}
	if len(oversized.Data) != 0 || oversized.TotalSize != int64(len(response)) || oversized.RevisionID == nil || *oversized.RevisionID != oversizedRevision || oversized.IntegrityStatus != CaptureIntegrityIndexedOnly {
		t.Fatalf("oversized context metadata = %#v; want exact revision and size without plaintext", oversized)
	}
}

func TestReadCaptureRevisionRangePinsPriorRevisionAfterCurrentChanges(t *testing.T) {
	ctx := context.Background()
	selected, writable, _ := newLiveTailFixture(t)
	pageWriter, err := OpenPageWriter(ctx, writable)
	if err != nil {
		t.Fatalf("OpenPageWriter() error = %v", err)
	}
	firstRun, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatalf("BeginRun(first) error = %v", err)
	}
	requestID := "revision-pinned-evidence"
	firstRequest := []byte("GET /items/one HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	firstResponse := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\none")
	firstExchange := fixtureHistoricalExchange(selected, requestID, firstRequest, firstResponse)
	if _, err := pageWriter.WritePage(ctx, HistoryPageInput{RunID: firstRun.RunID, Page: caido.HistoryPage{Exchanges: []caido.HttpExchange{firstExchange}}}); err != nil {
		t.Fatalf("WritePage(first) error = %v", err)
	}
	observationID, err := (project.CaidoExchangeRef{Project: selected, RequestID: requestID}).ObservationID()
	if err != nil {
		t.Fatalf("ObservationID() error = %v", err)
	}
	var firstRevision string
	if err := writable.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE observation_id=?", observationID,
	).Scan(&firstRevision); err != nil {
		t.Fatalf("read first revision: %v", err)
	}

	secondRun, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatalf("BeginRun(second) error = %v", err)
	}
	secondRequest := []byte("GET /items/two HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n")
	secondResponse := []byte("HTTP/1.1 201 Created\r\nContent-Type: text/plain\r\n\r\ntwo")
	secondExchange := fixtureHistoricalExchange(selected, requestID, secondRequest, secondResponse)
	if _, err := pageWriter.WritePage(ctx, HistoryPageInput{RunID: secondRun.RunID, Page: caido.HistoryPage{Exchanges: []caido.HttpExchange{secondExchange}}}); err != nil {
		t.Fatalf("WritePage(second) error = %v", err)
	}
	var secondRevision string
	if err := writable.Database.DB().QueryRowContext(ctx,
		"SELECT current_revision_id FROM captures WHERE observation_id=?", observationID,
	).Scan(&secondRevision); err != nil {
		t.Fatalf("read second revision: %v", err)
	}
	if firstRevision == secondRevision {
		t.Fatal("updated message retained the original revision identity")
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
		t.Fatalf("remove closed fixture SQLite sidecars: %v", err)
	}
	readonly, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, selected, filepath.Dir(filepath.Dir(projectDir)), lease)
	if err != nil {
		t.Fatalf("OpenProjectReadOnlyUnderMaintenance() error = %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	for _, test := range []struct {
		name, revision string
		want           []byte
	}{
		{name: "prior revision", revision: firstRevision, want: firstRequest},
		{name: "current revision", revision: secondRevision, want: secondRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ReadCaptureRevisionRange(ctx, readonly, requestID, test.revision, "request", 0, maxRawCaptureRange)
			if err != nil {
				t.Fatalf("ReadCaptureRevisionRange() error = %v", err)
			}
			if !bytes.Equal(got.Data, test.want) || got.RevisionID == nil || *got.RevisionID != test.revision {
				t.Fatalf("range = %q at revision %v, want %q at %q", got.Data, got.RevisionID, test.want, test.revision)
			}
			if test.name == "prior revision" && got.ParseState != "unknown" {
				t.Fatalf("prior revision parse state = %q, want unknown because only current parsing state is stored", got.ParseState)
			}
			clear(got.Data)
		})
	}
	if _, err := ReadCaptureRevisionRange(ctx, readonly, requestID, strings.Repeat("f", 64), "request", 0, 16); err == nil {
		t.Fatal("reader accepted a revision not belonging to the selected request")
	}
	latest, err := ReadCaptureRange(ctx, readonly, requestID, "request", 0, maxRawCaptureRange)
	if err != nil || !bytes.Equal(latest.Data, secondRequest) {
		t.Fatalf("un-pinned range = %q, err %v; want current revision %q", latest.Data, err, secondRequest)
	}
	clear(latest.Data)
}

func TestBoundedCaptureRangeKeepsSequentialUTF8PagesOnRuneBoundaries(t *testing.T) {
	data := []byte("a€z")
	first := boundedCaptureRange(data, 0, 3)
	if string(first) != "a" {
		t.Fatalf("first page = %q, want complete ASCII prefix", first)
	}
	second := boundedCaptureRange(data, int64(len(first)), 3)
	if string(second) != "€" {
		t.Fatalf("second page = %q, want complete euro sign", second)
	}
	third := boundedCaptureRange(data, int64(len(first)+len(second)), 3)
	if string(third) != "z" {
		t.Fatalf("third page = %q, want trailing ASCII byte", third)
	}
	if got := append(append(append([]byte(nil), first...), second...), third...); string(got) != string(data) {
		t.Fatalf("sequential pages = %q, want exact source bytes %q", got, data)
	}

	// Arbitrary offsets still return the exact requested byte range; callers
	// outside the sequential text viewer can identify that page as binary.
	arbitrary := boundedCaptureRange(data, 2, 2)
	if string(arbitrary) != string(data[2:4]) {
		t.Fatalf("arbitrary offset page = %v, want %v", arbitrary, data[2:4])
	}
}

func fixtureHistoricalExchange(selected project.CaidoProjectRef, requestID string, request, response []byte) caido.HttpExchange {
	status := 200
	return caido.HttpExchange{
		Ref:        caido.ExchangeRef{Project: selected, RequestID: requestID},
		RequestRaw: request, ResponseRaw: response, HasResponse: true,
		Method: "GET", Scheme: "https", Host: "api.fixture.invalid", Port: 443,
		Target: "https://api.fixture.invalid/", ResponseStatus: &status,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
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
