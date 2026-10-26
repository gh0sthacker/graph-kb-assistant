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
    raw_source_text: str | None = None
    raw_source_sha256: str | None = None

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
        raw_source_text: str | None = None,
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
            raw_source_text=raw_source_text,
            raw_source_sha256=(
                hashlib.sha256(raw_source_text.encode("utf-8")).hexdigest()
                if raw_source_text is not None else None
            ),
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
        source_span_exact: bool = True,
    ) -> "CanonicalChunk":
        if (ordinal < 0 or start_char < 0 or end_char < start_char
                or (source_span_exact and end_char - start_char != len(text)) or not text.strip()):
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
            metadata={**(document.metadata if metadata is None else metadata),
                      **({} if source_span_exact else {"source_span_exact": False})},
            source_sha256=document.content_sha256,
            chunk_sha256=chunk_hash,
        )


def chunk_document(document: SourceDocument, *, max_chars: int = 1920, overlap_chars: int = 0) -> tuple[CanonicalChunk, ...]:
    """Match RedAmon chunk content/titles and retain a truthful source span.

    RedAmon trims sections, normalizes merged paragraph separators and appends
    an ellipsis to clipped records. Non-literal chunks therefore carry a
    covering source span marked ``source_span_exact=False`` in metadata.
    """
    if max_chars < 256 or overlap_chars != 0:
        raise ValueError("RedAmon-compatible chunks require max_chars >= 256 and zero overlap")
    if not document.text:
        return ()
    if document.source_name == "tool_docs" and document.metadata.get("category_dir") == "tooling":
        # RedAmon indexes each tooling file as one compact summary card and
        # keeps the tool name as its title; the summary's first line is not a
        # Markdown section heading.
        rendered = [(document.text, document.title, False)]
    else:
        rendered = (_markdown_chunks(document.text, max_chars)
                    if document.source_name in {"tool_docs", "owasp"}
                    else _structured_chunks(document, max_chars))
    chunks = []
    cursor = 0
    for content, title, truncated in rendered:
        start, end, exact = _source_span(document.text, content, cursor)
        metadata = {**document.metadata, "chunk_truncated": True} if truncated else document.metadata
        chunk_title = title
        if document.source_name == "tool_docs" and document.metadata.get("category_dir") != "tooling":
            tool_name = str(document.metadata.get("tool_name") or document.title)
            if tool_name.lower() not in title.lower():
                chunk_title = f"{tool_name} — {title}"
        chunks.append(CanonicalChunk.from_document(
            document, len(chunks), start, end, content, title=chunk_title, metadata=metadata,
            source_span_exact=exact,
        ))
        cursor = end
    return tuple(chunks)


def _estimated_tokens(text: str) -> int:
    return len(text) // 4


def _structured_chunks(document: SourceDocument, limit: int) -> list[tuple[str, str, bool]]:
    content = document.text
    if _estimated_tokens(content) <= limit // 4:
        return [(content, document.title, False)]
    content = content[:limit].rsplit(" ", 1)[0] + "..."
    return [(content, document.title, True)]


def _markdown_chunks(text: str, limit: int) -> list[tuple[str, str, bool]]:
    import re

    sections = [section.strip() for section in re.split(r"(?m)(?=^##\s)", text) if section.strip()]
    parsed = []
    for section in sections:
        lines = section.split("\n", 1)
        parsed.append((lines[0].lstrip("#").strip(), section.strip()))

    merged: list[tuple[str, str]] = []
    pending: tuple[str, str] | None = None
    for title, content in parsed:
        if pending is not None:
            combined = pending[1] + "\n\n" + content
            if _estimated_tokens(combined) <= limit // 4:
                pending = (pending[0], combined)
                continue
            merged.append(pending)
            pending = None
        if _estimated_tokens(content) < 128:
            pending = (title, content)
        else:
            merged.append((title, content))
    if pending is not None:
        merged.append(pending)

    result: list[tuple[str, str, bool]] = []
    for title, content in merged:
        if _estimated_tokens(content) <= limit // 4:
            result.append((content, title, False))
            continue
        paragraphs = re.split(r"\n\n+", content)
        current = ""
        parts: list[str] = []
        for paragraph in paragraphs:
            candidate = (current + "\n\n" + paragraph).strip() if current else paragraph
            if _estimated_tokens(candidate) > limit // 4 and current:
                parts.append(current.strip())
                current = paragraph
            else:
                current = candidate
        if current.strip():
            parts.append(current.strip())
        for part, chunk_text in enumerate(parts, start=1):
            part_title = f"{title} (part {part})" if part > 1 or len(paragraphs) > 1 else title
            result.append((chunk_text, part_title, False))
    return result


def _source_span(source: str, chunk: str, cursor: int) -> tuple[int, int, bool]:
    exact_start = source.find(chunk, cursor)
    if exact_start >= 0:
        return exact_start, exact_start + len(chunk), True
    searchable = chunk[:-3] if chunk.endswith("...") else chunk
    literal_start = source.find(searchable, cursor)
    if literal_start >= 0:
        return literal_start, literal_start + len(searchable), False
    prefix = searchable[:min(80, len(searchable))].rstrip()
    suffix = searchable[-min(80, len(searchable)):].lstrip()
    start = source.find(prefix, cursor) if prefix else -1
    if start < 0:
        first_line = next((line.strip() for line in chunk.splitlines() if line.strip()), "")
        prefix = first_line[:min(80, len(first_line))]
        start = source.find(prefix, cursor) if prefix else -1
    if start < 0:
        raise ValueError("Cannot map RedAmon-compatible chunk to source text")
    suffix_start = source.find(suffix, start + len(prefix), min(len(source), start + len(chunk) + 4096)) if suffix else -1
    end = suffix_start + len(suffix) if suffix_start >= start else min(len(source), start + len(searchable))
    return start, end, source[start:end] == chunk
