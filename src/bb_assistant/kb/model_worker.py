"""Private JSON-lines adapter for the pinned RedAmon retrieval models."""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys
from typing import Any

from .embeddings import RedAmonEmbedder, RedAmonReranker


MAX_QUERY_CHARS = 4096
MAX_RERANK_ITEMS = 30
MAX_TEXT_CHARS = 1920
REDAMON_COMMIT = "4585b3442cf3c6e075fca5d3b512a8f47be501b6"
_REDAMON_RUNTIME: tuple[Any, Any, str, dict[str, int], int] | None = None


def _load_redamon_runtime() -> tuple[Any, Any, str, dict[str, int], int]:
    """Load the pinned upstream KB classes; retrieval stays in RedAmon code."""
    global _REDAMON_RUNTIME
    if _REDAMON_RUNTIME is not None:
        return _REDAMON_RUNTIME
    root_value = os.environ.get("BB_ASSISTANT_REDAMON_ROOT", "")
    if not root_value:
        raise RuntimeError("RedAmon source root is not configured")
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    requested_root = Path(root_value).expanduser()
    if requested_root.is_symlink():
        raise RuntimeError("RedAmon source root must not be a symbolic link")
    root = requested_root.resolve(strict=True)
    revision = subprocess.run(
        ["git", "-C", str(root), "rev-parse", "HEAD"],
        check=True, capture_output=True, text=True, timeout=5,
    ).stdout.strip()
    if revision != REDAMON_COMMIT:
        raise RuntimeError("RedAmon source revision does not match the pinned KB runtime")
    dirty = subprocess.run(
        ["git", "-C", str(root), "status", "--porcelain", "--untracked-files=all", "--", "services/knowledge_base"],
        check=True, capture_output=True, text=True, timeout=5,
    ).stdout
    if dirty:
        raise RuntimeError("RedAmon KB source tree has uncommitted changes")
    services = root / "services"
    package = services / "knowledge_base"
    if not package.is_dir() or package.is_symlink():
        raise RuntimeError("RedAmon KB package is unavailable")
    sys.path.insert(0, str(services))
    from neo4j import GraphDatabase
    from knowledge_base.embedder import Embedder
    from knowledge_base.faiss_indexer import FAISSIndexer
    from knowledge_base.kb_config import load_kb_config
    from knowledge_base.kb_orchestrator import PentestKnowledgeBase
    from knowledge_base.neo4j_loader import Neo4jLoader

    config_path = Path(os.environ.get("KB_CONFIG_FILE", str(package / "kb_config.yaml"))).expanduser()
    if config_path.is_symlink() or not config_path.is_file():
        raise RuntimeError("RedAmon KB configuration is unavailable")
    config = load_kb_config(path=config_path, refresh=True)
    if (not config.fulltext.enabled or not config.reranker.enabled or not config.mmr.enabled
            or config.embedder.model != "intfloat/e5-large-v2"
            or config.reranker.model != "BAAI/bge-reranker-base"
            or config.retrieval.top_k != 5 or config.retrieval.overfetch_factor != 6
            or config.retrieval.rrf_k != 60 or config.reranker.pool_size != 30
            or config.reranker.max_tokens_per_side != 480 or config.mmr.lambda_ != 0.65
            or config.source_boosts != {
                "tool_docs": 1.20, "gtfobins": 1.15, "lolbas": 1.15,
                "owasp": 1.05, "nuclei": 1.00, "nvd": 0.90, "exploitdb": 0.85,
            }):
        raise RuntimeError("RedAmon KB retrieval policy is not the pinned full profile")
    kb_path = Path(os.environ.get("BB_ASSISTANT_REDAMON_KB_PATH", str(package / "data"))).expanduser()
    if kb_path.is_symlink() or not kb_path.is_dir():
        raise RuntimeError("RedAmon KB data path is unavailable")
    indexer = FAISSIndexer(index_path=str(kb_path), dimensions=1024)
    if not indexer.load() or indexer.count() == 0 or len(set(indexer.chunk_ids)) != indexer.count():
        raise RuntimeError("RedAmon FAISS index is unavailable")
    marker = json.loads((kb_path / ".last_ingest").read_text(encoding="utf-8"))
    if not isinstance(marker, dict) or marker.get("profile") != "full":
        raise RuntimeError("RedAmon KB is not marked as a full-profile ingestion")
    uri, user, password = (os.environ.get(name, "") for name in ("NEO4J_URI", "NEO4J_USER", "NEO4J_PASSWORD"))
    if not uri or not user or not password:
        raise RuntimeError("RedAmon Neo4j credentials are unavailable")
    driver = GraphDatabase.driver(uri, auth=(user, password))
    loader = Neo4jLoader(driver)
    source_counts = loader.get_stats()
    if (set(source_counts) != {"tool_docs", "gtfobins", "lolbas", "owasp", "exploitdb", "nvd", "nuclei"}
            or sum(source_counts.values()) != indexer.count()):
        driver.close()
        raise RuntimeError("RedAmon FAISS and Neo4j KB inventories are incomplete or mismatched")
    kb = PentestKnowledgeBase(indexer, loader, Embedder(model_name=config.embedder.model), config=config)
    _REDAMON_RUNTIME = (kb, driver, marker.get("timestamp", ""), source_counts, indexer.count())
    return _REDAMON_RUNTIME


