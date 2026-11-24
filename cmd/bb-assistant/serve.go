package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"bb-graph-assistant/internal/managedapp"
)

func serveApplication(args []string) error {
	options := managedapp.DefaultOptions()
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var keys stringListFlag
	flags.Var(&keys, "project-key", "registered project to project continuously (repeat for up to 16 projects; omit for web only)")
	flags.StringVar(&options.DataRoot, "data-root", "", "private assistant data root")
	flags.StringVar(&options.KBIndex, "kb-index", "", "existing optional local KB index")
	flags.StringVar(&options.Address, "addr", options.Address, "loopback address for the local application")
	flags.DurationVar(&options.PollInterval, "poll-interval", options.PollInterval, "delay between projection batches (100ms-10m)")
	flags.DurationVar(&options.LeaseDuration, "lease-duration", options.LeaseDuration, "per-job lease and execution limit (30s-30m)")
	flags.IntVar(&options.MaxJobs, "max-jobs", options.MaxJobs, "maximum jobs per project per batch (1-100)")
	flags.IntVar(&options.MaxAttempts, "max-attempts", options.MaxAttempts, "maximum delivery attempts before dead-letter (1-100)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: bb-assistant serve [--project-key sha256 ...] [--data-root path] [--kb-index path] [--addr 127.0.0.1:8765] [--poll-interval 1s] [--max-jobs 10] [--lease-duration 10m] [--max-attempts 8]")
	}
	options.ProjectKeys = keys
	if err := options.Validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return managedapp.Run(ctx, options, os.Stdout)
}
