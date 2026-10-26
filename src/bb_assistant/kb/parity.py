"""Fail-closed comparison of RedAmon and Graph Assistant KB evidence exports.

Receipts are intentionally supplied by the operator: this module does not
fetch sources, inspect a running RedAmon service, or claim that an export is
authentic. It makes the parity criteria reproducible once both sides export
the same versioned evidence.
"""
from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any, Mapping


_HASH_LENGTH = 64
_MAX_RECEIPT_BYTES = 64 << 20
_COVERAGE_STATES = {"DONE", "PARTIAL", "BLOCKED"}


def load_receipt(path: Path, *, expected_producer: str) -> dict[str, Any]:
    """Load and validate one parity receipt without accepting partial evidence."""
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise ValueError(f"cannot read {expected_producer} parity receipt") from exc
    if len(raw) > _MAX_RECEIPT_BYTES:
        raise ValueError("receipt exceeds the 64 MiB size limit")
    try:
        value = json.loads(raw, object_pairs_hook=_unique_object_pairs, parse_constant=_reject_constant)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"cannot read {expected_producer} parity receipt") from exc
    if not isinstance(value, dict):
        raise ValueError(f"{expected_producer} receipt must be a JSON object")
    _require(value.get("schema_version") == 1 and not isinstance(value.get("schema_version"), bool), "receipt schema_version must be 1")
    _require(value.get("producer") == expected_producer, f"receipt producer must be {expected_producer}")
    profile = value.get("profile")
    _require(isinstance(profile, str) and profile.strip() == profile and bool(profile), "receipt profile is required")

    corpus = value.get("corpus")
    _require(isinstance(corpus, dict), "receipt corpus must be an object")
    assert isinstance(corpus, dict)
    for field in ("snapshot_id", "source_manifest_sha256", "source_documents_sha256", "chunks_sha256"):
        _require(_is_hash(corpus.get(field)), f"corpus.{field} must be a lowercase SHA-256 digest")
    for field in ("source_count", "document_count", "chunk_count"):
        _require(_is_nonnegative_int(corpus.get(field)), f"corpus.{field} must be a non-negative integer")
    coverage = corpus.get("coverage_state")
    _require(isinstance(coverage, str) and coverage in _COVERAGE_STATES, "corpus.coverage_state is invalid")
    _require(coverage == "DONE", "corpus coverage must be DONE")

    vectors = corpus.get("vectors")
    _require(isinstance(vectors, dict), "corpus.vectors evidence is required")
    assert isinstance(vectors, dict)
    for field in ("model", "revision"):
        _require(isinstance(vectors.get(field), str) and bool(vectors[field]), f"corpus.vectors.{field} is required")
    for field in ("dimensions", "embedded_chunks", "total_content_chunks"):
        _require(_is_nonnegative_int(vectors.get(field)), f"corpus.vectors.{field} must be a non-negative integer")
    _require(vectors.get("complete") is True, "corpus vectors must be explicitly complete")
    _require(vectors["embedded_chunks"] == vectors["total_content_chunks"], "vector counts are incomplete")

    reranker = corpus.get("reranker")
    _require(isinstance(reranker, dict), "corpus.reranker evidence is required")
    assert isinstance(reranker, dict)
    for field in ("model", "revision"):
        _require(isinstance(reranker.get(field), str) and bool(reranker[field]), f"corpus.reranker.{field} is required")
    retrieval = corpus.get("retrieval")
    _require(isinstance(retrieval, dict) and bool(retrieval), "corpus retrieval recipe is required")

    sources = value.get("sources")
    _require(isinstance(sources, list) and bool(sources), "receipt sources must be a non-empty array")
    assert isinstance(sources, list)
    source_names: set[str] = set()
    for index, source in enumerate(sources):
        _require(isinstance(source, dict), f"sources[{index}] must be an object")
        assert isinstance(source, dict)
        name = source.get("name")
        _require(isinstance(name, str) and bool(name), f"sources[{index}].name is required")
        _require(name not in source_names, f"duplicate source family {name}")
        source_names.add(name)
        _require(source.get("complete") is True, f"source family {name} is not explicitly complete")
        _require(isinstance(source.get("revision"), str) and bool(source["revision"]), f"source family {name} revision is required")
        _require(isinstance(source.get("filters"), dict), f"source family {name} filters are required")
        for field in ("document_count", "chunk_count"):
            _require(_is_nonnegative_int(source.get(field)), f"source family {name} {field} is invalid")
        for field in ("content_sha256", "chunk_sha256"):
            _require(_is_hash(source.get(field)), f"source family {name} {field} is invalid")
    _require(len(sources) == corpus["source_count"], "source family count does not match corpus")
    _require(sum(row["document_count"] for row in sources) == corpus["document_count"], "source document counts do not match corpus")
    _require(sum(row["chunk_count"] for row in sources) == corpus["chunk_count"], "source chunk counts do not match corpus")

    queries = value.get("queries")
    _require(isinstance(queries, list) and bool(queries), "receipt queries must be a non-empty array")
    assert isinstance(queries, list)
    query_ids: set[str] = set()
    for index, query in enumerate(queries):
        _require(isinstance(query, dict), f"queries[{index}] must be an object")
        assert isinstance(query, dict)
        query_id = query.get("id")
        _require(isinstance(query_id, str) and bool(query_id), f"queries[{index}].id is required")
        _require(query_id not in query_ids, f"duplicate query id {query_id}")
        query_ids.add(query_id)
        _require(isinstance(query.get("query"), str) and bool(query["query"].strip()), f"query {query_id} text is required")
        _require(_is_nonnegative_int(query.get("top_k")) and 1 <= query["top_k"] <= 100, f"query {query_id} top_k is invalid")
        _require(isinstance(query.get("filters"), dict), f"query {query_id} filters are required")
        results = query.get("results")
        _require(isinstance(results, list) and len(results) <= query["top_k"], f"query {query_id} results are invalid")
        assert isinstance(results, list)
        result_ids: set[str] = set()
        for rank, result in enumerate(results):
            _require(isinstance(result, dict), f"query {query_id} result {rank} must be an object")
            assert isinstance(result, dict)
            chunk_id = result.get("chunk_id")
            _require(isinstance(chunk_id, str) and bool(chunk_id), f"query {query_id} result {rank} chunk_id is required")
            _require(chunk_id not in result_ids, f"query {query_id} has a duplicate result chunk_id")
            result_ids.add(chunk_id)
            components = result.get("score_components")
            _require(isinstance(components, dict) and bool(components), f"query {query_id} score components are required")
            assert isinstance(components, dict)
            for name, score in components.items():
                _require(isinstance(name, str) and isinstance(score, (int, float)) and not isinstance(score, bool) and math.isfinite(score), f"query {query_id} has invalid score component")
    return value


