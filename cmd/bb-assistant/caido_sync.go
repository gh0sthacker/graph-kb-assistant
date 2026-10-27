package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

func syncCaidoProjects(args []string) error {
	flags := flag.NewFlagSet("caido-sync", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var scopeOrigins scopeOriginList
	for _, origin := range strings.FieldsFunc(os.Getenv("BB_ASSISTANT_CAIDO_SCOPE_ORIGINS"), func(r rune) bool { return r == ',' || r == '\n' }) {
		if origin = strings.TrimSpace(origin); origin != "" {
			scopeOrigins = append(scopeOrigins, origin)
		}
	}
	flags.Var(&scopeOrigins, "scope-origin", "exact authorized HTTP origin; repeat for each scheme://host:port")
	root := flags.String("data-root", "", "private assistant data root")
	instanceID := flags.String("instance-id", os.Getenv("BB_ASSISTANT_CAIDO_INSTANCE_ID"), "stable operator-defined Caido instance key")
	mcpCommand := flags.String("mcp-command", "", "pinned Caido MCP v4.3.0 executable")
	mcpSHA256 := flags.String("mcp-sha256", "", "expected SHA-256 of the Caido MCP executable")
	tokenFile := flags.String("token-file", "", "private Caido MCP token JSON file")
	pollInterval := flags.Duration("poll-interval", 30*time.Second, "project inventory and request tail interval (1s-10m)")
	missingConfirmations := flags.Int("missing-confirmations", 3, "successful project inventories required before reporting absence (2-20)")
	backfillPages := flags.Int("backfill-pages", 5, "historical pages per project per inventory cycle (1-100)")
	backfillPageSize := flags.Int("backfill-page-size", 50, "historical requests per page (1-100)")
	backfillBodyLimit := flags.Int("backfill-body-limit", 64<<10, "maximum historical request/response bytes per exchange")
	backfillDelay := flags.Duration("backfill-delay", 250*time.Millisecond, "delay between persisted historical pages")
	tailPages := flags.Int("tail-pages", 1, "live-tail pages per project per inventory cycle (1-100)")
	pageSize := flags.Int("page-size", 50, "live-tail requests per page (1-100)")
	bodyLimit := flags.Int("body-limit", 256<<10, "maximum live-tail request/response bytes per exchange")
	once := flags.Bool("once", false, "run one inventory and bounded per-project sync cycle, then exit with an error if any project failed")
	scopeScheme := flags.String("scope-scheme", os.Getenv("BB_ASSISTANT_CAIDO_SCOPE_SCHEME"), "optional exact HTTP origin scope scheme")
	scopeHost := flags.String("scope-host", os.Getenv("BB_ASSISTANT_CAIDO_SCOPE_HOST"), "optional exact HTTP origin scope host")
	scopePortDefault, err := liveTailScopePortDefault()
	if err != nil {
		return err
	}
	scopePort := flags.Int("scope-port", scopePortDefault, "optional exact HTTP origin scope port")
	requireScope := flags.Bool("require-scope", false, "fail unless one or more exact HTTP origin scopes are configured")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *instanceID == "" || *pollInterval < time.Second || *pollInterval > 10*time.Minute ||
		*missingConfirmations < 2 || *missingConfirmations > 20 ||
		*backfillPages < 1 || *backfillPages > 100 || *backfillPageSize < 1 || *backfillPageSize > 100 ||
		*backfillBodyLimit < 64<<10 || *backfillBodyLimit > 16<<20 ||
		*backfillDelay < 0 || *backfillDelay > time.Minute || *tailPages < 1 || *tailPages > 100 ||
		*pageSize < 1 || *pageSize > 100 || *bodyLimit < 64<<10 || *bodyLimit > 16<<20 ||
		!ingest.LivePageBudgetFits(*pageSize, *bodyLimit) {
		return errors.New("Caido sync options are invalid")
	}
	if graphaccess.ConfiguredBackend() != "neo4j" {
		return errors.New("continuous Caido sync requires the Neo4j graph backend")
	}
	scopes, err := normalizeCaidoScopeOrigins(scopeOrigins, *scopeScheme, *scopeHost, *scopePort, *requireScope)
	if err != nil {
		return err
	}
	if len(scopes) == 0 {
		return errors.New("Caido sync requires at least one exact authorized origin scope")
	}
	if *backfillPageSize < 1 || int64(*backfillPageSize)*int64(*backfillBodyLimit)*2 > 64<<20 {
		return errors.New("Caido historical import limits are invalid")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := caido.StartMCPProcess(caido.MCPProcessConfig{
		Context: ctx, Command: *mcpCommand, ExpectedSHA256: *mcpSHA256, TokenFile: *tokenFile,
		StartupTimeout: 30 * time.Second, CallTimeout: 180 * time.Second,
	})
	if err != nil {
		return err
	}
	defer client.Close()
	source, err := caido.NewMCPReadSource(client, *instanceID)
	if err != nil {
		return err
	}
	runners := make(map[string]*ingest.ProjectionWorkerRunner)
	maxCycles := 0
	if *once {
		maxCycles = 1
	}
	return ingest.RunProjectInventorySync(ctx, source, ingest.ProjectInventorySyncOptions{
		DataRoot: *root, InstanceID: *instanceID, PollInterval: *pollInterval,
		MissingConfirmations: *missingConfirmations, MaxCycles: maxCycles,
		OnProject: func(ctx context.Context, ref project.CaidoProjectRef) error {
			receipt, err := processCaidoProject(ctx, source, ref, *root, scopes, *backfillPageSize, *backfillBodyLimit, *backfillPages, *backfillDelay,
				*pageSize, *tailPages, *bodyLimit, runners)
			if err != nil {
				_ = json.NewEncoder(os.Stderr).Encode(receipt)
				return err
			}
			_ = json.NewEncoder(os.Stderr).Encode(receipt)
			return nil
		},
		OnProjectAbsent: func(_ context.Context, ref project.CaidoProjectRef) error {
			key, _ := ref.StorageKey()
			// A project removed from Caido is retained locally as historical
			// evidence. Projection workers stay alive to drain accepted jobs;
			// explicit project purge remains a separate operator action.
			_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "project_absent_from_caido", "project_key": key, "retained_as_history": true})
			return nil
		},
		OnError: func(event string, ref *project.CaidoProjectRef, _ error) {
			result := map[string]any{"event": event, "error": "sync operation failed"}
			if ref != nil {
				result["project_key"], _ = ref.StorageKey()
			}
			_ = json.NewEncoder(os.Stderr).Encode(result)
		},
	})
}

