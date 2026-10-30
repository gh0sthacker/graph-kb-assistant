package main

import (
	"strings"
	"testing"
)

func TestAttackSurfaceCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"--project-key", "bad"}, {"--project-key", strings.Repeat("a", 64), "--limit", "-1"}, {"--project-key", strings.Repeat("a", 64), "--classes", "made-up"}} {
		if e := showAttackSurface(args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
