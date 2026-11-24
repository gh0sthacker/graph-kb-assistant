package ingest

import (
	"path/filepath"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/hunt"
	"bb-graph-assistant/internal/store"
	"fmt"
	"os"
)

type huntSourceFixture struct {
	exchange caido.HttpExchange
	calls    []string
}

func (s *huntSourceFixture) GetExchange(ref caido.ExchangeRef) (*caido.HttpExchange, error) {
	s.calls = append(s.calls, ref.RequestID)
	v := s.exchange
	v.RequestRaw = append([]byte(nil), v.RequestRaw...)
	v.ResponseRaw = append([]byte(nil), v.ResponseRaw...)
	return &v, nil
}

type huntIndexedSourceFixture struct{ huntSourceFixture }

func (s *huntIndexedSourceFixture) FetchHistoryPage(p caido.ProjectRef, page caido.RequestMetadataPage, skip map[string]struct{}, max int) (caido.HistoryPage, error) {
	if len(page.Requests) != 1 || page.Requests[0].RequestID != s.exchange.Ref.RequestID {
		return caido.HistoryPage{}, fmt.Errorf("unexpected indexed read")
	}
	e, err := s.GetExchange(caido.ExchangeRef{Project: p, RequestID: page.Requests[0].RequestID})
	if err != nil {
		return caido.HistoryPage{}, err
	}
	return caido.HistoryPage{Exchanges: []caido.HttpExchange{*e}}, nil
}
func TestLiveImportProjectsHuntEvidence(t *testing.T) {
	ref, selected, w := newLiveTailFixture(t)
	checkpoint, e := w.BeginCycle(t.Context(), time.Now().UTC(), 90)
	if e != nil {
		t.Fatal(e)
	}
	status := 500
	exchange := caido.HttpExchange{Ref: caido.ExchangeRef{Project: ref, RequestID: "hunt-1"}, RequestRaw: []byte("GET /search?q=probe HTTP/1.1\r\nHost: api.fixture.invalid\r\n\r\n"), ResponseRaw: []byte("HTTP/1.1 500 Error\r\nContent-Type: text/plain\r\n\r\nSQLITE_ERROR"), HasResponse: true, Method: "GET", Scheme: "http", Host: "api.fixture.invalid", Port: 80, Target: "http://api.fixture.invalid/search?q=probe", ResponseStatus: &status, RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes"}
	if _, e = w.WritePage(t.Context(), checkpoint, caido.HistoryPage{Exchanges: []caido.HttpExchange{exchange}}); e != nil {
		t.Fatal(e)
	}
	s, e := hunt.OpenReadOnly(filepath.Join(selected.Directory, hunt.DatabaseName))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	g, e := s.Load(t.Context())
	if e != nil || len(g.Result.Candidates) == 0 {
		t.Fatalf("hunt graph %+v %v", g, e)
	}
	s.Close()
	w.Close()
	dir := selected.Directory
	root := filepath.Dir(filepath.Dir(dir))
	selected.Close()
	read, e := store.OpenProjectReadOnly(t.Context(), ref, root)
	if e != nil {
		t.Fatal(e)
	}
	defer read.Close()
	g, e = ReadHuntGraph(t.Context(), read)
	if e != nil || len(g.Result.Candidates) == 0 {
		t.Fatalf("read graph: %+v %v", g.Result, e)
	}
	if e = os.Remove(filepath.Join(dir, hunt.DatabaseName)); e != nil {
		t.Fatal(e)
	}
	g, e = ReadHuntGraph(t.Context(), read)
	if e != nil || len(g.Result.Candidates) != 0 || len(g.Result.CoverageGaps) == 0 {
		t.Fatalf("missing cache must be partial: %+v %v", g.Result, e)
	}
	source := &huntSourceFixture{exchange: exchange}
	result, e := BackfillHunt(t.Context(), read, source)
	if e != nil || result.Projected != 1 || len(source.calls) != 1 || source.calls[0] != "hunt-1" {
		t.Fatalf("backfill: %+v %v", result, e)
	}
	indexedSource := &huntIndexedSourceFixture{huntSourceFixture{exchange: exchange}}
	indexedResult, indexedErr := BackfillHunt(t.Context(), read, indexedSource)
	if indexedErr != nil || indexedResult.Projected != 1 || len(indexedSource.calls) != 1 {
		t.Fatalf("indexed backfill %+v %v", indexedResult, indexedErr)
	}
	source.exchange.ResponseRaw = append(source.exchange.ResponseRaw, []byte("changed")...)
	result, e = BackfillHunt(t.Context(), read, source)
	if e != nil || result.Projected != 0 || result.RevisionMismatches != 1 {
		t.Fatalf("changed capture accepted: %+v %v", result, e)
	}
}
