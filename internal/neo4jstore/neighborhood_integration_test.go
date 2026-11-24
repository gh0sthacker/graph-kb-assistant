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

func neighborhoodTestID(kind, name string) string {
	digest := sha256.Sum256([]byte(name))
	return kind + "-" + hex.EncodeToString(digest[:])
}

func TestNeo4jNeighborhoodIntegrationContinuesWithinPinnedGeneration(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	fixture := neighborhoodFixture(project, 340, 1)
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	seed := neighborhoodTestID("asset", "host")
	first, err := store.ReadProjectNeighborhoodPage(ctx, project, []string{seed}, 160, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == nil || !first.Truncated || first.CandidateCount != 161 {
		t.Fatalf("first page was not bounded with continuation: %#v", first)
	}
	seen := make(map[string]bool)
	collect := func(page Neighborhood) {
		t.Helper()
		for _, value := range page.Snapshot["assertions"].([]behaviorgraph.Record) {
			id := value["id"].(string)
			if seen[id] {
				t.Fatalf("assertion %s appeared on more than one page", id)
			}
			seen[id] = true
		}
	}
	collect(first)
	second, err := store.ReadProjectNeighborhoodPage(ctx, project, []string{seed}, 160, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if second.NextCursor == nil || !second.Truncated || second.Generation != first.Generation {
		t.Fatalf("second page lost generation or continuation: %#v", second)
	}
	collect(second)
	third, err := store.ReadProjectNeighborhoodPage(ctx, project, []string{seed}, 160, second.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if third.NextCursor != nil || third.Truncated || third.Generation != first.Generation {
		t.Fatalf("last page unexpectedly continued or changed generation: %#v", third)
	}
	collect(third)
	if len(seen) != 340 {
		t.Fatalf("continued pages returned %d unique assertions, want 340", len(seen))
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadProjectNeighborhoodPage(ctx, project, []string{seed}, 160, first.NextCursor); !errors.Is(err, ErrNeighborhoodCursorStale) {
		t.Fatalf("cursor after graph update returned %v, want ErrNeighborhoodCursorStale", err)
	}
}

func neighborhoodFixture(project string, endpointCount, sourceCount int) behaviorgraph.Record {
	sources := make([]behaviorgraph.Record, 0, sourceCount)
	for i := 0; i < sourceCount; i++ {
		sources = append(sources, behaviorgraph.Record{
			"path": "/synthetic/neighborhood-receipt.json", "sha256": strings.Repeat("a", 64), "record_locator": fmt.Sprintf("$.records[%d]", i),
		})
	}
	host := neighborhoodTestID("asset", "host")
	entities := []behaviorgraph.Record{{"id": host, "kind": "asset", "data": map[string]any{"label": "synthetic-host"}, "source_refs": sources}}
	assertions := make([]behaviorgraph.Record, 0, endpointCount)
	for i := 0; i < endpointCount; i++ {
		endpoint := neighborhoodTestID("endpoint", fmt.Sprintf("endpoint-%d", i))
		entities = append(entities, behaviorgraph.Record{
			"id": endpoint, "kind": "endpoint", "data": map[string]any{"method": "GET"}, "source_refs": []behaviorgraph.Record{sources[0]},
		})
		assertions = append(assertions, neighborhoodFixtureAssertion(project, endpoint, host, "references", sources[0]))
	}
	return behaviorgraph.Record{
		"schema_version": json.Number("2"), "program": project,
		"entities": entities, "assertions": assertions, "sources": sources, "cards": []behaviorgraph.Record{},
	}
}

func neighborhoodFixtureAssertion(project, subject, object, predicate string, source behaviorgraph.Record) behaviorgraph.Record {
	assertion := behaviorgraph.Record{
		"subject": subject, "object": object, "predicate": predicate,
		"context": map[string]any{"program": project}, "origin_kind": "parsed", "evidence_state": "observed", "source_refs": []behaviorgraph.Record{source},
	}
	identity, _ := json.Marshal(behaviorgraph.Record{"kind": "assertion", "identity": assertion})
	digest := sha256.Sum256(identity)
	assertion["id"] = "assertion-" + hex.EncodeToString(digest[:])
	return assertion
}

func neighborhoodIntegrationStore(t *testing.T) (*Store, context.Context, string) {
	t.Helper()
	if os.Getenv("BB_ASSISTANT_NEO4J_TEST") != "1" {
		t.Skip("set BB_ASSISTANT_NEO4J_TEST=1 to run against a local Neo4j instance")
	}
	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal("Neo4j test configuration is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	store, err := New(ctx, config)
	if err != nil {
		t.Fatal("could not connect to local Neo4j test database")
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	project := neighborhoodTestID("test", fmt.Sprintf("neighborhood-integration-%s-%d", t.Name(), time.Now().UnixNano()))[5:]
	registerNeighborhoodCleanup(t, store, project)
	return store, ctx, project
}

func registerNeighborhoodCleanup(t *testing.T, store *Store, project string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		session := store.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: store.database})
		defer session.Close(ctx)
		_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			for _, query := range []string{
				"MATCH (n:GraphRecord {project_key: $project_key}) DETACH DELETE n",
				"MATCH (n:GraphImport {project_key: $project_key}) DELETE n",
				"MATCH (n:GraphProject {project_key: $project_key}) DELETE n",
			} {
				result, err := tx.Run(ctx, query, map[string]any{"project_key": project})
				if err != nil {
					return nil, err
				}
				if _, err := result.Consume(ctx); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Error("synthetic neighborhood namespace cleanup failed")
		}
	})
}

func neighborhoodMutation(t *testing.T, ctx context.Context, store *Store, project, query string, params map[string]any) {
	t.Helper()
	if params == nil {
		params = make(map[string]any)
	}
	params["project_key"] = project
	session := store.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: store.database})
	defer session.Close(ctx)
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
	if err != nil {
		t.Fatal("synthetic fixture mutation failed")
	}
}

