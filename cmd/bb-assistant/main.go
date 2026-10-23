package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bb-graph-assistant/internal/caido"
	behaviorgraph "bb-graph-assistant/internal/graph"
	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/graphsync"
	"bb-graph-assistant/internal/historyreconcile"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/kb"
	"bb-graph-assistant/internal/mcpserver"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	projectaudit "bb-graph-assistant/internal/projectaudit"
	"bb-graph-assistant/internal/projectlock"
	capturesearch "bb-graph-assistant/internal/search"
	"bb-graph-assistant/internal/store"
	"bb-graph-assistant/internal/webui"
)

const version = "0.1.0-go-dev"

const (
	maxCaidoReconcilePages    = 10000
	maxCaidoReconcileRequests = 1000000
)

func isWatchLeaseContention(watch bool, parent context.Context, err error) bool {
	return watch && parent != nil && parent.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

func main() {
	err := run(os.Args[1:])
	if closeErr := graphaccess.CloseProcessService(context.Background()); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func writeCaidoImportProgress(output io.Writer, progress ingest.BackfillProgress) error {
	if output == nil {
		return errors.New("Caido import progress output is unavailable")
	}
	event := struct {
		Event string `json:"event"`
		ingest.BackfillProgress
	}{Event: "caido_import_page", BackfillProgress: progress}
	return json.NewEncoder(output).Encode(event)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bb-assistant <version|db-check|project-audit|neo4j-check|neo4j-backfill|neo4j-sync|projection-worker|caido-reconcile|kb-validate|kb-search|mcp-server|mcp-tools|web-ui|projects|caido-register|search|graph-slice|graph-import|caido-import|caido-tail>")
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "db-check":
		return dbCheck(args[1:])
	case "project-audit":
		return auditProject(args[1:])
	case "neo4j-check":
		return checkNeo4j(args[1:])
	case "neo4j-backfill":
		return backfillNeo4j(args[1:])
	case "neo4j-sync":
		return syncNeo4j(args[1:])
	case "projection-worker":
		return runProjectionWorker(args[1:])
	case "caido-reconcile":
		return reconcileCaidoHistory(args[1:])
	case "kb-validate":
		return validateKB(args[1:])
	case "kb-search":
		return searchKB(args[1:])
	case "mcp-server":
		return serveMCP(args[1:])
	case "mcp-tools":
		return printMCPTools(args[1:])
	case "web-ui":
		return serveWebUI(args[1:])
	case "projects":
		return listProjects(args[1:])
	case "caido-register":
		return registerCaidoProject(args[1:])
	case "search":
		return searchCaptures(args[1:])
	case "graph-slice":
		return readGraphSlice(args[1:])
	case "graph-import":
		return importGraphBundle(args[1:])
	case "caido-import":
		return importCaidoHistory(args[1:])
	case "caido-tail":
		return tailCaidoHistory(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func syncNeo4j(args []string) error {
	flags := flag.NewFlagSet("neo4j-sync", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("usage: bb-assistant neo4j-sync --project-key <sha256> [--data-root path]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			copyRef := ref
			selected = &copyRef
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}
	projectDirectory, err := store.ProjectDirectory(*selected, *root)
	if err != nil {
		return err
	}
	maintenance, err := projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	projectStore, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, *selected, *root, maintenance)
	if err != nil {
		return err
	}
	defer projectStore.Close()
	result, syncErr := graphsync.Drain(ctx, projectStore)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return errors.New("Neo4j sync result could not be written")
	}
	return syncErr
}

func checkNeo4j(args []string) error {
	flags := flag.NewFlagSet("neo4j-check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant neo4j-check")
	}
	cfg, err := neo4jstore.ConfigFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	graphStore, err := neo4jstore.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer graphStore.Close(context.Background())
	if err := graphStore.EnsureSchema(ctx); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"state": "ready", "backend": "neo4j", "database": cfg.Database,
	})
}

