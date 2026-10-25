package graphaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

type fakeNeo4jGraphStore struct {
	closeCount           atomic.Int32
	snapshotReads        atomic.Int32
	analyticalSliceRead  atomic.Int32
	signatureLookupRead  atomic.Int32
	signatureProjectKey  string
	signatureRequest     string
	signatureResponse    string
	signatureIDs         []string
	signatureTruncated   bool
	analyticalSlice      neo4jstore.AnalyticalSlice
	fieldValueSnapshot   behaviorgraph.Record
	fieldValueReceipts   string
	fieldValueGeneration int64
}

func (s *fakeNeo4jGraphStore) ReadProjectSnapshot(context.Context, string) (behaviorgraph.Record, error) {
	s.snapshotReads.Add(1)
	return behaviorgraph.Record{}, nil
}

func (*fakeNeo4jGraphStore) ReadProjectNeighborhood(context.Context, string, []string, int) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, nil
}
func (s *fakeNeo4jGraphStore) ReadProjectNeighborhoodPage(_ context.Context, projectKey string, _ []string, _ int, _ *neo4jstore.NeighborhoodCursor) (neo4jstore.Neighborhood, error) {
	if s.fieldValueSnapshot != nil {
		if projectKey != s.fieldValueSnapshot["program"] {
			return neo4jstore.Neighborhood{}, errors.New("synthetic project namespace mismatch")
		}
		return neo4jstore.Neighborhood{Snapshot: s.fieldValueSnapshot, Generation: s.fieldValueGeneration}, nil
	}
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

func (s *fakeNeo4jGraphStore) ReadProjectFieldValueTraversalSeeds(ctx context.Context, projectKey, observationID string, maxPaths int) ([]string, bool, int64, error) {
	if s.fieldValueSnapshot == nil || projectKey != s.fieldValueSnapshot["program"] {
		return nil, false, 0, errors.New("synthetic project namespace mismatch")
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, s.fieldValueSnapshot, projectKey, s.fieldValueReceipts)
	if err != nil {
		return nil, false, 0, err
	}
	defer reader.Close()
	ids, truncated, err := reader.FieldValueTraversalSeeds(observationID, maxPaths)
	return ids, truncated, s.fieldValueGeneration, err
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

func TestFieldValueTraversalGraphNeighborhoodReachesSecondCapture(t *testing.T) {
	ctx := context.Background()
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "synthetic-password")
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "field-value-instance", ProjectID: "field-value-project"}
	writable, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	projectKey, receiptRoot := writable.ProjectKey, writable.BehaviorReceiptPath
	fieldValueKey := []byte(strings.Repeat("k", 32))
	defer clear(fieldValueKey)
	rawA := "GET /items?id=SYNTHETIC_CROSS_CAPTURE_702 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"
	rawB := "GET /cart?item=SYNTHETIC_CROSS_CAPTURE_702 HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"
	bundleA := buildGraphValueCapture(t, ref, projectKey, receiptRoot, fieldValueKey, "capture-a", rawA)
	bundleB := buildGraphValueCapture(t, ref, projectKey, receiptRoot, fieldValueKey, "capture-b", rawB)
	snapshot := mergeGraphValueCaptures(projectKey, bundleA, bundleB)
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	selected, err := store.OpenProjectReadOnly(ctx, ref, root)
	if err != nil {
		t.Fatalf("open synthetic project read-only: %v", err)
	}
	defer selected.Close()

	backend := &fakeNeo4jGraphStore{fieldValueSnapshot: snapshot, fieldValueReceipts: receiptRoot, fieldValueGeneration: 17}
	service := newService(func(context.Context, neo4jstore.Config) (neo4jGraphStore, error) { return backend, nil })
	defer service.Close(context.Background())
	observationA := graphValueEntityID(bundleA, "observation")
	observationB := graphValueEntityID(bundleB, "observation")
	fieldA := graphValueEntityID(bundleA, "field_occurrence")
	fieldB := graphValueEntityID(bundleB, "field_occurrence")
	valueReference := graphValueEntityID(bundleA, "value_reference")
	seeds, truncated, generation, err := service.FieldValueTraversalSeeds(ctx, selected, observationA, behaviorgraph.MaxFieldValueTraversalPaths)
	if err != nil || truncated || generation != 17 {
		t.Fatalf("FieldValueTraversalSeeds(): seeds=%v truncated=%t generation=%d err=%v", seeds, truncated, generation, err)
	}
	page, err := service.ReadNeighborhoodPage(ctx, selected, seeds, nil)
	if err != nil {
		t.Fatalf("ReadNeighborhoodPage(): %v", err)
	}
	if page.Generation != generation {
		t.Fatalf("traversal generation %d differs from graph page %d", generation, page.Generation)
	}
	entities := page.Payload["entities"].([]behaviorgraph.Record)
	assertions := page.Payload["assertions"].([]behaviorgraph.Record)
	entityIDs, entityKinds, predicates := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, entity := range entities {
		entityIDs[entity["id"].(string)] = true
		entityKinds[entity["kind"].(string)] = true
	}
	for _, assertion := range assertions {
		predicates[assertion["predicate"].(string)] = true
	}
	for _, id := range []string{observationA, fieldA, valueReference, fieldB, observationB} {
		if !entityIDs[id] {
			t.Errorf("Neo4j graph page omitted path node %s", id)
		}
	}
	for _, kind := range []string{"field_occurrence", "value_reference", "observation"} {
		if !entityKinds[kind] {
			t.Errorf("Neo4j graph page omitted entity kind %s", kind)
		}
	}
	for _, predicate := range []string{"has_occurrence", "occurrence_of", "value_used_in"} {
		if !predicates[predicate] {
			t.Errorf("Neo4j graph page omitted relationship %s", predicate)
		}
	}
	encoded, err := json.Marshal(page.Payload)
	if err != nil || strings.Contains(string(encoded), "SYNTHETIC_CROSS_CAPTURE_702") {
		t.Fatalf("Neo4j graph page exposed a raw value or failed to encode: err=%v", err)
	}
	if _, _, _, err := backend.ReadProjectFieldValueTraversalSeeds(ctx, strings.Repeat("f", 64), observationA, behaviorgraph.MaxFieldValueTraversalPaths); err == nil {
		t.Fatal("field-value traversal accepted a different project namespace")
	}
}

