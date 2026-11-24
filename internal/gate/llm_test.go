package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLLMProbe(t *testing.T) {
	got := New(LLMProbe(LLMConfig{})).Check(context.Background())
	if got.Verdict != Valid || got.Skipped["llm"] == "" || len(got.Failures) != 0 {
		t.Fatalf("unconfigured LLM = %+v", got)
	}
	got = New(LLMProbe(LLMConfig{Model: "fixture"})).Check(context.Background())
	if got.Verdict != Degraded {
		t.Fatalf("partial config = %+v", got)
	}
	for _, tc := range []struct {
		name, body string
		status     int
		want       Verdict
	}{
		{"completion", `{"choices":[{"message":{"content":"OK"}}]}`, 200, Valid},
		{"no choices", `{"choices":[]}`, 200, Degraded},
		{"empty completion", `{"choices":[{"message":{"content":""}}]}`, 200, Degraded},
		{"provider unavailable", `{}`, 503, Degraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
					t.Errorf("bad completion request")
				}
				var payload struct {
					Model     string            `json:"model"`
					MaxTokens int               `json:"max_tokens"`
					Messages  []json.RawMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Model != "cheap-fixture" || payload.MaxTokens != 1 || len(payload.Messages) != 1 {
					t.Errorf("payload = %+v, %v", payload, err)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			got := New(LLMProbe(LLMConfig{URL: server.URL + "/v1", Model: "cheap-fixture", APIKey: "fixture-secret", Client: server.Client()})).Check(context.Background())
			if got.Verdict != tc.want || calls != 1 {
				t.Fatalf("result = %+v; calls=%d", got, calls)
			}
		})
	}
}
