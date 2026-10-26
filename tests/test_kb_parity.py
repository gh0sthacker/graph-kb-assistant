from __future__ import annotations

import json
from pathlib import Path

import pytest

from bb_assistant.kb.cli import main
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex
from bb_assistant.kb.parity import compare_receipts, export_assistant_receipt, load_query_fixture, load_receipt
from bb_assistant.kb.redamon_sources import RedAmonSnapshot
from bb_assistant.kb.rebuild import export_redamon_snapshot, import_snapshot


_HASH_A = "a" * 64
_HASH_B = "b" * 64


def _receipt(producer: str) -> dict[str, object]:
    return {
        "schema_version": 1,
        "producer": producer,
        "profile": "full",
        "corpus": {
            "snapshot_id": _HASH_A,
            "source_manifest_sha256": _HASH_A,
            "source_documents_sha256": _HASH_B,
            "chunks_sha256": _HASH_A,
            "source_count": 1,
            "document_count": 1,
            "chunk_count": 1,
            "coverage_state": "DONE",
            "vectors": {
                "model": "intfloat/e5-large-v2",
                "revision": "e5-pinned",
                "dimensions": 1024,
                "embedded_chunks": 1,
                "total_content_chunks": 1,
                "complete": True,
            },
            "reranker": {"model": "BAAI/bge-reranker-v2-m3", "revision": "bge-pinned"},
            "retrieval": {
                "lexical_engine": "neo4j-lucene",
                "vector_engine": "faiss",
                "candidate_pool": 30,
                "rrf_k": 60,
                "rerank_pool_size": 30,
                "mmr_lambda": 0.65,
            },
        },
        "sources": [{
            "name": "owasp",
            "revision": "pinned-source-revision",
            "filters": {"profile": "full"},
            "document_count": 1,
            "chunk_count": 1,
            "content_sha256": _HASH_B,
            "chunk_sha256": _HASH_A,
            "complete": True,
        }],
        "queries": [{
            "id": "authorization-boundary",
            "query": "authorization boundary",
            "top_k": 1,
            "filters": {"source_names": []},
            "results": [{"chunk_id": "chunk-a", "score_components": {"rrf": 0.5, "rerank": 0.9}}],
        }],
    }


def _fixture() -> dict[str, object]:
    return {"schema_version": 1, "queries": _receipt("redamon")["queries"]}


def test_load_receipt_rejects_incomplete_vectors(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["corpus"]["vectors"]["embedded_chunks"] = 0  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="explicitly complete|incomplete"):
        load_receipt(path, expected_producer="redamon")


def test_compare_receipts_passes_exact_complete_evidence() -> None:
    result = compare_receipts(_receipt("redamon"), _receipt("graph-kb-assistant"), _fixture())

    assert result["status"] == "PASS"
    assert result["failed_checks"] == 0
    assert result["passed_checks"] > 0


def test_compare_receipts_fails_on_rank_or_score_difference() -> None:
    redamon = _receipt("redamon")
    assistant = _receipt("graph-kb-assistant")
    assistant["queries"][0]["results"][0]["score_components"]["rrf"] = 0.4  # type: ignore[index]

    result = compare_receipts(redamon, assistant, _fixture())

    assert result["status"] == "FAIL"
    failures = [row["criterion"] for row in result["checks"] if row["status"] == "FAIL"]
    assert failures == ["queries.authorization-boundary.rank_1.score.rrf"]


def test_load_receipt_rejects_duplicate_source_names(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["sources"].append(dict(receipt["sources"][0]))  # type: ignore[union-attr,index]
    receipt["corpus"]["source_count"] = 2  # type: ignore[index]
    receipt["corpus"]["document_count"] = 2  # type: ignore[index]
    receipt["corpus"]["chunk_count"] = 2  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="duplicate source family"):
        load_receipt(path, expected_producer="redamon")


def test_load_receipt_rejects_partial_corpus_even_if_both_sides_could_match(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["corpus"]["coverage_state"] = "PARTIAL"  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="coverage must be DONE"):
        load_receipt(path, expected_producer="redamon")


def test_load_receipt_rejects_duplicate_json_keys(tmp_path: Path) -> None:
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(_receipt("redamon"))[:-1] + ',"producer":"other"}', encoding="utf-8")

    with pytest.raises(ValueError, match="duplicate JSON key"):
        load_receipt(path, expected_producer="redamon")


