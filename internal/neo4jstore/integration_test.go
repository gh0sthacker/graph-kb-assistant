package neo4jstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestNeo4jOutboxIntegrationPreservesCanonicalEntityProjection(t *testing.T) {
	if os.Getenv("BB_ASSISTANT_NEO4J_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_TEST=1 to run against a local Neo4j instance")
	}
	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal("Neo4j test configuration is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := New(ctx, config)
	if err != nil {
		t.Fatal("could not connect to local Neo4j test database")
	}
	defer store.Close(context.Background())
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}

	projectDigest := sha256.Sum256([]byte(fmt.Sprintf("neo4j-integration-%d", time.Now().UnixNano())))
	projectKey := hex.EncodeToString(projectDigest[:])
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		session := store.driver.NewSession(cleanupCtx, neo4j.SessionConfig{DatabaseName: store.database})
		defer session.Close(cleanupCtx)
		_, _ = session.ExecuteWrite(cleanupCtx, func(tx neo4j.ManagedTransaction) (any, error) {
			for _, query := range []string{
				"MATCH (n:GraphRecord {project_key: $project_key}) DETACH DELETE n",
				"MATCH (n:GraphImport {project_key: $project_key}) DELETE n",
				"MATCH (n:GraphProject {project_key: $project_key}) DELETE n",
			} {
				result, runErr := tx.Run(cleanupCtx, query, map[string]any{"project_key": projectKey})
				if runErr != nil {
					return nil, runErr
				}
				if _, runErr = result.Consume(cleanupCtx); runErr != nil {
					return nil, runErr
				}
			}
			return nil, nil
		})
	})

	baseSnapshot := testSnapshot(projectKey)
	baseSource := baseSnapshot["sources"].([]behaviorgraph.Record)[0]
	observationID := "observation-" + strings.Repeat("d", 64)
	requestVariantID := "request_variant-" + strings.Repeat("e", 64)
	requestSignature := strings.Repeat("f", 64)
	entities := baseSnapshot["entities"].([]behaviorgraph.Record)
	baseSnapshot["entities"] = append(entities,
		behaviorgraph.Record{
			"id": observationID, "kind": "observation", "data": map[string]any{},
			"source_refs": []behaviorgraph.Record{baseSource},
		},
		behaviorgraph.Record{
			"id": requestVariantID, "kind": "request_variant",
			"data":        map[string]any{"request_signature": requestSignature},
			"source_refs": []behaviorgraph.Record{baseSource},
		},
	)
	assertions := baseSnapshot["assertions"].([]behaviorgraph.Record)
	baseSnapshot["assertions"] = append(assertions, behaviorgraph.Record{
		"id": "assertion-" + strings.Repeat("3", 64), "subject": observationID,
		"predicate": "uses_request_variant", "object": requestVariantID,
		"source_refs": []behaviorgraph.Record{baseSource},
	})
	if _, err := store.ReplaceProjectSnapshot(ctx, projectKey, baseSnapshot); err != nil {
		t.Fatal("could not write integration fixture snapshot")
	}
	if ids, truncated, err := store.ReadProjectObservationIDsByVariantSignatures(ctx, projectKey, requestSignature, "", 1); err != nil || truncated || len(ids) != 1 || ids[0] != observationID {
		t.Fatalf("indexed signature lookup: ids=%v truncated=%v err=%v", ids, truncated, err)
	}

	endpointID := "endpoint-" + strings.Repeat("1", 64)
	additionalSource := behaviorgraph.Record{
		"path": "/receipt-later.json", "sha256": strings.Repeat("b", 64), "record_locator": "$.later",
	}
	updatedProjection := behaviorgraph.Record{
		"id": endpointID, "kind": "endpoint", "data": map[string]any{"method": "POST"},
		"source_refs": []behaviorgraph.Record{additionalSource},
	}
	bundle := behaviorgraph.Record{
		"schema_version": json.Number("1"), "program": projectKey,
		"sources":    []behaviorgraph.Record{additionalSource},
		"entities":   []behaviorgraph.Record{updatedProjection},
		"assertions": []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{},
	}
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundleDigest := sha256.Sum256(bundleJSON)
	bundleID := "bundle-" + hex.EncodeToString(bundleDigest[:])
	if applied, err := store.ApplyOutboxBundle(ctx, projectKey, bundleID, bundleJSON); err != nil || !applied {
		t.Fatalf("first outbox apply: applied=%v err=%v", applied, err)
	}
	if applied, err := store.ApplyOutboxBundle(ctx, projectKey, bundleID, bundleJSON); err != nil || applied {
		t.Fatalf("replayed outbox apply: applied=%v err=%v", applied, err)
	}
	readBack, err := store.ReadProjectSnapshot(ctx, projectKey)
	if err != nil {
		t.Fatal("could not read the incrementally updated integration fixture")
	}
	readBackEntities := readBack["entities"].([]behaviorgraph.Record)
	var canonicalEndpoint behaviorgraph.Record
	for _, entity := range readBackEntities {
		if entity["id"] == endpointID {
			canonicalEndpoint = entity
		}
	}
	if len(readBackEntities) != 3 || canonicalEndpoint == nil || canonicalEndpoint["data"].(map[string]any)["method"] != "GET" {
		t.Fatalf("outbox update replaced the canonical entity projection: %#v", readBackEntities)
	}
	refs := canonicalEndpoint["source_refs"].([]behaviorgraph.Record)
	if len(refs) != 2 {
		t.Fatalf("outbox update did not merge provenance: got %d references", len(refs))
	}

	legacySession := store.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: store.database})
	_, err = legacySession.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, runErr := tx.Run(ctx, `
			MATCH (m:GraphProject {project_key: $project_key})
			SET m.generation = m.generation + 1
			RETURN m.generation AS generation`, map[string]any{"project_key": projectKey})
		if runErr != nil {
			return nil, runErr
		}
		if !result.Next(ctx) {
			if result.Err() != nil {
				return nil, result.Err()
			}
			return nil, errors.New("integration GraphProject marker is missing")
		}
		return result.Consume(ctx)
	})
	_ = legacySession.Close(ctx)
	if err != nil {
		t.Fatal("could not simulate an older writer advancing the graph generation")
	}

	staleSource := behaviorgraph.Record{
		"path": "/receipt-after-stale.json", "sha256": strings.Repeat("c", 64), "record_locator": "$.after_stale",
	}
	staleBundle := behaviorgraph.Record{
		"schema_version": json.Number("1"), "program": projectKey,
		"sources": []behaviorgraph.Record{staleSource},
		"entities": []behaviorgraph.Record{{
			"id": endpointID, "kind": "endpoint", "data": map[string]any{"method": "PATCH"},
			"source_refs": []behaviorgraph.Record{staleSource},
		}},
		"assertions": []behaviorgraph.Record{}, "cards": []behaviorgraph.Record{},
	}
	staleBundleJSON, err := json.Marshal(staleBundle)
	if err != nil {
		t.Fatal(err)
	}
	staleBundleDigest := sha256.Sum256(staleBundleJSON)
	if applied, err := store.ApplyOutboxBundle(ctx, projectKey, "bundle-"+hex.EncodeToString(staleBundleDigest[:]), staleBundleJSON); err != nil || !applied {
		t.Fatalf("outbox apply after legacy graph generation change: applied=%v err=%v", applied, err)
	}
	if _, _, err := store.ReadProjectObservationIDsByVariantSignatures(ctx, projectKey, strings.Repeat("a", 64), "", 1); !errors.Is(err, ErrSignatureIndexUnavailable) {
		t.Fatalf("signature lookup accepted a stale marker after incremental projection: %v", err)
	}
}
