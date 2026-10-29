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

func TestRequiredProbeConfiguration(t *testing.T) {
	for _, p := range []Probe{TargetProbe(TargetConfig{}), CaidoProbe(CaidoConfig{})} {
		got := New(p).Check(context.Background())
		if got.Verdict != Invalid {
			t.Fatalf("result = %+v", got)
		}
	}
}
