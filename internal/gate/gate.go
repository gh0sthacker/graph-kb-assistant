// Package gate checks that usable data exists before and after network stages.
// A valid verdict is a readiness check, not proof of complete hunting coverage.
package gate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Verdict string

const (
	Valid    Verdict = "valid"
	Degraded Verdict = "degraded"
	Invalid  Verdict = "invalid"
)

type Probe struct {
	Name     string
	Critical bool
	Check    func(context.Context) error
}

type Result struct {
	Verdict   Verdict           `json:"verdict"`
	Failures  map[string]string `json:"failures"`
	CheckedAt time.Time         `json:"checked_at"`
	Skipped   map[string]string `json:"skipped,omitempty"`
}

type Gate struct{ probes []Probe }

// New copies the probe list; gates may be reused after each network stage.
func New(probes ...Probe) *Gate { return &Gate{probes: append([]Probe(nil), probes...)} }

type skippedError struct{ reason string }

func (e *skippedError) Error() string { return e.reason }

// Skip records an explicitly unconfigured optional dependency, not a failure.
// Critical probes are never allowed to skip.
func Skip(reason string) error { return &skippedError{reason: reason} }

func (g *Gate) Check(ctx context.Context) Result {
	result := Result{Verdict: Valid, Failures: map[string]string{}, CheckedAt: time.Now().UTC()}
	if g == nil || len(g.probes) == 0 {
		result.Verdict = Invalid
		result.Failures["gate"] = "no probes configured"
		return result
	}
	for i, p := range g.probes {
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("probe_%d", i+1)
		}
		var err error
		if p.Check == nil {
			err = errors.New("probe check is not configured")
		} else if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = p.Check(ctx)
		}
		if err == nil {
			continue
		}
		var skipped *skippedError
		if !p.Critical && errors.As(err, &skipped) {
			if result.Skipped == nil {
				result.Skipped = map[string]string{}
			}
			result.Skipped[name] = skipped.Error()
			continue
		}
		result.Failures[name] = err.Error()
		if p.Critical {
			result.Verdict = Invalid
		} else if result.Verdict == Valid {
			result.Verdict = Degraded
		}
	}
	return result
}
