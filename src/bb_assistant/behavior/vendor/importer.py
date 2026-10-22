"""Bounded, local capture parsing and redacted behavior-graph proposals.

Parsing never executes a request. Values remain in memory: public output contains
field shape, opaque metadata aliases, observations, and unexecuted check plans.
Complete parsing still requires semantic review; it is not a security conclusion.
"""
from __future__ import annotations

import base64
import binascii
from collections import Counter
from email import policy
from email.parser import BytesParser
import hashlib
from html.parser import HTMLParser
import json
from pathlib import Path
import re
import struct
from typing import Any
from urllib.parse import parse_qsl, unquote, urlsplit
import zlib

from .common import BehaviorError, entity_id
from .control import assert_public

_TOKEN = re.compile(r"^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
_NAME = re.compile(r"^[A-Za-z_$][A-Za-z0-9_$.-]{0,63}$")
_MEDIA = re.compile(r"^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$")
_RISK = re.compile(r"(?i)(?:bearer\s|eyJ[A-Za-z0-9_-]{8,}|password\s*[=:]|secret\s*[=:]|token\s*[=:]|api[_-]?key\s*[=:]|@|-----BEGIN|[a-f0-9]{24,}|[A-Za-z0-9_-]{32,})")
_LABEL = re.compile(r"^(?:[A-Z]|[ABab][0-9]?|[tv][0-9]{1,4}|[0-9]{1,4}|[A-Z][_-](?:v[0-9]{1,4}|(?:doc|document|object|item)[0-9]{0,4})|(?:actor|user|owner|tenant|session|object|doc|document|item|capture|cap|test|role)[_-]?[A-Za-z0-9]{1,12}|flow-[a-f0-9]{12}|admin|user|guest|anonymous|member|private|public|shared|owner_only|tenant_only|restricted|unknown)$")
_CONTEXT = ("actor", "session_version", "role", "tenant", "object", "object_owner", "access_policy", "policy_source")
_OBJECT_COLLECTIONS = {"account", "accounts", "address", "addresses", "card", "cards", "document", "documents", "item", "items", "order", "orders", "product", "products", "track-order", "user", "users"}
_VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr"}
_ROUTE_WORDS = {"api", "v1", "v2", "v3", "admin", "account", "accounts", "auth", "authorize", "callback", "cart", "checkout", "config", "dashboard", "delete", "doc", "docs", "document", "documents", "download", "edit", "export", "file", "files", "graphql", "health", "home", "index", "invite", "item", "items", "list", "login", "logout", "me", "object", "objects", "order", "orders", "password", "profile", "read", "register", "reset", "search", "session", "sessions", "settings", "share", "shared", "private", "public", "status", "submit", "tenant", "tenants", "token", "update", "upload", "user", "users", "verify", "approve", "payment", "payments", "report", "reports", "rest", "service", "services"}
_SAFE_MEDIA = {"application/json", "application/x-www-form-urlencoded", "multipart/form-data", "text/html", "application/xhtml+xml", "text/plain", "application/octet-stream", "image/png", "image/jpeg", "application/pdf"}
_RAW_BLOCKERS = {"raw_capture_missing", "raw_message_limit", "invalid_raw_base64", "raw_headers_or_body_missing", "invalid_raw_headers", "invalid_raw_message", "invalid_request_line", "caido_origin_missing", "inconsistent_status", "invalid_response_line", "input_truncated", "truncation_state_unknown"}
_CARD_KEYS = {"schema_version", "id", "capture_id", "program", "source_refs", "endpoint", "context", "request_analysis", "response_analysis", "business_action", "hypotheses", "missing_evidence", "analysis_state"}


def _media_metadata(value: str) -> str:
    if value in _SAFE_MEDIA:
        return value
    if value.startswith("application/") and value.endswith("+json"):
        return "application/*+json"
    return "unknown"


def _limits(max_message_bytes: int, max_fields: int, max_depth: int) -> dict:
    limits = {"max_message_bytes": max_message_bytes, "max_fields": max_fields, "max_depth": max_depth}
    ceilings = {"max_message_bytes": 64 * 1024 * 1024, "max_fields": 100000, "max_depth": 256}
    if any(type(value) is not int or value < 1 or value > ceilings[key] for key, value in limits.items()):
        raise BehaviorError("Invalid analysis resource limits")
    return limits


def _program(program: str) -> None:
    if not isinstance(program, str) or not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}", program):
        raise BehaviorError("Invalid program namespace")


def _reference(ref: dict, *, verify: bool = False) -> dict:
    if not isinstance(ref, dict) or set(ref) != {"path", "sha256", "record_locator"}:
        raise BehaviorError("Invalid source reference")
    if not isinstance(ref["path"], str) or not Path(ref["path"]).is_absolute() or not isinstance(ref["record_locator"], str) or not ref["record_locator"] or not isinstance(ref["sha256"], str) or not re.fullmatch(r"[a-f0-9]{64}", ref["sha256"]):
        raise BehaviorError("Invalid source reference")
    try:
        assert_public(ref)
        if re.search(r"(?i)(?:@|bearer\s|(?:password|secret|token|api[_-]?key)\s*[=:])", ref["path"] + ref["record_locator"]):
            raise ValueError
    except ValueError as exc:
        raise BehaviorError("Unsafe source metadata") from exc
    if verify:
        try:
            selected = Path(ref["path"])
            if not selected.is_file() or selected.resolve() != selected:
                raise BehaviorError("Source must be a canonical regular file")
            with selected.open("rb") as stream:
                actual = hashlib.file_digest(stream, "sha256").hexdigest()
        except OSError as exc:
            raise BehaviorError("Source is unavailable") from exc
        if actual != ref["sha256"]:
            raise BehaviorError("Source integrity mismatch")
    return dict(ref)


def _opaque(ref: dict, location: str, prefix: str = "metadata") -> str:
    # Hash provenance, never a low-entropy secret value. Replay is deterministic.
    return prefix + "_" + entity_id("alias", {"source": ref, "location": location}).split("-", 1)[1][:16]


def _safe_name(value: Any, ref: dict, location: str) -> str:
    if isinstance(value, str) and _NAME.fullmatch(value) and not _RISK.search(value):
        return value
    return _opaque(ref, location, "field")


def _safe_label(value: Any, ref: dict, location: str) -> str:
    if value is None or value == "" or value == "unknown":
        return "unknown"
    if isinstance(value, str) and _LABEL.fullmatch(value) and not _RISK.search(value):
        return value
    return _opaque(ref, location)


def _url(value: Any, ref: dict, location: str) -> tuple[dict, list[tuple[str, str]], list[str]]:
    blockers: list[str] = []
    if not isinstance(value, str) or not value or len(value.encode("utf-8", errors="replace")) > 65536:
        return {"origin": "unknown", "path": "unknown", "query_names": []}, [], ["invalid_or_oversized_url"]
    try:
        parsed = urlsplit(value)
        host = parsed.hostname
        port = parsed.port
    except ValueError:
        return {"origin": "unknown", "path": "unknown", "query_names": []}, [], ["invalid_url"]
    if parsed.scheme not in ("http", "https") or not host:
        blockers.append("missing_http_origin")
        origin = "unknown"
    else:
        host = host.lower()
        if not re.fullmatch(r"[A-Za-z0-9.-]+", host) or _RISK.search(host):
            host = _opaque(ref, location + ".host", "host") + ".invalid"
        default = 443 if parsed.scheme == "https" else 80
        origin = parsed.scheme + "://" + host + (":" + str(port) if port and port != default else "")
    parts = []
    path_segments = (parsed.path or "/").split("/")
    if len(path_segments) > 4096:
        return {"origin": origin, "path": "unknown", "query_names": []}, [], ["url_path_limit"]
    for index, segment in enumerate(path_segments):
        decoded = unquote(segment)
        if not segment:
            parts.append("")
        elif _NAME.fullmatch(decoded) and not _RISK.search(decoded) and (decoded.lower().split(".", 1)[0] in _ROUTE_WORDS or re.fullmatch(r"[A-Za-z]", decoded)):
            parts.append(decoded)
        else:
            parts.append(_opaque(ref, location + ".segment[" + str(index) + "]", "segment"))
    path = "/".join(parts)
    try:
        pairs = parse_qsl(parsed.query, keep_blank_values=True, encoding="utf-8", errors="strict", max_num_fields=100001)
    except (ValueError, UnicodeError):
        pairs = []
        blockers.append("invalid_or_oversized_query")
    names = [_safe_name(name, ref, location + ".query[" + str(index) + "]") for index, (name, _value) in enumerate(pairs)]
    return {"origin": origin, "path": path, "query_names": sorted(set(names))}, list(zip(names, ["string"] * len(names))), blockers