def test_cli_exits_nonzero_for_parity_mismatch(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    redamon = tmp_path / "redamon.json"
    assistant = tmp_path / "assistant.json"
    fixture = tmp_path / "fixture.json"
    left = _receipt("redamon")
    right = _receipt("graph-kb-assistant")
    right["sources"][0]["revision"] = "changed"  # type: ignore[index]
    redamon.write_text(json.dumps(left), encoding="utf-8")
    assistant.write_text(json.dumps(right), encoding="utf-8")
    fixture.write_text(json.dumps(_fixture()), encoding="utf-8")

    assert main(["parity-check", "--redamon", str(redamon), "--assistant", str(assistant), "--fixture", str(fixture)]) == 3
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "FAIL"
    assert "sources.owasp.revision" in {row["criterion"] for row in output["checks"] if row["status"] == "FAIL"}


def test_parity_gate_rejects_matching_but_non_fixture_queries() -> None:
    redamon = _receipt("redamon")
    assistant = _receipt("graph-kb-assistant")
    for receipt in (redamon, assistant):
        receipt["queries"][0]["query"] = "cherry picked query"  # type: ignore[index]

    result = compare_receipts(redamon, assistant, _fixture())

    assert result["status"] == "FAIL"
    failures = {row["criterion"] for row in result["checks"] if row["status"] == "FAIL"}
    assert "queries.authorization-boundary.pinned_query" in failures
    assert "queries.authorization-boundary.pinned_query_graph_assistant" in failures


def test_load_query_fixture_rejects_duplicate_ids(tmp_path: Path) -> None:
    fixture = _fixture()
    fixture["queries"].append(dict(fixture["queries"][0]))  # type: ignore[union-attr,index]
    path = tmp_path / "fixture.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(ValueError, match="duplicate fixture query id"):
        load_query_fixture(path)


def test_local_parity_export_requires_complete_vectors_and_emits_receipt(tmp_path: Path) -> None:
    class FakeEmbedder:
        model_name = "intfloat/e5-large-v2"

        def embed_documents_batch(self, texts: list[str], *, batch_size: int) -> list[list[float]]:
            return [[1.0] + [0.0] * 1023 for _ in texts]

        def embed_query(self, query: str) -> list[float]:
            return [1.0] + [0.0] * 1023

    class FakeReranker:
        def score(self, query: str, texts: list[str]) -> list[float]:
            return [0.5] * len(texts)

    docs = tuple(SourceDocument.create(
        source_name="owasp", source_id=f"guide-{index}", title=f"Synthetic guide {index}",
        text=f"Synthetic authorization-boundary evidence record {index}.", source_revision="pinned-revision",
        metadata={"document_role": "content", "profile": "full"},
    ) for index in range(2))
    chunks = tuple(chunk for doc in docs for chunk in chunk_document(doc))
    source_coverage = ({
        "source_name": "owasp", "state": "done", "source_revision": "pinned-revision",
        "requested_filters": {"profile": "full"}, "applied_filters": {"profile": "full"},
        "document_count": len(docs), "chunk_count": len(chunks),
    },)
    snapshot = RedAmonSnapshot("c" * 64, "DONE", docs, chunks, source_coverage)
    snapshot_root = tmp_path / "snapshots"
    exported = export_redamon_snapshot(snapshot, snapshot_root)
    snapshot_dir = snapshot_root / str(exported["snapshot_id"])
    fixture = tmp_path / "fixture.json"
    fixture.write_text(json.dumps({"schema_version": 1, "queries": [{
        "id": "authorization-boundary", "query": "authorization boundary", "top_k": 1,
        "filters": {"source_names": []},
    }]}), encoding="utf-8")
    index = KBIndex(tmp_path / "index.sqlite3", embedder=FakeEmbedder(), reranker=FakeReranker())
    import_snapshot(snapshot_dir, index)
    output = tmp_path / "receipt.json"

    with pytest.raises(ValueError, match="complete E5 vectors"):
        export_assistant_receipt(
            index=KBIndex(index.path, read_only=True), snapshot_dir=snapshot_dir,
            fixture_path=fixture, output_path=output, profile="full",
        )
    index.rebuild_embeddings(FakeEmbedder())
    receipt = export_assistant_receipt(
        index=KBIndex(index.path, read_only=True, embedder=FakeEmbedder(), reranker=FakeReranker()),
        snapshot_dir=snapshot_dir, fixture_path=fixture, output_path=output, profile="full",
    )

    loaded = load_receipt(output, expected_producer="graph-kb-assistant")
    assert receipt == loaded
    assert receipt["corpus"]["vectors"]["complete"] is True
    assert receipt["sources"][0]["document_count"] == 2
    assert receipt["queries"][0]["results"][0]["score_components"]["vector_cosine"] == 1.0
    assert output.stat().st_mode & 0o077 == 0
