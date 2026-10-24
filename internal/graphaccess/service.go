package graphaccess

import (
	"context"
	"errors"
	"sync"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
)

// GraphStore is the narrow Neo4j query surface used by graph access. It keeps
// application-query tests independent of a running Neo4j process.
type GraphStore interface {
	ReadProjectSnapshot(context.Context, string) (behaviorgraph.Record, error)
	ReadProjectNeighborhood(context.Context, string, []string, int) (neo4jstore.Neighborhood, error)
	ReadProjectNeighborhoodPage(context.Context, string, []string, int, *neo4jstore.NeighborhoodCursor) (neo4jstore.Neighborhood, error)
	ReadProjectAnalyticalSlice(context.Context, string, []string, neo4jstore.AnalyticalSliceOptions) (neo4jstore.AnalyticalSlice, error)
	ReadProjectObservationIDsByVariantSignatures(context.Context, string, string, string, int) ([]string, bool, error)
	Close(context.Context) error
}

// GraphStoreFactory creates the Neo4j-backed query store after the service
// has validated its configured connection. The constructor is also used by
// deterministic integration fixtures that provide an in-memory store.
type GraphStoreFactory func(context.Context, neo4jstore.Config) (GraphStore, error)

type neo4jGraphStore = GraphStore
type neo4jStoreFactory = GraphStoreFactory

// Service owns the Neo4j driver for one application process. Operations use
// short-lived sessions through neo4jstore; the driver is closed only after all
// acquired operations have returned.
type Service struct {
	mu        sync.Mutex
	active    sync.WaitGroup
	store     neo4jGraphStore
	factory   neo4jStoreFactory
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// NewService creates a lazy graph access service. It does not connect to
// Neo4j until a request selects that backend, so local startup remains usable
// while Neo4j is temporarily unavailable.
func NewService() *Service {
	return NewServiceWithFactory(func(ctx context.Context, cfg neo4jstore.Config) (GraphStore, error) {
		return neo4jstore.New(ctx, cfg)
	})
}

// NewServiceWithFactory creates a process-owned graph service with an injected
// Neo4j store factory. Production callers should normally use NewService.
func NewServiceWithFactory(factory GraphStoreFactory) *Service {
	return &Service{factory: factory}
}

func newService(factory neo4jStoreFactory) *Service {
	return NewServiceWithFactory(factory)
}

func (s *Service) acquireNeo4jStore(ctx context.Context) (neo4jGraphStore, func(), error) {
	if s == nil {
		return nil, nil, errors.New("graph query service is unavailable")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errors.New("graph query service is closed")
	}
	if s.store == nil {
		cfg, err := neo4jstore.ConfigFromEnv()
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		if s.factory == nil {
			s.mu.Unlock()
			return nil, nil, errors.New("Neo4j store factory is unavailable")
		}
		opened, err := s.factory(ctx, cfg)
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		if opened == nil {
			s.mu.Unlock()
			return nil, nil, errors.New("Neo4j store factory returned no store")
		}
		s.store = opened
	}
	backend := s.store
	s.active.Add(1)
	s.mu.Unlock()

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(s.active.Done) }
	return backend, release, nil
}

func (s *Service) checkOpen() error {
	if s == nil {
		return errors.New("graph query service is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("graph query service is closed")
	}
	return nil
}

// Close rejects new operations, waits for active reads, then closes the
// process-owned Neo4j driver. Repeated calls are safe.
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		backend := s.store
		s.mu.Unlock()

		s.active.Wait()
		if backend != nil {
			s.closeErr = backend.Close(ctx)
		}
	})
	return s.closeErr
}

var processService = NewService()

// DefaultService exposes the package-level process service used by legacy
// function APIs. Long-lived application components should create and own a
// dedicated service with NewService.
func DefaultService() *Service { return processService }

// CloseProcessService releases the default service used by package-level
// compatibility helpers. Application servers should own and close their own
// Service instance instead.
func CloseProcessService(ctx context.Context) error {
	return processService.Close(ctx)
}
