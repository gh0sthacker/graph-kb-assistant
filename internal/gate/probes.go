package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

// Client is optional; all network probes have a bounded timeout and disallow
// redirects so a login page cannot masquerade as the requested endpoint.
type TargetConfig struct {
	Origin string
	Client *http.Client
}
type CaidoConfig struct {
	URL, Token string
	Client     *http.Client
}

func probeClient(client *http.Client) *http.Client {
	copyClient := http.Client{Timeout: 10 * time.Second}
	if client != nil {
		copyClient = *client
	}
	if copyClient.Timeout <= 0 {
		copyClient.Timeout = 10 * time.Second
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copyClient
}

func endpoint(raw, path string, originOnly bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("expected an HTTP(S) URL without credentials, query or fragment")
	}
	if originOnly {
		u.Path = ""
		u.RawPath = ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	return u.String(), nil
}

func TargetProbe(config TargetConfig) Probe {
	return Probe{Name: "target", Critical: true, Check: func(ctx context.Context) error {
		target, err := endpoint(config.Origin, "/", true)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return errors.New("cannot create target request")
		}
		resp, err := probeClient(config.Client).Do(req)
		if err != nil {
			return errors.New("target request failed (network, TLS or timeout)")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("target HTTP status %d; want 200", resp.StatusCode)
		}
		ctype, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || ctype != "text/html" {
			return errors.New("target Content-Type is not text/html")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
		if err != nil {
			return errors.New("target body could not be read")
		}
		if len(body) <= 1024 {
			return fmt.Errorf("target body is %d bytes; want > 1024", len(body))
		}
		return nil
	}}
}

func CaidoProbe(config CaidoConfig) Probe {
	return Probe{Name: "caido", Critical: true, Check: func(ctx context.Context) error {
		target, err := endpoint(config.URL, "/graphql", false)
		if err != nil {
			return err
		}
		if strings.TrimSpace(config.Token) == "" {
			return errors.New("Caido token is not configured")
		}
		var result struct {
			Data struct {
				Projects []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"projects"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if err := postJSON(ctx, config.Client, target, config.Token, map[string]string{"query": "{projects{id name}}"}, &result); err != nil {
			return err
		}
		if len(result.Errors) > 0 {
			return errors.New("Caido returned GraphQL errors")
		}
		if len(result.Data.Projects) == 0 {
			return errors.New("Caido projects is empty; no usable data")
		}
		for _, p := range result.Data.Projects {
			if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
				return errors.New("Caido returned an incomplete project")
			}
		}
		return nil
	}}
}

// LLMConfig describes an OpenAI-compatible /chat/completions base URL. The
// caller selects a cheap model; no provider or model is silently assumed.
// APIKey may be empty for local unauthenticated providers.
type LLMConfig struct {
	URL, APIKey, Model string
	Client             *http.Client
}

func LLMProbe(config LLMConfig) Probe {
	return Probe{Name: "llm", Check: func(ctx context.Context) error {
		if config.URL == "" && config.Model == "" && config.APIKey == "" {
			return Skip("LLM is not configured")
		}
		if strings.TrimSpace(config.Model) == "" {
			return errors.New("LLM model is not configured")
		}
		target, err := endpoint(config.URL, "/chat/completions", false)
		if err != nil {
			return errors.New("LLM base URL is not configured or invalid")
		}
		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		payload := map[string]any{"model": config.Model, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "Reply OK"}}}
		if err := postJSON(ctx, config.Client, target, config.APIKey, payload, &result); err != nil {
			return err
		}
		if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
			return errors.New("LLM returned no completion content")
		}
		return nil
	}}
}

// ReconConfig uses defaults of 20 routes and 5 input points for zero values.
// Fingerprints are optional: when present, repeated HTML body hashes are a
// conservative SPA-shell heuristic, not proof of endpoint equivalence.
type ReconConfig struct {
	Dir                       string
	MinRoutes, MinInputPoints int
}

