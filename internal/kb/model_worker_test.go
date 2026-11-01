package kb

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

// This smoke test exercises the actual Go ↔ Python ↔ pinned-model boundary.
// It is opt-in because it loads local model weights and is not part of fast CI.
func TestPythonRetrievalModelsSmoke(t *testing.T) {
	python := os.Getenv("BB_ASSISTANT_KB_MODEL_SMOKE_PYTHON")
	if python == "" {
		t.Skip("set BB_ASSISTANT_KB_MODEL_SMOKE_PYTHON to run the local model smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	models, err := StartPythonRetrievalModels(ctx, python)
	if err != nil {
		t.Fatal(err)
	}
	defer models.Close()

	vector, err := models.EmbedQuery(ctx, "authorization boundary")
	if err != nil {
		t.Fatal(err)
	}
	if len(vector) != 1024 {
		t.Fatalf("E5 returned %d dimensions, want 1024", len(vector))
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			t.Fatal("E5 returned a non-finite value")
		}
	}

	scores, err := models.Rerank(ctx, "authorization boundary", []string{"synthetic local reranker smoke test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || math.IsNaN(scores[0]) || math.IsInf(scores[0], 0) {
		t.Fatalf("BGE returned invalid scores: %v", scores)
	}
}
