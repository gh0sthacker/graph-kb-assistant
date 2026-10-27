"""Fail-closed comparison of RedAmon and Graph Assistant KB evidence exports.

Receipts are intentionally supplied by the operator: this module does not
fetch sources, inspect a running RedAmon service, or claim that an export is
authentic. It makes the parity criteria reproducible once both sides export
the same versioned evidence.
"""
from __future__ import annotations

import json
import hashlib
import math
import os
from pathlib import Path
import tempfile
from typing import Any, Mapping

from .redamon_sources import SOURCE_FAMILIES
from .ranking import (
    DEFAULT_MMR_LAMBDA, DEFAULT_OVERFETCH_FACTOR, DEFAULT_RERANK_POOL_SIZE,
    DEFAULT_RRF_K, DEFAULT_SOURCE_BOOSTS, DEFAULT_TOP_K,
)


_HASH_LENGTH = 64
_MAX_RECEIPT_BYTES = 64 << 20
_COVERAGE_STATES = {"DONE", "PARTIAL", "BLOCKED"}
_SUPPORTED_CHUNK_SCHEMAS = {"redamon-compatible-chunk-v1", "redamon-compatible-chunk-v2"}
_PARITY_PROFILE = "full"
_FULL_PROFILE_SOURCE_FAMILIES = frozenset(SOURCE_FAMILIES)
_REQUIRED_SCORE_COMPONENTS = frozenset({
    "lexical_score", "vector_cosine", "reranker_logit", "combined_rank",
})


