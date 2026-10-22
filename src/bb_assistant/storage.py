"""Per-project SQLite metadata store and AES-GCM raw capture vault."""
from __future__ import annotations

import base64
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import tempfile
from typing import Iterator
import uuid

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from .domain import CaidoProjectRef, HttpExchange
from .paths import data_root, ensure_private_directory
from .registry import register_project


_MAGIC = b"BBA1"
_HEX64 = re.compile(r"^[a-f0-9]{64}$")
_SCHEMA_VERSION = 5
_SCHEMA = """
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS project_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_state (
  source_key TEXT PRIMARY KEY,
  cursor TEXT,
  last_request_id TEXT,
  run_id TEXT,
  state TEXT NOT NULL CHECK (state IN ('new','running','partial','done','failed')),
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_runs (
  run_id TEXT PRIMARY KEY,
  source_key TEXT NOT NULL,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  page_count INTEGER NOT NULL DEFAULT 0 CHECK (page_count >= 0),
  distinct_request_count INTEGER NOT NULL DEFAULT 0 CHECK (distinct_request_count >= 0),
  state TEXT NOT NULL CHECK (state IN ('running','partial','done'))
);
CREATE INDEX IF NOT EXISTS sync_runs_source_idx ON sync_runs(source_key, started_at);
CREATE TABLE IF NOT EXISTS sync_run_cursor_digests (
  run_id TEXT NOT NULL REFERENCES sync_runs(run_id),
  cursor_digest TEXT NOT NULL,
  PRIMARY KEY (run_id, cursor_digest)
);
CREATE TABLE IF NOT EXISTS sync_run_request_digests (
  run_id TEXT NOT NULL REFERENCES sync_runs(run_id),
  request_id_digest TEXT NOT NULL,
  PRIMARY KEY (run_id, request_id_digest)
);
CREATE TABLE IF NOT EXISTS captures (
  observation_id TEXT PRIMARY KEY,
  caido_instance_id TEXT NOT NULL,
  caido_project_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  captured_at TEXT,
  request_sha256 TEXT NOT NULL,
  response_sha256 TEXT,
  request_size INTEGER NOT NULL,
  response_size INTEGER,
  current_revision_id TEXT,
  request_vault_ref TEXT NOT NULL,
  response_vault_ref TEXT,
  parse_state TEXT NOT NULL DEFAULT 'not_parsed',
  imported_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE (caido_instance_id, caido_project_id, request_id)
);
CREATE TABLE IF NOT EXISTS graph_capture_map (
  observation_id TEXT PRIMARY KEY REFERENCES captures(observation_id),
  revision_id TEXT NOT NULL,
  graph_observation_id TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS capture_revisions (
  revision_id TEXT PRIMARY KEY,
  observation_id TEXT NOT NULL REFERENCES captures(observation_id),
  request_sha256 TEXT NOT NULL,
  response_sha256 TEXT,
  request_size INTEGER NOT NULL,
  response_size INTEGER,
  request_vault_ref TEXT NOT NULL,
  response_vault_ref TEXT,
  captured_at TEXT,
  request_truncated INTEGER CHECK (request_truncated IS NULL OR request_truncated IN (0,1)),
  response_truncated INTEGER CHECK (response_truncated IS NULL OR response_truncated IN (0,1)),
  request_wire_fidelity TEXT NOT NULL DEFAULT 'unknown'
    CHECK (request_wire_fidelity IN ('source_bytes','reconstructed_text','unknown')),
  response_wire_fidelity TEXT NOT NULL DEFAULT 'unknown'
    CHECK (response_wire_fidelity IN ('source_bytes','reconstructed_text','unknown')),
  first_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS graph_capture_revisions (
  revision_id TEXT PRIMARY KEY REFERENCES capture_revisions(revision_id),
  graph_observation_id TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS captures_request_id_idx ON captures(request_id);
CREATE INDEX IF NOT EXISTS captures_time_idx ON captures(captured_at);
CREATE TABLE IF NOT EXISTS capture_search (
  observation_id TEXT PRIMARY KEY REFERENCES captures(observation_id),
  method TEXT NOT NULL,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  route TEXT NOT NULL,
  response_status INTEGER,
  request_content_type TEXT NOT NULL,
  response_content_type TEXT NOT NULL,
  field_names_json TEXT NOT NULL,
  header_names_json TEXT NOT NULL,
  captured_at TEXT
);
CREATE TABLE IF NOT EXISTS live_sync_state (
  source_key TEXT PRIMARY KEY,
  watermark TEXT,
  window_since TEXT,
  window_until TEXT,
  cursor TEXT,
  state TEXT NOT NULL CHECK (state IN ('new','running','partial','idle','failed')),
  last_polled_at TEXT,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS live_capture_index (
  observation_id TEXT PRIMARY KEY,
  caido_instance_id TEXT NOT NULL,
  caido_project_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  graph_observation_id TEXT NOT NULL,
  synced_at TEXT NOT NULL,
  method TEXT NOT NULL,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  route TEXT NOT NULL,
  response_status INTEGER,
  request_content_type TEXT NOT NULL,
  response_content_type TEXT NOT NULL,
  request_size INTEGER NOT NULL,
  response_size INTEGER,
  request_signature TEXT NOT NULL,
  response_signature TEXT,
  request_url_key TEXT NOT NULL,
  referer_url_key TEXT,
  redirect_url_key TEXT,
  field_names_json TEXT NOT NULL,
  header_names_json TEXT NOT NULL,
  parse_state TEXT NOT NULL CHECK (parse_state IN ('complete','partial')),
  UNIQUE (caido_instance_id, caido_project_id, request_id)
);
CREATE INDEX IF NOT EXISTS live_capture_host_idx ON live_capture_index(host);
CREATE INDEX IF NOT EXISTS live_capture_route_idx ON live_capture_index(route);
CREATE INDEX IF NOT EXISTS live_capture_status_idx ON live_capture_index(response_status);
CREATE INDEX IF NOT EXISTS live_capture_synced_idx ON live_capture_index(synced_at);
CREATE INDEX IF NOT EXISTS live_capture_request_url_idx ON live_capture_index(request_url_key);
CREATE INDEX IF NOT EXISTS live_capture_redirect_url_idx ON live_capture_index(redirect_url_key);
CREATE VIRTUAL TABLE IF NOT EXISTS live_capture_fts USING fts5(
  observation_id UNINDEXED, method, host, route, fields, headers,
  tokenize='unicode61 remove_diacritics 2'
);
CREATE INDEX IF NOT EXISTS capture_search_method_idx ON capture_search(method);
CREATE INDEX IF NOT EXISTS capture_search_host_idx ON capture_search(host);
CREATE INDEX IF NOT EXISTS capture_search_route_idx ON capture_search(route);
CREATE INDEX IF NOT EXISTS capture_search_endpoint_idx ON capture_search(host,port,route);
CREATE INDEX IF NOT EXISTS capture_search_status_idx ON capture_search(response_status);
CREATE VIRTUAL TABLE IF NOT EXISTS capture_search_fts USING fts5(
  observation_id UNINDEXED, method, host, route, fields, headers,
  tokenize='unicode61 remove_diacritics 2'
);
"""


