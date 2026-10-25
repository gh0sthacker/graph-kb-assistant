"""Private JSON-lines adapter for the pinned RedAmon retrieval models."""
from __future__ import annotations

import json
import sys
from typing import Any

from .embeddings import RedAmonEmbedder, RedAmonReranker


MAX_QUERY_CHARS = 4096
MAX_RERANK_ITEMS = 30
MAX_TEXT_CHARS = 1920


def _response(request: dict[str, Any], embedder: RedAmonEmbedder, reranker: RedAmonReranker) -> dict[str, Any]:
    action = request.get("action")
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
