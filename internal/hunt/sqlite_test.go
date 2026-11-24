package hunt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hunt.sqlite3")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("read-only created missing store")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	obs := []Observation{fixture("1", "/search?q=probe", "SQLITE_ERROR", "text/plain")}
	if err = s.Project(t.Context(), obs); err != nil {
		t.Fatal(err)
	}
	if err = s.Project(t.Context(), obs); err != nil {
		t.Fatal(err)
	}
	g, err := s.Load(t.Context())
	if err != nil || g.Result.Stats.Observations != 1 || len(g.Result.Candidates) != 2 {
		t.Fatalf("roundtrip: %+v %v", g.Result, err)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 1 {
		t.Fatal("schema version", v, err)
	}
	for _, name := range []string{"endpoints", "parameters", "sinks", "identities", "infos", "edges", "candidates"} {
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM " + name).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("not private")
	}
}
func TestJSONSchema(t *testing.T) {
	g := Build([]Observation{fixture("1", "/search?q=probe", "SQLITE_ERROR", "text/plain")})
	r, e := g.Query(nil, 50)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := os.ReadFile("attack-surface.schema.json")
	if e != nil {
		t.Fatal(e)
	}
	var schema jsonschema.Schema
	if e = json.Unmarshal(encoded, &schema); e != nil {
		t.Fatal(e)
	}
	resolved, e := schema.Resolve(nil)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(r)
	var value any
	json.Unmarshal(b, &value)
	if e = resolved.Validate(value); e != nil {
		t.Fatal(e)
	}
	bad := value.(map[string]any)
	bad["candidates"].([]any)[0].(map[string]any)["why"] = []any{}
	if e = resolved.Validate(bad); e == nil {
		t.Fatal("schema accepts missing evidence")
	}
}
func TestStoreRejectsUnsupportedVersionAndSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hunt.sqlite3")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.db.Exec("PRAGMA user_version=99")
	s.Close()
	if _, e = Open(path); e == nil {
		t.Fatal("accepted unsupported schema")
	}
	link := path + ".link"
	os.Symlink(path, link)
	if _, e = OpenReadOnly(link); e == nil {
		t.Fatal("accepted symlink")
	}
}
func TestStoredObservationRedactsSecrets(t *testing.T) {
	o := Extract(Capture{ID: "1", Request: []byte("GET /search?q=hello&password=abcdef HTTP/1.1\r\nHost: example.test\r\nCookie: secret-value\r\nAuthorization: Bearer token-secret\r\n\r\n"), Response: []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"password\":\"abcdef\"}")})
	s, e := Open(filepath.Join(t.TempDir(), "hunt.sqlite3"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Project(t.Context(), []Observation{o}); e != nil {
		t.Fatal(e)
	}
	var b string
	if e = s.db.QueryRow("SELECT payload_json FROM observations").Scan(&b); e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{"abcdef", "secret-value", "token-secret"} {
		if strings.Contains(b, secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
}
