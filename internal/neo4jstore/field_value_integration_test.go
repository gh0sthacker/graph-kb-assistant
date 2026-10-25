package neo4jstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	behaviorgraph "bb-graph-assistant/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// This opt-in test must target a disposable local Neo4j instance with an
// explicit ephemeral port. It intentionally rejects the usual local port.
func TestNeo4jFieldValueTraversalIntegrationOnEphemeralPort(t *testing.T) {
	if os.Getenv("BB_ASSISTANT_NEO4J_FIELD_VALUE_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_FIELD_VALUE_TEST=1 only for a disposable Neo4j instance on an ephemeral port")
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal("Neo4j field-value integration configuration is invalid")
	}
	parsed, err := url.Parse(cfg.URI)
	if err != nil || parsed.Port() == "" || parsed.Port() == "7687" {
		t.Fatal("refusing field-value integration test without an explicit non-7687 Neo4j port")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1024 || port > 65535 {
		t.Fatal("refusing field-value integration test without a valid ephemeral port")
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); !(ip != nil && ip.IsLoopback()) && host != "localhost" {
		t.Fatal("refusing field-value integration test against a non-loopback Neo4j host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	graphStore, err := New(ctx, cfg)
	if err != nil {
		t.Fatal("could not connect to the explicitly configured ephemeral Neo4j instance")
	}
	defer graphStore.Close(context.Background())
	if err := graphStore.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}

	projectA, projectB := ephemeralProjectKey(t), ephemeralProjectKey(t)
	signatureA, signatureB := strings.Repeat("0", 64), strings.Repeat("1", 64)
	snapshotA, expectedA := fieldValueTraversalFixture(projectA, signatureA)
	snapshotB, expectedB := fieldValueTraversalFixture(projectB, signatureB)
	for _, project := range []string{projectA, projectB} {
		project := project
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			session := graphStore.driver.NewSession(cleanupCtx, neo4j.SessionConfig{DatabaseName: graphStore.database})
			defer session.Close(cleanupCtx)
			_, _ = session.ExecuteWrite(cleanupCtx, func(tx neo4j.ManagedTransaction) (any, error) {
				for _, query := range []string{
					"MATCH (n:GraphRecord {project_key: $project_key}) DETACH DELETE n",
					"MATCH (n:GraphProject {project_key: $project_key}) DELETE n",
				} {
					result, runErr := tx.Run(cleanupCtx, query, map[string]any{"project_key": project})
					if runErr != nil {
						return nil, runErr
					}
					if _, runErr := result.Consume(cleanupCtx); runErr != nil {
						return nil, runErr
					}
				}
				return nil, nil
			})
		})
	}
	if _, err := graphStore.ReplaceProjectSnapshot(ctx, projectA, snapshotA); err != nil {
		t.Fatal("could not write synthetic same-project field-value graph")
	}
	if _, err := graphStore.ReplaceProjectSnapshot(ctx, projectB, snapshotB); err != nil {
		t.Fatal("could not write synthetic second-project field-value graph")
	}

	ids, truncated, generation, err := graphStore.ReadProjectFieldValueTraversalSeeds(ctx, projectA, expectedA[0], behaviorgraph.MaxFieldValueTraversalPaths)
	if err != nil || truncated || generation < 1 {
		t.Fatalf("project A traversal: ids=%v truncated=%t generation=%d err=%v", ids, truncated, generation, err)
	}
	for _, id := range expectedA {
		if !containsID(ids, id) {
			t.Errorf("project A traversal omitted %s: %v", id, ids)
		}
	}
	if containsID(ids, expectedB[1]) {
		t.Fatal("project A traversal leaked an occurrence from project B")
	}
	neighborhood, err := graphStore.ReadProjectNeighborhoodPage(ctx, projectA, ids, 100, nil)
	if err != nil || neighborhood.Generation != generation {
		t.Fatalf("project A Neo4j graph page: generation=%d err=%v", neighborhood.Generation, err)
	}
	seenPredicates := map[string]bool{}
	for _, assertion := range neighborhood.Snapshot["assertions"].([]behaviorgraph.Record) {
		seenPredicates[assertion["predicate"].(string)] = true
	}
	for _, predicate := range []string{"has_occurrence", "occurrence_of", "value_used_in"} {
		if !seenPredicates[predicate] {
			t.Errorf("Neo4j graph page omitted %q", predicate)
		}
	}
	projectBIDs, projectBTruncated, projectBGeneration, err := graphStore.ReadProjectFieldValueTraversalSeeds(ctx, projectB, expectedB[0], behaviorgraph.MaxFieldValueTraversalPaths)
	if err != nil || projectBTruncated || projectBGeneration < 1 || len(projectBIDs) != len(expectedB) {
		t.Fatalf("different-key project traversal crossed namespaces: ids=%v truncated=%t generation=%d err=%v", projectBIDs, projectBTruncated, projectBGeneration, err)
	}
	for _, expected := range expectedB {
		if !containsID(projectBIDs, expected) {
			t.Errorf("project B traversal omitted %s: %v", expected, projectBIDs)
		}
	}
	for _, projectAID := range ids {
		if containsID(projectBIDs, projectAID) {
			t.Errorf("project B traversal included project A identity %s", projectAID)
		}
	}
	for _, returned := range append(append([]string{}, ids...), projectBIDs...) {
		if strings.Contains(returned, signatureA) || strings.Contains(returned, "SYNTHETIC_RAW_VALUE_") {
			t.Fatal("Neo4j traversal returned a signature or raw query value")
		}
	}
}

