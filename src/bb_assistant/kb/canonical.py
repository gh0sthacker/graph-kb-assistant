"""Stable, provenance-preserving KB document and chunk records."""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json


CHUNK_SCHEMA = "bb-kb-chunk-v2"


def _digest(payload: object) -> str:
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


@dataclass(frozen=True, slots=True)
class SourceDocument:
    source_name: str
    source_id: str
    title: str
    text: str
    source_url: str | None
    source_locator: str | None
    source_revision: str | None
    license_id: str | None
    attribution: str | None
    metadata: dict[str, str | int | float | bool | None]
    content_sha256: str

    @classmethod
    def create(
        cls,
        *,
        source_name: str,
        source_id: str,
        title: str,
        text: str,
        source_url: str | None = None,
        source_locator: str | None = None,
        source_revision: str | None = None,
        license_id: str | None = None,
        attribution: str | None = None,
        metadata: dict[str, str | int | float | bool | None] | None = None,
    ) -> "SourceDocument":
        return cls(
            source_name=source_name,
            source_id=source_id,
            title=title,
            text=text,
            source_url=source_url,
            source_locator=source_locator,
            source_revision=source_revision,
            license_id=license_id,
            attribution=attribution,
            metadata=metadata or {},
            content_sha256=hashlib.sha256(text.encode("utf-8")).hexdigest(),
        )


@dataclass(frozen=True, slots=True)
class CanonicalChunk:
    chunk_id: str
    source_name: str
    source_id: str
    title: str
    ordinal: int
    start_char: int
    end_char: int
    text: str
    source_url: str | None
    source_locator: str | None
    source_revision: str | None
    license_id: str | None
    attribution: str | None
    metadata: dict[str, str | int | float | bool | None]
    source_sha256: str
    chunk_sha256: str
    chunk_schema: str = CHUNK_SCHEMA

    @classmethod
    def from_document(
        cls, document: SourceDocument, ordinal: int, start_char: int, end_char: int, text: str,
        *, title: str | None = None, metadata: dict[str, str | int | float | bool | None] | None = None,
    ) -> "CanonicalChunk":
        if ordinal < 0 or start_char < 0 or end_char < start_char or end_char - start_char != len(text) or not text.strip():
            raise ValueError("Chunk ordinal must be nonnegative and text must not be blank")
        chunk_hash = hashlib.sha256(text.encode("utf-8")).hexdigest()
        identity = {
            "schema": CHUNK_SCHEMA,
            "source_name": document.source_name,
            "source_id": document.source_id,
            "source_sha256": document.content_sha256,
            "ordinal": ordinal,
            "start_char": start_char,
            "end_char": end_char,
            "chunk_sha256": chunk_hash,
        }
        return cls(
            chunk_id=_digest(identity),
            source_name=document.source_name,
            source_id=document.source_id,
            title=title or document.title,
            ordinal=ordinal,
            start_char=start_char,
            end_char=end_char,
            text=text,
            source_url=document.source_url,
            source_locator=document.source_locator,
            source_revision=document.source_revision,
            license_id=document.license_id,
            attribution=document.attribution,
            metadata=document.metadata if metadata is None else metadata,
            source_sha256=document.content_sha256,
            chunk_sha256=chunk_hash,
        )


def chunk_document(document: SourceDocument, *, max_chars: int = 1920, overlap_chars: int = 0) -> tuple[CanonicalChunk, ...]:
    """Chunk like RedAmon while keeping exact, non-overlapping source spans.

    RedAmon estimates tokens as ``len(text) // 4`` and uses a 480-token cap.
    Markdown playbooks are grouped on H2 boundaries and split on paragraphs;
    structured feeds stay atomic and oversized records are clipped at the cap.
    The stored chunk is always an exact substring of its source document so
    hashes and citations remain independently verifiable.
    """
    if max_chars < 256 or overlap_chars != 0:
        raise ValueError("RedAmon-compatible chunks require max_chars >= 256 and zero overlap")
    text = document.text
    if not text:
        return ()
    markdown = document.source_name in {"tool_docs", "owasp"}
    ranges: list[tuple[int, int, str, bool]] = []
    if markdown:
        ranges = _markdown_ranges(text, max_chars)
    else:
        ranges = _structured_ranges(text, max_chars)
    metadata = dict(document.metadata)
    chunks = []
    for start, end, title, truncated in ranges:
        chunk_metadata = {**metadata, "chunk_truncated": truncated} if truncated else metadata
        chunks.append(CanonicalChunk.from_document(
            document, len(chunks), start, end, text[start:end], title=title, metadata=chunk_metadata,
        ))
    return tuple(chunks)


def _estimated_tokens(text: str) -> int:
    return len(text.strip()) // 4


def _bounded_end(text: str, start: int, limit: int) -> tuple[int, bool]:
    end = min(len(text), start + limit)
    if end < len(text):
        whitespace = text.rfind(" ", start, end)
        if whitespace > start + limit // 2:
            end = whitespace
        return end, True
    return end, False


def _record_title(text: str, start: int, fallback: str) -> str:
    first = next((line.strip() for line in text[start:].splitlines() if line.strip()), "")
    return first.lstrip("#").strip() or fallback


def _structured_ranges(text: str, limit: int) -> list[tuple[int, int, str, bool]]:
    """One chunk per structured record; clip oversized records like RedAmon."""
    end, clipped = _bounded_end(text, 0, limit)
    return [(0, end, _record_title(text, 0, "Record"), clipped)]


def _markdown_ranges(text: str, limit: int) -> list[tuple[int, int, str, bool]]:
    import re

    starts = [match.start() for match in re.finditer(r"(?m)^##\s", text)]
    starts.insert(0, 0)
    sections = [(start, starts[index + 1] if index + 1 < len(starts) else len(text))
                for index, start in enumerate(starts)]
    sections = [(start, end) for start, end in sections if text[start:end].strip()]
    grouped: list[tuple[int, int, str]] = []
    pending: tuple[int, int, str] | None = None
    for start, end in sections:
        title = _record_title(text, start, "Markdown section")
        if pending is not None:
            if _estimated_tokens(text[pending[0]:end]) <= limit // 4:
                pending = (pending[0], end, pending[2])
                continue
            grouped.append(pending)
            pending = None
        if _estimated_tokens(text[start:end]) < 128:
            pending = (start, end, title)
        else:
            grouped.append((start, end, title))
    if pending is not None:
        grouped.append(pending)

    result: list[tuple[int, int, str, bool]] = []
    for start, end, title in grouped:
        cursor = start
        parts: list[tuple[int, int, bool]] = []
        while cursor < end:
            if _estimated_tokens(text[cursor:end]) <= limit // 4:
                parts.append((cursor, end, False))
                break
            hard_end = min(end, cursor + limit)
            boundary = text.rfind("\n\n", cursor, hard_end)
            if boundary > cursor:
                chunk_end = boundary
                clipped = False
            else:
                chunk_end, clipped = _bounded_end(text, cursor, limit)
                chunk_end = min(chunk_end, end)
            parts.append((cursor, chunk_end, clipped))
            cursor = chunk_end
            while cursor < end and text[cursor].isspace():
                cursor += 1
        for part, (part_start, part_end, clipped) in enumerate(parts, start=1):
            part_title = f"{title} (part {part})" if len(parts) > 1 else title
            result.append((part_start, part_end, part_title, clipped))
    return result
