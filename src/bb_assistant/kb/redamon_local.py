"""Verified local provider for RedAmon's pinned ToolDocs source snapshot."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import re
from typing import Mapping

from .redamon_sources import SourceBatch, SourceRecord, SourceRequest


_SHA256 = re.compile(r"^[0-9a-f]{64}$")
_COMMIT = re.compile(r"^[0-9a-f]{40}$")


class RedAmonLocalToolDocsProvider:
    """Read a vendored, hash-verified RedAmon ``agentic/skills`` snapshot.

    The provider reads only the project-local reference bundle, never a sibling
    checkout or arbitrary path from the manifest. Markdown is preserved whole;
    canonical chunking and retrieval are handled by the common KB pipeline.
    """

    def __init__(self, *, project_root: Path) -> None:
        self.project_root = project_root.resolve()

    def __call__(self, request: SourceRequest) -> SourceBatch:
        if request.source_name != "tool_docs":
            raise ValueError("provider only supports the tool_docs source")
        relative = request.source_spec.get("local_root")
        if not isinstance(relative, str) or not relative:
            raise ValueError("tool_docs local_root is missing")
        source_root = (self.project_root / relative).resolve()
        if self.project_root not in source_root.parents:
            raise ValueError("tool_docs root resolves outside the project")
        if not source_root.is_dir() or source_root.is_symlink():
            raise ValueError("vendored tool_docs root is unavailable")

        manifest_path = source_root / "SOURCE-MANIFEST.json"
        manifest = json.loads(manifest_path.read_text("utf-8"))
        if not isinstance(manifest, Mapping) or manifest.get("schema_version") != 1:
            raise ValueError("vendored tool_docs manifest has an unsupported schema")
        revision = manifest.get("source_commit")
        pinned_revision = request.expected_revision
        if not isinstance(revision, str) or not _COMMIT.fullmatch(revision) or revision != pinned_revision:
            raise ValueError("vendored tool_docs revision does not match the source pin")

        raw_rows = manifest.get("documents")
        if not isinstance(raw_rows, list):
            raise ValueError("vendored tool_docs manifest has no document list")
        expected: dict[str, tuple[str, int]] = {}
        for row in raw_rows:
            if not isinstance(row, Mapping):
                raise ValueError("vendored tool_docs manifest row is malformed")
            path = row.get("path")
            digest = row.get("sha256")
            size = row.get("bytes")
            if (
                not isinstance(path, str)
                or path.startswith("/")
                or ".." in Path(path).parts
                or not _SHA256.fullmatch(str(digest))
                or type(size) is not int
                or size < 0
                or path in expected
            ):
                raise ValueError("vendored tool_docs manifest row is invalid")
            expected[path] = (str(digest), size)

        actual_paths = {
            path.relative_to(source_root).as_posix()
            for path in source_root.rglob("*.md")
            if path.name != "root_agent.md"
        }
        if actual_paths != set(expected):
            raise ValueError("vendored tool_docs file set differs from its manifest")

        include = {str(item) for item in request.filters.get("include_categories", ())}
        exclude = {str(item) for item in request.filters.get("exclude", ())}
        records: list[SourceRecord] = []
        digest_rows: list[tuple[str, str]] = []
        for rel_path in sorted(expected):
            path = source_root / rel_path
            if path.is_symlink() or not path.is_file():
                raise ValueError("vendored tool_docs contains a non-regular file")
            raw = path.read_bytes()
            expected_hash, expected_size = expected[rel_path]
            actual_hash = hashlib.sha256(raw).hexdigest()
            if actual_hash != expected_hash or len(raw) != expected_size:
                raise ValueError("vendored tool_docs content hash mismatch")
            category, _, filename = rel_path.partition("/")
            digest_rows.append((rel_path, actual_hash))
            if category not in include or filename in exclude:
                continue
            text = raw.decode("utf-8", errors="strict")
            if not text.strip():
                raise ValueError("vendored tool_docs contains an empty document")
            title = _title(text, Path(filename).stem)
            search_text = _redamon_search_text(text, category, title)
            records.append(SourceRecord(
                source_id=rel_path,
                title=title,
                original_text=text,
                source_locator=f"agentic/skills/{rel_path}",
                source_url=(
                    "https://github.com/samugit83/redamon/blob/"
                    f"{revision}/agentic/skills/{rel_path}"
                ),
                source_revision=revision,
                metadata={
                    "category_dir": category,
                    "filename": filename,
                    "tool_name": title,
                    "source_content_sha256": actual_hash,
                    "search_normalization": (
                        "redamon_tooling_summary_v1" if category == "tooling"
                        else "redamon_frontmatter_stripped_v1"
                    ),
                },
                search_text=search_text,
            ))

        license_path = source_root / "LICENSE"
        if license_path.is_symlink() or not license_path.is_file():
            raise ValueError("vendored RedAmon MIT LICENSE is missing")
        license_text = license_path.read_text("utf-8")
        license_hash = hashlib.sha256(license_text.encode("utf-8")).hexdigest()
        if license_hash != manifest.get("license_sha256"):
            raise ValueError("vendored RedAmon LICENSE hash mismatch")
        records.append(SourceRecord(
            source_id="NOTICE:LICENSE",
            title="RedAmon MIT License",
            original_text=license_text,
            source_locator="LICENSE",
            source_url=f"https://github.com/samugit83/redamon/blob/{revision}/LICENSE",
            source_revision=revision,
            metadata={"document_role": "license_notice", "license_id": "MIT"},
            document_role="license_notice",
        ))

        artifact = hashlib.sha256(json.dumps(
            {"revision": revision, "documents": digest_rows, "license": license_hash},
            sort_keys=True, separators=(",", ":"), ensure_ascii=False,
        ).encode("utf-8")).hexdigest()
        count = len(records) - 1
        return SourceBatch(
            source_revision=revision,
            records=tuple(records),
            expected_count=count,
            fetched_count=count,
            parsed_count=count,
            rejected_count=0,
            complete=True,
            artifact_sha256=artifact,
            applied_filters=dict(request.filters),
            notices_complete=True,
            limitations=("Only the RedAmon ToolDocs family is bundled in this snapshot.",),
        )


def _title(text: str, fallback: str) -> str:
    frontmatter = re.match(r"\A---\s*\n(.*?)\n---(?:\s*\n|\Z)", text, re.DOTALL)
    if frontmatter:
        match = re.search(r"(?m)^name:\s*(.+?)\s*$", frontmatter.group(1))
        if match:
            return match.group(1).strip().strip("\"'")
    for line in text.splitlines():
        candidate = line.strip()
        if candidate.startswith("# "):
            return candidate[2:].strip()
    return fallback.replace("_", " ")


def _redamon_search_text(raw_content: str, category: str, tool_name: str) -> str:
    """Reproduce RedAmon ToolDocs' indexed text while retaining raw source.

    RedAmon removes YAML frontmatter for all non-tooling categories. Tooling
    docs instead become a compact description/syntax/example card; the full
    Markdown remains available as ``raw_source_text`` through the source API.
    """
    if category != "tooling":
        return _strip_frontmatter(raw_content)

    description = _frontmatter_field(raw_content, "description")
    if not description:
        raise ValueError(f"tooling/{tool_name}: RedAmon requires a frontmatter description")

    syntax = _first_backtick_after(raw_content, ("Canonical syntax", "Syntax", "Usage"))
    example = _first_backtick_after(raw_content, (
        "Agent-safe baseline for automation", "Agent-safe baseline", "Common usage", "Example",
    ))
    parts = [f"{tool_name} — {description}"]
    if syntax:
        parts.append(f"Syntax: `{syntax}`")
    if example:
        parts.append(f"Example: `{example}`")
    return "\n\n".join(parts)


def _strip_frontmatter(content: str) -> str:
    if content.startswith("---"):
        end = content.find("---", 3)
        if end != -1:
            return content[end + 3:].strip()
    return content


def _frontmatter_field(content: str, field: str) -> str:
    if not content.startswith("---"):
        return ""
    end = content.find("---", 3)
    if end == -1:
        return ""
    match = re.search(rf"(?m)^{re.escape(field)}:\s*(.+)$", content[3:end])
    return match.group(1).strip() if match else ""


def _first_backtick_after(content: str, anchors: tuple[str, ...]) -> str:
    for anchor in anchors:
        match = re.search(rf"{re.escape(anchor)}\s*:?\s*\n?\s*`([^`]+)`", content)
        if match:
            return match.group(1).strip()
    return ""
