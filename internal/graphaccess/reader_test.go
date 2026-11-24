package graphaccess

import (
	"context"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
)

func TestConfiguredBackendDefaultsToNeo4j(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "")
	if got := ConfiguredBackend(); got != "neo4j" {
		t.Fatalf("default graph backend = %q, want neo4j", got)
	}
}

func TestConfiguredBackendAllowsExplicitSQLiteCompatibility(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "sqlite")
	if got := ConfiguredBackend(); got != "sqlite" {
		t.Fatalf("explicit graph backend = %q, want sqlite", got)
	}
}

func TestInvalidatedSharedReaderReturnsErrors(t *testing.T) {
	reader := sharedReader{entry: &cacheEntry{}}
	if _, err := reader.Slice(context.Background(), behaviorgraph.SliceOptions{}); err == nil {
		t.Fatal("invalidated shared reader returned without an error")
	}
	if _, _, err := reader.ObservationIDsByVariantSignatures("request", "response", 10); err == nil {
		t.Fatal("invalidated shared reader signature lookup returned without an error")
	}
}