func buildGraphValueCapture(t *testing.T, ref project.CaidoProjectRef, projectKey, receiptRoot string, fieldValueKey []byte, requestID, raw string) behaviorgraph.Record {
	t.Helper()
	projectRef := caido.ProjectRef{InstanceID: ref.InstanceID, ProjectID: ref.ProjectID}
	receiptPath := filepath.Join(receiptRoot, requestID+".json")
	receiptBytes := []byte("synthetic receipt " + requestID)
	if err := os.WriteFile(receiptPath, receiptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	receiptDigest := sha256.Sum256(receiptBytes)
	status, notTruncated := 200, false
	requestLine := strings.SplitN(raw, "\r\n", 2)[0]
	fields := strings.Fields(requestLine)
	target := "/"
	if len(fields) > 1 {
		target = fields[1]
	}
	exchange := caido.HttpExchange{
		Ref: caido.ExchangeRef{Project: projectRef, RequestID: requestID}, RequestRaw: []byte(raw),
		ResponseRaw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), HasResponse: true,
		Method: "GET", Scheme: "https", Host: "fixture.invalid", Port: 443, Target: target,
		ResponseStatus: &status, RequestTruncated: &notTruncated, ResponseTruncated: &notTruncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
	bundle, _, err := behaviorgraph.BuildHistoricalBundleWithSignaturesAndFieldValueKey(exchange, projectKey, behaviorgraph.Record{
		"path": receiptPath, "sha256": hex.EncodeToString(receiptDigest[:]), "record_locator": "$.records[0]",
	}, strings.Repeat("b", 64), stringPointer(strings.Repeat("c", 64)), fieldValueKey, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func mergeGraphValueCaptures(projectKey string, bundles ...behaviorgraph.Record) behaviorgraph.Record {
	sources := []behaviorgraph.Record{}
	entitiesByID := map[string]behaviorgraph.Record{}
	assertions := []behaviorgraph.Record{}
	cards := []behaviorgraph.Record{}
	for _, bundle := range bundles {
		sources = append(sources, bundle["sources"].([]behaviorgraph.Record)...)
		for _, entity := range bundle["entities"].([]behaviorgraph.Record) {
			if _, exists := entitiesByID[entity["id"].(string)]; !exists {
				entitiesByID[entity["id"].(string)] = entity
			}
		}
		assertions = append(assertions, bundle["assertions"].([]behaviorgraph.Record)...)
		cards = append(cards, bundle["cards"].([]behaviorgraph.Record)...)
	}
	entities := make([]behaviorgraph.Record, 0, len(entitiesByID))
	for _, entity := range entitiesByID {
		entities = append(entities, entity)
	}
	return behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"sources": sources, "entities": entities, "assertions": assertions, "cards": cards,
	}
}

func graphValueEntityID(bundle behaviorgraph.Record, kind string) string {
	for _, entity := range bundle["entities"].([]behaviorgraph.Record) {
		if entity["kind"] == kind {
			return entity["id"].(string)
		}
	}
	return ""
}

func stringPointer(value string) *string { return &value }

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
