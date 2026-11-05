// Command kb-search-api serves the read-only local KB search endpoint as a
// standalone HTTP service. It is cross-built for KB hosts that have no Go
// toolchain and only ever opens its SQLite indexes read-only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"bb-graph-assistant/internal/hackerone"
	"bb-graph-assistant/internal/kb"
	"bb-graph-assistant/internal/kbmerge"
)

const (
	defaultListenAddr = "127.0.0.1:18766"

	addrEnvVar           = "KB_SEARCH_ADDR"
	kbIndexEnvVar        = "BB_ASSISTANT_KB_INDEX"
	hackeroneIndexEnvVar = "BB_ASSISTANT_HACKERONE_INDEX"

	maxQueryRunes      = 4096
	maxTopK            = 20
	defaultTopK        = 10
	maxSourceFamilies  = 20
	maxSourceNameBytes = 128

	searchTimeout          = 90 * time.Second
	startupSnapshotTimeout = 10 * time.Second
	shutdownTimeout        = 10 * time.Second
	readHeaderTimeout      = 5 * time.Second
	writeTimeout           = 100 * time.Second
	idleTimeout            = 60 * time.Second
)

// kbSearcher is the subset of *kb.Index used by the API. Keeping it narrow
// lets tests exercise handler error paths without a fixture database.
type kbSearcher interface {
	Search(context.Context, string, kb.SearchOptions) (kb.SearchResult, error)
}

// hackeroneSearcher is the subset of *hackerone.Index used by the API.
type hackeroneSearcher interface {
	Search(context.Context, string, int) ([]hackerone.Hit, error)
	Meta() hackerone.Meta
}

type server struct {
	kb         kbSearcher
	hackerone  hackeroneSearcher
	kbSnapshot string
	logger     *log.Logger
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "kb-search-api: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stderr io.Writer) error {
	logger := log.New(stderr, "kb-search-api: ", log.LstdFlags)

	flags := flag.NewFlagSet("kb-search-api", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", listenAddr(getenv(addrEnvVar)), "HTTP listen address (env "+addrEnvVar+")")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*addr) == "" {
		return errors.New("listen address must not be empty")
	}

	kbPath := strings.TrimSpace(getenv(kbIndexEnvVar))
	if kbPath == "" {
		return fmt.Errorf("%s must point at the local KB index", kbIndexEnvVar)
	}
	kbIndex, err := kb.OpenIndex(kbPath)
	if err != nil {
		return fmt.Errorf("open local KB index: %w", err)
	}
	defer func() {
		if err := kbIndex.Close(); err != nil {
			logger.Printf("closing KB index: %v", err)
		}
	}()

	var hackeroneIndex *hackerone.Index
	var supplemental hackeroneSearcher
	if path := strings.TrimSpace(getenv(hackeroneIndexEnvVar)); path != "" {
		index, err := hackerone.Open(path)
		if err != nil {
			// The supplemental index is optional: a broken or stale file must
			// not stop the primary KB search from serving.
			logger.Printf("HackerOne index disabled: %v", err)
		} else {
			hackeroneIndex = index
			supplemental = index
			defer func() {
				if closeErr := index.Close(); closeErr != nil {
					logger.Printf("closing HackerOne index: %v", closeErr)
				}
			}()
		}
	}

	service := newServer(kbIndex, supplemental, logger)
	service.kbSnapshot = readKBSnapshot(logger, kbIndex)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	httpServer := &http.Server{
		Handler:           service.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	hackeroneState := "disabled"
	if hackeroneIndex != nil {
		hackeroneState = fmt.Sprintf("%d reports", hackeroneIndex.Meta().ReportCount)
	}
	logger.Printf("listening on %s (kb snapshot %s, hackerone %s)", listener.Addr(), service.kbSnapshot, hackeroneState)

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func listenAddr(env string) string {
	if value := strings.TrimSpace(env); value != "" {
		return value
	}
	return defaultListenAddr
}

func newServer(kbIndex kbSearcher, hackeroneIndex hackeroneSearcher, logger *log.Logger) *server {
	return &server{kb: kbIndex, hackerone: hackeroneIndex, logger: logger}
}

// readKBSnapshot reads the active snapshot identity once at startup so the
// health endpoint does not touch the database per request. It reads the
// kb_meta row directly instead of calling Stats: the count and embedding
// queries behind Stats exceed the startup budget on the multi-gigabyte CT108
// index, which left kb_snapshot empty. A failure is logged and leaves the field
// empty; it does not disable the service.
func readKBSnapshot(logger *log.Logger, index *kb.Index) string {
	ctx, cancel := context.WithTimeout(context.Background(), startupSnapshotTimeout)
	defer cancel()
	snapshotID, err := index.SnapshotID(ctx)
	if err != nil {
		logger.Printf("KB snapshot metadata unavailable: %v", err)
		return ""
	}
	return snapshotID
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", getOnly(s.handleHealth))
	mux.HandleFunc("/api/kb/search", getOnly(s.handleSearch))
	return mux
}

func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "only GET and HEAD are supported")
			return
		}
		next(w, r)
	}
}