def export_assistant_receipt(
    *, index: object, snapshot_dir: Path, fixture_path: Path, output_path: Path, profile: str,
) -> dict[str, Any]:
    """Create a local parity receipt from a completed immutable KB snapshot.

    The index is opened read-only by the CLI. This deliberately refuses an
    active SQLite WAL, partial vectors, a stale snapshot, or incomplete query
    tiers so an export can never turn partial state into parity evidence.
    """
    from .embeddings import (
        BGE_RERANKER_MODEL, BGE_RERANKER_REVISION, E5_MODEL, E5_REVISION,
        RERANKER_MAX_CHARS_PER_SIDE,
    )
    from .index import KBIndex
    from .ranking import DEFAULT_OVERFETCH_FACTOR
    from .rebuild import _validate_coverage_report

    if not isinstance(index, KBIndex) or not index.read_only:
        raise ValueError("parity export requires a read-only KBIndex")
    if not isinstance(profile, str) or not profile.strip() or profile.strip() != profile:
        raise ValueError("parity export requires an explicit profile")
    fixture = load_query_fixture(fixture_path)
    root = Path(snapshot_dir).expanduser()
    if root.is_symlink() or not root.is_dir():
        raise ValueError("snapshot path must be a regular version directory")
    root = root.resolve(strict=True)
    manifest_path = root / "manifest.json"
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise ValueError("snapshot manifest is missing or not a regular file")
    try:
        manifest = json.loads(manifest_path.read_text("utf-8"), object_pairs_hook=_unique_object_pairs,
                              parse_constant=_reject_constant)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("snapshot manifest is unreadable") from exc
    _require(isinstance(manifest, dict), "snapshot manifest must be an object")
    assert isinstance(manifest, dict)
    snapshot_id = manifest.get("snapshot_id")
    _require(_is_hash(snapshot_id) and snapshot_id == root.name, "snapshot identity does not match its directory")
    _require(manifest.get("snapshot_dir") == snapshot_id, "snapshot directory marker is invalid")
    _require(manifest.get("export_complete") is True, "snapshot export is incomplete")
    coverage_state, coverage_hash, coverage = _validate_coverage_report(manifest)
    _require(coverage_state == "DONE" and isinstance(coverage, Mapping), "snapshot source coverage must be DONE")
    _require(manifest.get("chunk_schema") in _SUPPORTED_CHUNK_SCHEMAS, "snapshot chunk schema is unsupported")
    identity_payload = {
        "source_manifest_sha256": manifest.get("source_manifest_sha256"),
        "source_documents_sha256": manifest.get("source_documents_sha256"),
        "chunks_sha256": manifest.get("chunks_sha256"),
        "source_coverage_report_sha256": coverage_hash,
        "chunk_schema": manifest.get("chunk_schema"),
        "max_chars": manifest.get("max_chars"),
        "overlap_chars": manifest.get("overlap_chars"),
    }
    expected_id = hashlib.sha256(json.dumps(identity_payload, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    _require(expected_id == snapshot_id, "snapshot ID does not bind the manifest evidence")

    stats = index.stats()
    _require(stats.get("snapshot_id") == snapshot_id, "index and snapshot IDs differ")
    _require(stats.get("coverage_state") == "DONE", "index source coverage must be DONE")
    _require(stats.get("maintenance_state") is None, "index maintenance is in progress")
    with index.connect() as connection:
        meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
        total_content_chunks = int(connection.execute(
            "SELECT count(*) FROM chunks WHERE COALESCE(json_extract(metadata_json,'$.document_role'),'content')='content'"
        ).fetchone()[0])
        embedded_chunks = int(connection.execute(
            "SELECT count(*) FROM chunk_embeddings WHERE model_name=?", (E5_MODEL,)
        ).fetchone()[0])
        missing_vectors = int(connection.execute(
            "SELECT count(*) FROM chunks c LEFT JOIN chunk_embeddings e ON e.chunk_id=c.chunk_id AND e.model_name=? "
            "WHERE COALESCE(json_extract(c.metadata_json,'$.document_role'),'content')='content' AND e.chunk_id IS NULL",
            (E5_MODEL,),
        ).fetchone()[0])
        dimensions = connection.execute(
            "SELECT DISTINCT dimensions FROM chunk_embeddings WHERE model_name=?", (E5_MODEL,)
        ).fetchall()
    _require(not meta.get("vector_staging_snapshot_id"), "vector generation is still in progress")
    _require(meta.get("vector_model") == E5_MODEL and meta.get("vector_snapshot_id") == snapshot_id,
             "complete E5 vectors for this snapshot are not published")
    _require(total_content_chunks > 0 and embedded_chunks == total_content_chunks and missing_vectors == 0,
             "vector coverage is incomplete")
    _require(len(dimensions) == 1 and int(dimensions[0][0]) == 1024, "E5 vector dimensions are incomplete or inconsistent")

    source_rows = coverage.get("sources")
    _require(isinstance(source_rows, list) and bool(source_rows), "snapshot source coverage rows are missing")
    families: dict[str, dict[str, Any]] = {}
    for row in source_rows:
        _require(isinstance(row, dict) and row.get("state") == "done", "every source family must be complete")
        assert isinstance(row, dict)
        name = row.get("source_name")
        revision = row.get("source_revision")
        _require(isinstance(name, str) and name and name not in families, "source family names must be unique")
        _require(isinstance(revision, str) and revision, f"source family {name} revision is unavailable")
        requested = row.get("requested_filters")
        applied = row.get("applied_filters")
        _require(isinstance(requested, dict) and isinstance(applied, dict), f"source family {name} filters are unavailable")
        families[name] = {
            "name": name, "revision": revision, "filters": {"requested": requested, "applied": applied},
            "document_count": 0, "chunk_count": 0, "content_sha256": hashlib.sha256(),
            "chunk_sha256": hashlib.sha256(), "complete": True,
        }

    def digest_families(filename: str, expected_hash: object, expected_count: object, field: str) -> None:
        _require(_is_hash(expected_hash) and _is_nonnegative_int(expected_count), f"snapshot {filename} manifest fields are invalid")
        path = root / filename
        _require(not path.is_symlink() and path.is_file(), f"snapshot {filename} is not a regular file")
        _require(path.stat().st_size == manifest.get("source_documents_bytes" if field == "content_sha256" else "chunks_bytes"),
                 f"snapshot {filename} byte count mismatch")
        file_digest = hashlib.sha256()
        row_count = 0
        previous_by_family: dict[str, tuple[str, int] | tuple[str]] = {}
        with path.open("rb") as stream:
            for line_number, raw_line in enumerate(stream, start=1):
                file_digest.update(raw_line)
                _require(raw_line.endswith(b"\n"), f"snapshot {filename} has an unterminated row")
                try:
                    record = json.loads(raw_line[:-1].decode("utf-8"), object_pairs_hook=_unique_object_pairs,
                                        parse_constant=_reject_constant)
                except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                    raise ValueError(f"snapshot {filename} has invalid JSON at line {line_number}") from exc
                _require(isinstance(record, dict), f"snapshot {filename} rows must be objects")
                assert isinstance(record, dict)
                encoded = json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n"
                _require(encoded == raw_line, f"snapshot {filename} row is not canonical JSON")
                name, source_id = record.get("source_name"), record.get("source_id")
                _require(isinstance(name, str) and name in families and isinstance(source_id, str),
                         f"snapshot {filename} contains an unknown source family or invalid identity")
                if field == "content_sha256":
                    key: tuple[str, int] | tuple[str] = (source_id,)
                    metadata = record.get("metadata")
                    _require(isinstance(metadata, dict) and metadata.get("profile") == profile,
                             f"snapshot source profile differs from requested profile for {name}")
                    _require(record.get("source_revision") == families[name]["revision"],
                             f"snapshot source revision differs from coverage for {name}")
                    count_field = "document_count"
                else:
                    ordinal = record.get("ordinal")
                    _require(isinstance(ordinal, int) and not isinstance(ordinal, bool) and ordinal >= 0,
                             f"snapshot {filename} has an invalid chunk ordinal")
                    key = (source_id, ordinal)
                    count_field = "chunk_count"
                previous = previous_by_family.get(name)
                _require(previous is None or key >= previous, f"snapshot {filename} is not in canonical order")
                previous_by_family[name] = key
                families[name][field].update(encoded)
                families[name][count_field] += 1
                row_count += 1
        _require(row_count == expected_count and file_digest.hexdigest() == expected_hash,
                 f"snapshot {filename} digest or row count mismatch")

    digest_families("source-documents.jsonl", manifest.get("source_documents_sha256"), manifest.get("document_count"), "content_sha256")
    digest_families("chunks.jsonl", manifest.get("chunks_sha256"), manifest.get("chunk_count"), "chunk_sha256")
    source_receipts = []
    for name in sorted(families):
        row = families[name]
        coverage_row = next(item for item in source_rows if isinstance(item, dict) and item.get("source_name") == name)
        _require(row["document_count"] == coverage_row.get("document_count"), f"source family {name} document count mismatch")
        _require(row["chunk_count"] == coverage_row.get("chunk_count"), f"source family {name} chunk count mismatch")
        source_receipts.append({
            key: value for key, value in row.items()
            if key not in {"content_sha256", "chunk_sha256"}
        } | {
            "content_sha256": row["content_sha256"].hexdigest(),
            "chunk_sha256": row["chunk_sha256"].hexdigest(),
        })
    _require(len(source_receipts) == manifest.get("source_count"), "snapshot source family count mismatch")
    _require(sum(row["document_count"] for row in source_receipts) == manifest.get("document_count"), "snapshot document count mismatch")
    _require(sum(row["chunk_count"] for row in source_receipts) == manifest.get("chunk_count"), "snapshot chunk count mismatch")

    queries = []
    for spec in fixture["queries"]:
        hits = index.search(spec["query"], top_k=spec["top_k"], source_names=tuple(spec["filters"].get("source_names", ())))
        _require(bool(hits), f"pinned query {spec['id']} returned no results")
        result_rows = []
        for hit in hits:
            _require(hit.vector_score is not None, f"pinned query {spec['id']} did not use the vector tier")
            _require(hit.rerank_score is not None, f"pinned query {spec['id']} did not use the reranker")
            result_rows.append({"chunk_id": hit.chunk_id, "score_components": {
                "lexical_score": hit.bm25, "vector_cosine": hit.vector_score,
                "reranker_logit": hit.rerank_score, "combined_rank": hit.rank_score,
            }})
        queries.append({**spec, "results": result_rows})

    receipt = {
        "schema_version": 2, "producer": "graph-kb-assistant", "profile": profile,
        "corpus": {
            key: manifest[key] for key in (
                "snapshot_id", "source_manifest_sha256", "source_documents_sha256", "chunks_sha256",
                "source_count", "document_count", "chunk_count",
            )
        } | {
            "coverage_state": coverage_state,
            "vectors": {"model": E5_MODEL, "revision": E5_REVISION, "dimensions": 1024,
                        "embedded_chunks": embedded_chunks, "total_content_chunks": total_content_chunks, "complete": True},
            "reranker": {"model": BGE_RERANKER_MODEL, "revision": BGE_RERANKER_REVISION},
            "retrieval": {
                "lexical_engine": index.lexical_engine, "vector_engine": "sqlite-cosine",
                "candidate_pool": max(DEFAULT_TOP_K * DEFAULT_OVERFETCH_FACTOR, DEFAULT_RERANK_POOL_SIZE),
                "rrf_k": DEFAULT_RRF_K, "rerank_pool_size": DEFAULT_RERANK_POOL_SIZE,
                "mmr_lambda": DEFAULT_MMR_LAMBDA, "mmr_enabled": True,
                "source_boosts": dict(DEFAULT_SOURCE_BOOSTS), "reranker_enabled": True,
                "reranker_max_chars_per_side": RERANKER_MAX_CHARS_PER_SIDE,
            },
        },
        "sources": source_receipts, "queries": queries,
        "evidence": {"source_coverage_report_sha256": coverage_hash, "snapshot_manifest_sha256": hashlib.sha256(manifest_path.read_bytes()).hexdigest()},
    }
    load_receipt_from_value(receipt, expected_producer="graph-kb-assistant")
    destination = Path(output_path).expanduser()
    if destination.is_symlink():
        raise ValueError("receipt output must not be a symbolic link")
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=f".{destination.name}.", dir=destination.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write((json.dumps(receipt, ensure_ascii=False, indent=2, sort_keys=True) + "\n").encode("utf-8"))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, destination)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
    return receipt


def load_receipt_from_value(value: Mapping[str, Any], *, expected_producer: str) -> dict[str, Any]:
    """Apply the on-disk receipt validator to an in-memory export result."""
    import tempfile

    fd, temporary = tempfile.mkstemp(prefix="kb-parity-receipt-", suffix=".json")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(value, stream, ensure_ascii=False)
        return load_receipt(Path(temporary), expected_producer=expected_producer)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


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
    _require(value.get("schema_version") == 2 and not isinstance(value.get("schema_version"), bool), "receipt schema_version must be 2")
    _require(value.get("producer") == expected_producer, f"receipt producer must be {expected_producer}")
    profile = value.get("profile")
    _require(profile == _PARITY_PROFILE, f"receipt profile must be {_PARITY_PROFILE!r} for full KB parity")

    corpus = value.get("corpus")
    _require(isinstance(corpus, dict), "receipt corpus must be an object")
    assert isinstance(corpus, dict)
    for field in ("snapshot_id", "source_manifest_sha256", "source_documents_sha256", "chunks_sha256"):
        _require(_is_hash(corpus.get(field)), f"corpus.{field} must be a lowercase SHA-256 digest")
    for field in ("source_count", "document_count", "chunk_count"):
        _require(_is_nonnegative_int(corpus.get(field)), f"corpus.{field} must be a non-negative integer")
        _require(corpus[field] > 0, f"corpus.{field} must be positive")
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
    _require(vectors["dimensions"] > 0 and vectors["total_content_chunks"] > 0, "vector dimensions and content count must be positive")
    from .embeddings import (
        BGE_RERANKER_MODEL, BGE_RERANKER_REVISION, E5_MODEL, E5_REVISION,
    )
    _require(
        vectors["model"] == E5_MODEL and vectors["revision"] == E5_REVISION
        and vectors["dimensions"] == 1024,
        "full profile vectors must use the pinned E5-large-v2 model revision and 1024 dimensions",
    )
    _require(vectors.get("complete") is True, "corpus vectors must be explicitly complete")
    _require(vectors["embedded_chunks"] == vectors["total_content_chunks"], "vector counts are incomplete")

    reranker = corpus.get("reranker")
    _require(isinstance(reranker, dict), "corpus.reranker evidence is required")
    assert isinstance(reranker, dict)
    for field in ("model", "revision"):
        _require(isinstance(reranker.get(field), str) and bool(reranker[field]), f"corpus.reranker.{field} is required")
    _require(
        reranker["model"] == BGE_RERANKER_MODEL and reranker["revision"] == BGE_RERANKER_REVISION,
        "full profile reranker must use the pinned BGE model revision",
    )
    retrieval = corpus.get("retrieval")
    _require(isinstance(retrieval, dict) and bool(retrieval), "corpus retrieval recipe is required")
    assert isinstance(retrieval, dict)
    for field in ("lexical_engine", "vector_engine"):
        _require(isinstance(retrieval.get(field), str) and bool(retrieval[field]), f"corpus.retrieval.{field} is required")
    if expected_producer == "redamon":
        _require(
            retrieval["lexical_engine"] == "neo4j-lucene"
            and retrieval["vector_engine"] == "faiss",
            "RedAmon full-profile receipt must use the pinned Neo4j Lucene and FAISS engines",
        )
    expected_retrieval = {
        "lexical_engine": "neo4j-lucene" if expected_producer == "redamon" else retrieval["lexical_engine"],
        "vector_engine": "faiss" if expected_producer == "redamon" else retrieval["vector_engine"],
        "candidate_pool": max(DEFAULT_TOP_K * DEFAULT_OVERFETCH_FACTOR, DEFAULT_RERANK_POOL_SIZE),
        "rrf_k": DEFAULT_RRF_K,
        "rerank_pool_size": DEFAULT_RERANK_POOL_SIZE,
        "mmr_lambda": DEFAULT_MMR_LAMBDA,
        "mmr_enabled": True,
        "source_boosts": DEFAULT_SOURCE_BOOSTS,
        "reranker_enabled": True,
        "reranker_max_chars_per_side": 1920,
    }
    _require(
        set(retrieval) == set(expected_retrieval),
        "corpus.retrieval must contain exactly the pinned full-profile settings",
    )
    for field, expected in expected_retrieval.items():
        _require(retrieval.get(field) == expected, f"corpus.retrieval.{field} must match the pinned full-profile policy")
    for field in ("candidate_pool", "rrf_k", "rerank_pool_size"):
        _require(_is_nonnegative_int(retrieval.get(field)), f"corpus.retrieval.{field} must be a positive integer")
    _require(retrieval.get("mmr_enabled") is True, "corpus.retrieval.mmr_enabled must be true for full parity")
    _require(retrieval.get("reranker_enabled") is True, "corpus.retrieval.reranker_enabled must be true for full parity")
    _require(_is_nonnegative_int(retrieval.get("reranker_max_chars_per_side")),
             "corpus.retrieval.reranker_max_chars_per_side must be a positive integer")
    source_boosts = retrieval.get("source_boosts")
    _require(isinstance(source_boosts, dict) and set(source_boosts) == _FULL_PROFILE_SOURCE_FAMILIES,
             "corpus.retrieval.source_boosts must cover the full profile")
    assert isinstance(source_boosts, dict)
    for name, boost in source_boosts.items():
        _require(isinstance(boost, (int, float)) and not isinstance(boost, bool)
                 and math.isfinite(boost) and boost > 0,
                 f"corpus.retrieval.source_boosts.{name} must be positive and finite")
    mmr_lambda = retrieval.get("mmr_lambda")
    _require(isinstance(mmr_lambda, (int, float)) and not isinstance(mmr_lambda, bool) and math.isfinite(mmr_lambda) and 0 <= mmr_lambda <= 1, "corpus.retrieval.mmr_lambda must be between 0 and 1")

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
            _require(source[field] > 0, f"source family {name} {field} must be positive")
        for field in ("content_sha256", "chunk_sha256"):
            _require(_is_hash(source.get(field)), f"source family {name} {field} is invalid")
    _require(len(sources) == corpus["source_count"], "source family count does not match corpus")
    _require(
        source_names == _FULL_PROFILE_SOURCE_FAMILIES,
        "full profile source families do not match the pinned RedAmon family set",
    )
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
        _require(_is_nonnegative_int(query.get("top_k")) and query["top_k"] == DEFAULT_TOP_K, f"query {query_id} top_k must match the pinned default")
        _require(isinstance(query.get("filters"), dict), f"query {query_id} filters are required")
        filters = query["filters"]
        _require(set(filters) <= {"source_names"}, f"query {query_id} contains an unsupported filter")
        if "source_names" in filters:
            source_filter = filters["source_names"]
            _require(
                isinstance(source_filter, list)
                and all(isinstance(name, str) and name in _FULL_PROFILE_SOURCE_FAMILIES for name in source_filter)
                and len(source_filter) == len(set(source_filter)),
                f"query {query_id} source_names filter is invalid",
            )
        results = query.get("results")
        _require(
            isinstance(results, list) and len(results) == query["top_k"],
            f"query {query_id} must return exactly top_k results",
        )
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
            _require(
                isinstance(components, dict) and set(components) == _REQUIRED_SCORE_COMPONENTS,
                f"query {query_id} must include every required score component",
            )
            assert isinstance(components, dict)
            for name, score in components.items():
                _require(isinstance(name, str) and isinstance(score, (int, float)) and not isinstance(score, bool) and math.isfinite(score), f"query {query_id} has invalid score component")
    return value


def load_query_fixture(path: Path) -> dict[str, Any]:
    """Load the pinned public query fixture used to prevent query cherry-picking."""
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise ValueError("cannot read KB parity query fixture") from exc
    if len(raw) > _MAX_RECEIPT_BYTES:
        raise ValueError("query fixture exceeds the 64 MiB size limit")
    try:
        value = json.loads(raw, object_pairs_hook=_unique_object_pairs, parse_constant=_reject_constant)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("KB parity query fixture is invalid JSON") from exc
    _require(isinstance(value, dict), "KB parity query fixture must be an object")
    assert isinstance(value, dict)
    version = value.get("schema_version")
    _require(version == 1 and not isinstance(version, bool), "KB parity query fixture schema_version must be 1")
    queries = value.get("queries")
    _require(isinstance(queries, list) and bool(queries), "KB parity query fixture must contain queries")
    assert isinstance(queries, list)
    ids: set[str] = set()
    for index, query in enumerate(queries):
        _require(isinstance(query, dict), f"fixture queries[{index}] must be an object")
        assert isinstance(query, dict)
        query_id = query.get("id")
        _require(isinstance(query_id, str) and bool(query_id), f"fixture queries[{index}].id is required")
        _require(query_id not in ids, f"duplicate fixture query id {query_id}")
        ids.add(query_id)
        _require(isinstance(query.get("query"), str) and bool(query["query"].strip()), f"fixture query {query_id} text is required")
        _require(_is_nonnegative_int(query.get("top_k")) and query["top_k"] == DEFAULT_TOP_K, f"fixture query {query_id} top_k must match the pinned default")
        _require(isinstance(query.get("filters"), dict), f"fixture query {query_id} filters are required")
        filters = query["filters"]
        _require(set(filters) <= {"source_names"}, f"fixture query {query_id} contains an unsupported filter")
        if "source_names" in filters:
            source_filter = filters["source_names"]
            _require(
                isinstance(source_filter, list)
                and all(isinstance(name, str) and name in _FULL_PROFILE_SOURCE_FAMILIES for name in source_filter)
                and len(source_filter) == len(set(source_filter)),
                f"fixture query {query_id} source_names filter is invalid",
            )
    return value


def compare_receipts(
    redamon: Mapping[str, Any],
    assistant: Mapping[str, Any],
    expected_queries: Mapping[str, Any],
    *,
    score_tolerance: float = 0.0,
) -> dict[str, Any]:
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
    fixture_queries = {row["id"]: row for row in expected_queries["queries"]}
    check("pinned_query_fixture_ids", sorted(fixture_queries), sorted(left_queries))
    check("pinned_query_fixture_ids_graph_assistant", sorted(fixture_queries), sorted(right_queries))
    check("query_fixture_ids", sorted(left_queries), sorted(right_queries))
    if set(left_queries) == set(right_queries):
        for query_id in sorted(left_queries):
            left, right = left_queries[query_id], right_queries[query_id]
            if query_id in fixture_queries:
                for field in ("query", "top_k", "filters"):
                    check(f"queries.{query_id}.pinned_{field}", fixture_queries[query_id][field], left[field])
                    check(f"queries.{query_id}.pinned_{field}_graph_assistant", fixture_queries[query_id][field], right[field])
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
        "schema_version": 2,
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
