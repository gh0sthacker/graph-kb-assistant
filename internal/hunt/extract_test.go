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
