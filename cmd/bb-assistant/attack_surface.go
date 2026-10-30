package main

import (
	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
)

func showAttackSurface(args []string) error {
	f := flag.NewFlagSet("attack-surface", flag.ContinueOnError)
	key := f.String("project-key", "", "registered project key (required)")
	classes := f.String("classes", "", "comma-separated vulnerability classes")
	limit := f.Int("limit", 50, "maximum candidates (1-1000)")
	asJSON := f.Bool("json", false, "emit JSON")
	root := f.String("data-root", "", "private data root")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 || *limit < 1 {
		return errors.New("usage: attack-surface --project-key KEY [--classes a,b] [--limit N] [--json]")
	}
	req := appquery.AttackSurfaceRequest{ProjectKey: *key, Classes: appquery.ParseAttackClasses(*classes), Limit: *limit}
	if e := appquery.ValidateAttackSurface(req); e != nil {
		return e
	}
	if *root == "" {
		v, e := project.DataRoot()
		if e != nil {
			return e
		}
		*root = v
	}
	refs, e := project.LoadProjectRefsReadOnly(*root)
	if e != nil {
		return e
	}
	svc, e := appquery.NewGraphQueryService(*root, refs, nil)
	if e != nil {
		return e
	}
	defer svc.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, e := svc.AttackSurface(ctx, req)
	if e != nil {
		return e
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Candidates: %d (showing %d), endpoints: %d, parameters: %d, sinks: %d\nSCORE\tCLASS\tENDPOINT\tPARAMETER\tSINK\tWHY\n", result.Stats.Candidates, len(result.Candidates), result.Stats.Endpoints, result.Stats.Parameters, result.Stats.Sinks)
	for _, c := range result.Candidates {
		why, _ := json.Marshal(c.Why)
		fmt.Fprintf(w, "%.4f\t%s\t%s\t%s:%s\t%s/%s\t%s\n", c.Score, c.Class, c.Endpoint, c.Location, c.Parameter, c.Sink.Kind, c.Sink.Engine, why)
	}
	for _, gap := range result.CoverageGaps {
		fmt.Fprintf(w, "Coverage gap: %s\n", gap)
	}
	return w.Flush()
}

// Explicit cache-only migration for legacy live imports. The capture source
// adapter exposes only list/get/select, never a replay/send/scan operation.
func backfillHunt(args []string) error {
	f := flag.NewFlagSet("hunt-backfill", flag.ContinueOnError)
	key := f.String("project-key", "", "registered project key")
	root := f.String("data-root", "", "private data root (use an authorized copy)")
	confirmed := f.Bool("read-existing-caido", false, "allow read-only retrieval of already-imported IDs from Caido")
	if e := f.Parse(args); e != nil {
		return e
	}
	if !*confirmed || f.NArg() != 0 {
		return errors.New("hunt-backfill requires --read-existing-caido; writes only hunt.sqlite3")
	}
	if e := appquery.ValidateAttackSurface(appquery.AttackSurfaceRequest{ProjectKey: *key}); e != nil {
		return e
	}
	if *root == "" {
		v, e := project.DataRoot()
		if e != nil {
			return e
		}
		*root = v
	}
	refs, e := project.LoadProjectRefsReadOnly(*root)
	if e != nil {
		return e
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		k, _ := ref.StorageKey()
		if k == *key {
			v := ref
			selected = &v
			break
		}
	}
	if selected == nil {
		return errors.New("project is not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s, e := store.OpenProjectReadOnly(ctx, *selected, *root)
	if e != nil {
		return e
	}
	defer s.Close()
	process, e := caido.StartMCPProcess(caido.MCPProcessConfig{Context: ctx})
	if e != nil {
		return e
	}
	defer process.Close()
	source, e := caido.NewMCPReadSource(process, selected.InstanceID)
	if e != nil {
		return e
	}
	result, e := ingest.BackfillHunt(ctx, s, source)
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
