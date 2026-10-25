package mcpserver

import (
	"context"
	"strings"
	"testing"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
)

func TestOperationBackfillRequiresRegisteredProjectBeforeOpeningStorage(t *testing.T) {
	tools := &Tools{projectsByKey: map[string]project.CaidoProjectRef{}}
	key := strings.Repeat("a", 64)
	if _, err := tools.PreviewOperationBackfill(context.Background(), key, ingest.OperationBackfillOptions{}); err == nil || err.Error() != "selected project is not registered" {
		t.Fatalf("unregistered project preview error = %v", err)
	}
	if _, err := tools.ApplyOperationBackfill(context.Background(), key, strings.Repeat("b", 64), ingest.OperationBackfillOptions{}); err == nil || err.Error() != "selected project is not registered" {
		t.Fatalf("unregistered project apply error = %v", err)
	}
}