type scopeOriginList []string

// caidoSyncCycleReceipt is an operational summary, not proof that the source
// was exhaustively or atomically copied. Caido MCP exposes neither a stable
// snapshot token nor an expected history total, so coverage is never complete
// on this path, even after the cursor chain reaches its reported end.
type caidoSyncCycleReceipt struct {
	Event            string                   `json:"event"`
	CycleState       string                   `json:"cycle_state"`
	ObservedAt       string                   `json:"observed_at"`
	ProjectKey       string                   `json:"project_key"`
	CoverageStatus   string                   `json:"coverage_status"`
	CoverageComplete bool                     `json:"coverage_complete"`
	SnapshotProof    string                   `json:"snapshot_proof"`
	SelectionBinding string                   `json:"selection_binding"`
	WireFidelity     string                   `json:"wire_fidelity"`
	Scopes           []caidoScopeCycleReceipt `json:"scopes"`
}

type caidoScopeCycleReceipt struct {
	CycleState                 string `json:"cycle_state"`
	Origin                     string `json:"origin"`
	BackfillState              string `json:"backfill_state"`
	BackfillCursorState        string `json:"backfill_cursor_state"`
	BackfillPagesThisRun       int    `json:"backfill_pages_this_run"`
	BackfillPageCount          int64  `json:"backfill_page_count"`
	BackfillCoverageReconciled bool   `json:"backfill_coverage_reconciled"`
	BackfillLimitReached       bool   `json:"backfill_limit_reached"`
	ProjectionState            string `json:"projection_state"`
	LiveTailState              string `json:"live_tail_state"`
	LiveTailPages              int    `json:"live_tail_pages"`
	LiveTailWindowBoundary     string `json:"live_tail_window_boundary"`
	LiveTailLimitReached       bool   `json:"live_tail_limit_reached"`
	ResponseRefreshPending     bool   `json:"response_refresh_pending"`
}