func backfillNeo4j(args []string) error {
	flags := flag.NewFlagSet("neo4j-backfill", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	verifyOnly := flags.Bool("verify-only", false, "verify read-back parity without replacing the Neo4j snapshot")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("usage: bb-assistant neo4j-backfill --project-key <sha256> [--data-root path]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			copyRef := ref
			selected = &copyRef
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}
	projectDirectory, err := store.ProjectDirectory(*selected, *root)
	if err != nil {
		return err
	}
	// Hold the same cross-process lock as intake and outbox draining from
	// before the local snapshot through replacement and read-back. Otherwise
	// a newer applied event can be erased by an older snapshot.
	maintenance, err := projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	projectStore, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, *selected, *root, maintenance)
	if err != nil {
		return err
	}
	defer projectStore.Close()
	reader, err := behaviorgraph.OpenReader(ctx, projectStore.BehaviorGraphPath, projectStore.ProjectKey, projectStore.BehaviorReceiptPath)
	if err != nil {
		return err
	}
	localSnapshot, err := reader.Snapshot()
	readerCloseErr := reader.Close()
	if err != nil {
		return err
	}
	if readerCloseErr != nil {
		return errors.New("local BehaviorGraph could not be closed after snapshot validation")
	}
	localDigest, err := behaviorgraph.SnapshotDigest(localSnapshot)
	if err != nil {
		return err
	}
	cfg, err := neo4jstore.ConfigFromEnv()
	if err != nil {
		return err
	}
	graphStore, err := neo4jstore.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer graphStore.Close(context.Background())
	counts := map[string]int{}
	if !*verifyOnly {
		counts, err = graphStore.ReplaceProjectSnapshot(ctx, projectStore.ProjectKey, localSnapshot)
		if err != nil {
			return err
		}
	}
	neo4jSnapshot, err := graphStore.ReadProjectSnapshot(ctx, projectStore.ProjectKey)
	if err != nil {
		return err
	}
	neo4jDigest, err := behaviorgraph.SnapshotDigest(neo4jSnapshot)
	if err != nil {
		return err
	}
	state := "verified"
	if neo4jDigest != localDigest {
		state = "partial"
	}
	result := map[string]any{
		"state": state, "project_key": projectStore.ProjectKey, "backend": "neo4j",
		"snapshot_sha256": localDigest, "neo4j_snapshot_sha256": neo4jDigest,
		"counts": counts, "coverage": "source_sync_not_assessed", "verify_only": *verifyOnly, "record_counts": map[string]int{
			"sources":    len(localSnapshot["sources"].([]behaviorgraph.Record)),
			"entities":   len(localSnapshot["entities"].([]behaviorgraph.Record)),
			"assertions": len(localSnapshot["assertions"].([]behaviorgraph.Record)),
			"cards":      len(localSnapshot["cards"].([]behaviorgraph.Record)),
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return errors.New("Neo4j backfill result could not be written")
	}
	if state != "verified" {
		return errors.New("Neo4j backfill completed without snapshot parity")
	}
	return nil
}

func auditProject(args []string) error {
	flags := flag.NewFlagSet("project-audit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered Caido project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("usage: bb-assistant project-audit --project-key <sha256> [--data-root path]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := projectaudit.AuditProject(ctx, *root, selectedKeys[0])
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

type projectionWorkerDrainOutput struct {
	ProjectKey      string                          `json:"project_key"`
	Processed       int                             `json:"processed"`
	Applied         int                             `json:"applied"`
	Retried         int                             `json:"retried"`
	DeadLetter      int                             `json:"dead_letter"`
	Idle            bool                            `json:"idle"`
	BackendDisabled bool                            `json:"backend_disabled"`
	Jobs            []ingest.ProjectionWorkerResult `json:"jobs"`
}

func runProjectionWorker(args []string) error {
	flags := flag.NewFlagSet("projection-worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	maxJobs := flags.Int("max-jobs", 10, "maximum projection jobs to process in this invocation (1-100)")
	leaseDuration := flags.Duration("lease-duration", 10*time.Minute, "lease and per-job execution limit (30s-30m)")
	maxAttempts := flags.Int("max-attempts", 8, "maximum delivery attempts before dead-lettering (1-100)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("usage: bb-assistant projection-worker --project-key <sha256> [--max-jobs 10] [--lease-duration 10m] [--max-attempts 8] [--data-root path]")
	}
	if *maxJobs < 1 || *maxJobs > 100 || *leaseDuration < 30*time.Second ||
		*leaseDuration > 30*time.Minute || *maxAttempts < 1 || *maxAttempts > 100 {
		return errors.New("projection worker limits are invalid")
	}
	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			copyRef := ref
			selected = &copyRef
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}
	projectDirectory, err := store.ProjectDirectory(*selected, *root)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	output := projectionWorkerDrainOutput{ProjectKey: selectedKeys[0], Jobs: make([]ingest.ProjectionWorkerResult, 0, *maxJobs)}
	for index := 0; index < *maxJobs; index++ {
		localCtx, cancel := context.WithTimeout(ctx, *leaseDuration-time.Second)
		result, runErr := func() (ingest.ProjectionWorkerResult, error) {
			maintenance, lockErr := projectlock.Acquire(localCtx, projectDirectory)
			if lockErr != nil {
				return ingest.ProjectionWorkerResult{}, lockErr
			}
			defer maintenance.Close()
			selectedStore, openErr := store.OpenProjectForWrite(localCtx, *selected, *root)
			if openErr != nil {
				return ingest.ProjectionWorkerResult{}, openErr
			}
			defer selectedStore.Close()
			return ingest.RunProjectionWorkerOnce(localCtx, selectedStore, maintenance, ingest.ProjectionWorkerOptions{
				LeaseDuration: *leaseDuration, MaxAttempts: *maxAttempts,
			})
		}()
		cancel()
		if runErr != nil {
			if ctx.Err() != nil {
				break
			}
			return runErr
		}
		output.Jobs = append(output.Jobs, result)
		if result.State == "backend_disabled" {
			output.BackendDisabled = true
			break
		}
		if result.State == "idle" {
			output.Idle = true
			break
		}
		output.Processed++
		switch result.State {
		case "applied":
			output.Applied++
		case "pending":
			output.Retried++
		case "dead_letter":
			output.DeadLetter++
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		return errors.New("projection worker result could not be written")
	}
	return nil
}

func reconcileCaidoHistory(args []string) error {
	options, err := parseCaidoReconcileOptions(args)
	if err != nil {
		return err
	}
	refs, err := project.LoadProjectRefsReadOnly(options.dataRoot)
	if err != nil {
		return err
	}
	var instanceID string
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == options.projectKey {
			instanceID = ref.InstanceID
			break
		}
	}
	if instanceID == "" {
		return errors.New("selected project is not in the local project registry")
	}

	baseCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(baseCtx, options.timeout)
	defer cancel()
	client, err := caido.StartMCPProcess(caido.MCPProcessConfig{
		Context: ctx, Command: options.mcpCommand, ExpectedSHA256: options.mcpSHA256, TokenFile: options.tokenFile,
		StartupTimeout: 30 * time.Second, CallTimeout: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer client.Close()
	source, err := caido.NewMCPReadSource(client, instanceID)
	if err != nil {
		return err
	}
	result, err := historyreconcile.Run(ctx, options.dataRoot, options.projectKey, source, historyreconcile.Options{
		PageSize: options.pageSize, MaxPages: options.maxPages, MaxRequestIDs: options.maxRequests,
		MaxDuration: options.timeout,
	})
	if result.State != "" {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
			return errors.New("Caido reconciliation result could not be written")
		}
	}
	if err != nil {
		return err
	}
	return nil
}

type caidoReconcileOptions struct {
	projectKey  string
	dataRoot    string
	mcpCommand  string
	mcpSHA256   string
	tokenFile   string
	pageSize    int
	maxPages    int
	maxRequests int
	timeout     time.Duration
}

func parseCaidoReconcileOptions(args []string) (caidoReconcileOptions, error) {
	flags := flag.NewFlagSet("caido-reconcile", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered Caido project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	mcpCommand := flags.String("mcp-command", "", "pinned Caido MCP v4.3.0 executable")
	mcpSHA256 := flags.String("mcp-sha256", "", "expected SHA-256 of the Caido MCP executable")
	tokenFile := flags.String("token-file", "", "private Caido MCP token JSON file")
	pageSize := flags.Int("page-size", 50, "metadata rows per page (1-100)")
	maxPages := flags.Int("max-pages", 100, "maximum metadata pages (1-10000)")
	maxRequests := flags.Int("max-requests", 10000, "maximum distinct IDs per inventory (1-1000000)")
	timeout := flags.Duration("timeout", 30*time.Second, "total reconciliation deadline (1s-30m)")
	if err := flags.Parse(args); err != nil {
		return caidoReconcileOptions{}, err
	}
	if flags.NArg() != 0 {
		return caidoReconcileOptions{}, errors.New("usage: bb-assistant caido-reconcile --project-key <sha256> [--page-size 50] [--max-pages 100] [--max-requests 10000] [--timeout 30s]")
	}
	if len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return caidoReconcileOptions{}, errors.New("select exactly one registered project with --project-key <sha256>")
	}
	if *pageSize < 1 || *pageSize > 100 || *maxPages < 1 || *maxPages > maxCaidoReconcilePages ||
		*maxRequests < 1 || *maxRequests > maxCaidoReconcileRequests || *timeout < time.Second || *timeout > 30*time.Minute {
		return caidoReconcileOptions{}, errors.New("Caido reconciliation limits are invalid")
	}
	return caidoReconcileOptions{
		projectKey: selectedKeys[0], dataRoot: *root, mcpCommand: *mcpCommand,
		mcpSHA256: *mcpSHA256, tokenFile: *tokenFile, pageSize: *pageSize,
		maxPages: *maxPages, maxRequests: *maxRequests, timeout: *timeout,
	}, nil
}

func tailCaidoHistory(args []string) error {
	flags := flag.NewFlagSet("caido-tail", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered Caido project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	mcpCommand := flags.String("mcp-command", "", "pinned Caido MCP v4.3.0 executable")
	mcpSHA256 := flags.String("mcp-sha256", "", "expected SHA-256 of the Caido MCP executable")
	tokenFile := flags.String("token-file", "", "private Caido MCP token JSON file")
	pageSize := flags.Int("page-size", 50, "Caido metadata rows per page (1-100)")
	maxPages := flags.Int("max-pages", 1, "maximum pages in this bounded invocation (1-100)")
	watch := flags.Bool("watch", false, "repeat bounded live-tail cycles until interrupted")
	pollInterval := flags.Duration("poll-interval", 10*time.Second, "delay between --watch cycles (1s-10m)")
	overlap := flags.Int("overlap-seconds", 90, "time overlap used to recover delayed records (1-3600)")
	bodyLimit := flags.Int("body-limit", 256<<10, "maximum request/response bytes per live exchange")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant caido-tail --project-key <sha256> [--page-size 50] [--max-pages 1] [--watch --poll-interval 10s]")
	}
	if len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("select exactly one registered project with --project-key <sha256>")
	}
	if *pageSize < 1 || *pageSize > 100 || *maxPages < 1 || *maxPages > 100 ||
		*overlap < 1 || *overlap > 3600 || *bodyLimit < 64<<10 || *bodyLimit > 16<<20 ||
		*pollInterval < time.Second || *pollInterval > 10*time.Minute ||
		!ingest.LivePageBudgetFits(*pageSize, *bodyLimit) {
		return errors.New("Caido live-tail limits are invalid")
	}

	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			matched := ref
			selected = &matched
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}

	projectDirectory, err := store.ProjectDirectory(*selected, *root)
	if err != nil {
		return err
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
	source, err := caido.NewMCPReadSource(client, selected.InstanceID)
	if err != nil {
		return err
	}
	for {
		maintenanceContended := false
		result, runErr := func() (ingest.LiveTailResult, error) {
			localCtx, localCancel := context.WithTimeout(ctx, 30*time.Second)
			defer localCancel()
			maintenance, lockErr := projectlock.Acquire(localCtx, projectDirectory)
			if lockErr != nil {
				maintenanceContended = isWatchLeaseContention(*watch, ctx, lockErr)
				return ingest.LiveTailResult{}, lockErr
			}
			defer maintenance.Close()
			selectedStore, openErr := store.OpenProjectForWrite(localCtx, *selected, *root)
			if openErr != nil {
				return ingest.LiveTailResult{}, openErr
			}
			defer selectedStore.Close()
			tailWriter, writerErr := ingest.OpenLiveTailWriter(localCtx, selectedStore)
			if writerErr != nil {
				return ingest.LiveTailResult{}, writerErr
			}
			defer tailWriter.Close()
			return ingest.RunLiveTail(ctx, source, tailWriter, *selected, ingest.LiveTailOptions{
				PageSize: *pageSize, MaxPages: *maxPages, OverlapSeconds: *overlap, MaxBodyBytes: *bodyLimit,
			})
		}()
		if maintenanceContended {
			fmt.Fprintln(os.Stderr, "project is busy; caido-tail --watch will retry after the poll interval")
			timer := time.NewTimer(*pollInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil
			case <-timer.C:
			}
			continue
		}
		if runErr != nil && ctx.Err() != nil {
			return nil
		}
		if result.ProjectKey != "" {
			if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
				return errors.New("Caido live-tail result could not be written")
			}
		}
		if !*watch || ctx.Err() != nil || runErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return runErr
		}
		timer := time.NewTimer(*pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func importCaidoHistory(args []string) error {
	flags := flag.NewFlagSet("caido-import", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered Caido project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	mcpCommand := flags.String("mcp-command", "", "pinned Caido MCP v4.3.0 executable")
	mcpSHA256 := flags.String("mcp-sha256", "", "expected SHA-256 of the Caido MCP executable")
	tokenFile := flags.String("token-file", "", "private Caido MCP token JSON file")
	pageSize := flags.Int("page-size", 50, "history requests per page (1-100)")
	maxPages := flags.Int("max-pages", 0, "maximum pages in this invocation; 0 continues to completion")
	bodyLimit := flags.Int("body-limit", 512<<10, "maximum request/response body bytes per exchange")
	delay := flags.Duration("delay", time.Second, "pause between persisted pages")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant caido-import --project-key <sha256> [--page-size 50] [--max-pages 10]")
	}
	if len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("select exactly one registered project with --project-key <sha256>")
	}
	if *pageSize < 1 || *pageSize > 100 || *maxPages < 0 || *bodyLimit < 64<<10 || *bodyLimit > 128<<20 ||
		int64(*pageSize)*int64(*bodyLimit)*2 > 64<<20 || *delay < 0 || *delay > time.Minute {
		return errors.New("Caido import limits are invalid")
	}

	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			matched := ref
			selected = &matched
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	projectDirectory, err := store.ProjectDirectory(*selected, *root)
	if err != nil {
		return err
	}
	maintenance, err := projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	localCtx, localCancel := context.WithTimeout(ctx, 30*time.Second)
	defer localCancel()
	selectedStore, err := store.OpenProjectForWrite(localCtx, *selected, *root)
	if err != nil {
		return err
	}
	defer selectedStore.Close()
	pageWriter, err := ingest.OpenPageWriter(localCtx, selectedStore)
	if err != nil {
		return err
	}
	defer pageWriter.Close()

	client, err := caido.StartMCPProcess(caido.MCPProcessConfig{
		Context: ctx, Command: *mcpCommand, ExpectedSHA256: *mcpSHA256, TokenFile: *tokenFile,
		StartupTimeout: 30 * time.Second, CallTimeout: 180 * time.Second,
	})
	if err != nil {
		return err
	}
	defer client.Close()
	source, err := caido.NewMCPReadSource(client, selected.InstanceID)
	if err != nil {
		return err
	}
	availableProjects, err := source.ListProjects()
	if err != nil {
		return err
	}
	available := false
	for _, candidate := range availableProjects {
		if candidate.SameProject(*selected) {
			available = true
			break
		}
	}
	if !available {
		return errors.New("selected project is not available from the configured Caido MCP instance")
	}

	var progressWriteErr error
	result, runErr := ingest.RunBackfill(ctx, source, pageWriter, *selected, ingest.BackfillOptions{
		PageSize: *pageSize, MaxPages: *maxPages, MaxBodyBytes: *bodyLimit, Delay: *delay,
		OnProgress: func(progress ingest.BackfillProgress) {
			if progressWriteErr == nil {
				progressWriteErr = writeCaidoImportProgress(os.Stderr, progress)
			}
		},
	})
	if progressWriteErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, "warning: Caido page progress output failed; import continued")
	}
	if result.RunID != "" {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return errors.New("Caido import result could not be written")
		}
	}
	return runErr
}

