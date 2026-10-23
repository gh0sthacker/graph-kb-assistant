// Package webui serves the local, read-only graph explorer.
package webui

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	capturesearch "bb-graph-assistant/internal/search"
)

const (
	maxGraphSeeds    = 20
	maxGraphResponse = 8 << 20
	requestTimeout   = 30 * time.Second
	graphTimeout     = 3 * time.Minute
)

var projectKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

//go:embed assets/*
var assets embed.FS

type readAPI interface {
	GraphExplorerProjects(context.Context, string, int) (map[string]any, error)
	GraphExplorerSurface(context.Context, string, *capturesearch.SurfaceCursor, int) (map[string]any, error)
	GraphExplorerSlice(context.Context, string, []string) (map[string]any, error)
}

// NewHandler constructs the local UI and its narrowly scoped read-only API.
func NewHandler(api readAPI) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("graph explorer API is unavailable")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, "route is unavailable")
			return
		}
		serveAsset("assets/index.html", "text/html; charset=utf-8")(w, r)
	})
	mux.HandleFunc("GET /assets/app.js", serveAsset("assets/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /assets/graph-focus.js", serveAsset("assets/graph-focus.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /assets/style.css", serveAsset("assets/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /api/projects", projectsHandler(api))
	mux.HandleFunc("GET /api/surface", surfaceHandler(api))
	mux.HandleFunc("GET /api/graph", graphHandler(api))
	return securityHeaders(localRequestGate(mux)), nil
}

// ValidateLoopbackAddress rejects wildcard and non-loopback listeners.
func ValidateLoopbackAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return errors.New("web UI address must include a loopback host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("web UI port must be between 1 and 65535")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("web UI can only bind to localhost or a loopback IP address")
}

func projectsHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		after := query.Get("after_project_key")
		if after != "" && !projectKeyPattern.MatchString(after) {
			writeError(w, http.StatusBadRequest, "project cursor is invalid")
			return
		}
		limit := 200
		if value := query.Get("limit"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 200 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
				return
			}
			limit = parsed
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := api.GraphExplorerProjects(ctx, after, limit)
		writeResult(w, result, err)
	}
}

func surfaceHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		projectKey := query.Get("project_key")
		if !projectKeyPattern.MatchString(projectKey) {
			writeError(w, http.StatusBadRequest, "project_key must identify one registered project")
			return
		}
		limit := 40
		if value := query.Get("limit"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 40 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 40")
				return
			}
			limit = parsed
		}
		afterHistory, hasHistory, err := optionalNonnegativeInt64(query.Get("after_history"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "surface cursor is invalid")
			return
		}
		afterLive, hasLive, err := optionalNonnegativeInt64(query.Get("after_live"))
		if err != nil || hasHistory != hasLive {
			writeError(w, http.StatusBadRequest, "surface cursor is invalid")
			return
		}
		var after *capturesearch.SurfaceCursor
		if hasHistory {
			after = &capturesearch.SurfaceCursor{HistoricalRowID: afterHistory, LiveRowID: afterLive}
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, callErr := api.GraphExplorerSurface(ctx, projectKey, after, limit)
		writeResult(w, result, callErr)
	}
}

func graphHandler(api readAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		projectKey := query.Get("project_key")
		seeds := query["seed"]
		if !projectKeyPattern.MatchString(projectKey) || len(seeds) == 0 || len(seeds) > maxGraphSeeds {
			writeError(w, http.StatusBadRequest, "select one project and between 1 and 20 graph seeds")
			return
		}
		seen := make(map[string]struct{}, len(seeds))
		for _, seed := range seeds {
			if strings.TrimSpace(seed) != seed || seed == "" || len(seed) > 512 {
				writeError(w, http.StatusBadRequest, "graph seed is malformed")
				return
			}
			if _, exists := seen[seed]; exists {
				writeError(w, http.StatusBadRequest, "graph seeds must be unique")
				return
			}
			seen[seed] = struct{}{}
		}
		ctx, cancel := context.WithTimeout(r.Context(), graphTimeout)
		defer cancel()
		result, err := api.GraphExplorerSlice(ctx, projectKey, seeds)
		writeResult(w, result, err)
	}
}

func optionalNonnegativeInt64(value string) (int64, bool, error) {
	if value == "" {
		return 0, false, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, true, errors.New("invalid nonnegative integer")
	}
	return parsed, true, nil
}

func serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data, err := assets.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusNotFound, "asset is unavailable")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func writeResult(w http.ResponseWriter, result map[string]any, err error) {
	if err != nil {
		writeError(w, http.StatusBadRequest, "local graph data could not be read; check project compatibility and coverage")
		return
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil || len(encoded) > maxGraphResponse {
		writeError(w, http.StatusRequestEntityTooLarge, "graph response exceeds the local UI limit")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(encoded))
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func localRequestGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localAuthority(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "graph explorer accepts localhost requests only")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || !strings.EqualFold(parsed.Host, r.Host) {
				writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "read-only graph explorer")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localAuthority(authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = strings.Trim(authority, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
