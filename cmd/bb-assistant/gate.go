package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"bb-graph-assistant/internal/gate"
)

type gateVerdictError struct{ verdict gate.Verdict }

func (e *gateVerdictError) Error() string { return "gate: " + string(e.verdict) }

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var outcome *gateVerdictError
	if errors.As(err, &outcome) && outcome.verdict == gate.Invalid {
		return 3
	}
	return 2
}

// runGate makes no persistent writes and never treats missing critical
// configuration as a skipped probe. Tokens are read from environment only.
func runGate(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("gate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	target := flags.String("target", "", "authorized target origin")
	caidoURL := flags.String("caido-url", os.Getenv("CAIDO_URL"), "Caido base URL (token: CAIDO_ACCESS_TOKEN)")
	reconDir := flags.String("recon-dir", "", "directory with routes.json and input-points.json")
	jsonOutput := flags.Bool("json", false, "emit a JSON verdict")
	minRoutes := flags.Int("min-routes", 20, "minimum usable recon routes")
	minPoints := flags.Int("min-input-points", 5, "minimum recon input points")
	timeout := flags.Duration("timeout", 30*time.Second, "total gate deadline")
	llmURL := flags.String("llm-url", gateEnv("BB_ASSISTANT_LLM_URL", "OPENAI_BASE_URL"), "optional OpenAI-compatible base URL including /v1")
	llmModel := flags.String("llm-model", gateEnv("BB_ASSISTANT_LLM_MODEL", "OPENAI_MODEL"), "optional cheap completion model")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 || *minRoutes < 1 || *minPoints < 1 {
		return errors.New("usage: bb-assistant gate --target URL --caido-url URL --recon-dir DIR [--json]; thresholds and timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result := gate.New(
		gate.TargetProbe(gate.TargetConfig{Origin: *target}),
		gate.CaidoProbe(gate.CaidoConfig{URL: *caidoURL, Token: os.Getenv("CAIDO_ACCESS_TOKEN")}),
		gate.LLMProbe(gate.LLMConfig{URL: *llmURL, Model: *llmModel, APIKey: gateEnv("BB_ASSISTANT_LLM_API_KEY", "OPENAI_API_KEY")}),
		gate.ReconProbe(gate.ReconConfig{Dir: *reconDir, MinRoutes: *minRoutes, MinInputPoints: *minPoints}),
	).Check(ctx)
	if *jsonOutput {
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(out, "gate: %s\nchecked_at: %s\n", result.Verdict, result.CheckedAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		for _, group := range []struct {
			status string
			items  map[string]string
		}{{"failed", result.Failures}, {"skipped", result.Skipped}} {
			names := make([]string, 0, len(group.items))
			for name := range group.items {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if _, err := fmt.Fprintf(out, "  %s: %s (%s)\n", name, group.status, group.items[name]); err != nil {
					return err
				}
			}
		}
	}
	if result.Verdict != gate.Valid {
		return &gateVerdictError{verdict: result.Verdict}
	}
	return nil
}

func gateEnv(primary, fallback string) string {
	if value := os.Getenv(primary); value != "" {
		return value
	}
	return os.Getenv(fallback)
}
