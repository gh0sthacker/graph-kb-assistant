package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
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
	root := flags.String("data-root", "", "private assistant data root")
	instanceID := flags.String("instance-id", os.Getenv("BB_ASSISTANT_CAIDO_INSTANCE_ID"), "stable operator-defined Caido instance key")
	mcpCommand := flags.String("mcp-command", "", "pinned Caido MCP v4.3.0 executable")
	mcpSHA256 := flags.String("mcp-sha256", "", "expected SHA-256 of the Caido MCP executable")
	tokenFile := flags.String("token-file", "", "private Caido MCP token JSON file")
	pollInterval := flags.Duration("poll-interval", 30*time.Second, "project inventory and request tail interval (1s-10m)")
	missingConfirmations := flags.Int("missing-confirmations", 3, "successful project inventories required before purge (2-20)")
	backfillPages := flags.Int("backfill-pages", 5, "historical pages per project per inventory cycle (1-100)")
	backfillPageSize := flags.Int("backfill-page-size", 50, "historical requests per page (1-100)")
	backfillDelay := flags.Duration("backfill-delay", 250*time.Millisecond, "delay between persisted historical pages")
	tailPages := flags.Int("tail-pages", 1, "live-tail pages per project per inventory cycle (1-100)")
	pageSize := flags.Int("page-size", 50, "live-tail requests per page (1-100)")
	bodyLimit := flags.Int("body-limit", 256<<10, "maximum live-tail request/response bytes per exchange")
	scopeScheme := flags.String("scope-scheme", os.Getenv("BB_ASSISTANT_CAIDO_SCOPE_SCHEME"), "optional exact HTTP origin scope scheme")
	scopeHost := flags.String("scope-host", os.Getenv("BB_ASSISTANT_CAIDO_SCOPE_HOST"), "optional exact HTTP origin scope host")
	scopePortDefault, err := liveTailScopePortDefault()
	if err != nil {
		return err
	}
	scopePort := flags.Int("scope-port", scopePortDefault, "optional exact HTTP origin scope port")
	requireScope := flags.Bool("require-scope", false, "fail unless an exact HTTP origin scope is configured")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *instanceID == "" || *pollInterval < time.Second || *pollInterval > 10*time.Minute ||
		*missingConfirmations < 2 || *missingConfirmations > 20 ||
		*backfillPages < 1 || *backfillPages > 100 || *backfillPageSize < 1 || *backfillPageSize > 100 ||
		*backfillDelay < 0 || *backfillDelay > time.Minute || *tailPages < 1 || *tailPages > 100 ||
		*pageSize < 1 || *pageSize > 100 || *bodyLimit < 64<<10 || *bodyLimit > 16<<20 ||
		!ingest.LivePageBudgetFits(*pageSize, *bodyLimit) {
		return errors.New("Caido sync options are invalid")
	}
	if graphaccess.ConfiguredBackend() != "neo4j" {
		return errors.New("continuous Caido sync requires the Neo4j graph backend")
	}
	scope, err := normalizeTailScope(*scopeScheme, *scopeHost, *scopePort, *requireScope)
	if err != nil {
		return err
	}
	if *backfillPageSize < 1 || int64(*backfillPageSize)*int64(512<<10)*2 > 64<<20 {
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
	return ingest.RunProjectInventorySync(ctx, source, ingest.ProjectInventorySyncOptions{
		DataRoot: *root, InstanceID: *instanceID, PollInterval: *pollInterval,
		MissingConfirmations: *missingConfirmations,
		OnProject: func(ctx context.Context, ref project.CaidoProjectRef) error {
			if err := processCaidoProject(ctx, source, ref, *root, scope, *backfillPageSize, *backfillPages, *backfillDelay,
				*pageSize, *tailPages, *bodyLimit, runners); err != nil {
				return err
			}
			key, _ := ref.StorageKey()
			_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "project_synced", "project_key": key})
			return nil
		},
		OnProjectMissing: func(ctx context.Context, ref project.CaidoProjectRef) error {
			if err := purgeCaidoProject(ctx, ref, *root); err != nil {
				return err
			}
			key, _ := ref.StorageKey()
			delete(runners, key)
			_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "project_purged", "project_key": key})
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

func processCaidoProject(
	ctx context.Context,
	source caido.ReadSource,
	ref project.CaidoProjectRef,
	root string,
	scope *ingest.LiveTailScope,
	backfillPageSize, backfillPages int,
	backfillDelay time.Duration,
	tailPageSize, tailPages, bodyLimit int,
	runners map[string]*ingest.ProjectionWorkerRunner,
) error {
	refs, err := project.LoadProjectRefsReadOnly(root)
	if err != nil {
		return err
	}
	registered := false
	for _, current := range refs {
		if current.SameProject(ref) {
			registered = true
			if !sameProjectDisplayName(current.DisplayName, ref.DisplayName) {
				if err := project.RegisterProject(ref, root); err != nil {
					return err
				}
			}
			break
		}
	}
	if !registered {
		created, createErr := store.CreateProject(ctx, ref, root)
		if createErr != nil {
			return errors.New("new Caido project store could not be initialized safely")
		}
		if closeErr := created.Close(); closeErr != nil {
			return errors.New("new Caido project store could not be closed")
		}
	}
	projectDirectory, err := store.ProjectDirectory(ref, root)
	if err != nil {
		return err
	}

	localCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	maintenance, err := projectlock.Acquire(localCtx, projectDirectory)
	if err != nil {
		return err
	}
	selected, err := store.OpenProjectForWrite(localCtx, ref, root)
	if err != nil {
		_ = maintenance.Close()
		return err
	}
	pageWriter, err := ingest.OpenPageWriter(localCtx, selected)
	if err != nil {
		_ = selected.Close()
		_ = maintenance.Close()
		return err
	}
	_, backfillErr := ingest.RunBackfill(localCtx, source, pageWriter, ref, ingest.BackfillOptions{
		PageSize: backfillPageSize, MaxPages: backfillPages, MaxBodyBytes: 512 << 10,
		Delay: backfillDelay, Scope: scope,
	})
	pageWriter.Close()
	closeErr := selected.Close()
	leaseErr := maintenance.Close()
	if backfillErr != nil {
		return backfillErr
	}
	if closeErr != nil || leaseErr != nil {
		return errors.New("Caido project backfill resources could not be closed")
	}

	maintenance, err = projectlock.Acquire(localCtx, projectDirectory)
	if err != nil {
		return err
	}
	selected, err = store.OpenProjectForWrite(localCtx, ref, root)
	if err != nil {
		_ = maintenance.Close()
		return err
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
		return err
	}
	_, tailErr := ingest.RunLiveTail(localCtx, source, liveWriter, ref, ingest.LiveTailOptions{
		PageSize: tailPageSize, MaxPages: tailPages, MaxBodyBytes: bodyLimit,
	})
	liveWriter.Close()
	closeErr = selected.Close()
	leaseErr = maintenance.Close()
	if tailErr != nil {
		return tailErr
	}
	if closeErr != nil || leaseErr != nil {
		return errors.New("Caido live-tail resources could not be closed")
	}
	return drainProjectProjection(localCtx, ref, root, runners)
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