type healthResponse struct {
	Status           string `json:"status"`
	KB               bool   `json:"kb"`
	HackerOne        bool   `json:"hackerone"`
	KBSnapshot       string `json:"kb_snapshot"`
	HackerOneReports int64  `json:"hackerone_reports"`
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	response := healthResponse{
		Status:     "ok",
		KB:         s.kb != nil,
		HackerOne:  s.hackerone != nil,
		KBSnapshot: s.kbSnapshot,
	}
	if s.hackerone != nil {
		response.HackerOneReports = s.hackerone.Meta().ReportCount
	}
	writeJSON(w, http.StatusOK, response)
}

// kbSearchResponse flattens the KB snapshot metadata and hits alongside the
// request echo and timing so clients do not need a nested result object. The
// excerpts are reference material and must remain flagged as untrusted.
type kbSearchResponse struct {
	Query  string `json:"query"`
	TookMS int64  `json:"took_ms"`
	kb.SearchResult
	ReferenceMaterialIsUntrusted bool `json:"reference_material_is_untrusted"`
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	query := strings.TrimSpace(values.Get("query"))
	if query == "" || utf8.RuneCountInString(query) > maxQueryRunes {
		writeError(w, http.StatusBadRequest, "query must contain 1 to 4096 characters")
		return
	}
	topK := defaultTopK
	if rawTopK, present := values["top_k"]; present {
		if len(rawTopK) != 1 {
			writeError(w, http.StatusBadRequest, "top_k must be an integer between 1 and 20")
			return
		}
		parsed, err := strconv.Atoi(rawTopK[0])
		if err != nil || parsed < 1 || parsed > maxTopK {
			writeError(w, http.StatusBadRequest, "top_k must be an integer between 1 and 20")
			return
		}
		topK = parsed
	}
	rawSources := values["source"]
	if len(rawSources) > maxSourceFamilies {
		writeError(w, http.StatusBadRequest, "select no more than 20 source families")
		return
	}
	sources := make([]string, 0, len(rawSources))
	seen := make(map[string]struct{}, len(rawSources))
	for _, raw := range rawSources {
		source := strings.TrimSpace(raw)
		if source == "" || source != raw || len(source) > maxSourceNameBytes {
			writeError(w, http.StatusBadRequest, "source family names must be trimmed, non-empty, and at most 128 bytes")
			return
		}
		if _, duplicate := seen[source]; duplicate {
			continue
		}
		seen[source] = struct{}{}
		sources = append(sources, source)
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	started := time.Now()
	result, err := s.search(ctx, query, topK, sources)
	tookMS := time.Since(started).Milliseconds()
	if err != nil {
		if errors.Is(err, kb.ErrIndexUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "local KB index is unavailable")
			return
		}
		s.logger.Printf("KB search failed: %v", err)
		writeError(w, http.StatusInternalServerError, "KB search failed")
		return
	}
	if result.Hits == nil {
		result.Hits = []kb.Hit{}
	}
	writeJSON(w, http.StatusOK, kbSearchResponse{
		Query: query, TookMS: tookMS, SearchResult: result,
		ReferenceMaterialIsUntrusted: true,
	})
}

// search mirrors the MCP facade's KB search semantics: the "hackerone"
// pseudo-source selects the supplemental index, its hits are fused with the KB
// ranking through reciprocal rank fusion, and a supplemental failure never
// hides a usable KB result.
func (s *server) search(ctx context.Context, query string, topK int, sources []string) (kb.SearchResult, error) {
	includeHackerOne, kbSources := kbmerge.SplitSources(sources)
	hackeroneOnly := len(sources) > 0 && len(kbSources) == 0 && includeHackerOne

	var kbResult kb.SearchResult
	kbAvailable := false
	if !hackeroneOnly {
		if s.kb == nil {
			return kb.SearchResult{}, kb.ErrIndexUnavailable
		}
		result, err := s.kb.Search(ctx, query, kb.SearchOptions{TopK: topK, SourceNames: kbSources})
		switch {
		case err == nil:
			kbResult, kbAvailable = result, true
		case !errors.Is(err, kb.ErrIndexUnavailable):
			return kb.SearchResult{}, err
		}
	}
	if !kbAvailable {
		// A usable HackerOne index still answers a degraded query when the
		// primary KB index is absent or the caller asked for it alone.
		if includeHackerOne && s.hackerone != nil {
			hits, err := s.hackerone.Search(ctx, query, topK)
			if err == nil {
				return kbmerge.HackerOneOnly(hits, s.hackerone.Meta()), nil
			}
		}
		return kb.SearchResult{}, kb.ErrIndexUnavailable
	}
	if !includeHackerOne || s.hackerone == nil {
		return kbResult, nil
	}
	hits, err := s.hackerone.Search(ctx, query, kbmerge.CandidateLimit(topK))
	if err != nil {
		// Supplemental reference material must never hide a usable KB result.
		s.logger.Printf("HackerOne supplemental search failed: %v", err)
		return kbResult, nil
	}
	return kbmerge.Merge(kbResult, hits, s.hackerone.Meta(), topK), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":"response could not be encoded"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
