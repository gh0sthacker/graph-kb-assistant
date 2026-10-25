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
from .rebuild import export_redamon_snapshot, import_snapshot
from .ranking import DEFAULT_TOP_K


def build_kb(
    *,
    project_root: Path,
    data_home: Path | None = None,
    snapshot_root: Path | None = None,
    index_path: Path | None = None,
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


def _data_root() -> Path:
    from ..paths import data_root

    return data_root()


def _configured_index_path(explicit_path: Path | None) -> Path | None:
    """Resolve the index override shared by the CLI and the MCP facade."""
    if explicit_path is not None:
        return explicit_path.expanduser().resolve()
    configured = os.environ.get("BB_ASSISTANT_KB_INDEX")
    return Path(configured).expanduser().resolve() if configured else None


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
    commands.add_parser("build", help="build available pinned sources and import the KB index")
    commands.add_parser("stats", help="show KB source and coverage counts")
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
        "--batch-size", type=int, default=64, choices=range(1, 257),
        help="number of similarly sized chunks per embedding batch (1-256; default: 64)",
    )
    models = commands.add_parser("models", help="prepare pinned local RedAmon retrieval models")
    models.add_argument(
        "--download", action="store_true",
        help="download pinned E5 and BGE model revisions and verify local inference",
    )
    search = commands.add_parser("search", help="search indexed KB chunks")
    search.add_argument("query")
    search.add_argument("--top-k", type=int, default=DEFAULT_TOP_K)
    search.add_argument("--source", action="append", default=[])
    args = parser.parse_args(argv)

    if args.data_home is not None:
        os.environ["BB_ASSISTANT_DATA_HOME"] = str(args.data_home.expanduser().resolve())
    if args.command == "build":
        result = build_kb(
            project_root=args.project_root,
            data_home=args.data_home,
            snapshot_root=args.snapshot_root,
            index_path=args.index_path,
        )
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
        return 0 if result["source_coverage_state"] == "DONE" else 3
    if args.command == "stats":
        index = KBIndex(_configured_index_path(args.index_path))
        print(json.dumps(index.stats(), ensure_ascii=False, indent=2, sort_keys=True))
        return 0
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
        from .embeddings import RedAmonEmbedder
        index = KBIndex(_configured_index_path(args.index_path))
        embedder = RedAmonEmbedder(local_files_only=not args.download_model)

        last_progress = 0

        def report_progress(done: int, total: int) -> None:
            nonlocal last_progress
            if done == total or done - last_progress >= 4096:
                print(f"embedded {done}/{total} KB chunks", file=sys.stderr, flush=True)
                last_progress = done

        count = index.rebuild_embeddings(
            embedder, batch_size=args.batch_size, progress_callback=report_progress,
        )
        print(json.dumps({"embedded_chunks": count, **index.stats()}, ensure_ascii=False, indent=2, sort_keys=True))
        return 0
    index = KBIndex(_configured_index_path(args.index_path))
    hits = index.search(args.query, top_k=args.top_k, source_names=tuple(args.source))
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
            "span": {"start_char": hit.start_char, "end_char": hit.end_char},
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
