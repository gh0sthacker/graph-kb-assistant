package neo4jstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestDeleteProjectPurgesOnlySelectedNamespace(t *testing.T) {
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

	key := func(label string) string {
		digest := sha256.Sum256([]byte(fmt.Sprintf("neo4j-delete-%s-%d", label, time.Now().UnixNano())))
		return hex.EncodeToString(digest[:])
	}
	removedKey, retainedKey := key("removed"), key("retained")
	cleanup := func(projectKey string) {
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
	}
	t.Cleanup(func() { cleanup(removedKey); cleanup(retainedKey) })
	if _, err := store.ReplaceProjectSnapshot(ctx, removedKey, testSnapshot(removedKey)); err != nil {
		t.Fatal("could not create removal fixture")
	}
	if _, err := store.ReplaceProjectSnapshot(ctx, retainedKey, testSnapshot(retainedKey)); err != nil {
		t.Fatal("could not create retained-project fixture")
	}
	if err := store.DeleteProject(ctx, removedKey); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	session := store.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: store.database})
	defer session.Close(ctx)
	for _, check := range []struct {
		projectKey string
		wantZero   bool
	}{{removedKey, true}, {retainedKey, false}} {
		result, err := session.Run(ctx, `
			MATCH (n:GraphRecord {project_key: $project_key})
			RETURN count(n) AS records`, map[string]any{"project_key": check.projectKey})
		if err != nil || !result.Next(ctx) {
			t.Fatalf("count project records: err=%v", err)
		}
		count, valueOK := result.Record().Get("records")
		if !valueOK {
			t.Fatal("record count was not returned")
		}
		if _, err := result.Consume(ctx); err != nil {
			t.Fatal(err)
		}
		actual, ok := count.(int64)
		if !ok || (actual == 0) != check.wantZero {
			t.Fatalf("project %s records=%v, wantZero=%v", check.projectKey, count, check.wantZero)
		}
	}
}
