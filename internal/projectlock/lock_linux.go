//go:build linux

package projectlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const lockFileName = ".maintenance.lock"
const retryInterval = 25 * time.Millisecond

func acquire(ctx context.Context, projectDir string, maxWait time.Duration) (*Lock, error) {
	return acquireWithMode(ctx, projectDir, maxWait, true)
}

func acquireRead(ctx context.Context, projectDir string, maxWait time.Duration) (*Lock, error) {
	return acquireWithMode(ctx, projectDir, maxWait, false)
}

func acquireWithMode(ctx context.Context, projectDir string, maxWait time.Duration, exclusive bool) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	directory, directoryStat, err := openDirectory(projectDir)
	if err != nil {
		return nil, err
	}
	directoryOwned := false
	defer func() {
		if !directoryOwned {
			_ = syscall.Close(directory)
		}
	}()
	directoryLockMode := syscall.LOCK_SH
	if exclusive {
		directoryLockMode = syscall.LOCK_EX
	}
	for {
		if err := waitError(ctx, waitCtx); err != nil {
			return nil, err
		}
		err := syscall.Flock(directory, directoryLockMode|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("project maintenance directory lease could not be acquired: %w", err)
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, waitError(ctx, waitCtx)
		case <-timer.C:
		}
	}
	if !exclusive {
		if err := validateDirectoryLocation(projectDir, directory, directoryStat); err != nil {
			return nil, err
		}
		if err := waitError(ctx, waitCtx); err != nil {
			return nil, err
		}
		directoryOwned = true
		return &Lock{directory: projectDir, exclusive: false, release: func() error {
			unlockErr := syscall.Flock(directory, syscall.LOCK_UN)
			closeErr := syscall.Close(directory)
			if unlockErr != nil || closeErr != nil {
				return errors.New("project read lease could not be released cleanly")
			}
			return nil
		}}, nil
	}
	fd, err := syscall.Openat(directory, lockFileName,
		syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: could not open lock file", ErrInvalidLockFile)
	}
	file := os.NewFile(uintptr(fd), lockFileName)
	owned := false
	defer func() {
		if !owned {
			_ = file.Close()
		}
	}()
	fileStat, err := validateFile(fd)
	if err != nil {
		return nil, err
	}
	for {
		if err := waitError(ctx, waitCtx); err != nil {
			return nil, err
		}
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Recheck after waiting so a renamed directory or replaced lock file
			// cannot grant a lease for a stale project path or inode.
			if err := validateLocation(projectDir, directory, directoryStat, fileStat); err != nil {
				return nil, err // Closing the descriptor also releases flock.
			}
			if err := waitError(ctx, waitCtx); err != nil {
				return nil, err
			}
			owned = true
			directoryOwned = true
			return &Lock{directory: projectDir, exclusive: true, release: func() error {
				unlockErr := syscall.Flock(fd, syscall.LOCK_UN)
				closeErr := file.Close()
				directoryUnlockErr := syscall.Flock(directory, syscall.LOCK_UN)
				directoryCloseErr := syscall.Close(directory)
				if unlockErr != nil || closeErr != nil || directoryUnlockErr != nil || directoryCloseErr != nil {
					return errors.New("project maintenance lock could not be released cleanly")
				}
				return nil
			}}, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("project maintenance lock could not be acquired: %w", err)
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, waitError(ctx, waitCtx)
		case <-timer.C:
		}
	}
}

func validateDirectoryLocation(path string, directory int, expected syscall.Stat_t) error {
	current, stat, err := openDirectory(path)
	if err != nil {
		return err
	}
	_ = syscall.Close(current)
	if stat.Dev != expected.Dev || stat.Ino != expected.Ino {
		return ErrInvalidDirectory
	}
	return nil
}

func waitError(ctx, waitCtx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if waitCtx.Err() != nil {
		return errors.Join(ErrWaitTimeout, context.DeadlineExceeded)
	}
	return nil
}

// Walk each component without following symlinks, then retain the final
// descriptor for openat. No caller-controlled lock filename is accepted.
func openDirectory(path string) (int, syscall.Stat_t, error) {
	var stat syscall.Stat_t
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, stat, ErrInvalidDirectory
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, stat, ErrInvalidDirectory
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := syscall.Openat(fd, part,
			syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(fd)
		if openErr != nil {
			return -1, stat, ErrInvalidDirectory
		}
		fd = next
	}
	if err := syscall.Fstat(fd, &stat); err != nil ||
		stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Mode&0o7777 != 0o700 || stat.Uid != uint32(os.Geteuid()) {
		_ = syscall.Close(fd)
		return -1, stat, ErrInvalidDirectory
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Fstatfs(fd, &filesystem); err != nil || !supportedFilesystem(uint64(uint32(filesystem.Type))) {
		_ = syscall.Close(fd)
		return -1, stat, ErrUnsupportedFilesystem
	}
	return fd, stat, nil
}

func validateFile(fd int) (syscall.Stat_t, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil ||
		stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0o7777 != 0o600 ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return stat, ErrInvalidLockFile
	}
	return stat, nil
}

func validateLocation(path string, directory int, expectedDir, expectedFile syscall.Stat_t) error {
	current, stat, err := openDirectory(path)
	if err != nil {
		return err
	}
	_ = syscall.Close(current)
	if stat.Dev != expectedDir.Dev || stat.Ino != expectedDir.Ino {
		return ErrInvalidDirectory
	}
	fd, err := syscall.Openat(directory, lockFileName,
		syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrInvalidLockFile
	}
	defer syscall.Close(fd)
	stat, err = validateFile(fd)
	if err != nil {
		return err
	}
	if stat.Dev != expectedFile.Dev || stat.Ino != expectedFile.Ino {
		return ErrInvalidLockFile
	}
	return nil
}

// Linux filesystem magic values from include/uapi/linux/magic.h. Only local
// filesystems with normal flock semantics are admitted. Network and unknown
// filesystem types fail closed; FUSE is omitted because it can be remote.
func supportedFilesystem(magic uint64) bool {
	switch magic {
	case 0xef53, // ext2/ext3/ext4
		0x58465342, // XFS
		0x9123683e, // Btrfs
		0x01021994, // tmpfs
		0x858458f6, // ramfs
		0x794c7630, // overlayfs
		0xf2f52010, // F2FS
		0x2fc12fc1, // ZFS
		0x3153464a, // JFS
		0x52654973, // ReiserFS
		0x3434:     // NILFS
		return true
	default:
		return false
	}
}
