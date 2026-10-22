"""Bridge one normalized Caido exchange to the vendored behavior graph.

The receipt contains hashes and capture metadata only. HTTP message bytes are
base64-encoded transiently for the parser and never written by this module.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
from itertools import islice
import os
from pathlib import Path
import re
import stat
from typing import Any, Iterable, Mapping, Sequence
from urllib.parse import urlsplit

from ..domain import HttpExchange
from .vendor.common import BehaviorError, source_reference
from .vendor.control import assert_public, canonical_json
from .vendor.graph import BehaviorGraph
from .vendor.importer import analyze_capture, bundle_from_cards


_TIMESTAMP = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}"
    r"(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$"
)
_MAX_BATCH_EXCHANGES = 100


def _private_directory(path: Path) -> Path:
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if os.name == "posix":
        try:
            path.chmod(0o700)
        except OSError:
            pass
        if stat.S_IMODE(path.stat().st_mode) & 0o077:
            raise BehaviorError("Receipt directory permissions are not private")
    return path.resolve(strict=True)


def _selected_program(exchange: HttpExchange, program: str | None) -> str:
    expected = exchange.ref.project.storage_key
    if program is not None and program != expected:
        raise BehaviorError("Behavior graph namespace must match the Caido project")
    return expected


def _write_immutable_receipt(exchange: HttpExchange, receipt_root: str | Path) -> tuple[Path, dict]:
    project_key = exchange.ref.project.storage_key
    observation_id = exchange.ref.observation_id
    receipt = _receipt_record(exchange)
    assert_public(receipt)

    root = _private_directory(Path(receipt_root).expanduser())
    project_dir = root / project_key
    if project_dir.is_symlink():
        raise BehaviorError("Receipt project directory must not be a symbolic link")
    project_dir.mkdir(mode=0o700, exist_ok=True)
    project_dir = _private_directory(project_dir)
    if project_dir.parent != root:
        raise BehaviorError("Receipt directory escaped its configured root")
    encoded = (canonical_json(receipt) + "\n").encode("utf-8")
    receipt_digest = hashlib.sha256(encoded).hexdigest()
    path = project_dir / (observation_id + "." + receipt_digest + ".receipt.json")

    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags, 0o600)
    except FileExistsError:
        if path.is_symlink() or not path.is_file() or path.resolve() != path:
            raise BehaviorError("Existing receipt is not a regular canonical file") from None
        if os.name == "posix" and stat.S_IMODE(path.stat().st_mode) & 0o077:
            raise BehaviorError("Immutable receipt permissions are not private")
        if path.read_bytes() != encoded:
            raise BehaviorError("Immutable receipt conflicts with existing capture provenance") from None
    else:
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(encoded)
                stream.flush()
                os.fsync(stream.fileno())
        except BaseException:
            try:
                os.close(descriptor)
            except OSError:
                pass
            try:
                path.unlink()
            except OSError:
                pass
            raise
    return path, receipt


def _receipt_record(exchange: HttpExchange) -> dict[str, Any]:
    project_key = exchange.ref.project.storage_key
    observation_id = exchange.ref.observation_id
    if exchange.request_truncated is not None and type(exchange.request_truncated) is not bool:
        raise BehaviorError("Request truncation state is invalid")
    if exchange.response_truncated is not None and type(exchange.response_truncated) is not bool:
        raise BehaviorError("Response truncation state is invalid")

    receipt: dict[str, Any] = {
        "schema_version": 1,
        "source": "caido_http_history",
        "hash_basis": "HttpExchange bytes delivered to the parser",
        "project_key": project_key,
        "observation_id": observation_id,
        "request_sha256": hashlib.sha256(exchange.request_raw).hexdigest(),
        "request_bytes": len(exchange.request_raw),
        "request_truncated": exchange.request_truncated,
        "response_sha256": (
            hashlib.sha256(exchange.response_raw).hexdigest()
            if exchange.response_raw is not None else None
        ),
        "response_bytes": (
            len(exchange.response_raw) if exchange.response_raw is not None else None
        ),
        "response_truncated": exchange.response_truncated,
    }
    if isinstance(exchange.captured_at, str) and _TIMESTAMP.fullmatch(exchange.captured_at):
        receipt["captured_at"] = exchange.captured_at
    return receipt


def _write_page_receipt(exchanges: list[HttpExchange], receipt_root: str | Path) -> tuple[Path, list[dict]]:
    if not exchanges:
        raise BehaviorError("A page receipt requires at least one exchange")
    project_key = exchanges[0].ref.project.storage_key
    if any(exchange.ref.project.storage_key != project_key for exchange in exchanges):
        raise BehaviorError("A page receipt cannot mix Caido projects")
    receipt = {
        "schema_version": 1,
        "source": "caido_http_history_page",
        "hash_basis": "HttpExchange bytes delivered to the parser",
        "project_key": project_key,
        "records": [_receipt_record(exchange) for exchange in exchanges],
    }
    assert_public(receipt)
    root = _private_directory(Path(receipt_root).expanduser())
    project_dir = root / project_key
    if project_dir.is_symlink():
        raise BehaviorError("Receipt project directory must not be a symbolic link")
    project_dir.mkdir(mode=0o700, exist_ok=True)
    project_dir = _private_directory(project_dir)
    if project_dir.parent != root:
        raise BehaviorError("Receipt directory escaped its configured root")
    encoded = (canonical_json(receipt) + "\n").encode("utf-8")
    receipt_digest = hashlib.sha256(encoded).hexdigest()
    path = project_dir / ("page-" + receipt_digest + ".receipt.json")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags, 0o600)
    except FileExistsError:
        if path.is_symlink() or not path.is_file() or path.resolve() != path:
            raise BehaviorError("Existing receipt is not a regular canonical file") from None
        if os.name == "posix" and stat.S_IMODE(path.stat().st_mode) & 0o077:
            raise BehaviorError("Existing receipt permissions are not private")
        if path.read_bytes() != encoded:
            raise BehaviorError("Immutable page receipt conflicts with existing capture provenance") from None
    else:
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(encoded)
                stream.flush()
                os.fsync(stream.fileno())
        except BaseException:
            try:
                os.close(descriptor)
            except OSError:
                pass
            try:
                path.unlink()
            except OSError:
                pass
            raise
    source_refs = [source_reference(path, f"$.records[{index}]") for index in range(len(exchanges))]
    return path, source_refs


def build_exchange_bundle(
    exchange: HttpExchange,
    receipt_root: str | Path,
    *,
    program: str | None = None,
    request_fingerprint_key: bytes | None = None,
    response_fingerprint_key: bytes | None = None,
    max_message_bytes: int = 2 * 1024 * 1024,
    max_fields: int = 2000,
    max_depth: int = 64,
) -> dict:
    """Create a source-pinned redacted graph bundle for one Caido exchange.

    ``receipt_root`` should be the private runtime data directory for this
    application. One immutable receipt is placed below the hashed Caido project
    namespace. The returned bundle contains no request/response values.
    """
    selected_program = _selected_program(exchange, program)
    path, _receipt = _write_immutable_receipt(exchange, receipt_root)
    source_ref = source_reference(path, "$.observation_id")
    return _build_exchange_bundle_from_ref(
        exchange,
        source_ref,
        selected_program,
        request_fingerprint_key=request_fingerprint_key,
        response_fingerprint_key=response_fingerprint_key,
        max_message_bytes=max_message_bytes,
        max_fields=max_fields,
        max_depth=max_depth,
    )


def _build_exchange_bundle_from_ref(
    exchange: HttpExchange,
    source_ref: dict,
    selected_program: str,
    *,
    request_fingerprint_key: bytes | None = None,
    response_fingerprint_key: bytes | None = None,
    max_message_bytes: int = 2 * 1024 * 1024,
    max_fields: int = 2000,
    max_depth: int = 64,
) -> dict:

    try:
        target_parts = urlsplit(exchange.target or "")
    except ValueError:
        target_parts = urlsplit("")
    scheme = exchange.scheme if exchange.scheme in {"http", "https"} else target_parts.scheme.lower()
    host = exchange.host or target_parts.hostname
    port = exchange.port or target_parts.port
    if scheme not in {"http", "https"}:
        host = None
        port = None
    if port is None and scheme in {"http", "https"}:
        port = 443 if scheme == "https" else 80

    node = {
        # Do not persist Caido's lookup ID in the graph. The stable digest is
        # scoped to both the Caido instance and project.
        "id": exchange.ref.observation_id,
        "observed_at": exchange.captured_at,
        "method": exchange.method,
        "host": host,
        "port": port,
        "isTls": scheme == "https",
        "path": exchange.target if isinstance(exchange.target, str) else "/",
        "request_truncated": exchange.request_truncated,
        "raw": base64.b64encode(exchange.request_raw).decode("ascii"),
        "response": {
            "statusCode": exchange.response_status,
            "truncated": exchange.response_truncated,
        },
    }
    if exchange.response_raw is not None:
        node["response"]["raw"] = base64.b64encode(exchange.response_raw).decode("ascii")

    card = analyze_capture(
        node,
        source_ref,
        selected_program,
        max_message_bytes=max_message_bytes,
        max_fields=max_fields,
        max_depth=max_depth,
    )
    card["request_analysis"]["capture_truncated"] = exchange.request_truncated
    card["response_analysis"]["capture_truncated"] = exchange.response_truncated
    if request_fingerprint_key is not None:
        if not isinstance(request_fingerprint_key, bytes) or len(request_fingerprint_key) < 32:
            raise BehaviorError("Request fingerprint key must contain at least 32 bytes")
        card["request_analysis"]["request_signature"] = hmac.new(
            request_fingerprint_key,
            b"bb-graph-request-v1\x00" + selected_program.encode("ascii") + b"\x00"
            + exchange.request_raw + b"\x00extent=" + str(exchange.request_truncated).encode("ascii"),
            hashlib.sha256,
        ).hexdigest()
    if exchange.response_raw is not None and response_fingerprint_key is not None:
        if not isinstance(response_fingerprint_key, bytes) or len(response_fingerprint_key) < 32:
            raise BehaviorError("Response fingerprint key must contain at least 32 bytes")
        card["response_analysis"]["response_signature"] = hmac.new(
            response_fingerprint_key,
            b"bb-graph-response-v1\x00" + selected_program.encode("ascii") + b"\x00"
            + exchange.response_raw + b"\x00extent=" + str(exchange.response_truncated).encode("ascii"),
            hashlib.sha256,
        ).hexdigest()
    return bundle_from_cards([card], [source_ref], selected_program)


def import_exchanges(
    graph: BehaviorGraph,
    exchanges: Iterable[HttpExchange],
    receipt_root: str | Path,
    *,
    include_snapshot: bool = True,
    request_fingerprint_key: bytes | None = None,
    response_fingerprint_key: bytes | None = None,
    causal_predecessors: Mapping[
        str,
        Sequence[tuple[str, str] | tuple[str, str, str]],
    ] | None = None,
    max_message_bytes: int = 2 * 1024 * 1024,
    max_fields: int = 2000,
    max_depth: int = 64,
) -> dict:
    """Parse one Caido page and import it with one graph transaction.

    The input is bounded to the Caido history page limit. Namespace validation
    completes before receipt creation; all page bundles are built before any
    graph mutation. Receipt files created before a later parse/import failure
    remain immutable provenance, while the graph transaction is all-or-nothing.
    Large backfills can set ``include_snapshot=False`` to defer the full graph
    digest until a later read instead of recomputing it after every page.
    """
    try:
        selected = list(islice(iter(exchanges), _MAX_BATCH_EXCHANGES + 1))
    except TypeError as exc:
        raise BehaviorError("Caido exchanges must be an iterable page") from exc
    if not selected:
        raise BehaviorError("A behavior graph page must contain at least one exchange")
    if len(selected) > _MAX_BATCH_EXCHANGES:
        raise BehaviorError("Caido page exceeds the 100-exchange import limit")

    expected_program = graph.program
    for exchange in selected:
        if not isinstance(exchange, HttpExchange):
            raise BehaviorError("Caido page contains a non-HttpExchange value")
        if exchange.ref.project.storage_key != expected_program:
            raise BehaviorError("BehaviorGraph is not namespaced for every Caido exchange")

    _receipt_path, source_refs = _write_page_receipt(selected, receipt_root)
    bundles = []
    entries = []
    pending_links: list[tuple[str, str, str, str]] = []
    for exchange, source_ref in zip(selected, source_refs, strict=True):
        bundle = _build_exchange_bundle_from_ref(
            exchange,
            source_ref,
            expected_program,
            request_fingerprint_key=request_fingerprint_key,
            response_fingerprint_key=response_fingerprint_key,
            max_message_bytes=max_message_bytes,
            max_fields=max_fields,
            max_depth=max_depth,
        )
        observations = [row for row in bundle["entities"] if row.get("kind") == "observation"]
        if len(observations) != 1 or len(bundle["cards"]) != 1:
            raise BehaviorError("One Caido exchange must produce one graph observation and card")
        predecessors = (causal_predecessors or {}).get(exchange.ref.observation_id, ())
        for link in predecessors:
            if not isinstance(link, (tuple, list)) or len(link) not in {2, 3}:
                raise BehaviorError("Causal graph link has an invalid shape")
            predecessor_id, predicate = link[:2]
            basis = link[2] if len(link) == 3 else predicate
            if (
                not isinstance(predecessor_id, str)
                or not re.fullmatch(r"(?:observation-)?[a-f0-9]{64}", predecessor_id)
                or not isinstance(predicate, str)
                or predicate not in {"referrer_context_for", "redirects_to"}
                or not isinstance(basis, str)
                or not (
                    predicate == "referrer_context_for"
                    and basis in {"referrer_exact_url", "referrer_route_template", "referrer_context_for"}
                    or predicate == "redirects_to"
                    and basis in {"redirect_exact_url", "redirect_route_template", "redirects_to"}
                )
            ):
                raise BehaviorError("Causal graph link is invalid")
            pending_links.append((exchange.ref.observation_id, predecessor_id, predicate, basis))
        bundles.append(bundle)
        entries.append({
            "capture_observation_id": exchange.ref.observation_id,
            "graph_observation_id": observations[0]["id"],
            "analysis_state": bundle["cards"][0]["analysis_state"],
        })

    capture_to_graph = {
        entry["capture_observation_id"]: entry["graph_observation_id"]
        for entry in entries
    }
    bundle_by_capture = {
        entry["capture_observation_id"]: bundle
        for entry, bundle in zip(entries, bundles, strict=True)
    }
    for current_capture_id, predecessor_id, predicate, basis in pending_links:
        current_bundle = bundle_by_capture[current_capture_id]
        observation = next(row for row in current_bundle["entities"] if row.get("kind") == "observation")
        subject_id = capture_to_graph.get(predecessor_id, predecessor_id)
        if not re.fullmatch(r"observation-[a-f0-9]{64}", subject_id):
            raise BehaviorError("Causal graph predecessor is not an observation")
        record = {
            "subject": subject_id,
            "predicate": predicate,
            "object": observation["id"],
            "context": {
                "program": expected_program,
                "link_basis": basis,
                "causality": "navigation_evidence_only",
            },
            "origin_kind": "parsed",
            "evidence_state": "observed",
            "source_refs": observation["source_refs"],
        }
        record["id"] = entity_id("assertion", record)
        current_bundle["assertions"].append(record)
        current_bundle["assertions"].sort(key=lambda item: item["id"])

    imported = graph.import_bundles(bundles, include_snapshot=include_snapshot)
    return {**imported, "exchanges": entries}


def import_exchange(
    graph: BehaviorGraph,
    exchange: HttpExchange,
    receipt_root: str | Path,
    *,
    request_fingerprint_key: bytes | None = None,
    response_fingerprint_key: bytes | None = None,
    max_message_bytes: int = 2 * 1024 * 1024,
    max_fields: int = 2000,
    max_depth: int = 64,
) -> dict:
    """Backward-compatible single-exchange wrapper around ``import_exchanges``."""
    batch = import_exchanges(
        graph,
        [exchange],
        receipt_root,
        request_fingerprint_key=request_fingerprint_key,
        response_fingerprint_key=response_fingerprint_key,
        max_message_bytes=max_message_bytes,
        max_fields=max_fields,
        max_depth=max_depth,
    )
    imported = {key: value for key, value in batch.items() if key != "exchanges"}
    imported.update({
        "graph_observation_id": batch["exchanges"][0]["graph_observation_id"],
        "analysis_state": batch["exchanges"][0]["analysis_state"],
    })
    return imported
