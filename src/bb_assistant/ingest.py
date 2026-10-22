"""Resumable, sequential initial backfill of an explicitly selected project."""
from __future__ import annotations

from dataclasses import dataclass
from dataclasses import replace
from datetime import datetime, timedelta, timezone
import hashlib
import hmac
import math
import re
import time
from urllib.parse import urljoin, urlsplit, urlunsplit

from .behavior import BehaviorGraph, import_exchanges
from .caido import CaidoReadSource, validate_page
from .caido_mcp import CaidoMCPError
from .domain import CaidoProjectRef
from .graph import _route_template, project_exchange, store_projection
from .storage import ProjectStore, decode_vault_key


_SOURCE_KEY = "caido-http-history-v1"
_LIVE_SOURCE_KEY = "caido-http-live-tail-v1"
_LIVE_BODY_MAX_DEFAULT = 256 * 1024


@dataclass(frozen=True, slots=True)
class SyncReceipt:
    project_key: str
    state: str
    exchanges_seen: int
    exchanges_saved: int
    next_cursor: str | None
    problem: str | None = None
    run_id: str | None = None
    started_at: str | None = None
    pages_completed: int = 0
    distinct_request_ids: int = 0


def _cursor_digest(cursor: str | None) -> str:
    if cursor is None:
        material = b"caido-history-cursor:start\x00"
    else:
        material = b"caido-history-cursor:value\x00" + cursor.encode("utf-8", errors="strict")
    return hashlib.sha256(material).hexdigest()


def _request_id_digest(request_id: str) -> str:
    return hashlib.sha256(b"caido-history-request-id\x00" + request_id.encode("utf-8", errors="strict")).hexdigest()


def _project_index_keys(project: CaidoProjectRef, root_key: bytes) -> tuple[bytes, bytes]:
    index_key = hmac.new(
        root_key,
        b"bb-assistant-live-index-v1\x00" + project.storage_key.encode("ascii"),
        hashlib.sha256,
    ).digest()
    response_key = hmac.new(index_key, b"response-signature-v1", hashlib.sha256).digest()
    return index_key, response_key


