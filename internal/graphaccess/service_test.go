package graphaccess

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
)

type fakeNeo4jGraphStore struct {
	closeCount atomic.Int32
}

func (*fakeNeo4jGraphStore) ReadProjectSnapshot(context.Context, string) (behaviorgraph.Record, error) {
	return behaviorgraph.Record{}, nil
}

func (*fakeNeo4jGraphStore) ReadProjectNeighborhood(context.Context, string, []string, int) (neo4jstore.Neighborhood, error) {
	return neo4jstore.Neighborhood{}, nil
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