func TestNeo4jNeighborhoodIntegrationOneHopIsolationAndDegreeBudget(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	fixture := neighborhoodFixture(project, 340, 1)
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	seed := neighborhoodTestID("endpoint", "endpoint-0")
	neighborhood, err := store.ReadProjectNeighborhood(ctx, project, []string{seed}, 80)
	if err != nil {
		t.Fatal(err)
	}
	if neighborhood.Truncated || neighborhood.CandidateCount != 1 || neighborhood.Generation < 1 || len(neighborhood.Snapshot["assertions"].([]behaviorgraph.Record)) != 1 || len(neighborhood.Snapshot["entities"].([]behaviorgraph.Record)) != 2 {
		t.Fatal("endpoint neighborhood expanded through its shared host")
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, neighborhood.Snapshot, project, t.TempDir())
	if err != nil {
		t.Fatalf("bounded neighborhood failed shared validation: %v", err)
	}
	_ = reader.Close()
	neighborhood, err = store.ReadProjectNeighborhood(ctx, project, []string{neighborhoodTestID("asset", "host")}, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !neighborhood.Truncated || neighborhood.CandidateCount != 81 || len(neighborhood.Snapshot["assertions"].([]behaviorgraph.Record)) != 80 || len(neighborhood.Snapshot["entities"].([]behaviorgraph.Record)) != 81 {
		t.Fatal("high-degree neighborhood did not honor limit+1 edge selection")
	}
	other := neighborhoodTestID("test", "other-"+project)[5:]
	registerNeighborhoodCleanup(t, store, other)
	otherFixture := neighborhoodFixture(other, 340, 1)
	otherFixture["entities"].([]behaviorgraph.Record)[1]["data"].(map[string]any)["method"] = "POST"
	if _, err := store.ReplaceProjectSnapshot(ctx, other, otherFixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ project, method string }{{project, "GET"}, {other, "POST"}} {
		selected, err := store.ReadProjectNeighborhood(ctx, test.project, []string{seed}, 80)
		if err != nil {
			t.Fatal(err)
		}
		for _, entity := range selected.Snapshot["entities"].([]behaviorgraph.Record) {
			if entity["id"] == seed && entity["data"].(map[string]any)["method"] != test.method {
				t.Fatal("same-ID records leaked across project namespaces")
			}
		}
	}
}

func TestNeo4jNeighborhoodIntegrationSamplesEntityProvenanceAndClosesEvidence(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	fixture := neighborhoodFixture(project, 2, 1_100)
	assertions := fixture["assertions"].([]behaviorgraph.Record)
	source := fixture["sources"].([]behaviorgraph.Record)[0]
	evidenceSeed := neighborhoodTestID("asset", "evidence-seed")
	entities := fixture["entities"].([]behaviorgraph.Record)
	entities = append(entities, behaviorgraph.Record{
		"id": evidenceSeed, "kind": "asset", "data": map[string]any{"label": "evidence-seed"}, "source_refs": []behaviorgraph.Record{source},
	})
	fixture["entities"] = entities
	evidence := neighborhoodFixtureAssertion(project, evidenceSeed, assertions[0]["id"].(string), "supports", source)
	fixture["assertions"] = append(assertions, evidence)
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	neighborhood, err := store.ReadProjectNeighborhood(ctx, project, []string{evidenceSeed}, 80)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Snapshot["assertions"].([]behaviorgraph.Record)) != 2 || len(neighborhood.Snapshot["sources"].([]behaviorgraph.Record)) > maxNeighborhoodEntityRefs+1 || !neighborhood.ProvenanceTruncated {
		t.Fatal("evidence dependencies were omitted or large entity provenance was not marked as sampled")
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, neighborhood.Snapshot, project, t.TempDir())
	if err != nil {
		t.Fatalf("closed evidence neighborhood failed shared validation: %v", err)
	}
	_ = reader.Close()
	if _, err := store.ReadProjectNeighborhood(ctx, project, []string{evidenceSeed}, 1); err == nil {
		t.Fatal("dependency closure bypassed the requested edge budget")
	}
}

