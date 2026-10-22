package graphaccess

import (
	"context"
	"testing"

	behaviorgraph "bb-graph-assistant/internal/graph"
)

func TestInvalidatedSharedReaderReturnsErrors(t *testing.T) {
	reader := sharedReader{entry: &cacheEntry{}}
	if _, err := reader.Slice(context.Background(), behaviorgraph.SliceOptions{}); err == nil {
		t.Fatal("invalidated shared reader returned without an error")
	}
	if _, _, err := reader.ObservationIDsByVariantSignatures("request", "response", 10); err == nil {
		t.Fatal("invalidated shared reader signature lookup returned without an error")
	}
}