def _path_object_signal(value: Any) -> bool:
    """Recognize a likely object selector without retaining its value."""
    if not isinstance(value, str):
        return False
    try:
        segments = [unquote(item).lower() for item in urlsplit(value).path.split("/") if item]
    except ValueError:
        return False
    for parent, child in zip(segments, segments[1:]):
        if parent in _OBJECT_COLLECTIONS and child not in _ROUTE_WORDS and not re.search(r"\.[a-z0-9]{1,8}$", child):
            return True
    return False


def _graph_path_template(path: Any) -> Any:
    """Aggregate repeated array/query occurrences in the graph, not in evidence cards."""
    if not isinstance(path, str):
        return path
    return re.sub(r"\[(\d+)\]", "[]", path)


class _StopFields(Exception):
    pass


class _Fields:
    def __init__(self, limit: int, blockers: list[str]):
        self.limit = limit
        self.blockers = blockers
        self.items: list[dict] = []

    def add(self, path: str, kind: str, location: str, **metadata: Any) -> None:
        if len(self.items) >= self.limit:
            if "field_limit" not in self.blockers:
                self.blockers.append("field_limit")
            raise _StopFields
        self.items.append({"path": path, "type": kind, "location": location, **metadata})


def _headers(raw: Any, ref: dict, location: str, limit: int) -> tuple[list[tuple[str, str]], list[dict], list[str], int]:
    if raw is None:
        return [], [], ["headers_missing"], 0
    if isinstance(raw, dict):
        entries = raw.items()
    elif isinstance(raw, list):
        entries = raw
    else:
        return [], [], ["invalid_headers"], 0
    headers: list[tuple[str, str]] = []
    names: Counter = Counter()
    blockers = []
    size = 0
    for index, item in enumerate(entries):
        if index >= limit:
            blockers.append("header_limit")
            break
        if isinstance(item, dict) and set(item) >= {"name", "value"}:
            name, value = item["name"], item["value"]
        elif isinstance(item, (list, tuple)) and len(item) == 2:
            name, value = item
        else:
            blockers.append("invalid_header_entry")
            continue
        values = value if isinstance(value, list) else [value]
        for occurrence, child in enumerate(values):
            if not isinstance(name, str) or not isinstance(child, str) or not _TOKEN.fullmatch(name) or "\n" in child or "\r" in child:
                blockers.append("invalid_header_entry")
                continue
            if len(headers) >= limit:
                blockers.append("header_limit")
                break
            try:
                size += len(name.encode("utf-8")) + len(child.encode("utf-8")) + 4
            except UnicodeError:
                blockers.append("invalid_header_entry")
                continue
            headers.append((name.lower(), child))
            safe = _safe_name(name.lower(), ref, location + "[" + str(index) + "," + str(occurrence) + "]")
            names[safe] += 1
    return headers, [{"name": name, "count": count} for name, count in sorted(names.items())], list(dict.fromkeys(blockers)), size


def _header_value(headers: list[tuple[str, str]], name: str, blockers: list[str]) -> str:
    values = [value.strip() for key, value in headers if key == name]
    if len(set(values)) > 1:
        blockers.append("conflicting_" + name.replace("-", "_"))
    return values[0] if values else ""


