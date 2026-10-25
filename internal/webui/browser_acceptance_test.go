package webui

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/mcpserver"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

type browserAcceptanceCapture struct {
	RequestID          string `json:"request_id"`
	RevisionID         string `json:"revision_id"`
	GraphObservationID string `json:"graph_observation_id"`
	RequestBytes       int    `json:"request_bytes"`
	ResponseBytes      int    `json:"response_bytes"`
}

type browserAcceptanceInfo struct {
	URL          string                     `json:"url"`
	DataHome     string                     `json:"data_home"`
	VaultKey     string                     `json:"vault_key"`
	GraphBackend string                     `json:"graph_backend"`
	ProjectKey   string                     `json:"project_key"`
	Captures     []browserAcceptanceCapture `json:"captures"`
	ExpiresAt    time.Time                  `json:"expires_at"`
}

// TestBrowserAcceptanceFixture is an explicitly opted-in, temporary local
// application for browser acceptance. It serves the real embedded assets and
// HTTP/application services. Only synthetic encrypted captures are used; the
// graph backend can be SQLite or an explicitly selected disposable Neo4j.
// No Caido client, KB index, or model is configured.
func TestBrowserAcceptanceFixture(t *testing.T) {
	infoPath := os.Getenv("BB_BROWSER_ACCEPTANCE_INFO")
	if infoPath == "" {
		t.Skip("set BB_BROWSER_ACCEPTANCE_INFO to an absolute temporary JSON path to serve the synthetic browser fixture")
	}
	infoPath = validateBrowserAcceptanceInfoPath(t, infoPath)

	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "synthetic-runtime")
	graphBackend := strings.ToLower(strings.TrimSpace(os.Getenv("BB_BROWSER_ACCEPTANCE_GRAPH_BACKEND")))
	if graphBackend == "" {
		graphBackend = "sqlite"
	}
	if graphBackend != "sqlite" && graphBackend != "neo4j" {
		t.Fatalf("unsupported browser acceptance graph backend %q", graphBackend)
	}
	vaultKey := make([]byte, 32)
	if _, err := rand.Read(vaultKey); err != nil {
		t.Fatal(err)
	}
	defer clear(vaultKey)
	vaultKeyEncoded := base64.RawURLEncoding.EncodeToString(vaultKey)
	t.Setenv("BB_ASSISTANT_DATA_HOME", root)
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", graphBackend)
	t.Setenv("BB_ASSISTANT_VAULT_KEY", vaultKeyEncoded)
	t.Setenv("BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL", "0")
	// An explicit absent path prevents NewTools from resolving a real KB index.
	kbPath := filepath.Join(root, "absent-synthetic-kb.sqlite3")
	t.Setenv("BB_ASSISTANT_KB_INDEX", kbPath)

	name := "Synthetic Browser Acceptance"
	ref := project.CaidoProjectRef{InstanceID: "browser-acceptance-fixture", ProjectID: "synthetic-two-captures", DisplayName: &name}
	selected, err := store.CreateProject(ctx, ref, root)
	if err != nil {
		t.Fatalf("create synthetic project: %v", err)
	}
	t.Cleanup(func() { _ = selected.Close() })
	maintenance, err := projectlock.Acquire(ctx, selected.Directory)
	if err != nil {
		t.Fatalf("lock synthetic project: %v", err)
	}
	t.Cleanup(func() { _ = maintenance.Close() })
	pageWriter, err := ingest.OpenPageWriter(ctx, selected)
	if err != nil {
		t.Fatalf("open synthetic encrypted intake: %v", err)
	}
	defer pageWriter.Close()
	checkpoint, err := pageWriter.BeginRun(ctx)
	if err != nil {
		t.Fatalf("begin synthetic intake: %v", err)
	}
	exchanges := []caido.HttpExchange{
		browserAcceptanceExchange(ref, "browser-capture-one", "/items/alpha", "", 1, "draft"),
		browserAcceptanceExchange(ref, "browser-capture-two", "/items/beta", "/items/alpha", 2, "confirmed"),
	}
	accepted, err := pageWriter.WritePage(ctx, ingest.HistoryPageInput{
		RunID: checkpoint.RunID, Page: caido.HistoryPage{Exchanges: exchanges},
	})
	pageWriter.Close()
	if err != nil || accepted.Stored != 2 {
		t.Fatalf("write synthetic encrypted captures: stored=%d err=%v", accepted.Stored, err)
	}
	var graphWriter *behaviorgraph.Writer
	if graphBackend == "sqlite" {
		graphWriter, err = behaviorgraph.OpenWriter(ctx, selected)
		if err != nil {
			t.Fatalf("open synthetic graph writer: %v", err)
		}
	}
	captures := make([]browserAcceptanceCapture, 0, len(exchanges))
	for _, exchange := range exchanges {
		var revision string
		if err := selected.Database.DB().QueryRowContext(ctx,
			"SELECT current_revision_id FROM captures WHERE request_id=?", exchange.Ref.RequestID,
		).Scan(&revision); err != nil {
			t.Fatalf("read synthetic revision: %v", err)
		}
		job, err := behaviorgraph.ProjectionJobByRevision(ctx, selected.Database.DB(), selected.ProjectKey, revision)
		if err != nil {
			t.Fatalf("read canonical synthetic projection: %v", err)
		}
		if graphBackend == "sqlite" {
			if _, err := graphWriter.ImportBundle(ctx, job.PayloadBundle); err != nil {
				t.Fatalf("import canonical synthetic graph: %v", err)
			}
			// SQLite compatibility mode materializes the local graph directly;
			// Neo4j mode uses the durable projection worker below.
			timestamp := time.Now().UTC().Format(time.RFC3339Nano)
			tx, err := selected.Database.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO graph_capture_revisions(revision_id,graph_observation_id,updated_at) VALUES(?,?,?)",
				revision, job.GraphObservationID, timestamp,
			); err != nil {
				_ = tx.Rollback()
				t.Fatalf("store synthetic revision mapping: %v", err)
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO graph_capture_map(observation_id,revision_id,graph_observation_id,updated_at) VALUES(?,?,?,?)",
				job.ObservationID, revision, job.GraphObservationID, timestamp,
			); err != nil {
				_ = tx.Rollback()
				t.Fatalf("store synthetic current mapping: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		captures = append(captures, browserAcceptanceCapture{
			RequestID: exchange.Ref.RequestID, RevisionID: revision, GraphObservationID: job.GraphObservationID,
			RequestBytes: len(exchange.RequestRaw), ResponseBytes: len(exchange.ResponseRaw),
		})
	}
	if graphBackend == "neo4j" {
		results, err := ingest.RunProjectionWorkerBatch(ctx, selected, maintenance, ingest.ProjectionWorkerOptions{
			LeaseDuration: time.Minute, MaxAttempts: 1,
		}, len(exchanges))
		if err != nil || len(results) != len(exchanges) {
			t.Fatalf("project synthetic captures to Neo4j: applied=%d expected=%d err=%v", len(results), len(exchanges), err)
		}
		for _, result := range results {
			if result.State != "applied" {
				t.Fatalf("synthetic Neo4j projection did not apply: state=%s code=%s", result.State, result.ErrorCode)
			}
		}
	}
	projectKey := selected.ProjectKey
	if err := selected.Close(); err != nil {
		t.Fatalf("close synthetic intake store: %v", err)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatalf("release synthetic intake lock: %v", err)
	}

	tools, err := mcpserver.NewTools(root, kbPath)
	if err != nil {
		t.Fatalf("initialize actual application tools: %v", err)
	}
	t.Cleanup(func() {
		if err := tools.Close(); err != nil {
			t.Errorf("close application tools: %v", err)
		}
	})
	// Fail before publishing readiness if the real catalog, graph, or encrypted
	// evidence comparison cannot resolve the exact two synthetic revisions.
	surface, err := tools.GraphExplorerSurface(ctx, appquery.SurfaceRequest{ProjectKey: projectKey})
	if err != nil || surface.TotalObservations != 2 {
		t.Fatalf("query actual synthetic surface: observations=%d err=%v", surface.TotalObservations, err)
	}
	if _, err := tools.Graph(ctx, appquery.GraphRequest{
		ProjectKey: projectKey, View: appquery.GraphViewFull,
		SeedIDs: []string{captures[0].GraphObservationID, captures[1].GraphObservationID},
		Budget:  appquery.GraphBudget{MaxNodes: 100, MaxEdges: 300, MaxDepth: 2},
	}); err != nil {
		if queryErr, ok := err.(*appquery.QueryError); ok {
			t.Fatalf("query actual synthetic graph: %v (cause: %v)", err, queryErr.Unwrap())
		}
		t.Fatalf("query actual synthetic graph: %v", err)
	}
	comparison, err := tools.CompareCaptureMessages(ctx, appquery.CaptureComparisonRequest{
		ProjectKey: projectKey, LeftRequestID: captures[0].RequestID, LeftRevisionID: captures[0].RevisionID,
		RightRequestID: captures[1].RequestID, RightRevisionID: captures[1].RevisionID,
	})
	if err != nil || comparison.Request.FirstDifferenceOffset == nil || comparison.Response.FirstDifferenceOffset == nil {
		t.Fatalf("compare actual encrypted synthetic evidence: err=%v", err)
	}
	handler, err := NewHandler(tools)
	if err != nil {
		t.Fatalf("initialize actual web handler: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	expiresAt := time.Now().UTC().Add(30 * time.Minute)
	info := browserAcceptanceInfo{
		URL: server.URL, DataHome: root, VaultKey: vaultKeyEncoded, GraphBackend: graphBackend,
		ProjectKey: projectKey, Captures: captures, ExpiresAt: expiresAt,
	}
	encoded, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	publishBrowserAcceptanceInfo(t, infoPath, append(encoded, '\n'))
	t.Logf("synthetic browser fixture ready: info=%s expires=%s; create %s.stop to stop", infoPath, expiresAt.Format(time.RFC3339), infoPath)

	deadline := time.NewTimer(time.Until(expiresAt))
	defer deadline.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			t.Log("synthetic browser fixture reached its thirty-minute lifetime")
			return
		case <-poll.C:
			if _, err := os.Lstat(infoPath + ".stop"); err == nil {
				t.Log("synthetic browser fixture stopped by coordinator")
				return
			} else if !os.IsNotExist(err) {
				t.Fatalf("inspect synthetic stop marker: %v", err)
			}
		}
	}
}

