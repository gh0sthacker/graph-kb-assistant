package graphaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

type fakeNeo4jGraphStore struct {
	closeCount          atomic.Int32
	snapshotReads       atomic.Int32
	analyticalSliceRead atomic.Int32
	signatureLookupRead atomic.Int32
	signatureProjectKey string
	signatureRequest    string
	signatureResponse   string
	signatureIDs        []string
	signatureTruncated  bool
	analyticalSlice     neo4jstore.AnalyticalSlice
}

func (s *fakeNeo4jGraphStore) ReadProjectSnapshot(context.Context, string) (behaviorgraph.Record, error) {
	s.snapshotReads.Add(1)
	return behaviorgraph.Record{}, nil
}

func (*fakeNeo4jGraphStore) ReadProjectNeighborhood(context.Context, string, []string, int) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, nil
}
func (*fakeNeo4jGraphStore) ReadProjectNeighborhoodPage(context.Context, string, []string, int, *neo4jstore.NeighborhoodCursor) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, nil
}

func (s *fakeNeo4jGraphStore) ReadProjectAnalyticalSlice(context.Context, string, []string, neo4jstore.AnalyticalSliceOptions) (neo4jstore.AnalyticalSlice, error) {
	s.analyticalSliceRead.Add(1)
	return s.analyticalSlice, nil
}

func (s *fakeNeo4jGraphStore) ReadProjectObservationIDsByVariantSignatures(_ context.Context, projectKey, requestSignature, responseSignature string, _ int) ([]string, bool, error) {
	s.signatureLookupRead.Add(1)
	s.signatureProjectKey = projectKey
	s.signatureRequest = requestSignature
	s.signatureResponse = responseSignature
	return s.signatureIDs, s.signatureTruncated, nil
}

func (s *fakeNeo4jGraphStore) Close(context.Context) error {
	s.closeCount.Add(1)
	return nil
}

func TestServiceReusesProcessOwnedNeo4jStoreAndClosesItOnce(t *testing.T) {
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "test-password")
	var factoryCalls atomic.Int32
	backend := &fakeNeo4jGraphStore{}
	service := newService(func(context.Context, neo4jstore.Config) (neo4jGraphStore, error) {
		factoryCalls.Add(1)
		return backend, nil
	})

	first, releaseFirst, err := service.acquireNeo4jStore(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, releaseSecond, err := service.acquireNeo4jStore(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if first != second {
		t.Fatal("service created a different Neo4j store for the second operation")
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("Neo4j store factory called %d times, want 1", got)
	}
	releaseFirst()
	releaseSecond()

	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got := backend.closeCount.Load(); got != 1 {
		t.Fatalf("Neo4j store closed %d times, want 1", got)
	}
	if _, release, err := service.acquireNeo4jStore(context.Background()); err == nil {
		release()
		t.Fatal("closed service accepted a new Neo4j operation")
	}
}

func TestServiceCloseWaitsForActiveOperations(t *testing.T) {
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "test-password")
	backend := &fakeNeo4jGraphStore{}
	service := newService(func(context.Context, neo4jstore.Config) (neo4jGraphStore, error) {
		return backend, nil
	})
	_, release, err := service.acquireNeo4jStore(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- service.Close(context.Background()) }()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		service.mu.Lock()
		closed := service.closed
		service.mu.Unlock()
		if closed {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("service close did not begin")
		}
	}
	if got := backend.closeCount.Load(); got != 0 {
		t.Fatalf("Neo4j store closed with an active operation: close count %d", got)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("service close returned before the operation released: %v", err)
	default:
	}

	release()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service close did not finish after the active operation released")
	}
	if got := backend.closeCount.Load(); got != 1 {
		t.Fatalf("Neo4j store closed %d times, want 1", got)
	}
}

func TestClosedServiceRejectsSQLiteNeighborhoodCalls(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	service := NewService()
	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := service.ReadNeighborhood(context.Background(), nil, nil); err == nil || err.Error() != "graph query service is closed" {
		t.Fatalf("closed service neighborhood error = %v, want closed-service error", err)
	}
}

func TestSQLiteNeighborhoodReturnsTypedUnsupportedCursorError(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "synthetic-instance", ProjectID: "continuation-test"}
	writable, err := store.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close synthetic project: %v", err)
	}
	selected, err := store.OpenProjectReadOnly(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("open synthetic project read-only: %v", err)
	}
	defer selected.Close()
	cursor := &neo4jstore.NeighborhoodCursor{
		Generation: 1, Priority: 1, AfterAssertionID: "assertion-" + strings.Repeat("a", 64),
	}
	_, err = NewService().ReadNeighborhoodPage(context.Background(), selected, []string{"endpoint-" + strings.Repeat("b", 64)}, cursor)
	if !errors.Is(err, ErrNeighborhoodCursorUnsupported) {
		t.Fatalf("SQLite continuation error = %v, want typed unsupported error", err)
	}
}

func TestReadAnalyticalSliceUsesBoundedNeo4jQueryWithoutSnapshotReader(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "test-password")
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "synthetic-instance", ProjectID: "analytical-slice-test"}
	writable, err := store.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close synthetic project: %v", err)
	}
	selected, err := store.OpenProjectReadOnly(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("open synthetic project read-only: %v", err)
	}
	defer selected.Close()
	seed := graphFixtureID("endpoint", "analytical-seed")
	backend := &fakeNeo4jGraphStore{analyticalSlice: neo4jstore.AnalyticalSlice{Generation: 7}}
	service := newService(func(context.Context, neo4jstore.Config) (neo4jGraphStore, error) { return backend, nil })
	defer service.Close(context.Background())
	result, err := service.ReadAnalyticalSlice(context.Background(), selected, []string{seed}, neo4jstore.AnalyticalSliceOptions{
		MaxNodes: 20, MaxEdges: 30, MaxDepth: 2,
	})
	if err != nil {
		t.Fatalf("ReadAnalyticalSlice(): %v", err)
	}
	if result.Generation != 7 || backend.analyticalSliceRead.Load() != 1 {
		t.Fatalf("bounded query result/call count = generation %d, reads %d", result.Generation, backend.analyticalSliceRead.Load())
	}
	if backend.snapshotReads.Load() != 0 {
		t.Fatalf("bounded analytical query opened the full snapshot %d times", backend.snapshotReads.Load())
	}
}

