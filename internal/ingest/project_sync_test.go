package ingest

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
)

type inventorySource struct {
	calls atomic.Int32
}

func (s *inventorySource) ListProjects() ([]caido.ProjectRef, error) {
	call := s.calls.Add(1)
	if call == 1 {
		return nil, errors.New("synthetic inventory outage")
	}
	return []caido.ProjectRef{}, nil
}
func (*inventorySource) ListRequestPage(project.CaidoProjectRef, *string, int, *string) (caido.RequestMetadataPage, error) {
	return caido.RequestMetadataPage{}, nil
}
func (*inventorySource) FetchHistoryPage(project.CaidoProjectRef, caido.RequestMetadataPage, map[string]struct{}, int) (caido.HistoryPage, error) {
	return caido.HistoryPage{}, nil
}
func (*inventorySource) GetExchange(caido.ExchangeRef) (*caido.HttpExchange, error) {
	return nil, nil
}

func TestProjectInventorySyncReportsAbsenceWithoutDeletingHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := project.CaidoProjectRef{InstanceID: "caido", ProjectID: "retain-history"}
	if err := project.RegisterProject(ref, root); err != nil {
		t.Fatal(err)
	}
	source := &inventorySource{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var errorEvents atomic.Int32
	var missingCalls atomic.Int32
	absent := make(chan struct{}, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- RunProjectInventorySync(ctx, source, ProjectInventorySyncOptions{
			DataRoot: root, InstanceID: "caido", PollInterval: time.Second,
			MissingConfirmations: 3,
			OnProject:            func(context.Context, project.CaidoProjectRef) error { return nil },
			OnProjectAbsent: func(_ context.Context, missing project.CaidoProjectRef) error {
				if !missing.SameProject(ref) {
					t.Errorf("reported absent project = %+v, want %+v", missing, ref)
				}
				missingCalls.Add(1)
				absent <- struct{}{}
				cancel()
				return nil
			},
			OnError: func(event string, _ *project.CaidoProjectRef, _ error) {
				if event == "inventory_failed" {
					errorEvents.Add(1)
				}
			},
		})
	}()
	select {
	case <-absent:
	case <-time.After(8 * time.Second):
		t.Fatal("project absence was not reported after three successful omissions")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("inventory sync did not stop after cancellation")
	}
	refs, err := project.LoadProjectRefs(root)
	if err != nil || len(refs) != 1 || !refs[0].SameProject(ref) {
		t.Fatalf("confirmed Caido absence deleted local history: refs=%v err=%v", refs, err)
	}
	if missingCalls.Load() != 1 || errorEvents.Load() != 1 || source.calls.Load() < 4 {
		t.Fatalf("inventory outage accounting: errors=%d successful-checks=%d", errorEvents.Load(), source.calls.Load()-1)
	}
}
