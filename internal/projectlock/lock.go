// Package projectlock coordinates project graph reads and maintenance across
// processes. All graph mutation and projection drain boundaries must take the
// exclusive lock; readers hold a shared lease for their complete read.
package projectlock

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"
)

// DefaultWaitTimeout caps acquisition, even when the caller has no deadline.
const DefaultWaitTimeout = 30 * time.Second

var (
	ErrInvalidDirectory      = errors.New("project maintenance requires an existing private canonical directory")
	ErrInvalidLockFile       = errors.New("project maintenance lock must be a private regular file with one link")
	ErrUnsupportedFilesystem = errors.New("project maintenance requires a supported local filesystem")
	ErrWaitTimeout           = errors.New("project maintenance lock wait limit exceeded")
	ErrUnsupported           = errors.New("project maintenance locking is supported on Linux only")
)

// Lock is an exclusive project maintenance lease. It must not be copied.
type Lock struct {
	mu        sync.Mutex
	directory string
	release   func() error
}

// CoversDirectory reports whether this lease was acquired for the given
// directory. Callers that open a snapshot under an existing exclusive lease
// use it to prevent accidentally bypassing coordination with a lease for a
// different project.
func (l *Lock) CoversDirectory(path string) bool {
	if l == nil || path == "" {
		return false
	}
	absolute, err := filepath.Abs(path)
	return err == nil && l.directory == filepath.Clean(absolute)
}

// Acquire waits for exclusive access to projectDir for at most
// DefaultWaitTimeout, or until ctx is canceled. The directory must be an
// absolute canonical path to an existing owner-only local project directory.
// The caller must also validate that it belongs to the selected project.
//
// Acquisition is not reentrant, including within one process. Acquire before
// opening transactions or taking other graph locks; close after the complete
// snapshot read/replace/readback or graph mutation/drain operation. Never hold
// this lock over a live-tail polling sleep or recursively acquire in a writer.
// Canceling ctx after acquisition does not release the lock; always call Close.
//
// Writers lock the project directory inode exclusively and then the retained
// .maintenance.lock protocol file. The file must never be removed or replaced,
// even when unlocked. Removing it can allow simultaneous locks on different
// inodes. Platforms other than Linux fail explicitly with ErrUnsupported.
func Acquire(ctx context.Context, projectDir string) (*Lock, error) {
	if ctx == nil {
		return nil, errors.New("project maintenance lock requires a context")
	}
	return acquire(ctx, projectDir, DefaultWaitTimeout)
}

// AcquireRead obtains a shared lease on an existing private project directory.
// It does not create a lock file. Exclusive maintenance leases block until all
// shared readers close, and shared readers block while maintenance is active.
// The caller must hold the returned lease for the complete read operation.
func AcquireRead(ctx context.Context, projectDir string) (*Lock, error) {
	if ctx == nil {
		return nil, errors.New("project read lease requires a context")
	}
	return acquireRead(ctx, projectDir, DefaultWaitTimeout)
}

// Close releases the lease. It is safe to call more than once and concurrently.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.release == nil {
		return nil
	}
	release := l.release
	l.release = nil
	return release()
}
