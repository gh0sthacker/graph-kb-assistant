from __future__ import annotations

import json
import hashlib
from pathlib import Path

import pytest

from bb_assistant.kb.cli import main
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.embeddings import (
    BGE_RERANKER_MODEL, BGE_RERANKER_REVISION, E5_MODEL, E5_REVISION,
    RERANKER_MAX_CHARS_PER_SIDE,
)
from bb_assistant.kb.index import KBIndex
from bb_assistant.kb.parity import compare_receipts, export_assistant_receipt, load_query_fixture, load_receipt
from bb_assistant.kb.redamon_sources import RedAmonSnapshot, SOURCE_FAMILIES
from bb_assistant.kb.rebuild import _validate_coverage_source_counts, export_redamon_snapshot, import_snapshot
from bb_assistant.kb.ranking import DEFAULT_SOURCE_BOOSTS


_HASH_A = "a" * 64
_HASH_B = "b" * 64


def _receipt(producer: str) -> dict[str, object]:
    sources = [{
        "name": name,
        "revision": "pinned-source-revision",
        "filters": {"profile": "full"},
        "document_count": 1,
        "chunk_count": 1,
        "content_sha256": _HASH_B,
        "chunk_sha256": _HASH_A,
        "complete": True,
    } for name in sorted(SOURCE_FAMILIES)]
    inventory_sources = [{
        "name": name,
        "chunk_count": 1,
        "sha256": hashlib.sha256(f"inventory-{name}".encode()).hexdigest(),
    } for name in sorted(SOURCE_FAMILIES)]
    return {
        "schema_version": 3,
        "producer": producer,
        "profile": "full",
        "corpus": {
            "snapshot_id": _HASH_A,
            "source_manifest_sha256": _HASH_A,
            "source_documents_sha256": _HASH_B,
            "chunks_sha256": _HASH_A,
            "source_count": len(SOURCE_FAMILIES),
            "document_count": len(SOURCE_FAMILIES),
            "chunk_count": len(SOURCE_FAMILIES),
            "coverage_state": "DONE",
            "runtime_inventory": {
                "chunk_count": len(SOURCE_FAMILIES),
                "sha256": _HASH_A,
                "sources": inventory_sources,
            },
            "vectors": {
                "model": E5_MODEL,
                "revision": E5_REVISION,
                "dimensions": 1024,
                "embedded_chunks": len(SOURCE_FAMILIES),
                "total_content_chunks": len(SOURCE_FAMILIES),
                "complete": True,
            },
            "reranker": {"model": BGE_RERANKER_MODEL, "revision": BGE_RERANKER_REVISION},
            "retrieval": {
                "lexical_engine": "neo4j-lucene",
                "vector_engine": "faiss",
                "candidate_pool": 30,
                "rrf_k": 60,
                "rerank_pool_size": 30,
                "mmr_lambda": 0.65,
                "mmr_enabled": True,
                "source_boosts": dict(DEFAULT_SOURCE_BOOSTS),
                "reranker_enabled": True,
                "reranker_max_chars_per_side": RERANKER_MAX_CHARS_PER_SIDE,
            },
        },
        "sources": sources,
        "queries": [{
            "id": "authorization-boundary",
            "query": "authorization boundary",
            "top_k": 5,
            "filters": {"source_names": []},
            "results": [{"chunk_id": f"chunk-{rank}", "score_components": {
                "lexical_score": 0.5, "vector_cosine": 0.8,
                "reranker_logit": 0.9, "combined_rank": 0.9,
            }} for rank in range(5)],
        }],
    }


def _fixture() -> dict[str, object]:
    return {"schema_version": 1, "queries": _receipt("redamon")["queries"]}


def test_done_coverage_counts_must_match_the_snapshot_records() -> None:
    report = {"coverage_state": "DONE", "sources": [{
        "source_name": "owasp", "state": "done", "document_count": 1, "chunk_count": 2,
    }]}

    with pytest.raises(ValueError, match="chunk_count does not match snapshot"):
        _validate_coverage_source_counts(report, {"owasp": 1}, {"owasp": 1})

    _validate_coverage_source_counts(report, {"owasp": 1}, {"owasp": 2})


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
    assistant["queries"][0]["results"][0]["score_components"]["combined_rank"] = 0.4  # type: ignore[index]

    result = compare_receipts(redamon, assistant, _fixture())

    assert result["status"] == "FAIL"
    failures = [row["criterion"] for row in result["checks"] if row["status"] == "FAIL"]
    assert failures == ["queries.authorization-boundary.rank_1.score.combined_rank"]


def test_load_receipt_rejects_partial_source_set_labeled_full(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["sources"] = receipt["sources"][:-1]  # type: ignore[index]
    receipt["corpus"]["source_count"] -= 1  # type: ignore[index]
    receipt["corpus"]["document_count"] -= 1  # type: ignore[index]
    receipt["corpus"]["chunk_count"] -= 1  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="pinned RedAmon family set"):
        load_receipt(path, expected_producer="redamon")


def test_load_receipt_rejects_schema_v1(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["schema_version"] = 1
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="schema_version must be 3"):
        load_receipt(path, expected_producer="redamon")


def test_load_receipt_rejects_unpinned_redamon_search_engines(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["corpus"]["retrieval"]["vector_engine"] = "sqlite-cosine"  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="must use the pinned Neo4j Lucene and FAISS"):
        load_receipt(path, expected_producer="redamon")


def test_load_receipt_rejects_unknown_retrieval_settings(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["corpus"]["retrieval"]["unverified_knob"] = 123  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="exactly the pinned full-profile settings"):
        load_receipt(path, expected_producer="redamon")