def sync_project(
    source: CaidoReadSource,
    project: CaidoProjectRef,
    *,
    page_size: int = 50,
    page_delay_seconds: float = 1.0,
    max_pages_per_run: int | None = None,
    store: ProjectStore | None = None,
) -> SyncReceipt:
    """Backfill one project; a stopped/failed run resumes at its saved page cursor.

    This is the separately controlled full-history import. The continuous
    incremental path is ``sync_live_project`` below and does not use this cursor.
    """
    # Caido's MCP list_requests contract caps each page at 100 rows. Keep the
    # ingest API aligned so an oversized setting cannot turn a whole-project
    # sync into a partial run after it starts.
    if not (1 <= page_size <= 100):
        raise ValueError("page_size must be between 1 and 100")
    if not math.isfinite(page_delay_seconds) or page_delay_seconds < 0:
        raise ValueError("page_delay_seconds must be a finite non-negative number")
    if max_pages_per_run is not None and max_pages_per_run < 1:
        raise ValueError("max_pages_per_run must be positive")
    target_store = store or ProjectStore(project)
    if not target_store.project.same_project(project):
        raise ValueError("ProjectStore namespace does not match the selected Caido project")
    vault_key = decode_vault_key()
    _index_key, response_key = _project_index_keys(project, vault_key)
    run = target_store.begin_sync_run(_SOURCE_KEY)
    run_id = run["run_id"]
    started_at = run["started_at"]
    cursor = run["cursor"]
    if not isinstance(run_id, str) or not isinstance(started_at, str) or (cursor is not None and not isinstance(cursor, str)):
        raise ValueError("Persisted sync run state is invalid")
    seen = 0
    saved = 0
    pages_processed = 0
    behavior_graph = None
    try:
        behavior_graph = BehaviorGraph(target_store.behavior_graph_path, project.storage_key)
        while True:
            page = source.list_history(project, cursor=cursor, page_size=page_size)
            validate_page(page, project, cursor)
            request_id_digests = tuple(_request_id_digest(exchange.ref.request_id) for exchange in page.exchanges)
            next_cursor_digest = _cursor_digest(page.next_cursor) if page.has_more else None
            cursor_digest = _cursor_digest(cursor)
            target_store.validate_sync_page(
                run_id,
                cursor_digest=cursor_digest,
                next_cursor_digest=next_cursor_digest,
                request_id_digests=request_id_digests,
                has_more=page.has_more,
            )
            saved_captures = []
            for exchange in page.exchanges:
                saved_captures.append(target_store.save_exchange(exchange, key=vault_key))
            graph_receipt = import_exchanges(
                behavior_graph,
                page.exchanges,
                target_store.behavior_receipt_directory,
                include_snapshot=False,
                request_fingerprint_key=_index_key,
                response_fingerprint_key=response_key,
            ) if page.exchanges else {"exchanges": []}
            graph_exchanges = graph_receipt["exchanges"]
            if len(graph_exchanges) != len(page.exchanges):
                raise RuntimeError("BehaviorGraph returned an unexpected exchange count")
            for exchange, saved_capture, graph_exchange in zip(
                page.exchanges, saved_captures, graph_exchanges, strict=True
            ):
                if graph_exchange.get("capture_observation_id") != exchange.ref.observation_id:
                    raise RuntimeError("BehaviorGraph changed Caido page ordering or identity")
                target_store.set_graph_observation(
                    exchange.ref.observation_id,
                    graph_exchange["graph_observation_id"],
                    revision_id=saved_capture["revision_id"],
                )
                projection = project_exchange(exchange)
                store_projection(
                    target_store,
                    projection,
                    analysis_state=graph_exchange["analysis_state"],
                )
                seen += 1
                saved += 1
            progress = target_store.commit_sync_page(
                _SOURCE_KEY,
                run_id,
                cursor_digest=cursor_digest,
                next_cursor_digest=next_cursor_digest,
                request_id_digests=request_id_digests,
                next_cursor=page.next_cursor if page.has_more else None,
                last_request_id=page.exchanges[-1].ref.request_id if page.exchanges else None,
                has_more=page.has_more,
            )
            pages_processed += 1
            cursor = page.next_cursor if page.has_more else None
            if not page.has_more:
                return SyncReceipt(
                    project.storage_key,
                    "done",
                    seen,
                    saved,
                    cursor,
                    run_id=run_id,
                    started_at=started_at,
                    pages_completed=progress["page_count"],
                    distinct_request_ids=progress["distinct_request_count"],
                )
            if max_pages_per_run is not None and pages_processed >= max_pages_per_run:
                target_store.mark_sync_partial(_SOURCE_KEY, run_id)
                return SyncReceipt(
                    project.storage_key,
                    "partial",
                    seen,
                    saved,
                    cursor,
                    "page_limit_reached",
                    run_id=run_id,
                    started_at=started_at,
                    pages_completed=progress["page_count"],
                    distinct_request_ids=progress["distinct_request_count"],
                )
            if page_delay_seconds:
                time.sleep(page_delay_seconds)
    except BaseException:
        try:
            target_store.mark_sync_partial(_SOURCE_KEY, run_id)
        except Exception:
            # Preserve the original interruption/error. The last committed page
            # remains resumable even if the status update itself cannot finish.
            pass
        raise
    finally:
        if behavior_graph is not None:
            behavior_graph.close()


