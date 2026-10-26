"""Local SQLite FTS5 index for canonical KB chunks and verifiable citations."""
from __future__ import annotations

from dataclasses import dataclass, replace
from contextlib import contextmanager, nullcontext
import hashlib
import importlib.util
import json
import logging
import math
import os
from pathlib import Path
import re
import sqlite3
import struct
import time
from typing import Callable, Iterable, Iterator, Mapping

from ..paths import data_root, ensure_private_directory
from ..project_lock import project_maintenance_lock, project_maintenance_read_lock
from .canonical import CHUNK_SCHEMA, CanonicalChunk, SourceDocument, redamon_chunk_id
from .ranking import (
    DEFAULT_OVERFETCH_FACTOR,
    DEFAULT_RERANK_POOL_SIZE,
    DEFAULT_TOP_K,
    RankedCandidate,
    rank_candidates,
)

logger = logging.getLogger(__name__)

_MAX_SEARCH_TERMS = 32


def _redamon_fulltext_expression(query: str) -> str:
    """Build the same unprefixed OR/Lucene-operator query RedAmon sends to Neo4j."""
    sanitized = re.sub(r'[+\-!(){}\[\]^"~*?:\\/]', " ", query)
    sanitized = sanitized.replace("&&", " ").replace("||", " ").strip()
    items: list[str] = []
    term_count = 0
    for word in sanitized.split():
        if word in {"AND", "OR", "NOT"}:
            items.append(word)
            continue
        for term in re.findall(r"[\w-]{1,64}", word, flags=re.UNICODE):
            if term_count >= _MAX_SEARCH_TERMS:
                break
            items.append('"' + term.replace('"', '""') + '"')
            term_count += 1
        if term_count >= _MAX_SEARCH_TERMS:
            break
    if not items:
        return ""

    expression: list[str] = []
    previous_was_term = False
    for index, item in enumerate(items):
        if item in {"AND", "OR", "NOT"}:
            if not previous_was_term or index + 1 == len(items) or items[index + 1] in {"AND", "OR", "NOT"}:
                return ""
            expression.append(item)
            previous_was_term = False
        else:
            expression.append(item)
            previous_was_term = True
    rendered = []
    for item in expression:
        if rendered and item not in {"AND", "OR", "NOT"} and rendered[-1] not in {"AND", "OR", "NOT"}:
            rendered.append("OR")
        rendered.append(item)
    return " ".join(rendered)


def _commit_with_busy_retry(connection: sqlite3.Connection, *, retries: int = 5) -> None:
    """Retry a durable batch commit when another local reader briefly holds SQLite."""
    for attempt in range(retries + 1):
        try:
            connection.commit()
            return
        except sqlite3.OperationalError as exc:
            message = str(exc).lower()
            if attempt >= retries or not ("locked" in message or "busy" in message):
                raise
            time.sleep(min(0.1 * (2 ** attempt), 1.6))


_SCHEMA = """
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS kb_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS source_documents (
  source_name TEXT NOT NULL,
  source_id TEXT NOT NULL,
  title TEXT NOT NULL,
  source_url TEXT,
  source_locator TEXT,
  source_revision TEXT,
  license_id TEXT,
  attribution TEXT,
  metadata_json TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  text TEXT NOT NULL,
  raw_source_sha256 TEXT,
  raw_source_text TEXT,
  PRIMARY KEY(source_name,source_id)
);
CREATE TABLE IF NOT EXISTS chunks (
  chunk_id TEXT PRIMARY KEY,
  source_name TEXT NOT NULL,
  source_id TEXT NOT NULL,
  title TEXT NOT NULL,
  start_char INTEGER NOT NULL,
  end_char INTEGER NOT NULL,
  source_locator TEXT,
  source_url TEXT,
  source_revision TEXT,
  license_id TEXT,
  attribution TEXT,
  metadata_json TEXT NOT NULL,
  source_sha256 TEXT NOT NULL,
  chunk_sha256 TEXT NOT NULL,
  chunk_schema TEXT NOT NULL,
  text TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS chunk_embeddings (
  chunk_id TEXT PRIMARY KEY REFERENCES chunks(chunk_id) ON DELETE CASCADE,
  model_name TEXT NOT NULL,
  dimensions INTEGER NOT NULL,
  vector BLOB NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS chunk_search USING fts5(
  chunk_id UNINDEXED, source_name UNINDEXED, title, text,
  tokenize='unicode61 remove_diacritics 2'
);
CREATE INDEX IF NOT EXISTS chunks_source_idx ON chunks(source_name,source_id);
CREATE INDEX IF NOT EXISTS chunks_embed_order_idx ON chunks(length(text),chunk_id);
"""


@dataclass(frozen=True, slots=True)
class KBHit:
    chunk_id: str
    source_name: str
    source_id: str
    title: str
    excerpt: str
    source_locator: str | None
    source_url: str | None
    source_revision: str | None
    license_id: str | None
    attribution: str | None
    source_sha256: str
    chunk_sha256: str
    metadata: dict[str, str | int | float | bool | None]
    start_char: int
    end_char: int
    bm25: float
    rank_score: float
    vector_score: float | None = None
    rerank_score: float | None = None


