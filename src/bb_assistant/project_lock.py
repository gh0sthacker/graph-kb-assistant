"""Linux project maintenance leases shared with internal/projectlock in Go.

Writers flock the project-directory inode exclusively and also retain the
.maintenance.lock file for protocol validation; never unlink or replace that
file. Read-only snapshots take a shared directory-inode lease and create no
coordination files. The writer lease is not reentrant.
"""
from __future__ import annotations

from contextlib import contextmanager
import errno
import math
import os
from pathlib import Path
import stat
import sys
from threading import Event
import time
from typing import Iterator

try:
    import fcntl
except ImportError:  # Unsupported systems must fail at acquisition explicitly.
    fcntl = None

from .paths import _mount_for_path


DEFAULT_WAIT_TIMEOUT = 30.0
_RETRY_INTERVAL = 0.025
_LOCK_NAME = ".maintenance.lock"
_LOCAL_FILESYSTEMS = frozenset({
    "ext2", "ext3", "ext4", "xfs", "btrfs", "tmpfs", "ramfs", "overlay",
    "f2fs", "zfs", "jfs", "reiserfs", "nilfs2",
})


class ProjectLockError(Exception):
    """The maintenance lease could not be safely acquired."""


class ProjectLockTimeout(TimeoutError, ProjectLockError):
    """The bounded acquisition deadline expired."""


class ProjectLockCancelled(InterruptedError, ProjectLockError):
    """The caller canceled acquisition."""


def _check_wait(deadline: float, cancel_event: Event | None) -> None:
    if cancel_event is not None and cancel_event.is_set():
        raise ProjectLockCancelled("project maintenance lock wait canceled")
    if time.monotonic() >= deadline:
        raise ProjectLockTimeout("project maintenance lock wait limit exceeded")


def _acquire_flock(fd: int, operation: int, deadline: float, cancel_event: Event | None) -> None:
    while True:
        _check_wait(deadline, cancel_event)
        try:
            fcntl.flock(fd, operation | fcntl.LOCK_NB)
            return
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            if exc.errno not in {errno.EAGAIN, errno.EWOULDBLOCK}:
                raise ProjectLockError("project maintenance lock could not be safely acquired") from None
        delay = min(_RETRY_INTERVAL, max(0, deadline - time.monotonic()))
        if cancel_event is None:
            time.sleep(delay)
        else:
            cancel_event.wait(delay)


def _open_directory(path: str) -> tuple[int, os.stat_result]:
    if not path or not os.path.isabs(path) or os.path.normpath(path) != path:
        raise ProjectLockError("project maintenance requires an existing private canonical directory")
    fd = None
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW
    try:
        fd = os.open("/", flags)
        for component in path.removeprefix("/").split("/"):
            if not component:
                continue
            next_fd = os.open(component, flags, dir_fd=fd)
            os.close(fd)
            fd = next_fd
        info = os.fstat(fd)
        if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.geteuid():
            raise ProjectLockError("project maintenance requires an owner-only directory")
        mount = _mount_for_path(Path(path))
        if mount is None or mount[0] not in _LOCAL_FILESYSTEMS:
            raise ProjectLockError("project maintenance requires a supported local filesystem")
        result = fd
        fd = None
        return result, info
    except OSError:
        raise ProjectLockError("project maintenance directory could not be safely opened") from None
    finally:
        if fd is not None:
            os.close(fd)


def _validate_file(fd: int) -> os.stat_result:
    info = os.fstat(fd)
    if (
        not stat.S_ISREG(info.st_mode)
        or stat.S_IMODE(info.st_mode) != 0o600
        or info.st_uid != os.geteuid()
        or info.st_nlink != 1
    ):
        raise ProjectLockError("project maintenance lock must be an owner-only regular file with one link")
    return info


