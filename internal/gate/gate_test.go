package gate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name              string
		probes            []Probe
		want              Verdict
		failures, skipped int
	}{
		{"all OK", []Probe{{Name: "data", Critical: true, Check: func(context.Context) error { return nil }}}, Valid, 0, 0},
		{"optional failure", []Probe{{Name: "llm", Check: func(context.Context) error { return errors.New("offline") }}}, Degraded, 1, 0},
		{"critical failure", []Probe{{Name: "recon", Critical: true, Check: func(context.Context) error { return errors.New("missing data") }}}, Invalid, 1, 0},
		{"critical dominates", []Probe{{Name: "llm", Check: func(context.Context) error { return errors.New("offline") }}, {Name: "target", Critical: true, Check: func(context.Context) error { return errors.New("offline") }}}, Invalid, 2, 0},
		{"skipped optional", []Probe{{Name: "llm", Check: func(context.Context) error { return Skip("not configured") }}}, Valid, 0, 1},
		{"critical cannot skip", []Probe{{Name: "target", Critical: true, Check: func(context.Context) error { return Skip("not configured") }}}, Invalid, 1, 0},
		{"nil check", []Probe{{Name: "recon", Critical: true}}, Invalid, 1, 0},
		{"empty gate", nil, Invalid, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			got := New(tc.probes...).Check(context.Background())
			if got.Verdict != tc.want || len(got.Failures) != tc.failures || len(got.Skipped) != tc.skipped {
				t.Fatalf("result = %+v", got)
			}
			if got.CheckedAt.Before(before) || got.CheckedAt.After(time.Now()) {
				t.Fatalf("invalid checked_at: %s", got.CheckedAt)
			}
		})
	}
}

func TestCheckCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := New(Probe{Name: "target", Critical: true, Check: func(ctx context.Context) error { return ctx.Err() }}).Check(ctx)
	if got.Verdict != Invalid {
		t.Fatalf("result = %+v", got)
	}
}