func TestSignatureLookupUsesBoundedNeo4jStoreWithoutSnapshotReader(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "test-password")
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "synthetic-instance", ProjectID: "signature-lookup-test"}
	writable, err := store.CreateProject(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close synthetic project: %v", err)
	}
	selected, err := store.OpenProjectReadOnly(context.Background(), ref, root)
	if err != nil {
		t.Fatalf("open synthetic project read-only: %v", err)
	}
	defer selected.Close()

	wantIDs := []string{"observation-" + strings.Repeat("a", 64)}
	backend := &fakeNeo4jGraphStore{
		signatureProjectKey: selected.ProjectKey,
		signatureRequest:    strings.Repeat("b", 64),
		signatureResponse:   strings.Repeat("c", 64),
		signatureIDs:        wantIDs,
		signatureTruncated:  true,
	}
	service := newService(func(context.Context, neo4jstore.Config) (neo4jGraphStore, error) { return backend, nil })
	defer service.Close(context.Background())
	gotIDs, truncated, err := service.ObservationIDsByVariantSignatures(
		context.Background(), selected, backend.signatureRequest, backend.signatureResponse, behaviorgraph.MaxVariantObservations,
	)
	if err != nil {
		t.Fatalf("ObservationIDsByVariantSignatures(): %v", err)
	}
	if !truncated || len(gotIDs) != 1 || gotIDs[0] != wantIDs[0] {
		t.Fatalf("signature lookup result = %#v truncated=%t", gotIDs, truncated)
	}
	if backend.signatureLookupRead.Load() != 1 || backend.signatureProjectKey != selected.ProjectKey {
		t.Fatalf("signature query calls=%d project=%q, want one query for %q", backend.signatureLookupRead.Load(), backend.signatureProjectKey, selected.ProjectKey)
	}
	if backend.snapshotReads.Load() != 0 {
		t.Fatalf("bounded signature query loaded %d complete snapshots", backend.snapshotReads.Load())
	}
}

func TestNeighborhoodSliceBudgetFitsEveryHighDegreePageCandidate(t *testing.T) {
	projectKey := strings.Repeat("a", 64)
	seed := graphFixtureID("asset", "high-degree-seed")
	snapshot := highDegreeNeighborhoodSnapshot(projectKey, seed, 99)
	options := neighborhoodSliceOptions([]string{seed})
	if options.MaxNodes != 100 || options.MaxEdges != 99 {
		t.Fatalf("one-seed page budget = nodes %d edges %d, want 100 and 99", options.MaxNodes, options.MaxEdges)
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(context.Background(), snapshot, projectKey, t.TempDir())
	if err != nil {
		t.Fatalf("open synthetic high-degree graph: %v", err)
	}
	defer reader.Close()
	page, err := reader.Slice(context.Background(), options)
	if err != nil {
		t.Fatalf("slice synthetic high-degree graph: %v", err)
	}
	if got := len(page["assertions"].([]behaviorgraph.Record)); got != 99 {
		t.Fatalf("slice returned %d assertions from a 99-candidate page", got)
	}
	if got := len(page["entities"].([]behaviorgraph.Record)); got != 100 {
		t.Fatalf("slice returned %d nodes from a 100-node page", got)
	}
	seeds := make([]string, 20)
	for i := range seeds {
		seeds[i] = graphFixtureID("endpoint", fmt.Sprintf("seed-%d", i))
	}
	if got := neighborhoodSliceOptions(seeds).MaxEdges; got != 80 {
		t.Fatalf("20-seed page edge budget = %d, want 80", got)
	}
}

func highDegreeNeighborhoodSnapshot(projectKey, seed string, edgeCount int) behaviorgraph.Record {
	refs := []behaviorgraph.Record{{"path": "/synthetic/high-degree.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.record"}}
	entities := []behaviorgraph.Record{{"id": seed, "kind": "asset", "data": map[string]any{"label": "synthetic host"}, "source_refs": refs}}
	assertions := make([]behaviorgraph.Record, 0, edgeCount)
	for index := 0; index < edgeCount; index++ {
		endpoint := graphFixtureID("endpoint", fmt.Sprintf("endpoint-%d", index))
		entities = append(entities, behaviorgraph.Record{"id": endpoint, "kind": "endpoint", "data": map[string]any{"method": "GET"}, "source_refs": refs})
		assertion := behaviorgraph.Record{
			"subject": endpoint, "object": seed, "predicate": "references",
			"context": map[string]any{"program": projectKey}, "origin_kind": "parsed", "evidence_state": "observed", "source_refs": refs,
		}
		identity, _ := json.Marshal(behaviorgraph.Record{"kind": "assertion", "identity": assertion})
		digest := sha256.Sum256(identity)
		assertion["id"] = "assertion-" + hex.EncodeToString(digest[:])
		assertions = append(assertions, assertion)
	}
	return behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"entities": entities, "assertions": assertions, "sources": refs, "cards": []behaviorgraph.Record{},
	}
}

func graphFixtureID(kind, name string) string {
	digest := sha256.Sum256([]byte(name))
	return kind + "-" + hex.EncodeToString(digest[:])
}