func browserAcceptanceExchange(ref project.CaidoProjectRef, requestID, path, referer string, quantity int, state string) caido.HttpExchange {
	requestBody := fmt.Sprintf(`{"item":"shared-item","quantity":%d}`, quantity)
	responseBody := fmt.Sprintf(`{"item":"shared-item","quantity":%d,"state":%q}`, quantity, state)
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: app.fixture.invalid\r\nContent-Type: application/json\r\nAuthorization: Bearer synthetic-browser-only\r\nContent-Length: %d\r\n", path, len(requestBody))
	if referer != "" {
		request += "Referer: https://app.fixture.invalid" + referer + "\r\n"
	}
	status, truncated := 200, false
	return caido.HttpExchange{
		Ref:         caido.ExchangeRef{Project: ref, RequestID: requestID},
		RequestRaw:  []byte(request + "\r\n" + requestBody),
		ResponseRaw: []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(responseBody), responseBody)),
		HasResponse: true, Method: "POST", Scheme: "https", Host: "app.fixture.invalid", Port: 443,
		Target: "https://app.fixture.invalid" + path, ResponseStatus: &status,
		RequestTruncated: &truncated, ResponseTruncated: &truncated,
		RequestWireFidelity: "source_bytes", ResponseWireFidelity: "source_bytes",
	}
}

func validateBrowserAcceptanceInfoPath(t *testing.T, path string) string {
	t.Helper()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Ext(path) != ".json" {
		t.Fatal("BB_BROWSER_ACCEPTANCE_INFO must be an absolute canonical temporary JSON path")
	}
	temporaryRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || parent != filepath.Dir(path) {
		t.Fatal("browser fixture info parent must be an existing canonical temporary directory")
	}
	relative, err := filepath.Rel(temporaryRoot, parent)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatal("browser fixture info must be inside its own directory under TMPDIR")
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 {
		t.Fatal("browser fixture info parent must be an owner-only temporary directory")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("browser fixture info path must not already exist")
	}
	if _, err := os.Lstat(path + ".stop"); !os.IsNotExist(err) {
		t.Fatal("browser fixture stop marker must not already exist")
	}
	return path
}

func publishBrowserAcceptanceInfo(t *testing.T, path string, encoded []byte) {
	t.Helper()
	file, err := os.CreateTemp(filepath.Dir(path), ".browser-acceptance-info-*.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(file.Name()) })
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Link publishes the complete owner-only JSON atomically without replacing
	// an existing coordinator file. The temporary link is removed on cleanup.
	if err := os.Link(file.Name(), path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}