func importGraphBundle(args []string) error {
	flags := flag.NewFlagSet("graph-import", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered project key (exactly one required)")
	root := flags.String("data-root", "", "private assistant data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: bb-assistant graph-import --project-key <sha256> <bundle.json>")
	}
	if len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("select exactly one registered project with --project-key <sha256>")
	}

	inputPath := flags.Arg(0)
	inputInfo, err := os.Lstat(inputPath)
	if err != nil || inputInfo.Mode()&os.ModeSymlink != 0 || !inputInfo.Mode().IsRegular() || inputInfo.Size() < 1 || inputInfo.Size() > behaviorgraph.MaxBundleJSONBytes {
		return errors.New("bundle input must be a regular file within the size limit")
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return errors.New("bundle input could not be opened")
	}
	openedInfo, statErr := input.Stat()
	if statErr != nil || !os.SameFile(inputInfo, openedInfo) {
		_ = input.Close()
		return errors.New("bundle input changed during validation")
	}
	encoded, readErr := io.ReadAll(io.LimitReader(input, behaviorgraph.MaxBundleJSONBytes+1))
	closeErr := input.Close()
	if readErr != nil || closeErr != nil || len(encoded) > behaviorgraph.MaxBundleJSONBytes {
		return errors.New("bundle input could not be read within the size limit")
	}
	bundle, err := behaviorgraph.DecodeBundleJSON(encoded)
	if err != nil {
		return err
	}

	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			matched := ref
			selected = &matched
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	projectDirectory, err := store.ProjectDirectory(*selected, *root)
	if err != nil {
		return err
	}
	maintenance, err := projectlock.Acquire(ctx, projectDirectory)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	projectStore, err := store.OpenProjectReadOnlyUnderMaintenance(ctx, *selected, *root, maintenance)
	if err != nil {
		return err
	}
	defer projectStore.Close()
	writer, err := behaviorgraph.OpenWriter(ctx, projectStore)
	if err != nil {
		return err
	}
	result, err := writer.ImportBundle(ctx, bundle)
	if err != nil {
		return err
	}
	syncResult, syncErr := graphsync.Drain(ctx, projectStore)
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"import": result, "neo4j_sync": syncResult}); err != nil {
		return errors.New("graph import result could not be written")
	}
	return syncErr
}