def compare_receipts(redamon: Mapping[str, Any], assistant: Mapping[str, Any], *, score_tolerance: float = 0.0) -> dict[str, Any]:
    """Compare validated evidence and return all mismatches, never a soft pass."""
    if not math.isfinite(score_tolerance) or score_tolerance < 0:
        raise ValueError("score tolerance must be finite and non-negative")
    checks: list[dict[str, Any]] = []

    def check(name: str, left: Any, right: Any, *, equal: bool | None = None) -> None:
        passed = left == right if equal is None else equal
        checks.append({"criterion": name, "status": "PASS" if passed else "FAIL", "redamon": left, "graph_assistant": right})

    check("selected_profile", redamon["profile"], assistant["profile"])
    for field in ("snapshot_id", "source_manifest_sha256", "source_documents_sha256", "chunks_sha256", "source_count", "document_count", "chunk_count", "coverage_state"):
        check(f"corpus.{field}", redamon["corpus"][field], assistant["corpus"][field])
    check("corpus.retrieval", redamon["corpus"]["retrieval"], assistant["corpus"]["retrieval"])
    for section, fields in (("vectors", ("model", "revision", "dimensions", "embedded_chunks", "total_content_chunks", "complete")), ("reranker", ("model", "revision"))):
        for field in fields:
            check(f"corpus.{section}.{field}", redamon["corpus"][section][field], assistant["corpus"][section][field])
    check("source_family_names", sorted(row["name"] for row in redamon["sources"]), sorted(row["name"] for row in assistant["sources"]))
    left_sources = {row["name"]: row for row in redamon["sources"]}
    right_sources = {row["name"]: row for row in assistant["sources"]}
    if set(left_sources) == set(right_sources):
        for name in sorted(left_sources):
            for field in ("revision", "filters", "document_count", "chunk_count", "content_sha256", "chunk_sha256", "complete"):
                check(f"sources.{name}.{field}", left_sources[name][field], right_sources[name][field])

    left_queries = {row["id"]: row for row in redamon["queries"]}
    right_queries = {row["id"]: row for row in assistant["queries"]}
    check("query_fixture_ids", sorted(left_queries), sorted(right_queries))
    if set(left_queries) == set(right_queries):
        for query_id in sorted(left_queries):
            left, right = left_queries[query_id], right_queries[query_id]
            for field in ("query", "top_k", "filters"):
                check(f"queries.{query_id}.{field}", left[field], right[field])
            left_results, right_results = left["results"], right["results"]
            check(f"queries.{query_id}.ordered_chunk_ids", [row["chunk_id"] for row in left_results], [row["chunk_id"] for row in right_results])
            if len(left_results) == len(right_results):
                for rank, (left_result, right_result) in enumerate(zip(left_results, right_results, strict=True)):
                    left_scores, right_scores = left_result["score_components"], right_result["score_components"]
                    check(f"queries.{query_id}.rank_{rank + 1}.score_component_names", sorted(left_scores), sorted(right_scores))
                    if set(left_scores) == set(right_scores):
                        for component in sorted(left_scores):
                            check(
                                f"queries.{query_id}.rank_{rank + 1}.score.{component}",
                                left_scores[component], right_scores[component],
                                equal=abs(left_scores[component] - right_scores[component]) <= score_tolerance,
                            )
    failed = sum(item["status"] == "FAIL" for item in checks)
    return {
        "schema_version": 1,
        "status": "PASS" if failed == 0 else "FAIL",
        "checks": checks,
        "passed_checks": len(checks) - failed,
        "failed_checks": failed,
        "score_tolerance": score_tolerance,
        "provenance_note": "The checker validates receipt shape and compares their claims; it does not authenticate who produced either receipt.",
    }


def _is_hash(value: object) -> bool:
    return isinstance(value, str) and len(value) == _HASH_LENGTH and all(char in "0123456789abcdef" for char in value)


def _is_nonnegative_int(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def _unique_object_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def _reject_constant(value: str) -> None:
    raise ValueError(f"non-standard JSON numeric value {value}")
