package hunt

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"bb-graph-assistant/internal/publicdata"
	"golang.org/x/net/html"
)

func digest(s string) string       { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func node(kind, key string) string { return kind + "-" + digest(key) }

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hexPattern = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{64}|[0-9a-f]{40}|[0-9a-f]{32})\b`)
var jwtPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
var secretName = regexp.MustCompile(`(?i)password|passwd|secret|token|authorization|cookie|api.?key|session|credential|email|csrf|xsrf|jwt|bearer|access.?key|private.?key|phone|ssn|(?:^|[._-])key(?:$|[._-])|(?:^|[._-])auth(?:$|[._-])|^sid$`)
var stackPattern = regexp.MustCompile(`(?m)(?:^|\n)\s*(?:at .+|File ".+", line \d+|#\d+ .+)`)
var rules = []struct{ kind, engine, signature string }{
	{"sql", "sqlite", "SQLITE_ERROR"}, {"sql", "sql", "SQLSTATE"}, {"sql", "postgres", "syntax error at or near"}, {"sql", "oracle", "ORA-"}, {"sql", "mysql", "You have an error in your SQL syntax"},
	{"template", "template", "TemplateSyntaxError"}, {"template", "jinja2", "jinja2"}, {"template", "twig", "Twig"}, {"template", "freemarker", "freemarker"}, {"template", "velocity", "velocity"},
	{"eval", "js", "Function (<anonymous>)"}, {"eval", "js", "ReferenceError"}, {"eval", "js", "SyntaxError:"}, {"eval", "js", "Unexpected identifier"},
	{"fs", "node", "ENOENT"}, {"fs", "fs", "EACCES"}, {"fs", "fs", "No such file or directory"}, {"fs", "fs", "Permission denied"},
	{"deser", "python", "pickle"}, {"deser", "php", "unserialize"}, {"deser", "js", "__proto__"}, {"deser", "java", "ObjectInputStream"}, {"deser", "marshal", "marshal"},
}

// Extract discards raw messages. Only short nonsecret samples, value digests,
// fixed signature labels and evidence locators survive projection.
func Extract(c Capture) Observation {
	o := Observation{CaptureID: c.ID, ObservationID: c.ObservationID, RevisionID: c.RevisionID, Parameters: []Parameter{}, Sinks: []Sink{}, Infos: []Info{}}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(c.Request)))
	if err != nil {
		return o
	}
	defer req.Body.Close()
	body := messageBody(c.Request)
	body = body[:min(len(body), 1<<20)]
	host := req.Host
	port := 80
	if req.URL.Scheme == "https" || c.Scheme == "https" {
		port = 443
	}
	if h, p, e := net.SplitHostPort(host); e == nil {
		host = h
		port, _ = strconv.Atoi(p)
	}
	if c.Host != "" {
		host = c.Host
	}
	if c.Port > 0 {
		port = c.Port
	}
	route := req.URL.Path
	if route == "" {
		route = "/"
	}
	parts := strings.Split(route, "/")
	type entry struct{ name, loc, value string }
	inputs := []entry{}
	for i, s := range parts {
		if s == "" {
			continue
		}
		_, numErr := strconv.ParseInt(s, 10, 64)
		dynamic := numErr == nil || uuidPattern.MatchString(s) || hexPattern.MatchString(s) || jwtPattern.MatchString(s) || (i > 0 && secretName.MatchString(parts[i-1])) || (i > 0 && strings.EqualFold(parts[i-1], "track-order")) || strings.ContainsAny(s, "'\"<>{}()") || len(s) > 64
		if dynamic {
			name := "id"
			if i > 0 && secretName.MatchString(parts[i-1]) {
				name = "token"
			}
			if i < len(parts)-1 && name != "token" {
				name = fmt.Sprintf("path%d", i)
			}
			inputs = append(inputs, entry{name, "path", s})
			parts[i] = "{" + name + "}"
		}
	}
	route = strings.Join(parts, "/")
	o.Endpoint = Endpoint{Method: req.Method, Route: route, Host: host, Port: port, StateChanging: req.Method != "GET" && req.Method != "HEAD" && req.Method != "OPTIONS"}
	o.Endpoint.ID = node("endpoint", c.Namespace+"|"+req.Method+"|"+host+"|"+strconv.Itoa(port)+"|"+route)
	addValues := func(v url.Values, loc string) {
		for _, k := range sortedKeys(v) {
			for _, value := range v[k] {
				inputs = append(inputs, entry{k, loc, value})
			}
		}
	}
	addValues(req.URL.Query(), "query")
	if req.URL.Fragment != "" {
		if v, e := url.ParseQuery(req.URL.Fragment); e == nil {
			addValues(v, "hash")
		}
	}
	// Browser fragments are not sent over HTTP. Extract only fragments actually
	// present in the captured target; never infer them from a Referer.
	contentType := req.Header.Get("Content-Type")
	if strings.Contains(contentType, "json") || (len(body) > 0 && (body[0] == '{' || body[0] == '[')) {
		var value any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if dec.Decode(&value) == nil {
			walkJSONFields(value, "", func(k, v string) { inputs = append(inputs, entry{k, "body", v}) })
		}
	} else if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if v, e := url.ParseQuery(string(body)); e == nil {
			addValues(v, "body")
		}
	}
	for _, k := range sortedKeys(req.Header) {
		for _, v := range req.Header.Values(k) {
			inputs = append(inputs, entry{strings.ToLower(k), "header", v})
		}
	}
	responseBody := messageBody(c.Response)
	responseBody = responseBody[:min(len(responseBody), 1<<20)]
	responseType := ""
	if res, e := http.ReadResponse(bufio.NewReader(bytes.NewReader(c.Response)), nil); e == nil {
		o.Status = res.StatusCode
		responseType = res.Header.Get("Content-Type")
		if b, e := io.ReadAll(io.LimitReader(res.Body, 1<<20)); e == nil {
			responseBody = b
		}
		res.Body.Close()
		if strings.EqualFold(res.Header.Get("Content-Encoding"), "gzip") {
			if z, e := gzip.NewReader(bytes.NewReader(responseBody)); e == nil {
				if b, e := io.ReadAll(io.LimitReader(z, 1<<20)); e == nil {
					responseBody = b
				}
				z.Close()
			} else {
				responseBody = nil
			}
		}
	}
	text := string(responseBody)
	seen := map[string]bool{}
	for _, in := range inputs {
		if in.name == "" || len(in.name) > 256 {
			continue
		}
		key := in.loc + "|" + in.name + "|" + in.value
		if seen[key] {
			continue
		}
		seen[key] = true
		p := Parameter{ID: node("parameter", o.Endpoint.ID+"|"+in.loc+"|"+in.name), EndpointID: o.Endpoint.ID, Name: in.name, Location: in.loc, ObservedTypes: []string{valueType(in.value)}, Samples: []string{}, ValueDigests: []string{digest(c.Namespace + "|" + in.value)}, DistinctCount: 1}
		if safeSample(in.name, in.value) {
			p.Samples = append(p.Samples, in.value)
		}
		if !secretName.MatchString(in.name) && in.value != "" {
			p.ReflectionContext = reflection(in.value, text, responseType)
			p.Reflected = p.ReflectionContext != ""
		}
		o.Parameters = append(o.Parameters, p)
	}
	confidence := 0.6
	evidenceKind := "error_signature"
	// Escaped newlines in JSON stack traces are still a precise stack.
	if stackPattern.MatchString(strings.ReplaceAll(text, `\n`, "\n")) {
		confidence = 0.9
		evidenceKind = "stack_trace"
	}
	for _, rule := range rules {
		if strings.Contains(strings.ToLower(text), strings.ToLower(rule.signature)) {
			s := Sink{Kind: rule.kind, Engine: rule.engine, EvidenceKind: evidenceKind, Confidence: confidence, Signature: rule.signature}
			s.ID = node("sink", o.Endpoint.ID+"|"+s.Kind+"|"+s.Signature)
			o.Sinks = append(o.Sinks, s)
		}
	}
	token := req.Header.Get("Authorization")
	cookie := req.Header.Get("Cookie")
	label := "anon"
	ref := "anon"
	if token != "" || cookie != "" {
		ref = "token-ref-" + digest(c.Namespace+"|"+token+"|"+cookie)
		label = "custom#" + ref[len(ref)-8:]
	}
	o.Identity = Identity{ID: node("identity", c.Namespace+"|"+ref), Label: label, TokenRef: ref, Capabilities: []string{}}
	infoKinds := map[string]bool{}
	if hexPattern.Match(responseBody) {
		infoKinds["password_hash"] = true
	}
	if jwtPattern.Match(responseBody) {
		infoKinds["token"] = true
	}
	if strings.Contains(text, "-----BEGIN ") && strings.Contains(text, "PRIVATE KEY-----") {
		infoKinds["config"] = true
	}
	var responseJSON any
	if json.Unmarshal(responseBody, &responseJSON) == nil {
		walkJSON(responseJSON, "", func(k, v string) {
			if strings.EqualFold(k, "password") || strings.HasSuffix(strings.ToLower(k), ".password") {
				infoKinds["password"] = true
			}
		})
	}
	for _, kind := range sortedKeys(infoKinds) {
		o.Infos = append(o.Infos, Info{ID: node("info", o.Endpoint.ID+"|"+kind), Kind: kind, Where: o.Endpoint.ID, Redacted: true, Value: "[REDACTED]"})
	}
	sort.Slice(o.Parameters, func(i, j int) bool {
		if o.Parameters[i].ID == o.Parameters[j].ID {
			return o.Parameters[i].ValueDigests[0] < o.Parameters[j].ValueDigests[0]
		}
		return o.Parameters[i].ID < o.Parameters[j].ID
	})
	return o
}
func messageBody(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[i+4:]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[i+2:]
	}
	return nil
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func valueType(v string) string {
	if _, e := strconv.ParseInt(v, 10, 64); e == nil {
		return "int"
	}
	if v == "true" || v == "false" {
		return "bool"
	}
	if uuidPattern.MatchString(v) {
		return "uuid"
	}
	return "string"
}
func safeSample(name, v string) bool {
	if len(v) >= 32 || secretName.MatchString(name) || jwtPattern.MatchString(v) || hexPattern.MatchString(v) || strings.ContainsAny(v, "@\r\n") {
		return false
	}
	r, spans, e := publicdata.RedactText(v)
	return e == nil && len(spans) == 0 && r == v
}
func walkJSON(v any, prefix string, visit func(string, string)) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walkJSON(x[k], p, visit)
		}
	case []any:
		for _, item := range x {
			walkJSON(item, prefix+"[]", visit)
		}
	case string:
		visit(prefix, x)
	case json.Number:
		visit(prefix, x.String())
	case float64:
		visit(prefix, strconv.FormatFloat(x, 'f', -1, 64))
	case bool:
		visit(prefix, strconv.FormatBool(x))
	}
}