func readGraphSlice(args []string) error {
	flags := flag.NewFlagSet("graph-slice", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered project key (exactly one required)")
	var seeds stringListFlag
	flags.Var(&seeds, "seed", "typed graph seed ID (repeat for multiple seeds)")
	root := flags.String("data-root", "", "private assistant data root")
	maxNodes := flags.Int("max-nodes", 80, "maximum selected graph nodes (1-100)")
	maxEdges := flags.Int("max-edges", 160, "maximum selected graph edges (0-300)")
	maxDepth := flags.Int("max-depth", 3, "maximum graph traversal depth (0-6)")
	contextJSON := flags.String("context-json", "", "optional JSON object restricting actor/tenant/role context")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant graph-slice --project-key <sha256> --seed <typed-id> [--seed <typed-id>] [limits]")
	}
	if len(selectedKeys) != 1 || !validProjectKey(selectedKeys[0]) {
		return errors.New("select exactly one registered project with --project-key <sha256>")
	}
	if len(seeds) == 0 {
		return errors.New("provide at least one graph seed with --seed")
	}
	seenSeeds := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		if _, exists := seenSeeds[seed]; exists {
			return errors.New("graph seed IDs must be unique")
		}
		seenSeeds[seed] = struct{}{}
	}
	var selectedContext map[string]any
	if strings.TrimSpace(*contextJSON) != "" {
		decoder := json.NewDecoder(strings.NewReader(*contextJSON))
		decoder.UseNumber()
		if err := decoder.Decode(&selectedContext); err != nil || selectedContext == nil {
			return errors.New("context-json must be a JSON object")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return errors.New("context-json must contain one JSON object")
		}
	}

	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		if key == selectedKeys[0] {
			matched := ref
			selected = &matched
			break
		}
	}
	if selected == nil {
		return errors.New("selected project is not in the local project registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	projectStore, err := store.OpenProjectReadOnly(ctx, *selected, *root)
	if err != nil {
		return err
	}
	defer projectStore.Close()
	result, err := capturesearch.GraphSlice(ctx, projectStore, seeds, *maxNodes, *maxEdges, *maxDepth, selectedContext)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func dbCheck(args []string) error {
	flags := flag.NewFlagSet("db-check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: bb-assistant db-check <existing-project.sqlite3>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := store.OpenReadOnly(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	defer db.Close()
	counts := map[string]int64{}
	for _, table := range []string{"captures", "capture_revisions", "capture_search", "live_capture_index"} {
		var count int64
		query := "SELECT count(*) FROM " + table
		if err := db.DB().QueryRowContext(ctx, query).Scan(&count); err != nil {
			return errors.New("database row counts could not be read")
		}
		counts[table] = count
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"schema_version": store.CurrentSchemaVersion,
		"read_only":      true,
		"counts":         counts,
	})
}

func validateKB(args []string) error {
	flags := flag.NewFlagSet("kb-validate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: bb-assistant kb-validate <snapshot-directory>")
	}
	snapshot, err := kb.OpenSnapshot(flags.Arg(0))
	if err != nil {
		return err
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"snapshot_id":           snapshot.Manifest.SnapshotID,
		"documents":             snapshot.Manifest.DocumentCount,
		"chunks":                snapshot.Manifest.ChunkCount,
		"source_coverage_state": snapshot.Manifest.SourceCoverageState,
		"validated":             true,
	})
}

