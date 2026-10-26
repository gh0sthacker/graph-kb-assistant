// Package managedapp owns the local web and selected projection workers in one
// process. It never starts Caido intake, a model or a separate MCP subprocess.
package managedapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/mcpserver"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
	"bb-graph-assistant/internal/webui"
)

var projectKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Options struct {
	Address       string
	DataRoot      string
	KBIndex       string
	ProjectKeys   []string
	PollInterval  time.Duration
	LeaseDuration time.Duration
	MaxJobs       int
	MaxAttempts   int
}

func DefaultOptions() Options {
	return Options{Address: "127.0.0.1:8765", PollInterval: time.Second, LeaseDuration: 10 * time.Minute, MaxJobs: 10, MaxAttempts: 8}
}

func (options Options) Validate() error {
	if err := webui.ValidateLoopbackAddress(options.Address); err != nil {
		return err
	}
	if options.PollInterval < 100*time.Millisecond || options.PollInterval > 10*time.Minute || options.LeaseDuration < 30*time.Second || options.LeaseDuration > 30*time.Minute || options.MaxJobs < 1 || options.MaxJobs > 100 || options.MaxAttempts < 1 || options.MaxAttempts > 100 || len(options.ProjectKeys) > 16 {
		return errors.New("managed application limits are invalid")
	}
	seen := map[string]bool{}
	for _, key := range options.ProjectKeys {
		if !projectKeyPattern.MatchString(key) || seen[key] {
			return errors.New("managed application requires distinct valid project keys")
		}
		seen[key] = true
	}
	return nil
}

// Event describes process state, not source coverage or backend parity. Errors
// are fixed codes; credential-bearing driver messages are never forwarded.
type Event struct {
	Component          string `json:"component"`
	State              string `json:"state"`
	Address            string `json:"address,omitempty"`
	ProjectKey         string `json:"project_key,omitempty"`
	ProjectionProjects int    `json:"projection_projects,omitempty"`
	ErrorCode          string `json:"error_code,omitempty"`
	AttemptCount       int    `json:"attempt_count,omitempty"`
	GraphConnectivity  string `json:"graph_connectivity,omitempty"`
	CaidoIntake        string `json:"caido_intake,omitempty"`
	ModelExecution     string `json:"model_execution,omitempty"`
}

type selectedProject struct {
	ref            project.CaidoProjectRef
	key, directory string
}
type dependencies struct {
	listen  func(string, string) (net.Listener, error)
	handler func(Options) (http.Handler, func() error, error)
	worker  func(context.Context, Options, selectedProject, func(Event)) error
}

// Run binds the listener before starting workers. Any component failure cancels
// the others, drains HTTP and waits for worker leases to be released.
func Run(ctx context.Context, options Options, output io.Writer) error {
	return run(ctx, options, output, dependencies{
		listen:  net.Listen,
		handler: newApplicationHandler,
		worker:  runProjectWorker,
	})
}

func newApplicationHandler(options Options) (http.Handler, func() error, error) {
	tools, err := mcpserver.NewTools(options.DataRoot, options.KBIndex)
	if err != nil {
		return nil, nil, errors.New("managed application services could not be initialized")
	}
	webHandler, err := webui.NewHandler(tools)
	if err != nil {
		_ = tools.Close()
		return nil, nil, errors.New("managed application web handler could not be initialized")
	}
	mcpHandler, err := mcpserver.NewStreamableHTTPHandler(tools)
	if err != nil {
		_ = tools.Close()
		return nil, nil, errors.New("managed application MCP handler could not be initialized")
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/", webHandler)
	return mux, tools.Close, nil
}

func run(ctx context.Context, options Options, output io.Writer, deps dependencies) (retErr error) {
	if ctx == nil || output == nil || deps.listen == nil || deps.handler == nil || deps.worker == nil {
		return errors.New("managed application context and dependencies are required")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	selected, err := selectProjects(ctx, options)
	if err != nil {
		return err
	}
	if len(selected) > 0 {
		if graphaccess.ConfiguredBackend() != "neo4j" {
			return errors.New("managed projection workers require the Neo4j backend; omit project keys for web-only compatibility mode")
		}
		if _, err := neo4jstore.ConfigFromEnv(); err != nil {
			return errors.New("managed projection workers require valid Neo4j configuration")
		}
	}
	handler, closeServices, err := deps.handler(options)
	if err != nil {
		return err
	}
	if handler == nil || closeServices == nil {
		return errors.New("managed application services are unavailable")
	}
	defer func() {
		if err := closeServices(); err != nil {
			retErr = errors.Join(retErr, errors.New("managed application services did not close cleanly"))
		}
	}()
	listener, err := deps.listen("tcp", options.Address)
	if err != nil {
		return errors.New("managed application loopback address is unavailable")
	}
	defer listener.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return runCtx }}
	var eventMutex sync.Mutex
	emit := func(event Event) {
		eventMutex.Lock()
		defer eventMutex.Unlock()
		_ = json.NewEncoder(output).Encode(event)
	}
	type componentResult struct {
		name string
		err  error
	}
	results := make(chan componentResult, len(selected)+1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- componentResult{"web", err}
	}()
	for _, selected := range selected {
		go func() {
			emit(Event{Component: "projection", ProjectKey: selected.key, State: "starting"})
			results <- componentResult{"projection", deps.worker(runCtx, options, selected, emit)}
		}()
	}
	emit(Event{Component: "application", State: "listening", Address: listener.Addr().String(), ProjectionProjects: len(selected), GraphConnectivity: "not_checked", CaidoIntake: "not_started", ModelExecution: "not_invoked"})
	remaining := len(selected) + 1
	select {
	case <-ctx.Done():
	case result := <-results:
		remaining--
		if ctx.Err() == nil {
			retErr = fmt.Errorf("managed application %s component stopped unexpectedly", result.name)
			emit(Event{Component: result.name, State: "failed", ErrorCode: "component_stopped"})
		}
	}
	cancel()
	emit(Event{Component: "application", State: "stopping"})
	shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		retErr = errors.Join(retErr, errors.New("managed application HTTP drain did not finish cleanly"))
	}
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err != nil && !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, context.DeadlineExceeded) {
				retErr = errors.Join(retErr, fmt.Errorf("managed application %s component did not stop cleanly", result.name))
			}
		case <-shutdownCtx.Done():
			emit(Event{Component: "application", State: "failed", ErrorCode: "shutdown_timeout"})
			return errors.Join(retErr, errors.New("managed application component shutdown timed out"))
		}
	}
	state := "stopped"
	if retErr != nil {
		state = "failed"
	}
	emit(Event{Component: "application", State: state})
	return retErr
}

