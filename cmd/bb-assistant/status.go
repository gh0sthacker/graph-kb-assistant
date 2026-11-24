package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"bb-graph-assistant/internal/runtimeaudit"
)

func runtimeStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataRoot := flags.String("data-root", "", "private assistant data root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant status [--data-root path]")
	}
	if err := requireConfiguredProjectRegistry(*dataRoot); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	report, err := runtimeaudit.Build(ctx, *dataRoot, runtimeaudit.ConfiguredGraphBackend(), time.Now().UTC())
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return errors.New("runtime status could not be written")
	}
	return nil
}