func searchKB(args []string) error {
	flags := flag.NewFlagSet("kb-search", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	indexPath := flags.String("index", "", "existing local KB SQLite index (defaults to BB_ASSISTANT_KB_INDEX or the private data root)")
	query := flags.String("query", "", "search query")
	topK := flags.Int("top-k", kb.DefaultSearchTopK, "maximum citations to return (1-100)")
	var sourceNames stringListFlag
	flags.Var(&sourceNames, "source", "restrict retrieval to a KB source family; repeat for multiple families")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*query) == "" {
		return errors.New("usage: bb-assistant kb-search --query <text> [--top-k 10] [--source <family>] [--index path]")
	}
	index, err := kb.OpenIndex(*indexPath)
	if err != nil {
		return err
	}
	defer index.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := index.Search(ctx, *query, kb.SearchOptions{TopK: *topK, SourceNames: sourceNames})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func serveMCP(args []string) (retErr error) {
	flags := flag.NewFlagSet("mcp-server", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("data-root", "", "private assistant data root")
	kbIndexPath := flags.String("kb-index", "", "existing local KB SQLite index")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant mcp-server [--data-root path] [--kb-index path]")
	}
	tools, err := mcpserver.NewTools(*root, *kbIndexPath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, tools.Close()) }()
	server, err := mcpserver.NewServer(tools)
	if err != nil {
		return err
	}
	return server.Serve(context.Background(), os.Stdin, os.Stdout)
}

