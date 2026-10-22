"""Continuous delta sync from Caido into the compact local behavior graph."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import sys
import time
from typing import Sequence

from .caido_mcp import CaidoMCPReadSource
from .caido_mcp_process import CaidoMCPConfigurationError, CaidoMCPProcessClient
from .ingest import tail_all_projects
from .storage import decode_vault_key


_PROJECT_KEY = re.compile(r"^[a-f0-9]{64}$")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Continuously index only new Caido HTTP traffic; full messages remain in Caido"
    )
    parser.add_argument("--instance-key", help="Stable local Caido instance name; env: BB_ASSISTANT_CAIDO_INSTANCE_KEY")
    parser.add_argument("--data-home", type=Path, help="Private local assistant data root")
    parser.add_argument("--poll-interval-seconds", type=float, default=10.0, help="Delay between delta polls")
    parser.add_argument("--overlap-seconds", type=int, default=90, help="Time overlap used to recover delayed records")
    parser.add_argument("--page-size", type=int, default=50, help="Caido metadata rows per page (1–100)")
    parser.add_argument("--max-pages-per-cycle", type=int, default=1, help="Bound each project cycle; cursors resume next cycle")
    parser.add_argument("--max-body-bytes", type=int, default=256 * 1024, help="Maximum request/response body bytes read per exchange")
    parser.add_argument("--project-key", action="append", dest="project_keys", help="Optional local project hash; repeat to select multiple")
    parser.add_argument("--once", action="store_true", help="Run one bounded cycle and exit")
    args = parser.parse_args(argv)

    if args.data_home is not None:
        os.environ["BB_ASSISTANT_DATA_HOME"] = str(args.data_home.expanduser().resolve())
    instance_key = args.instance_key or os.environ.get("BB_ASSISTANT_CAIDO_INSTANCE_KEY")
    if not instance_key:
        parser.error("Set --instance-key or BB_ASSISTANT_CAIDO_INSTANCE_KEY to a stable local Caido instance name")
    if args.poll_interval_seconds < 1 or args.poll_interval_seconds > 3600:
        parser.error("--poll-interval-seconds must be between 1 and 3600")
    if not 1 <= args.page_size <= 100:
        parser.error("--page-size must be between 1 and 100")
    if args.max_pages_per_cycle < 1:
        parser.error("--max-pages-per-cycle must be positive")
    if not 64 * 1024 <= args.max_body_bytes <= 16 * 1024 * 1024:
        parser.error("--max-body-bytes must be between 65536 and 16777216")
    if not 1 <= args.overlap_seconds <= 3600:
        parser.error("--overlap-seconds must be between 1 and 3600")
    if args.project_keys and any(_PROJECT_KEY.fullmatch(value) is None for value in args.project_keys):
        parser.error("Every --project-key must be a 64-character lowercase hexadecimal hash")
    try:
        decode_vault_key()
    except ValueError as exc:
        parser.error(str(exc))

    selected = frozenset(args.project_keys) if args.project_keys else None
    try:
        with CaidoMCPProcessClient() as caido_client:
            source = CaidoMCPReadSource(caido_client.call_tool, instance_key=instance_key)
            while True:
                receipts = tail_all_projects(
                    source,
                    page_size=args.page_size,
                    max_pages_per_cycle=args.max_pages_per_cycle,
                    overlap_seconds=args.overlap_seconds,
                    max_body_bytes=args.max_body_bytes,
                    selected_project_keys=selected,
                )
                print(json.dumps({
                    "mode": "caido_http_live_tail",
                    "projects": [
                        {
                            "project_key": receipt.project_key,
                            "state": receipt.state,
                            "requests_seen": receipt.requests_seen,
                            "requests_added_to_graph": receipt.requests_added,
                            "pages_completed": receipt.pages_completed,
                            "watermark": receipt.watermark,
                            "cursor_pending": receipt.next_cursor is not None,
                            "problem": receipt.problem,
                        }
                        for receipt in receipts
                    ],
                }, ensure_ascii=False), flush=True)
                if args.once:
                    return 0 if all(receipt.state == "idle" for receipt in receipts) else 3
                time.sleep(args.poll_interval_seconds)
    except KeyboardInterrupt:
        print(json.dumps({"mode": "caido_http_live_tail", "state": "stopped"}), flush=True)
        return 130
    except CaidoMCPConfigurationError as exc:
        print(json.dumps({"state": "blocked", "problem": str(exc)}), file=sys.stderr)
        return 2
    except Exception:
        print(json.dumps({"state": "failed", "problem": "Caido live tail could not be completed"}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
