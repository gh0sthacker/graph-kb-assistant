package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTargetProbe(t *testing.T) {
	t.Setenv("HUNTER_GATE_TARGET_STATUS", "")
	for _, tc := range []struct {
		name   string
		status int
		ctype  string
		size   int
		want   Verdict
	}{
		{"usable HTML", 200, "text/html; charset=utf-8", 1025, Valid},
		{"too short", 200, "text/html", 1024, Invalid},
		{"wrong type", 200, "application/json", 2048, Invalid},
		{"HTTP error", 503, "text/html", 2048, Invalid},
		{"redirect", 302, "text/html", 2048, Invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", tc.ctype)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, strings.Repeat("x", tc.size))
			}))
			defer server.Close()
			got := New(TargetProbe(TargetConfig{Origin: server.URL, Client: server.Client()})).Check(context.Background())
			if got.Verdict != tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestTargetProbePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, strings.Repeat("x", 1025))
	}))
	defer server.Close()
	got := New(TargetProbe(TargetConfig{Origin: server.URL + "/app"})).Check(context.Background())
	if got.Verdict != Valid {
		t.Fatalf("result = %+v", got)
	}
}

func TestTargetProbeRedirects(t *testing.T) {
	t.Setenv("HUNTER_GATE_TARGET_STATUS", "")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cross-origin server must not be contacted")
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle", http.StatusFound)
		case "/middle":
			http.Redirect(w, r, "/end", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, other.URL+"/outside", http.StatusFound)
		case "/five0", "/five1", "/five2", "/five3", "/five4":
			n := int(r.URL.Path[len(r.URL.Path)-1] - '0')
			http.Redirect(w, r, fmt.Sprintf("/five%d", n+1), http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, strings.Repeat("x", 1025))
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		path    string
		want    Verdict
		failure string
	}{
		{"/start", Valid, ""},
		{"/five0", Valid, ""},
		{"/cross", Invalid, "target redirects outside its origin: " + other.URL + "/outside"},
		{"/loop", Invalid, "target redirect limit exceeded"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := New(TargetProbe(TargetConfig{Origin: server.URL + tc.path})).Check(context.Background())
			if got.Verdict != tc.want || (tc.failure != "" && !strings.Contains(got.Failures["target"], tc.failure)) {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestTargetProbeStatusOverride(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, tc := range []struct {
		override string
		want     Verdict
	}{
		{"", Invalid}, {"404", Valid}, {"bad, 99, 600, 404, 302", Valid}, {"bad,99,600", Invalid},
	} {
		t.Run(tc.override, func(t *testing.T) {
			t.Setenv("HUNTER_GATE_TARGET_STATUS", tc.override)
			got := New(TargetProbe(TargetConfig{Origin: server.URL})).Check(context.Background())
			if got.Verdict != tc.want {
				t.Fatalf("result = %+v", got)
			}
			if tc.override != "" {
				data, err := json.Marshal(got)
				if err != nil || !strings.Contains(string(data), `"target_status_override":`) {
					t.Fatalf("missing override evidence: %s (%v)", data, err)
				}
			}
		})
	}
}

func TestTargetProbeOverrideFinalStatus(t *testing.T) {
	t.Setenv("HUNTER_GATE_TARGET_STATUS", "302,404,200")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/missing", http.StatusFound)
		case "/missing":
			http.NotFound(w, r)
		case "/final302":
			w.WriteHeader(http.StatusFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "short")
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		path string
		want Verdict
	}{{"/redirect", Valid}, {"/final302", Valid}, {"/", Invalid}} {
		got := New(TargetProbe(TargetConfig{Origin: server.URL + tc.path})).Check(context.Background())
		if got.Verdict != tc.want {
			t.Errorf("path %s: result = %+v", tc.path, got)
		}
	}
}

func TestCaidoProbe(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       Verdict
	}{
		{"projects available", `{"data":{"projects":[{"id":"one","name":"Test"}]}}`, Valid},
		{"HTTP 200 empty projects", `{"data":{"projects":[]}}`, Invalid},
		{"null projects", `{"data":{"projects":null}}`, Invalid},
		{"SPA HTML", `<html>app</html>`, Invalid},
		{"GraphQL errors", `{"data":{"projects":[{"id":"one","name":"Test"}]},"errors":[{"message":"denied"}]}`, Invalid},
		{"empty project record", `{"data":{"projects":[{}]}}`, Invalid},
		{"trailing garbage", `{"data":{"projects":[{"id":"one","name":"Test"}]}} trailing`, Invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("bad GraphQL request")
				}
				var payload struct {
					Query string `json:"query"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Query != "{projects{id name}}" {
					t.Errorf("query = %+v, %v", payload, err)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			got := New(CaidoProbe(CaidoConfig{URL: server.URL, Token: "fixture-token", Client: server.Client()})).Check(context.Background())
			if got.Verdict != tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestCaidoGraphQLErrorsPreserveDetails(t *testing.T) {
	longMessage := strings.Repeat("x", 301)
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "invalid token code",
			body: `{"errors":[{"message":"Invalid token","extensions":{"code":"INVALID_TOKEN"}}]}`,
			want: "Caido GraphQL error: INVALID_TOKEN: Invalid token",
		},
		{
			name: "nested Caido reason takes precedence",
			body: `{"data":null,"errors":[{"message":"Operation error","path":["projects"],"extensions":{"CAIDO":{"reason":"INVALID_TOKEN","code":"AUTHORIZATION"}}}]}`,
			want: "Caido GraphQL error: INVALID_TOKEN: Operation error",
		},
		{
			name: "nested Caido code fallback",
			body: `{"errors":[{"message":"Operation error","extensions":{"CAIDO":{"code":"AUTHORIZATION"}}}]}`,
			want: "Caido GraphQL error: AUTHORIZATION: Operation error",
		},
		{
			name: "message without extensions",
			body: `{"errors":[{"message":"Unauthorized"}]}`,
			want: "Caido GraphQL error: Unauthorized",
		},
		{
			name: "message truncated to 300 characters",
			body: fmt.Sprintf(`{"errors":[{"message":%q}]}`, longMessage),
			want: "Caido GraphQL error: " + longMessage[:300],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			got := New(CaidoProbe(CaidoConfig{URL: server.URL, Token: "fixture-token", Client: server.Client()})).Check(context.Background())
			failure, ok := got.Failures["caido"]
			if !ok {
				t.Fatalf("failures.caido missing: %+v", got)
			}
			if failure != tc.want {
				t.Fatalf("failures.caido = %q, want %q", failure, tc.want)
			}
		})
	}
}

func TestRequiredProbeConfiguration(t *testing.T) {
	for _, p := range []Probe{TargetProbe(TargetConfig{}), CaidoProbe(CaidoConfig{})} {
		got := New(p).Check(context.Background())
		if got.Verdict != Invalid {
			t.Fatalf("result = %+v", got)
		}
	}
}