class KBIndex:
    """Lexical retrieval only; vector ranking can be added behind this API."""

    def __init__(
        self, path: Path | None = None, *, read_only: bool = False,
        embedder: object | None = None, reranker: object | None = None,
    ) -> None:
        requested_path = (path if path is not None else data_root() / "kb" / "index.sqlite3").expanduser()
        if read_only and Path(os.path.abspath(requested_path)) != requested_path.resolve():
            raise ValueError("Read-only KB index path must not contain symbolic links")
        self.path = requested_path.resolve()
        self.read_only = bool(read_only)
        self._embedder = embedder
        self._reranker = reranker
        source_root = Path(__file__).resolve().parents[3]
        if self.path == source_root or source_root in self.path.parents:
            raise ValueError("KB indexes must be stored outside the source checkout")
        if self.read_only:
            if self.path.is_symlink() or not self.path.is_file():
                raise ValueError("KB index must already exist as a regular file for read-only access")
        else:
            ensure_private_directory(self.path.parent)
            with project_maintenance_lock(self.path.parent):
                self._initialize()
            try:
                self.path.chmod(0o600)
            except OSError:
                pass

    @contextmanager
    def connect(self, *, _maintenance_lock_held: bool = False) -> Iterator[sqlite3.Connection]:
        if self.read_only:
            lease = project_maintenance_read_lock(self.path.parent)
        elif _maintenance_lock_held:
            lease = nullcontext()
        else:
            lease = project_maintenance_lock(self.path.parent)
        with lease:
            if self.read_only:
                for suffix, label in (("-wal", "WAL"), ("-journal", "rollback journal")):
                    sidecar = Path(str(self.path) + suffix)
                    if sidecar.is_symlink():
                        raise ValueError(f"Read-only KB index {label} must not be a symbolic link")
                    if sidecar.exists() and sidecar.stat().st_size:
                        if suffix == "-wal":
                            raise ValueError("KB index WAL requires a writable checkpoint before read-only access")
                        raise ValueError(
                            "KB index rollback journal requires writable recovery before read-only access"
                        )
                connection = sqlite3.connect(
                    self.path.as_uri() + "?mode=ro&immutable=1", uri=True, timeout=30
                )
            else:
                connection = sqlite3.connect(self.path, timeout=30)
            try:
                connection.row_factory = sqlite3.Row
                connection.execute("PRAGMA busy_timeout = 30000")
                if self.read_only:
                    connection.execute("PRAGMA query_only = ON")
                with connection:
                    yield connection
            finally:
                connection.close()

    def _initialize(self) -> None:
        with self.connect(_maintenance_lock_held=True) as connection:
            connection.executescript(_SCHEMA)
            columns = {row[1] for row in connection.execute("PRAGMA table_info(source_documents)")}
            if "raw_source_sha256" not in columns:
                connection.execute("ALTER TABLE source_documents ADD COLUMN raw_source_sha256 TEXT")
            if "raw_source_text" not in columns:
                connection.execute("ALTER TABLE source_documents ADD COLUMN raw_source_text TEXT")

    def replace_snapshot(
        self,
        chunks: Iterable[CanonicalChunk],
        snapshot_id: str,
        *,
        sources: Iterable[SourceDocument] = (),
        source_manifest_sha256: str | None = None,
        source_coverage_state: str = "unverified",
        source_coverage_report_sha256: str | None = None,
        source_coverage_report: Mapping[str, object] | None = None,
    ) -> int:
        if self.read_only:
            raise ValueError("KB index is read-only")
        with project_maintenance_lock(self.path.parent):
            return self._replace_snapshot(
                chunks,
                snapshot_id,
                sources=sources,
                source_manifest_sha256=source_manifest_sha256,
                source_coverage_state=source_coverage_state,
                source_coverage_report_sha256=source_coverage_report_sha256,
                source_coverage_report=source_coverage_report,
            )

    def _replace_snapshot(
        self,
        chunks: Iterable[CanonicalChunk],
        snapshot_id: str,
        *,
        sources: Iterable[SourceDocument] = (),
        source_manifest_sha256: str | None = None,
        source_coverage_state: str = "unverified",
        source_coverage_report_sha256: str | None = None,
        source_coverage_report: Mapping[str, object] | None = None,
    ) -> int:
        if not isinstance(snapshot_id, str) or not re.fullmatch(r"[a-f0-9]{64}", snapshot_id):
            raise ValueError("snapshot_id must be a lowercase SHA-256 digest")
        if source_manifest_sha256 is not None and not re.fullmatch(r"[a-f0-9]{64}", source_manifest_sha256):
            raise ValueError("source_manifest_sha256 must be a lowercase SHA-256 digest")
        coverage_states = {"DONE", "PARTIAL", "BLOCKED", "unverified"}
        if not isinstance(source_coverage_state, str) or source_coverage_state not in coverage_states:
            raise ValueError("source_coverage_state must be DONE, PARTIAL, BLOCKED or unverified")
        if source_coverage_report is None:
            if source_coverage_state != "unverified" or source_coverage_report_sha256 is not None:
                raise ValueError("verified coverage state requires a matching coverage report")
        else:
            if not isinstance(source_coverage_report, Mapping):
                raise ValueError("source_coverage_report must be an object")
            report_state = source_coverage_report.get("coverage_state")
            report_sources = source_coverage_report.get("sources")
            if not isinstance(report_state, str) or report_state not in {"DONE", "PARTIAL", "BLOCKED"}:
                raise ValueError("coverage report has an invalid coverage_state")
            if source_coverage_state != report_state or not isinstance(report_sources, list):
                raise ValueError("source coverage state does not match its report")
            for row in report_sources:
                if (
                    not isinstance(row, dict)
                    or not isinstance(row.get("source_name"), str)
                    or not row.get("source_name")
                    or not isinstance(row.get("state"), str)
                    or row.get("state") not in {"done", "partial", "blocked"}
                ):
                    raise ValueError("coverage report has malformed per-source states")
            if not isinstance(source_coverage_report_sha256, str) or not re.fullmatch(r"[a-f0-9]{64}", source_coverage_report_sha256):
                raise ValueError("coverage report SHA-256 is invalid")
            try:
                report_bytes = json.dumps(
                    dict(source_coverage_report), sort_keys=True, separators=(",", ":"), ensure_ascii=False
                ).encode("utf-8") + b"\n"
            except (TypeError, ValueError) as exc:
                raise ValueError("coverage report is not canonical JSON data") from exc
            if hashlib.sha256(report_bytes).hexdigest() != source_coverage_report_sha256:
                raise ValueError("coverage report SHA-256 does not match its contents")
            if report_state == "DONE":
                if any(not isinstance(row, dict) or row.get("state") != "done" for row in report_sources):
                    raise ValueError("coverage report claims DONE while a source is not done")
                for key in ("blocked_source_count", "partial_source_count"):
                    count_value = source_coverage_report.get(key, 0)
                    if not isinstance(count_value, int) or isinstance(count_value, bool) or count_value != 0:
                        raise ValueError("coverage report claims DONE with blocked or partial sources")
                source_count = source_coverage_report.get("source_count", len(report_sources))
                if not isinstance(source_count, int) or isinstance(source_count, bool) or source_count != len(report_sources):
                    raise ValueError("coverage report claims DONE with an inconsistent source count")

        source_rows = list(sources)
        chunk_rows = list(chunks)
        source_lookup: dict[tuple[str, str], SourceDocument] = {}
        for source in source_rows:
            if not isinstance(source, SourceDocument):
                raise ValueError("source records must be SourceDocument values")
            expected = hashlib.sha256(source.text.encode("utf-8")).hexdigest()
            if source.content_sha256 != expected:
                raise ValueError(f"Source hash mismatch for source ID {source.source_id!r}")
            if source.raw_source_text is None:
                if source.raw_source_sha256 is not None:
                    raise ValueError(f"Raw source hash has no text for source ID {source.source_id!r}")
            elif hashlib.sha256(source.raw_source_text.encode("utf-8")).hexdigest() != source.raw_source_sha256:
                raise ValueError(f"Raw source hash mismatch for source ID {source.source_id!r}")
            key = (source.source_name, source.source_id)
            if key in source_lookup:
                raise ValueError("duplicate source document identity")
            source_lookup[key] = source
        for chunk in chunk_rows:
            if not isinstance(chunk, CanonicalChunk):
                raise ValueError("chunk records must be CanonicalChunk values")
            self._validate_chunk(chunk)
            source = source_lookup.get((chunk.source_name, chunk.source_id))
            if source is not None:
                if chunk.source_sha256 != source.content_sha256 or chunk.end_char > len(source.text):
                    raise ValueError("KB chunk source span does not match its source document")
                if chunk.metadata.get("source_span_exact", True) and source.text[chunk.start_char:chunk.end_char] != chunk.text:
                    raise ValueError("Exact KB chunk does not match its source document span")

        with self.connect(_maintenance_lock_held=True) as connection:
            connection.execute("PRAGMA temp_store=MEMORY")
            connection.executescript("""
                CREATE TEMP TABLE incoming_sources(
                  source_name TEXT NOT NULL, source_id TEXT NOT NULL, title TEXT NOT NULL,
                  source_url TEXT, source_locator TEXT, source_revision TEXT, license_id TEXT,
                  attribution TEXT, metadata_json TEXT NOT NULL, content_sha256 TEXT NOT NULL,
                  text TEXT NOT NULL, raw_source_sha256 TEXT, raw_source_text TEXT,
                  PRIMARY KEY(source_name,source_id));
                CREATE TEMP TABLE incoming_chunks(
                  chunk_id TEXT PRIMARY KEY, source_name TEXT NOT NULL, source_id TEXT NOT NULL,
                  title TEXT NOT NULL, start_char INTEGER NOT NULL, end_char INTEGER NOT NULL,
                  source_locator TEXT, source_url TEXT, source_revision TEXT, license_id TEXT,
                  attribution TEXT, metadata_json TEXT NOT NULL, source_sha256 TEXT NOT NULL,
                  chunk_sha256 TEXT NOT NULL, chunk_schema TEXT NOT NULL, text TEXT NOT NULL);
            """)
            connection.executemany(
                "INSERT INTO incoming_sources VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    (source.source_name, source.source_id, source.title, source.source_url,
                     source.source_locator, source.source_revision, source.license_id,
                     source.attribution,
                     json.dumps(source.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
                     source.content_sha256, source.text, source.raw_source_sha256, source.raw_source_text)
                    for source in source_rows
                ),
            )
            connection.executemany(
                "INSERT INTO incoming_chunks VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    (chunk.chunk_id, chunk.source_name, chunk.source_id, chunk.title, chunk.start_char,
                     chunk.end_char, chunk.source_locator, chunk.source_url, chunk.source_revision,
                     chunk.license_id, chunk.attribution,
                     json.dumps(chunk.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
                     chunk.source_sha256, chunk.chunk_sha256, chunk.chunk_schema, chunk.text)
                    for chunk in chunk_rows
                ),
            )

            # FTS5 leaves chunk_id UNINDEXED, so probing it for each incoming
            # row degenerates into a quadratic full-table scan at RedAmon scale.
            # Rebuild the lexical projection linearly; canonical chunks and
            # unchanged vectors remain in their keyed tables.
            connection.execute("""
                DELETE FROM chunk_embeddings WHERE chunk_id IN (
                  SELECT old.chunk_id FROM chunks old JOIN incoming_chunks new USING(chunk_id)
                  WHERE old.chunk_sha256 IS NOT new.chunk_sha256)
            """)
            connection.execute("DELETE FROM chunk_search")
            connection.execute("""
                DELETE FROM chunks WHERE NOT EXISTS (
                  SELECT 1 FROM incoming_chunks i WHERE i.chunk_id=chunks.chunk_id)
            """)
            connection.execute("""
                DELETE FROM chunk_embeddings WHERE NOT EXISTS (
                  SELECT 1 FROM incoming_chunks i WHERE i.chunk_id=chunk_embeddings.chunk_id)
            """)
            connection.execute("""
                DELETE FROM source_documents WHERE NOT EXISTS (
                  SELECT 1 FROM incoming_sources i
                  WHERE i.source_name=source_documents.source_name AND i.source_id=source_documents.source_id)
            """)
            connection.execute("""
                INSERT INTO source_documents(
                  source_name,source_id,title,source_url,source_locator,source_revision,license_id,
                  attribution,metadata_json,content_sha256,text,raw_source_sha256,raw_source_text)
                SELECT source_name,source_id,title,source_url,source_locator,source_revision,license_id,
                  attribution,metadata_json,content_sha256,text,raw_source_sha256,raw_source_text
                FROM incoming_sources WHERE 1
                ON CONFLICT(source_name,source_id) DO UPDATE SET
                  title=excluded.title,source_url=excluded.source_url,source_locator=excluded.source_locator,
                  source_revision=excluded.source_revision,license_id=excluded.license_id,
                  attribution=excluded.attribution,metadata_json=excluded.metadata_json,
                  content_sha256=excluded.content_sha256,text=excluded.text,
                  raw_source_sha256=excluded.raw_source_sha256,raw_source_text=excluded.raw_source_text
                WHERE source_documents.title IS NOT excluded.title
                  OR source_documents.source_url IS NOT excluded.source_url
                  OR source_documents.source_locator IS NOT excluded.source_locator
                  OR source_documents.source_revision IS NOT excluded.source_revision
                  OR source_documents.license_id IS NOT excluded.license_id
                  OR source_documents.attribution IS NOT excluded.attribution
                  OR source_documents.metadata_json IS NOT excluded.metadata_json
                  OR source_documents.content_sha256 IS NOT excluded.content_sha256
                  OR source_documents.text IS NOT excluded.text
                  OR source_documents.raw_source_sha256 IS NOT excluded.raw_source_sha256
                  OR source_documents.raw_source_text IS NOT excluded.raw_source_text
            """)
            connection.execute("""
                INSERT INTO chunks(
                  chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,
                  source_revision,license_id,attribution,metadata_json,source_sha256,chunk_sha256,chunk_schema,text)
                SELECT chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,
                  source_revision,license_id,attribution,metadata_json,source_sha256,chunk_sha256,chunk_schema,text
                FROM incoming_chunks WHERE 1
                ON CONFLICT(chunk_id) DO UPDATE SET
                  source_name=excluded.source_name,source_id=excluded.source_id,title=excluded.title,
                  start_char=excluded.start_char,end_char=excluded.end_char,source_locator=excluded.source_locator,
                  source_url=excluded.source_url,source_revision=excluded.source_revision,
                  license_id=excluded.license_id,attribution=excluded.attribution,
                  metadata_json=excluded.metadata_json,source_sha256=excluded.source_sha256,
                  chunk_sha256=excluded.chunk_sha256,chunk_schema=excluded.chunk_schema,text=excluded.text
                WHERE chunks.source_name IS NOT excluded.source_name OR chunks.source_id IS NOT excluded.source_id
                  OR chunks.title IS NOT excluded.title OR chunks.start_char IS NOT excluded.start_char
                  OR chunks.end_char IS NOT excluded.end_char OR chunks.source_locator IS NOT excluded.source_locator
                  OR chunks.source_url IS NOT excluded.source_url OR chunks.source_revision IS NOT excluded.source_revision
                  OR chunks.license_id IS NOT excluded.license_id OR chunks.attribution IS NOT excluded.attribution
                  OR chunks.metadata_json IS NOT excluded.metadata_json OR chunks.source_sha256 IS NOT excluded.source_sha256
                  OR chunks.chunk_sha256 IS NOT excluded.chunk_sha256 OR chunks.chunk_schema IS NOT excluded.chunk_schema
                  OR chunks.text IS NOT excluded.text
            """)
            connection.execute("""
                INSERT INTO chunk_search(chunk_id,source_name,title,text)
                SELECT chunk_id,source_name,title,text FROM incoming_chunks
            """)
            count = len(chunk_rows)
            connection.execute(
                "DELETE FROM kb_meta WHERE key IN ('snapshot_id','chunk_count','coverage_state','coverage_report_sha256','source_coverage','source_manifest_sha256','vector_model','vector_dimensions','vector_snapshot_id','vector_staging_snapshot_id','vector_staging_model','vector_staging_dimensions')"
            )
            connection.executemany(
                "INSERT INTO kb_meta(key,value) VALUES(?,?)",
                (
                    ("snapshot_id", snapshot_id),
                    ("chunk_count", str(count)),
                    ("coverage_state", source_coverage_state),
                    ("coverage_report_sha256", source_coverage_report_sha256 or ""),
                    ("source_manifest_sha256", source_manifest_sha256 or ""),
                    (
                        "source_coverage",
                        json.dumps(
                            list(source_coverage_report.get("sources", ())) if source_coverage_report is not None else [],
                            sort_keys=True, separators=(",", ":"), ensure_ascii=False,
                        ),
                    ),
                ),
            )
        return count

    def refresh_chunks(
        self,
        chunks: Iterable[CanonicalChunk],
        snapshot_id: str,
        *,
        coverage_state: str,
        coverage_report_sha256: str | None,
        batch_size: int = 2048,
    ) -> int:
        """Idempotently refresh chunk rows in bounded, resumable transactions.

        A durable maintenance marker blocks readers after interruption. Each
        batch is committed separately to bound rollback-journal size; rerunning
        the operation safely reapplies the same target snapshot.
        """
        if self.read_only:
            raise ValueError("KB index is read-only")
        if not re.fullmatch(r"[a-f0-9]{64}", snapshot_id):
            raise ValueError("snapshot_id must be a lowercase SHA-256 digest")
        if coverage_state not in {"DONE", "PARTIAL", "BLOCKED", "unverified"}:
            raise ValueError("unsupported KB coverage state")
        if coverage_report_sha256 is not None and not re.fullmatch(r"[a-f0-9]{64}", coverage_report_sha256):
            raise ValueError("coverage report SHA-256 is invalid")
        if not 1 <= batch_size <= 10_000:
            raise ValueError("chunk refresh batch_size must be between 1 and 10000")

        chunk_rows = list(chunks)
        if not chunk_rows:
            raise ValueError("cannot refresh an empty KB chunk snapshot")
        seen: set[str] = set()
        for chunk in chunk_rows:
            self._validate_chunk(chunk)
            if chunk.chunk_id in seen:
                raise ValueError("duplicate chunk identity in refresh")
            seen.add(chunk.chunk_id)

        with project_maintenance_lock(self.path.parent):
            with self.connect(_maintenance_lock_held=True) as connection:
                meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
                previous_snapshot = meta.get("snapshot_id")
                if not previous_snapshot:
                    raise ValueError("Cannot refresh chunks without an installed snapshot")
                retained_model = meta.get("vector_staging_model") or meta.get("vector_model")
                retained_dimensions = meta.get("vector_staging_dimensions") or meta.get("vector_dimensions")

                connection.execute("PRAGMA temp_store=MEMORY")
                connection.executescript("""
                    CREATE TEMP TABLE IF NOT EXISTS incoming_refresh_chunks(
                      batch_no INTEGER NOT NULL, chunk_id TEXT PRIMARY KEY,
                      source_name TEXT NOT NULL, source_id TEXT NOT NULL, title TEXT NOT NULL,
                      start_char INTEGER NOT NULL, end_char INTEGER NOT NULL, source_locator TEXT,
                      source_url TEXT, source_revision TEXT, license_id TEXT, attribution TEXT,
                      metadata_json TEXT NOT NULL, source_sha256 TEXT NOT NULL,
                      chunk_sha256 TEXT NOT NULL, chunk_schema TEXT NOT NULL, text TEXT NOT NULL);
                    DELETE FROM incoming_refresh_chunks;
                """)
                connection.executemany(
                    "INSERT INTO incoming_refresh_chunks VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        (ordinal // batch_size, chunk.chunk_id, chunk.source_name, chunk.source_id,
                         chunk.title, chunk.start_char, chunk.end_char, chunk.source_locator, chunk.source_url,
                         chunk.source_revision, chunk.license_id, chunk.attribution,
                         json.dumps(chunk.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
                         chunk.source_sha256, chunk.chunk_sha256, chunk.chunk_schema, chunk.text)
                        for ordinal, chunk in enumerate(chunk_rows)
                    ),
                )
                connection.commit()

                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('rechunk_target_snapshot_id',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (snapshot_id,),
                )
                connection.execute(
                    "DELETE FROM kb_meta WHERE key IN ('vector_model','vector_dimensions','vector_snapshot_id')"
                )
                connection.commit()

                connection.execute("""
                    DELETE FROM chunk_search WHERE chunk_id NOT IN
                      (SELECT chunk_id FROM incoming_refresh_chunks)
                """)
                connection.execute("""
                    DELETE FROM chunks WHERE NOT EXISTS
                      (SELECT 1 FROM incoming_refresh_chunks i WHERE i.chunk_id=chunks.chunk_id)
                """)
                connection.execute("""
                    DELETE FROM chunk_embeddings WHERE NOT EXISTS
                      (SELECT 1 FROM incoming_refresh_chunks i WHERE i.chunk_id=chunk_embeddings.chunk_id)
                """)
                connection.commit()

                batch_count = (len(chunk_rows) + batch_size - 1) // batch_size
                for batch_no in range(batch_count):
                    connection.execute("""
                        DELETE FROM chunk_search WHERE chunk_id IN (
                          SELECT f.chunk_id FROM chunk_search f
                          JOIN incoming_refresh_chunks i ON i.chunk_id=f.chunk_id
                          WHERE i.batch_no=? AND (f.title IS NOT i.title OR f.text IS NOT i.text))
                    """, (batch_no,))
                    connection.execute("""
                        INSERT INTO chunks(
                          chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,
                          source_revision,license_id,attribution,metadata_json,source_sha256,chunk_sha256,chunk_schema,text)
                        SELECT chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,
                          source_revision,license_id,attribution,metadata_json,source_sha256,chunk_sha256,chunk_schema,text
                        FROM incoming_refresh_chunks WHERE batch_no=?
                        ON CONFLICT(chunk_id) DO UPDATE SET
                          source_name=excluded.source_name,source_id=excluded.source_id,title=excluded.title,
                          start_char=excluded.start_char,end_char=excluded.end_char,source_locator=excluded.source_locator,
                          source_url=excluded.source_url,source_revision=excluded.source_revision,
                          license_id=excluded.license_id,attribution=excluded.attribution,
                          metadata_json=excluded.metadata_json,source_sha256=excluded.source_sha256,
                          chunk_sha256=excluded.chunk_sha256,chunk_schema=excluded.chunk_schema,text=excluded.text
                        WHERE chunks.source_name IS NOT excluded.source_name OR chunks.source_id IS NOT excluded.source_id
                          OR chunks.title IS NOT excluded.title OR chunks.start_char IS NOT excluded.start_char
                          OR chunks.end_char IS NOT excluded.end_char OR chunks.source_locator IS NOT excluded.source_locator
                          OR chunks.source_url IS NOT excluded.source_url OR chunks.source_revision IS NOT excluded.source_revision
                          OR chunks.license_id IS NOT excluded.license_id OR chunks.attribution IS NOT excluded.attribution
                          OR chunks.metadata_json IS NOT excluded.metadata_json OR chunks.source_sha256 IS NOT excluded.source_sha256
                          OR chunks.chunk_sha256 IS NOT excluded.chunk_sha256 OR chunks.chunk_schema IS NOT excluded.chunk_schema
                          OR chunks.text IS NOT excluded.text
                    """, (batch_no,))
                    connection.execute("""
                        INSERT INTO chunk_search(chunk_id,source_name,title,text)
                        SELECT i.chunk_id,i.source_name,i.title,i.text FROM incoming_refresh_chunks i
                        WHERE i.batch_no=? AND NOT EXISTS
                          (SELECT 1 FROM chunk_search f WHERE f.chunk_id=i.chunk_id)
                    """, (batch_no,))
                    connection.commit()

                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES(?,?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    ("snapshot_id", snapshot_id),
                )
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('chunk_count',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (str(len(chunk_rows)),),
                )
                if coverage_report_sha256:
                    connection.execute(
                        "INSERT INTO kb_meta(key,value) VALUES('coverage_report_sha256',?) "
                        "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (coverage_report_sha256,),
                    )
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('coverage_state',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (coverage_state,),
                )
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('vector_staging_snapshot_id',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (snapshot_id,),
                )
                if retained_model:
                    connection.execute(
                        "INSERT INTO kb_meta(key,value) VALUES('vector_staging_model',?) "
                        "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (retained_model,),
                    )
                if retained_dimensions:
                    connection.execute(
                        "INSERT INTO kb_meta(key,value) VALUES('vector_staging_dimensions',?) "
                        "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (retained_dimensions,),
                    )
                connection.execute("DELETE FROM kb_meta WHERE key='rechunk_target_snapshot_id'")
                connection.commit()
        return len(chunk_rows)

    def get_source(self, source_name: str, source_id: str) -> dict[str, object] | None:
        with self.connect() as connection:
            if connection.execute("SELECT 1 FROM kb_meta WHERE key='rechunk_target_snapshot_id'").fetchone():
                raise RuntimeError("KB index maintenance is in progress")
            row = connection.execute(
                "SELECT * FROM source_documents WHERE source_name=? AND source_id=?",
                (source_name, source_id),
            ).fetchone()
        if row is None:
            return None
        result = dict(row)
        result["metadata"] = json.loads(result.pop("metadata_json"))
        return result

    def stats(self) -> dict[str, object]:
        with self.connect() as connection:
            source_count = connection.execute("SELECT COUNT(*) FROM (SELECT DISTINCT source_name FROM source_documents)").fetchone()[0]
            document_count = connection.execute("SELECT COUNT(*) FROM source_documents").fetchone()[0]
            chunk_count = connection.execute("SELECT COUNT(*) FROM chunks").fetchone()[0]
            vector_count = connection.execute("SELECT COUNT(*) FROM chunk_embeddings").fetchone()[0]
            meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
        try:
            source_coverage = json.loads(meta.get("source_coverage", "[]"))
        except json.JSONDecodeError:
            source_coverage = []
        source_manifest_sha256 = meta.get("source_manifest_sha256") or None
        if source_manifest_sha256 is None:
            snapshot_id = meta.get("snapshot_id")
            if isinstance(snapshot_id, str) and re.fullmatch(r"[a-f0-9]{64}", snapshot_id):
                snapshot_manifest_path = self.path.parent / "snapshots" / snapshot_id / "manifest.json"
                try:
                    snapshot_manifest = json.loads(snapshot_manifest_path.read_text(encoding="utf-8"))
                except (OSError, UnicodeError, json.JSONDecodeError):
                    snapshot_manifest = None
                if isinstance(snapshot_manifest, dict):
                    candidate = snapshot_manifest.get("source_manifest_sha256")
                    if isinstance(candidate, str) and re.fullmatch(r"[a-f0-9]{64}", candidate):
                        source_manifest_sha256 = candidate
        try:
            current_manifest_path = Path(__file__).with_name("sources.yaml")
            current_source_manifest_sha256 = hashlib.sha256(current_manifest_path.read_bytes()).hexdigest()
        except OSError:
            current_source_manifest_sha256 = None
        source_manifest_matches_current = (
            source_manifest_sha256 == current_source_manifest_sha256
            if source_manifest_sha256 is not None and current_source_manifest_sha256 is not None
            else None
        )
        vector_storage_complete = (
            vector_count > 0 and meta.get("vector_snapshot_id") == meta.get("snapshot_id")
        )
        try:
            vector_backend_available = importlib.util.find_spec("numpy") is not None
        except (ImportError, ValueError):
            vector_backend_available = False
        return {
            "source_families": source_count,
            "source_documents": document_count,
            "chunks": chunk_count,
            "snapshot_id": meta.get("snapshot_id"),
            "coverage_state": meta.get("coverage_state", "unverified"),
            "coverage_report_sha256": meta.get("coverage_report_sha256") or None,
            "source_coverage": source_coverage,
            "source_manifest_sha256": source_manifest_sha256,
            "current_source_manifest_sha256": current_source_manifest_sha256,
            "source_manifest_matches_current": source_manifest_matches_current,
            "embedded_chunks": vector_count,
            "embedding_model": meta.get("vector_model") or None,
            "embedding_snapshot_id": meta.get("vector_snapshot_id") or None,
            "vector_storage_complete": vector_storage_complete,
            "vector_backend_available": vector_backend_available,
            "vector_ready": vector_storage_complete and vector_backend_available,
            "maintenance_state": "rechunking" if meta.get("rechunk_target_snapshot_id") else None,
        }

    def rebuild_embeddings(
        self,
        embedder: object,
        *,
        batch_size: int = 2,
        progress_callback: Callable[[int, int], None] | None = None,
        before_batch_callback: Callable[[], object] | None = None,
    ) -> int:
        from .worker_lock import embedding_worker_lock

        if self.read_only:
            raise ValueError("KB index is read-only")
        with embedding_worker_lock(self.path.parent):
            return self._rebuild_embeddings_locked(
                embedder,
                batch_size=batch_size,
                progress_callback=progress_callback,
                before_batch_callback=before_batch_callback,
            )

    def _rebuild_embeddings_locked(
        self,
        embedder: object,
        *,
        batch_size: int = 2,
        progress_callback: Callable[[int, int], None] | None = None,
        before_batch_callback: Callable[[], object] | None = None,
    ) -> int:
        """Build snapshot-bound vectors in durable batches and publish when complete.

        Completed batches survive process restarts. The ready snapshot marker is
        written only after every content chunk has a matching vector, so readers
        never mistake a partial generation for a ready index.
        """
        model_name = getattr(embedder, "model_name", None)
        batch_encoder = getattr(embedder, "embed_documents_batch", None)
        if not isinstance(model_name, str) or not model_name or not callable(batch_encoder):
            raise TypeError("embedder must expose model_name and embed_documents_batch")
        if not 1 <= batch_size <= 2:
            raise ValueError("embedding batch_size must be between 1 and 2 for CPU safety")
        if self.read_only:
            raise ValueError("KB index is read-only")

        with project_maintenance_lock(self.path.parent), self.connect(_maintenance_lock_held=True) as connection:
            meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
            snapshot_id = meta.get("snapshot_id")
            if not snapshot_id:
                raise ValueError("Cannot build embeddings without an imported KB snapshot")
            if meta.get("rechunk_target_snapshot_id"):
                raise ValueError("Cannot build vectors while the KB index is being re-chunked")
            total = int(connection.execute(
                "SELECT count(*) FROM chunks WHERE COALESCE(json_extract(metadata_json,'$.document_role'),'content')='content'"
            ).fetchone()[0])
            if total == 0:
                raise ValueError("KB snapshot contains no content chunks to embed")
            existing_count = int(connection.execute(
                "SELECT count(*) FROM chunk_embeddings WHERE model_name=?", (model_name,),
            ).fetchone()[0])
            if meta.get("vector_snapshot_id") == snapshot_id and meta.get("vector_model") == model_name and existing_count == total:
                if progress_callback is not None:
                    progress_callback(total, total)
                return total
            staging_matches = (
                meta.get("vector_staging_snapshot_id") == snapshot_id
                and meta.get("vector_staging_model") == model_name
            )
            dimension_rows = connection.execute(
                "SELECT DISTINCT dimensions FROM chunk_embeddings WHERE model_name=?", (model_name,),
            ).fetchall()
            if len(dimension_rows) > 1:
                raise ValueError("existing matching vectors have inconsistent dimensions")
            existing_dimensions = int(dimension_rows[0][0]) if dimension_rows else 0
            if not staging_matches:
                connection.execute(
                    "DELETE FROM chunk_embeddings WHERE chunk_id NOT IN ("
                    "SELECT chunk_id FROM chunks WHERE COALESCE(json_extract(metadata_json,'$.document_role'),'content')='content')"
                )
                connection.execute("DELETE FROM kb_meta WHERE key IN ('vector_model','vector_dimensions','vector_snapshot_id')")
                connection.executemany(
                    "INSERT INTO kb_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    (("vector_staging_snapshot_id", snapshot_id), ("vector_staging_model", model_name)),
                )
                connection.execute("DELETE FROM kb_meta WHERE key='vector_staging_dimensions'")
                dimensions = existing_dimensions
            else:
                dimensions = int(meta.get("vector_staging_dimensions", str(existing_dimensions)) or 0)
            _commit_with_busy_retry(connection)

        while True:
            # Keep only the short SQLite read/write windows under the project
            # lease. CPU-bound model inference runs outside it; the vector
            # snapshot stays unpublished until its final complete batch.
            with project_maintenance_lock(self.path.parent), self.connect(_maintenance_lock_held=True) as connection:
                meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
                if meta.get("snapshot_id") != snapshot_id or meta.get("rechunk_target_snapshot_id"):
                    raise ValueError("KB snapshot changed during vector generation")
                if (meta.get("vector_staging_snapshot_id") != snapshot_id
                        or meta.get("vector_staging_model") != model_name):
                    raise ValueError("KB vector staging state changed during generation")
                rows = connection.execute(
                    """SELECT c.chunk_id,c.text FROM chunks c
                       LEFT JOIN chunk_embeddings e ON e.chunk_id=c.chunk_id AND e.model_name=?
                       WHERE COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content'
                         AND e.chunk_id IS NULL
                       ORDER BY length(c.text),c.chunk_id LIMIT ?""",
                    (model_name, batch_size),
                ).fetchall()
                staged_dimensions = meta.get("vector_staging_dimensions")
                if staged_dimensions is not None:
                    dimensions = int(staged_dimensions)
                if not rows:
                    embedded = int(connection.execute(
                        "SELECT count(*) FROM chunk_embeddings WHERE model_name=?", (model_name,),
                    ).fetchone()[0])
                    if dimensions == 0 or embedded != total:
                        raise ValueError("KB vector generation is incomplete")
                    connection.executemany(
                        "INSERT INTO kb_meta(key,value) VALUES(?,?) "
                        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                        (("vector_model", model_name), ("vector_dimensions", str(dimensions)),
                         ("vector_snapshot_id", snapshot_id)),
                    )
                    connection.execute(
                        "DELETE FROM kb_meta WHERE key IN ('vector_staging_snapshot_id','vector_staging_model','vector_staging_dimensions')"
                    )
                    return embedded
                batch = [(row["chunk_id"], row["text"]) for row in rows]

            if before_batch_callback is not None:
                before_batch_callback()
            vectors = batch_encoder([text for _, text in batch], batch_size=batch_size)
            if len(vectors) != len(batch):
                raise ValueError("embedder returned a different number of vectors than chunks")
            encoded_rows = []
            next_dimensions = dimensions
            for (chunk_id, _), vector in zip(batch, vectors):
                values = [float(value) for value in vector]
                if not values or any(not math.isfinite(value) for value in values):
                    raise ValueError("embedder returned an empty or non-finite vector")
                if next_dimensions == 0:
                    next_dimensions = len(values)
                elif len(values) != next_dimensions:
                    raise ValueError("embedder returned inconsistent vector dimensions")
                encoded_rows.append((chunk_id, model_name, len(values), struct.pack(f"<{len(values)}f", *values)))

            with project_maintenance_lock(self.path.parent), self.connect(_maintenance_lock_held=True) as connection:
                meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
                if meta.get("snapshot_id") != snapshot_id or meta.get("rechunk_target_snapshot_id"):
                    raise ValueError("KB snapshot changed during vector generation")
                if (meta.get("vector_staging_snapshot_id") != snapshot_id
                        or meta.get("vector_staging_model") != model_name):
                    raise ValueError("KB vector staging state changed during generation")
                connection.executemany(
                    "INSERT OR IGNORE INTO chunk_embeddings(chunk_id,model_name,dimensions,vector) VALUES(?,?,?,?)",
                    encoded_rows,
                )
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('vector_staging_dimensions',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (str(next_dimensions,),),
                )
                _commit_with_busy_retry(connection)
                dimensions = next_dimensions
                embedded = int(connection.execute(
                    "SELECT count(*) FROM chunk_embeddings WHERE model_name=?", (model_name,),
                ).fetchone()[0])
            if progress_callback is not None:
                progress_callback(embedded, total)

    def _vector_search(
        self, query: str, *, top_k: int,
    ) -> tuple[list[str], dict[str, float]]:
        with self.connect() as connection:
            meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
            model_name = meta.get("vector_model", "")
            dimensions = int(meta.get("vector_dimensions", "0") or 0)
            snapshot_id = meta.get("snapshot_id", "")
            vector_count = connection.execute("SELECT COUNT(*) FROM chunk_embeddings").fetchone()[0]
        if (
            vector_count == 0 or not model_name or dimensions <= 0
            or meta.get("vector_snapshot_id") != snapshot_id
        ):
            return [], {}
        embedder = self._embedder
        if embedder is None:
            try:
                from .embeddings import RedAmonEmbedder
                embedder = RedAmonEmbedder(local_files_only=True)
                self._embedder = embedder
            except Exception as exc:
                logger.info("KB vector retrieval unavailable: %s", type(exc).__name__)
                return [], {}
        if getattr(embedder, "model_name", None) != model_name:
            logger.warning("KB vector model does not match the stored embedding model; skipping vector search")
            return [], {}
        try:
            query_vector = [float(value) for value in embedder.embed_query(query)]
            if len(query_vector) != dimensions or not query_vector or any(not math.isfinite(v) for v in query_vector):
                raise ValueError("query embedding dimensions or values are invalid")
            import numpy as np
            query_array = np.asarray(query_vector, dtype=np.float32)
            sql = """SELECT e.chunk_id,e.vector FROM chunk_embeddings e
                     JOIN chunks c ON c.chunk_id=e.chunk_id
                     WHERE e.model_name=? AND e.dimensions=?
                       AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content'"""
            params: list[object] = [model_name, dimensions]
            with self.connect() as connection:
                rows = connection.execute(sql, params).fetchall()
            if not rows:
                return [], {}
            matrix = np.vstack([np.frombuffer(row["vector"], dtype="<f4") for row in rows])
            if matrix.shape[1] != dimensions:
                raise ValueError("stored embedding dimensions are invalid")
            scores = matrix @ query_array
            selected = np.argsort(scores)[::-1][:min(top_k, len(rows))]
            ranked_ids = [rows[int(index)]["chunk_id"] for index in selected]
            score_map = {rows[int(index)]["chunk_id"]: float(scores[int(index)]) for index in selected}
            return ranked_ids, score_map
        except Exception as exc:
            logger.warning("KB vector retrieval failed; using available lexical results (%s)", type(exc).__name__)
            return [], {}

    def _get_reranker(self) -> object | None:
        if self._reranker is not None:
            return self._reranker
        try:
            from .embeddings import RedAmonReranker
            self._reranker = RedAmonReranker(local_files_only=True)
            return self._reranker
        except Exception as exc:
            logger.info("KB cross-encoder reranking unavailable: %s", type(exc).__name__)
            return None

    def search(self, query: str, *, top_k: int = DEFAULT_TOP_K, source_names: tuple[str, ...] = ()) -> tuple[KBHit, ...]:
        if not (1 <= top_k <= 100):
            raise ValueError("top_k must be between 1 and 100")
        if len(query) > 4096:
            raise ValueError("KB query exceeds the 4096-character limit")
        match_query = _redamon_fulltext_expression(query)
        if not match_query:
            return ()
        sql = """SELECT c.chunk_id,c.source_name,c.source_id,c.title,c.text,c.source_locator,c.source_url,
                        c.source_revision,c.license_id,c.attribution,c.source_sha256,c.chunk_sha256,c.metadata_json,
                        c.start_char,c.end_char,bm25(chunk_search,0.0,0.0,1.0,1.0) AS score
                 FROM chunk_search JOIN chunks c ON c.chunk_id=chunk_search.chunk_id
                 WHERE chunk_search MATCH ?
                   AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content') = 'content'"""
        params: list[object] = [match_query]
        candidate_limit = max(top_k * DEFAULT_OVERFETCH_FACTOR, DEFAULT_RERANK_POOL_SIZE)
        sql += " ORDER BY score LIMIT ?"
        params.append(candidate_limit)
        with self.connect() as connection:
            if connection.execute("SELECT 1 FROM kb_meta WHERE key='rechunk_target_snapshot_id'").fetchone():
                raise RuntimeError("KB index maintenance is in progress")
            rows = connection.execute(sql, params).fetchall()
        # RedAmon applies source allowlists after asking Lucene for its global
        # top-k pool. Filtering in SQL before LIMIT would backfill lower-ranked
        # family-specific rows and change the RRF candidates.
        requested_sources = set(source_names)
        if requested_sources:
            rows = [row for row in rows if row["source_name"] in requested_sources]
        fulltext_ids = [row["chunk_id"] for row in rows]
        fulltext_scores = {row["chunk_id"]: float(row["score"]) for row in rows}
        vector_ids, vector_scores = self._vector_search(
            query, top_k=candidate_limit,
        )
        candidate_ids = list(dict.fromkeys([*fulltext_ids, *vector_ids]))
        if not candidate_ids:
            return ()
        metadata_sql = """SELECT c.chunk_id,c.source_name,c.source_id,c.title,c.text,c.source_locator,c.source_url,
                               c.source_revision,c.license_id,c.attribution,c.source_sha256,c.chunk_sha256,c.metadata_json,
                               c.start_char,c.end_char
                        FROM chunks c WHERE c.chunk_id IN (""" + ",".join("?" for _ in candidate_ids) + ")"
        metadata_params: list[object] = list(candidate_ids)
        metadata_sql += " AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content'"
        if requested_sources:
            metadata_sql += " AND c.source_name IN (" + ",".join("?" for _ in requested_sources) + ")"
            metadata_params.extend(requested_sources)
        with self.connect() as connection:
            candidate_rows = connection.execute(metadata_sql, metadata_params).fetchall()
        hits_by_id: dict[str, KBHit] = {}
        rerank_text_by_id: dict[str, str] = {}
        candidates: list[RankedCandidate] = []
        for row in candidate_rows:
            text = row["text"]
            excerpt = text[:1200]
            hit = KBHit(
                chunk_id=row["chunk_id"], source_name=row["source_name"], source_id=row["source_id"],
                title=row["title"], excerpt=excerpt, source_locator=row["source_locator"],
                source_url=row["source_url"], source_revision=row["source_revision"],
                license_id=row["license_id"], attribution=row["attribution"],
                source_sha256=row["source_sha256"], chunk_sha256=row["chunk_sha256"],
                metadata=json.loads(row["metadata_json"]),
                start_char=row["start_char"], end_char=row["end_char"],
                bm25=fulltext_scores.get(row["chunk_id"], 0.0), rank_score=0.0,
                vector_score=vector_scores.get(row["chunk_id"]),
            )
            hits_by_id[hit.chunk_id] = hit
            rerank_text_by_id[hit.chunk_id] = text[:1920]
            candidates.append(RankedCandidate(
                chunk_id=hit.chunk_id, source_name=hit.source_name, title=hit.title,
                lexical_score=hit.bm25 if hit.chunk_id in fulltext_scores else None,
                vector_score=hit.vector_score,
            ))
        rerank_scores: dict[str, float] | None = None
        pre_ranked = rank_candidates(
            candidates,
            vector_ids=vector_ids,
            fulltext_ids=fulltext_ids,
            top_k=max(len(candidates), 1),
            mmr_enabled=False,
        )
        reranker = self._get_reranker() if len(pre_ranked) > 1 else None
        scorer = getattr(reranker, "score", None)
        if callable(scorer) and len(pre_ranked) > 1:
            pool = pre_ranked[:DEFAULT_RERANK_POOL_SIZE]
            try:
                scores = scorer(query, [rerank_text_by_id[item.chunk_id] for item in pool])
                if len(scores) == len(pool):
                    rerank_scores = {item.chunk_id: float(score) for item, score in zip(pool, scores)}
            except Exception as exc:
                logger.info("KB cross-encoder unavailable; retaining fused ranking (%s)", type(exc).__name__)
        ranked = rank_candidates(
            pre_ranked,
            vector_ids=vector_ids,
            fulltext_ids=fulltext_ids,
            top_k=top_k,
            rerank_scores=rerank_scores,
        )
        return tuple(
            replace(hits_by_id[item.chunk_id], rank_score=item.score, rerank_score=item.rerank_score)
            for item in ranked
        )

    @staticmethod
    def _validate_chunk(chunk: CanonicalChunk) -> None:
        if not chunk.chunk_id or not chunk.source_name or not chunk.source_id or not chunk.text.strip():
            raise ValueError("Incomplete KB chunk record")
        if (
            not isinstance(chunk.ordinal, int) or isinstance(chunk.ordinal, bool) or chunk.ordinal < 0
            or not isinstance(chunk.start_char, int) or isinstance(chunk.start_char, bool) or chunk.start_char < 0
            or not isinstance(chunk.end_char, int) or isinstance(chunk.end_char, bool)
            or chunk.end_char < chunk.start_char
            or (chunk.metadata.get("source_span_exact", True)
                and chunk.end_char - chunk.start_char != len(chunk.text))
        ):
            raise ValueError("KB chunk span does not match its text length")
        expected_chunk_sha256 = hashlib.sha256(chunk.text.encode("utf-8")).hexdigest()
        if chunk.chunk_sha256 != expected_chunk_sha256:
            raise ValueError("KB chunk content hash does not match its text")
        if not re.fullmatch(r"[a-f0-9]{64}", chunk.source_sha256):
            raise ValueError("KB chunk source hash is invalid")
        identity = {
            "schema": chunk.chunk_schema,
            "source_name": chunk.source_name,
            "source_id": chunk.source_id,
            "source_sha256": chunk.source_sha256,
            "ordinal": chunk.ordinal,
            "start_char": chunk.start_char,
            "end_char": chunk.end_char,
            "chunk_sha256": chunk.chunk_sha256,
        }
        unique_key = chunk.metadata.get("redamon_unique_key")
        expected_chunk_id = (
            redamon_chunk_id(chunk.source_name, unique_key)
            if isinstance(unique_key, str) and unique_key
            else hashlib.sha256(
                json.dumps(identity, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
            ).hexdigest()
        )
        if chunk.chunk_schema != CHUNK_SCHEMA or chunk.chunk_id != expected_chunk_id:
            raise ValueError("KB chunk identity does not match its provenance")
