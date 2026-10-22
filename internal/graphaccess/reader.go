// Package graphaccess selects the configured read backend while presenting the
// shared validated graph Reader to CLI, MCP, and web consumers.
package graphaccess

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/store"
)

const neo4jReaderCacheTTL = time.Minute

type Reader interface {
	Slice(context.Context, behaviorgraph.SliceOptions) (map[string]any, error)
	ObservationIDsByVariantSignatures(string, string, int) ([]string, bool, error)
	Close() error
}

type cacheEntry struct {
	mu      sync.Mutex
	reader  *behaviorgraph.Reader
	expires time.Time
}

type sharedReader struct{ entry *cacheEntry }

func (r sharedReader) Slice(ctx context.Context, options behaviorgraph.SliceOptions) (map[string]any, error) {
	r.entry.mu.Lock()
	defer r.entry.mu.Unlock()
	if r.entry.reader == nil {
		return nil, errors.New("cached Neo4j graph reader was invalidated")
	}
	return r.entry.reader.Slice(ctx, options)
}

func (r sharedReader) ObservationIDsByVariantSignatures(requestSignature, responseSignature string, limit int) ([]string, bool, error) {
	r.entry.mu.Lock()
	defer r.entry.mu.Unlock()
	if r.entry.reader == nil {
		return nil, false, errors.New("cached Neo4j graph reader was invalidated")
	}
	return r.entry.reader.ObservationIDsByVariantSignatures(requestSignature, responseSignature, limit)
}

func (r sharedReader) Close() error { return nil }

var (
	neo4jCacheMu sync.Mutex
	neo4jCache   = make(map[string]*cacheEntry)
)

// Invalidate drops the cached validated reader after a Neo4j materialization.
func Invalidate(projectKey string) {
	neo4jCacheMu.Lock()
	entry := neo4jCache[projectKey]
	delete(neo4jCache, projectKey)
	neo4jCacheMu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	reader := entry.reader
	entry.reader = nil
	entry.mu.Unlock()
	if reader != nil {
		_ = reader.Close()
	}
}

// OpenReader opens the project's selected graph backend. SQLite remains the
// default during migration; setting BB_ASSISTANT_GRAPH_BACKEND=neo4j switches
// reads to the project-scoped Neo4j projection after a verified backfill.
func OpenReader(ctx context.Context, selected *store.ProjectStore) (Reader, error) {
	if selected == nil || selected.Database == nil || selected.Database.DB() == nil || !selected.Database.ReadOnly() {
		return nil, errors.New("graph reader requires a read-only selected project store")
	}
	backend := ConfiguredBackend()
	if backend == "sqlite" {
		return behaviorgraph.OpenReader(ctx, selected.BehaviorGraphPath, selected.ProjectKey, selected.BehaviorReceiptPath)
	}
	if backend != "neo4j" {
		return nil, errors.New("configured graph backend is unsupported")
	}
	neo4jCacheMu.Lock()
	defer neo4jCacheMu.Unlock()
	entry := neo4jCache[selected.ProjectKey]
	if entry != nil {
		entry.mu.Lock()
		fresh := entry.reader != nil && time.Now().Before(entry.expires)
		entry.mu.Unlock()
		if fresh {
			return sharedReader{entry: entry}, nil
		}
	}
	loaded, err := loadNeo4jReader(ctx, selected)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		entry = &cacheEntry{}
		neo4jCache[selected.ProjectKey] = entry
	}
	entry.mu.Lock()
	old := entry.reader
	entry.reader = loaded
	entry.expires = time.Now().Add(neo4jReaderCacheTTL)
	entry.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return sharedReader{entry: entry}, nil
}

func loadNeo4jReader(ctx context.Context, selected *store.ProjectStore) (*behaviorgraph.Reader, error) {
	cfg, err := neo4jstore.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	graphStore, err := neo4jstore.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	snapshot, readErr := graphStore.ReadProjectSnapshot(ctx, selected.ProjectKey)
	closeErr := graphStore.Close(context.Background())
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, errors.New("Neo4j connection could not be closed")
	}
	return behaviorgraph.OpenReaderFromSnapshot(ctx, snapshot, selected.ProjectKey, selected.BehaviorReceiptPath)
}

func ConfiguredBackend() string {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("BB_ASSISTANT_GRAPH_BACKEND")))
	if backend == "" {
		return "sqlite"
	}
	return backend
}
