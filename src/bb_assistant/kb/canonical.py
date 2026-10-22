"""Stable, provenance-preserving KB document and chunk records."""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json


CHUNK_SCHEMA = "bb-kb-chunk-v1"


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
    def from_document(cls, document: SourceDocument, ordinal: int, start_char: int, end_char: int, text: str) -> "CanonicalChunk":
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
            title=document.title,
            ordinal=ordinal,
            start_char=start_char,
            end_char=end_char,
            text=text,
            source_url=document.source_url,
            source_locator=document.source_locator,
            source_revision=document.source_revision,
            license_id=document.license_id,
            attribution=document.attribution,
            metadata=document.metadata,
            source_sha256=document.content_sha256,
            chunk_sha256=chunk_hash,
        )


def chunk_document(document: SourceDocument, *, max_chars: int = 6000, overlap_chars: int = 400) -> tuple[CanonicalChunk, ...]:
    """Deterministic character-window chunking with original-text spans."""
    if max_chars < 256 or overlap_chars < 0 or overlap_chars >= max_chars:
        raise ValueError("Require max_chars >= 256 and 0 <= overlap_chars < max_chars")
    text = document.text
    if not text:
        return ()
    chunks: list[CanonicalChunk] = []
    start = 0
    while start < len(text):
        end = min(len(text), start + max_chars)
        if end < len(text):
            paragraph = text.rfind("\n\n", start + max_chars // 2, end)
            if paragraph > start:
                end = paragraph + 2
        chunks.append(CanonicalChunk.from_document(document, len(chunks), start, end, text[start:end]))
        if end == len(text):
            break
        next_start = max(start + 1, end - overlap_chars)
        newline = text.find("\n", next_start, min(len(text), next_start + 200))
        if newline >= 0 and newline < end:
            next_start = newline + 1
        start = next_start
    return tuple(chunks)
