"""Import HTTP history from every visible Caido project via a local MCP server."""
from __future__ import annotations

import argparse
import json
import math
import os
from pathlib import Path
import sys
from typing import Sequence

from .caido_mcp import CaidoMCPReadSource
from .caido_mcp_process import CaidoMCPConfigurationError, CaidoMCPProcessClient
from .domain import CaidoProjectRef
from .ingest import backfill_all_projects
from .storage import decode_vault_key


def _project_payload(projects: Sequence[CaidoProjectRef]) -> dict[str, object]:
    return {
        "projects": [
            {
                "instance_key": project.instance_id,
                "project_id": project.project_id,
                "display_name": project.display_name,
                "project_key": project.storage_key,
            }
            for project in projects
        ],
        "count": len(projects),
    }


def _receipt_payload(receipts: Sequence[object]) -> dict[str, object]:
    enumeration_terminal = bool(receipts) and all(receipt.state == "done" for receipt in receipts)
    if not receipts:
        coverage_state = "no_projects"
        blocker = "Caido returned no projects; empty enumeration is not complete coverage."
    elif enumeration_terminal:
        coverage_state = "terminal_cursor_unreconciled"
        blocker = (
            "The MCP cursor chain terminated, but the source exposes no stable snapshot, "
            "watermark, or expected total for reconciliation."
        )
    else:
        coverage_state = "partial"
        blocker = "One or more projects could not be enumerated to a terminal cursor."
    return {
        "projects": [
            {
                "project_key": receipt.project_key,
                "state": receipt.state,
                "exchanges_seen_this_run": receipt.exchanges_seen,
                "exchanges_saved_this_run": receipt.exchanges_saved,
                "pages_completed": receipt.pages_completed,
                "distinct_request_ids": receipt.distinct_request_ids,
                "run_id": receipt.run_id,
                "problem": receipt.problem,
            }
            for receipt in receipts
        ],
        "coverage_state": coverage_state,
        "enumeration_terminal": enumeration_terminal,
        "coverage_reconciled": False,
        "complete": False,
        "completion_blocker": blocker,
    }


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Read saved HTTP history from Caido MCP into isolated local project stores"
    )
    parser.add_argument("--page-size", type=int, default=50, help="Requests per Caido history page (1–100)")
    parser.add_argument(
        "--page-delay-seconds",
        type=float,
        default=1.0,
        help="Pause after each committed page to limit sustained disk and CPU load",
    )
    parser.add_argument(
        "--max-pages-per-project",
        type=int,
        help="Optional pilot cap per project for this run; saved cursors resume on the next run",
    )
    parser.add_argument("--data-home", type=Path, help="Private local assistant data root")
    parser.add_argument("--instance-key", help="Stable local name for this Caido instance; env: BB_ASSISTANT_CAIDO_INSTANCE_KEY")
    parser.add_argument("--list-only", action="store_true", help="List available Caido projects without importing traffic")
    args = parser.parse_args(argv)
    if not 1 <= args.page_size <= 100:
        parser.error("--page-size must be between 1 and 100")
    if not math.isfinite(args.page_delay_seconds) or args.page_delay_seconds < 0:
        parser.error("--page-delay-seconds must be a finite non-negative number")
    if args.max_pages_per_project is not None and args.max_pages_per_project < 1:
        parser.error("--max-pages-per-project must be positive")
    if args.data_home is not None:
        os.environ["BB_ASSISTANT_DATA_HOME"] = str(args.data_home.expanduser().resolve())
    instance_key = args.instance_key or os.environ.get("BB_ASSISTANT_CAIDO_INSTANCE_KEY")
    if not instance_key:
        parser.error("Set --instance-key or BB_ASSISTANT_CAIDO_INSTANCE_KEY to a stable local Caido instance name")
    if not args.list_only:
        try:
            decode_vault_key()
        except ValueError as exc:
            parser.error(str(exc))

    try:
        with CaidoMCPProcessClient() as caido_client:
            source = CaidoMCPReadSource(caido_client.call_tool, instance_key=instance_key)
            if args.list_only:
                print(json.dumps(_project_payload(source.list_projects()), ensure_ascii=False, indent=2))
                return 0
            receipts = backfill_all_projects(
                source,
                page_size=args.page_size,
                page_delay_seconds=args.page_delay_seconds,
                max_pages_per_run=args.max_pages_per_project,
            )
            summary = _receipt_payload(receipts)
            print(json.dumps(summary, ensure_ascii=False, indent=2))
            return 0 if summary["enumeration_terminal"] else 3
    except CaidoMCPConfigurationError as exc:
        print(json.dumps({"state": "blocked", "problem": str(exc)}), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print(json.dumps({"state": "partial", "problem": "interrupted"}), file=sys.stderr)
        return 130
    except Exception:
        print(json.dumps({"state": "failed", "problem": "Caido MCP import could not be completed"}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
