"""Portable, provenance-rich source snapshot and chunk export.

Feed-specific fetching and normalization belong in source adapters. This module
only serializes already-fetched source documents and deterministic chunks.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import asdict, fields
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile
from typing import BinaryIO, Iterable, Iterator, Mapping

from .canonical import CHUNK_SCHEMA, CanonicalChunk, SourceDocument, chunk_document


def _json_line(value: object) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n"


@contextmanager
def _atomic_writer(path: Path) -> Iterator[BinaryIO]:
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as stream:
            yield stream
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def export_snapshot(
    documents: Iterable[SourceDocument],
    output_dir: Path,
    *,
    source_manifest_sha256: str,
    source_coverage_report: Mapping[str, object] | None = None,
    max_chars: int = 6000,
    overlap_chars: int = 400,
) -> dict[str, object]:
    """Write original UTF-8 text, canonical chunks and a manifest atomically.

    The manifest is replaced last and acts as the snapshot commit marker. A
    failed run may leave temporary files, but it never advertises a partial
    snapshot as complete.
    """
    if len(source_manifest_sha256) != 64 or any(ch not in "0123456789abcdef" for ch in source_manifest_sha256):
        raise ValueError("source_manifest_sha256 must be a lowercase SHA-256 digest")
    coverage_report = dict(source_coverage_report) if source_coverage_report is not None else None
    coverage_state = "unverified"
    coverage_report_sha256 = None
    if coverage_report is not None:
        coverage_state = coverage_report.get("coverage_state")
        if not isinstance(coverage_state, str) or coverage_state not in {"DONE", "PARTIAL", "BLOCKED"}:
            raise ValueError("Coverage report must have DONE, PARTIAL or BLOCKED state")
        if not isinstance(coverage_report.get("sources"), list):
            raise ValueError("Coverage report must include per-source coverage rows")
        coverage_report_sha256 = hashlib.sha256(_json_line(coverage_report)).hexdigest()
    selected_dir = output_dir.expanduser().resolve()
    source_root = Path(__file__).resolve().parents[3]
    if selected_dir == source_root or source_root in selected_dir.parents:
        raise ValueError("KB snapshots must be written outside the source checkout")
    selected_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        selected_dir.chmod(0o700)
    except OSError:
        pass

    staging_dir = Path(tempfile.mkdtemp(prefix=".snapshot-", dir=selected_dir))
    try:
        staging_dir.chmod(0o700)
    except OSError:
        pass
    document_count = 0
    chunk_count = 0
    source_names: set[str] = set()
    source_digest = hashlib.sha256()
    chunks_digest = hashlib.sha256()
    source_size = 0
    chunks_size = 0
    try:
        with _atomic_writer(staging_dir / "source-documents.jsonl") as source_stream:
            with _atomic_writer(staging_dir / "chunks.jsonl") as chunk_stream:
                for document in documents:
                    expected = hashlib.sha256(document.text.encode("utf-8")).hexdigest()
                    if document.content_sha256 != expected:
                        raise ValueError(f"Source hash mismatch for source ID {document.source_id!r}")
                    line = _json_line(asdict(document))
                    source_stream.write(line)
                    source_digest.update(line)
                    source_size += len(line)
                    document_count += 1
                    source_names.add(document.source_name)
                    for chunk in chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars):
                        chunk_line = _json_line(asdict(chunk))
                        chunk_stream.write(chunk_line)
                        chunks_digest.update(chunk_line)
                        chunks_size += len(chunk_line)
                        chunk_count += 1
        if document_count == 0:
            raise ValueError("Cannot create an empty KB snapshot")
        source_digest_hex = source_digest.hexdigest()
        chunks_digest_hex = chunks_digest.hexdigest()
        snapshot_identity = {
            "source_manifest_sha256": source_manifest_sha256,
            "source_documents_sha256": source_digest_hex,
            "chunks_sha256": chunks_digest_hex,
            "source_coverage_report_sha256": coverage_report_sha256,
            "chunk_schema": "bb-kb-chunk-v1",
            "max_chars": max_chars,
            "overlap_chars": overlap_chars,
        }
        snapshot_id = hashlib.sha256(json.dumps(snapshot_identity, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
        manifest = {
            "schema_version": 1,
            "snapshot_id": snapshot_id,
            "snapshot_dir": snapshot_id,
            **snapshot_identity,
            "source_count": len(source_names),
            "document_count": document_count,
            "chunk_count": chunk_count,
            "source_documents_bytes": source_size,
            "chunks_bytes": chunks_size,
            "built_at": datetime.now(timezone.utc).isoformat(),
            "export_complete": True,
            "source_coverage_state": coverage_state,
            "source_coverage_report": coverage_report,
            "limitations": [
                "export_complete means snapshot files were committed; it does not mean source coverage is DONE.",
                "No vector or lexical indexes are built by this export step.",
            ],
        }
        manifest_tmp = staging_dir / "manifest.json.tmp"
        with manifest_tmp.open("wb") as stream:
            os.chmod(manifest_tmp, 0o600)
            stream.write(_json_line(manifest))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(manifest_tmp, staging_dir / "manifest.json")
        final_dir = selected_dir / snapshot_id
        if final_dir.exists():
            existing = json.loads((final_dir / "manifest.json").read_text("utf-8"))
            if existing.get("snapshot_id") != snapshot_id:
                raise FileExistsError("Snapshot directory exists with a different manifest")
            shutil.rmtree(staging_dir)
        else:
            os.replace(staging_dir, final_dir)
        return manifest
    except Exception:
        shutil.rmtree(staging_dir, ignore_errors=True)
        raise


def export_redamon_snapshot(
    snapshot: object,
    output_dir: Path,
    *,
    max_chars: int = 6000,
    overlap_chars: int = 400,
) -> dict[str, object]:
    """Export a canonical RedAmon snapshot with its measured source coverage."""
    from .redamon_sources import RedAmonSnapshot

    if not isinstance(snapshot, RedAmonSnapshot):
        raise TypeError("snapshot must be a RedAmonSnapshot")
    return export_snapshot(
        snapshot.documents,
        output_dir,
        source_manifest_sha256=snapshot.source_manifest_sha256,
        source_coverage_report=snapshot.coverage_report(),
        max_chars=max_chars,
        overlap_chars=overlap_chars,
    )


def _is_sha256(value: object) -> bool:
    return (
        isinstance(value, str)
        and len(value) == 64
        and all(char in "0123456789abcdef" for char in value)
    )


def _manifest_integer(manifest: Mapping[str, object], key: str, minimum: int = 0) -> int:
    value = manifest.get(key)
    if not isinstance(value, int) or isinstance(value, bool) or value < minimum:
        raise ValueError(f"snapshot manifest has invalid {key}")
    return value


def _strict_json_value(value: str) -> None:
    raise ValueError(f"snapshot contains non-standard JSON constant: {value}")


def _read_jsonl(path: Path, expected_sha256: object, expected_rows: int) -> list[dict[str, object]]:
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"snapshot JSONL file is missing or not a regular file: {path.name}")
    payload = path.read_bytes()
    if not _is_sha256(expected_sha256) or hashlib.sha256(payload).hexdigest() != expected_sha256:
        raise ValueError(f"snapshot JSONL hash mismatch: {path.name}")
    if payload and not payload.endswith(b"\n"):
        raise ValueError(f"snapshot JSONL file has an unterminated final row: {path.name}")
    raw_rows = payload[:-1].split(b"\n") if payload else []
    if len(raw_rows) != expected_rows:
        raise ValueError(f"snapshot JSONL row count mismatch: {path.name}")

    rows: list[dict[str, object]] = []
    for line_number, raw_row in enumerate(raw_rows, start=1):
        if not raw_row:
            raise ValueError(f"snapshot JSONL contains a blank row: {path.name}:{line_number}")
        try:
            value = json.loads(raw_row.decode("utf-8"), parse_constant=_strict_json_value)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ValueError(f"snapshot JSONL row is invalid: {path.name}:{line_number}") from exc
        if not isinstance(value, dict):
            raise ValueError(f"snapshot JSONL row must be an object: {path.name}:{line_number}")
        rows.append(value)
    return rows


def _validate_metadata(value: object, field_name: str) -> dict[str, str | int | float | bool | None]:
    if not isinstance(value, dict):
        raise ValueError(f"snapshot record has invalid {field_name}")
    for key, item in value.items():
        if not isinstance(key, str) or item is not None and type(item) not in (str, int, float, bool):
            raise ValueError(f"snapshot record has invalid {field_name}")
    return value


def _source_document_from_record(value: dict[str, object]) -> SourceDocument:
    expected_keys = {item.name for item in fields(SourceDocument)}
    if set(value) != expected_keys:
        raise ValueError("snapshot source document has an unexpected schema")
    string_fields = ("source_name", "source_id", "title", "text", "content_sha256")
    if any(not isinstance(value.get(key), str) for key in string_fields):
        raise ValueError("snapshot source document has invalid string fields")
    if not value["source_name"] or not value["source_id"] or not _is_sha256(value["content_sha256"]):
        raise ValueError("snapshot source document has invalid identity or content hash")
    optional_fields = ("source_url", "source_locator", "source_revision", "license_id", "attribution")
    if any(value.get(key) is not None and not isinstance(value.get(key), str) for key in optional_fields):
        raise ValueError("snapshot source document has invalid optional fields")
    metadata = _validate_metadata(value.get("metadata"), "metadata")
    document = SourceDocument(**{**value, "metadata": metadata})  # type: ignore[arg-type]
    if hashlib.sha256(document.text.encode("utf-8")).hexdigest() != document.content_sha256:
        raise ValueError(f"snapshot source document content hash mismatch: {document.source_name}/{document.source_id}")
    return document


def _validate_coverage_report(
    manifest: Mapping[str, object],
) -> tuple[str, str | None, Mapping[str, object] | None]:
    state = manifest.get("source_coverage_state")
    report_sha256 = manifest.get("source_coverage_report_sha256")
    report = manifest.get("source_coverage_report")
    valid_states = {"DONE", "PARTIAL", "BLOCKED", "unverified"}
    if not isinstance(state, str) or state not in valid_states:
        raise ValueError("snapshot manifest has invalid source_coverage_state")
    if report is None:
        if state != "unverified" or report_sha256 is not None:
            raise ValueError("snapshot coverage state requires a matching coverage report")
        return state, None, None
    if not isinstance(report, dict):
        raise ValueError("snapshot source coverage report must be an object")
    report_state = report.get("coverage_state")
    sources = report.get("sources")
    if not isinstance(report_state, str) or report_state not in {"DONE", "PARTIAL", "BLOCKED"}:
        raise ValueError("snapshot source coverage report has invalid coverage_state")
    if state != report_state or not isinstance(sources, list):
        raise ValueError("snapshot coverage state does not match its report")
    if not _is_sha256(report_sha256) or hashlib.sha256(_json_line(report)).hexdigest() != report_sha256:
        raise ValueError("snapshot source coverage report hash mismatch")
    if report_state == "DONE":
        if any(not isinstance(source, dict) or source.get("state") != "done" for source in sources):
            raise ValueError("snapshot report claims DONE while a source is not done")
        for key in ("blocked_source_count", "partial_source_count"):
            count = report.get(key, 0)
            if not isinstance(count, int) or isinstance(count, bool) or count != 0:
                raise ValueError("snapshot report claims DONE with blocked or partial sources")
        source_count = report.get("source_count", len(sources))
        if not isinstance(source_count, int) or isinstance(source_count, bool) or source_count != len(sources):
            raise ValueError("snapshot report claims DONE with an inconsistent source count")
    return state, report_sha256, report


def import_snapshot(snapshot_dir: Path, index: object) -> dict[str, object]:
    """Validate and atomically import a previously exported lexical KB snapshot.

    The importer performs no fetching. It accepts the exact version directory emitted by
    :func:`export_snapshot`, verifies its manifest, file digests, row counts, canonical
    source hashes, deterministic chunk identities and spans, then delegates replacement
    to ``KBIndex.replace_snapshot``. A coverage state other than ``unverified`` is only
    accepted when the snapshot contains a matching coverage report.
    """
    from .index import KBIndex

    if not isinstance(index, KBIndex):
        raise TypeError("index must be a KBIndex")
    root = Path(snapshot_dir).expanduser()
    if root.is_symlink() or not root.is_dir():
        raise ValueError("snapshot path must be a regular version directory")
    root = root.resolve(strict=True)
    manifest_path = root / "manifest.json"
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise ValueError("snapshot manifest is missing or not a regular file")
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("snapshot manifest is invalid JSON") from exc
    if not isinstance(manifest, dict):
        raise ValueError("snapshot manifest must be an object")
    schema_version = manifest.get("schema_version")
    if not isinstance(schema_version, int) or isinstance(schema_version, bool) or schema_version != 1:
        raise ValueError("unsupported snapshot manifest schema")
    snapshot_id = manifest.get("snapshot_id")
    if not _is_sha256(snapshot_id) or manifest.get("snapshot_dir") != snapshot_id or root.name != snapshot_id:
        raise ValueError("snapshot manifest identity does not match its version directory")
    if manifest.get("export_complete") is not True:
        raise ValueError("snapshot export is not marked complete")
    source_manifest_sha256 = manifest.get("source_manifest_sha256")
    source_documents_sha256 = manifest.get("source_documents_sha256")
    chunks_sha256 = manifest.get("chunks_sha256")
    if not all(_is_sha256(value) for value in (source_manifest_sha256, source_documents_sha256, chunks_sha256)):
        raise ValueError("snapshot manifest has an invalid SHA-256 field")
    coverage_state, coverage_report_sha256, coverage_report = _validate_coverage_report(manifest)
    if manifest.get("chunk_schema") != CHUNK_SCHEMA:
        raise ValueError("snapshot chunk schema is unsupported")
    max_chars = _manifest_integer(manifest, "max_chars", minimum=256)
    overlap_chars = _manifest_integer(manifest, "overlap_chars")
    if overlap_chars >= max_chars:
        raise ValueError("snapshot manifest has invalid chunk overlap")
    document_count = _manifest_integer(manifest, "document_count")
    chunk_count = _manifest_integer(manifest, "chunk_count")
    source_count = _manifest_integer(manifest, "source_count", minimum=1)
    source_bytes = _manifest_integer(manifest, "source_documents_bytes")
    chunk_bytes = _manifest_integer(manifest, "chunks_bytes")
    source_path = root / "source-documents.jsonl"
    chunks_path = root / "chunks.jsonl"
    document_records = _read_jsonl(source_path, source_documents_sha256, document_count)
    chunk_records = _read_jsonl(chunks_path, chunks_sha256, chunk_count)
    if source_path.stat().st_size != source_bytes or chunks_path.stat().st_size != chunk_bytes:
        raise ValueError("snapshot manifest byte count mismatch")
    if not document_records:
        raise ValueError("snapshot contains no source documents")

    source_documents = [_source_document_from_record(record) for record in document_records]
    source_names = {document.source_name for document in source_documents}
    if len(source_names) != source_count:
        raise ValueError("snapshot manifest source count mismatch")
    document_keys = [(document.source_name, document.source_id) for document in source_documents]
    if len(set(document_keys)) != len(document_keys):
        raise ValueError("snapshot contains duplicate source document identities")

    expected_chunks = [
        chunk
        for document in source_documents
        for chunk in chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars)
    ]
    if len(expected_chunks) != chunk_count:
        raise ValueError("snapshot chunk count does not match its source documents")
    expected_chunk_keys = {item.name for item in fields(CanonicalChunk)}
    for ordinal, (record, expected) in enumerate(zip(chunk_records, expected_chunks)):
        integer_fields_valid = all(
            isinstance(record.get(key), int) and not isinstance(record.get(key), bool)
            for key in ("ordinal", "start_char", "end_char")
        )
        if set(record) != expected_chunk_keys or not integer_fields_valid or record != asdict(expected):
            raise ValueError(f"snapshot chunk hash, identity, provenance, or span mismatch at row {ordinal}")

    identity_payload = {
        "source_manifest_sha256": source_manifest_sha256,
        "source_documents_sha256": source_documents_sha256,
        "chunks_sha256": chunks_sha256,
        "source_coverage_report_sha256": coverage_report_sha256,
        "chunk_schema": manifest["chunk_schema"],
        "max_chars": max_chars,
        "overlap_chars": overlap_chars,
    }
    expected_snapshot_id = hashlib.sha256(
        json.dumps(identity_payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()
    if expected_snapshot_id != snapshot_id:
        raise ValueError("snapshot manifest id does not match its content digests")

    imported_count = index.replace_snapshot(
        expected_chunks,
        snapshot_id,
        sources=source_documents,
        source_coverage_state=coverage_state,
        source_coverage_report_sha256=coverage_report_sha256,
        source_coverage_report=coverage_report,
    )
    if imported_count != chunk_count:
        raise RuntimeError("KB index imported an unexpected chunk count")
    return index.stats()
