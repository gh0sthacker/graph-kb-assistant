package hunt

import "testing"

func TestSinkSignatures(t *testing.T) {
	for _, tc := range []struct{ kind, text string }{
		{"sql", "SQLITE_ERROR"}, {"sql", "SQLSTATE"}, {"sql", "syntax error at or near"}, {"sql", "ORA-00933"}, {"sql", "You have an error in your SQL syntax"},
		{"template", "TemplateSyntaxError"}, {"template", "jinja2"}, {"template", "Twig"}, {"template", "freemarker"}, {"template", "velocity"},
		{"eval", "Function (<anonymous>)"}, {"eval", "ReferenceError"}, {"eval", "SyntaxError:"}, {"eval", "Unexpected identifier"},
		{"fs", "ENOENT"}, {"fs", "EACCES"}, {"fs", "No such file or directory"}, {"fs", "Permission denied"},
		{"deser", "pickle"}, {"deser", "unserialize"}, {"deser", "__proto__"}, {"deser", "ObjectInputStream"}, {"deser", "marshal"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			o := Extract(Capture{ID: "1", ObservationID: "obs-1", Request: []byte("GET /search?q=probe HTTP/1.1\r\nHost: example.test\r\n\r\n"), Response: []byte("HTTP/1.1 500 Error\r\nContent-Type: text/plain\r\n\r\n" + tc.text)})
			if len(o.Sinks) != 1 || o.Sinks[0].Kind != tc.kind || o.Sinks[0].Confidence != 0.6 {
				t.Fatalf("sinks: %+v", o.Sinks)
			}
		})
	}
}

func TestExtractFiltersTransportHeadersButKeepsInputHeaders(t *testing.T) {
	o := Extract(Capture{
		ID:            "headers",
		ObservationID: "obs-headers",
		Request:       []byte("GET /search?q=probe HTTP/1.1\r\nHost: example.test\r\nAccept: */*\r\nAccept-Encoding: gzip\r\nContent-Type: application/json\r\nUser-Agent: scanner\r\nContent-Length: 0\r\nCache-Control: no-cache\r\nConnection: keep-alive\r\nSec-Fetch-Site: same-origin\r\nCookie: sid=secret\r\nReferer: https://example.test/page\r\nAuthorization: Bearer token-secret\r\nX-Trace-ID: trace-value\r\nX-Input: actual-input\r\n\r\n"),
		Response:      []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok"),
	})
	got := map[string]string{}
	for _, p := range o.Parameters {
		got[p.Location+":"+p.Name] = p.Name
	}
	for _, name := range []string{"header:x-trace-id", "header:x-input", "query:q"} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing real input %q; got %v", name, got)
		}
	}
	for _, name := range []string{"host", "accept", "accept-encoding", "content-type", "user-agent", "content-length", "cache-control", "connection", "sec-fetch-site", "cookie", "referer", "authorization"} {
		if _, ok := got["header:"+name]; ok {
			t.Errorf("transport/identity header projected as parameter: %q", name)
		}
	}
	if o.Identity.TokenRef == "anon" {
		t.Fatal("Authorization should still contribute to identity")
	}
}

func TestExtractUsesNamedPathTemplateName(t *testing.T) {
	o := Extract(Capture{
		ID:            "named-path",
		ObservationID: "obs-named-path",
		Request:       []byte("GET /rest/orders/{orderId}/items HTTP/1.1\r\nHost: example.test\r\n\r\n"),
		Response:      []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok"),
	})
	if o.Endpoint.Route != "/rest/orders/{orderId}/items" {
		t.Fatalf("route template = %q", o.Endpoint.Route)
	}
	found := false
	for _, p := range o.Parameters {
		if p.Name == "orderId" && p.Location == "path" {
			found = true
		}
	}
	if !found {
		t.Fatalf("named path parameter missing: %+v", o.Parameters)
	}
}

func TestExtractPathTemplateParameterAndSinkEdge(t *testing.T) {
	o := Extract(Capture{
		ID:            "path-sink",
		ObservationID: "obs-path-sink",
		Request:       []byte("GET /rest/orders/123/items HTTP/1.1\r\nHost: example.test\r\n\r\n"),
		Response:      []byte("HTTP/1.1 500 Error\r\nContent-Type: text/plain\r\n\r\nFunction (<anonymous>)"),
	})
	if o.Endpoint.Route != "/rest/orders/{id}/items" {
		t.Fatalf("route template = %q", o.Endpoint.Route)
	}
	g := Build([]Observation{o})
	var parameterID, sinkID string
	for _, p := range g.Parameters {
		if p.Name == "id" && p.Location == "path" {
			parameterID = p.ID
		}
	}
	for _, s := range g.Sinks {
		if s.Kind == "eval" && s.Engine == "js" {
			sinkID = s.ID
		}
	}
	if parameterID == "" || sinkID == "" {
		t.Fatalf("path parameter or eval/js sink missing: parameters=%+v sinks=%+v", g.Parameters, g.Sinks)
	}
	foundCandidate, foundEdge := false, false
	for _, c := range g.Result.Candidates {
		if c.Class == "eval/js" && c.Parameter == "id" && c.Location == "path" {
			foundCandidate = true
		}
	}
	for _, e := range g.Edges {
		if e.Kind == "REACHES" && e.From == parameterID && e.To == sinkID {
			foundEdge = true
		}
	}
	if !foundCandidate || !foundEdge {
		t.Fatalf("candidate=%v REACHES edge=%v", foundCandidate, foundEdge)
	}
}
