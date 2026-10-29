package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/gate"
)

func TestGateCommandVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name              string
		routes            int
		projects, llmFail bool
		want              gate.Verdict
		code              int
	}{
		{"all OK", 20, true, false, gate.Valid, 0},
		{"Caido 200 empty", 20, false, false, gate.Invalid, 3},
		{"only 4 routes", 4, true, false, gate.Invalid, 3},
		{"optional LLM failed", 20, true, true, gate.Degraded, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAIDO_ACCESS_TOKEN", "fixture-token")
			for _, key := range []string{"BB_ASSISTANT_LLM_URL", "BB_ASSISTANT_LLM_MODEL", "BB_ASSISTANT_LLM_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_API_KEY"} {
				t.Setenv(key, "")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, strings.Repeat("x", 2048))
				case "/graphql":
					if tc.projects {
						fmt.Fprint(w, `{"data":{"projects":[{"id":"one","name":"Fixture"}]}}`)
					} else {
						fmt.Fprint(w, `{"data":{"projects":[]}}`)
					}
				case "/v1/chat/completions":
					http.Error(w, "provider unavailable", 503)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			routes := make([]string, tc.routes)
			for i := range routes {
				routes[i] = fmt.Sprintf("/route/%d", i)
			}
			for name, data := range map[string]any{"routes.json": routes, "input-points.json": []string{"a", "b", "c", "d", "e"}} {
				body, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--target", server.URL, "--caido-url", server.URL, "--recon-dir", dir, "--json"}
			if tc.llmFail {
				args = append(args, "--llm-url", server.URL+"/v1", "--llm-model", "fixture")
			}
			var out bytes.Buffer
			err := runGate(context.Background(), args, &out)
			if code := commandExitCode(err); code != tc.code {
				t.Fatalf("exit=%d want=%d err=%v", code, tc.code, err)
			}
			var result gate.Result
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("JSON=%q err=%v", out.String(), err)
			}
			if result.Verdict != tc.want || result.CheckedAt.IsZero() {
				t.Fatalf("result = %+v", result)
			}
			if !tc.llmFail && result.Skipped["llm"] == "" {
				t.Fatalf("missing skipped reason: %+v", result)
			}
			t.Logf("exit=%d JSON=%s", tc.code, strings.TrimSpace(out.String()))
		})
	}
}

func TestGateCommandHumanAndInvalidConfiguration(t *testing.T) {
	var out bytes.Buffer
	err := runGate(context.Background(), nil, &out)
	if commandExitCode(err) != 3 || !strings.Contains(out.String(), "gate: invalid") || !strings.Contains(out.String(), "target:") || !strings.Contains(out.String(), "caido:") || !strings.Contains(out.String(), "recon:") {
		t.Fatalf("err=%v output=%q", err, out.String())
	}
	for _, args := range [][]string{{"unexpected"}, {"--timeout", "0"}, {"--min-routes", "-1"}, {"--min-input-points", "0"}} {
		out.Reset()
		err := runGate(context.Background(), args, &out)
		if err == nil {
			t.Fatalf("accepted invalid flags: %v", args)
		}
	}
}
