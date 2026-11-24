"""Pinned, bounded LOLBAS source provider for the RedAmon KB rebuild."""
from __future__ import annotations

from datetime import datetime, timezone
import hashlib
from pathlib import PurePosixPath
from typing import Callable
from urllib.parse import quote

from .pinned_github_archive import download_pinned_archive, read_pinned_archive
from .redamon_sources import SourceBatch, SourceRecord, SourceRequest


_REPOSITORY = "https://github.com/LOLBAS-Project/LOLBAS"
_REPOSITORY_SLUG = "LOLBAS"

Fetcher = Callable[[str], bytes]


def _download_archive(url: str) -> bytes:
    prefix = f"{_REPOSITORY}/archive/"
    if not url.startswith(prefix):
        raise ValueError("LOLBAS archive URL differs from the pinned repository")
    revision = url[len(prefix):].removesuffix(".tar.gz")
    return download_pinned_archive(_REPOSITORY, revision)


class RedAmonLOLBASProvider:
    """Read pinned YAML records and required notices without filesystem extraction."""

    def __init__(self, *, fetcher: Fetcher = _download_archive) -> None:
        if not callable(fetcher):
            raise TypeError("fetcher must be callable")
        self._fetcher = fetcher

    def __call__(self, request: SourceRequest) -> SourceBatch:
        if request.source_name != "lolbas":
            raise ValueError("provider only supports the lolbas source")
        revision = request.expected_revision
        if not isinstance(revision, str) or len(revision) != 40 or any(
            char not in "0123456789abcdef" for char in revision
        ):
            raise ValueError("LOLBAS revision is not a pinned commit")
        if request.source_spec.get("url") != _REPOSITORY:
            raise ValueError("LOLBAS repository differs from the audited source")
        if request.filters.get("source_directory") != "yml":
            raise ValueError("LOLBAS directory filter differs from the audited source")

        archive_bytes = self._fetcher(f"{_REPOSITORY}/archive/{revision}.tar.gz")
        content, notices, artifact_sha256 = read_pinned_archive(
            archive_bytes,
            repository_slug=_REPOSITORY_SLUG,
            revision=revision,
            content_roots=("yml",),
            allowed_extensions=(".yml", ".yaml"),
        )
        if not content:
            raise ValueError("LOLBAS archive has no usable YAML source files")
        if "license" not in notices or "notice.md" not in notices:
            raise ValueError("LOLBAS pinned archive is missing LICENSE or NOTICE.md")

        records: list[SourceRecord] = []
        base_url = f"{_REPOSITORY}/blob/{revision}/"
        for relative, raw in sorted(content.items()):
            source_id = relative
            original_text = raw.decode("utf-8", errors="strict")
            if not original_text.strip():
                raise ValueError("LOLBAS archive contains an empty YAML source file")
            filename = PurePosixPath(relative).name
            records.append(SourceRecord(
                source_id=source_id,
                title=PurePosixPath(filename).stem,
                original_text=original_text,
                source_locator=source_id,
                source_url=base_url + quote(source_id, safe="/"),
                source_revision=revision,
                metadata={
                    "filename": filename,
                    "source_content_sha256": hashlib.sha256(raw).hexdigest(),
                },
            ))

        for name, (filename, raw) in sorted(notices.items()):
            notice_text = raw.decode("utf-8", errors="strict")
            if not notice_text.strip():
                raise ValueError("LOLBAS archive contains an empty license/notice file")
            records.append(SourceRecord(
                source_id=f"NOTICE:{name}",
                title=f"LOLBAS {filename}",
                original_text=notice_text,
                source_locator=filename,
                source_url=base_url + quote(filename, safe="/"),
                source_revision=revision,
                metadata={
                    "notice_name": filename,
                    "source_content_sha256": hashlib.sha256(raw).hexdigest(),
                },
                document_role="license_notice",
            ))

        content_count = len(content)
        return SourceBatch(
            source_revision=revision,
            records=tuple(records),
            expected_count=content_count,
            fetched_count=content_count,
            parsed_count=content_count,
            rejected_count=0,
            complete=True,
            artifact_sha256=artifact_sha256,
            fetched_at=datetime.now(timezone.utc).isoformat(),
            applied_filters=dict(request.filters),
            notices_complete=True,
            limitations=(
                "GitHub tar.gz archives are not guaranteed to be byte-reproducible; the content is commit-pinned.",
                "Only YAML files under yml/ and root license/notice files are included.",
                "The retained NOTICE includes the MITRE ATT&CK license notice; external detection links remain references only.",
            ),
        )
