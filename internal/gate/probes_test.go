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
			got := New(TargetProbe(TargetConfig{Origin: server.URL + "/ignored", Client: server.Client()})).Check(context.Background())
			if got.Verdict != tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
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