def _raw_message(encoded: Any, limit: int) -> tuple[dict, list[str]]:
    if encoded is None:
        return {}, ["raw_capture_missing"]
    if not isinstance(encoded, str) or len(encoded) > ((limit + 2) // 3) * 4:
        return {}, ["raw_message_limit"]
    try:
        data = base64.b64decode(encoded, validate=True)
    except (ValueError, binascii.Error):
        return {}, ["invalid_raw_base64"]
    if len(data) > limit:
        return {}, ["raw_message_limit"]
    separator = b"\r\n\r\n" if b"\r\n\r\n" in data else b"\n\n"
    if separator not in data:
        return {}, ["raw_headers_or_body_missing"]
    head, body = data.split(separator, 1)
    lines = head.splitlines()
    try:
        first = lines[0].decode("ascii")
        headers = []
        for line in lines[1:]:
            if not line or line[:1] in (b" ", b"\t") or b":" not in line:
                return {}, ["invalid_raw_headers"]
            name, value = line.split(b":", 1)
            headers.append((name.decode("ascii"), value.strip().decode("latin-1")))
    except (UnicodeError, IndexError):
        return {}, ["invalid_raw_message"]
    return {"first_line": first, "headers": headers, "body": body, "message_bytes": len(data)}, []


def _caido(node: dict, limit: int) -> dict:
    request_raw, request_blockers = _raw_message(node.get("raw"), limit)
    if node.get("request_truncated") is True:
        request_blockers.append("input_truncated")
    elif "request_truncated" in node and node.get("request_truncated") is None:
        request_blockers.append("truncation_state_unknown")
    elif "request_truncated" in node and type(node.get("request_truncated")) is not bool:
        request_blockers.append("truncation_state_unknown")
    response_node = node.get("response")
    if not isinstance(response_node, dict):
        response_node = {}
    response_raw, response_blockers = _raw_message(response_node.get("raw"), limit)
    if response_node.get("truncated") is True:
        response_blockers.append("input_truncated")
    elif "truncated" in response_node and response_node.get("truncated") is None:
        response_blockers.append("truncation_state_unknown")
    elif "truncated" in response_node and type(response_node.get("truncated")) is not bool:
        response_blockers.append("truncation_state_unknown")
    method = node.get("method")
    target = str(node.get("path", "/"))
    if node.get("query"):
        target += "?" + str(node["query"]).lstrip("?")
    if request_raw:
        match = re.fullmatch(r"([A-Z]+) ([^ ]+) HTTP/\d(?:\.\d)?", request_raw["first_line"])
        if match:
            method, target = match.groups()
        else:
            request_blockers.append("invalid_request_line")
    host = node.get("host")
    port = node.get("port")
    if isinstance(host, str) and host and isinstance(port, int) and not isinstance(port, bool) and 0 < port < 65536:
        base = ("https" if node.get("isTls") is True else "http") + "://" + host + ":" + str(port)
        url = target if target.startswith(("https://", "http://")) else base + (target if target.startswith("/") else "/" + target)
    else:
        url = target if target.startswith(("https://", "http://")) else ""
        request_blockers.append("caido_origin_missing")
    status = response_node.get("statusCode")
    if response_raw:
        match = re.match(r"^HTTP/\d(?:\.\d)? ([0-9]{3})(?: |$)", response_raw["first_line"])
        if match:
            parsed_status = int(match.group(1))
            if status is not None and status != parsed_status:
                response_blockers.append("inconsistent_status")
            status = parsed_status
        else:
            response_blockers.append("invalid_response_line")
    request = {"method": method, "url": url, "headers": request_raw.get("headers"), "_blockers": request_blockers, "_raw": True}
    response = {"status": status, "headers": response_raw.get("headers"), "_blockers": response_blockers, "_raw": True}
    if node.get("truncation_reason") == "message_limit":
        request_blockers.append("raw_message_limit")
    if response_node.get("truncation_reason") == "message_limit":
        response_blockers.append("raw_message_limit")
    for raw, side in ((request_raw, request), (response_raw, response)):
        if raw:
            side["body"] = raw["body"]
            side["_message_bytes"] = raw["message_bytes"]
    for name in ("truncated", "body_truncated", "input_truncated"):
        if response_node.get(name) is True:
            response[name] = True
    result = {"id": node.get("id"), "observed_at": node.get("observed_at"), "request": request, "response": response, "context": node.get("context", {})}
    for name in ("truncated", "body_truncated", "input_truncated"):
        if node.get(name) is True:
            result[name] = True
    return result


def _unchunk(data: bytes, limit: int) -> tuple[bytes, list[tuple[str, str]]]:
    output = bytearray()
    position = 0
    while True:
        end = data.find(b"\r\n", position)
        if end < 0 or end - position > 8192:
            raise BehaviorError("Invalid chunked transfer")
        size_text = data[position:end].split(b";", 1)[0]
        if not re.fullmatch(rb"[0-9A-Fa-f]{1,16}", size_text):
            raise BehaviorError("Invalid chunked transfer")
        size = int(size_text, 16)
        position = end + 2
        if size == 0:
            trailer_data = data[position:]
            if trailer_data == b"\r\n":
                return bytes(output), []
            if not trailer_data.endswith(b"\r\n\r\n"):
                raise BehaviorError("Invalid chunked trailers")
            trailers = []
            try:
                for line in trailer_data[:-4].split(b"\r\n"):
                    name, value = line.split(b":", 1)
                    trailers.append((name.decode("ascii"), value.strip().decode("latin-1")))
            except (ValueError, UnicodeError):
                raise BehaviorError("Invalid chunked trailers") from None
            return bytes(output), trailers
        if size > limit - len(output):
            raise BehaviorError("Decoded message limit")
        if position + size + 2 > len(data) or data[position + size:position + size + 2] != b"\r\n":
            raise BehaviorError("Invalid chunked transfer")
        output.extend(data[position:position + size])
        position += size + 2


def _transport(body: bytes, headers: list[tuple[str, str]], blockers: list[str], limit: int) -> tuple[bytes | None, list[tuple[str, str]]]:
    trailers: list[tuple[str, str]] = []
    transfer = _header_value(headers, "transfer-encoding", blockers).lower()
    content = _header_value(headers, "content-encoding", blockers).lower()
    if transfer not in ("", "identity", "chunked"):
        blockers.append("unsupported_transfer_encoding")
        return None, trailers
    if transfer == "chunked":
        try:
            body, trailers = _unchunk(body, limit)
        except BehaviorError:
            blockers.append("invalid_or_oversized_chunked_body")
            return None, trailers
    if content not in ("", "identity", "gzip"):
        blockers.append("unsupported_content_encoding")
        return None, trailers
    if content == "gzip":
        try:
            decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
            output = decoder.decompress(body, limit + 1)
            if len(output) > limit or decoder.unconsumed_tail or decoder.unused_data or not decoder.eof:
                blockers.append("invalid_or_oversized_gzip_body")
                return None, trailers
            body = output
        except zlib.error:
            blockers.append("invalid_gzip_body")
            return None, trailers
    if len(body) > limit:
        blockers.append("decoded_message_limit")
        return None, trailers
    return body, trailers


def _png_metadata(body: bytes) -> dict[str, int | str] | None:
    """Validate PNG chunk framing/CRCs and return only safe IHDR metadata."""
    if not body.startswith(b"\x89PNG\r\n\x1a\n"):
        return None
    position = 8
    chunk_count = 0
    seen_ihdr = seen_idat = seen_iend = False
    metadata: dict[str, int | str] = {}
    while position < len(body):
        if position + 12 > len(body):
            return None
        length = int.from_bytes(body[position:position + 4], "big")
        kind = body[position + 4:position + 8]
        data_start = position + 8
        data_end = data_start + length
        chunk_end = data_end + 4
        if (not re.fullmatch(rb"[A-Za-z]{4}", kind) or chunk_end > len(body)
                or seen_iend):
            return None
        data = body[data_start:data_end]
        expected_crc = int.from_bytes(body[data_end:chunk_end], "big")
        if zlib.crc32(kind + data) & 0xFFFFFFFF != expected_crc:
            return None
        if chunk_count == 0 and kind != b"IHDR":
            return None
        if kind == b"IHDR":
            if seen_ihdr or chunk_count != 0 or length != 13:
                return None
            width, height, depth, color, compression, filtering, interlace = struct.unpack(">IIBBBBB", data)
            allowed_depths = {0: {1, 2, 4, 8, 16}, 2: {8, 16}, 3: {1, 2, 4, 8},
                              4: {8, 16}, 6: {8, 16}}
            if (not width or not height or color not in allowed_depths
                    or depth not in allowed_depths[color] or compression != 0
                    or filtering != 0 or interlace not in {0, 1}):
                return None
            metadata = {"format": "png", "width": width, "height": height, "bit_depth": depth,
                        "color_type": color, "interlace": interlace}
            seen_ihdr = True
        elif kind == b"IDAT":
            if not seen_ihdr or seen_iend:
                return None
            seen_idat = True
        elif kind == b"IEND":
            if length != 0 or not seen_ihdr or not seen_idat:
                return None
            seen_iend = True
        chunk_count += 1
        position = chunk_end
        if seen_iend:
            if position != len(body):
                return None
            break
    if not (seen_ihdr and seen_idat and seen_iend):
        return None
    return {"chunk_count": chunk_count, **metadata}


def _json_fields(text: str, fields: _Fields, ref: dict, location: str, max_depth: int) -> None:
    # Bound container allocation before json.loads. Count nodes lexically while
    # skipping quoted values; commas/braces inside strings do not affect limits.
    index = nodes = nesting = 0
    while index < len(text):
        char = text[index]
        if char.isspace() or char in ",:":
            index += 1
            continue
        depth = nesting
        if char in "[{":
            nesting += 1
            nodes += 1
        elif char in "]}":
            nesting -= 1
            index += 1
            continue
        elif char == '"':
            index += 1
            while index < len(text):
                if text[index] == "\\":
                    index += 2
                elif text[index] == '"':
                    break
                else:
                    index += 1
            after = index + 1
            while after < len(text) and text[after].isspace():
                after += 1
            if after < len(text) and text[after] == ":":
                index += 1
                continue
            nodes += 1
        else:
            while index + 1 < len(text) and not text[index + 1].isspace() and text[index + 1] not in ",:]}{[":
                index += 1
            nodes += 1
        if depth > max_depth:
            fields.blockers.append("depth_limit")
            return
        if nodes > fields.limit - len(fields.items):
            fields.blockers.append("field_limit")
            return
        index += 1
    def pairs(entries: list) -> dict:
        result = {}
        for name, value in entries:
            if name in result:
                raise ValueError
            result[name] = value
        return result

    def nonfinite(_value: str) -> None:
        raise ValueError

    try:
        document = json.loads(text, object_pairs_hook=pairs, parse_constant=nonfinite)
    except (ValueError, RecursionError):
        fields.blockers.append("invalid_json")
        return
    stack = [("$", document, 0)]
    while stack:
        path, item, depth = stack.pop()
        if depth > max_depth:
            fields.blockers.append("depth_limit")
            continue
        kind = "object" if isinstance(item, dict) else "array" if isinstance(item, list) else "null" if item is None else "boolean" if isinstance(item, bool) else "number" if isinstance(item, (int, float)) else "string"
        fields.add(path, kind, location)
        if isinstance(item, dict):
            children = list(item.items())
            for index in range(len(children) - 1, -1, -1):
                name, value = children[index]
                safe = _safe_name(name, ref, location + path + ".key[" + str(index) + "]")
                stack.append((path + "[" + json.dumps(safe, ensure_ascii=True) + "]", value, depth + 1))
        elif isinstance(item, list):
            for index in range(len(item) - 1, -1, -1):
                stack.append((path + "[" + str(index) + "]", item[index], depth + 1))


def _pairs_fields(text: str, fields: _Fields, ref: dict, location: str) -> None:
    try:
        pairs = parse_qsl(text, keep_blank_values=True, encoding="utf-8", errors="strict", max_num_fields=fields.limit + 1)
    except (ValueError, UnicodeError):
        fields.blockers.append("invalid_or_oversized_form_fields")
        return
    counts: Counter = Counter()
    for index, (name, _value) in enumerate(pairs):
        safe = _safe_name(name, ref, location + ".key[" + str(index) + "]")
        occurrence = counts[safe]
        counts[safe] += 1
        fields.add(location + "[" + json.dumps(safe) + "][" + str(occurrence) + "]", "string", location)


class _HTML(HTMLParser):
    def __init__(self, fields: _Fields, ref: dict, location: str, max_depth: int):
        super().__init__(convert_charrefs=True)
        self.fields, self.ref, self.location, self.max_depth = fields, ref, location, max_depth
        self.forms: list[dict] = []
        self.links: list[dict] = []
        self.inputs = 0
        self.password = False
        self.stack: list[str] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag not in _VOID:
            if len(self.stack) >= self.max_depth:
                self.fields.blockers.append("depth_limit")
                raise _StopFields
            self.stack.append(tag)
        attributes = dict(attrs)
        if tag == "form":
            index = len(self.forms)
            action, _pairs, blockers = _url(attributes.get("action", "/"), self.ref, self.location + ".form[" + str(index) + "]")
            # Relative HTML actions are expected. Origin remains explicitly unknown.
            self.fields.blockers.extend(item for item in blockers if item != "missing_http_origin")
            method = (attributes.get("method") or "GET").upper()
            method = method if re.fullmatch(r"[A-Z]{1,16}", method) else "UNKNOWN"
            self.fields.add("html.forms[" + str(index) + "]", "form", self.location)
            self.forms.append({"action": action, "method": method})
        if tag in {"input", "select", "textarea", "button"}:
            index = self.inputs
            self.inputs += 1
            name = _safe_name(attributes.get("name", "unnamed"), self.ref, self.location + ".input[" + str(index) + "]")
            input_type = (attributes.get("type") or tag).lower()
            input_type = input_type if input_type in {"text", "password", "hidden", "file", "email", "number", "submit", "checkbox", "radio", "input", "select", "textarea", "button", "url", "search", "date", "tel"} else "other"
            self.password |= input_type == "password"
            self.fields.add("html.inputs[" + str(index) + "][" + json.dumps(name) + "]", "input:" + input_type, self.location)
        if tag in {"a", "link"} and "href" in attributes:
            index = len(self.links)
            url, _pairs, blockers = _url(attributes["href"], self.ref, self.location + ".link[" + str(index) + "]")
            self.fields.blockers.extend(item for item in blockers if item != "missing_http_origin")
            self.fields.add("html.links[" + str(index) + "]", "link", self.location)
            self.links.append(url)

    def handle_endtag(self, tag: str) -> None:
        if tag in self.stack:
            index = len(self.stack) - 1 - self.stack[::-1].index(tag)
            del self.stack[index:]

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self.handle_starttag(tag, attrs)
        if tag not in _VOID:
            self.handle_endtag(tag)


def _multipart(body: bytes, content_type: str, fields: _Fields, ref: dict, location: str, max_depth: int) -> None:
    try:
        message = BytesParser(policy=policy.default).parsebytes(b"Content-Type: " + content_type.encode("latin-1") + b"\r\nMIME-Version: 1.0\r\n\r\n" + body)
        if not message.is_multipart() or message.defects:
            fields.blockers.append("invalid_multipart")
            return
        stack = [(part, 1) for part in reversed(list(message.iter_parts()))]
        index = 0
        while stack:
            part, depth = stack.pop()
            if depth > max_depth:
                fields.blockers.append("depth_limit")
                continue
            if part.defects:
                fields.blockers.append("invalid_multipart_part")
            if part.is_multipart():
                stack.extend((child, depth + 1) for child in reversed(list(part.iter_parts())))
                continue
            raw_name = part.get_param("name", header="content-disposition")
            if not isinstance(raw_name, str):
                fields.blockers.append("multipart_field_name_missing")
            name = _safe_name(raw_name, ref, location + ".part[" + str(index) + "]")
            filename = part.get_filename()
            transfer_encoding = str(part.get("Content-Transfer-Encoding", "")).lower().strip()
            if transfer_encoding not in {"", "7bit", "8bit", "binary", "base64", "quoted-printable"}:
                fields.blockers.append("unsupported_multipart_encoding")
            payload = part.get_payload(decode=True)
            if part.defects:
                fields.blockers.append("invalid_multipart_part")
            if payload is None:
                fields.blockers.append("unsupported_multipart_encoding")
                size = None
            else:
                size = len(payload)
            metadata: dict = {"bytes": size, "content_type": _media_metadata(part.get_content_type())}
            if filename is not None:
                metadata["filename"] = _opaque(ref, location + ".part[" + str(index) + "].filename", "file")
            fields.add(location + "[" + json.dumps(name) + "][" + str(index) + "]", "file" if filename is not None else "string", location, **metadata)
            index += 1
    except (ValueError, UnicodeError, RecursionError):
        fields.blockers.append("invalid_multipart")


def _side(side: Any, label: str, ref: dict, limits: dict, query: list[tuple[str, str]] | None = None, path: str = "") -> dict:
    if not isinstance(side, dict):
        side = {}
    internal_blockers = side.get("_blockers", [])
    blockers = list(internal_blockers) if isinstance(internal_blockers, list) and all(isinstance(item, str) and item in _RAW_BLOCKERS for item in internal_blockers) else ["invalid_capture_metadata"]
    if any(side.get(name) is True for name in ("truncated", "body_truncated", "input_truncated")):
        blockers.append("input_truncated")
    headers, metadata, header_blockers, header_size = _headers(side.get("headers"), ref, label + ".headers", limits["max_fields"])
    blockers.extend(header_blockers)
    content_type = _header_value(headers, "content-type", blockers)
    media = content_type.split(";", 1)[0].strip().lower()
    media = media if _MEDIA.fullmatch(media) else "unknown"
    result = {"parse_state": "incomplete", "body_present": "body" in side, "body_bytes": None, "content_type": _media_metadata(media), "fields": [], "header_metadata": metadata, "blockers": blockers, "characteristics": {}, "limits": dict(limits)}
    fields = _Fields(limits["max_fields"], blockers)
    try:
        for index, (name, kind) in enumerate(query or []):
            fields.add("query[" + json.dumps(name) + "][" + str(index) + "]", kind, "query")
        if label == "request":
            for index, segment in enumerate(path.split("/")):
                if segment:
                    fields.add("path[" + str(index) + "]", "string", "path")
        if "body" not in side:
            blockers.append("body_missing")
        else:
            raw_body = side["body"]
            if isinstance(raw_body, str):
                if len(raw_body) > limits["max_message_bytes"]:
                    result["body_bytes"] = len(raw_body) if raw_body.isascii() else None
                    result["body_bytes_minimum"] = len(raw_body)
                    blockers.append("message_limit")
                    body = None
                else:
                    try:
                        body = raw_body.encode("utf-8")
                    except UnicodeError:
                        blockers.append("invalid_text_encoding")
                        body = None
            elif isinstance(raw_body, bytes) and side.get("_raw") is True:
                body = raw_body
            else:
                blockers.append("invalid_body_type")
                body = None
            if body is not None:
                result["body_bytes"] = len(body)
                message_bytes = side.get("_message_bytes", len(body) + header_size + (len(str(side.get("url", "")).encode("utf-8", errors="replace")) if label == "request" else 0))
                if message_bytes > limits["max_message_bytes"]:
                    blockers.append("message_limit")
                    body = None
                if body is not None:
                    lengths = [value.strip() for name, value in headers if name == "content-length"]
                    if lengths and not _header_value(headers, "transfer-encoding", blockers):
                        if any(not value.isascii() or not value.isdecimal() or len(value) > 20 or int(value) != len(body) for value in lengths):
                            blockers.append("content_length_mismatch")
                    body, trailers = _transport(body, headers, blockers, limits["max_message_bytes"])
                    if trailers:
                        _trailer_headers, trailer_metadata, trailer_blockers, _size = _headers(trailers, ref, label + ".trailers", limits["max_fields"])
                        result["trailer_metadata"] = trailer_metadata
                        blockers.extend(trailer_blockers)
                    if body is not None:
                        result["decoded_body_bytes"] = len(body)
                        text = None
                        if not body:
                            result["characteristics"]["explicit_empty_body"] = True
                        elif media == "image/png":
                            metadata = _png_metadata(body)
                            if metadata is None:
                                blockers.append("invalid_png")
                            else:
                                result["binary_metadata"] = metadata
                                result["characteristics"]["binary_format_png"] = True
                                fields.add("$", "binary", label + ".body")
                        elif media.startswith("image/") or media == "application/pdf":
                            result["characteristics"]["binary_media"] = True
                            blockers.append("unsupported_binary_media")
                        elif media == "multipart/form-data":
                            _multipart(body, content_type, fields, ref, label + ".body", limits["max_depth"])
                        else:
                            charset = re.search(r"(?i)charset\s*=\s*[\"']?([^;\s\"']+)", content_type)
                            encoding = charset.group(1).lower() if charset else "utf-8"
                            if encoding not in {"utf-8", "utf8", "us-ascii", "ascii", "iso-8859-1", "latin-1"}:
                                blockers.append("unsupported_charset")
                            else:
                                try:
                                    text = body.decode(encoding)
                                except UnicodeError:
                                    blockers.append("invalid_text_encoding")
                        if text is not None:
                            if label == "response":
                                result["characteristics"]["stack_trace_signal"] = bool(re.search(
                                    r"(?i)(?:[\"'](?:stack|stacktrace|stack_trace|traceback)[\"']\s*:|Traceback \(most recent call last\)|\b[A-Za-z]+Error\s+at\s+[A-Za-z0-9_.$/<>-]+)", text))
                            if media == "application/json" or (media.startswith("application/") and media.endswith("+json")):
                                _json_fields(text, fields, ref, label + ".body", limits["max_depth"])
                            elif media == "application/x-www-form-urlencoded":
                                _pairs_fields(text, fields, ref, label + ".body")
                            elif media in {"text/html", "application/xhtml+xml"}:
                                parser = _HTML(fields, ref, label + ".body", limits["max_depth"])
                                try:
                                    parser.feed(text)
                                    parser.close()
                                finally:
                                    result["html"] = {"forms": parser.forms, "links": parser.links}
                                    result["characteristics"].update({"form_count": len(parser.forms), "input_count": parser.inputs, "link_count": len(parser.links), "has_password_input": parser.password, "login_page_signal": parser.password or bool(re.search(r"(?i)\b(?:sign in|log in|login)\b", text))})
                                result["characteristics"].update({"error_page_signal": bool(re.search(r"(?i)\b(?:access denied|forbidden|internal server error|stack trace)\b", text)), "possible_soft_404_signal": bool(re.search(r"(?i)\b(?:not found|page does not exist|404)\b", text))})
                            elif media == "text/plain":
                                result["characteristics"]["error_text_signal"] = bool(re.search(r"(?i)\b(?:error|denied|not found|exception)\b", text))
                                fields.add("$", "text", label + ".body")
                            else:
                                blockers.append("unsupported_media_type")
    except _StopFields:
        pass
    blockers[:] = list(dict.fromkeys(blockers))
    result["fields"] = fields.items
    result["field_count"] = len(fields.items)
    result["parse_state"] = "incomplete" if blockers else "complete"
    return result


def _context(raw: Any, ref: dict) -> dict:
    if not isinstance(raw, dict):
        raw = {}
    context = {name: _safe_label(raw.get(name), ref, "context." + name) for name in _CONTEXT}
    context["scenario"] = _safe_label(raw.get("scenario"), ref, "context.scenario")
    step = raw.get("scenario_step")
    context["scenario_step"] = step if type(step) is int and 0 <= step <= 100000 else "unknown"
    origin = raw.get("annotation_origin", "operator_asserted")
    context["annotation_origin"] = origin if origin in {"parsed", "operator_asserted", "model_proposed", "check_result"} else "operator_asserted"
    context["expected_visibility"] = context["access_policy"] if context["policy_source"] != "unknown" and context["access_policy"] != "unknown" else "unknown"
    return context


def _plan(kind: str, fact: str, alternative: str, positive: str, negative: str, steps: list[str], baseline: list[str], capabilities: list[str], ref: dict, endpoint: dict) -> dict:
    return {"id": entity_id("hypothesis", {"source": ref, "endpoint": endpoint["id"], "kind": kind}), "kind": kind, "observed_fact": fact, "alternative_explanation": alternative, "contrasting_predictions": {"hypothesis_supported": positive, "alternative_supported": negative}, "baseline_requirements": baseline, "next_check": {"steps": steps, "preserve": ["method, request shape and non-test inputs", "actor, session version, tenant and object state"], "repeat": "An independently captured repeat after the baseline and distinguishing test; a copied capture is insufficient.", "execution_state": "not_run"}, "required_capabilities": capabilities, "outcome": "candidate"}


def _semantics(endpoint: dict, context: dict, request: dict, response: dict, ref: dict,
               path_object_signal: bool = False) -> tuple[dict, list[dict], list[str]]:
    method, path = endpoint["method"], endpoint["path"]
    action = {"GET": "read_candidate", "HEAD": "metadata_read_candidate", "OPTIONS": "capability_probe_candidate", "POST": "submission_or_creation_candidate", "PUT": "replacement_candidate", "PATCH": "update_candidate", "DELETE": "deletion_candidate"}.get(method, "unknown")
    for word, candidate in (("logout", "session_end_candidate"), ("login", "session_start_candidate"), ("approve", "approval_candidate"), ("invite", "invitation_candidate"), ("export", "export_candidate"), ("search", "search_candidate"), ("delete", "deletion_candidate")):
        if word in path.lower().split("/"):
            action = candidate
            break
    business = {"inferred_action": action, "inference_basis": "HTTP method and sanitized path semantics only", "known_effect": "unknown", "expected_boundary": context["expected_visibility"], "boundary_origin": "operator_asserted" if context["expected_visibility"] != "unknown" else "unknown"}
    missing = ["Current known-good, nonexistent-route and invalid-object baselines", "Semantic identity of the returned object or actual server-side effect", "Independent distinguishing test and repeat", "Current authorization, scope, manifest, budget and a compatible execution adapter"]
    if context["actor"] == "unknown" or context["session_version"] == "unknown":
        missing.append("Known actor and compatible session version")
    if context["object_owner"] == "unknown":
        missing.append("Provenance for object ownership")
    if context["expected_visibility"] == "unknown":
        missing.append("Policy source establishing expected visibility, including legitimate sharing")
    if request["parse_state"] != "complete" or response["parse_state"] != "complete":
        missing.insert(0, "Complete supported request and response capture within declared limits")
    status = response.get("status")
    indicators = [name for name, present in response["characteristics"].items() if present is True]
    hypotheses = []
    input_paths = [item["path"] for item in request["fields"] if item["location"] != "path"]
    object_signal = path_object_signal or context["object"] != "unknown" or any(re.search(r'(?i)(?:"(?:id|object|object_id|document_id|tenant|tenant_id|owner|owner_id)"|Id")', item) for item in input_paths)
    if object_signal:
        if context["expected_visibility"] == "unknown":
            hypotheses.append(_plan("visibility_policy", "Object or tenant selector metadata is present; expected visibility has no established policy source.", "The object can be public, shared, or subject to a different tenant/role policy.", "A policy source and owner baseline establish a restricted boundary for the exact object and actor.", "The policy explicitly allows the observed actor, legitimate sharing, or public visibility.", ["Obtain the operator policy source and an owner-controlled object annotation.", "Record public/shared/private state and the actors allowed by that policy.", "Only then select a permitted owner/non-owner comparison using compatible sessions."], ["Policy source", "Known owner-controlled object", "Sharing and tenant state"], ["operator_policy_annotation", "owner_baseline", "compatible_test_sessions"], ref, endpoint))
        else:
            hypotheses.append(_plan("annotated_access_boundary", "Operator annotations supply an expected access boundary; the saved response does not prove a violation.", "Legitimate sharing, different object state, mismatched sessions or a generic public result can explain the response.", "A policy-disallowed actor receives object-specific restricted content or a independently observed restricted effect.", "The actor is denied, receives only public content, or the policy permits access in the tested state.", ["Capture owner and permitted-reader baselines for the annotated object and sharing state.", "Change only the selected object reference while preserving the policy-disallowed actor's compatible session and tenant context.", "Review actual object identity/content against owner and invalid-object baselines; independently repeat the comparison."], ["Policy source for the exact object", "Owner baseline", "Compatible A/B sessions", "Invalid-object and public-content controls"], ["authorized_authz_readonly_runner", "semantic_response_review", "independent_repeat"], ref, endpoint))
    privilege_signal = any(re.search(r'(?i)(?:"(?:role|admin|is_admin|isadmin|permission|permissions|privilege|scope)"|(?:^|\W)(?:role|admin|permission|privilege|scope)(?:$|\W))', item) for item in input_paths)
    if privilege_signal:
        hypotheses.append(_plan("privilege_control_input", "The request contains a role, permission, admin or scope control field; server-side authorization is unproven.", "The field may be ignored, display-only, or restricted to a legitimate self-service operation.", "Changing only the control field changes an independently observed authorization boundary or privileged effect.", "The server rejects or ignores the field and effective privileges remain unchanged.", ["Establish the current account's role and the normal request/response baseline.", "Use only an explicitly authorized synthetic context and vary one control field.", "Verify effective permissions through an independent read-only check and repeat from a clean session."], ["Known account role", "Unmodified request baseline", "Independent effective-permission check"], ["authorized_authz_readonly_runner", "semantic_response_review", "independent_repeat"], ref, endpoint))
    if method not in {"GET", "HEAD", "OPTIONS", "UNKNOWN"} or action in {"session_end_candidate", "approval_candidate", "deletion_candidate"}:
        hypotheses.append(_plan("actual_effect", "Method/path suggests " + action + "; actual server-side effect remains unknown.", "The server may validate only, return a receipt, ignore input, queue work, reject internally, or perform an idempotent action.", "An independent authorized state observation after the action shows the intended transition on the exact controlled object.", "Before/after state is unchanged, only a receipt differs, or the transition is explained by a separate action.", ["Obtain the controlled object's pre-action state and a valid-action baseline.", "Use the existing permitted state-change adapter for one bounded distinguishing action, with cleanup and stop rules established.", "Observe state independently after the action and repeat only when the manifest permits the side effect."], ["Pre-action state", "Valid-action baseline", "Independent post-action state", "Cleanup/stop conditions"], ["authorized_state_change_adapter", "independent_state_observer", "side_effect_budget"], ref, endpoint))
    if any(re.search(r'(?i)"(?:url|uri|callback|redirect|redirect_uri|next|return_url|destination)"', path) for path in input_paths):
        hypotheses.append(_plan("destination_handling", "A destination-like input field occurs in the complete inspected request shape; its runtime use is unknown.", "It may be ignored, validated, stored, rendered client-side, or used only in a redirect.", "A permitted inert marker has a independently observed destination-related effect attributable to this action.", "No destination effect occurs or the marker appears only as inert stored/client-visible data.", ["Establish a baseline with the existing legitimate destination and review its actual effect.", "Vary only the destination field using an operator-approved inert marker within scope.", "Use an existing approved observer to distinguish redirect, storage/client rendering, and server interaction; do not infer server fetch from reflected text."], ["Legitimate destination baseline", "Exact destination field", "Approved inert marker and observer"], ["approved_destination_test_adapter", "independent_effect_observer"], ref, endpoint))
    if status is not None and status >= 500 and response["characteristics"].get("stack_trace_signal") is True:
        hypotheses.append(_plan("error_stack_disclosure", "A 5xx response contains a stack/trace field marker; disclosure impact is not established.", "The trace may be sanitized, generic or limited to non-sensitive framework names.", "A bounded repeat exposes internal paths, source locations or implementation details beyond the public error contract.", "The error is sanitized and reveals no internal implementation details.", ["Compare with a random 404 and a known generic server error under the same anonymous context.", "Review only the redacted response shape for internal paths or source locations.", "Repeat once with the same bounded request and retain the exact Caido evidence reference."], ["Random 404 baseline", "Generic error baseline", "Redacted trace structure", "Independent repeat"], ["semantic_response_review", "independent_repeat"], ref, endpoint))
    return business, hypotheses, missing


def analyze_capture(capture: dict, source_ref: dict, program: str, *, max_message_bytes: int = 2097152, max_fields: int = 2000, max_depth: int = 64) -> dict:
    """Analyze in memory; source shape is checked, its existence is not required.

    A caller may use a provisional reference for transient live triage, then strip
    it. Importable bundles separately verify real source hashes and provenance.
    """
    _program(program)
    ref = _reference(source_ref)
    limits = _limits(max_message_bytes, max_fields, max_depth)
    if not isinstance(capture, dict):
        raise BehaviorError("Capture must be an object")
    is_caido = "request" not in capture
    if is_caido:
        capture = _caido(capture, max_message_bytes)
    raw_capture_id = capture.get("id")
    if is_caido and isinstance(raw_capture_id, str) and (re.fullmatch(r"[0-9]{1,20}", raw_capture_id) or re.fullmatch(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}", raw_capture_id)):
        # Caido inventory IDs are lookup provenance, not session credentials or
        # executed-action receipts. Retain them after raw nodes are discarded.
        capture_id = raw_capture_id
    else:
        capture_id = _safe_label(raw_capture_id, ref, "capture.id")
    request = capture.get("request") if isinstance(capture.get("request"), dict) else {}
    response = capture.get("response") if isinstance(capture.get("response"), dict) else {}
    endpoint, query, url_blockers = _url(request.get("url"), ref, "request.url")
    method = request.get("method")
    endpoint["method"] = method if isinstance(method, str) and re.fullmatch(r"[A-Z]{1,16}", method) else "UNKNOWN"
    endpoint["id"] = entity_id("endpoint", {"program": program, **endpoint})
    request_analysis = _side(request, "request", ref, limits, query, endpoint["path"])
    request_analysis["blockers"].extend(url_blockers)
    if endpoint["method"] == "UNKNOWN":
        request_analysis["blockers"].append("method_missing_or_invalid")
    status = response.get("status")
    response_analysis = _side(response, "response", ref, limits)
    response_analysis["status"] = status if type(status) is int and 100 <= status <= 599 else None
    if response_analysis["status"] is None:
        response_analysis["blockers"].append("status_missing_or_invalid")
    for side in (request_analysis, response_analysis):
        if any(capture.get(name) is True for name in ("truncated", "body_truncated", "input_truncated")):
            side["blockers"].append("input_truncated")
        side["blockers"] = list(dict.fromkeys(side["blockers"]))
        side["parse_state"] = "incomplete" if side["blockers"] else "complete"
    context = _context(capture.get("context"), ref)
    business, hypotheses, missing = _semantics(endpoint, context, request_analysis, response_analysis, ref,
                                               _path_object_signal(request.get("url")))
    timestamp = capture.get("observed_at")
    observed_at = timestamp if isinstance(timestamp, str) and re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})", timestamp) else "unknown"
    context["observed_at"] = observed_at
    business["semantic_review"] = {"required": True, "required_conclusions": ["Expected access boundary with policy provenance", "Actual object identity or independent server-side effect", "Alternative explanation and distinguishing baseline/test/repeat", "supported, not_supported_in_context, inconclusive, or blocked"], "status": "not_run"}
    card = {"schema_version": 1, "id": entity_id("card", {"program": program, "source": ref, "capture_id": capture_id}), "capture_id": capture_id, "program": program, "source_refs": [ref], "endpoint": endpoint, "context": context, "request_analysis": request_analysis, "response_analysis": response_analysis, "business_action": business, "hypotheses": hypotheses, "missing_evidence": missing, "analysis_state": "needs_review" if all(side["parse_state"] == "complete" for side in (request_analysis, response_analysis)) else "incomplete"}
    try:
        assert_public(card)
    except ValueError as exc:
        raise BehaviorError("Unsafe analysis output") from exc
    return card