def backfill_all_projects(
    source: CaidoReadSource,
    *,
    page_size: int = 50,
    page_delay_seconds: float = 1.0,
    max_pages_per_run: int | None = None,
) -> tuple[SyncReceipt, ...]:
    """Backfill each project returned by the read adapter, one at a time.

    A project that is closed, inaccessible or unsupported receives a partial
    receipt; later projects are still attempted. The caller should display the
    exact project coverage gaps and not claim completion while any are partial.
    """
    receipts = []
    projects = tuple(source.list_projects())
    if not projects:
        if source.list_projects():
            raise ValueError("Caido project inventory changed during enumeration")
        return ()
    keys = [project.storage_key for project in projects]
    if len(set(keys)) != len(keys):
        raise ValueError("Caido project listing contains duplicate identities")
    for project in projects:
        try:
            receipts.append(
                sync_project(
                    source,
                    project,
                    page_size=page_size,
                    page_delay_seconds=page_delay_seconds,
                    max_pages_per_run=max_pages_per_run,
                )
            )
        except Exception as exc:
            store = ProjectStore(project)
            # The MCP adapter's messages are fixed, sanitized diagnostics. Keep
            # them so resumable imports distinguish a stale cursor, a Caido
            # tool error, and a local normalization failure without exposing
            # request/response contents or credentials.
            problem = str(exc) if isinstance(exc, CaidoMCPError) else type(exc).__name__
            progress = store.sync_progress(_SOURCE_KEY)
            if progress is None:
                with store.connect() as connection:
                    connection.execute(
                        "INSERT INTO sync_state(source_key,cursor,last_request_id,state,updated_at) "
                        "VALUES(?,NULL,NULL,'partial',?) ON CONFLICT(source_key) DO UPDATE SET "
                        "state='partial',updated_at=excluded.updated_at",
                        (_SOURCE_KEY, datetime.now(timezone.utc).isoformat()),
                    )
                progress = store.sync_progress(_SOURCE_KEY)
            receipts.append(
                SyncReceipt(
                    project.storage_key,
                    "partial",
                    0,
                    0,
                    progress["cursor"] if progress and isinstance(progress["cursor"], str) else None,
                    problem,
                    run_id=progress["run_id"] if progress and isinstance(progress["run_id"], str) else None,
                    started_at=progress["started_at"] if progress and isinstance(progress["started_at"], str) else None,
                    pages_completed=progress["page_count"] if progress and isinstance(progress["page_count"], int) else 0,
                    distinct_request_ids=progress["distinct_request_count"] if progress and isinstance(progress["distinct_request_count"], int) else 0,
                )
            )

    def mark_inventory_incomplete(problem: str) -> tuple[SyncReceipt, ...]:
        projects_by_key = {project.storage_key: project for project in projects}
        updated_receipts = []
        for receipt in receipts:
            project = projects_by_key.get(receipt.project_key)
            if project is not None and receipt.run_id is not None:
                try:
                    ProjectStore(project).mark_sync_partial(_SOURCE_KEY, receipt.run_id)
                except Exception:
                    pass
            updated_receipts.append(replace(receipt, state="partial", problem=problem))
        return tuple(updated_receipts)

    try:
        projects_after = tuple(source.list_projects())
    except Exception:
        return mark_inventory_incomplete("project_inventory_recheck_failed")
    initial_keys = {project.storage_key for project in projects}
    final_keys = {project.storage_key for project in projects_after}
    if initial_keys != final_keys:
        return mark_inventory_incomplete("project_inventory_changed_during_import")
    return tuple(receipts)


@dataclass(frozen=True, slots=True)
class LiveTailReceipt:
    project_key: str
    state: str
    requests_seen: int
    requests_added: int
    pages_completed: int
    watermark: str | None
    next_cursor: str | None = None
    problem: str | None = None