func printMCPTools(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: bb-assistant mcp-tools")
	}
	encoded, err := mcpserver.ToolCatalogJSON(os.Getenv("BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL") == "1")
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		return errors.New("MCP tool reference could not be written")
	}
	return nil
}

func serveWebUI(args []string) (retErr error) {
	flags := flag.NewFlagSet("web-ui", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("data-root", "", "private assistant data root")
	kbIndexPath := flags.String("kb-index", "", "existing local KB SQLite index")
	address := flags.String("addr", "127.0.0.1:8765", "loopback address for the local graph explorer")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant web-ui [--data-root path] [--kb-index path] [--addr 127.0.0.1:8765]")
	}
	if err := webui.ValidateLoopbackAddress(*address); err != nil {
		return err
	}
	tools, err := mcpserver.NewTools(*root, *kbIndexPath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, tools.Close()) }()
	handler, err := webui.NewHandler(tools)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "Graph explorer: http://%s/ (read-only, loopback only)\n", *address)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func listProjects(args []string) error {
	flags := flag.NewFlagSet("projects", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("data-root", "", "private assistant data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant projects [--data-root path]")
	}
	projects, err := project.LoadProjectRefs(*root)
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(projects))
	for _, item := range projects {
		key, err := item.StorageKey()
		if err != nil {
			return errors.New("project registry contains an invalid identity")
		}
		rows = append(rows, map[string]any{
			"project_key":  key,
			"instance_id":  item.InstanceID,
			"project_id":   item.ProjectID,
			"display_name": item.DisplayName,
		})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"projects": rows})
}