def _records(document: Any, program: str) -> list[tuple[dict, str]]:
    if isinstance(document, list):
        return [(item, "$[" + str(index) + "]") for index, item in enumerate(document)]
    if not isinstance(document, dict):
        raise BehaviorError("Unsupported capture document")
    if "captures" in document:
        if document.get("schema_version") != 1 or document.get("program") != program or not isinstance(document["captures"], list):
            raise BehaviorError("Invalid normalized capture document")
        return [(item, "$.captures[" + str(index) + "]") for index, item in enumerate(document["captures"])]
    if isinstance(document.get("nodes"), list):
        return [(item, "$.nodes[" + str(index) + "]") for index, item in enumerate(document["nodes"])]
    try:
        edges = document["data"]["requests"]["edges"]
        if not isinstance(edges, list) or any(not isinstance(edge, dict) or not isinstance(edge.get("node"), dict) for edge in edges):
            raise TypeError
        return [(edge["node"], "$.data.requests.edges[" + str(index) + "].node") for index, edge in enumerate(edges)]
    except (KeyError, TypeError):
        raise BehaviorError("Unsupported capture document") from None


def import_capture_document(document: Any, source_ref: dict, program: str, *, max_message_bytes: int = 2097152, max_fields: int = 2000, max_depth: int = 64) -> dict:
    _program(program)
    ref = _reference(source_ref, verify=True)
    records = _records(document, program)
    if not records:
        raise BehaviorError("Capture document is empty; analysis is incomplete")
    ids = set()
    cards = []
    envelopes = [document] if isinstance(document, dict) else []
    if isinstance(document, dict):
        for name in ("meta", "data"):
            if isinstance(document.get(name), dict):
                envelopes.append(document[name])
        if isinstance(document.get("data"), dict) and isinstance(document["data"].get("requests"), dict):
            envelopes.append(document["data"]["requests"])
    document_truncated = any(envelope.get(name) is True for envelope in envelopes
                             for name in ("truncated", "body_truncated", "input_truncated"))
    for capture, locator in records:
        if document_truncated and isinstance(capture, dict):
            capture = {**capture, "input_truncated": True}
        if not isinstance(capture, dict) or not isinstance(capture.get("id"), str) or not capture["id"]:
            raise BehaviorError("Capture identity is missing")
        if capture["id"] in ids:
            raise BehaviorError("Duplicate capture identity within a source")
        ids.add(capture["id"])
        record_ref = {**ref, "record_locator": ref["record_locator"] + locator[1:]}
        if isinstance(document, dict) and any(document.get(name) is True for name in ("truncated", "input_truncated")):
            capture = {**capture, "input_truncated": True}
        cards.append(analyze_capture(capture, record_ref, program, max_message_bytes=max_message_bytes, max_fields=max_fields, max_depth=max_depth))
    return bundle_from_cards(cards, [ref], program)


