"""Pinned WSTG test-document provider for the RedAmon KB rebuild."""
from __future__ import annotations

from datetime import datetime, timezone
import hashlib
from pathlib import PurePosixPath
from typing import Callable
from urllib.parse import quote

from .pinned_github_archive import download_pinned_archive, read_pinned_archive
from .redamon_sources import SourceBatch, SourceRecord, SourceRequest


_REPOSITORY = "https://github.com/OWASP/wstg"
_REPOSITORY_SLUG = "wstg"
_TEST_DIRECTORY = "document/4-Web_Application_Security_Testing"

Fetcher = Callable[[str], bytes]


def _download_archive(url: str) -> bytes:
    prefix = f"{_REPOSITORY}/archive/"
    if not url.startswith(prefix):
        raise ValueError("OWASP WSTG archive URL differs from the pinned repository")
    revision = url[len(prefix):].removesuffix(".tar.gz")
    return download_pinned_archive(_REPOSITORY, revision)


class RedAmonOWASPProvider:
    """Preserve the selected WSTG Markdown test cases and root license notice."""

    def __init__(self, *, fetcher: Fetcher = _download_archive) -> None:
        if not callable(fetcher):
            raise TypeError("fetcher must be callable")
        self._fetcher = fetcher

    def __call__(self, request: SourceRequest) -> SourceBatch:
        if request.source_name != "owasp":
            raise ValueError("provider only supports the owasp source")
        revision = request.expected_revision
        if not isinstance(revision, str) or len(revision) != 40 or any(
            char not in "0123456789abcdef" for char in revision
        ):
            raise ValueError("OWASP WSTG revision is not a pinned commit")
        if request.source_spec.get("url") != _REPOSITORY:
            raise ValueError("OWASP WSTG repository differs from the audited source")
        expected_filters = {"test_directory": _TEST_DIRECTORY, "exclude_readme": True}
        if dict(request.filters) != expected_filters:
            raise ValueError("OWASP WSTG filters differ from the audited source")

        archive_bytes = self._fetcher(f"{_REPOSITORY}/archive/{revision}.tar.gz")
        content, notices, artifact_sha256 = read_pinned_archive(
            archive_bytes,
            repository_slug=_REPOSITORY_SLUG,
            revision=revision,
            content_roots=("document",),
            allowed_extensions=(".md",),
        )
        if "license" not in notices:
            raise ValueError("OWASP WSTG pinned archive does not contain its LICENSE file")

        selected: dict[str, bytes] = {}
        prefix = _TEST_DIRECTORY + "/"
        for relative, raw in content.items():
            if not relative.startswith(prefix):
                continue
            if PurePosixPath(relative).name.casefold() == "readme.md":
                continue
            selected[relative] = raw
        if not selected:
            raise ValueError("OWASP WSTG archive has no selected test documents")

        records: list[SourceRecord] = []
        base_url = f"{_REPOSITORY}/blob/{revision}/"
        for relative, raw in sorted(selected.items()):
            original_text = raw.decode("utf-8", errors="strict")
            if not original_text.strip():
                raise ValueError("OWASP WSTG archive contains an empty test document")
            path = PurePosixPath(relative)
            records.append(SourceRecord(
                source_id=relative,
                title=path.stem.replace("_", " "),
                original_text=original_text,
                source_locator=relative,
                source_url=base_url + quote(relative, safe="/"),
                source_revision=revision,
                metadata={
                    "category": path.parent.name,
                    "filename": path.name,
                    "source_content_sha256": hashlib.sha256(raw).hexdigest(),
                },
            ))

        for name, (filename, raw) in sorted(notices.items()):
            notice_text = raw.decode("utf-8", errors="strict")
            if not notice_text.strip():
                raise ValueError("OWASP WSTG archive contains an empty license/notice file")
            records.append(SourceRecord(
                source_id=f"NOTICE:{name}",
                title=f"OWASP WSTG {filename}",
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

        content_count = len(selected)
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
            applied_filters=expected_filters,
            notices_complete=True,
            limitations=(
                "GitHub tar.gz archives are not guaranteed to be byte-reproducible; the content is commit-pinned.",
                "Only Markdown test cases under document/4-Web_Application_Security_Testing are indexed; README.md is excluded.",
            ),
        )