func registerCaidoProject(args []string) error {
	flags := flag.NewFlagSet("caido-register", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("data-root", "", "private assistant data root")
	instanceID := flags.String("instance-id", "", "stable operator-defined Caido instance key")
	projectID := flags.String("project-id", "", "exact Caido project ID to register")
	displayName := flags.String("name", "", "optional Caido project display name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant caido-register --instance-id <stable-key> --project-id <id> [--name <display-name>]")
	}
	ref := project.CaidoProjectRef{InstanceID: *instanceID, ProjectID: *projectID}
	if *displayName != "" {
		ref.DisplayName = displayName
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, err := store.CreateProject(ctx, ref, *root)
	if err != nil {
		return errors.New("Caido project store could not be initialized; existing stores are never overwritten")
	}
	key := created.ProjectKey
	if err := created.Close(); err != nil {
		return errors.New("Caido project store could not be closed")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"project_key": key, "instance_id": ref.InstanceID,
		"project_id": ref.ProjectID, "display_name": ref.DisplayName,
	})
}

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type optionalIntFlag struct {
	value int
	set   bool
}

func (value *optionalIntFlag) String() string {
	if !value.set {
		return ""
	}
	return strconv.Itoa(value.value)
}

func (value *optionalIntFlag) Set(raw string) error {
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return errors.New("status must be an integer HTTP status code")
	}
	value.value = parsed
	value.set = true
	return nil
}

