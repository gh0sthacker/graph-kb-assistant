"""Pinned Nuclei templates provider for the RedAmon KB rebuild."""
from __future__ import annotations

from datetime import datetime, timezone
import hashlib
from pathlib import PurePosixPath
from typing import Callable
from urllib.parse import quote

from .pinned_github_archive import download_pinned_archive, read_pinned_archive
from .redamon_sources import SourceBatch, SourceRecord, SourceRequest


_REPOSITORY = "https://github.com/projectdiscovery/nuclei-templates"
_REPOSITORY_SLUG = "nuclei-templates"
_MAX_ARCHIVE_BYTES = 200 * 1024 * 1024
_MAX_ARCHIVE_MEMBERS = 50_000
_MAX_EXPANDED_BYTES = 500 * 1024 * 1024
_MAX_CONTENT_FILES = 30_000
_MAX_TEMPLATE_BYTES = 16 * 1024 * 1024

Fetcher = Callable[[str], bytes]


def _download_archive(url: str) -> bytes:
    prefix = f"{_REPOSITORY}/archive/"
    if not url.startswith(prefix):
        raise ValueError("Nuclei templates archive URL differs from the pinned repository")
    revision = url[len(prefix):].removesuffix(".tar.gz")
    return download_pinned_archive(
        _REPOSITORY, revision, max_archive_bytes=_MAX_ARCHIVE_BYTES
    )


class RedAmonNucleiProvider:
    """Preserve all selected YAML templates and the repository license notice."""

    def __init__(self, *, fetcher: Fetcher = _download_archive) -> None:
        if not callable(fetcher):
            raise TypeError("fetcher must be callable")
        self._fetcher = fetcher

    def __call__(self, request: SourceRequest) -> SourceBatch:
        if request.source_name != "nuclei":
            raise ValueError("provider only supports the nuclei source")
        revision = request.expected_revision
        if not isinstance(revision, str) or len(revision) != 40 or any(
            char not in "0123456789abcdef" for char in revision
        ):
            raise ValueError("Nuclei templates revision is not a pinned commit")
        if request.source_spec.get("url") != _REPOSITORY:
            raise ValueError("Nuclei templates repository differs from the audited source")
        expected_filters = {
            "extensions": (".yaml", ".yml"),
            "exclude_prefixes": (".github/",),
            "exclude_paths": (".pre-commit-config.yaml", ".pre-commit-config.yml"),
        }
        if dict(request.filters) != expected_filters:
            raise ValueError("Nuclei template filters differ from the audited source")

        archive_bytes = self._fetcher(f"{_REPOSITORY}/archive/{revision}.tar.gz")
        content, notices, artifact_sha256 = read_pinned_archive(
            archive_bytes,
            repository_slug=_REPOSITORY_SLUG,
            revision=revision,
            content_roots=None,
            allowed_extensions=expected_filters["extensions"],
            excluded_prefixes=expected_filters["exclude_prefixes"],
            excluded_paths=expected_filters["exclude_paths"],
            max_member_bytes=_MAX_TEMPLATE_BYTES,
            max_archive_bytes=_MAX_ARCHIVE_BYTES,
            max_archive_members=_MAX_ARCHIVE_MEMBERS,
            max_expanded_bytes=_MAX_EXPANDED_BYTES,
            max_content_files=_MAX_CONTENT_FILES,
        )
        if not content:
            raise ValueError("Nuclei archive has no usable YAML templates")
        if "license.md" not in notices and "license" not in notices:
            raise ValueError("Nuclei pinned archive does not contain LICENSE.md")

        records: list[SourceRecord] = []
        base_url = f"{_REPOSITORY}/blob/{revision}/"
        for relative, raw in sorted(content.items()):
            original_text = raw.decode("utf-8", errors="strict")
            if not original_text.strip():
                raise ValueError("Nuclei archive contains an empty YAML template")
            path = PurePosixPath(relative)
            records.append(SourceRecord(
                source_id=relative,
                title=path.stem,
                original_text=original_text,
                source_locator=relative,
                source_url=base_url + quote(relative, safe="/"),
                source_revision=revision,
                metadata={
                    "filename": path.name,
                    "template_id": path.stem,
                    "source_content_sha256": hashlib.sha256(raw).hexdigest(),
                },
            ))

        for name, (filename, raw) in sorted(notices.items()):
            notice_text = raw.decode("utf-8", errors="strict")
            if not notice_text.strip():
                raise ValueError("Nuclei archive contains an empty license/notice file")
            records.append(SourceRecord(
                source_id=f"NOTICE:{name}",
                title=f"Nuclei templates {filename}",
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
            applied_filters=expected_filters,
            notices_complete=True,
            limitations=(
                "GitHub tar.gz archives are not guaranteed to be byte-reproducible; the content is commit-pinned.",
                "All selected YAML source is preserved verbatim; no template is executed or sent to a target.",
            ),
        )