func ephemeralProjectKey(t *testing.T) string {
	t.Helper()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append([]byte(t.Name()), random...))
	return hex.EncodeToString(digest[:])
}

func fieldValueTraversalFixture(projectKey, signature string) (behaviorgraph.Record, []string) {
	idFor := func(kind, label string) string {
		digest := sha256.Sum256([]byte(projectKey + "\x00" + label))
		return kind + "-" + hex.EncodeToString(digest[:])
	}
	ids := []string{
		idFor("observation", "first capture"),
		idFor("observation", "second capture"),
		idFor("field_occurrence", "first occurrence"),
		idFor("field_occurrence", "second occurrence"),
		idFor("value_reference", "shared keyed value"),
		idFor("parameter", "query identifier"),
	}
	source := behaviorgraph.Record{"path": "/synthetic/field-value.json", "sha256": strings.Repeat("9", 64), "record_locator": "$.fixture"}
	ref := []behaviorgraph.Record{source}
	entities := []behaviorgraph.Record{
		{"id": ids[0], "kind": "observation", "data": behaviorgraph.Record{}, "source_refs": ref},
		{"id": ids[1], "kind": "observation", "data": behaviorgraph.Record{}, "source_refs": ref},
		{"id": ids[2], "kind": "field_occurrence", "data": behaviorgraph.Record{"path": "query.id"}, "source_refs": ref},
		{"id": ids[3], "kind": "field_occurrence", "data": behaviorgraph.Record{"path": "query.item"}, "source_refs": ref},
		{"id": ids[4], "kind": "value_reference", "data": behaviorgraph.Record{"equality_signature": signature}, "source_refs": ref},
		{"id": ids[5], "kind": "parameter", "data": behaviorgraph.Record{"path": "query.id"}, "source_refs": ref},
	}
	assertions := []behaviorgraph.Record{
		fieldValueFixtureAssertion("1", ids[0], "has_occurrence", ids[2], ref),
		fieldValueFixtureAssertion("2", ids[1], "has_occurrence", ids[3], ref),
		fieldValueFixtureAssertion("3", ids[4], "value_used_in", ids[2], ref),
		fieldValueFixtureAssertion("4", ids[4], "value_used_in", ids[3], ref),
		fieldValueFixtureAssertion("5", ids[2], "occurrence_of", ids[5], ref),
		fieldValueFixtureAssertion("6", ids[3], "occurrence_of", ids[5], ref),
	}
	return behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": projectKey,
		"sources": []behaviorgraph.Record{source}, "entities": entities,
		"assertions": assertions, "cards": []behaviorgraph.Record{},
	}, []string{ids[0], ids[2], ids[4], ids[3], ids[1]}
}

func fieldValueFixtureAssertion(suffix, subject, predicate, object string, refs []behaviorgraph.Record) behaviorgraph.Record {
	return behaviorgraph.Record{
		"id": "assertion-" + strings.Repeat(suffix, 64), "subject": subject,
		"predicate": predicate, "object": object,
		"context":     behaviorgraph.Record{"scenario": "keyed_value_equality", "scenario_step": "correlation_only"},
		"origin_kind": "parsed", "evidence_state": "observed", "source_refs": refs,
	}
}

func containsID(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