func searchCaptures(args []string) error {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var selectedKeys stringListFlag
	flags.Var(&selectedKeys, "project-key", "registered project key (repeat to select multiple projects)")
	root := flags.String("data-root", "", "private assistant data root")
	textFilter := flags.String("text", "", "search structural FTS fields")
	method := flags.String("method", "", "exact HTTP method")
	host := flags.String("host", "", "case-insensitive host substring")
	route := flags.String("route", "", "case-insensitive normalized route substring")
	var status optionalIntFlag
	flags.Var(&status, "status", "exact HTTP response status")
	fieldName := flags.String("field-name", "", "case-insensitive request/response field-name substring")
	requestSignature := flags.String("request-signature", "", "exact keyed request variant digest")
	responseSignature := flags.String("response-signature", "", "exact keyed response variant digest")
	since := flags.String("since", "", "inclusive capture/sync timestamp")
	until := flags.String("until", "", "inclusive capture/sync timestamp")
	limit := flags.Int("limit", capturesearch.DefaultLimit, "maximum results (1-500)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant search --project-key <sha256> [--project-key <sha256>] [filters]")
	}
	if len(selectedKeys) == 0 {
		return errors.New("select at least one registered project with --project-key")
	}
	if len(selectedKeys) > capturesearch.MaximumProjects {
		return errors.New("select no more than 20 Caido projects")
	}
	seen := make(map[string]struct{}, len(selectedKeys))
	for _, key := range selectedKeys {
		if !validProjectKey(key) {
			return errors.New("project key must be a lowercase 64-character SHA-256 digest")
		}
		if _, ok := seen[key]; ok {
			return errors.New("a project may appear only once in the selection")
		}
		seen[key] = struct{}{}
	}

	refs, err := project.LoadProjectRefsReadOnly(*root)
	if err != nil {
		return err
	}
	byKey := make(map[string]project.CaidoProjectRef, len(refs))
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		byKey[key] = ref
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stores := make([]*store.ProjectStore, 0, len(selectedKeys))
	closeStores := func() {
		for _, projectStore := range stores {
			_ = projectStore.Close()
		}
	}
	for _, key := range selectedKeys {
		ref, ok := byKey[key]
		if !ok {
			closeStores()
			return errors.New("selected project is not in the local project registry")
		}
		projectStore, openErr := store.OpenProjectReadOnly(ctx, ref, *root)
		if openErr != nil {
			closeStores()
			return openErr
		}
		stores = append(stores, projectStore)
	}
	defer closeStores()

	filters := capturesearch.Filters{
		Text: *textFilter, Method: *method, Host: *host, Route: *route,
		FieldName: *fieldName, RequestSignature: *requestSignature,
		ResponseSignature: *responseSignature, Since: *since, Until: *until,
		Limit: *limit,
	}
	if status.set {
		filters.Status = &status.value
	}
	result, err := capturesearch.Captures(ctx, stores, filters)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func validProjectKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, value := range key {
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')) {
			return false
		}
	}
	return true
}