def _validate_location(path: str, directory: int, expected_directory: os.stat_result, expected_file: os.stat_result) -> None:
    current, info = _open_directory(path)
    os.close(current)
    if (info.st_dev, info.st_ino) != (expected_directory.st_dev, expected_directory.st_ino):
        raise ProjectLockError("project maintenance directory changed while waiting")
    fd = os.open(_LOCK_NAME, os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    try:
        info = _validate_file(fd)
        if (info.st_dev, info.st_ino) != (expected_file.st_dev, expected_file.st_ino):
            raise ProjectLockError("project maintenance lock file changed while waiting")
    finally:
        os.close(fd)


@contextmanager
def project_maintenance_lock(
    project_dir: str | os.PathLike[str],
    *,
    timeout: float = DEFAULT_WAIT_TIMEOUT,
    cancel_event: Event | None = None,
) -> Iterator[None]:
    """Hold the same exclusive lease as Go's projectlock.Acquire.

    The caller validates the selected project's registry identity. This helper
    validates the existing canonical owner-only directory and the fixed lock
    file. Acquisition has a hard 30 second ceiling; a smaller timeout or an
    optional cancellation event can shorten the wait. KeyboardInterrupt also
    interrupts waiting. Cancellation after acquisition does not release the
    lease; leaving this context always releases it.
    """
    if sys.platform != "linux" or fcntl is None:
        raise ProjectLockError("project maintenance locking is supported on Linux only")
    if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or not 0 < timeout <= DEFAULT_WAIT_TIMEOUT:
        raise ValueError("project maintenance timeout must be finite, positive and at most 30 seconds")
    deadline = time.monotonic() + timeout
    _check_wait(deadline, cancel_event)
    directory, directory_info = _open_directory(os.fspath(project_dir))
    fd = None
    directory_locked = False
    acquired = False
    try:
        # Flock the directory inode as well as the retained protocol file. The
        # inode lease lets read-only users coordinate without creating files.
        _acquire_flock(directory, fcntl.LOCK_EX, deadline, cancel_event)
        directory_locked = True
        try:
            fd = os.open(
                _LOCK_NAME,
                os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
                0o600,
                dir_fd=directory,
            )
            file_info = _validate_file(fd)
            _acquire_flock(fd, fcntl.LOCK_EX, deadline, cancel_event)
            acquired = True
            _validate_location(os.fspath(project_dir), directory, directory_info, file_info)
            _check_wait(deadline, cancel_event)
        except ProjectLockError:
            raise
        except OSError:
            raise ProjectLockError("project maintenance lock could not be safely acquired") from None
        yield
    finally:
        try:
            if fd is not None:
                try:
                    if acquired:
                        fcntl.flock(fd, fcntl.LOCK_UN)
                finally:
                    os.close(fd)
        finally:
            if directory_locked:
                fcntl.flock(directory, fcntl.LOCK_UN)
            os.close(directory)


@contextmanager
def project_maintenance_read_lock(
    project_dir: str | os.PathLike[str],
    *,
    timeout: float = DEFAULT_WAIT_TIMEOUT,
    cancel_event: Event | None = None,
) -> Iterator[None]:
    """Hold a shared directory-inode lease without creating project files."""
    if sys.platform != "linux" or fcntl is None:
        raise ProjectLockError("project maintenance locking is supported on Linux only")
    if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or not 0 < timeout <= DEFAULT_WAIT_TIMEOUT:
        raise ValueError("project maintenance timeout must be finite, positive and at most 30 seconds")
    deadline = time.monotonic() + timeout
    _check_wait(deadline, cancel_event)
    directory, expected = _open_directory(os.fspath(project_dir))
    acquired = False
    try:
        _acquire_flock(directory, fcntl.LOCK_SH, deadline, cancel_event)
        acquired = True
        current, info = _open_directory(os.fspath(project_dir))
        os.close(current)
        if (info.st_dev, info.st_ino) != (expected.st_dev, expected.st_ino):
            raise ProjectLockError("project maintenance directory changed while waiting")
        _check_wait(deadline, cancel_event)
        yield
    finally:
        try:
            if acquired:
                fcntl.flock(directory, fcntl.LOCK_UN)
        finally:
            os.close(directory)