def _utc_now() -> str:
    from datetime import datetime, timezone

    return datetime.now(timezone.utc).isoformat()


def decode_vault_key(value: str | None = None) -> bytes:
    encoded = value if value is not None else os.environ.get("BB_ASSISTANT_VAULT_KEY")
    if not encoded:
        raise ValueError("Set BB_ASSISTANT_VAULT_KEY to a Base64URL-encoded 32-byte key")
    if not re.fullmatch(r"[A-Za-z0-9_-]+={0,2}", encoded):
        raise ValueError("BB_ASSISTANT_VAULT_KEY is not valid Base64URL")
    try:
        key = base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4))
    except Exception as exc:
        raise ValueError("BB_ASSISTANT_VAULT_KEY is not valid Base64URL") from exc
    if len(key) != 32:
        raise ValueError("BB_ASSISTANT_VAULT_KEY must decode to exactly 32 bytes")
    return key


class ProjectStore:
    """One project's database and encrypted byte vault."""

    def __init__(self, project: CaidoProjectRef, root: Path | None = None) -> None:
        self.project = project
        self.project_key = project.storage_key
        base = data_root() if root is None else root.expanduser().resolve()
        source_root = Path(__file__).resolve().parents[2]
        if base == source_root or source_root in base.parents:
            raise ValueError("Runtime data must be outside the source checkout")
        self.directory = ensure_private_directory(base / "projects" / self.project_key)
        if base not in self.directory.parents:
            raise ValueError("Project data directory resolved outside the configured data root")
        self.vault_directory = ensure_private_directory(self.directory / "vault")
        if self.vault_directory.parent != self.directory:
            raise ValueError("Vault directory resolved outside the project namespace")
        self.db_path = self.directory / "graph.sqlite3"
        if self.db_path.is_symlink():
            raise ValueError("Project database path must not be a symbolic link")
        self.behavior_graph_path = self.directory / "behavior.sqlite3"
        if self.behavior_graph_path.is_symlink():
            raise ValueError("Behavior graph path must not be a symbolic link")
        self.behavior_receipt_directory = self.directory / "behavior-receipts"
        self._initialize()
        register_project(self.project, base)
        try:
            self.db_path.chmod(0o600)
        except OSError:
            pass

    @contextmanager
    def connect(self) -> Iterator[sqlite3.Connection]:
        connection = sqlite3.connect(self.db_path, timeout=30)
        try:
            connection.row_factory = sqlite3.Row
            connection.execute("PRAGMA foreign_keys = ON")
            connection.execute("PRAGMA busy_timeout = 30000")
            connection.execute("PRAGMA synchronous = FULL")
            with connection:
                yield connection
        finally:
            connection.close()

    def _initialize(self) -> None:
        with self.connect() as connection:
            connection.executescript(_SCHEMA)
            connection.execute("PRAGMA journal_mode = WAL")
            current_version = connection.execute("PRAGMA user_version").fetchone()[0]
            if current_version not in (0, 1, 2, 3, 4, _SCHEMA_VERSION):
                raise RuntimeError(f"Unsupported project database schema version {current_version}")
            if current_version < _SCHEMA_VERSION:
                columns = {
                    row["name"]
                    for row in connection.execute("PRAGMA table_info(sync_state)").fetchall()
                }
                if "run_id" not in columns:
                    connection.execute("ALTER TABLE sync_state ADD COLUMN run_id TEXT")
                live_columns = {
                    row["name"] for row in connection.execute("PRAGMA table_info(live_sync_state)").fetchall()
                }
                if live_columns and "window_until" not in live_columns:
                    connection.execute("ALTER TABLE live_sync_state ADD COLUMN window_until TEXT")
                capture_columns = {
                    row["name"] for row in connection.execute("PRAGMA table_info(captures)").fetchall()
                }
                if "current_revision_id" not in capture_columns:
                    connection.execute("ALTER TABLE captures ADD COLUMN current_revision_id TEXT")
                revision_columns = {
                    row["name"] for row in connection.execute("PRAGMA table_info(capture_revisions)").fetchall()
                }
                for column, declaration in (
                    ("captured_at", "TEXT"),
                    ("request_truncated", "INTEGER"),
                    ("response_truncated", "INTEGER"),
                    ("request_wire_fidelity", "TEXT NOT NULL DEFAULT 'unknown'"),
                    ("response_wire_fidelity", "TEXT NOT NULL DEFAULT 'unknown'"),
                ):
                    if column not in revision_columns:
                        connection.execute(f"ALTER TABLE capture_revisions ADD COLUMN {column} {declaration}")
                graph_map_columns = {
                    row["name"] for row in connection.execute("PRAGMA table_info(graph_capture_map)").fetchall()
                }
                if "revision_id" not in graph_map_columns:
                    connection.execute("ALTER TABLE graph_capture_map ADD COLUMN revision_id TEXT")
                # Upgrade the old content-only revision identity. Existing graph
                # seeds are exposed only when their graph revision matches the
                # capture row's current revision.
                if current_version in (0, 1, 2):
                    for capture in connection.execute(
                        "SELECT observation_id,request_sha256,response_sha256 FROM captures"
                    ).fetchall():
                        legacy_payload = (
                            f"{capture['observation_id']}:{capture['request_sha256']}:"
                            f"{capture['response_sha256'] or 'missing'}"
                        ).encode("ascii")
                        legacy_revision = hashlib.sha256(legacy_payload).hexdigest()
                        exists = connection.execute(
                            "SELECT 1 FROM capture_revisions WHERE revision_id=? AND observation_id=?",
                            (legacy_revision, capture["observation_id"]),
                        ).fetchone()
                        if exists is None:
                            continue
                        connection.execute(
                            "UPDATE captures SET current_revision_id=? WHERE observation_id=?",
                            (legacy_revision, capture["observation_id"]),
                        )
                        graph_revision = connection.execute(
                            "SELECT graph_observation_id FROM graph_capture_revisions WHERE revision_id=?",
                            (legacy_revision,),
                        ).fetchone()
                        graph_map = connection.execute(
                            "SELECT graph_observation_id FROM graph_capture_map WHERE observation_id=?",
                            (capture["observation_id"],),
                        ).fetchone()
                        if graph_revision is not None and graph_map is not None and graph_revision[0] == graph_map[0]:
                            connection.execute(
                                "UPDATE graph_capture_map SET revision_id=? WHERE observation_id=?",
                                (legacy_revision, capture["observation_id"]),
                            )
            connection.execute(f"PRAGMA user_version = {_SCHEMA_VERSION}")
            connection.execute(
                "INSERT OR IGNORE INTO project_meta(key,value) VALUES('project_key',?)",
                (self.project_key,),
            )

    def live_sync_progress(self, source_key: str) -> dict[str, str | None] | None:
        if not isinstance(source_key, str) or not source_key:
            raise ValueError("Live sync source key must be non-empty")
        with self.connect() as connection:
            row = connection.execute(
                "SELECT watermark,window_since,window_until,cursor,state,last_polled_at,updated_at "
                "FROM live_sync_state WHERE source_key=?", (source_key,)
            ).fetchone()
        return dict(row) if row is not None else None

    def save_live_sync_progress(
        self,
        source_key: str,
        *,
        watermark: str | None,
        window_since: str | None,
        window_until: str | None,
        cursor: str | None,
        state: str,
        last_polled_at: str | None = None,
    ) -> None:
        if not isinstance(source_key, str) or not source_key:
            raise ValueError("Live sync source key must be non-empty")
        if state not in {"new", "running", "partial", "idle", "failed"}:
            raise ValueError("Invalid live sync state")
        for value in (watermark, window_since, window_until, cursor, last_polled_at):
            if value is not None and not isinstance(value, str):
                raise ValueError("Live sync checkpoint fields must be strings or null")
        with self.connect() as connection:
            connection.execute(
                "INSERT INTO live_sync_state(source_key,watermark,window_since,window_until,cursor,state,last_polled_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(source_key) DO UPDATE SET "
                "watermark=excluded.watermark,window_since=excluded.window_since,window_until=excluded.window_until,cursor=excluded.cursor,"
                "state=excluded.state,last_polled_at=excluded.last_polled_at,updated_at=excluded.updated_at",
                (source_key, watermark, window_since, window_until, cursor, state, last_polled_at, _utc_now()),
            )

    def known_live_request_ids(self, request_ids: tuple[str, ...]) -> frozenset[str]:
        if not request_ids:
            return frozenset()
        if len(request_ids) > 100 or any(not isinstance(value, str) or not value for value in request_ids):
            raise ValueError("Live request ID lookup must contain at most 100 non-empty IDs")
        placeholders = ",".join("?" for _ in request_ids)
        with self.connect() as connection:
            rows = connection.execute(
                "SELECT request_id FROM live_capture_index WHERE caido_instance_id=? "
                "AND caido_project_id=? AND request_id IN (" + placeholders + ") "
                "UNION SELECT request_id FROM captures WHERE caido_instance_id=? "
                "AND caido_project_id=? AND request_id IN (" + placeholders + ")",
                (self.project.instance_id, self.project.project_id, *request_ids,
                 self.project.instance_id, self.project.project_id, *request_ids),
            ).fetchall()
        return frozenset(row[0] for row in rows)

    def live_causal_candidates(self, url_keys: tuple[str, ...]) -> list[dict[str, str]]:
        if not url_keys:
            return []
        if len(url_keys) > 200 or any(not isinstance(value, str) or not _HEX64.fullmatch(value) for value in url_keys):
            raise ValueError("Live URL-key lookup is invalid")
        placeholders = ",".join("?" for _ in url_keys)
        with self.connect() as connection:
            rows = connection.execute(
                "SELECT request_url_key,redirect_url_key,graph_observation_id,synced_at,host,port,route "
                "FROM live_capture_index WHERE caido_instance_id=? AND caido_project_id=? AND "
                "(request_url_key IN (" + placeholders + ") OR redirect_url_key IN (" + placeholders + ")) "
                "ORDER BY synced_at DESC LIMIT 400",
                (self.project.instance_id, self.project.project_id, *url_keys, *url_keys),
            ).fetchall()
        return [dict(row) for row in rows]

    def historical_causal_candidates(
        self, endpoint_keys: tuple[tuple[str, int, str], ...]
    ) -> list[dict[str, str | int]]:
        """Find imported-history observations matching a normalized Referer route."""
        endpoints = tuple(dict.fromkeys(endpoint_keys))
        if not endpoints:
            return []
        if len(endpoints) > 100 or any(
            not isinstance(host, str) or not host or type(port) is not int or not 1 <= port <= 65535
            or not isinstance(route, str) or not route.startswith("/") or len(route) > 2048
            for host, port, route in endpoints
        ):
            raise ValueError("Historical causal endpoint lookup is invalid")
        clauses = " OR ".join("(s.host=? AND s.port=? AND s.route=?)" for _ in endpoints)
        parameters: list[object] = [self.project.instance_id, self.project.project_id]
        for host, port, route in endpoints:
            parameters.extend((host, port, route))
        with self.connect() as connection:
            rows = connection.execute(
                "SELECT s.host,s.port,s.route,m.graph_observation_id "
                "FROM capture_search s "
                "JOIN captures c ON c.observation_id=s.observation_id "
                "JOIN graph_capture_map m ON m.observation_id=s.observation_id "
                "WHERE c.caido_instance_id=? AND c.caido_project_id=? AND (" + clauses + ") "
                "ORDER BY c.captured_at DESC,c.request_id LIMIT 400",
                parameters,
            ).fetchall()
        return [dict(row) for row in rows]

    def save_live_graph_captures(self, records: list[dict[str, object]]) -> int:
        """Commit one bounded page of graph references and structural search rows."""
        if not isinstance(records, list) or len(records) > 100:
            raise ValueError("Live graph page must contain at most 100 records")
        inserted = 0
        with self.connect() as connection:
            for record in records:
                required_strings = (
                    "observation_id", "request_id", "graph_observation_id", "synced_at",
                    "request_signature", "request_url_key", "parse_state",
                )
                if any(not isinstance(record.get(key), str) for key in required_strings):
                    raise ValueError("Live graph record is incomplete")
                for key in ("observation_id", "request_signature", "request_url_key"):
                    if not _HEX64.fullmatch(str(record[key])):
                        raise ValueError("Live graph record contains an invalid digest")
                for key in ("response_signature", "referer_url_key", "redirect_url_key"):
                    value = record.get(key)
                    if value is not None and (not isinstance(value, str) or not _HEX64.fullmatch(value)):
                        raise ValueError("Live graph record contains an invalid optional digest")
                if record["parse_state"] not in {"complete", "partial"}:
                    raise ValueError("Live graph parse state is invalid")
                search = record.get("search_record")
                if not isinstance(search, dict):
                    raise ValueError("Live graph search projection is missing")
                field_names = search.get("field_names")
                header_names = search.get("header_names")
                if not isinstance(field_names, list) or not isinstance(header_names, list):
                    raise ValueError("Live graph search projection is invalid")
                if any(not isinstance(search.get(key), str) for key in (
                    "method", "host", "route", "request_content_type", "response_content_type"
                )):
                    raise ValueError("Live graph search metadata is incomplete")
                result = connection.execute(
                    """INSERT OR IGNORE INTO live_capture_index(
                       observation_id,caido_instance_id,caido_project_id,request_id,graph_observation_id,synced_at,
                       method,host,port,route,response_status,request_content_type,response_content_type,
                       request_size,response_size,request_signature,response_signature,request_url_key,
                       referer_url_key,redirect_url_key,field_names_json,header_names_json,parse_state
                       ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                    (
                        record["observation_id"], self.project.instance_id, self.project.project_id,
                        record["request_id"], record["graph_observation_id"], record["synced_at"],
                        search["method"], search["host"], search["port"], search["route"],
                        search["response_status"], search["request_content_type"], search["response_content_type"],
                        record["request_size"], record["response_size"], record["request_signature"],
                        record.get("response_signature"), record["request_url_key"],
                        record.get("referer_url_key"), record.get("redirect_url_key"),
                        json.dumps(field_names, separators=(",", ":")),
                        json.dumps(header_names, separators=(",", ":")), record["parse_state"],
                    ),
                )
                if result.rowcount:
                    fields_text = " ".join(str(value) for value in field_names)
                    headers_text = " ".join(str(value) for value in header_names)
                    connection.execute(
                        "INSERT INTO live_capture_fts(observation_id,method,host,route,fields,headers) "
                        "VALUES(?,?,?,?,?,?)",
                        (record["observation_id"], search["method"], search["host"], search["route"], fields_text, headers_text),
                    )
                    inserted += 1
        return inserted

    def begin_sync_run(self, source_key: str) -> dict[str, str | int | None]:
        """Resume an interrupted run with its counters, or create a fresh run."""
        if not isinstance(source_key, str) or not source_key:
            raise ValueError("Sync source key must be non-empty")
        now = _utc_now()
        with self.connect() as connection:
            state = connection.execute(
                "SELECT cursor,state,run_id FROM sync_state WHERE source_key=?",
                (source_key,),
            ).fetchone()
            if state is not None and state["state"] in {"partial", "running"} and state["run_id"]:
                run = connection.execute(
                    "SELECT started_at,page_count,distinct_request_count,state FROM sync_runs WHERE run_id=? AND source_key=?",
                    (state["run_id"], source_key),
                ).fetchone()
                if run is not None and run["state"] in {"partial", "running"}:
                    connection.execute(
                        "UPDATE sync_state SET state='running',updated_at=? WHERE source_key=?",
                        (now, source_key),
                    )
                    connection.execute(
                        "UPDATE sync_runs SET state='running',updated_at=? WHERE run_id=?",
                        (now, state["run_id"]),
                    )
                    return {
                        "run_id": state["run_id"],
                        "started_at": run["started_at"],
                        "cursor": state["cursor"],
                        "page_count": run["page_count"],
                        "distinct_request_count": run["distinct_request_count"],
                    }

            run_id = uuid.uuid4().hex
            connection.execute(
                "INSERT INTO sync_runs(run_id,source_key,started_at,updated_at,page_count,distinct_request_count,state) "
                "VALUES(?,?,?, ?,0,0,'running')",
                (run_id, source_key, now, now),
            )
            connection.execute(
                "INSERT INTO sync_state(source_key,cursor,last_request_id,run_id,state,updated_at) "
                "VALUES(?,NULL,NULL,?,'running',?) "
                "ON CONFLICT(source_key) DO UPDATE SET cursor=NULL,last_request_id=NULL,run_id=excluded.run_id,"
                "state='running',updated_at=excluded.updated_at",
                (source_key, run_id, now),
            )
            return {
                "run_id": run_id,
                "started_at": now,
                "cursor": None,
                "page_count": 0,
                "distinct_request_count": 0,
            }

    def sync_progress(self, source_key: str) -> dict[str, str | int | None] | None:
        """Return the current resumable cursor and cumulative run evidence."""
        with self.connect() as connection:
            row = connection.execute(
                "SELECT s.cursor,s.state,s.run_id,r.started_at,r.page_count,r.distinct_request_count "
                "FROM sync_state AS s LEFT JOIN sync_runs AS r ON r.run_id=s.run_id "
                "WHERE s.source_key=?",
                (source_key,),
            ).fetchone()
        if row is None:
            return None
        return {
            "cursor": row["cursor"],
            "state": row["state"],
            "run_id": row["run_id"],
            "started_at": row["started_at"],
            "page_count": row["page_count"],
            "distinct_request_count": row["distinct_request_count"],
        }

    def validate_sync_page(
        self,
        run_id: str,
        *,
        cursor_digest: str,
        next_cursor_digest: str | None,
        request_id_digests: tuple[str, ...],
        has_more: bool,
    ) -> None:
        """Fail before persistence if this page cycles or repeats prior IDs."""
        self._validate_sync_page_values(
            run_id, cursor_digest, next_cursor_digest, request_id_digests, has_more
        )
        with self.connect() as connection:
            self._validate_sync_page_connection(
                connection, run_id, cursor_digest, next_cursor_digest, request_id_digests, has_more
            )

    def commit_sync_page(
        self,
        source_key: str,
        run_id: str,
        *,
        cursor_digest: str,
        next_cursor_digest: str | None,
        request_id_digests: tuple[str, ...],
        next_cursor: str | None,
        last_request_id: str | None,
        has_more: bool,
    ) -> dict[str, int]:
        """Commit a successfully imported page and its cumulative evidence."""
        self._validate_sync_page_values(
            run_id, cursor_digest, next_cursor_digest, request_id_digests, has_more
        )
        if (has_more and next_cursor is None) or (not has_more and next_cursor is not None):
            raise ValueError("Sync page cursor does not match its terminal state")
        now = _utc_now()
        with self.connect() as connection:
            self._validate_sync_page_connection(
                connection, run_id, cursor_digest, next_cursor_digest, request_id_digests, has_more
            )
            connection.execute(
                "INSERT INTO sync_run_cursor_digests(run_id,cursor_digest) VALUES(?,?)",
                (run_id, cursor_digest),
            )
            connection.executemany(
                "INSERT INTO sync_run_request_digests(run_id,request_id_digest) VALUES(?,?)",
                ((run_id, digest) for digest in request_id_digests),
            )
            state = "running" if has_more else "done"
            connection.execute(
                "UPDATE sync_runs SET page_count=page_count+1,distinct_request_count=distinct_request_count+?,"
                "state=?,updated_at=? WHERE run_id=? AND source_key=?",
                (len(request_id_digests), state, now, run_id, source_key),
            )
            updated = connection.execute(
                "UPDATE sync_state SET cursor=?,last_request_id=?,state=?,updated_at=? "
                "WHERE source_key=? AND run_id=?",
                (next_cursor, last_request_id, state, now, source_key, run_id),
            )
            if updated.rowcount != 1:
                raise ValueError("Sync run is no longer active for this project")
            row = connection.execute(
                "SELECT page_count,distinct_request_count FROM sync_runs WHERE run_id=?",
                (run_id,),
            ).fetchone()
            return {"page_count": row["page_count"], "distinct_request_count": row["distinct_request_count"]}

    def mark_sync_partial(self, source_key: str, run_id: str | None) -> None:
        """Keep committed page evidence while leaving the active run resumable."""
        now = _utc_now()
        with self.connect() as connection:
            if run_id:
                connection.execute(
                    "UPDATE sync_state SET state='partial',updated_at=? WHERE source_key=? AND run_id=?",
                    (now, source_key, run_id),
                )
                connection.execute(
                    "UPDATE sync_runs SET state='partial',updated_at=? WHERE run_id=? AND source_key=?",
                    (now, run_id, source_key),
                )
            else:
                connection.execute(
                    "UPDATE sync_state SET state='partial',updated_at=? WHERE source_key=?",
                    (now, source_key),
                )

    @staticmethod
    def _validate_sync_page_values(
        run_id: str,
        cursor_digest: str,
        next_cursor_digest: str | None,
        request_id_digests: tuple[str, ...],
        has_more: bool,
    ) -> None:
        if not isinstance(run_id, str) or not run_id:
            raise ValueError("Sync run ID is invalid")
        if not isinstance(cursor_digest, str) or not _HEX64.fullmatch(cursor_digest):
            raise ValueError("Sync cursor digest is invalid")
        if next_cursor_digest is not None and (
            not isinstance(next_cursor_digest, str) or not _HEX64.fullmatch(next_cursor_digest)
        ):
            raise ValueError("Next sync cursor digest is invalid")
        if type(has_more) is not bool or (has_more and next_cursor_digest is None):
            raise ValueError("Sync page continuation is invalid")
        if any(not isinstance(digest, str) or not _HEX64.fullmatch(digest) for digest in request_id_digests):
            raise ValueError("Sync page contains an invalid request ID digest")
        if len(set(request_id_digests)) != len(request_id_digests):
            raise ValueError("Sync page contains duplicate or invalid request IDs")

    @staticmethod
    def _validate_sync_page_connection(
        connection: sqlite3.Connection,
        run_id: str,
        cursor_digest: str,
        next_cursor_digest: str | None,
        request_id_digests: tuple[str, ...],
        has_more: bool,
    ) -> None:
        run = connection.execute(
            "SELECT state FROM sync_runs WHERE run_id=?", (run_id,)
        ).fetchone()
        if run is None or run["state"] != "running":
            raise ValueError("Sync run is not active")
        seen_cursor = connection.execute(
            "SELECT 1 FROM sync_run_cursor_digests WHERE run_id=? AND cursor_digest=?",
            (run_id, cursor_digest),
        ).fetchone()
        if seen_cursor is not None:
            raise ValueError("Sync pagination repeated a cursor")
        if has_more and next_cursor_digest is not None:
            repeated_next = connection.execute(
                "SELECT 1 FROM sync_run_cursor_digests WHERE run_id=? AND cursor_digest=?",
                (run_id, next_cursor_digest),
            ).fetchone()
            if repeated_next is not None or next_cursor_digest == cursor_digest:
                raise ValueError("Sync pagination would repeat a cursor")
        if request_id_digests:
            placeholders = ",".join("?" for _ in request_id_digests)
            repeated_ids = connection.execute(
                f"SELECT 1 FROM sync_run_request_digests WHERE run_id=? AND request_id_digest IN ({placeholders}) LIMIT 1",
                (run_id, *request_id_digests),
            ).fetchone()
            if repeated_ids is not None:
                raise ValueError("Sync pagination repeated a request ID")

    def save_exchange(self, exchange: HttpExchange, key: bytes | None = None) -> dict[str, str | None]:
        return self.save_exchanges([exchange], key=key)[0]

    def save_exchanges(
        self, exchanges: list[HttpExchange], key: bytes | None = None
    ) -> list[dict[str, str | None]]:
        """Persist one bounded history page with one metadata transaction."""
        if not isinstance(exchanges, list) or len(exchanges) > 100:
            raise ValueError("Capture batch must be a list of at most 100 exchanges")
        if not exchanges:
            return []
        selected_key = decode_vault_key() if key is None else key
        if len(selected_key) != 32:
            raise ValueError("AES-256-GCM requires a 32-byte key")
        allowed_fidelity = {"source_bytes", "reconstructed_text", "unknown"}
        prepared: list[dict[str, object]] = []
        for exchange in exchanges:
            if not exchange.ref.project.same_project(self.project):
                raise ValueError("Exchange belongs to a different Caido project")
            if len(exchange.request_raw) > 128 * 1024 * 1024 or (
                exchange.response_raw is not None and len(exchange.response_raw) > 128 * 1024 * 1024
            ):
                raise ValueError("Capture side exceeds the 128 MiB ingestion limit")
            if exchange.captured_at is not None and not isinstance(exchange.captured_at, str):
                raise ValueError("Capture timestamp must be a string or null")
            for value, label in (
                (exchange.request_truncated, "request"),
                (exchange.response_truncated, "response"),
            ):
                if value is not None and type(value) is not bool:
                    raise ValueError(f"{label.title()} truncation state must be boolean or null")
            for value, label in (
                (exchange.request_wire_fidelity, "request"),
                (exchange.response_wire_fidelity, "response"),
            ):
                if value not in allowed_fidelity:
                    raise ValueError(f"{label.title()} wire fidelity is unsupported")

            request_hash = hashlib.sha256(exchange.request_raw).hexdigest()
            response_hash = hashlib.sha256(exchange.response_raw).hexdigest() if exchange.response_raw is not None else None
            response_size = len(exchange.response_raw) if exchange.response_raw is not None else None
            revision_payload = json.dumps(
                {
                    "observation_id": exchange.ref.observation_id,
                    "request_sha256": request_hash,
                    "response_sha256": response_hash,
                    "request_size": len(exchange.request_raw),
                    "response_size": response_size,
                    "captured_at": exchange.captured_at,
                    "request_truncated": exchange.request_truncated,
                    "response_truncated": exchange.response_truncated,
                    "request_wire_fidelity": exchange.request_wire_fidelity,
                    "response_wire_fidelity": exchange.response_wire_fidelity,
                },
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
            revision_id = hashlib.sha256(revision_payload).hexdigest()
            request_ref = self._write_blob(
                exchange.ref.observation_id, "request", request_hash, exchange.request_raw, selected_key
            )
            response_ref = None
            if exchange.response_raw is not None:
                response_ref = self._write_blob(
                    exchange.ref.observation_id, "response", response_hash, exchange.response_raw, selected_key
                )
            prepared.append({
                "exchange": exchange,
                "request_hash": request_hash,
                "response_hash": response_hash,
                "response_size": response_size,
                "revision_id": revision_id,
                "request_ref": request_ref,
                "response_ref": response_ref,
            })

        with self.connect() as connection:
            for item in prepared:
                exchange = item["exchange"]
                request_hash = item["request_hash"]
                response_hash = item["response_hash"]
                response_size = item["response_size"]
                revision_id = item["revision_id"]
                request_ref = item["request_ref"]
                response_ref = item["response_ref"]
                connection.execute(
                    """INSERT INTO captures (
                         observation_id,caido_instance_id,caido_project_id,request_id,captured_at,
                         request_sha256,response_sha256,request_size,response_size,request_vault_ref,response_vault_ref,current_revision_id
                       ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
                       ON CONFLICT(caido_instance_id,caido_project_id,request_id) DO UPDATE SET
                         captured_at=excluded.captured_at, request_sha256=excluded.request_sha256,
                         response_sha256=excluded.response_sha256, request_size=excluded.request_size,
                         response_size=excluded.response_size, request_vault_ref=excluded.request_vault_ref,
                         response_vault_ref=excluded.response_vault_ref,current_revision_id=excluded.current_revision_id""",
                    (
                        exchange.ref.observation_id, self.project.instance_id, self.project.project_id,
                        exchange.ref.request_id, exchange.captured_at, request_hash, response_hash,
                        len(exchange.request_raw), response_size, request_ref, response_ref, revision_id,
                    ),
                )
                connection.execute(
                    """INSERT OR IGNORE INTO capture_revisions(
                         revision_id,observation_id,request_sha256,response_sha256,request_size,response_size,
                         request_vault_ref,response_vault_ref,captured_at,request_truncated,response_truncated,
                         request_wire_fidelity,response_wire_fidelity,first_seen_at
                       ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                    (
                        revision_id, exchange.ref.observation_id, request_hash, response_hash,
                        len(exchange.request_raw), response_size, request_ref, response_ref, exchange.captured_at,
                        None if exchange.request_truncated is None else int(exchange.request_truncated),
                        None if exchange.response_truncated is None else int(exchange.response_truncated),
                        exchange.request_wire_fidelity, exchange.response_wire_fidelity, _utc_now(),
                    ),
                )
        return [
            {
                "observation_id": item["exchange"].ref.observation_id,
                "revision_id": item["revision_id"],
                "request_sha256": item["request_hash"],
                "response_sha256": item["response_hash"],
            }
            for item in prepared
        ]

    def set_graph_observation(
        self,
        observation_id: str,
        graph_observation_id: str,
        *,
        revision_id: str,
    ) -> None:
        """Map one capture revision's selected graph observation for search seeds."""
        self.set_graph_observations([(observation_id, graph_observation_id, revision_id)])

    def set_graph_observations(self, mappings: list[tuple[str, str, str]]) -> None:
        """Map one page of capture revisions in a single SQLite transaction."""
        if not isinstance(mappings, list) or len(mappings) > 100:
            raise ValueError("Graph mapping batch must be a list of at most 100 items")
        if not mappings:
            return
        for observation_id, graph_observation_id, revision_id in mappings:
            if not _HEX64.fullmatch(observation_id):
                raise ValueError("Invalid capture observation ID")
            if not _HEX64.fullmatch(revision_id):
                raise ValueError("Invalid capture revision ID")
            if not re.fullmatch(r"observation-[a-f0-9]{64}", graph_observation_id):
                raise ValueError("Invalid behavior graph observation ID")
        with self.connect() as connection:
            for observation_id, graph_observation_id, revision_id in mappings:
                exists = connection.execute(
                    "SELECT 1 FROM captures WHERE observation_id=?", (observation_id,)
                ).fetchone()
                if exists is None:
                    raise KeyError("Cannot map a graph observation before saving its capture")
                revision_exists = connection.execute(
                    "SELECT 1 FROM capture_revisions WHERE revision_id=? AND observation_id=?",
                    (revision_id, observation_id),
                ).fetchone()
                if revision_exists is None:
                    raise KeyError("Cannot map a graph observation before saving its capture revision")
                current_revision = connection.execute(
                    "SELECT current_revision_id FROM captures WHERE observation_id=?",
                    (observation_id,),
                ).fetchone()
                if current_revision is None or current_revision[0] != revision_id:
                    raise ValueError("Cannot map a graph observation to a stale capture revision")
                timestamp = _utc_now()
                connection.execute(
                    "INSERT INTO graph_capture_map(observation_id,revision_id,graph_observation_id,updated_at) VALUES(?,?,?,?) "
                    "ON CONFLICT(observation_id) DO UPDATE SET revision_id=excluded.revision_id,"
                    "graph_observation_id=excluded.graph_observation_id,updated_at=excluded.updated_at",
                    (observation_id, revision_id, graph_observation_id, timestamp),
                )
                connection.execute(
                    "INSERT INTO graph_capture_revisions(revision_id,graph_observation_id,updated_at) VALUES(?,?,?) "
                    "ON CONFLICT(revision_id) DO UPDATE SET graph_observation_id=excluded.graph_observation_id,updated_at=excluded.updated_at",
                    (revision_id, graph_observation_id, timestamp),
                )

    def read_blob(self, observation_id: str, side: str, sha256: str, key: bytes | None = None) -> bytes:
        selected_key = decode_vault_key() if key is None else key
        path = self._blob_path(observation_id, side, sha256)
        encoded = path.read_bytes()
        if len(encoded) < len(_MAGIC) + 12 or not encoded.startswith(_MAGIC):
            raise ValueError("Invalid or truncated vault object")
        nonce = encoded[len(_MAGIC):len(_MAGIC) + 12]
        ciphertext = encoded[len(_MAGIC) + 12:]
        return AESGCM(selected_key).decrypt(nonce, ciphertext, self._aad(observation_id, side))

    def _write_blob(self, observation_id: str, side: str, sha256: str, raw: bytes, key: bytes) -> str:
        path = self._blob_path(observation_id, side, sha256)
        if path.is_file() and not path.is_symlink():
            try:
                if self.read_blob(observation_id, side, sha256, key) == raw:
                    return path.name
            except (OSError, ValueError):
                # Replace an invalid or truncated object through the atomic path below.
                pass
        nonce = os.urandom(12)
        encoded = _MAGIC + nonce + AESGCM(key).encrypt(nonce, raw, self._aad(observation_id, side))
        fd, temp_name = tempfile.mkstemp(prefix=".capture-", dir=path.parent)
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "wb") as stream:
                stream.write(encoded)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temp_name, path)
        finally:
            try:
                os.unlink(temp_name)
            except FileNotFoundError:
                pass
        return path.name

    def _blob_path(self, observation_id: str, side: str, sha256: str) -> Path:
        if not _HEX64.fullmatch(observation_id) or not _HEX64.fullmatch(sha256) or side not in {"request", "response"}:
            raise ValueError("Invalid vault object reference")
        return self.vault_directory / f"{observation_id}.{side}.{sha256}.bba"

    @staticmethod
    def _aad(observation_id: str, side: str) -> bytes:
        return f"bb-graph-assistant:v1:{observation_id}:{side}".encode("ascii")
