"""Small deterministic graph projection from one Caido HTTP exchange.

The projection stores endpoint/field shapes and provenance, never message
values. Semantic interpretation (ownership, authorization or impact) is outside
this parser and must remain unknown unless separately annotated.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import re
from typing import Iterable
from urllib.parse import parse_qsl, urlsplit

from .domain import HttpExchange
from .storage import ProjectStore


_STATIC_SEGMENTS = {
    "api", "v1", "v2", "v3", "admin", "account", "accounts", "auth",
    "callback", "cart", "checkout", "config", "dashboard", "delete",
    "document", "documents", "download", "edit", "export", "file", "files",
    "graphql", "health", "home", "invite", "item", "items", "list", "login",
    "logout", "me", "order", "orders", "password", "profile", "register",
    "reset", "search", "session", "settings", "share", "status", "submit",
    "tenant", "tenants", "token", "update", "upload", "user", "users", "verify",
}
_FIELD_NAME = re.compile(r"^[A-Za-z_$][A-Za-z0-9_$.-]{0,63}$")
_SECRET_NAME = re.compile(r"(?i)(?:token|secret|password|passwd|cookie|authorization|session|api[_-]?key|credential|email|phone|ssn)")
_HTTP_METHOD = re.compile(r"^[A-Z]{1,16}$")
_STATUS = re.compile(r"^HTTP/\d(?:\.\d)?\s+(\d{3})(?:\s|$)", re.I)


@dataclass(frozen=True, slots=True)
class ExchangeProjection:
    observation_id: str
    parse_state: str
    search_record: dict


def _route_template(target: str | None) -> str:
    raw_path = (target or "/").split("?", 1)[0]
    try:
        path = urlsplit(raw_path).path or "/"
    except (ValueError, UnicodeError):
        path = "/"
    output: list[str] = []
    for segment in path.split("/"):
        decoded = segment.lower()
        if not segment:
            output.append("")
        elif decoded in _STATIC_SEGMENTS or re.fullmatch(r"v[0-9]{1,2}", decoded):
            output.append(decoded)
        else:
            output.append("{value}")
    return "/".join(output)[:2048]


def _safe_field(name: str) -> str:
    if not _FIELD_NAME.fullmatch(name) or _SECRET_NAME.search(name):
        return "field_" + hashlib.sha256(name.encode("utf-8", errors="replace")).hexdigest()[:16]
    return name


def _request_line(exchange: HttpExchange) -> tuple[str | None, str | None]:
    first_line = exchange.request_raw.split(b"\n", 1)[0][:4096].decode("latin-1", errors="replace").strip("\r")
    parts = first_line.split()
    if len(parts) >= 2:
        method = parts[0].upper()
        target = parts[1]
        return (method if _HTTP_METHOD.fullmatch(method) else None), target
    return None, None


def _header_names(raw: bytes) -> tuple[list[str], str, bool]:
    boundary = raw.find(b"\r\n\r\n")
    delimiter = 4
    if boundary < 0:
        boundary = raw.find(b"\n\n")
        delimiter = 2
    if boundary < 0:
        return [], "", False
    header_block = raw[:boundary]
    body = raw[boundary + delimiter:]
    names: list[str] = []
    lines = header_block.splitlines()
    complete = len(lines) <= 257
    for line in lines[1:257]:
        if b":" not in line:
            continue
        name = line.split(b":", 1)[0].decode("ascii", errors="ignore").strip().lower()
        if re.fullmatch(r"[a-z0-9!#$%&'*+.^_`|~-]{1,128}", name):
            names.append(name)
    return names, body, complete


def _content_type(raw: bytes) -> str:
    for line in raw.splitlines()[1:257]:
        if line[:13].lower() == b"content-type:":
            candidate = line.split(b":", 1)[1].decode("latin-1", errors="replace").strip().split(";", 1)[0].lower()
            if re.fullmatch(r"[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+", candidate):
                return candidate
            return "unknown"
    return "unknown"


def _status_code(raw: bytes | None) -> int | None:
    if not raw:
        return None
    status_line = raw.split(b"\n", 1)[0][:256].decode("ascii", errors="ignore").strip("\r")
    match = _STATUS.match(status_line)
    return int(match.group(1)) if match else None


def _body_field_paths(body: bytes, content_type: str, *, max_bytes: int = 2 * 1024 * 1024) -> tuple[set[str], bool]:
    if len(body) > max_bytes:
        return set(), False
    media_type = content_type.split(";", 1)[0].strip().lower()
    if media_type != "application/json" and not (media_type.startswith("application/") and media_type.endswith("+json")):
        if media_type == "application/x-www-form-urlencoded":
            try:
                pairs = parse_qsl(body.decode("utf-8", errors="strict"), keep_blank_values=True, max_num_fields=5000)
                return {_safe_field(key) for key, _value in pairs}, True
            except (UnicodeError, ValueError):
                return set(), False
        return set(), not body
    try:
        document = json.loads(body.decode("utf-8", errors="strict"))
    except (UnicodeError, json.JSONDecodeError):
        return set(), False
    fields: set[str] = set()
    pending: list[tuple[object, str, int]] = [(document, "$", 0)]
    count = 0
    while pending:
        value, prefix, depth = pending.pop()
        if depth > 32 or count > 5000:
            return fields, False
        if isinstance(value, dict):
            for key, child in value.items():
                count += 1
                safe = _safe_field(str(key))
                path = f"{prefix}.{safe}"
                fields.add(path[:512])
                pending.append((child, path, depth + 1))
        elif isinstance(value, list):
            count += len(value)
            if count > 5000:
                return fields, False
            for child in value:
                pending.append((child, prefix + "[]", depth + 1))
    return fields, True


def project_exchange(exchange: HttpExchange) -> ExchangeProjection:
    fallback_method, line_target = _request_line(exchange)
    method = (exchange.method or fallback_method or "UNKNOWN").upper()
    if not _HTTP_METHOD.fullmatch(method):
        method = "UNKNOWN"
    target = exchange.target or line_target or "/"
    try:
        parsed = urlsplit(target)
        target_port = parsed.port
    except ValueError:
        parsed = urlsplit("/")
        target_port = None
    host = (exchange.host or parsed.hostname or "unknown").lower()
    if not re.fullmatch(r"[A-Za-z0-9.-]{1,253}", host):
        host = "unknown"
    scheme = (exchange.scheme or parsed.scheme or "unknown").lower()
    if scheme not in {"http", "https"}:
        scheme = "unknown"
    port = exchange.port or target_port or (443 if scheme == "https" else 80 if scheme == "http" else 0)
    if type(port) is not int or not (1 <= port <= 65535):
        port = 0
    route = _route_template(target)
    query_names: set[str] = set()
    parse_complete = True
    if parsed.query:
        try:
            query_names = {_safe_field(name) for name, _value in parse_qsl(parsed.query, keep_blank_values=True, max_num_fields=5000)}
        except ValueError:
            parse_complete = False

    req_header_names, req_body, req_boundary = _header_names(exchange.request_raw)
    req_content_type = _content_type(exchange.request_raw)
    req_body_fields, req_body_complete = _body_field_paths(req_body, req_content_type) if req_boundary else (set(), False)
    req_fields = query_names | req_body_fields

    response_header_names: list[str] = []
    response_body_fields: set[str] = set()
    response_content_type = "unknown"
    response_boundary = False
    response_headers_complete = False
    response_body_complete = False
    if exchange.response_raw is not None:
        response_header_names, response_body, response_headers_complete = _header_names(exchange.response_raw)
        response_boundary = b"\r\n\r\n" in exchange.response_raw or b"\n\n" in exchange.response_raw
        response_content_type = _content_type(exchange.response_raw)
        response_body_fields, response_body_complete = _body_field_paths(response_body, response_content_type) if response_boundary else (set(), False)
    status = exchange.response_status or _status_code(exchange.response_raw)
    parse_complete = (
        parse_complete and req_boundary and req_body_complete and response_boundary and
        response_headers_complete and response_body_complete and status is not None and
        exchange.request_truncated is False and exchange.response_truncated is False
    )

    observation_id = exchange.ref.observation_id
    search_record = {
        "method": method, "host": host, "port": port, "route": route,
        "response_status": status, "request_content_type": req_content_type,
        "response_content_type": response_content_type,
        "field_names": [f"req:{name}" for name in sorted(req_fields)] + [f"resp:{name}" for name in sorted(response_body_fields)],
        "header_names": sorted({_safe_field(name) for name in req_header_names + response_header_names}),
        "captured_at": exchange.captured_at,
    }
    return ExchangeProjection(observation_id, "complete" if parse_complete else "partial", search_record)


def store_projection(
    store: ProjectStore,
    projection: ExchangeProjection,
    *,
    analysis_state: str | None = None,
) -> None:
    store_projections(store, [(projection, analysis_state)])


def store_projections(
    store: ProjectStore,
    projections: Iterable[tuple[ExchangeProjection, str | None]],
) -> None:
    """Update a page of the denormalized capture search index atomically.

    The authoritative behavior graph is stored by the vendored, provenance-
    checked graph package. This table is only a fast structural lookup index.
    """
    selected = list(projections)
    if len(selected) > 100:
        raise ValueError("Projection batch exceeds the 100-exchange import limit")
    for _projection, analysis_state in selected:
        if analysis_state is not None and analysis_state not in {"needs_review", "incomplete"}:
            raise ValueError("Unsupported behavior analysis state")
    with store.connect() as connection:
        for projection, analysis_state in selected:
            parse_state = (
                "complete" if analysis_state == "needs_review"
                else "partial" if analysis_state == "incomplete"
                else projection.parse_state
            )
            connection.execute(
                "UPDATE captures SET parse_state=? WHERE observation_id=?",
                (parse_state, projection.observation_id),
            )
            search = projection.search_record
            fields_text = " ".join(search["field_names"])
            headers_text = " ".join(search["header_names"])
            connection.execute(
                """INSERT INTO capture_search(
                     observation_id,method,host,port,route,response_status,request_content_type,response_content_type,
                     field_names_json,header_names_json,captured_at
                   ) VALUES(?,?,?,?,?,?,?,?,?,?,?)
                   ON CONFLICT(observation_id) DO UPDATE SET
                     method=excluded.method,host=excluded.host,port=excluded.port,route=excluded.route,
                     response_status=excluded.response_status,request_content_type=excluded.request_content_type,
                     response_content_type=excluded.response_content_type,
                     field_names_json=excluded.field_names_json,header_names_json=excluded.header_names_json,
                     captured_at=excluded.captured_at""",
                (
                    projection.observation_id, search["method"], search["host"], search["port"], search["route"],
                    search["response_status"], search["request_content_type"], search["response_content_type"],
                    json.dumps(search["field_names"], separators=(",", ":")),
                    json.dumps(search["header_names"], separators=(",", ":")), search["captured_at"],
                ),
            )
            connection.execute("DELETE FROM capture_search_fts WHERE observation_id=?", (projection.observation_id,))
            connection.execute(
                "INSERT INTO capture_search_fts(observation_id,method,host,route,fields,headers) VALUES(?,?,?,?,?,?)",
                (projection.observation_id, search["method"], search["host"], search["route"], fields_text, headers_text),
            )
