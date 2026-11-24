"""Bounded readers for immutable GitHub source archives."""
from __future__ import annotations

import hashlib
from io import BytesIO
from pathlib import PurePosixPath
import tarfile
from urllib.parse import urlsplit
from urllib.request import Request, urlopen


_LICENSE_NAMES = {"license", "license.md", "copying", "notice", "notice.md"}
_MAX_ARCHIVE_BYTES = 20 * 1024 * 1024
_MAX_ARCHIVE_MEMBERS = 8_000
_MAX_MEMBER_BYTES = 4 * 1024 * 1024
_MAX_EXPANDED_BYTES = 128 * 1024 * 1024
_MAX_CONTENT_FILES = 5_000


def download_pinned_archive(
    repository: str, revision: str, *, max_archive_bytes: int = _MAX_ARCHIVE_BYTES
) -> bytes:
    """Download one pinned GitHub archive, allowing only GitHub HTTPS redirects."""
    if type(max_archive_bytes) is not int or max_archive_bytes < 1:
        raise ValueError("Pinned archive byte limit must be a positive integer")
    url = f"{repository}/archive/{revision}.tar.gz"
    request = Request(url, headers={"User-Agent": "bb-graph-assistant/0.1"})
    with urlopen(request, timeout=60) as response:
        final_url = urlsplit(response.geturl())
        if final_url.scheme != "https" or final_url.hostname not in {
            "github.com", "codeload.github.com"
        }:
            raise ValueError("Pinned archive redirected outside GitHub")
        archive_bytes = response.read(max_archive_bytes + 1)
    if len(archive_bytes) > max_archive_bytes:
        raise ValueError("Pinned archive exceeds the download limit")
    return archive_bytes


def read_pinned_archive(
    archive_bytes: bytes,
    *,
    repository_slug: str,
    revision: str,
    content_roots: tuple[str, ...] | None,
    allowed_extensions: tuple[str, ...],
    max_member_bytes: int = _MAX_MEMBER_BYTES,
    excluded_prefixes: tuple[str, ...] = (),
    excluded_paths: tuple[str, ...] = (),
    max_archive_bytes: int = _MAX_ARCHIVE_BYTES,
    max_archive_members: int = _MAX_ARCHIVE_MEMBERS,
    max_expanded_bytes: int = _MAX_EXPANDED_BYTES,
    max_content_files: int = _MAX_CONTENT_FILES,
) -> tuple[dict[str, bytes], dict[str, tuple[str, bytes]], str]:
    """Read selected regular files and root notices without filesystem extraction.

    Returned content paths are repository-relative. An empty extension tuple
    selects every regular file under the declared content root(s), which is
    needed for extensionless source records such as GTFOBins entries. Notice
    keys are case-folded file names; their original file names and bytes are
    retained in the values. The GitHub archive top-level directory must match
    the requested repo/commit.
    """
    if not isinstance(archive_bytes, bytes) or not archive_bytes:
        raise ValueError("Pinned archive response is empty or unsupported")
    limits = (
        max_archive_bytes,
        max_archive_members,
        max_expanded_bytes,
        max_content_files,
        max_member_bytes,
    )
    if any(type(limit) is not int or limit < 1 for limit in limits):
        raise ValueError("Pinned archive limits must be positive integers")
    if len(archive_bytes) > max_archive_bytes:
        raise ValueError("Pinned archive exceeds the download limit")
    if not repository_slug or "/" in repository_slug or "\\" in repository_slug:
        raise ValueError("Pinned archive repository slug is invalid")
    if content_roots is not None and (
        not content_roots or any(not root or "/" in root or "\\" in root for root in content_roots)
    ):
        raise ValueError("Pinned archive content roots are invalid")
    root_name = f"{repository_slug}-{revision}"
    roots = frozenset(content_roots or ())
    extensions = tuple(ext.casefold() for ext in allowed_extensions)
    excluded_prefixes_normalized = tuple(prefix.strip("/") + "/" for prefix in excluded_prefixes)
    excluded_paths_normalized = frozenset(path.strip("/") for path in excluded_paths)
    content: dict[str, bytes] = {}
    notices: dict[str, tuple[str, bytes]] = {}
    expanded_bytes = 0
    member_count = 0

    with tarfile.open(fileobj=BytesIO(archive_bytes), mode="r:gz") as archive:
        for member in archive:
            member_count += 1
            if member_count > max_archive_members:
                raise ValueError("Pinned archive contains too many entries")
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or "\\" in member.name:
                raise ValueError("Pinned archive contains an unsafe path")
            if not path.parts or path.parts[0] != root_name:
                raise ValueError("Pinned archive root does not match repository and revision")
            if member.isdir():
                continue
            if not member.isfile():
                raise ValueError("Pinned archive contains a non-regular entry")
            if member.size < 0 or member.size > max_member_bytes:
                raise ValueError("Pinned archive member exceeds the file limit")
            expanded_bytes += member.size
            if expanded_bytes > max_expanded_bytes:
                raise ValueError("Pinned archive exceeds the expanded-size limit")

            relative_parts = path.parts[1:]
            if not relative_parts:
                raise ValueError("Pinned archive contains an invalid root file")
            relative = "/".join(relative_parts)
            filename = relative_parts[-1]
            is_notice = len(relative_parts) == 1 and filename.casefold() in _LICENSE_NAMES
            matches_root = (
                content_roots is None
                or (len(relative_parts) >= 2 and relative_parts[0] in roots)
            )
            excluded = relative in excluded_paths_normalized or any(
                relative.startswith(prefix) for prefix in excluded_prefixes_normalized
            )
            is_content = (
                matches_root and not excluded
                and (not extensions or any(filename.casefold().endswith(ext) for ext in extensions))
            )
            if not is_notice and not is_content:
                continue

            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError("Pinned archive member could not be read")
            raw = stream.read(max_member_bytes + 1)
            if len(raw) != member.size or len(raw) > max_member_bytes:
                raise ValueError("Pinned archive member size does not match its header")

            if is_notice:
                notice_key = filename.casefold()
                if notice_key in notices:
                    raise ValueError("Pinned archive contains duplicate notice paths")
                notices[notice_key] = (filename, raw)
            else:
                if relative in content:
                    raise ValueError("Pinned archive contains duplicate source paths")
                content[relative] = raw
                if len(content) > max_content_files:
                    raise ValueError("Pinned archive contains too many source files")

    return content, notices, hashlib.sha256(archive_bytes).hexdigest()