def _redamon_search(request: dict[str, Any]) -> dict[str, Any]:
    query, top_k = request.get("query"), request.get("top_k")
    sources = request.get("source_names", [])
    min_cvss, severity = request.get("min_cvss"), request.get("severity")
    if (not isinstance(query, str) or not query.strip() or len(query) > MAX_QUERY_CHARS
            or type(top_k) is not int or not 1 <= top_k <= 20
            or not isinstance(sources, list) or len(sources) > 20
            or any(not isinstance(source, str) or not source for source in sources)
            or (min_cvss is not None and (type(min_cvss) not in (int, float) or not 0 <= min_cvss <= 10))
            or not isinstance(severity, str)):
        return {"ok": False, "error_code": "invalid_redamon_search"}
    if request.get("include_supplemental") is True:
        return {"ok": False, "error_code": "redamon_has_no_supplemental_profile"}
    try:
        kb, _driver, ingested_at, _source_counts, _vector_count = _load_redamon_runtime()
        hits = kb.query(
            query, top_k=top_k, include_sources=sources or None,
            min_cvss=min_cvss, severity=severity or None,
        )
        return {"ok": True, "hits": hits, "ingested_at": ingested_at, "retrieval_engine": "redamon-pinned"}
    except Exception:
        return {"ok": False, "error_code": "redamon_search_unavailable"}


def _redamon_stats() -> dict[str, Any]:
    try:
        _kb, _driver, ingested_at, source_counts, vector_count = _load_redamon_runtime()
        return {
            "ok": True, "ingested_at": ingested_at,
            "source_chunk_counts": source_counts,
            "vector_count": vector_count,
            "retrieval_engine": "redamon-pinned",
        }
    except Exception:
        return {"ok": False, "error_code": "redamon_stats_unavailable"}


def _response(request: dict[str, Any], embedder: RedAmonEmbedder, reranker: RedAmonReranker) -> dict[str, Any]:
    action = request.get("action")
    if action == "redamon_search":
        return _redamon_search(request)
    if action == "redamon_stats":
        return _redamon_stats()
    if action == "embed_query":
        query = request.get("query")
        if not isinstance(query, str) or len(query) > MAX_QUERY_CHARS:
            return {"ok": False, "error_code": "invalid_query"}
        try:
            return {"ok": True, "result": embedder.embed_query(query)}
        except Exception:
            return {"ok": False, "error_code": "embedding_unavailable"}
    if action == "rerank":
        query, texts = request.get("query"), request.get("texts")
        if not isinstance(query, str) or not isinstance(texts, list) or len(texts) > MAX_RERANK_ITEMS:
            return {"ok": False, "error_code": "invalid_rerank_request"}
        if len(query) > MAX_TEXT_CHARS or any(not isinstance(text, str) or len(text) > MAX_TEXT_CHARS for text in texts):
            return {"ok": False, "error_code": "rerank_text_too_large"}
        try:
            return {"ok": True, "result": reranker.score(query, texts)}
        except Exception:
            return {"ok": False, "error_code": "reranker_unavailable"}
    return {"ok": False, "error_code": "unknown_action"}


def main() -> int:
    # Offline-only loading is intentional. Model installation/download happens
    # through the explicit `bb-assistant-kb models --download` command.
    embedder = RedAmonEmbedder(local_files_only=True)
    reranker = RedAmonReranker(local_files_only=True)
    for line in sys.stdin:
        try:
            request = json.loads(line)
            if not isinstance(request, dict):
                raise ValueError("request must be an object")
            response = _response(request, embedder, reranker)
        except Exception:
            response = {"ok": False, "error_code": "invalid_request"}
        sys.stdout.write(json.dumps(response, separators=(",", ":"), allow_nan=False) + "\n")
        sys.stdout.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
