//go:build !linux

package projectlock

import (
	"context"
	"errors"
	"testing"
)

func TestUnsupportedPlatformFailsExplicitly(t *testing.T) {
	lock, err := Acquire(context.Background(), t.TempDir())
	if lock != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported platform must fail explicitly: lock=%v err=%v", lock, err)
	}
}