func TestNeo4jNeighborhoodIntegrationBoundsCommonEntityProvenanceFanout(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	// All three entities share >10k sources: >33k EVIDENCED_BY links, but
	// selecting the host still has just two incident assertions and three nodes.
	fixture := neighborhoodFixture(project, 2, 11_001)
	sources := fixture["sources"].([]behaviorgraph.Record)
	for _, entity := range fixture["entities"].([]behaviorgraph.Record) {
		entity["source_refs"] = sources
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
		t.Fatal(err)
	}
	neighborhood, err := store.ReadProjectNeighborhood(ctx, project, []string{neighborhoodTestID("asset", "host")}, 80)
	if err != nil {
		t.Fatalf("small neighborhood failed because common entities have high provenance degree: %v", err)
	}
	if neighborhood.Truncated || !neighborhood.ProvenanceTruncated || len(neighborhood.TruncatedEntityIDs) != 3 || neighborhood.EntitySourceRefLimit != maxNeighborhoodEntityRefs {
		t.Fatal("entity provenance truncation was hidden or confused with candidate-edge truncation")
	}
	if len(neighborhood.Snapshot["entities"].([]behaviorgraph.Record)) != 3 || len(neighborhood.Snapshot["assertions"].([]behaviorgraph.Record)) != 2 {
		t.Fatal("small graph topology changed while bounding provenance")
	}
	for _, entity := range neighborhood.Snapshot["entities"].([]behaviorgraph.Record) {
		refs := entity["source_refs"].([]behaviorgraph.Record)
		if len(refs) == 0 || len(refs) > maxNeighborhoodEntityRefs {
			t.Fatal("entity provenance exceeds the sample budget")
		}
	}
	for _, assertion := range neighborhood.Snapshot["assertions"].([]behaviorgraph.Record) {
		refs := assertion["source_refs"].([]behaviorgraph.Record)
		if len(refs) != 1 || recordSortKey(refs[0]) != recordSortKey(sources[0]) {
			t.Fatal("mandatory assertion provenance changed")
		}
	}
	reader, err := behaviorgraph.OpenReaderFromSnapshot(ctx, neighborhood.Snapshot, project, t.TempDir())
	if err != nil {
		t.Fatalf("sampled entity provenance failed shared identity/provenance validation: %v", err)
	}
	_ = reader.Close()
	repeated, err := store.ReadProjectNeighborhood(ctx, project, []string{neighborhoodTestID("asset", "host")}, 80)
	if err != nil {
		t.Fatal(err)
	}
	firstPayload, _ := json.Marshal(neighborhood)
	repeatedPayload, _ := json.Marshal(repeated)
	if string(firstPayload) != string(repeatedPayload) {
		t.Fatal("entity provenance sampling is not deterministic")
	}
}