func selectProjects(ctx context.Context, options Options) ([]selectedProject, error) {
	selected := []selectedProject{}
	if len(options.ProjectKeys) == 0 {
		return selected, nil
	}
	refs, err := project.LoadProjectRefsReadOnly(options.DataRoot)
	if err != nil {
		return nil, errors.New("managed application project registry is unavailable")
	}
	byKey := map[string]project.CaidoProjectRef{}
	for _, ref := range refs {
		key, err := ref.StorageKey()
		if err != nil {
			return nil, errors.New("managed application project registry is invalid")
		}
		byKey[key] = ref
	}
	for _, key := range options.ProjectKeys {
		ref, exists := byKey[key]
		if !exists {
			return nil, errors.New("managed application selected project is not registered")
		}
		directory, err := store.ProjectDirectory(ref, options.DataRoot)
		if err != nil {
			return nil, errors.New("managed application selected project directory is unavailable")
		}
		readOnly, err := store.OpenProjectReadOnly(ctx, ref, options.DataRoot)
		if err != nil {
			return nil, errors.New("managed application selected project schema is unavailable")
		}
		if err := readOnly.Close(); err != nil {
			return nil, errors.New("managed application project preflight did not close cleanly")
		}
		selected = append(selected, selectedProject{ref: ref, key: key, directory: directory})
	}
	return selected, nil
}

func runProjectWorker(ctx context.Context, options Options, selected selectedProject, emit func(Event)) error {
	runner := ingest.NewProjectionWorkerRunner()
	lastState := ""
	progress := func(state, code string, attempt int) {
		if state != lastState || state == "applied" || state == "pending" || state == "dead_letter" {
			emit(Event{Component: "projection", ProjectKey: selected.key, State: state, ErrorCode: code, AttemptCount: attempt})
			lastState = state
		}
	}
	for ctx.Err() == nil {
		lockCtx, cancelLock := context.WithTimeout(ctx, min(options.PollInterval, 5*time.Second))
		maintenance, err := projectlock.Acquire(lockCtx, selected.directory)
		cancelLock()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, projectlock.ErrWaitTimeout) {
				progress("project_busy", "", 0)
				if !wait(ctx, options.PollInterval) {
					return ctx.Err()
				}
				continue
			}
			return errors.New("projection project lease is unavailable")
		}
		results, err := func() ([]ingest.ProjectionWorkerResult, error) {
			defer maintenance.Close()
			selectedStore, err := store.OpenProjectForWrite(ctx, selected.ref, options.DataRoot)
			if err != nil {
				return nil, errors.New("projection selected store is unavailable")
			}
			defer selectedStore.Close()
			return runner.RunBatch(ctx, selectedStore, maintenance, ingest.ProjectionWorkerOptions{LeaseDuration: options.LeaseDuration, MaxAttempts: options.MaxAttempts}, options.MaxJobs, func(result ingest.ProjectionWorkerResult) {
				progress(result.State, result.ErrorCode, result.AttemptCount)
			})
		}()
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.State == "backend_disabled" {
				return errors.New("projection backend became disabled")
			}
		}
		if !wait(ctx, options.PollInterval) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
