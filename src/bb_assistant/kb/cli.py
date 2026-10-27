"""Commands for rebuilding and querying the local RedAmon-derived KB."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
from typing import Sequence

from .index import KBIndex
from .exploitdb import RedAmonExploitDBProvider
from .gtfobins import RedAmonGTFOBinsProvider
from .lolbas import RedAmonLOLBASProvider
from .nvd import RedAmonNVDProvider
from .nuclei import RedAmonNucleiProvider
from .owasp import RedAmonOWASPProvider
from .redamon_local import RedAmonLocalToolDocsProvider
from .redamon_sources import RedAmonSourceAdapter
from .rebuild import export_redamon_snapshot, import_snapshot, rechunk_index
from .parity import compare_receipts, export_assistant_receipt, load_query_fixture, load_receipt
from .ranking import DEFAULT_TOP_K


def build_kb(
    *,
    project_root: Path,
    data_home: Path | None = None,
    snapshot_root: Path | None = None,
    index_path: Path | None = None,
    profile: str | None = None,
) -> dict[str, object]:
    """Build the currently available source families and import the snapshot."""
    if data_home is not None:
        os.environ["BB_ASSISTANT_DATA_HOME"] = str(data_home.expanduser().resolve())
    try:
        import yaml
    except ImportError as exc:
        raise RuntimeError("Install the optional KB dependency with `pip install -e '.[kb]'`.") from exc

    root = project_root.resolve()
    manifest_path = root / "src" / "bb_assistant" / "kb" / "sources.yaml"
    raw_manifest = manifest_path.read_bytes()
    manifest = yaml.safe_load(raw_manifest)
    if not isinstance(manifest, dict):
        raise ValueError("KB source manifest must be a YAML object")
    if profile is not None:
        manifest = {**manifest, "profile": profile}

    providers = {
        "tool_docs": RedAmonLocalToolDocsProvider(project_root=root),
        "exploitdb": RedAmonExploitDBProvider(),
        "gtfobins": RedAmonGTFOBinsProvider(),
        "lolbas": RedAmonLOLBASProvider(),
        "nvd": RedAmonNVDProvider(),
        "nuclei": RedAmonNucleiProvider(),
        "owasp": RedAmonOWASPProvider(),
    }
    adapter = RedAmonSourceAdapter(
        manifest,
        providers,
        source_manifest_sha256=hashlib.sha256(raw_manifest).hexdigest(),
    )
    snapshot = adapter.build()
    if not snapshot.documents:
        raise RuntimeError("No eligible RedAmon source documents were produced")

    data_root = (data_home.expanduser().resolve() if data_home else _data_root())
    snapshots_root = snapshot_root.expanduser().resolve() if snapshot_root else data_root / "kb" / "snapshots"
    exported = export_redamon_snapshot(snapshot, snapshots_root)
    selected_index_path = _configured_index_path(index_path) or data_root / "kb" / "index.sqlite3"
    index = KBIndex(selected_index_path)
    indexed = import_snapshot(snapshots_root / str(exported["snapshot_id"]), index)
    return {
        "snapshot_id": exported["snapshot_id"],
        "snapshot_path": str(snapshots_root / str(exported["snapshot_id"])),
        "index_path": str(selected_index_path),
        "source_coverage_state": snapshot.coverage_state,
        "documents_built": len(snapshot.documents),
        "chunks_built": len(snapshot.chunks),
        "index": indexed,
    }


def _snapshot_profile(snapshot_dir: Path) -> str:
    manifest = json.loads((snapshot_dir.expanduser() / "manifest.json").read_text(encoding="utf-8"))
    report = manifest.get("source_coverage_report", {}) if isinstance(manifest, dict) else {}
    profile = report.get("profile") if isinstance(report, dict) else None
    if profile not in {"lite", "full"}:
        raise ValueError("snapshot does not record a supported source profile")
    return profile


def _data_root() -> Path:
    from ..paths import data_root

    return data_root()


def _configured_index_path(explicit_path: Path | None) -> Path | None:
    """Resolve the index override shared by the CLI and the MCP facade."""
    if explicit_path is not None:
        return explicit_path.expanduser().resolve()
    configured = os.environ.get("BB_ASSISTANT_KB_INDEX")
    return Path(configured).expanduser().resolve() if configured else None


def _embedding_candidate_index_path(explicit_path: Path | None) -> Path:
    """Require embedding to target an existing isolated candidate index.

    Embedding is a long-running, mutating operation. Requiring the canonical
    ``kb/candidates/<id>/index.sqlite3`` layout prevents a deployment unit that
    accidentally points at the live application index from filling it in
    place.
    """
    configured = explicit_path
    if configured is None:
        raw_configured = os.environ.get("BB_ASSISTANT_KB_INDEX")
        configured = Path(raw_configured) if raw_configured else None
    if configured is None:
        raise ValueError("embedding requires --index-path or BB_ASSISTANT_KB_INDEX for an isolated candidate")
    requested = configured.expanduser()
    if requested.is_symlink():
        raise ValueError("embedding candidate index must not be a symbolic link")
    try:
        resolved = requested.resolve(strict=True)
    except OSError as exc:
        raise ValueError("embedding candidate index must already exist") from exc
    if (
        not resolved.is_file()
        or resolved.name != "index.sqlite3"
        or resolved.parent.parent.name != "candidates"
        or resolved.parent.parent.parent.name != "kb"
    ):
        raise ValueError("embedding is restricted to kb/candidates/<id>/index.sqlite3")
    return resolved


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="bb-assistant-kb")
    parser.add_argument(
        "--project-root", type=Path,
        default=Path(__file__).resolve().parents[3],
        help="standalone graph-kb-assistant checkout",
    )
    parser.add_argument("--data-home", type=Path, help="private data root, outside the source checkout")
    parser.add_argument("--snapshot-root", type=Path, help="override the KB snapshot directory")
    parser.add_argument("--index-path", type=Path, help="override the SQLite FTS5 index path")
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build", help="build available pinned sources and import the KB index")
    build.add_argument("--profile", choices=("lite", "full"), help="source profile (defaults to the manifest)")
    commands.add_parser("stats", help="show KB source and coverage counts")
    publish_lucene = commands.add_parser("publish-lucene", help="publish a verified snapshot-isolated Neo4j lexical generation")
    publish_lucene.add_argument("--batch-size", type=int, default=200)
    rechunk = commands.add_parser("rechunk", help="rebuild installed chunks from stored source documents without fetching")
    rechunk.add_argument("--profile", choices=("lite", "full"), help="source profile (defaults to the manifest)")
    rechunk.add_argument(
        "--refresh-local-sources", action="store_true",
        help="refresh pinned local Markdown sources before rechunking (candidate indexes only)",
    )
    import_command = commands.add_parser(
        "import-snapshot",
        help="validate and import an existing source snapshot without fetching feeds",
    )
    import_command.add_argument("snapshot_dir", type=Path, help="version directory containing manifest.json")
    embed = commands.add_parser("embed", help="build pinned E5 vectors for the imported snapshot")
    embed.add_argument(
        "--download-model", action="store_true",
        help="allow downloading the pinned E5 model; otherwise use local model files only",
    )
    embed.add_argument(
        "--batch-size", type=int, default=2, choices=(1, 2),
        help="CPU embedding batch size (1-2)",
    )
    embed.add_argument("--threads", type=int, default=1, choices=(1,),
                       help="PyTorch CPU threads (fixed at 1 for thermal safety)")
    embed.add_argument("--thermal-sensor", type=Path, required=True,
                       help="read-only sysfs sensor file; accepts Celsius or millidegrees Celsius")
    embed.add_argument("--stop-temperature-c", type=float, default=70.0,
                       help="stop before the next batch at this temperature (65-80 C; default 70)")
    models = commands.add_parser("models", help="prepare pinned local RedAmon retrieval models")
    models.add_argument(
        "--download", action="store_true",
        help="download pinned E5 and BGE model revisions and verify local inference",
    )
    search = commands.add_parser("search", help="search indexed KB chunks")
    search.add_argument("query")
    search.add_argument("--top-k", type=int, default=DEFAULT_TOP_K)
    search.add_argument("--source", action="append", default=[])
    search.add_argument(
        "--include-supplemental", action="store_true",
        help="include retained Nuclei records outside RedAmon's parser policy",
    )
    parity = commands.add_parser(
        "parity-check",
        help="fail-closed comparison of RedAmon and local KB evidence receipts",
    )
    parity.add_argument("--redamon", type=Path, required=True, help="RedAmon JSON parity receipt")
    parity.add_argument("--assistant", type=Path, required=True, help="Graph Assistant JSON parity receipt")
    parity.add_argument("--fixture", type=Path, required=True, help="pinned JSON query fixture")
    parity.add_argument("--score-tolerance", type=float, default=0.0, help="absolute score tolerance (default: exact)")
    parity_export = commands.add_parser(
        "parity-export",
        help="write a fail-closed local receipt from a complete, read-only KB snapshot",
    )
    parity_export.add_argument("--snapshot-dir", type=Path, required=True, help="version directory containing manifest.json")
    parity_export.add_argument("--profile", choices=("lite", "full"), help="explicit selected profile, e.g. full")
    parity_export.add_argument("--fixture", type=Path, required=True, help="pinned JSON query fixture")
    parity_export.add_argument("--output", type=Path, required=True, help="private local JSON receipt output path")
    args = parser.parse_args(argv)

    if args.data_home is not None:
        os.environ["BB_ASSISTANT_DATA_HOME"] = str(args.data_home.expanduser().resolve())
    if args.command == "build":
        result = build_kb(
            project_root=args.project_root,
            data_home=args.data_home,
            snapshot_root=args.snapshot_root,
            index_path=args.index_path,
            profile=args.profile,
        )
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
        return 0 if result["source_coverage_state"] == "DONE" else 3
    if args.command == "stats":
        index = KBIndex(_configured_index_path(args.index_path))
        print(json.dumps(index.stats(), ensure_ascii=False, indent=2, sort_keys=True))
        return 0
    if args.command == "publish-lucene":
        from .neo4j_lexical import _driver_from_env, publish_generation
        index = KBIndex(_configured_index_path(args.index_path), read_only=True)
        driver, database = _driver_from_env()
        try:
            result = publish_generation(index, driver, database=database, batch_size=args.batch_size)
        finally:
            driver.close()
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
        return 0
    if args.command == "parity-check":
        redamon = load_receipt(args.redamon, expected_producer="redamon")
        assistant = load_receipt(args.assistant, expected_producer="graph-kb-assistant")
        fixture = load_query_fixture(args.fixture)
        result = compare_receipts(redamon, assistant, fixture, score_tolerance=args.score_tolerance)
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
        return 0 if result["status"] == "PASS" else 3
    if args.command == "parity-export":
        index = KBIndex(_configured_index_path(args.index_path), read_only=True)
        receipt = export_assistant_receipt(
            index=index, snapshot_dir=args.snapshot_dir, fixture_path=args.fixture,
            output_path=args.output, profile=args.profile or _snapshot_profile(args.snapshot_dir),
        )
        print(json.dumps({
            "status": "EXPORTED", "producer": receipt["producer"], "profile": receipt["profile"],
            "snapshot_id": receipt["corpus"]["snapshot_id"], "receipt_path": str(args.output.expanduser().resolve()),
            "query_count": len(receipt["queries"]), "source_count": len(receipt["sources"]),
        }, ensure_ascii=False, indent=2, sort_keys=True))
        return 0
    if args.command == "rechunk":
        index = KBIndex(_configured_index_path(args.index_path))
        if args.refresh_local_sources and "candidates" not in index.path.parts:
            raise ValueError("--refresh-local-sources is restricted to isolated candidate indexes")
        last_progress = 0

        def report_progress(done: int, total: int) -> None:
            nonlocal last_progress
            if done == total or done - last_progress >= 10000:
                print(f"rechunked {done}/{total} KB sources", file=sys.stderr, flush=True)
                last_progress = done

        result = rechunk_index(
            index,
            project_root=args.project_root,
            snapshot_root=args.snapshot_root,
            refresh_local_sources=args.refresh_local_sources,
            profile=args.profile,
            progress_callback=report_progress,
        )
        print(json.dumps({**result, **index.stats()}, ensure_ascii=False, indent=2, sort_keys=True))
        return 0 if result["coverage_state"] == "DONE" else 3
    if args.command == "import-snapshot":
        index = KBIndex(_configured_index_path(args.index_path))
        result = import_snapshot(args.snapshot_dir, index)
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
        return 0 if result["coverage_state"] == "DONE" else 3
    if args.command == "models":
        from .embeddings import (
            BGE_RERANKER_MODEL,
            BGE_RERANKER_REVISION,
            E5_MODEL,
            E5_REVISION,
            RedAmonEmbedder,
            RedAmonReranker,
        )
        import importlib.util

        if importlib.util.find_spec("sentence_transformers") is None:
            raise RuntimeError("Install the pinned retrieval runtime with `pip install -e '.[kb-ml]'`.")
        if not args.download:
            print(json.dumps({
                "models": [
                    {"model": E5_MODEL, "revision": E5_REVISION},
                    {"model": BGE_RERANKER_MODEL, "revision": BGE_RERANKER_REVISION},
                ],
                "status": "declared; use --download to cache and verify local weights",
            }, ensure_ascii=False, indent=2, sort_keys=True))
            return 0
        import gc

        embedder = RedAmonEmbedder(local_files_only=False)
        embedder.embed_query("authorization boundary")
        del embedder
        gc.collect()
        reranker = RedAmonReranker(local_files_only=False)
        scores = reranker.score("authorization boundary", ["synthetic local model check"])
        del reranker
        gc.collect()
        if len(scores) != 1:
            raise RuntimeError("Pinned BGE model verification returned an invalid result")
        print(json.dumps({
            "models": [
                {"model": E5_MODEL, "revision": E5_REVISION, "status": "cached and verified"},
                {"model": BGE_RERANKER_MODEL, "revision": BGE_RERANKER_REVISION, "status": "cached and verified"},
            ],
            "status": "ready",
        }, ensure_ascii=False, indent=2, sort_keys=True))
        return 0
    if args.command == "embed":
        # Validate the destination before constructing KBIndex: its writable
        # constructor initializes a missing database, so the live-index guard
        # must run first.
        candidate_path = _embedding_candidate_index_path(args.index_path)
        from .embeddings import RedAmonEmbedder
        from .thermal import ThermalLimitReached, ThermalMonitor
        index = KBIndex(candidate_path)
        thermal = ThermalMonitor(args.thermal_sensor, stop_celsius=args.stop_temperature_c)
        embedder = None

        last_progress = 0

        def report_progress(done: int, total: int) -> None:
            nonlocal last_progress
            if done == total or done - last_progress >= 4096:
                print(f"embedded {done}/{total} KB chunks", file=sys.stderr, flush=True)
                last_progress = done

        while True:
            thermal.wait_for_cool_dwell()
            if embedder is None:
                embedder = RedAmonEmbedder(
                    local_files_only=not args.download_model,
                    cpu_threads=args.threads,
                )
            try:
                count = index.rebuild_embeddings(
                    embedder, batch_size=args.batch_size, progress_callback=report_progress,
                    before_batch_callback=thermal.check,
                )
            except ThermalLimitReached as exc:
                print(
                    f"{exc}; durable embedding batches are preserved; waiting for a 10-minute cool dwell",
                    file=sys.stderr, flush=True,
                )
                continue
            print(json.dumps({"embedded_chunks": count, **index.stats()}, ensure_ascii=False, indent=2, sort_keys=True))
            return 0
    index = KBIndex(_configured_index_path(args.index_path))
    hits = index.search(
        args.query, top_k=args.top_k, source_names=tuple(args.source),
        include_supplemental=args.include_supplemental,
    )
    print(json.dumps([
        {
            "chunk_id": hit.chunk_id,
            "source_name": hit.source_name,
            "source_id": hit.source_id,
            "title": hit.title,
            "excerpt": hit.excerpt,
            "source_locator": hit.source_locator,
            "source_url": hit.source_url,
            "source_revision": hit.source_revision,
            "source_sha256": hit.source_sha256,
            "chunk_sha256": hit.chunk_sha256,
            "span": {
                "start_char": hit.start_char, "end_char": hit.end_char,
                "exact": hit.metadata.get("source_span_exact", True),
            },
            "bm25": hit.bm25,
            "rank_score": hit.rank_score,
            "vector_score": hit.vector_score,
            "rerank_score": hit.rerank_score,
        }
        for hit in hits
    ], ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
