#!/usr/bin/env python3
"""Export read-only runtime evidence from a pinned RedAmon KB installation.

This command loads RedAmon's existing FAISS index, reads KBChunk nodes through
a Neo4j read transaction, and executes only the pinned public parity queries.
It never calls an ingestion/fetch method or writes to FAISS/Neo4j. Run it in an
environment that has RedAmon's service dependencies and KB credentials.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from typing import Any


REDAMON_COMMIT = "4585b3442cf3c6e075fca5d3b512a8f47be501b6"
SOURCE_FAMILIES = ("tool_docs", "gtfobins", "lolbas", "owasp", "exploitdb", "nvd", "nuclei")
E5_MODEL = "intfloat/e5-large-v2"
BGE_MODEL = "BAAI/bge-reranker-base"
FIXTURE_DEFAULT = Path(__file__).resolve().parent.parent / "references" / "kb-parity-queries.json"


class ReceiptError(RuntimeError):
    pass


class _ReadOnlyDriver:
    """Force RedAmon's existing loader queries onto Neo4j read routing."""

    def __init__(self, driver: Any, read_access: Any) -> None:
        self._driver = driver
        self._read_access = read_access

    def session(self, *args: Any, **kwargs: Any) -> Any:
        kwargs.setdefault("default_access_mode", self._read_access)
        return self._driver.session(*args, **kwargs)


