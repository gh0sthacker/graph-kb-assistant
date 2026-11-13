"""Exclusive process lock preventing duplicate long-running KB embedding jobs."""
from __future__ import annotations

from contextlib import contextmanager
import errno
import fcntl
import os
from pathlib import Path
import stat
from typing import Iterator


@contextmanager
def embedding_worker_lock(index_dir: Path) -> Iterator[None]:
    path = index_dir / ".kb-embedding.lock"
    try:
        fd = os.open(
            path,
            os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
            0o600,
        )
    except OSError as exc:
        raise RuntimeError("KB embedding worker lock could not be safely opened") from exc
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.geteuid() or info.st_nlink != 1):
            raise RuntimeError("KB embedding worker lock is not a private owner-only file")
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            if exc.errno in {errno.EAGAIN, errno.EWOULDBLOCK}:
                raise RuntimeError("another KB embedding worker is already active") from None
            raise RuntimeError("KB embedding worker lock could not be safely acquired") from exc
        yield
    finally:
        os.close(fd)