// walkJSONFields emits the JSON leaf key as the parameter name. Parent paths
// are useful for response inspection, but a request field such as user.role
// must remain the attack-surface parameter "role".
func walkJSONFields(v any, name string, visit func(string, string)) {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range sortedKeys(x) {
			walkJSONFields(x[key], key, visit)
		}
	case []any:
		for _, item := range x {
			walkJSONFields(item, name, visit)
		}
	case string:
		if name != "" {
			visit(name, x)
		}
	case json.Number:
		if name != "" {
			visit(name, x.String())
		}
	case float64:
		if name != "" {
			visit(name, strconv.FormatFloat(x, 'f', -1, 64))
		}
	case bool:
		if name != "" {
			visit(name, strconv.FormatBool(x))
		}
	}
}
func reflection(value, body, ct string) string {
	if !strings.Contains(body, value) {
		return ""
	}
	if strings.Contains(ct, "json") || json.Valid([]byte(body)) {
		found := false
		var v any
		d := json.NewDecoder(strings.NewReader(body))
		d.UseNumber()
		if d.Decode(&v) == nil {
			walkJSON(v, "", func(_, s string) {
				if strings.Contains(s, value) {
					found = true
				}
			})
		}
		if found {
			return "json"
		}
		return ""
	}
	if strings.Contains(ct, "javascript") {
		return "js"
	}
	if !strings.Contains(ct, "html") && !strings.Contains(body, "<") {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(body))
	script := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		tok := z.Token()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			for _, a := range tok.Attr {
				if strings.Contains(a.Val, value) {
					return "attr"
				}
			}
			if tok.Data == "script" {
				script = true
			}
		case html.EndTagToken:
			if tok.Data == "script" {
				script = false
			}
		case html.TextToken:
			if strings.Contains(tok.Data, value) {
				if script {
					return "js"
				}
				return "html"
			}
		}
	}
	return ""
}