def _git(root: Path, *args: str) -> str:
    try:
        result = subprocess.run(
            ["git", "-C", str(root), *args], check=True, capture_output=True, text=True,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise ReceiptError("cannot verify the RedAmon checkout") from exc
    return result.stdout.strip()


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _canonical_line(value: dict[str, Any]) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n"


def _write_private(path: Path, value: dict[str, Any]) -> None:
    path = path.expanduser()
    if path.is_symlink():
        raise ReceiptError("receipt output must not be a symbolic link")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(json.dumps(value, sort_keys=True, indent=2).encode("utf-8") + b"\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    except Exception:
        try:
            os.close(fd)
        except OSError:
            pass
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def _load_fixture(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ReceiptError("cannot read the pinned parity query fixture") from exc
    if not isinstance(value, dict) or value.get("schema_version") != 1 or not isinstance(value.get("queries"), list):
        raise ReceiptError("parity query fixture has an unsupported shape")
    if not value["queries"]:
        raise ReceiptError("parity query fixture has no queries")
    for row in value["queries"]:
        if not isinstance(row, dict) or not isinstance(row.get("id"), str) or not row["id"]:
            raise ReceiptError("parity fixture contains an invalid query")
        if not isinstance(row.get("query"), str) or not row["query"].strip() or row.get("top_k") != 5:
            raise ReceiptError("parity fixture queries must be non-empty and use top_k=5")
        filters = row.get("filters")
        if not isinstance(filters, dict) or set(filters) - {"source_names"}:
            raise ReceiptError("parity fixture contains unsupported filters")
        source_names = filters.get("source_names", [])
        if not isinstance(source_names, list) or any(name not in SOURCE_FAMILIES for name in source_names):
            raise ReceiptError("parity fixture source_names filter is invalid")
    return value


def _runtime_inventory(driver: Any, faiss_ids: list[str]) -> tuple[dict[str, Any], dict[str, int], dict[str, int]]:
    try:
        from neo4j import READ_ACCESS
    except ImportError as exc:
        raise ReceiptError("Neo4j Python driver is required") from exc

    family_hashes = {name: hashlib.sha256() for name in sorted(SOURCE_FAMILIES)}
    family_chunks = {name: 0 for name in sorted(SOURCE_FAMILIES)}
    family_paths: dict[str, set[str]] = {name: set() for name in sorted(SOURCE_FAMILIES)}
    total_hash = hashlib.sha256()
    graph_ids: set[str] = set()
    query = (
        "MATCH (c:KBChunk) "
        "RETURN c.chunk_id AS chunk_id, c.source AS source_name, c.title AS title, "
        "c.content AS content, c.source_path AS source_path, c.cve_id AS cve_id, "
        "c.edb_id AS edb_id ORDER BY c.chunk_id"
    )
    try:
        with driver.session(default_access_mode=READ_ACCESS, fetch_size=1000) as session:
            for row in session.run(query):
                chunk_id, source_name = row["chunk_id"], row["source_name"]
                title, content, source_path = row["title"], row["content"], row["source_path"]
                if (not isinstance(chunk_id, str) or not chunk_id or source_name not in family_hashes
                        or not isinstance(title, str) or not isinstance(content, str) or not content
                        or not isinstance(source_path, str) or not source_path):
                    raise ReceiptError("RedAmon KB contains a chunk with missing identity or provenance")
                if chunk_id in graph_ids:
                    raise ReceiptError("RedAmon Neo4j KB has duplicate chunk IDs")
                graph_ids.add(chunk_id)
                # NVD and ExploitDB store many independently searchable
                # records behind one shared source_path; count those by their
                # stable record key. Other families use one source_path per
                # source document.
                if source_name == "nvd":
                    document_id = row["cve_id"]
                elif source_name == "exploitdb":
                    document_id = row["edb_id"]
                else:
                    document_id = source_path
                if not isinstance(document_id, str) or not document_id:
                    raise ReceiptError("RedAmon KB contains a chunk with missing document identity")
                family_paths[source_name].add(document_id)
                record = {
                    "chunk_id": chunk_id,
                    "source_name": source_name,
                    "title": title,
                    "content_sha256": _sha256(content.encode("utf-8")),
                }
                encoded = _canonical_line(record)
                total_hash.update(encoded)
                family_hashes[source_name].update(encoded)
                family_chunks[source_name] += 1
    except ReceiptError:
        raise
    except Exception as exc:
        raise ReceiptError("read-only RedAmon KB inventory query failed") from exc

    index_ids = set(faiss_ids)
    if len(index_ids) != len(faiss_ids) or index_ids != graph_ids:
        raise ReceiptError("RedAmon FAISS IDs do not exactly match the Neo4j KB chunk IDs")
    if not graph_ids or any(family_chunks[name] == 0 or not family_paths[name] for name in family_hashes):
        raise ReceiptError("RedAmon runtime does not contain every full-profile source family")
    inventory = {
        "chunk_count": len(graph_ids),
        "sha256": total_hash.hexdigest(),
        "sources": [
            {"name": name, "chunk_count": family_chunks[name], "sha256": family_hashes[name].hexdigest()}
            for name in sorted(family_hashes)
        ],
    }
    return inventory, family_chunks, {name: len(paths) for name, paths in family_paths.items()}


def _score(value: Any, field: str) -> float:
    if not isinstance(value, (int, float)) or isinstance(value, bool) or not math.isfinite(value):
        raise ReceiptError(f"RedAmon query omitted finite {field} evidence")
    return float(value)


def _score_or_zero(value: Any, field: str) -> float:
    # A result can be contributed by only one candidate channel. Preserve a
    # finite, schema-compatible score vector and encode an absent channel as 0.
    return 0.0 if value is None else _score(value, field)


def export_receipt(*, root: Path, kb_path: Path, fixture_path: Path, output_path: Path) -> dict[str, Any]:
    root = root.expanduser()
    if root.is_symlink():
        raise ReceiptError("RedAmon root must not be a symbolic link")
    root = root.resolve(strict=True)
    if _git(root, "rev-parse", "HEAD") != REDAMON_COMMIT:
        raise ReceiptError(f"RedAmon checkout must be exactly {REDAMON_COMMIT}")
    if _git(root, "status", "--porcelain", "--untracked-files=all", "--", "services/knowledge_base"):
        raise ReceiptError("RedAmon knowledge_base source tree has uncommitted or untracked files")
    package = root / "services" / "knowledge_base"
    default_config_path = package / "kb_config.yaml"
    config_path = Path(os.environ.get("KB_CONFIG_FILE", str(default_config_path))).expanduser()
    if not config_path.is_file() or config_path.is_symlink():
        raise ReceiptError("RedAmon KB configuration is missing or unsafe")
    kb_path = kb_path.expanduser()
    if kb_path.is_symlink():
        raise ReceiptError("RedAmon KB path must not be a symbolic link")
    kb_path = kb_path.resolve(strict=True)
    last_ingest = kb_path / ".last_ingest"
    try:
        marker = json.loads(last_ingest.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ReceiptError("RedAmon last-ingest marker is missing or invalid") from exc
    if not isinstance(marker, dict) or marker.get("profile") != "full":
        raise ReceiptError("RedAmon active index is not marked as a full-profile ingestion")

    fixture = _load_fixture(fixture_path)
    services = root / "services"
    if str(services) not in sys.path:
        sys.path.insert(0, str(services))
    try:
        from neo4j import GraphDatabase, READ_ACCESS
        from knowledge_base.embedder import Embedder
        from knowledge_base.faiss_indexer import FAISSIndexer
        from knowledge_base.kb_config import load_kb_config
        from knowledge_base.kb_orchestrator import PentestKnowledgeBase
        from knowledge_base.neo4j_loader import Neo4jLoader
        from knowledge_base.curation.pins import model_revision
    except ImportError as exc:
        raise ReceiptError("RedAmon service dependencies are unavailable") from exc

    config = load_kb_config(path=config_path, refresh=True)
    if config.embedder.model != E5_MODEL or config.reranker.model != BGE_MODEL:
        raise ReceiptError("RedAmon runtime model selection differs from the pinned full-profile models")
    if not config.reranker.enabled or not config.fulltext.enabled or not config.mmr.enabled:
        raise ReceiptError("RedAmon runtime has a disabled full-profile retrieval stage")
    if (config.retrieval.top_k != 5 or config.retrieval.overfetch_factor != 6 or config.retrieval.rrf_k != 60
            or config.reranker.pool_size != 30 or config.mmr.lambda_ != 0.65):
        raise ReceiptError("RedAmon runtime retrieval settings differ from the pinned full-profile policy")

    uri, user, password = os.environ.get("NEO4J_URI"), os.environ.get("NEO4J_USER"), os.environ.get("NEO4J_PASSWORD")
    if not uri or not user or not password:
        raise ReceiptError("NEO4J_URI, NEO4J_USER and NEO4J_PASSWORD must be set in the process environment")
    indexer = FAISSIndexer(index_path=str(kb_path), dimensions=1024)
    if not indexer.load() or not indexer.chunk_ids:
        raise ReceiptError("RedAmon FAISS index is missing, invalid, or empty")
    driver = GraphDatabase.driver(uri, auth=(user, password))
    try:
        loader = Neo4jLoader(_ReadOnlyDriver(driver, READ_ACCESS))
        kb = PentestKnowledgeBase(indexer, loader, Embedder(model_name=config.embedder.model), config=config)
        inventory, family_chunks, family_docs = _runtime_inventory(driver, indexer.chunk_ids)

        queries: list[dict[str, Any]] = []
        for spec in fixture["queries"]:
            source_filter = spec["filters"].get("source_names", [])
            results = kb.query(
                spec["query"], top_k=spec["top_k"],
                include_sources=source_filter or None,
                mmr_enabled=kb.mmr_enabled, mmr_lambda=kb.mmr_lambda,
                source_boosts=kb.source_boosts,
            )
            if len(results) != spec["top_k"]:
                raise ReceiptError(f"pinned RedAmon query {spec['id']} did not return exactly top_k results")
            result_rows = []
            for row in results:
                components = {
                    "lexical_score": _score_or_zero(row.get("fulltext_score"), "lexical score"),
                    "vector_cosine": _score_or_zero(row.get("vector_score"), "vector cosine"),
                    "reranker_logit": _score(row.get("rerank_score"), "reranker logit"),
                    "combined_rank": _score(row.get("score"), "combined rank"),
                }
                result_rows.append({"chunk_id": row.get("chunk_id"), "score_components": components})
            queries.append({**spec, "results": result_rows})

        reranker = kb._reranker
        max_chars = getattr(reranker, "_effective_max_chars", None)
        if not kb.rerank_enabled or max_chars != 1920:
            raise ReceiptError("RedAmon reranker did not run with the pinned 480-token/1920-character budget")
        source_rows = [{
            "name": name,
            "revision": f"redamon-tree:{REDAMON_COMMIT}",
            "filters": {"profile": "full"},
            "document_count": family_docs[name],
            "chunk_count": family_chunks[name],
            "content_sha256": next(row["sha256"] for row in inventory["sources"] if row["name"] == name),
            "chunk_sha256": next(row["sha256"] for row in inventory["sources"] if row["name"] == name),
            "complete": True,
        } for name in sorted(SOURCE_FAMILIES)]
        effective_config_path = Path(config.source_path) if config.source_path else config_path
        if effective_config_path.is_symlink() or not effective_config_path.is_file():
            raise ReceiptError("effective RedAmon KB configuration is missing or unsafe")
        config_bytes = effective_config_path.read_bytes()
        config_hash = _sha256(config_bytes)
        snapshot_material = _canonical_line({
            "redamon_commit": REDAMON_COMMIT,
            "config_sha256": config_hash,
            "runtime_inventory": inventory,
        })
        retrieval = {
            "lexical_engine": "neo4j-lucene",
            "vector_engine": "faiss",
            "candidate_pool": max(kb.top_k * kb.overfetch_factor, kb.rerank_pool_size),
            "rrf_k": kb.rrf_k,
            "rerank_pool_size": kb.rerank_pool_size,
            "mmr_lambda": kb.mmr_lambda,
            "mmr_enabled": kb.mmr_enabled,
            "source_boosts": dict(kb.source_boosts),
            "reranker_enabled": kb.rerank_enabled,
            "reranker_max_chars_per_side": max_chars,
        }
        receipt = {
            "schema_version": 3,
            "producer": "redamon",
            "profile": "full",
            "corpus": {
                "snapshot_id": _sha256(snapshot_material),
                "source_manifest_sha256": config_hash,
                "source_count": len(source_rows),
                "document_count": sum(family_docs.values()),
                "chunk_count": inventory["chunk_count"],
                "coverage_state": "DONE",
                "runtime_inventory": inventory,
                "vectors": {
                    "model": config.embedder.model,
                    "revision": model_revision(config.embedder.model),
                    "dimensions": 1024,
                    "embedded_chunks": len(indexer.chunk_ids),
                    "total_content_chunks": len(indexer.chunk_ids),
                    "complete": True,
                },
                "reranker": {
                    "model": config.reranker.model,
                    "revision": model_revision(config.reranker.model),
                },
                "retrieval": retrieval,
            },
            "sources": source_rows,
            "queries": queries,
            "evidence": {
                "redamon_commit": REDAMON_COMMIT,
                "last_ingest_profile": marker["profile"],
                "last_ingest_sha256": _sha256(last_ingest.read_bytes()),
                "faiss_index_count": len(indexer.chunk_ids),
            },
        }
        from bb_assistant.kb.parity import load_receipt_from_value
        load_receipt_from_value(receipt, expected_producer="redamon")
        _write_private(output_path, receipt)
        return receipt
    finally:
        driver.close()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--redamon-root", type=Path, required=True)
    parser.add_argument("--kb-path", type=Path)
    parser.add_argument("--fixture", type=Path, default=FIXTURE_DEFAULT)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = args.redamon_root.expanduser()
    kb_path = args.kb_path or root / "services" / "knowledge_base" / "data"
    try:
        receipt = export_receipt(root=root, kb_path=kb_path, fixture_path=args.fixture, output_path=args.output)
    except (OSError, ValueError, ReceiptError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    print(json.dumps({
        "status": "EXPORTED",
        "chunk_count": receipt["corpus"]["runtime_inventory"]["chunk_count"],
        "source_families": receipt["corpus"]["source_count"],
        "query_count": len(receipt["queries"]),
        "output": str(args.output.expanduser().resolve()),
    }, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
