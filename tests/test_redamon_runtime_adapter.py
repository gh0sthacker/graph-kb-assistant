from __future__ import annotations

from bb_assistant.kb import model_worker


def test_redamon_search_delegates_query_and_filters_to_pinned_kb(monkeypatch):
    calls = []
    expected = [{
        "chunk_id": "chunk-1", "source": "owasp", "source_path": "WSTG/authorization.md",
        "title": "Authorization", "content": "Use object-level checks.",
        "score": 0.3, "vector_score": 0.8, "fulltext_score": 1.2, "rerank_score": 0.7,
    }]

    class KB:
        def query(self, query, **kwargs):
            calls.append((query, kwargs))
            return expected

    monkeypatch.setattr(
        model_worker, "_load_redamon_runtime",
        lambda: (KB(), object(), "2026-10-26T00:00:00+00:00", {}, 1),
    )
    response = model_worker._response({
        "action": "redamon_search", "query": "object authorization",
        "top_k": 5, "source_names": ["owasp"], "min_cvss": 7.0,
        "severity": "high", "include_supplemental": False,
    }, object(), object())

    assert response["ok"] is True
    assert response["retrieval_engine"] == "redamon-pinned"
    assert response["hits"] == expected
    assert response["ingested_at"] == "2026-10-26T00:00:00+00:00"
    assert calls == [(
        "object authorization", {
            "top_k": 5, "include_sources": ["owasp"],
            "min_cvss": 7.0, "severity": "high",
        },
    )]


def test_redamon_search_refuses_supplemental_without_falling_back(monkeypatch):
    def unexpected_load():
        raise AssertionError("must reject before loading a different backend")

    monkeypatch.setattr(model_worker, "_load_redamon_runtime", unexpected_load)
    response = model_worker._response({
        "action": "redamon_search", "query": "query", "top_k": 5,
        "source_names": [], "severity": "", "include_supplemental": True,
    }, object(), object())

    assert response == {"ok": False, "error_code": "redamon_has_no_supplemental_profile"}