func newCaidoSyncCycleReceipt(ref project.CaidoProjectRef, observedAt time.Time) (caidoSyncCycleReceipt, error) {
	key, err := ref.StorageKey()
	if err != nil {
		return caidoSyncCycleReceipt{}, errors.New("Caido project identity is invalid")
	}
	return caidoSyncCycleReceipt{
		Event: "project_sync_cycle_receipt", CycleState: "partial", ObservedAt: observedAt.UTC().Format(time.RFC3339Nano), ProjectKey: key,
		CoverageStatus: "unreconciled", CoverageComplete: false,
		SnapshotProof:    "unavailable_no_stable_snapshot_token_or_expected_history_total",
		SelectionBinding: "checked_before_and_after_each_mcp_history_call; concurrent_selection_change_can_interrupt_cycle",
		WireFidelity:     "recorded_per_exchange; mcp_text_adapter_marks_reconstructed_text_or_unknown; source_bytes_not_asserted",
		Scopes:           []caidoScopeCycleReceipt{},
	}, nil
}

func caidoScopeReceiptInventory(scopes []*ingest.LiveTailScope) []caidoScopeCycleReceipt {
	receipts := make([]caidoScopeCycleReceipt, len(scopes))
	for index, scope := range scopes {
		if scope == nil {
			continue
		}
		receipts[index] = caidoScopeCycleReceipt{
			CycleState: "not_started",
			Origin:     scope.Scheme + "://" + scope.Host + ":" + strconv.Itoa(scope.Port),
		}
	}
	return receipts
}

func (origins *scopeOriginList) String() string {
	if origins == nil {
		return ""
	}
	return strings.Join(*origins, ",")
}

func (origins *scopeOriginList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("exact Caido scope origin is empty")
	}
	*origins = append(*origins, value)
	return nil
}

func normalizeCaidoScopeOrigins(origins []string, legacyScheme, legacyHost string, legacyPort int, required bool) ([]*ingest.LiveTailScope, error) {
	if len(origins) == 0 {
		scope, err := normalizeTailScope(legacyScheme, legacyHost, legacyPort, required)
		if err != nil {
			return nil, err
		}
		if scope == nil {
			return nil, nil
		}
		return []*ingest.LiveTailScope{scope}, nil
	}
	if legacyScheme != "" || legacyHost != "" || legacyPort != 0 {
		return nil, errors.New("use repeated --scope-origin values or legacy scheme/host/port fields, not both")
	}
	seen := make(map[string]struct{}, len(origins))
	normalized := make([]*ingest.LiveTailScope, 0, len(origins))
	for _, raw := range origins {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed == nil || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" ||
			parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.Hostname() == "" {
			return nil, errors.New("Caido scope origin must be an exact scheme://host:port origin")
		}
		scheme := strings.ToLower(parsed.Scheme)
		port := 0
		if parsed.Port() != "" {
			port, err = strconv.Atoi(parsed.Port())
			if err != nil {
				return nil, errors.New("Caido scope origin port is invalid")
			}
		} else if scheme == "https" {
			port = 443
		} else if scheme == "http" {
			port = 80
		}
		scope, err := normalizeTailScope(scheme, parsed.Hostname(), port, true)
		if err != nil {
			return nil, err
		}
		key := scope.Scheme + "://" + scope.Host + ":" + strconv.Itoa(scope.Port)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, scope)
	}
	if len(normalized) == 0 && required {
		return nil, errors.New("at least one exact Caido scope origin is required")
	}
	sort.Slice(normalized, func(i, j int) bool {
		left := normalized[i].Scheme + "://" + normalized[i].Host + ":" + strconv.Itoa(normalized[i].Port)
		right := normalized[j].Scheme + "://" + normalized[j].Host + ":" + strconv.Itoa(normalized[j].Port)
		return left < right
	})
	return normalized, nil
}

