"""Snapshot-isolated Neo4j Lucene candidates using RedAmon's query contract.

SQLite remains the provenance authority. A generation is readable only after
its complete projection and full-text indexes have been checked and published.
"""
from __future__ import annotations

import hashlib
import json
import math
import os
import re
from typing import Any, Mapping
from urllib.parse import urlsplit


_SNAPSHOT = re.compile(r"^[a-f0-9]{64}$")
_CHUNK = re.compile(r"^(?:[a-f0-9]{16}|[a-f0-9]{64})$")


def generation_names(snapshot_id: str) -> tuple[str, str, str, str]:
    if not isinstance(snapshot_id, str) or not _SNAPSHOT.fullmatch(snapshot_id):
        raise ValueError("Lucene generation requires a SHA-256 snapshot ID")
    return (f"KBGen_{snapshot_id}", f"KBGen_{snapshot_id}_all",
            f"kb_{snapshot_id}", f"kb_{snapshot_id}_all")


def sanitize_redamon_query(query: str) -> str:
    # Verbatim sanitization from pinned RedAmon Neo4jLoader.fulltext_search.
    return re.sub(r'[+\-!(){}\[\]^"~*?:\\/]', " ", query).replace("&&", " ").replace("||", " ").strip()


def _driver_config_from_env(environ: Mapping[str, str] | None = None) -> tuple[str, str, str, str]:
    env = os.environ if environ is None else environ
    uri = env.get("BB_ASSISTANT_KB_NEO4J_URI", "").strip()
    username = env.get("BB_ASSISTANT_KB_NEO4J_USERNAME", "").strip()
    password = env.get("BB_ASSISTANT_KB_NEO4J_PASSWORD", "")
    database = env.get("BB_ASSISTANT_KB_NEO4J_DATABASE", "").strip()
    parsed = urlsplit(uri)
    if (not uri or not username or not password or not database
            or parsed.scheme not in {"bolt", "bolt+s", "bolt+ssc", "neo4j", "neo4j+s", "neo4j+ssc"}
            or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment
            or not re.fullmatch(r"[A-Za-z0-9_-]{1,63}", database)):
        raise ValueError("dedicated KB Neo4j connection settings are missing or invalid")
    return uri, username, password, database


def _driver_from_env() -> tuple[Any, str]:
    uri, username, password, database = _driver_config_from_env()
    try:
        from neo4j import GraphDatabase
    except ImportError as exc:
        raise RuntimeError("Neo4j Lucene requires the kb-neo4j dependency") from exc
    return GraphDatabase.driver(uri, auth=(username, password), connection_timeout=10), database


class Neo4jLexicalProvider:
    engine = "neo4j-lucene"

    def __init__(self, driver: Any, *, database: str = "neo4j", owns_driver: bool = False) -> None:
        self.driver, self.database, self.owns_driver = driver, database, owns_driver

    @classmethod
    def from_env(cls) -> "Neo4jLexicalProvider":
        driver, database = _driver_from_env()
        return cls(driver, database=database, owns_driver=True)

    def close(self) -> None:
        if self.owns_driver:
            self.driver.close()

    def search(self, snapshot_id: str, query: str, *, limit: int,
               include_supplemental: bool = False) -> tuple[tuple[str, float], ...]:
        _, _, core_index, all_index = generation_names(snapshot_id)
        if type(limit) is not int or not 1 <= limit <= 600:
            raise ValueError("Lucene candidate limit must be between 1 and 600")
        escaped = sanitize_redamon_query(query)
        if not escaped:
            return ()
        try:
            with self.driver.session(database=self.database, default_access_mode="READ") as session:
                markers = list(session.run(
                    "MATCH (g:AssistantKBGeneration {snapshot_id:$snapshot_id}) "
                    "RETURN g.state AS state,g.schema_version AS schema_version,"
                    "g.snapshot_id AS snapshot_id,g.core_count AS core_count,g.total_count AS total_count,"
                    "g.core_sha256 AS core_sha256,g.total_sha256 AS total_sha256 LIMIT 2",
                    snapshot_id=snapshot_id,
                ))
                marker = markers[0] if len(markers) == 1 else None
                if (marker is None or marker["state"] != "ready" or marker["schema_version"] != 1
                        or marker.get("snapshot_id") != snapshot_id
                        or type(marker.get("core_count")) is not int or marker["core_count"] <= 0
                        or type(marker.get("total_count")) is not int or marker["total_count"] < marker["core_count"]
                        or not isinstance(marker.get("core_sha256"), str) or not _SNAPSHOT.fullmatch(marker["core_sha256"])
                        or not isinstance(marker.get("total_sha256"), str) or not _SNAPSHOT.fullmatch(marker["total_sha256"])):
                    raise RuntimeError("KB Lucene generation is missing or not published")
                result = session.run(
                    "CALL db.index.fulltext.queryNodes($index_name,$q_text,{limit:$limit}) "
                    "YIELD node,score RETURN node.chunk_id AS chunk_id,"
                    "node.snapshot_id AS snapshot_id,score ORDER BY score DESC",
                    index_name=all_index if include_supplemental else core_index,
                    q_text=escaped, limit=limit,
                )
                rows: list[tuple[str, float]] = []
                seen: set[str] = set()
                for row in result:
                    chunk_id, score = row["chunk_id"], row["score"]
                    if (row["snapshot_id"] != snapshot_id or not isinstance(chunk_id, str)
                            or not _CHUNK.fullmatch(chunk_id) or chunk_id in seen
                            or type(score) not in {int, float} or not math.isfinite(score) or score < 0):
                        raise RuntimeError("KB Lucene candidate has invalid snapshot provenance")
                    seen.add(chunk_id)
                    rows.append((chunk_id, float(score)))
                if len(rows) > limit:
                    raise RuntimeError("KB Lucene candidate pool exceeded its bound")
                return tuple(rows)
        except RuntimeError:
            raise
        except Exception as exc:
            # Connection/authentication errors may contain private URI details.
            raise RuntimeError("KB Lucene candidates could not be read") from None