func ReconProbe(config ReconConfig) Probe {
	return Probe{Name: "recon", Critical: true, Check: func(ctx context.Context) error {
		if config.Dir == "" {
			return errors.New("recon directory is not configured")
		}
		n, m := config.MinRoutes, config.MinInputPoints
		if n < 0 || m < 0 {
			return errors.New("recon thresholds must be positive")
		}
		if n == 0 {
			n = 20
		}
		if m == 0 {
			m = 5
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		routes, err := readRecords(filepath.Join(config.Dir, "routes.json"), "routes")
		if err != nil {
			return fmt.Errorf("routes.json: %w", err)
		}
		points, err := readRecords(filepath.Join(config.Dir, "input-points.json"), "input_points", "input-points")
		if err != nil {
			return fmt.Errorf("input-points.json: %w", err)
		}
		if len(routes) < n {
			return fmt.Errorf("recon routes: %d < required %d", len(routes), n)
		}
		if len(points) < m {
			return fmt.Errorf("recon input-points: %d < required %d", len(points), m)
		}
		hashes := map[string]int{}
		for _, raw := range routes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(raw) > 0 && raw[0] == '"' {
				continue
			}
			var fingerprint struct {
				Hash        string `json:"body_sha256"`
				ContentType string `json:"content_type"`
			}
			if err := json.Unmarshal(raw, &fingerprint); err != nil {
				return errors.New("invalid route fingerprint fields")
			}
			if fingerprint.Hash == "" {
				continue
			}
			ctype := strings.ToLower(strings.TrimSpace(strings.SplitN(fingerprint.ContentType, ";", 2)[0]))
			if ctype == "" || ctype == "text/html" {
				hashes[fingerprint.Hash]++
			}
		}
		shells := 0
		for _, count := range hashes {
			if count > 1 {
				shells += count
			}
		}
		if shells*2 >= len(routes) {
			return fmt.Errorf("recon SPA-shell share: %d/%d >= 50%%", shells, len(routes))
		}
		return ctx.Err()
	}}
}

// readRecords accepts a record array or a named array envelope, never a summary
// count. It bounds input size and rejects null/empty/primitive records.
func readRecords(path string, keys ...string) ([]json.RawMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open recon artifact")
	}
	defer file.Close()
	const limit = 32 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, errors.New("cannot read recon artifact")
	}
	if len(data) > limit {
		return nil, errors.New("recon artifact exceeds size limit")
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, errors.New("invalid JSON envelope")
		}
		data = nil
		for _, key := range keys {
			if value, ok := envelope[key]; ok {
				if data != nil {
					return nil, errors.New("ambiguous record arrays")
				}
				data = bytes.TrimSpace(value)
			}
		}
	}
	if len(data) == 0 || data[0] != '[' {
		return nil, errors.New("expected a record array")
	}
	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, errors.New("invalid record array JSON")
	}
	for _, raw := range records {
		raw = bytes.TrimSpace(raw)
		switch {
		case len(raw) > 0 && raw[0] == '{':
			var record map[string]json.RawMessage
			if err := json.Unmarshal(raw, &record); err != nil || len(record) == 0 {
				return nil, errors.New("empty or invalid record")
			}
		case len(raw) > 0 && raw[0] == '"':
			var record string
			if err := json.Unmarshal(raw, &record); err != nil || strings.TrimSpace(record) == "" {
				return nil, errors.New("empty or invalid record")
			}
		default:
			return nil, errors.New("expected nonempty object or string records")
		}
	}
	return records, nil
}

// postJSON does not include URLs, provider response bodies or tokens in errors.
func postJSON(ctx context.Context, client *http.Client, target, token string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.New("cannot encode probe request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create probe request")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := probeClient(client).Do(req)
	if err != nil {
		return errors.New("probe request failed (network, TLS or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe HTTP status %d; want 200", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("probe response could not be read")
	}
	if len(data) > maxResponseBytes {
		return errors.New("probe response exceeds size limit")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("probe response is not valid expected JSON")
	}
	return nil
}
