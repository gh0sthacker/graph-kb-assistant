"""Portable, provenance-rich source snapshot and chunk export.

Feed-specific fetching and normalization belong in source adapters. This module
only serializes already-fetched source documents and deterministic chunks.
"""
from __future__ import annotations

from collections import Counter
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
from ..project_lock import project_maintenance_lock


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
    max_chars: int = 1920,
    overlap_chars: int = 0,
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
    documents_by_source: Counter[str] = Counter()
    chunks_by_source: Counter[str] = Counter()
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
                    documents_by_source[document.source_name] += 1
                    for chunk in chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars):
                        chunk_line = _json_line(asdict(chunk))
                        chunk_stream.write(chunk_line)
                        chunks_digest.update(chunk_line)
                        chunks_size += len(chunk_line)
                        chunk_count += 1
                        chunks_by_source[document.source_name] += 1
        if document_count == 0:
            raise ValueError("Cannot create an empty KB snapshot")
        _validate_coverage_source_counts(coverage_report, documents_by_source, chunks_by_source)
        source_digest_hex = source_digest.hexdigest()
        chunks_digest_hex = chunks_digest.hexdigest()
        snapshot_identity = {
            "source_manifest_sha256": source_manifest_sha256,
            "source_documents_sha256": source_digest_hex,
            "chunks_sha256": chunks_digest_hex,
            "source_coverage_report_sha256": coverage_report_sha256,
            "chunk_schema": CHUNK_SCHEMA,
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
    max_chars: int = 1920,
    overlap_chars: int = 0,
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


def _refresh_pinned_local_sources(index: object, *, project_root: Path, manifest: Mapping[str, object], manifest_sha256: str) -> None:
    """Replace local-source rows from their pinned, hash-verified source bundle."""
    from .index import KBIndex
    from .redamon_local import RedAmonLocalToolDocsProvider
    from .redamon_sources import RedAmonSourceAdapter

    if not isinstance(index, KBIndex):
        raise TypeError("index must be a KBIndex")
    specs = manifest.get("sources")
    if not isinstance(specs, list):
        raise ValueError("KB source manifest is malformed")
    local_families = {
        spec["id"] for spec in specs
        if isinstance(spec, dict) and spec.get("kind") == "local_markdown" and isinstance(spec.get("id"), str)
    }
    if not local_families:
        raise ValueError("KB source manifest has no pinned local Markdown sources")

    snapshot = RedAmonSourceAdapter(
        manifest,
        {family: RedAmonLocalToolDocsProvider(project_root=project_root) for family in local_families},
        source_manifest_sha256=manifest_sha256,
    ).build()
    documents_by_family: dict[str, list[SourceDocument]] = {family: [] for family in local_families}
    for document in snapshot.documents:
        if document.source_name in documents_by_family:
            documents_by_family[document.source_name].append(document)
    fresh_coverage = {
        str(row["source_name"]): dict(row)
        for row in snapshot.source_coverage
        if isinstance(row, Mapping) and isinstance(row.get("source_name"), str)
    }
    if snapshot.coverage_state != "DONE" and not all(
        fresh_coverage.get(family, {}).get("state") == "done" and documents_by_family[family]
        for family in local_families
    ):
        raise ValueError("pinned local sources did not pass their completeness checks")
    if any(fresh_coverage.get(family, {}).get("state") != "done" or not documents_by_family[family]
           for family in local_families):
        raise ValueError("pinned local source is incomplete")

    with project_maintenance_lock(index.path.parent):
        with index.connect(_maintenance_lock_held=True) as connection:
            meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
            coverage_rows = json.loads(meta.get("source_coverage", "[]"))
            if not isinstance(coverage_rows, list) or any(not isinstance(row, dict) for row in coverage_rows):
                raise ValueError("stored source coverage rows are malformed")
            coverage_by_family = {
                row.get("source_name"): row for row in coverage_rows
                if isinstance(row.get("source_name"), str)
            }
            if meta.get("coverage_state") == "DONE" and not local_families.issubset(coverage_by_family):
                raise ValueError("DONE source coverage is missing a local source family")

            insert_sql = """INSERT INTO source_documents(
                source_name,source_id,title,source_url,source_locator,source_revision,license_id,attribution,
                metadata_json,content_sha256,text,raw_source_sha256,raw_source_text
            ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)"""
            for family in sorted(local_families):
                connection.execute("DELETE FROM source_documents WHERE source_name=?", (family,))
                for document in documents_by_family[family]:
                    connection.execute(insert_sql, (
                        document.source_name, document.source_id, document.title, document.source_url,
                        document.source_locator, document.source_revision, document.license_id,
                        document.attribution,
                        json.dumps(document.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False),
                        document.content_sha256, document.text, document.raw_source_sha256, document.raw_source_text,
                    ))
                coverage_by_family[family] = fresh_coverage[family]
            source_order = {
                spec["id"]: position for position, spec in enumerate(specs)
                if isinstance(spec, dict) and isinstance(spec.get("id"), str)
            }
            coverage_rows = sorted(coverage_by_family.values(), key=lambda row: source_order.get(row["source_name"], 10**9))
            connection.execute(
                "INSERT INTO kb_meta(key,value) VALUES('source_coverage',?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (json.dumps(coverage_rows, sort_keys=True, separators=(",", ":"), ensure_ascii=False),),
            )
            connection.commit()


def rechunk_index(
    index: object,
    *,
    project_root: Path,
    snapshot_root: Path | None = None,
    refresh_local_sources: bool = False,
    max_chars: int = 1920,
    overlap_chars: int = 0,
    batch_documents: int = 1000,
    progress_callback: object | None = None,
) -> dict[str, object]:
    """Rebuild chunks from stored sources in durable, restartable batches.

    The maintenance marker remains present until all batches and final pruning
    commit, so application readers fail closed if this process is interrupted.
    """
    if max_chars < 256 or overlap_chars != 0:
        raise ValueError("RedAmon-compatible chunks require max_chars >= 256 and zero overlap")
    if not 1 <= batch_documents <= 10_000:
        raise ValueError("batch_documents must be between 1 and 10000")
    try:
        import yaml
    except ImportError as exc:
        raise RuntimeError("Install the optional KB dependency with `pip install -e '.[kb]'`.") from exc

    root = project_root.expanduser().resolve()
    manifest_path = root / "src" / "bb_assistant" / "kb" / "sources.yaml"
    raw_manifest = manifest_path.read_bytes()
    manifest = yaml.safe_load(raw_manifest)
    if not isinstance(manifest, dict) or not isinstance(manifest.get("sources"), list):
        raise ValueError("KB source manifest is malformed")
    family_order = {
        row["id"]: position
        for position, row in enumerate(manifest["sources"])
        if isinstance(row, dict) and isinstance(row.get("id"), str)
    }
    if refresh_local_sources:
        _refresh_pinned_local_sources(
            index, project_root=root, manifest=manifest,
            manifest_sha256=hashlib.sha256(raw_manifest).hexdigest(),
        )

    chunk_columns = (
        "chunk_id,source_name,source_id,title,start_char,end_char,source_locator,source_url,"
        "source_revision,license_id,attribution,metadata_json,source_sha256,chunk_sha256,chunk_schema,text"
    )
    upsert_chunk_sql = f"""INSERT INTO chunks({chunk_columns}) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(chunk_id) DO UPDATE SET
          source_name=excluded.source_name,source_id=excluded.source_id,title=excluded.title,
          start_char=excluded.start_char,end_char=excluded.end_char,source_locator=excluded.source_locator,
          source_url=excluded.source_url,source_revision=excluded.source_revision,
          license_id=excluded.license_id,attribution=excluded.attribution,metadata_json=excluded.metadata_json,
          source_sha256=excluded.source_sha256,chunk_sha256=excluded.chunk_sha256,
          chunk_schema=excluded.chunk_schema,text=excluded.text
        WHERE chunks.source_name IS NOT excluded.source_name OR chunks.source_id IS NOT excluded.source_id
          OR chunks.title IS NOT excluded.title OR chunks.start_char IS NOT excluded.start_char
          OR chunks.end_char IS NOT excluded.end_char OR chunks.source_locator IS NOT excluded.source_locator
          OR chunks.source_url IS NOT excluded.source_url OR chunks.source_revision IS NOT excluded.source_revision
          OR chunks.license_id IS NOT excluded.license_id OR chunks.attribution IS NOT excluded.attribution
          OR chunks.metadata_json IS NOT excluded.metadata_json OR chunks.source_sha256 IS NOT excluded.source_sha256
          OR chunks.chunk_sha256 IS NOT excluded.chunk_sha256 OR chunks.chunk_schema IS NOT excluded.chunk_schema
          OR chunks.text IS NOT excluded.text"""

    with project_maintenance_lock(index.path.parent):
        with index.connect(_maintenance_lock_held=True) as connection:
            meta = dict(connection.execute("SELECT key,value FROM kb_meta").fetchall())
            old_snapshot_id = meta.get("snapshot_id")
            if not old_snapshot_id:
                raise ValueError("Cannot re-chunk an index without an installed snapshot")
            source_manifest_sha256 = meta.get("source_manifest_sha256")
            if not _is_sha256(source_manifest_sha256):
                manifest_root = (
                    snapshot_root.expanduser().resolve()
                    if snapshot_root is not None else index.path.parent / "snapshots"
                )
                try:
                    old_manifest = json.loads(
                        (manifest_root / old_snapshot_id / "manifest.json").read_text("utf-8")
                    )
                except (OSError, UnicodeError, json.JSONDecodeError):
                    old_manifest = None
                candidate = old_manifest.get("source_manifest_sha256") if isinstance(old_manifest, dict) else None
                if _is_sha256(candidate):
                    source_manifest_sha256 = candidate
            if any(name not in family_order for name in {
                row[0] for row in connection.execute("SELECT DISTINCT source_name FROM source_documents")
            }):
                raise ValueError("Installed KB contains a family absent from the current source manifest")

            coverage_rows = json.loads(meta.get("source_coverage", "[]"))
            coverage_state = meta.get("coverage_state", "unverified")
            if not isinstance(coverage_rows, list) or any(
                not isinstance(row, dict) or not isinstance(row.get("source_name"), str)
                for row in coverage_rows
            ):
                raise ValueError("source coverage rows are malformed")
            coverage_names = {row["source_name"] for row in coverage_rows}
            document_names = {
                row[0] for row in connection.execute("SELECT DISTINCT source_name FROM source_documents")
            }
            if coverage_state == "DONE" and (
                coverage_names != document_names or document_names != set(family_order)
            ):
                raise ValueError("DONE source coverage families do not match stored source documents")
            coverage_report: dict[str, object] | None = None
            coverage_sha: str | None = None
            if coverage_state != "unverified":
                coverage_report = {
                    "coverage_state": coverage_state,
                    "source_count": len(coverage_rows),
                    "blocked_source_count": sum(row.get("state") == "blocked" for row in coverage_rows),
                    "partial_source_count": sum(row.get("state") == "partial" for row in coverage_rows),
                    "sources": coverage_rows,
                }
                coverage_sha = hashlib.sha256(_json_line(coverage_report)).hexdigest()

            target_snapshot_id = meta.get("rechunk_target_snapshot_id")
            state_manifest = meta.get("rechunk_manifest_sha256")
            manifest_sha = hashlib.sha256(raw_manifest).hexdigest()
            reset_progress = not target_snapshot_id or state_manifest != manifest_sha
            if reset_progress:
                target_snapshot_id = hashlib.sha256(json.dumps({
                    "rechunk_of": old_snapshot_id, "source_manifest_sha256": manifest_sha,
                    "chunk_schema": CHUNK_SCHEMA, "max_chars": max_chars, "overlap_chars": overlap_chars,
                }, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
                connection.execute("DROP TABLE IF EXISTS kb_rechunk_ids")
                connection.execute("DROP TABLE IF EXISTS kb_rechunk_fts_ids")
                connection.execute("CREATE TABLE kb_rechunk_ids(chunk_id TEXT PRIMARY KEY)")
                connection.execute("CREATE TABLE kb_rechunk_fts_ids(chunk_id TEXT PRIMARY KEY, fts_rowid INTEGER NOT NULL)")
                connection.execute("INSERT INTO kb_rechunk_fts_ids(chunk_id,fts_rowid) SELECT chunk_id,rowid FROM chunk_search")
                for key in ("rechunk_progress_family", "rechunk_progress_source_id"):
                    connection.execute("DELETE FROM kb_meta WHERE key=?", (key,))
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('rechunk_target_snapshot_id',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (target_snapshot_id,),
                )
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('rechunk_manifest_sha256',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (manifest_sha,),
                )
                connection.commit()
            elif not connection.execute(
                "SELECT 1 FROM sqlite_master WHERE type='table' AND name='kb_rechunk_fts_ids'"
            ).fetchone():
                connection.execute("CREATE TABLE kb_rechunk_fts_ids(chunk_id TEXT PRIMARY KEY, fts_rowid INTEGER NOT NULL)")
                connection.execute("INSERT INTO kb_rechunk_fts_ids(chunk_id,fts_rowid) SELECT chunk_id,rowid FROM chunk_search")
                connection.commit()

            family_idx = 0 if reset_progress else int(meta.get("rechunk_progress_family", "0"))
            last_id = "" if reset_progress else meta.get("rechunk_progress_source_id", "")
            processed = 0
            total_chunks = 0
            total_documents = connection.execute("SELECT count(*) FROM source_documents").fetchone()[0]
            while family_idx < len(family_order):
                family = next(name for name, order in family_order.items() if order == family_idx)
                rows = connection.execute(
                    "SELECT * FROM source_documents WHERE source_name=? AND source_id>? "
                    "ORDER BY source_id LIMIT ?", (family, last_id, batch_documents),
                ).fetchall()
                if not rows:
                    family_idx += 1
                    last_id = ""
                    connection.execute(
                        "INSERT INTO kb_meta(key,value) VALUES('rechunk_progress_family',?) "
                        "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (str(family_idx),),
                    )
                    connection.execute("DELETE FROM kb_meta WHERE key='rechunk_progress_source_id'")
                    connection.commit()
                    continue
                chunks: list[CanonicalChunk] = []
                for row in rows:
                    record = dict(row)
                    record["metadata"] = json.loads(record.pop("metadata_json"))
                    document = _source_document_from_record(record)
                    chunks.extend(chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars))
                for chunk in chunks:
                    metadata_json = json.dumps(chunk.metadata, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
                    old_fts = connection.execute(
                        "SELECT fts_rowid FROM kb_rechunk_fts_ids WHERE chunk_id=?", (chunk.chunk_id,),
                    ).fetchone()
                    if old_fts:
                        old_text = connection.execute(
                            "SELECT title,text FROM chunk_search WHERE rowid=?", (old_fts[0],),
                        ).fetchone()
                        if old_text and (old_text[0] != chunk.title or old_text[1] != chunk.text):
                            connection.execute("DELETE FROM chunk_search WHERE rowid=?", (old_fts[0],))
                            connection.execute("DELETE FROM kb_rechunk_fts_ids WHERE chunk_id=?", (chunk.chunk_id,))
                            old_fts = None
                    values = (
                        chunk.chunk_id, chunk.source_name, chunk.source_id, chunk.title,
                        chunk.start_char, chunk.end_char, chunk.source_locator, chunk.source_url,
                        chunk.source_revision, chunk.license_id, chunk.attribution, metadata_json,
                        chunk.source_sha256, chunk.chunk_sha256, chunk.chunk_schema, chunk.text,
                    )
                    connection.execute(upsert_chunk_sql, values)
                    connection.execute("INSERT OR IGNORE INTO kb_rechunk_ids(chunk_id) VALUES(?)", (chunk.chunk_id,))
                    if not old_fts:
                        cursor = connection.execute(
                            "INSERT INTO chunk_search(chunk_id,source_name,title,text) VALUES(?,?,?,?)",
                            (chunk.chunk_id, chunk.source_name, chunk.title, chunk.text),
                        )
                        connection.execute(
                            "INSERT OR REPLACE INTO kb_rechunk_fts_ids(chunk_id,fts_rowid) VALUES(?,?)",
                            (chunk.chunk_id, cursor.lastrowid),
                        )
                last_id = rows[-1]["source_id"]
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('rechunk_progress_family',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (str(family_idx),),
                )
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES('rechunk_progress_source_id',?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (last_id,),
                )
                connection.commit()
                processed += len(rows)
                total_chunks += len(chunks)
                if progress_callback is not None:
                    progress_callback(processed, total_documents)

            # The staging ID table is the durable source of truth for pruning;
            # each delete is idempotent and is safe to repeat after interruption.
            connection.execute("DELETE FROM chunk_search WHERE rowid IN (SELECT f.fts_rowid FROM kb_rechunk_fts_ids f WHERE NOT EXISTS (SELECT 1 FROM kb_rechunk_ids i WHERE i.chunk_id=f.chunk_id))")
            connection.commit()
            connection.execute("DELETE FROM chunks WHERE NOT EXISTS (SELECT 1 FROM kb_rechunk_ids i WHERE i.chunk_id=chunks.chunk_id)")
            connection.commit()
            chunk_count = connection.execute("SELECT count(*) FROM chunks").fetchone()[0]
            documents_by_source = dict(connection.execute(
                "SELECT source_name,count(*) FROM source_documents GROUP BY source_name"
            ).fetchall())
            chunks_by_source = dict(connection.execute(
                "SELECT source_name,count(*) FROM chunks GROUP BY source_name"
            ).fetchall())
            for row in coverage_rows:
                name = row["source_name"]
                row["document_count"] = int(documents_by_source.get(name, 0))
                row["chunk_count"] = int(chunks_by_source.get(name, 0))
            if coverage_report is not None:
                coverage_report["sources"] = coverage_rows
                coverage_sha = hashlib.sha256(_json_line(coverage_report)).hexdigest()
            connection.execute("DELETE FROM kb_meta WHERE key IN ('vector_model','vector_dimensions','vector_snapshot_id')")
            for key, value in (
                ("snapshot_id", target_snapshot_id), ("chunk_count", str(chunk_count)),
                ("source_manifest_sha256", source_manifest_sha256 or ""),
                ("vector_staging_snapshot_id", target_snapshot_id), ("coverage_state", coverage_state),
                ("coverage_report_sha256", coverage_sha or ""),
                ("source_coverage", json.dumps(coverage_rows, sort_keys=True, separators=(",", ":"), ensure_ascii=False)),
            ):
                connection.execute(
                    "INSERT INTO kb_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    (key, value),
                )
            connection.execute("DROP TABLE kb_rechunk_ids")
            connection.execute("DROP TABLE kb_rechunk_fts_ids")
            connection.execute("DELETE FROM kb_meta WHERE key LIKE 'rechunk_%'")
            connection.commit()
            if progress_callback is not None:
                progress_callback(total_documents, total_documents)

    return {
        "previous_snapshot_id": old_snapshot_id,
        "snapshot_id": target_snapshot_id,
        "source_documents": total_documents,
        "chunks": chunk_count,
        "coverage_state": coverage_state,
    }


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


def _iter_jsonl(path: Path, expected_sha256: object, expected_rows: int) -> Iterator[dict[str, object]]:
    """Validate and yield JSONL rows without materializing the source file."""
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"snapshot JSONL file is missing or not a regular file: {path.name}")
    if not _is_sha256(expected_sha256):
        raise ValueError(f"snapshot JSONL hash is invalid: {path.name}")
    digest = hashlib.sha256()
    row_count = 0
    with path.open("rb") as stream:
        for line_number, raw_row in enumerate(stream, start=1):
            digest.update(raw_row)
            if not raw_row.endswith(b"\n"):
                raise ValueError(f"snapshot JSONL file has an unterminated final row: {path.name}")
            payload = raw_row[:-1]
            if not payload:
                raise ValueError(f"snapshot JSONL contains a blank row: {path.name}:{line_number}")
            try:
                value = json.loads(payload.decode("utf-8"), parse_constant=_strict_json_value)
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise ValueError(f"snapshot JSONL row is invalid: {path.name}:{line_number}") from exc
            if not isinstance(value, dict):
                raise ValueError(f"snapshot JSONL row must be an object: {path.name}:{line_number}")
            row_count += 1
            yield value
    if row_count != expected_rows:
        raise ValueError(f"snapshot JSONL row count mismatch: {path.name}")
    if digest.hexdigest() != expected_sha256:
        raise ValueError(f"snapshot JSONL hash mismatch: {path.name}")


def _read_jsonl(path: Path, expected_sha256: object, expected_rows: int) -> list[dict[str, object]]:
    """Compatibility helper for small callers; large imports use _iter_jsonl."""
    return list(_iter_jsonl(path, expected_sha256, expected_rows))


def _validate_metadata(value: object, field_name: str) -> dict[str, str | int | float | bool | None]:
    if not isinstance(value, dict):
        raise ValueError(f"snapshot record has invalid {field_name}")
    for key, item in value.items():
        if not isinstance(key, str) or item is not None and type(item) not in (str, int, float, bool):
            raise ValueError(f"snapshot record has invalid {field_name}")
    return value


def _source_document_from_record(value: dict[str, object]) -> SourceDocument:
    expected_keys = {item.name for item in fields(SourceDocument)}
    # Older snapshots predate the separate raw-upstream payload. They remain
    # importable with the optional provenance columns defaulting to null.
    legacy_keys = expected_keys - {"raw_source_text", "raw_source_sha256"}
    if not legacy_keys.issubset(value) or not set(value).issubset(expected_keys):
        raise ValueError("snapshot source document has an unexpected schema")
    string_fields = ("source_name", "source_id", "title", "text", "content_sha256")
    if any(not isinstance(value.get(key), str) for key in string_fields):
        raise ValueError("snapshot source document has invalid string fields")
    if not value["source_name"] or not value["source_id"] or not _is_sha256(value["content_sha256"]):
        raise ValueError("snapshot source document has invalid identity or content hash")
    optional_fields = ("source_url", "source_locator", "source_revision", "license_id", "attribution")
    if any(value.get(key) is not None and not isinstance(value.get(key), str) for key in optional_fields):
        raise ValueError("snapshot source document has invalid optional fields")
    if value.get("raw_source_text") is not None and not isinstance(value.get("raw_source_text"), str):
        raise ValueError("snapshot source document has invalid raw source text")
    if value.get("raw_source_sha256") is not None and not _is_sha256(value.get("raw_source_sha256")):
        raise ValueError("snapshot source document has invalid raw source hash")
    metadata = _validate_metadata(value.get("metadata"), "metadata")
    document = SourceDocument(**{
        "raw_source_text": None,
        "raw_source_sha256": None,
        **value,
        "metadata": metadata,
    })  # type: ignore[arg-type]
    if hashlib.sha256(document.text.encode("utf-8")).hexdigest() != document.content_sha256:
        raise ValueError(f"snapshot source document content hash mismatch: {document.source_name}/{document.source_id}")
    if document.raw_source_text is None:
        if document.raw_source_sha256 is not None:
            raise ValueError("snapshot source document has a raw hash without raw source text")
    elif hashlib.sha256(document.raw_source_text.encode("utf-8")).hexdigest() != document.raw_source_sha256:
        raise ValueError(f"snapshot raw source hash mismatch: {document.source_name}/{document.source_id}")
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


def _validate_coverage_source_counts(
    report: Mapping[str, object] | None,
    documents_by_source: Mapping[str, int],
    chunks_by_source: Mapping[str, int],
) -> None:
    """Reject a DONE report whose source counts do not describe its files."""
    if report is None or report.get("coverage_state") != "DONE":
        return
    sources = report.get("sources")
    if not isinstance(sources, list):
        raise ValueError("DONE source coverage must include per-source counts")
    names: set[str] = set()
    for row in sources:
        if not isinstance(row, dict):
            raise ValueError("DONE source coverage contains a malformed source row")
        name = row.get("source_name")
        if not isinstance(name, str) or not name or name in names:
            raise ValueError("DONE source coverage contains a missing or duplicate source name")
        names.add(name)
        for field, actual in (
            ("document_count", documents_by_source.get(name, 0)),
            ("chunk_count", chunks_by_source.get(name, 0)),
        ):
            value = row.get(field)
            if not isinstance(value, int) or isinstance(value, bool) or value < 0 or value != actual:
                raise ValueError(f"DONE source coverage {field} does not match snapshot for {name}")
    if names != set(documents_by_source) or names != set(chunks_by_source):
        raise ValueError("DONE source coverage families do not match snapshot records")


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
    if overlap_chars != 0:
        raise ValueError("snapshot manifest chunk overlap must be zero for RedAmon-compatible chunks")
    document_count = _manifest_integer(manifest, "document_count")
    chunk_count = _manifest_integer(manifest, "chunk_count")
    source_count = _manifest_integer(manifest, "source_count", minimum=1)
    source_bytes = _manifest_integer(manifest, "source_documents_bytes")
    chunk_bytes = _manifest_integer(manifest, "chunks_bytes")
    source_path = root / "source-documents.jsonl"
    chunks_path = root / "chunks.jsonl"
    if source_path.stat().st_size != source_bytes or chunks_path.stat().st_size != chunk_bytes:
        raise ValueError("snapshot manifest byte count mismatch")
    source_documents = [
        _source_document_from_record(record)
        for record in _iter_jsonl(source_path, source_documents_sha256, document_count)
    ]
    if not source_documents:
        raise ValueError("snapshot contains no source documents")
    source_names = {document.source_name for document in source_documents}
    if len(source_names) != source_count:
        raise ValueError("snapshot manifest source count mismatch")
    documents_by_source = Counter(document.source_name for document in source_documents)
    document_keys = [(document.source_name, document.source_id) for document in source_documents]
    if len(set(document_keys)) != len(document_keys):
        raise ValueError("snapshot contains duplicate source document identities")

    expected_chunks = (
        chunk
        for document in source_documents
        for chunk in chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars)
    )
    expected_chunk_keys = {item.name for item in fields(CanonicalChunk)}
    chunk_count_seen = 0
    chunks_by_source: Counter[str] = Counter()
    for ordinal, record in enumerate(_iter_jsonl(chunks_path, chunks_sha256, chunk_count)):
        try:
            expected = next(expected_chunks)
        except StopIteration as exc:
            raise ValueError("snapshot chunk count does not match its source documents") from exc
        integer_fields_valid = all(
            isinstance(record.get(key), int) and not isinstance(record.get(key), bool)
            for key in ("ordinal", "start_char", "end_char")
        )
        if set(record) != expected_chunk_keys or not integer_fields_valid or record != asdict(expected):
            raise ValueError(f"snapshot chunk hash, identity, provenance, or span mismatch at row {ordinal}")
        chunk_count_seen += 1
        chunks_by_source[expected.source_name] += 1
    try:
        next(expected_chunks)
    except StopIteration:
        pass
    else:
        raise ValueError("snapshot chunk count does not match its source documents")
    if chunk_count_seen != chunk_count:
        raise ValueError("snapshot chunk count does not match its source documents")
    _validate_coverage_source_counts(coverage_report, documents_by_source, chunks_by_source)

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

    replacement_chunks = (
        chunk
        for document in source_documents
        for chunk in chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars)
    )
    imported_count = index.replace_snapshot(
        replacement_chunks,
        snapshot_id,
        sources=source_documents,
        source_manifest_sha256=source_manifest_sha256,
        source_coverage_state=coverage_state,
        source_coverage_report_sha256=coverage_report_sha256,
        source_coverage_report=coverage_report,
    )
    if imported_count != chunk_count:
        raise RuntimeError("KB index imported an unexpected chunk count")
    return index.stats()
