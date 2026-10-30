package appquery

import (
	"bb-graph-assistant/internal/hunt"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
	"path/filepath"
	"testing"
)

func TestAttackSurfaceReadOnlyAndEvidence(t *testing.T) {
	root := t.TempDir()
	ref := project.CaidoProjectRef{InstanceID: "hunt", ProjectID: "test"}
	s, e := store.CreateProject(t.Context(), ref, root)
	if e != nil {
		t.Fatal(e)
	}
	key := s.ProjectKey
	obs, _ := (project.CaidoExchangeRef{Project: ref, RequestID: "1"}).ObservationID()
	rev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, e = s.Database.DB().Exec(`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,request_size,current_revision_id,request_vault_ref) VALUES(?,?,?,?,?,0,?,?)`, obs, ref.InstanceID, ref.ProjectID, "1", rev, rev, "unused")
	if e != nil {
		t.Fatal(e)
	}
	dir := s.Directory
	s.Close()
	cache, e := hunt.Open(filepath.Join(dir, hunt.DatabaseName))
	if e != nil {
		t.Fatal(e)
	}
	o := hunt.Extract(hunt.Capture{ID: "1", ObservationID: obs, RevisionID: rev, Request: []byte("GET /search?q=probe HTTP/1.1\r\nHost: test.invalid\r\n\r\n"), Response: []byte("HTTP/1.1 500 Error\r\n\r\nSQLITE_ERROR")})
	if e = cache.Project(t.Context(), []hunt.Observation{o}); e != nil {
		t.Fatal(e)
	}
	cache.Close()
	svc, e := NewGraphQueryService(root, []project.CaidoProjectRef{ref}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer svc.Close(t.Context())
	result, e := svc.AttackSurface(t.Context(), AttackSurfaceRequest{ProjectKey: key, Classes: []string{"sqli"}, Limit: 1})
	if e != nil || len(result.Candidates) != 1 || result.Candidates[0].Sink.Kind != "sql" {
		t.Fatalf("result %+v %v", result, e)
	}
	for _, req := range []AttackSurfaceRequest{{ProjectKey: "invalid"}, {ProjectKey: key, Limit: -1}, {ProjectKey: key, Classes: []string{"made-up"}}} {
		if _, e = svc.AttackSurface(t.Context(), req); e == nil {
			t.Fatal("accepted invalid query")
		}
	}
}