func TestNeo4jNeighborhoodIntegrationRejectsCorruptLocalRecordsAndLinks(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	fixture := neighborhoodFixture(project, 2, 1)
	seed := neighborhoodTestID("endpoint", "endpoint-0")
	assertion := fixture["assertions"].([]behaviorgraph.Record)[0]["id"].(string)
	host := neighborhoodTestID("asset", "host")
	other := neighborhoodTestID("test", "foreign-"+project)[5:]
	registerNeighborhoodCleanup(t, store, other)
	if _, err := store.ReplaceProjectSnapshot(ctx, other, neighborhoodFixture(other, 1, 1)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, query string
		params      map[string]any
	}{
		{"record checksum", `MATCH (n:GraphRecord {project_key:$project_key, record_id:$seed}) SET n.payload_sha256 = $bad`, map[string]any{"seed": seed, "bad": strings.Repeat("0", 64)}},
		{"source checksum", `MATCH (n:GraphRecord {project_key:$project_key, record_type:'source'}) SET n.payload_sha256 = $bad`, map[string]any{"bad": strings.Repeat("0", 64)}},
		{"source identity", `MATCH (n:GraphRecord {project_key:$project_key, record_type:'source'}) SET n.record_id = $bad`, map[string]any{"bad": "source-" + strings.Repeat("0", 64)}},
		{"missing provenance", `MATCH (n:GraphRecord {project_key:$project_key, record_id:$seed})-[r:EVIDENCED_BY {project_key:$project_key}]->(s:GraphRecord {project_key:$project_key}) DELETE r`, map[string]any{"seed": seed}},
		{"missing topology", `MATCH (a:GraphRecord {project_key:$project_key, record_id:$assertion})-[r:OBJECT {project_key:$project_key}]->(n:GraphRecord {project_key:$project_key}) DELETE r`, map[string]any{"assertion": assertion}},
		{"wrong topology", `MATCH (a:GraphRecord {project_key:$project_key, record_id:$assertion})-[r:OBJECT {project_key:$project_key}]->(n:GraphRecord {project_key:$project_key}) MATCH (wrong:GraphRecord {project_key:$project_key, record_id:$seed}) DELETE r CREATE (a)-[:OBJECT {project_key:$project_key}]->(wrong)`, map[string]any{"assertion": assertion, "seed": seed}},
		{"duplicate topology", `MATCH (a:GraphRecord {project_key:$project_key, record_id:$assertion}), (n:GraphRecord {project_key:$project_key, record_id:$host}) CREATE (a)-[:OBJECT {project_key:$project_key}]->(n)`, map[string]any{"assertion": assertion, "host": host}},
		{"unscoped topology", `MATCH (a:GraphRecord {project_key:$project_key, record_id:$assertion}), (n:GraphRecord {project_key:$project_key, record_id:$host}) CREATE (a)-[:OBJECT]->(n)`, map[string]any{"assertion": assertion, "host": host}},
		{"foreign topology", `MATCH (a:GraphRecord {project_key:$project_key, record_id:$assertion}), (n:GraphRecord {project_key:$other, record_id:$host}) CREATE (a)-[:OBJECT {project_key:$project_key}]->(n)`, map[string]any{"assertion": assertion, "host": host, "other": other}},
		{"foreign provenance", `MATCH (a:GraphRecord {project_key:$project_key, record_id:$seed}), (n:GraphRecord {project_key:$other, record_type:'source'}) CREATE (a)-[:EVIDENCED_BY {project_key:$project_key}]->(n)`, map[string]any{"seed": seed, "other": other}},
		{"schema", `MATCH (m:GraphProject {project_key:$project_key}) SET m.schema_version=1`, nil},
		{"generation", `MATCH (m:GraphProject {project_key:$project_key}) SET m.generation=0`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.ReplaceProjectSnapshot(ctx, project, fixture); err != nil {
				t.Fatal(err)
			}
			neighborhoodMutation(t, ctx, store, project, test.query, test.params)
			if _, err := store.ReadProjectNeighborhood(ctx, project, []string{seed}, 80); err == nil {
				t.Fatal("accepted a corrupt synthetic neighborhood")
			}
		})
	}
}

type neighborhoodChangingGenerationTx struct {
	neo4j.ManagedTransaction
	metadataReads int
}

func (tx *neighborhoodChangingGenerationTx) Run(ctx context.Context, query string, params map[string]any) (neo4j.Result, error) {
	if strings.Contains(query, "m.schema_version AS schema_version, m.generation AS generation") {
		tx.metadataReads++
		if tx.metadataReads == 2 {
			query = strings.Replace(query, "m.generation AS generation", "m.generation + 1 AS generation", 1)
		}
	}
	return tx.ManagedTransaction.Run(ctx, query, params)
}

func TestNeo4jNeighborhoodIntegrationRejectsGenerationChange(t *testing.T) {
	store, ctx, project := neighborhoodIntegrationStore(t)
	if _, err := store.ReplaceProjectSnapshot(ctx, project, neighborhoodFixture(project, 1, 1)); err != nil {
		t.Fatal(err)
	}
	session := store.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: store.database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	_, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return readProjectNeighborhoodTx(ctx, &neighborhoodChangingGenerationTx{ManagedTransaction: tx}, project, []string{neighborhoodTestID("endpoint", "endpoint-0")}, 80, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatal("a changed project generation was accepted")
	}
}