def test_compare_receipts_uses_runtime_inventory_not_private_snapshot_ids() -> None:
    redamon = _receipt("redamon")
    assistant = _receipt("graph-kb-assistant")
    redamon["corpus"]["snapshot_id"] = "a" * 64  # type: ignore[index]
    assistant["corpus"]["snapshot_id"] = "b" * 64  # type: ignore[index]
    redamon["corpus"]["source_manifest_sha256"] = "c" * 64  # type: ignore[index]
    assistant["corpus"]["source_manifest_sha256"] = "d" * 64  # type: ignore[index]

    result = compare_receipts(redamon, assistant, _fixture())

    assert result["status"] == "PASS"


def test_compare_receipts_fails_when_runtime_inventory_differs() -> None:
    redamon = _receipt("redamon")
    assistant = _receipt("graph-kb-assistant")
    assistant["corpus"]["runtime_inventory"]["sources"][0]["sha256"] = _HASH_B  # type: ignore[index]

    result = compare_receipts(redamon, assistant, _fixture())

    assert result["status"] == "FAIL"
    assert "corpus.runtime_inventory" in {row["criterion"] for row in result["checks"] if row["status"] == "FAIL"}


@pytest.mark.parametrize("field,value", [
    ("candidate_pool", 29),
    ("rrf_k", 61),
    ("rerank_pool_size", 29),
    ("mmr_lambda", 0.5),
    ("reranker_max_chars_per_side", 1800),
])
def test_load_receipt_rejects_matching_but_wrong_retrieval_policy(
    tmp_path: Path, field: str, value: object,
) -> None:
    receipt = _receipt("redamon")
    receipt["corpus"]["retrieval"][field] = value  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match=f"retrieval\\.{field} must match"):
        load_receipt(path, expected_producer="redamon")


def test_load_receipt_rejects_cpu_lite_as_full_parity_evidence(tmp_path: Path) -> None:
    receipt = _receipt("redamon")
    receipt["profile"] = "cpu-lite"
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="profile must be 'full'"):
        load_receipt(path, expected_producer="redamon")


@pytest.mark.parametrize("bad_results", [[], [{"chunk_id": "chunk-a", "score_components": {"rrf": 0.5}}]])
def test_load_receipt_rejects_empty_or_incomplete_query_evidence(tmp_path: Path, bad_results: list[object]) -> None:
    receipt = _receipt("redamon")
    receipt["queries"][0]["results"] = bad_results  # type: ignore[index]
    path = tmp_path / "receipt.json"
    path.write_text(json.dumps(receipt), encoding="utf-8")

    with pytest.raises(ValueError, match="exactly top_k results|required score component"):
        load_receipt(path, expected_producer="redamon")


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
    right["corpus"]["runtime_inventory"]["sha256"] = _HASH_B  # type: ignore[index]
    redamon.write_text(json.dumps(left), encoding="utf-8")
    assistant.write_text(json.dumps(right), encoding="utf-8")
    fixture.write_text(json.dumps(_fixture()), encoding="utf-8")

    assert main(["parity-check", "--redamon", str(redamon), "--assistant", str(assistant), "--fixture", str(fixture)]) == 3
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "FAIL"
    assert "corpus.runtime_inventory" in {row["criterion"] for row in output["checks"] if row["status"] == "FAIL"}


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


def test_load_query_fixture_rejects_unapplied_filters(tmp_path: Path) -> None:
    fixture = _fixture()
    fixture["queries"][0]["filters"]["unknown_filter"] = ["nvd"]  # type: ignore[index]
    path = tmp_path / "fixture.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(ValueError, match="unsupported filter"):
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

    source_text = {
        "gtfobins": "---\nfunctions:\n  shell:\n    - description: safe fixture\n      code: echo safe\n---\n",
        "lolbas": "Name: Fixture\nDescription: safe fixture\nCommands:\n  - Command: fixture.exe\n    Description: safe fixture\n",
        "nuclei": "id: fixture-template\ninfo:\n  name: Fixture\n  severity: info\n",
    }
    docs = tuple(SourceDocument.create(
        source_name=source_name, source_id=f"guide-{source_name}", title=f"Synthetic guide {source_name}",
        text=source_text.get(source_name, f"Synthetic authorization-boundary evidence record for {source_name}."),
        source_revision="pinned-revision", metadata={
            "document_role": "content", "profile": "full", "binary_name": "fixture",
            "template_id": "fixture-template",
        },
    ) for source_name in SOURCE_FAMILIES)
    chunks = tuple(chunk for doc in docs for chunk in chunk_document(doc))
    source_coverage = tuple({
        "source_name": source_name, "state": "done", "source_revision": "pinned-revision",
        "requested_filters": {"profile": "full"}, "applied_filters": {"profile": "full"},
        "document_count": 1, "chunk_count": sum(1 for chunk in chunks if chunk.source_name == source_name),
    } for source_name in SOURCE_FAMILIES)
    snapshot = RedAmonSnapshot("c" * 64, "DONE", docs, chunks, source_coverage)
    snapshot_root = tmp_path / "snapshots"
    exported = export_redamon_snapshot(snapshot, snapshot_root)
    snapshot_dir = snapshot_root / str(exported["snapshot_id"])
    fixture = tmp_path / "fixture.json"
    fixture.write_text(json.dumps({"schema_version": 1, "queries": [{
        "id": "authorization-boundary", "query": "authorization boundary", "top_k": 5,
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
    assert receipt["corpus"]["document_count"] == len(SOURCE_FAMILIES)
    assert receipt["queries"][0]["results"][0]["score_components"]["vector_cosine"] == 1.0
    assert output.stat().st_mode & 0o077 == 0