func processCaidoProject(
	ctx context.Context,
	source caido.ReadSource,
	ref project.CaidoProjectRef,
	root string,
	scopes []*ingest.LiveTailScope,
	backfillPageSize, backfillBodyLimit, backfillPages int,
	backfillDelay time.Duration,
	tailPageSize, tailPages, bodyLimit int,
	runners map[string]*ingest.ProjectionWorkerRunner,
) (caidoSyncCycleReceipt, error) {
	if len(scopes) == 0 {
		return caidoSyncCycleReceipt{}, errors.New("Caido project sync requires at least one exact origin scope")
	}
	receipt, err := newCaidoSyncCycleReceipt(ref, time.Now())
	if err != nil {
		return caidoSyncCycleReceipt{}, err
	}
	receipt.Scopes = caidoScopeReceiptInventory(scopes)
	for index, scope := range scopes {
		if scope == nil {
			return receipt, errors.New("Caido project sync contains an invalid origin scope")
		}
		scopeReceipt, err := processCaidoProjectScope(ctx, source, ref, root, scope,
			backfillPageSize, backfillBodyLimit, backfillPages, backfillDelay, tailPageSize, tailPages, bodyLimit, runners)
		scopeReceipt.CycleState = "partial"
		if err != nil {
			receipt.Scopes[index] = scopeReceipt
			return receipt, err
		}
		scopeReceipt.CycleState = "processed"
		receipt.Scopes[index] = scopeReceipt
	}
	receipt.CycleState = "processed"
	return receipt, nil
}

func processCaidoProjectScope(
	ctx context.Context,
	source caido.ReadSource,
	ref project.CaidoProjectRef,
	root string,
	scope *ingest.LiveTailScope,
	backfillPageSize, backfillBodyLimit, backfillPages int,
	backfillDelay time.Duration,
	tailPageSize, tailPages, bodyLimit int,
	runners map[string]*ingest.ProjectionWorkerRunner,
) (caidoScopeCycleReceipt, error) {
	scopeReceipt := caidoScopeCycleReceipt{Origin: scope.Scheme + "://" + scope.Host + ":" + strconv.Itoa(scope.Port)}
	refs, err := project.LoadProjectRefsReadOnly(root)
	if err != nil {
		return scopeReceipt, err
	}
	registered := false
	for _, current := range refs {
		if current.SameProject(ref) {
			registered = true
			if !sameProjectDisplayName(current.DisplayName, ref.DisplayName) {
				if err := project.RegisterProject(ref, root); err != nil {
					return scopeReceipt, err
				}
			}
			break
		}
	}
	if !registered {
		created, createErr := store.CreateProject(ctx, ref, root)
		if createErr != nil {
			return scopeReceipt, errors.New("new Caido project store could not be initialized safely")
		}
		if closeErr := created.Close(); closeErr != nil {
			return scopeReceipt, errors.New("new Caido project store could not be closed")
		}
	}
	projectDirectory, err := store.ProjectDirectory(ref, root)
	if err != nil {
		return scopeReceipt, err
	}

	localCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	maintenance, err := projectlock.Acquire(localCtx, projectDirectory)
	if err != nil {
		return scopeReceipt, err
	}
	selected, err := store.OpenProjectForWrite(localCtx, ref, root)
	if err != nil {
		_ = maintenance.Close()
		return scopeReceipt, err
	}
	pageWriter, err := ingest.OpenPageWriter(localCtx, selected)
	if err != nil {
		_ = selected.Close()
		_ = maintenance.Close()
		return scopeReceipt, err
	}
	backfill, backfillErr := ingest.RunBackfill(localCtx, source, pageWriter, ref, ingest.BackfillOptions{
		PageSize: backfillPageSize, MaxPages: backfillPages, MaxBodyBytes: backfillBodyLimit,
		Delay: backfillDelay, Scope: scope,
	})
	pageWriter.Close()
	closeErr := selected.Close()
	leaseErr := maintenance.Close()
	scopeReceipt.BackfillState = backfill.State
	scopeReceipt.BackfillCursorState = backfill.CursorState
	scopeReceipt.BackfillPagesThisRun = backfill.PagesThisRun
	scopeReceipt.BackfillPageCount = backfill.PageCount
	scopeReceipt.BackfillCoverageReconciled = backfill.CoverageReconciled
	scopeReceipt.BackfillLimitReached = backfill.LimitReached
	if backfillErr != nil {
		return scopeReceipt, backfillErr
	}
	if closeErr != nil || leaseErr != nil {
		return scopeReceipt, errors.New("Caido project backfill resources could not be closed")
	}

	maintenance, err = projectlock.Acquire(localCtx, projectDirectory)
	if err != nil {
		return scopeReceipt, err
	}
	selected, err = store.OpenProjectForWrite(localCtx, ref, root)
	if err != nil {
		_ = maintenance.Close()
		return scopeReceipt, err
	}
	var liveWriter *ingest.LiveTailWriter
	if scope != nil {
		liveWriter, err = ingest.OpenLiveTailWriterForScope(localCtx, selected, scope.Scheme, scope.Host, scope.Port)
	} else {
		liveWriter, err = ingest.OpenLiveTailWriter(localCtx, selected)
	}
	if err != nil {
		_ = selected.Close()
		_ = maintenance.Close()
		return scopeReceipt, err
	}
	tail, tailErr := ingest.RunLiveTail(localCtx, source, liveWriter, ref, ingest.LiveTailOptions{
		PageSize: tailPageSize, MaxPages: tailPages, MaxBodyBytes: bodyLimit,
	})
	liveWriter.Close()
	closeErr = selected.Close()
	leaseErr = maintenance.Close()
	scopeReceipt.LiveTailState = tail.State
	scopeReceipt.LiveTailPages = tail.PagesCompleted
	scopeReceipt.LiveTailLimitReached = tail.LimitReached
	scopeReceipt.ResponseRefreshPending = tail.ResponseRefreshPending
	if tail.Watermark != nil {
		scopeReceipt.LiveTailWindowBoundary = *tail.Watermark
	}
	if tailErr != nil {
		return scopeReceipt, tailErr
	}
	if closeErr != nil || leaseErr != nil {
		return scopeReceipt, errors.New("Caido live-tail resources could not be closed")
	}
	if err := drainProjectProjection(localCtx, ref, root, runners); err != nil {
		return scopeReceipt, err
	}
	scopeReceipt.ProjectionState = "drained"
	return scopeReceipt, nil
}

