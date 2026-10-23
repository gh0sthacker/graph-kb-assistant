//go:build !linux

package projectlock

import (
	"context"
	"time"
)

func acquire(ctx context.Context, _ string, _ time.Duration) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

func acquireRead(ctx context.Context, _ string, _ time.Duration) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}
