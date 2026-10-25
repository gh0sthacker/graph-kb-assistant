"""Local SQLite FTS5 index for canonical KB chunks and verifiable citations."""
from __future__ import annotations

from dataclasses import dataclass
from contextlib import contextmanager, nullcontext
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
from typing import Iterable, Iterator, Mapping

from ..paths import data_root, ensure_private_directory
from ..project_lock import project_maintenance_lock, project_maintenance_read_lock
from .canonical import CHUNK_SCHEMA, CanonicalChunk, SourceDocument


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
CREATE VIRTUAL TABLE IF NOT EXISTS chunk_search USING fts5(
  chunk_id UNINDEXED, source_name UNINDEXED, title, text,
  tokenize='unicode61 remove_diacritics 2'
);
CREATE INDEX IF NOT EXISTS chunks_source_idx ON chunks(source_name,source_id);
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


class KBIndex:
    """Lexical retrieval only; vector ranking can be added behind this API."""

    def __init__(self, path: Path | None = None, *, read_only: bool = False) -> None:
        requested_path = (path if path is not None else data_root() / "kb" / "index.sqlite3").expanduser()
        if read_only and Path(os.path.abspath(requested_path)) != requested_path.resolve():
            raise ValueError("Read-only KB index path must not contain symbolic links")
        self.path = requested_path.resolve()
        self.read_only = bool(read_only)
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

    def replace_snapshot(
        self,
        chunks: Iterable[CanonicalChunk],
        snapshot_id: str,
        *,
        sources: Iterable[SourceDocument] = (),
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
        source_coverage_state: str = "unverified",
        source_coverage_report_sha256: str | None = None,
        source_coverage_report: Mapping[str, object] | None = None,
    ) -> int:
        if not isinstance(snapshot_id, str) or not re.fullmatch(r"[a-f0-9]{64}", snapshot_id):
            raise ValueError("snapshot_id must be a lowercase SHA-256 digest")
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
                if chunk.source_sha256 != source.content_sha256 or source.text[chunk.start_char:chunk.end_char] != chunk.text:
                    raise ValueError("KB chunk does not match its source document span")

        count = 0
        with self.connect(_maintenance_lock_held=True) as connection:
            connection.execute("DELETE FROM chunk_search")
            connection.execute("DELETE FROM chunks")
            connection.execute("DELETE FROM source_documents")
            for source in source_rows:
                connection.execute(
                    """INSERT INTO source_documents(
                         source_name,source_id,title,source_url,source_locator,source_revision,
                         license_id,attribution,metadata_json,content_sha256,text
                       ) VALUES(?,?,?,?,?,?,?,?,?,?,?)""",
                    (
                        source.source_name, source.source_id, source.title, source.source_url,
                        source.source_locator, source.source_revision, source.license_id,
                        source.attribution,
                        json.dumps(source.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
                        source.content_sha256, source.text,
                    ),
                )
            for chunk in chunk_rows:
                connection.execute(
                    """INSERT INTO chunks(
                         chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,
                         source_revision,license_id,attribution,metadata_json,source_sha256,chunk_sha256,
                         chunk_schema,text
                       ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                    (
                        chunk.chunk_id, chunk.source_name, chunk.source_id, chunk.title, chunk.start_char,
                        chunk.end_char, chunk.source_locator, chunk.source_url, chunk.source_revision,
                        chunk.license_id, chunk.attribution,
                        json.dumps(chunk.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
                        chunk.source_sha256, chunk.chunk_sha256, chunk.chunk_schema, chunk.text,
                    ),
                )
                connection.execute(
                    "INSERT INTO chunk_search(chunk_id,source_name,title,text) VALUES(?,?,?,?)",
                    (chunk.chunk_id, chunk.source_name, chunk.title, chunk.text),
                )
                count += 1
            connection.execute(
                "DELETE FROM kb_meta WHERE key IN ('snapshot_id','chunk_count','coverage_state','coverage_report_sha256','source_coverage')"
            )
            connection.executemany(
                "INSERT INTO kb_meta(key,value) VALUES(?,?)",
                (
                    ("snapshot_id", snapshot_id),
                    ("chunk_count", str(count)),
                    ("coverage_state", source_coverage_state),
                    ("coverage_report_sha256", source_coverage_report_sha256 or ""),
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

    def get_source(self, source_name: str, source_id: str) -> dict[str, object] | None:
        with self.connect() as connection:
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
            meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
        try:
            source_coverage = json.loads(meta.get("source_coverage", "[]"))
        except json.JSONDecodeError:
            source_coverage = []
        return {
            "source_families": source_count,
            "source_documents": document_count,
            "chunks": chunk_count,
            "snapshot_id": meta.get("snapshot_id"),
            "coverage_state": meta.get("coverage_state", "unverified"),
            "coverage_report_sha256": meta.get("coverage_report_sha256") or None,
            "source_coverage": source_coverage,
        }

    def search(self, query: str, *, top_k: int = 10, source_names: tuple[str, ...] = ()) -> tuple[KBHit, ...]:
        if not (1 <= top_k <= 100):
            raise ValueError("top_k must be between 1 and 100")
        if len(query) > 4096:
            raise ValueError("KB query exceeds the 4096-character limit")
        terms = re.findall(r"[\w-]{2,64}", query, flags=re.UNICODE)[:32]
        if not terms:
            return ()
        match_query = " OR ".join('"' + term.replace('"', '""') + '"*' for term in terms)
        sql = """SELECT c.chunk_id,c.source_name,c.source_id,c.title,c.text,c.source_locator,c.source_url,
                        c.source_revision,c.license_id,c.attribution,c.source_sha256,c.chunk_sha256,c.metadata_json,
                        c.start_char,c.end_char,bm25(chunk_search,0.0,0.0,4.0,1.0) AS score
                 FROM chunk_search JOIN chunks c ON c.chunk_id=chunk_search.chunk_id
                 WHERE chunk_search MATCH ?
                   AND COALESCE(json_extract(c.metadata_json,'$.document_role'),'content') = 'content'"""
        params: list[object] = [match_query]
        if source_names:
            sql += " AND c.source_name IN (" + ",".join("?" for _ in source_names) + ")"
            params.extend(source_names)
        sql += " ORDER BY score LIMIT ?"
        params.append(top_k)
        with self.connect() as connection:
            rows = connection.execute(sql, params).fetchall()
        hits = []
        for row in rows:
            text = row["text"]
            excerpt = text[:1200]
            hits.append(KBHit(
                chunk_id=row["chunk_id"], source_name=row["source_name"], source_id=row["source_id"],
                title=row["title"], excerpt=excerpt, source_locator=row["source_locator"],
                source_url=row["source_url"], source_revision=row["source_revision"],
                license_id=row["license_id"], attribution=row["attribution"],
                source_sha256=row["source_sha256"], chunk_sha256=row["chunk_sha256"],
                metadata=json.loads(row["metadata_json"]),
                start_char=row["start_char"], end_char=row["end_char"], bm25=row["score"],
            ))
        return tuple(hits)

    @staticmethod
    def _validate_chunk(chunk: CanonicalChunk) -> None:
        if not chunk.chunk_id or not chunk.source_name or not chunk.source_id or not chunk.text.strip():
            raise ValueError("Incomplete KB chunk record")
        if (
            not isinstance(chunk.ordinal, int) or isinstance(chunk.ordinal, bool) or chunk.ordinal < 0
            or not isinstance(chunk.start_char, int) or isinstance(chunk.start_char, bool) or chunk.start_char < 0
            or not isinstance(chunk.end_char, int) or isinstance(chunk.end_char, bool)
            or chunk.end_char - chunk.start_char != len(chunk.text)
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
        expected_chunk_id = hashlib.sha256(
            json.dumps(identity, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        if chunk.chunk_schema != CHUNK_SCHEMA or chunk.chunk_id != expected_chunk_id:
            raise ValueError("KB chunk identity does not match its provenance")
