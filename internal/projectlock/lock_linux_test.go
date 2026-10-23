//go:build linux

package projectlock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func takeLock(t *testing.T, dir string) *Lock {
	t.Helper()
	lock, err := Acquire(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	return lock
}

func TestAcquireReleaseAndPrivatePermissions(t *testing.T) {
	dir := privateDirectory(t)
	lock := takeLock(t, dir)
	info, err := os.Lstat(filepath.Join(dir, lockFileName))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file is not a private regular file: info=%v err=%v", info, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); err != nil {
		t.Fatalf("Close must retain the lock file: %v", err)
	}
	takeLock(t, dir)
	var nilLock *Lock
	if err := nilLock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentProjectsDoNotBlock(t *testing.T) {
	takeLock(t, privateDirectory(t))
	takeLock(t, privateDirectory(t))
}

func TestReadAndWriteLeasesExcludeEachOther(t *testing.T) {
	dir := privateDirectory(t)

	reader, err := AcquireRead(context.Background(), dir)
	if err != nil {
		t.Fatalf("acquire shared read lease: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	writer, writeErr := Acquire(ctx, dir)
	cancel()
	if writer != nil {
		_ = writer.Close()
		t.Fatal("writer acquired an exclusive lease while a reader held a shared lease")
	}
	if !errors.Is(writeErr, context.DeadlineExceeded) {
		t.Fatalf("writer did not wait for shared lease: %v", writeErr)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	writer = takeLock(t, dir)
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Millisecond)
	reader, readErr := AcquireRead(ctx, dir)
	cancel()
	if reader != nil {
		_ = reader.Close()
		t.Fatal("reader acquired a shared lease while a writer held an exclusive lease")
	}
	if !errors.Is(readErr, context.DeadlineExceeded) {
		t.Fatalf("reader did not wait for exclusive lease: %v", readErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadLeaseDoesNotCreateLockFiles(t *testing.T) {
	dir := privateDirectory(t)
	lease, err := AcquireRead(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := os.Lstat(filepath.Join(dir, lockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read lease created a maintenance lock file: %v", err)
	}
}

func TestLeaseCoverageRequiresActiveExclusiveLockWhenRequested(t *testing.T) {
	dir := privateDirectory(t)
	reader, err := AcquireRead(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.CoversDirectory(dir) || reader.CoversExclusiveDirectory(dir) {
		t.Fatal("shared lease coverage did not distinguish its lock mode")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if reader.CoversDirectory(dir) || reader.CoversExclusiveDirectory(dir) {
		t.Fatal("closed shared lease still reported directory coverage")
	}

	writer, err := Acquire(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !writer.CoversDirectory(dir) || !writer.CoversExclusiveDirectory(dir) {
		t.Fatal("active exclusive lease did not report directory coverage")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if writer.CoversDirectory(dir) || writer.CoversExclusiveDirectory(dir) {
		t.Fatal("closed exclusive lease still reported directory coverage")
	}
}

func TestAcquisitionCancellationAndWaitBound(t *testing.T) {
	dir := privateDirectory(t)
	takeLock(t, dir)
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(40*time.Millisecond, cancel)
		defer timer.Stop()
		defer cancel()
		start := time.Now()
		lock, err := Acquire(ctx, dir)
		if lock != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquisition: lock=%v err=%v", lock, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("canceled waiter was not released promptly")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		lock, err := Acquire(ctx, dir)
		if lock != nil || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrWaitTimeout) {
			t.Fatalf("caller deadline must be preserved: lock=%v err=%v", lock, err)
		}
	})
	t.Run("internal bound", func(t *testing.T) {
		start := time.Now()
		lock, err := acquire(context.Background(), dir, 40*time.Millisecond)
		if lock != nil || !errors.Is(err, ErrWaitTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("internal acquisition bound: lock=%v err=%v", lock, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("bounded waiter was not released promptly")
		}
	})
}

func TestWaitingAcquisitionProceedsAfterRelease(t *testing.T) {
	dir := privateDirectory(t)
	first := takeLock(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan *Lock, 1)
	failure := make(chan error, 1)
	go func() {
		lock, err := Acquire(ctx, dir)
		if err != nil {
			failure <- err
			return
		}
		result <- lock
	}()
	select {
	case lock := <-result:
		_ = lock.Close()
		t.Fatal("second lease acquired before release")
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case lock := <-result:
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	case err := <-failure:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("second lease did not acquire after release")
	}
}

func TestCancelAfterAcquisitionDoesNotReleaseLease(t *testing.T) {
	dir := privateDirectory(t)
	ctx, cancel := context.WithCancel(context.Background())
	first, err := Acquire(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	cancel()
	second, err := acquire(context.Background(), dir, 40*time.Millisecond)
	if second != nil || !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("holder context cancellation released lease: lock=%v err=%v", second, err)
	}
}

func TestRejectInvalidProjectDirectories(t *testing.T) {
	t.Run("invalid paths", func(t *testing.T) {
		dir := privateDirectory(t)
		for _, path := range []string{"", ".", dir + "/.", filepath.Join(dir, "missing")} {
			lock, err := Acquire(context.Background(), path)
			if lock != nil || !errors.Is(err, ErrInvalidDirectory) {
				t.Fatalf("accepted invalid directory %q: lock=%v err=%v", path, lock, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("acquisition created a missing project directory")
		}
	})
	t.Run("public directory", func(t *testing.T) {
		dir := privateDirectory(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Acquire(context.Background(), dir); !errors.Is(err, ErrInvalidDirectory) {
			t.Fatalf("accepted public directory: %v", err)
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatal("acquisition silently changed directory permissions")
		}
	})
	t.Run("symlink and ancestor symlink", func(t *testing.T) {
		root := privateDirectory(t)
		dir := filepath.Join(root, "project")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(dir, alias); err != nil {
			t.Fatal(err)
		}
		ancestor := filepath.Join(root, "ancestor")
		if err := os.Symlink(root, ancestor); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{alias, filepath.Join(ancestor, "project")} {
			if _, err := Acquire(context.Background(), path); !errors.Is(err, ErrInvalidDirectory) {
				t.Fatalf("accepted symlink path: %v", err)
			}
		}
	})
}

func TestRejectInvalidLockFiles(t *testing.T) {
	for _, kind := range []string{"public", "read-only", "symlink", "hardlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateDirectory(t)
			path := filepath.Join(dir, lockFileName)
			var err error
			switch kind {
			case "public":
				err = os.WriteFile(path, nil, 0o644)
				if err == nil {
					err = os.Chmod(path, 0o644)
				}
			case "read-only":
				err = os.WriteFile(path, nil, 0o400)
			case "symlink", "hardlink":
				target := filepath.Join(dir, "unrelated")
				err = os.WriteFile(target, []byte("untouched"), 0o600)
				if err == nil && kind == "symlink" {
					err = os.Symlink(target, path)
				} else if err == nil {
					err = os.Link(target, path)
				}
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := Acquire(context.Background(), dir); lock != nil || !errors.Is(err, ErrInvalidLockFile) {
				t.Fatalf("accepted %s lock file: lock=%v err=%v", kind, lock, err)
			}
			if kind == "symlink" || kind == "hardlink" {
				data, err := os.ReadFile(filepath.Join(dir, "unrelated"))
				if err != nil || string(data) != "untouched" {
					t.Fatal("acquisition modified an unrelated linked file")
				}
			}
		})
	}
}

func TestLocationRevalidationRejectsReplacement(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := privateDirectory(t)
			dir := filepath.Join(root, "project")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			lock := takeLock(t, dir)
			defer lock.Close()
			fd, directoryStat, err := openDirectory(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			var fileStat syscall.Stat_t
			path := filepath.Join(dir, lockFileName)
			if err := syscall.Stat(path, &fileStat); err != nil {
				t.Fatal(err)
			}
			var want error
			if kind == "file" {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				want = ErrInvalidLockFile
			} else {
				if err := os.Rename(dir, dir+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				want = ErrInvalidDirectory
			}
			if err := validateLocation(dir, fd, directoryStat, fileStat); !errors.Is(err, want) {
				t.Fatalf("replacement not rejected: %v", err)
			}
		})
	}
}

func TestFilesystemPolicyFailsClosed(t *testing.T) {
	for _, magic := range []uint64{0x6969, 0xff534d42, 0x517b, 0x65735546, 0x01021997, 0} {
		if supportedFilesystem(magic) {
			t.Fatalf("accepted network or unknown filesystem type %x", magic)
		}
	}
}

func TestCrossProcessExclusionAndRelease(t *testing.T) {
	for _, mode := range []string{"close", "process exit"} {
		t.Run(mode, func(t *testing.T) {
			dir := privateDirectory(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProjectLockProcessHelper$")
			cmd.Env = []string{"BB_PROJECTLOCK_HELPER=1", "BB_PROJECTLOCK_DIRECTORY=" + dir}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			ready := make(chan string, 1)
			go func() {
				line, _ := bufio.NewReader(stdout).ReadString('\n')
				ready <- line
			}()
			select {
			case line := <-ready:
				if line != "locked\n" {
					t.Fatalf("helper did not acquire lock: %q", line)
				}
			case <-ctx.Done():
				t.Fatal("helper did not signal readiness")
			}
			contender, err := acquire(context.Background(), dir, 60*time.Millisecond)
			if contender != nil || !errors.Is(err, ErrWaitTimeout) {
				t.Fatalf("another process's lease did not exclude parent: lock=%v err=%v", contender, err)
			}
			if mode == "close" {
				if _, err := fmt.Fprintln(stdin, "release"); err != nil {
					t.Fatal(err)
				}
				_ = stdin.Close()
				if err := cmd.Wait(); err != nil {
					t.Fatalf("helper close failed: %v: %s", err, stderr.String())
				}
			} else {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err == nil {
					t.Fatal("killed helper unexpectedly exited successfully")
				}
			}
			waited = true
			takeLock(t, dir)
		})
	}
}

func TestProjectLockProcessHelper(t *testing.T) {
	if os.Getenv("BB_PROJECTLOCK_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	lock, err := Acquire(ctx, os.Getenv("BB_PROJECTLOCK_DIRECTORY"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Println("locked")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
