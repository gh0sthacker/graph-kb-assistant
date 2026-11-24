package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"bb-graph-assistant/internal/coverage"
	"bb-graph-assistant/internal/project"
)

func showCoverage(args []string) error {
	flags := flag.NewFlagSet("coverage", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	run := flags.String("run", "", "coverage run ID (required)")
	asJSON := flags.Bool("json", false, "emit the coverage matrix as JSON")
	root := flags.String("data-root", "", "private assistant data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*run) == "" {
		return errors.New("usage: bb-assistant coverage --run ID [--json] [--data-root path]")
	}
	if *root == "" {
		resolved, err := project.DataRoot()
		if err != nil {
			return err
		}
		*root = resolved
	}
	store, err := coverage.OpenReadOnly(filepath.Join(*root, coverage.DatabaseName))
	if err != nil {
		return fmt.Errorf("coverage database is unavailable: %w", err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	matrix, err := store.Load(ctx, *run)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(matrix)
	}
	canFinish, open := matrix.CanFinish()
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(writer, "run: %s\ncoverage_pct: %.2f\ncan_finish: %t\nCLASS\tSTATUS\tTESTS\tBUDGET\tEVIDENCE\n", matrix.Run, matrix.Pct(), canFinish); err != nil {
		return err
	}
	classes := make([]string, 0, len(matrix.Cells))
	for class := range matrix.Cells {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		cell := matrix.Cells[class]
		evidence, err := json.Marshal(cell.Evidence)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%d\t%d\t%s\n", cell.Class, cell.Status, cell.Tests, cell.Budget, evidence); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "open: %s\n", strings.Join(open, ", ")); err != nil {
		return err
	}
	if !canFinish {
		if _, err := fmt.Fprintln(writer, "Coverage incomplete: do not claim that there are no vulnerabilities."); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer, "Budget completion is not proof of absence; blocked and untested classes remain coverage gaps."); err != nil {
		return err
	}
	return writer.Flush()
}
