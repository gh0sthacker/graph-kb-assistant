package managedapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

type eventRecorder struct {
	mu     sync.Mutex
	events chan Event
	data   bytes.Buffer
}

func newEventRecorder() *eventRecorder { return &eventRecorder{events: make(chan Event, 64)} }
func (w *eventRecorder) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, err
	}
	w.events <- event
	return w.data.Write(data)
}
func (w *eventRecorder) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.data.String() }
func awaitListening(t *testing.T, recorder *eventRecorder) Event {
	t.Helper()
	for {
		select {
		case event := <-recorder.events:
			if event.State == "listening" {
				return event
			}
		case <-time.After(3 * time.Second):
			t.Fatal("application did not bind listener")
			return Event{}
		}
	}
}

func TestOptionsRejectInvalidInputBeforeOpeningServices(t *testing.T) {
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Address = "0.0.0.0:8765" }, func(o *Options) { o.PollInterval = 0 }, func(o *Options) { o.LeaseDuration = time.Second }, func(o *Options) { o.MaxJobs = 101 }, func(o *Options) { o.MaxAttempts = 0 }, func(o *Options) { o.ProjectKeys = []string{"bad"} }, func(o *Options) { o.ProjectKeys = []string{strings.Repeat("a", 64), strings.Repeat("a", 64)} },
	} {
		opts := DefaultOptions()
		mutate(&opts)
		opened := false
		deps := dependencies{listen: net.Listen, handler: func(Options) (http.Handler, func() error, error) { opened = true; return nil, nil, nil }, worker: runProjectWorker}
		if err := run(t.Context(), opts, io.Discard, deps); err == nil || opened {
			t.Fatalf("invalid options reached I/O: err=%v opened=%t", err, opened)
		}
	}
}

func TestManagedWebStopsAndRestartsOnTheSameListener(t *testing.T) {
	address := "127.0.0.1:0"
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithCancel(t.Context())
		recorder := newEventRecorder()
		closed := make(chan struct{})
		opts := DefaultOptions()
		deps := dependencies{
			listen: func(network, _ string) (net.Listener, error) { return net.Listen(network, address) },
			handler: func(Options) (http.Handler, func() error, error) {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "synthetic application")
				}), func() error { close(closed); return nil }, nil
			},
			worker: func(context.Context, Options, selectedProject, func(Event)) error {
				t.Error("web-only launch invoked a projection worker")
				return nil
			},
		}
		finished := make(chan error, 1)
		go func() { finished <- run(ctx, opts, recorder, deps) }()
		ready := awaitListening(t, recorder)
		address = ready.Address
		if ready.GraphConnectivity != "not_checked" || ready.ModelExecution != "not_invoked" || ready.CaidoIntake != "not_started" {
			t.Fatalf("listener readiness overstated external readiness: %+v", ready)
		}
		client := http.Client{Timeout: time.Second}
		response, err := client.Get("http://" + address + "/")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || string(body) != "synthetic application" {
			t.Fatalf("HTTP response=%d %q", response.StatusCode, body)
		}
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("managed web did not shut down")
		}
		select {
		case <-closed:
		default:
			t.Fatal("application services were not closed")
		}
		if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
			connection.Close()
			t.Fatal("listener survived shutdown")
		}
		if !strings.Contains(recorder.String(), `"state":"stopped"`) {
			t.Fatal("application omitted final lifecycle state")
		}
	}
}

func TestWorkerFailureCancelsPeerAndReleasesItsProjectLease(t *testing.T) {
	t.Setenv("BB_ASSISTANT_GRAPH_BACKEND", "neo4j")
	t.Setenv("BB_ASSISTANT_NEO4J_URI", "bolt://127.0.0.1:1")
	t.Setenv("BB_ASSISTANT_NEO4J_USERNAME", "synthetic")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "synthetic-secret-must-not-be-logged")
	t.Setenv("BB_ASSISTANT_NEO4J_DATABASE", "neo4j")
	opts := DefaultOptions()
	opts.DataRoot = t.TempDir()
	directories := map[string]string{}
	for _, id := range []string{"first", "second"} {
		selected, err := store.CreateProject(t.Context(), project.CaidoProjectRef{InstanceID: "managed-fixture", ProjectID: id}, opts.DataRoot)
		if err != nil {
			t.Fatal(err)
		}
		opts.ProjectKeys = append(opts.ProjectKeys, selected.ProjectKey)
		directories[selected.ProjectKey] = selected.Directory
		if err := selected.Close(); err != nil {
			t.Fatal(err)
		}
	}
	active, released := make(chan struct{}), make(chan struct{})
	recorder := newEventRecorder()
	deps := dependencies{
		listen: func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") },
		handler: func(Options) (http.Handler, func() error, error) {
			return http.NotFoundHandler(), func() error { return nil }, nil
		},
		worker: func(ctx context.Context, _ Options, selected selectedProject, _ func(Event)) error {
			if selected.key == opts.ProjectKeys[0] {
				lease, err := projectlock.Acquire(ctx, selected.directory)
				if err != nil {
					return err
				}
				defer close(released)
				defer lease.Close()
				close(active)
				<-ctx.Done()
				return ctx.Err()
			}
			select {
			case <-active:
				return errors.New("synthetic-secret-must-not-be-logged")
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := run(ctx, opts, recorder, deps)
	if err == nil || !strings.Contains(err.Error(), "projection component stopped unexpectedly") {
		t.Fatalf("component failure result=%v", err)
	}
	if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(recorder.String(), "synthetic-secret") {
		t.Fatal("component failure exposed credential-bearing error text")
	}
	select {
	case <-released:
	default:
		t.Fatal("peer worker was not joined before return")
	}
	lease, err := projectlock.Acquire(ctx, directories[opts.ProjectKeys[0]])
	if err != nil {
		t.Fatalf("peer retained project maintenance lease: %v", err)
	}
	lease.Close()
}

func TestBindFailureClosesServicesAndStartsNoWorker(t *testing.T) {
	closed := false
	opts := DefaultOptions()
	err := run(t.Context(), opts, io.Discard, dependencies{
		listen: func(string, string) (net.Listener, error) { return nil, errors.New("synthetic bind failure") },
		handler: func(Options) (http.Handler, func() error, error) {
			return http.NotFoundHandler(), func() error { closed = true; return nil }, nil
		},
		worker: func(context.Context, Options, selectedProject, func(Event)) error {
			t.Error("worker started before listener bind")
			return nil
		},
	})
	if err == nil || !closed {
		t.Fatalf("bind failure cleanup: err=%v closed=%t", err, closed)
	}
}
