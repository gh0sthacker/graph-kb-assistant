package mcpserver

import (
	"encoding/json"
	"testing"
 "path/filepath"
 "bb-graph-assistant/internal/appquery"
 "bb-graph-assistant/internal/project"
 "bb-graph-assistant/internal/store"
 "bb-graph-assistant/internal/hunt"
)

func TestAttackSurfaceMCPCall(t *testing.T) {
 root:=t.TempDir();ref:=project.CaidoProjectRef{InstanceID:"hunt-mcp",ProjectID:"p"};s,e:=store.CreateProject(t.Context(),ref,root);if e!=nil{t.Fatal(e)};key:=s.ProjectKey;dir:=s.Directory
 obs,_:=(project.CaidoExchangeRef{Project:ref,RequestID:"1"}).ObservationID();rev:="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
 _,e=s.Database.DB().Exec(`INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,request_sha256,request_size,current_revision_id,request_vault_ref) VALUES(?,?,?,?,?,0,?,?)`,obs,ref.InstanceID,ref.ProjectID,"1",rev,rev,"unused");if e!=nil{t.Fatal(e)};s.Close()
 cache,e:=hunt.Open(filepath.Join(dir,hunt.DatabaseName));if e!=nil{t.Fatal(e)}
 o:=hunt.Extract(hunt.Capture{ID:"1",ObservationID:obs,RevisionID:rev,Request:[]byte("GET /rest/products/search?q=probe HTTP/1.1\r\nHost: test.invalid\r\n\r\n"),Response:[]byte("HTTP/1.1 500 Error\r\n\r\nSQLITE_ERROR")})
 if e=cache.Project(t.Context(),[]hunt.Observation{o});e!=nil{t.Fatal(e)};cache.Close()
 svc,e:=appquery.NewGraphQueryService(root,[]project.CaidoProjectRef{ref},nil);if e!=nil{t.Fatal(e)};defer svc.Close(t.Context());tools:=&Tools{root:root,graphQueries:svc};server,e:=NewServer(tools);if e!=nil{t.Fatal(e)}
 args,_:=json.Marshal(map[string]any{"name":"bb_attack_surface","arguments":map[string]any{"project_key":key,"classes":[]string{"sqli"},"limit":1}})
 response,e:=server.callTool(t.Context(),args);if e!=nil||response["isError"]==true{t.Fatalf("%+v %v",response,e)}
 b,_:=json.Marshal(response["structuredContent"]);var result hunt.Result;if e=json.Unmarshal(b,&result);e!=nil||len(result.Candidates)!=1||result.Candidates[0].Sink.Kind!="sql"{t.Fatalf("%s %v",b,e)}
}
func TestAttackSurfaceMCPCatalog(t *testing.T) {
	tools := &Tools{}
	found := false
	for _, s := range tools.catalog() {
		if s.Name == "bb_attack_surface" {
			found = true
			if s.Annotations["readOnlyHint"] != true || s.InputSchema["additionalProperties"] != false {
				t.Fatal("unsafe contract")
			}
		}
	}
	if !found {
		t.Fatal("bb_attack_surface missing")
	}
	for _, a := range []string{`{"project_key":"bad"}`, `{"project_key":"bad","extra":true}`, `{"project_key":"bad","classes":"sqli"}`} {
		if _, e := tools.call(t.Context(), "bb_attack_surface", json.RawMessage(a)); e == nil {
			t.Fatal("invalid args accepted")
		}
	}
}