func sameProjectDisplayName(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func drainProjectProjection(ctx context.Context, ref project.CaidoProjectRef, root string, runners map[string]*ingest.ProjectionWorkerRunner) error {
	key, err := ref.StorageKey()
	if err != nil {
		return errors.New("Caido project identity is invalid")
	}
	directory, err := store.ProjectDirectory(ref, root)
	if err != nil {
		return err
	}
	maintenance, err := projectlock.Acquire(ctx, directory)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	selected, err := store.OpenProjectForWrite(ctx, ref, root)
	if err != nil {
		return err
	}
	defer selected.Close()
	runner := runners[key]
	if runner == nil {
		runner = ingest.NewProjectionWorkerRunner()
		runners[key] = runner
	}
	for batch := 0; batch < 10; batch++ {
		results, runErr := runner.RunBatch(ctx, selected, maintenance, ingest.ProjectionWorkerOptions{
			LeaseDuration: time.Minute, MaxAttempts: 8,
		}, 100, nil)
		if runErr != nil {
			return runErr
		}
		if len(results) == 1 && results[0].State == "idle" {
			return nil
		}
		for _, result := range results {
			if result.State != "applied" {
				return errors.New("graph projection queue contains unapplied jobs")
			}
		}
		if len(results) < 100 {
			return nil
		}
	}
	return errors.New("graph projection queue remains pending after the bounded drain")
}

func purgeCaidoProject(ctx context.Context, ref project.CaidoProjectRef, root string) error {
	key, err := ref.StorageKey()
	if err != nil {
		return errors.New("Caido project identity is invalid")
	}
	directory, err := store.ProjectDirectory(ref, root)
	if err != nil {
		return err
	}
	maintenance, err := projectlock.Acquire(ctx, directory)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	config, err := neo4jstore.ConfigFromEnv()
	if err != nil {
		return err
	}
	graph, err := neo4jstore.New(ctx, config)
	if err != nil {
		return errors.New("Neo4j connection is unavailable; project purge was not applied")
	}
	defer graph.Close(context.Background())
	if err := graph.DeleteProject(ctx, key); err != nil {
		return err
	}
	if err := store.PurgeProjectFilesUnderMaintenance(ref, root, maintenance); err != nil {
		return err
	}
	return project.UnregisterProject(ctx, ref, root)
}
