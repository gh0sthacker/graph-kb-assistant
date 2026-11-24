package hunt

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func fixture(id, target, body, ct string) Observation {
	return Extract(Capture{ID: id, ObservationID: "observation-" + id, Request: []byte("GET " + target + " HTTP/1.1\r\nHost: example.test\r\n\r\n"), Response: []byte("HTTP/1.1 200 OK\r\nContent-Type: " + ct + "\r\n\r\n" + body)})
}
func TestReflectionContexts(t *testing.T) {
	for _, tc := range []struct{ context, body, ct string }{{"html", "<p>probe</p>", "text/html"}, {"attr", "<input value='probe'>", "text/html"}, {"js", "<script>let x='probe'</script>", "text/html"}, {"json", `{"value":"probe"}`, "application/json"}} {
		t.Run(tc.context, func(t *testing.T) {
			o := fixture("1", "/search?q=probe", tc.body, tc.ct)
			g := Build([]Observation{o})
			if len(g.Result.Candidates) == 0 {
				t.Fatal("missing reflected candidate")
			}
			for _, p := range g.Parameters {
				if p.Name == "q" && (!p.Reflected || p.ReflectionContext != tc.context) {
					t.Fatalf("parameter %+v", p)
				}
			}
		})
	}
}
func TestStaticAndNonHTMLResponsesDoNotCreateXSSCandidates(t *testing.T) {
	for _, tc := range []struct{ path, contentType, body string }{
		{"/api-docs/client.js?q=probe", "text/html", "<p>probe</p>"},
		{"/api-docs/schema.json?q=probe", "text/html", "<p>probe</p>"},
		{"/api-docs/site.css?q=probe", "text/html", "<p>probe</p>"},
		{"/api-docs/site.map?q=probe", "text/html", "<p>probe</p>"},
		{"/search?q=probe", "application/javascript", "<script>probe</script>"},
	} {
		t.Run(tc.path+"|"+tc.contentType, func(t *testing.T) {
			g := Build([]Observation{fixture("static", tc.path, tc.body, tc.contentType)})
			for _, c := range g.Result.Candidates {
				if c.Class == "xss" {
					t.Fatalf("unexpected XSS candidate for %s (%s): %+v", tc.path, tc.contentType, c)
				}
			}
		})
	}
}

func TestNoEvidenceNoCandidate(t *testing.T) {
	o := fixture("", "/search?q=probe", "SQLITE_ERROR", "text/plain")
	o.ObservationID = ""
	if g := Build([]Observation{o}); len(g.Result.Candidates) != 0 || len(g.Edges) != 0 {
		t.Fatal("created unsupported evidence")
	}
}
func TestRanking(t *testing.T) {
	low := fixture("1", "/low?q=probe", "SQLITE_ERROR", "text/plain")
	high := fixture("2", "/high?q=probe", "SQLITE_ERROR\n at query (db.js:2:1)", "text/plain")
	g := Build([]Observation{low, high})
	r, err := g.Query([]string{"sqli"}, 50)
	if err != nil || len(r.Candidates) != 2 || r.Candidates[0].Sink.Confidence != 0.9 || r.Candidates[1].Sink.Confidence != 0.6 {
		t.Fatalf("result %+v, %v", r, err)
	}
	if math.Abs(r.Candidates[0].Score-0.45) > 1e-9 {
		t.Fatalf("ADR product: %v", r.Candidates[0].Score)
	}
}
func TestIdentityUnlockRole(t *testing.T) {
	anon := fixture("1", "/api/Users", "unauthorized", "text/plain")
	anon.Status = 401
	auth := Extract(Capture{ID: "2", ObservationID: "obs2", Request: []byte("POST /api/Users HTTP/1.1\r\nHost: example.test\r\nAuthorization: Bearer hidden-secret\r\nContent-Type: application/json\r\n\r\n{\"user\":{\"role\":\"admin\",\"deluxeToken\":\"hidden-token\",\"isAdmin\":true,\"lastLoginIp\":\"127.0.0.1\",\"password\":\"hidden-secret\"}}"), Response: []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{}")})
	allowed := auth
	allowed.Endpoint = anon.Endpoint
	allowed.Parameters = nil
	g := Build([]Observation{anon, auth, allowed})
	found, unlock := false, false
	parameterNames := map[string]bool{}
	for _, p := range g.Parameters {
		parameterNames[p.Name] = true
	}
	for _, c := range g.Result.Candidates {
		if c.Endpoint == "POST /api/Users" && c.Parameter == "role" && (c.Class == "bopla" || c.Class == "bfla") {
			found = true
		}
	}
	for _, e := range g.Edges {
		if e.Kind == "UNLOCKS" {
			unlock = true
			if len(e.Evidence) < 2 {
				t.Fatal("unlock needs both statuses")
			}
		}
	}
	if !found || !unlock {
		t.Fatalf("role=%v unlock=%v", found, unlock)
	}
	for _, name := range []string{"role", "deluxeToken", "isAdmin", "lastLoginIp"} {
		if !parameterNames[name] {
			t.Errorf("missing extracted parameter %q; got %v", name, parameterNames)
		}
	}
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), "hidden-secret") {
		t.Fatal("secret leaked")
	}
}
func TestAggregationAndRedaction(t *testing.T) {
	obs := []Observation{}
	for _, v := range []string{"one", "two", "three", "four", "five", "six", "six"} {
		obs = append(obs, fixture(v, "/search?q="+v, `{"password":"0123456789abcdef0123456789abcdef","token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature"}`, "application/json"))
	}
	g := Build(obs)
	for _, p := range g.Parameters {
		if p.Name == "q" && (p.DistinctCount != 6 || len(p.Samples) != 5) {
			t.Fatalf("aggregation: %+v", p)
		}
	}
	if len(g.Infos) != 3 {
		t.Fatalf("infos %+v", g.Infos)
	}
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), "0123456789abcdef0123456789abcdef") || strings.Contains(string(b), "eyJhbGci") {
		t.Fatal("info not redacted")
	}
}
func TestScoreFactors(t *testing.T) {
	if got := score(0.9, true, 1, 1.5, "tested", true, "bfla"); math.Abs(got-0.6075) > 1e-9 {
		t.Fatal(got)
	}
}
func TestDeterministicOrder(t *testing.T) {
	a := fixture("1", "/search?q=probe", "SQLITE_ERROR", "text/plain")
	b := fixture("2", "/search?q=other", "SQLITE_ERROR\n at query (db.js:1:1)", "text/plain")
	x, _ := json.Marshal(Build([]Observation{a, b}))
	y, _ := json.Marshal(Build([]Observation{b, a}))
	if string(x) != string(y) {
		t.Fatal("input ordering changed graph")
	}
}
func TestClassFitTable(t *testing.T) {
	for _, tc := range []struct{ name, class string }{{"id", "idor"}, {"uid", "bfla"}, {"user", "idor"}, {"q", "sqli"}, {"search", "nosql"}, {"filter", "xss"}, {"url", "ssrf"}, {"redirect", "open-redirect"}, {"next", "open-redirect"}, {"return", "ssrf"}, {"template", "ssti"}, {"tpl", "ssti"}, {"file", "path-traversal"}, {"path", "path-traversal"}, {"name", "path-traversal"}, {"price", "business-logic"}, {"qty", "business-logic"}, {"amount", "business-logic"}, {"coupon", "business-logic"}} {
		if ClassFit(tc.name, tc.class) != 1 {
			t.Fatalf("%s -> %s", tc.name, tc.class)
		}
	}
}