def _projection_row(row: Any) -> dict[str, str | bool]:
    text = row["text"]
    metadata = json.loads(row["metadata_json"])
    if (not _CHUNK.fullmatch(row["chunk_id"]) or not isinstance(text, str)
            or hashlib.sha256(text.encode("utf-8")).hexdigest() != row["chunk_sha256"]
            or not isinstance(row["source_name"], str) or not isinstance(row["title"], str)):
        raise ValueError("KB chunk projection failed content/hash validation")
    return {"chunk_id": row["chunk_id"], "source": row["source_name"],
            "title": row["title"], "content": text,
            "supplemental": (metadata.get("canonicalization_fallback") is not None
                             or metadata.get("redamon_excluded_reason") is not None)}


def _digest_row(digest: Any, row: Any) -> None:
    digest.update(json.dumps([row["chunk_id"], row["source"], row["title"], row["content"]],
                             ensure_ascii=False, separators=(",", ":")).encode("utf-8") + b"\n")


def publish_generation(index: Any, driver: Any, *, database: str = "neo4j",
                       batch_size: int = 200) -> dict[str, Any]:
    """Project a read-only SQLite snapshot; publish only after a full re-read.

    Existing ready generations are immutable. An interrupted generation can be
    rebuilt using only its own labels and composite node identities.
    """
    if type(batch_size) is not int or not 1 <= batch_size <= 500:
        raise ValueError("Lucene projection batch size must be between 1 and 500")
    if not index.read_only:
        raise ValueError("Lucene publishing requires a read-only SQLite index")
    with index.connect() as connection:
        connection.execute("BEGIN")
        meta = dict(connection.execute("SELECT key,value FROM kb_meta"))
        snapshot_id = meta.get("snapshot_id", "")
        core_label, all_label, core_index, all_index = generation_names(snapshot_id)
        if any(meta.get(key) for key in ("rechunk_target_snapshot_id", "maintenance_state")):
            raise ValueError("KB snapshot maintenance is in progress")
        core_hash, all_hash = hashlib.sha256(), hashlib.sha256()
        core_count = total_count = 0
        sql = ("SELECT chunk_id,source_name,title,text,chunk_sha256,metadata_json FROM chunks "
               "WHERE COALESCE(json_extract(metadata_json,'$.document_role'),'content')='content' "
               "ORDER BY chunk_id")
        # Validate before any remote write and bind the generation to exact text.
        for stored in connection.execute(sql):
            row = _projection_row(stored)
            _digest_row(all_hash, row)
            total_count += 1
            if not row["supplemental"]:
                _digest_row(core_hash, row)
                core_count += 1
        if not core_count:
            raise ValueError("Cannot publish an empty KB Lucene core")
        evidence = {"snapshot_id": snapshot_id, "schema_version": 1,
                    "core_count": core_count, "total_count": total_count,
                    "core_sha256": core_hash.hexdigest(), "total_sha256": all_hash.hexdigest()}
        with driver.session(database=database) as session:
            existing = session.run(
                "MATCH (g:AssistantKBGeneration {snapshot_id:$snapshot_id}) RETURN properties(g) AS generation",
                snapshot_id=snapshot_id,
            ).single()
            if existing is not None and existing["generation"].get("state") == "ready":
                if any(existing["generation"].get(key) != value for key, value in evidence.items()):
                    raise ValueError("Ready Lucene generation differs from the SQLite snapshot")
                return {**evidence, "state": "ready", "already_published": True}
            session.run(
                "CREATE CONSTRAINT assistant_kb_chunk_identity IF NOT EXISTS "
                "FOR (c:AssistantKBChunk) REQUIRE (c.snapshot_id,c.chunk_id) IS UNIQUE"
            ).consume()
            session.run(
                "CREATE CONSTRAINT assistant_kb_generation_identity IF NOT EXISTS "
                "FOR (g:AssistantKBGeneration) REQUIRE g.snapshot_id IS UNIQUE"
            ).consume()
            session.run(
                "MERGE (g:AssistantKBGeneration {snapshot_id:$snapshot_id}) "
                "SET g.state='building',g.schema_version=1", snapshot_id=snapshot_id,
            ).consume()
            # Rebuild only an unpublished generation. Never mutate other KBs.
            while True:
                result = session.run(
                    "MATCH (c:AssistantKBChunk {snapshot_id:$snapshot_id}) WITH c LIMIT $batch "
                    "DETACH DELETE c RETURN count(*) AS removed",
                    snapshot_id=snapshot_id, batch=batch_size,
                ).single()
                if result is None or result["removed"] == 0:
                    break
            batch: list[dict[str, Any]] = []

            def write_batch() -> None:
                session.run(
                    f"UNWIND $rows AS row CREATE (c:AssistantKBChunk:{all_label}) "
                    "SET c.snapshot_id=$snapshot_id,c.chunk_id=row.chunk_id,c.source=row.source,"
                    "c.title=row.title,c.content=row.content "
                    f"FOREACH (_ IN CASE WHEN row.supplemental THEN [] ELSE [1] END | SET c:{core_label})",
                    rows=batch, snapshot_id=snapshot_id,
                ).consume()

            for stored in connection.execute(sql):
                batch.append(_projection_row(stored))
                if len(batch) >= batch_size:
                    write_batch()
                    batch.clear()
            if batch:
                write_batch()
            verified_core, verified_all = hashlib.sha256(), hashlib.sha256()
            verified_core_count = verified_total = 0
            for row in session.run(
                f"MATCH (c:AssistantKBChunk:{all_label}) "
                f"RETURN c.chunk_id AS chunk_id,c.source AS source,c.title AS title,c.content AS content,"
                f"c:{core_label} AS core ORDER BY chunk_id"
            ):
                _digest_row(verified_all, row)
                verified_total += 1
                if row["core"]:
                    _digest_row(verified_core, row)
                    verified_core_count += 1
            if (verified_total != total_count or verified_core_count != core_count
                    or verified_all.hexdigest() != all_hash.hexdigest()
                    or verified_core.hexdigest() != core_hash.hexdigest()):
                raise RuntimeError("Lucene generation re-read differs from SQLite; generation remains unpublished")
            for name, label in ((core_index, core_label), (all_index, all_label)):
                session.run(f"CREATE FULLTEXT INDEX {name} IF NOT EXISTS "
                            f"FOR (c:{label}) ON EACH [c.content,c.title]").consume()
                session.run("CALL db.awaitIndex($index_name,120)", index_name=name).consume()
                state = session.run("SHOW INDEXES YIELD name,state WHERE name=$name RETURN state", name=name).single()
                if state is None or state["state"] != "ONLINE":
                    raise RuntimeError("Lucene generation full-text index is not online")
            session.run(
                "MATCH (g:AssistantKBGeneration {snapshot_id:$snapshot_id}) SET g += $evidence,g.state='ready'",
                snapshot_id=snapshot_id, evidence=evidence,
            ).consume()
        return {**evidence, "state": "ready", "already_published": False}