def _utc_stamp(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _header_value(raw: bytes | None, name: str) -> str | None:
    if not raw:
        return None
    boundary = raw.find(b"\r\n\r\n")
    delimiter = b"\r\n"
    if boundary < 0:
        boundary = raw.find(b"\n\n")
        delimiter = b"\n"
    if boundary < 0:
        return None
    found: list[str] = []
    for line in raw[:boundary].split(delimiter)[1:]:
        field, separator, value = line.partition(b":")
        if separator and field.strip().lower() == name.encode("ascii"):
            found.append(value.strip().decode("latin-1", errors="replace"))
    return found[0] if len(found) == 1 else None


def _url_index_key(value: str | None, base: str | None, key: bytes) -> str | None:
    if not value:
        return None
    try:
        absolute = urljoin(base or "", value)
        parts = urlsplit(absolute)
        if (
            parts.scheme.lower() not in {"http", "https"}
            or not parts.hostname
            or parts.username is not None
            or parts.password is not None
        ):
            return None
        host = parts.hostname.lower()
        port = parts.port
        authority = host if port is None or port == (443 if parts.scheme.lower() == "https" else 80) else f"{host}:{port}"
        normalized = urlunsplit((parts.scheme.lower(), authority, parts.path or "/", parts.query, ""))
    except (TypeError, ValueError, UnicodeError):
        return None
    return hmac.new(key, b"bb-assistant-url-index-v1\x00" + normalized.encode("utf-8"), hashlib.sha256).hexdigest()


def _url_endpoint(value: str | None, base: str | None = None) -> tuple[str, int, str] | None:
    """Return the redacted host/port/route identity used for historical links."""
    if not value:
        return None
    try:
        parts = urlsplit(urljoin(base or "", value))
        if (
            parts.scheme.lower() not in {"http", "https"}
            or not parts.hostname
            or parts.username is not None
            or parts.password is not None
        ):
            return None
        host = parts.hostname.lower()
        if re.fullmatch(r"[A-Za-z0-9.-]{1,253}", host) is None:
            return None
        port = parts.port or (443 if parts.scheme.lower() == "https" else 80)
        if not 1 <= port <= 65535:
            return None
        route = _route_template(parts.path or "/")
    except (TypeError, ValueError, UnicodeError):
        return None
    return host, port, route


def _live_material(exchange, index_key: bytes, response_key: bytes) -> dict[str, object]:
    projection = project_exchange(exchange)
    request_url_key = _url_index_key(exchange.target, None, index_key)
    if request_url_key is None:
        raise ValueError("Caido request URL could not be normalized for graph indexing")
    referer_url_key = _url_index_key(_header_value(exchange.request_raw, "referer"), exchange.target, index_key)
    referer_endpoint = _url_endpoint(_header_value(exchange.request_raw, "referer"), exchange.target)
    redirect_url_key = None
    redirect_endpoint = None
    if exchange.response_status is not None and 300 <= exchange.response_status < 400:
        location = _header_value(exchange.response_raw, "location")
        redirect_url_key = _url_index_key(location, exchange.target, index_key)
        redirect_endpoint = _url_endpoint(location, exchange.target)
    request_signature = hmac.new(
        index_key,
        b"bb-graph-request-v1\x00" + exchange.ref.project.storage_key.encode("ascii")
        + b"\x00" + exchange.request_raw + b"\x00extent="
        + str(exchange.request_truncated).encode("ascii"),
        hashlib.sha256,
    ).hexdigest()
    response_signature = (
        hmac.new(
            response_key,
            b"bb-graph-response-v1\x00" + exchange.ref.project.storage_key.encode("ascii")
            + b"\x00" + exchange.response_raw + b"\x00extent="
            + str(exchange.response_truncated).encode("ascii"),
            hashlib.sha256,
        ).hexdigest()
        if exchange.response_raw is not None else None
    )
    return {
        "exchange": exchange,
        "projection": projection,
        "request_url_key": request_url_key,
        "referer_url_key": referer_url_key,
        "redirect_url_key": redirect_url_key,
        "request_endpoint": (projection.search_record["host"], projection.search_record["port"], projection.search_record["route"]),
        "referer_endpoint": referer_endpoint,
        "redirect_endpoint": redirect_endpoint,
        "request_signature": request_signature,
        "response_signature": response_signature,
    }


def sync_live_project(
    source: CaidoReadSource,
    project: CaidoProjectRef,
    *,
    page_size: int = 100,
    max_pages_per_cycle: int = 5,
    overlap_seconds: int = 90,
    max_body_bytes: int = _LIVE_BODY_MAX_DEFAULT,
    store: ProjectStore | None = None,
    now: datetime | None = None,
) -> LiveTailReceipt:
    """Import only the recent delta window; Caido remains the raw-message store.

    Every cycle uses a fixed RFC3339 time window with overlap. Request IDs are
    deduplicated before message bodies are fetched, and the exact window cursor
    is durable when a bounded cycle must continue on its next invocation.
    """
    if not 1 <= page_size <= 100:
        raise ValueError("page_size must be between 1 and 100")
    if max_pages_per_cycle < 1:
        raise ValueError("max_pages_per_cycle must be positive")
    if not 1 <= overlap_seconds <= 3600:
        raise ValueError("overlap_seconds must be between 1 and 3600")
    if not (64 * 1024 <= max_body_bytes <= 16 * 1024 * 1024):
        raise ValueError("max_body_bytes must be between 65536 and 16777216")
    target_store = store or ProjectStore(project)
    if not target_store.project.same_project(project):
        raise ValueError("ProjectStore namespace does not match the selected Caido project")
    fingerprint_root_key = decode_vault_key()
    index_key, response_key = _project_index_keys(project, fingerprint_root_key)

    current_time = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    current_progress = target_store.live_sync_progress(_LIVE_SOURCE_KEY)
    watermark = current_progress.get("watermark") if current_progress else None
    resume_window = bool(
        current_progress
        and current_progress.get("window_since")
        and current_progress.get("window_until")
        and current_progress.get("state") in {"running", "partial"}
    )
    if resume_window:
        window_since = current_progress["window_since"]
        window_until = current_progress["window_until"]
        cursor = current_progress.get("cursor")
    else:
        lower = (
            datetime.fromisoformat(watermark.replace("Z", "+00:00")) - timedelta(seconds=overlap_seconds)
            if watermark else current_time - timedelta(seconds=overlap_seconds)
        )
        window_since = _utc_stamp(lower)
        window_until = _utc_stamp(current_time)
        cursor = None
    httpql = f'req.created_at.gt:"{window_since}" AND req.created_at.lt:"{window_until}"'
    target_store.save_live_sync_progress(
        _LIVE_SOURCE_KEY, watermark=watermark, window_since=window_since,
        window_until=window_until, cursor=cursor, state="running",
    )

    seen = added = pages_completed = 0
    behavior_graph = BehaviorGraph(target_store.behavior_graph_path, project.storage_key)
    try:
        while pages_completed < max_pages_per_cycle:
            metadata_page = source.list_request_page(
                project, cursor=cursor, page_size=page_size, httpql=httpql
            )
            if metadata_page.has_more and (
                not metadata_page.next_cursor or metadata_page.next_cursor == cursor
            ):
                raise ValueError("Caido live-tail cursor did not advance")
            if not metadata_page.has_more and metadata_page.next_cursor is not None:
                raise ValueError("Caido live-tail terminal page unexpectedly has a cursor")
            ids = tuple(row.request_id for row in metadata_page.requests)
            if len(ids) != len(set(ids)):
                raise ValueError("Caido live-tail page contains duplicate request IDs")
            known = target_store.known_live_request_ids(ids)
            page = source.fetch_history_page(
                project, metadata_page, skip_request_ids=known,
                max_body_bytes=max_body_bytes,
            )
            if page.next_cursor != metadata_page.next_cursor or page.has_more != metadata_page.has_more:
                raise ValueError("Caido live-tail request and detail pages disagree")
            if any(exchange.ref.request_id in known for exchange in page.exchanges):
                raise ValueError("Caido returned a request already indexed by this project")
            seen += len(ids)

            if page.exchanges:
                materials = [_live_material(exchange, index_key, response_key) for exchange in page.exchanges]
                lookup_keys = tuple(sorted({
                    str(value)
                    for material in materials
                    for value in (material["request_url_key"], material["referer_url_key"])
                    if value is not None
                }))
                previous = target_store.live_causal_candidates(lookup_keys)
                referer_endpoints = tuple(sorted({
                    value for material in materials
                    for value in (material["referer_endpoint"],)
                    if value is not None
                }))
                historical = target_store.historical_causal_candidates(referer_endpoints)
                by_request_url: dict[str, str] = {}
                by_redirect: dict[str, str] = {}
                by_request_endpoint: dict[tuple[str, int, str], str] = {}
                by_redirect_endpoint: dict[tuple[str, int, str], list[str]] = {}
                for candidate in previous:
                    by_request_url.setdefault(candidate["request_url_key"], candidate["graph_observation_id"])
                    if candidate["redirect_url_key"]:
                        by_redirect.setdefault(candidate["redirect_url_key"], candidate["graph_observation_id"])
                    by_request_endpoint.setdefault(
                        (candidate["host"], candidate["port"], candidate["route"]),
                        candidate["graph_observation_id"],
                    )
                for candidate in historical:
                    endpoint = (candidate["host"], candidate["port"], candidate["route"])
                    by_request_endpoint.setdefault(endpoint, candidate["graph_observation_id"])
                for material in materials:
                    endpoint = material["redirect_endpoint"]
                    if endpoint is not None:
                        by_redirect_endpoint.setdefault(endpoint, []).append(
                            material["exchange"].ref.observation_id
                        )

                causal_predecessors: dict[str, list[tuple[str, str, str]]] = {}
                for material in materials:
                    exchange = material["exchange"]
                    current_observation = exchange.ref.observation_id
                    predecessors: list[tuple[str, str, str]] = []
                    referer_key = material["referer_url_key"]
                    exact_referer = by_request_url.get(referer_key) if referer_key else None
                    if exact_referer and exact_referer != current_observation:
                        predecessors.append((exact_referer, "referrer_context_for", "referrer_exact_url"))
                    else:
                        referer_endpoint = material["referer_endpoint"]
                        route_referer = by_request_endpoint.get(referer_endpoint) if referer_endpoint else None
                        if route_referer and route_referer != current_observation:
                            predecessors.append((route_referer, "referrer_context_for", "referrer_route_template"))
                    request_key = material["request_url_key"]
                    if request_key in by_redirect and by_redirect[request_key] != current_observation:
                        predecessors.append((by_redirect[request_key], "redirects_to", "redirect_exact_url"))
                    else:
                        request_endpoint = material["request_endpoint"]
                        redirect_predecessor = next(
                            (candidate for candidate in by_redirect_endpoint.get(request_endpoint, ())
                             if candidate != current_observation),
                            None,
                        )
                        if redirect_predecessor is not None:
                            predecessors.append((redirect_predecessor, "redirects_to", "redirect_route_template"))
                    if predecessors:
                        causal_predecessors[current_observation] = list(dict.fromkeys(predecessors))
                    by_request_url[str(request_key)] = current_observation
                    by_request_endpoint[request_endpoint] = current_observation
                    if material["redirect_url_key"]:
                        by_redirect[str(material["redirect_url_key"])] = current_observation

                graph_receipt = import_exchanges(
                    behavior_graph,
                    page.exchanges,
                    target_store.behavior_receipt_directory,
                    include_snapshot=False,
                    request_fingerprint_key=index_key,
                    response_fingerprint_key=response_key,
                    causal_predecessors=causal_predecessors,
                )
                graph_exchanges = graph_receipt["exchanges"]
                if len(graph_exchanges) != len(page.exchanges):
                    raise RuntimeError("BehaviorGraph returned an unexpected live page size")
                synced_at = _utc_stamp(datetime.now(timezone.utc))
                records = []
                for material, graph_exchange in zip(materials, graph_exchanges, strict=True):
                    exchange = material["exchange"]
                    if graph_exchange.get("capture_observation_id") != exchange.ref.observation_id:
                        raise RuntimeError("BehaviorGraph changed live page identity or ordering")
                    projection = material["projection"]
                    records.append({
                        "observation_id": exchange.ref.observation_id,
                        "request_id": exchange.ref.request_id,
                        "graph_observation_id": graph_exchange["graph_observation_id"],
                        "synced_at": synced_at,
                        "request_size": len(exchange.request_raw),
                        "response_size": len(exchange.response_raw) if exchange.response_raw is not None else None,
                        "request_signature": material["request_signature"],
                        "response_signature": material["response_signature"],
                        "request_url_key": material["request_url_key"],
                        "referer_url_key": material["referer_url_key"],
                        "redirect_url_key": material["redirect_url_key"],
                        "parse_state": "complete" if graph_exchange["analysis_state"] == "needs_review" else "partial",
                        "search_record": projection.search_record,
                    })
                added += target_store.save_live_graph_captures(records)

            pages_completed += 1
            cursor = metadata_page.next_cursor if metadata_page.has_more else None
            if not metadata_page.has_more:
                completed_watermark = window_until
                polled_at = _utc_stamp(datetime.now(timezone.utc))
                target_store.save_live_sync_progress(
                    _LIVE_SOURCE_KEY, watermark=completed_watermark,
                    window_since=None, window_until=None, cursor=None,
                    state="idle", last_polled_at=polled_at,
                )
                return LiveTailReceipt(project.storage_key, "idle", seen, added, pages_completed, completed_watermark)

            target_store.save_live_sync_progress(
                _LIVE_SOURCE_KEY, watermark=watermark, window_since=window_since,
                window_until=window_until, cursor=cursor, state="partial",
            )

        target_store.save_live_sync_progress(
            _LIVE_SOURCE_KEY, watermark=watermark, window_since=window_since,
            window_until=window_until, cursor=cursor, state="partial",
        )
        return LiveTailReceipt(project.storage_key, "partial", seen, added, pages_completed, watermark, cursor, "page_budget_reached")
    except BaseException as exc:
        try:
            target_store.save_live_sync_progress(
                _LIVE_SOURCE_KEY, watermark=watermark, window_since=window_since,
                window_until=window_until, cursor=cursor, state="partial",
            )
        except Exception:
            pass
        if isinstance(exc, (KeyboardInterrupt, SystemExit)):
            raise
        problem = str(exc) if isinstance(exc, CaidoMCPError) else type(exc).__name__
        return LiveTailReceipt(project.storage_key, "partial", seen, added, pages_completed, watermark, cursor, problem)
    finally:
        behavior_graph.close()


def tail_all_projects(
    source: CaidoReadSource,
    *,
    page_size: int = 100,
    max_pages_per_cycle: int = 5,
    overlap_seconds: int = 90,
    max_body_bytes: int = _LIVE_BODY_MAX_DEFAULT,
    selected_project_keys: frozenset[str] | None = None,
) -> tuple[LiveTailReceipt, ...]:
    projects = tuple(source.list_projects())
    if len({project.storage_key for project in projects}) != len(projects):
        raise ValueError("Caido project listing contains duplicate identities")
    if selected_project_keys is not None:
        available = {project.storage_key for project in projects}
        if not selected_project_keys <= available:
            raise ValueError("A selected project is not currently available in Caido")
        projects = tuple(project for project in projects if project.storage_key in selected_project_keys)
    receipts = []
    for project in projects:
        receipts.append(sync_live_project(
            source, project, page_size=page_size,
            max_pages_per_cycle=max_pages_per_cycle,
            overlap_seconds=overlap_seconds,
            max_body_bytes=max_body_bytes,
        ))
    return tuple(receipts)