def bundle_from_cards(cards: list[dict], sources: list[dict], program: str) -> dict:
    """Convert redacted, provenance-rebound cards to graph proposals.

    Exact source versions are verified; record locators may differ from the
    declared root locator. This does not grant semantic acceptance or execution.
    """
    _program(program)
    if not isinstance(cards, list) or not cards or not isinstance(sources, list) or not sources:
        raise BehaviorError("Analysis cards and source provenance are required")
    declared = [_reference(ref, verify=True) for ref in sources]
    versions = {(ref["path"], ref["sha256"]) for ref in declared}
    entities: dict[str, dict] = {}
    assertions: dict[str, dict] = {}
    card_ids = set()

    def no_raw_values(item: Any) -> None:
        if isinstance(item, dict):
            if any(str(key).lower().replace("-", "_") in {"body", "raw", "raw_body", "raw_request", "raw_response", "headers", "header_values", "value", "values"} for key in item):
                raise BehaviorError("Raw values are forbidden in analysis cards")
            for child in item.values():
                no_raw_values(child)
        elif isinstance(item, list):
            for child in item:
                no_raw_values(child)

    def entity(kind: str, identity: dict, data: dict, refs: list[dict], forced: str | None = None) -> str:
        identity_id = forced or entity_id(kind, {"program": program, **identity})
        if identity_id in entities:
            for ref in refs:
                if ref not in entities[identity_id]["source_refs"]:
                    entities[identity_id]["source_refs"].append(ref)
        else:
            entities[identity_id] = {"id": identity_id, "kind": kind, "data": data, "source_refs": list(refs)}
        return identity_id

    def assertion(subject: str, predicate: str, object_id: str, context: dict, refs: list[dict], origin: str = "parsed") -> None:
        item = {"subject": subject, "predicate": predicate, "object": object_id, "context": {"program": program, **context}, "origin_kind": origin, "evidence_state": "observed", "source_refs": refs}
        item["id"] = entity_id("assertion", item)
        assertions[item["id"]] = item

    flow_steps: dict[str, list[tuple[int, str, list[dict], dict]]] = {}
    for card in cards:
        try:
            if not isinstance(card, dict) or set(card) != _CARD_KEYS:
                raise BehaviorError("Invalid analysis card shape")
            no_raw_values(card)
            assert_public(card)
            if card["schema_version"] != 1 or card["program"] != program or card["id"] in card_ids:
                raise BehaviorError("Invalid or duplicate analysis card")
            if card["analysis_state"] not in {"needs_review", "incomplete"} or any(hypothesis["outcome"] != "candidate" for hypothesis in card["hypotheses"]):
                raise BehaviorError("Analysis cannot promote a candidate")
            refs = [_reference(ref) for ref in card["source_refs"]]
            if not refs or any((ref["path"], ref["sha256"]) not in versions for ref in refs):
                raise BehaviorError("Analysis card lacks declared provenance")
            endpoint, context = card["endpoint"], card["context"]
            card_ids.add(card["id"])
            asset = entity("asset", {"origin": endpoint["origin"]}, {"program": program, "origin": endpoint["origin"]}, refs)
            endpoint_id = entity("endpoint", {key: value for key, value in endpoint.items() if key != "id"}, {"program": program, **{key: value for key, value in endpoint.items() if key != "id"}}, refs, endpoint["id"])
            observation_data = {"program": program, "capture_id": card["capture_id"], "card_id": card["id"], "endpoint_id": endpoint_id, "observation_kind": "historical_capture", "observed_at": context.get("observed_at", "unknown"), "analysis_state": card["analysis_state"], "freshness": "unknown", "context": context}
            if card["response_analysis"]["status"] is not None:
                observation_data["http_status"] = card["response_analysis"]["status"]
            observation = entity("observation", {"card_id": card["id"], "source_refs": refs}, observation_data, refs)
            request_analysis = card["request_analysis"]
            request_signature = request_analysis.get("request_signature")
            if request_signature is not None:
                if not isinstance(request_signature, str) or not re.fullmatch(r"[a-f0-9]{64}", request_signature):
                    raise BehaviorError("Invalid keyed request signature")
                request_data = {
                    "program": program,
                    "request_signature": request_signature,
                    "signature_state": (
                        "complete" if request_analysis.get("capture_truncated") is False
                        else "partial_prefix" if request_analysis.get("capture_truncated") is True
                        else "unknown_extent"
                    ),
                    "method": card["endpoint"].get("method", "UNKNOWN"),
                    "content_type": request_analysis.get("content_type", "unknown"),
                    "body_bytes": request_analysis.get("body_bytes"),
                    "parse_state": request_analysis.get("parse_state", "incomplete"),
                    "header_metadata": request_analysis.get("header_metadata", []),
                    "fields": request_analysis.get("fields", []),
                    "characteristics": request_analysis.get("characteristics", {}),
                }
                request_variant = entity(
                    "request_variant",
                    {"request_signature": request_signature},
                    request_data,
                    refs,
                )
                assertion(observation, "uses_request_variant", request_variant, context, refs)
            response_analysis = card["response_analysis"]
            response_signature = response_analysis.get("response_signature")
            if response_signature is not None:
                if not isinstance(response_signature, str) or not re.fullmatch(r"[a-f0-9]{64}", response_signature):
                    raise BehaviorError("Invalid keyed server-response signature")
                response_data = {
                    "program": program,
                    "response_signature": response_signature,
                    "signature_state": (
                        "complete" if response_analysis.get("capture_truncated") is False
                        else "partial_prefix" if response_analysis.get("capture_truncated") is True
                        else "unknown_extent"
                    ),
                    "status": response_analysis.get("status"),
                    "content_type": response_analysis.get("content_type", "unknown"),
                    "body_bytes": response_analysis.get("body_bytes"),
                    "decoded_body_bytes": response_analysis.get("decoded_body_bytes"),
                    "parse_state": response_analysis.get("parse_state", "incomplete"),
                    "header_metadata": response_analysis.get("header_metadata", []),
                    "fields": response_analysis.get("fields", []),
                    "characteristics": response_analysis.get("characteristics", {}),
                }
                response_variant = entity(
                    "response_variant",
                    {"response_signature": response_signature},
                    response_data,
                    refs,
                )
                assertion(observation, "received_response", response_variant, context, refs)
            scenario = context.get("scenario", "unknown")
            scenario_step = context.get("scenario_step", "unknown")
            if scenario != "unknown" and type(scenario_step) is int:
                flow_steps.setdefault(scenario, []).append((scenario_step, observation, refs, context))
            assertion(asset, "exposes", endpoint_id, context, refs)
            assertion(observation, "invokes", endpoint_id, context, refs)
            known_actor = context.get("actor", "unknown") != "unknown"
            actor_identity = {"alias": context.get("actor", "unknown"), "tenant": context.get("tenant", "unknown")}
            if not known_actor or actor_identity["tenant"] == "unknown":
                actor_identity["source_context"] = card["id"]
            actor = entity("actor", actor_identity, {"program": program, **actor_identity, "annotation_origin": "operator_asserted"}, refs)
            session_identity = {key: context.get(key, "unknown") for key in ("actor", "session_version", "role", "tenant")}
            if any(value == "unknown" for value in session_identity.values()):
                session_identity["source_context"] = card["id"]
            session = entity("session_context", session_identity, {"program": program, **session_identity, "annotation_origin": "operator_asserted"}, refs)
            assertion(actor, "invokes", observation, context, refs, "operator_asserted")
            assertion(session, "invokes", observation, context, refs, "operator_asserted")
            if context.get("object", "unknown") != "unknown":
                # Without an explicit object-type namespace, a matching string is
                # insufficient for cross-capture object identity or data flow.
                object_identity = {"alias": context["object"], "origin": endpoint["origin"], "tenant": context.get("tenant", "unknown"), "source_context": card["id"]}
                object_id = entity("object", object_identity, {"program": program, **object_identity, "expected_visibility": context["expected_visibility"], "annotation_origin": "operator_asserted"}, refs)
                assertion(observation, "references", object_id, context, refs, "operator_asserted")
                owner = context.get("object_owner", "unknown")
                if owner != "unknown":
                    owner_id = entity("actor", {"alias": owner, "tenant": context.get("tenant", "unknown"), "source_context": card["id"]}, {"program": program, "alias": owner, "tenant": context.get("tenant", "unknown"), "annotation_origin": "operator_asserted"}, refs)
                    assertion(object_id, "owned_by", owner_id, context, refs, "operator_asserted")
            for side_name in ("request_analysis", "response_analysis"):
                for field in card[side_name]["fields"]:
                    parameter_data = {"program": program, "endpoint_id": endpoint_id, "side": side_name.split("_", 1)[0], **field}
                    parameter_data["path"] = _graph_path_template(parameter_data.get("path"))
                    parameter = entity("parameter", parameter_data, parameter_data, refs)
                    assertion(endpoint_id if side_name == "request_analysis" else observation, "has_parameter" if side_name == "request_analysis" else "returns", parameter, context, refs)
            boundary = entity("condition", {"card_id": card["id"], "kind": "expected_visibility"}, {"program": program, "condition_kind": "expected_visibility", "value": "unknown", "annotation": context["expected_visibility"], "policy_source": context.get("policy_source", "unknown"), "annotation_origin": "operator_asserted"}, refs)
            assertion(observation, "requires", boundary, context, refs, "operator_asserted")
        except (KeyError, TypeError, ValueError) as exc:
            if isinstance(exc, BehaviorError):
                raise
            raise BehaviorError("Invalid or unsafe analysis card") from exc
    for scenario, steps in flow_steps.items():
        flow_refs = [ref for _step, _observation, refs, _context in steps for ref in refs]
        flow_id = entity("flow", {"scenario": scenario},
                         {"program": program, "scenario": scenario, "annotation_origin": "parsed"},
                         flow_refs)
        ordered = sorted(steps, key=lambda item: (item[0], item[1]))
        previous = None
        for step, observation, refs, _context in ordered:
            flow_context = {"scenario": scenario, "scenario_step": step}
            assertion(flow_id, "contains", observation, flow_context, refs)
            if previous is not None and step == previous[0] + 1:
                assertion(previous[1], "precedes", observation, flow_context, refs)
            previous = (step, observation)
    for item in entities.values():
        item["source_refs"].sort(key=lambda ref: (ref["path"], ref["sha256"], ref["record_locator"]))
    bundle = {"schema_version": 1, "program": program, "sources": sorted(declared, key=lambda ref: (ref["path"], ref["sha256"], ref["record_locator"])), "entities": sorted(entities.values(), key=lambda item: item["id"]), "assertions": sorted(assertions.values(), key=lambda item: item["id"]), "cards": sorted(cards, key=lambda item: item["id"])}
    try:
        assert_public(bundle)
    except ValueError as exc:
        raise BehaviorError("Unsafe graph proposal") from exc
    return bundle
