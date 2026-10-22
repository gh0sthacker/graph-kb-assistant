"""Filesystem layout for local data, kept outside the source checkout."""
from __future__ import annotations

import os
from pathlib import Path
import re


def data_root() -> Path:
    configured = os.environ.get("BB_ASSISTANT_DATA_HOME")
    if configured:
        root = Path(configured).expanduser()
    elif os.environ.get("XDG_DATA_HOME"):
        root = Path(os.environ["XDG_DATA_HOME"]).expanduser() / "bb-graph-assistant"
    else:
        root = Path.home() / ".local" / "share" / "bb-graph-assistant"
    return root.resolve()


def ensure_private_directory(path: Path) -> Path:
    """Create a private directory on a local filesystem.

    SQLite WAL and the encrypted capture vault depend on local filesystem
    locking and rename semantics. Refuse known network mounts before creating
    runtime files, then check again after resolving the resulting directory.
    """
    requested = path.expanduser().resolve()
    _reject_network_filesystem(requested)
    requested.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        requested.chmod(0o700)
    except OSError:
        pass
    resolved = requested.resolve()
    _reject_network_filesystem(resolved)
    return resolved


_NETWORK_FILESYSTEMS = (
    "cifs", "smb", "nfs", "9p", "sshfs", "fuse.sshfs", "davfs", "davfs2",
    "fuse.davfs", "ceph", "glusterfs", "afs", "lustre", "gpfs", "curlftpfs",
    "fuse.curlftpfs",
)


def _reject_network_filesystem(path: Path) -> None:
    mount = _mount_for_path(path)
    if mount is None:
        return
    filesystem, mountpoint = mount
    normalized = filesystem.lower()
    if any(normalized == name or normalized.startswith(name + ".") for name in _NETWORK_FILESYSTEMS):
        raise ValueError(
            f"Runtime data must be stored on a local filesystem; {mountpoint} uses {filesystem}"
        )


def _mount_for_path(path: Path) -> tuple[str, Path] | None:
    """Return the most specific Linux mount for path when mountinfo exists."""
    mountinfo = Path("/proc/self/mountinfo")
    try:
        lines = mountinfo.read_text(encoding="utf-8").splitlines()
    except OSError:
        return None

    matches: list[tuple[int, str, Path]] = []
    for line in lines:
        before, separator, after = line.partition(" - ")
        if not separator:
            continue
        left = before.split()
        right = after.split()
        if len(left) < 5 or not right:
            continue
        mountpoint_text = re.sub(r"\\([0-7]{3})", lambda match: chr(int(match.group(1), 8)), left[4])
        mountpoint = Path(mountpoint_text)
        try:
            if path == mountpoint or path.is_relative_to(mountpoint):
                matches.append((len(mountpoint.parts), right[0], mountpoint))
        except (OSError, ValueError):
            continue
    if not matches:
        return None
    _specificity, filesystem, mountpoint = max(matches, key=lambda item: item[0])
    return filesystem, mountpoint
