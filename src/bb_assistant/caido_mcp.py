"""Strict adapter from the connected Caido MCP tools to ``CaidoReadSource``.

This module is deliberately harness-neutral: the caller supplies a synchronous
``call_tool(name, args)`` function. It does not discover an MCP server URL or
read Caido's internal database.
"""
from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from contextlib import contextmanager
from dataclasses import dataclass
import json
import re
import threading
from typing import Any, Iterator
from urllib.parse import urlsplit

from .caido import HistoryPage, RequestMetadata, RequestMetadataPage
from .domain import CaidoExchangeRef, CaidoProjectRef, HttpExchange


_TOOL_LIST_PROJECTS = "caido_list_projects"
_TOOL_SELECT_PROJECT = "caido_select_project"
_TOOL_LIST_REQUESTS = "caido_list_requests"
_TOOL_GET_REQUEST = "caido_get_request"
_INCLUDE = ["requestHeaders", "requestBody", "responseHeaders", "responseBody"]
_PAGE_MAX = 100
_DETAIL_BATCH = 8
_BODY_CHUNK = 64 * 1024
_BODY_MAX = 128 * 1024 * 1024
_HTTP_TOKEN = re.compile(r"^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
_REQUEST_LINE = re.compile(r"^(\S+)\s+(\S+)\s+(HTTP/\d(?:\.\d)?)$")
_RESPONSE_LINE = re.compile(r"^HTTP/\d(?:\.\d)?\s+(\d{3})(?:\s|$)", re.I)
_LOCK = threading.RLock()


class CaidoMCPError(RuntimeError):
    """A sanitized bridge error; messages never include tool values."""


@dataclass(frozen=True, slots=True)
class _RequestMetadata:
    request_id: str
    method: str
    url: str
    status_code: int | None


class CaidoMCPReadSource:
    """Implement ``CaidoReadSource`` using the four connected Caido MCP tools.

    ``instance_key`` is a caller-configured local namespace key. The current
    Caido tools do not expose a stable instance identifier, so this class does
    not derive one from a version string or other mutable metadata.
    """

    def __init__(self, call_tool: Callable[[str, dict[str, Any]], Any], *, instance_key: str) -> None:
        if not callable(call_tool):
            raise TypeError("call_tool must be callable")
        if not isinstance(instance_key, str) or not instance_key:
            raise ValueError("instance_key must be a non-empty configured identifier")
        # Reuse the domain contract's opaque-id validation without persisting
        # or deriving identity from tool output.
        CaidoProjectRef(instance_key, "validation-only")
        self._call_tool = call_tool
        self._instance_key = instance_key

    def list_projects(self) -> Sequence[CaidoProjectRef]:
        with _LOCK:
            try:
                return self._list_projects_unlocked()[0]
            except CaidoMCPError:
                raise
            except Exception:
                raise CaidoMCPError("Caido project listing could not be normalized") from None

    def list_history(
        self,
        project: CaidoProjectRef,
        *,
        cursor: str | None = None,
        page_size: int = 100,
    ) -> HistoryPage:
        page = self.list_request_page(project, cursor=cursor, page_size=page_size)
        return self.fetch_history_page(project, page)

    def list_request_page(
        self,
        project: CaidoProjectRef,
        *,
        cursor: str | None = None,
        page_size: int = 100,
        httpql: str | None = None,
    ) -> RequestMetadataPage:
        if not project.same_project(CaidoProjectRef(self._instance_key, project.project_id, project.display_name)):
            raise CaidoMCPError("Project belongs to a different configured Caido instance")
        if type(page_size) is not int or not (1 <= page_size <= _PAGE_MAX):
            raise CaidoMCPError("Caido MCP pages must contain between 1 and 100 requests")
        if cursor is not None and (not isinstance(cursor, str) or not cursor):
            raise CaidoMCPError("Caido cursor is invalid")
        if httpql is not None and (
            not isinstance(httpql, str) or not httpql.startswith('req.created_at.gt:"')
            or not httpql.endswith('"') or len(httpql) > 128
        ):
            raise CaidoMCPError("Caido HTTPQL time filter is invalid")
        with _LOCK:
            try:
                with self._selected_project_unlocked(project):
                    args: dict[str, Any] = {"limit": page_size}
                    if cursor is not None:
                        args["after"] = cursor
                    if httpql is not None:
                        args["httpql"] = httpql
                    result = self._payload(
                        _TOOL_LIST_REQUESTS, args,
                        expected_project_id=project.project_id,
                    )
                    rows, next_cursor, has_more = self._parse_request_page(result, cursor)
                    return RequestMetadataPage(
                        tuple(RequestMetadata(row.request_id, row.method, row.url, row.status_code) for row in rows),
                        next_cursor,
                        has_more,
                    )
            except CaidoMCPError:
                raise
            except Exception:
                raise CaidoMCPError("Caido request metadata page could not be normalized") from None

    def fetch_history_page(
        self,
        project: CaidoProjectRef,
        page: RequestMetadataPage,
        *,
        skip_request_ids: frozenset[str] = frozenset(),
        max_body_bytes: int = _BODY_MAX,
    ) -> HistoryPage:
        if not project.same_project(CaidoProjectRef(self._instance_key, project.project_id, project.display_name)):
            raise CaidoMCPError("Project belongs to a different configured Caido instance")
        if not isinstance(page, RequestMetadataPage) or type(page.has_more) is not bool:
            raise CaidoMCPError("Caido request metadata page is invalid")
        if page.has_more and (not isinstance(page.next_cursor, str) or not page.next_cursor):
            raise CaidoMCPError("Caido request metadata cursor is invalid")
        if not isinstance(skip_request_ids, frozenset) or any(not isinstance(value, str) for value in skip_request_ids):
            raise CaidoMCPError("Caido request skip set is invalid")
        if type(max_body_bytes) is not int or not (_BODY_CHUNK <= max_body_bytes <= _BODY_MAX):
            raise CaidoMCPError("Caido live body limit is outside supported bounds")
        rows = tuple(
            _RequestMetadata(row.request_id, row.method, row.url, row.status_code)
            for row in page.requests
            if row.request_id not in skip_request_ids
        )
        if len(rows) > _PAGE_MAX or len({row.request_id for row in rows}) != len(rows):
            raise CaidoMCPError("Caido request metadata page contains duplicate IDs")
        with _LOCK:
            try:
                with self._selected_project_unlocked(project):
                    exchanges_list: list[HttpExchange] = []
                    for start in range(0, len(rows), _DETAIL_BATCH):
                        batch = rows[start:start + _DETAIL_BATCH]
                        payloads = self._get_details_unlocked(
                            tuple(row.request_id for row in batch), 0,
                            expected_project_id=project.project_id,
                        )
                        exchanges_list.extend(
                            self._fetch_exchange_unlocked(
                                project, row, None, initial_payload=payloads[row.request_id],
                                max_body_bytes=max_body_bytes,
                            )
                            for row in batch
                        )
                    return HistoryPage(tuple(exchanges_list), page.next_cursor, page.has_more)
            except CaidoMCPError:
                raise
            except Exception:
                raise CaidoMCPError("Caido history page could not be normalized") from None

    def get_exchange(self, ref: CaidoExchangeRef) -> HttpExchange | None:
        if not ref.project.same_project(
            CaidoProjectRef(self._instance_key, ref.project.project_id, ref.project.display_name)
        ):
            raise CaidoMCPError("Project belongs to a different configured Caido instance")
        with _LOCK:
            try:
                with self._selected_project_unlocked(ref.project):
                    metadata = self._find_request_metadata_unlocked(
                        ref.request_id, expected_project_id=ref.project.project_id
                    )
                    if metadata is None:
                        return None
                    return self._fetch_exchange_unlocked(ref.project, metadata, None)
            except CaidoMCPError:
                raise
            except Exception:
                raise CaidoMCPError("Caido exchange could not be normalized") from None

    def _list_projects_unlocked(self) -> tuple[tuple[CaidoProjectRef, ...], str | None]:
        payload = self._payload(_TOOL_LIST_PROJECTS, {})
        if not isinstance(payload, Mapping) or "projects" not in payload:
            raise CaidoMCPError("Caido project response has an unsupported shape")
        raw_projects = payload["projects"]
        if raw_projects is None:
            raw_projects = []
        if not isinstance(raw_projects, list):
            raise CaidoMCPError("Caido project response has an unsupported shape")

        projects: list[CaidoProjectRef] = []
        current_ids: list[str] = []
        seen: set[str] = set()
        for item in raw_projects:
            if not isinstance(item, Mapping):
                raise CaidoMCPError("Caido project response has an unsupported shape")
            project_id = item.get("id")
            name = item.get("name")
            status = item.get("status")
            version = item.get("version")
            is_current = item.get("isCurrent")
            if (
                not isinstance(project_id, str)
                or not isinstance(name, str)
                or not isinstance(status, str)
                or not isinstance(version, str)
                or type(is_current) is not bool
            ):
                raise CaidoMCPError("Caido project response has an unsupported shape")
            if project_id in seen:
                raise CaidoMCPError("Caido project response contains duplicate IDs")
            seen.add(project_id)
            try:
                project = CaidoProjectRef(self._instance_key, project_id, name)
            except (TypeError, ValueError):
                raise CaidoMCPError("Caido project response contains an invalid ID") from None
            projects.append(project)
            if is_current:
                current_ids.append(project_id)
        if len(current_ids) > 1:
            raise CaidoMCPError("Caido reported an ambiguous selected project")
        return tuple(projects), current_ids[0] if current_ids else None

    @contextmanager
    def _selected_project_unlocked(self, project: CaidoProjectRef) -> Iterator[None]:
        projects, original_id = self._list_projects_unlocked()
        known = next((candidate for candidate in projects if candidate.same_project(project)), None)
        if known is None:
            raise CaidoMCPError("Requested Caido project is not available")
        if original_id == project.project_id:
            yield
            self._assert_current_project_unlocked(project.project_id)
            return
        # The exposed tools have no operation to clear a selection. Switching
        # from an initially empty selection would therefore be irreversible.
        if original_id is None:
            raise CaidoMCPError("Cannot switch projects while preserving an empty selection")

        switch_attempted = False
        try:
            switch_attempted = True
            self._select_project_unlocked(project.project_id)
            yield
            self._assert_current_project_unlocked(project.project_id)
        finally:
            if switch_attempted:
                try:
                    _projects, current_id = self._list_projects_unlocked()
                    if current_id == project.project_id:
                        self._select_project_unlocked(original_id)
                    elif current_id != original_id:
                        # Another client changed the shared selection. Preserve
                        # that client's state instead of restoring over it.
                        raise CaidoMCPError("Caido project selection changed externally")
                except CaidoMCPError:
                    raise
                except Exception:
                    # Do not expose tool exceptions or returned values: they
                    # may contain captured secrets.
                    raise CaidoMCPError("Failed to restore the original Caido project") from None

    def _select_project_unlocked(self, project_id: str) -> None:
        payload = self._payload(_TOOL_SELECT_PROJECT, {"id": project_id})
        if (
            not isinstance(payload, Mapping)
            or payload.get("id") != project_id
            or not isinstance(payload.get("name"), str)
        ):
            raise CaidoMCPError("Caido project selection was not confirmed")
        _projects, current_id = self._list_projects_unlocked()
        if current_id != project_id:
            raise CaidoMCPError("Caido active project does not match the requested project")

    def _assert_current_project_unlocked(self, project_id: str) -> None:
        _projects, current_id = self._list_projects_unlocked()
        if current_id != project_id:
            raise CaidoMCPError("Caido active project changed during history read")

    def _parse_request_page(
        self, payload: Any, previous_cursor: str | None
    ) -> tuple[tuple[_RequestMetadata, ...], str | None, bool]:
        if not isinstance(payload, Mapping):
            raise CaidoMCPError("Caido request page has an unsupported shape")
        raw_rows = payload.get("requests")
        has_more = payload.get("hasMore")
        next_cursor = payload.get("nextCursor")
        if raw_rows is None:
            raw_rows = []
        if not isinstance(raw_rows, list) or type(has_more) is not bool:
            raise CaidoMCPError("Caido request page has an unsupported shape")
        if next_cursor is not None and (not isinstance(next_cursor, str) or not next_cursor):
            raise CaidoMCPError("Caido request page has an invalid cursor")
        if has_more and (next_cursor is None or next_cursor == previous_cursor):
            raise CaidoMCPError("Caido pagination did not advance")

        rows: list[_RequestMetadata] = []
        seen: set[str] = set()
        for row in raw_rows:
            if not isinstance(row, Mapping):
                raise CaidoMCPError("Caido request page has an unsupported shape")
            request_id = row.get("id")
            if (
                not isinstance(request_id, str)
                or not request_id
                or not isinstance(row.get("method"), str)
                or not isinstance(row.get("url"), str)
            ):
                raise CaidoMCPError("Caido request page has an unsupported shape")
            status = row.get("statusCode")
            if status is not None and (type(status) is not int or not 100 <= status <= 599):
                raise CaidoMCPError("Caido request page has an invalid status")
            if request_id in seen:
                raise CaidoMCPError("Caido request page contains duplicate IDs")
            seen.add(request_id)
            rows.append(_RequestMetadata(request_id, row["method"], row["url"], status))
        if has_more and not rows:
            raise CaidoMCPError("Caido returned an advancing cursor without request rows")
        if not has_more:
            next_cursor = None
        return tuple(rows), next_cursor, has_more

    def _find_request_metadata_unlocked(
        self, request_id: str, *, expected_project_id: str
    ) -> _RequestMetadata | None:
        cursor: str | None = None
        seen_cursors: set[str] = set()
        while True:
            if cursor is not None and cursor in seen_cursors:
                raise CaidoMCPError("Caido request metadata pagination repeated a cursor")
            if cursor is not None:
                seen_cursors.add(cursor)
            args: dict[str, Any] = {"limit": _PAGE_MAX}
            if cursor is not None:
                args["after"] = cursor
            payload = self._payload(
                _TOOL_LIST_REQUESTS, args, expected_project_id=expected_project_id
            )
            rows, next_cursor, has_more = self._parse_request_page(payload, cursor)
            match = next((row for row in rows if row.request_id == request_id), None)
            if match is not None:
                return match
            if not has_more:
                return None
            cursor = next_cursor

    def _get_detail_unlocked(
        self, request_id: str, body_offset: int, *, expected_project_id: str
    ) -> Mapping[str, Any]:
        return self._get_details_unlocked(
            (request_id,), body_offset, expected_project_id=expected_project_id
        )[request_id]

    def _get_details_unlocked(
        self, request_ids: tuple[str, ...], body_offset: int, *, expected_project_id: str
    ) -> dict[str, Mapping[str, Any]]:
        if not request_ids or len(request_ids) > _DETAIL_BATCH or len(set(request_ids)) != len(request_ids):
            raise CaidoMCPError("Caido detail batch is invalid")
        payload = self._payload(
            _TOOL_GET_REQUEST,
            {
                "ids": list(request_ids),
                "include": list(_INCLUDE),
                "bodyLimit": _BODY_CHUNK,
                "bodyOffset": body_offset,
            },
            expected_project_id=expected_project_id,
        )
        if len(request_ids) == 1:
            request_id = request_ids[0]
            if "requests" in payload or payload.get("id") != request_id:
                raise CaidoMCPError("Caido single exchange response did not match the requested ID")
            return {request_id: payload}

        if "id" in payload or not isinstance(payload.get("requests"), list):
            raise CaidoMCPError("Caido batch exchange response has an unsupported shape")
        expected_ids = set(request_ids)
        normalized: dict[str, Mapping[str, Any]] = {}
        for item in payload["requests"]:
            if not isinstance(item, Mapping):
                raise CaidoMCPError("Caido batch exchange response has an unsupported shape")
            request_id = item.get("id")
            if not isinstance(request_id, str) or request_id not in expected_ids or request_id in normalized:
                raise CaidoMCPError("Caido batch exchange response IDs do not match the request")
            normalized[request_id] = item
        if set(normalized) != expected_ids:
            raise CaidoMCPError("Caido batch exchange response IDs do not match the request")
        return normalized

    def _fetch_exchange_unlocked(
        self,
        project: CaidoProjectRef,
        metadata: _RequestMetadata,
        source_cursor: str | None,
        *,
        initial_payload: Mapping[str, Any] | None = None,
        max_body_bytes: int = _BODY_MAX,
    ) -> HttpExchange:
        request_id = metadata.request_id
        payload = initial_payload if initial_payload is not None else self._get_detail_unlocked(
            request_id, 0, expected_project_id=project.project_id
        )
        request = payload.get("request")
        raw_response = payload.get("response")
        if not isinstance(request, Mapping):
            raise CaidoMCPError("Caido exchange is missing its request")
        if raw_response is None:
            if metadata.status_code is not None:
                raise CaidoMCPError("Caido response is missing despite response status metadata")
            response = None
        elif isinstance(raw_response, Mapping):
            response = raw_response
        else:
            raise CaidoMCPError("Caido response has an unsupported shape")

        request_method = metadata.method
        request_line = self._parse_request_line(request.get("firstLine"), request_method)
        # Caido may omit the method in list_requests metadata while retaining
        # the original method on the stored request line. Use that line only
        # when the metadata field is empty; a non-empty disagreement remains
        # an integrity error.
        if not request_method:
            request_method = request_line.split(" ", 1)[0].upper()
        request_headers = self._parse_headers(request.get("headers"))
        request_size = self._body_size(request, request_side=True)
        if response is None:
            response_line = None
            response_status = None
            response_headers: tuple[tuple[str, str], ...] = ()
            response_size = 0
        else:
            response_line, response_status = self._parse_response_line(response.get("firstLine"))
            response_headers = self._parse_headers(response.get("headers"))
            response_size = self._body_size(response, request_side=False)
            if metadata.status_code is not None and response_status != metadata.status_code:
                raise CaidoMCPError("Caido response status does not match request metadata")
        if request_size > _BODY_MAX or response_size > _BODY_MAX:
            raise CaidoMCPError("Caido body exceeds the local ingestion limit")

        request_body, request_truncated = self._read_body_unlocked(
            request_id, "request", payload, request_size, request_headers,
            expected_project_id=project.project_id, max_body_bytes=max_body_bytes,
        )
        request_raw = self._serialize_message(request_line, request_headers, request_body)
        if response is None:
            response_raw = None
            response_truncated = None
        else:
            # The same bodyOffset parameter applies to both sides. Fetch the
            # response independently so differing body lengths cannot misalign.
            response_body, response_truncated = self._read_body_unlocked(
                request_id, "response", payload, response_size, response_headers,
                expected_project_id=project.project_id, max_body_bytes=max_body_bytes,
            )
            response_raw = self._serialize_message(response_line, response_headers, response_body)

        try:
            parsed_url = urlsplit(metadata.url)
            if (
                parsed_url.scheme.lower() not in {"http", "https"}
                or not parsed_url.hostname
                or parsed_url.username is not None
                or parsed_url.password is not None
            ):
                raise ValueError
            port = parsed_url.port
            if port is not None and not 1 <= port <= 65535:
                raise ValueError
        except (TypeError, ValueError, UnicodeError):
            raise CaidoMCPError("Caido exchange URL is invalid") from None

        return HttpExchange(
            ref=CaidoExchangeRef(project, request_id),
            captured_at=None,
            request_raw=request_raw,
            response_raw=response_raw,
            method=request_method,
            scheme=parsed_url.scheme.lower(),
            host=parsed_url.hostname,
            port=port or (443 if parsed_url.scheme.lower() == "https" else 80),
            target=metadata.url,
            response_status=response_status,
            request_truncated=request_truncated,
            response_truncated=response_truncated if response is not None else None,
            source_cursor=source_cursor,
            request_wire_fidelity="reconstructed_text",
            response_wire_fidelity="reconstructed_text" if response_raw is not None else "unknown",
        )

    @staticmethod
    def _parse_request_line(value: Any, metadata_method: str) -> str:
        if not isinstance(value, str) or "\r" in value or "\n" in value:
            raise CaidoMCPError("Caido request line is missing or unsupported")
        match = _REQUEST_LINE.fullmatch(value)
        if match is None or not _HTTP_TOKEN.fullmatch(match.group(1)):
            raise CaidoMCPError("Caido request line is unsupported")
        if metadata_method and match.group(1).upper() != metadata_method.upper():
            raise CaidoMCPError("Caido request method does not match request metadata")
        try:
            value.encode("latin-1", errors="strict")
        except UnicodeError:
            raise CaidoMCPError("Caido request line cannot be represented as HTTP bytes") from None
        return value

    @staticmethod
    def _parse_response_line(value: Any) -> tuple[str, int]:
        if not isinstance(value, str) or "\r" in value or "\n" in value:
            raise CaidoMCPError("Caido response line is missing or unsupported")
        match = _RESPONSE_LINE.match(value)
        if match is None:
            raise CaidoMCPError("Caido response line is unsupported")
        status = int(match.group(1))
        if not 100 <= status <= 599:
            raise CaidoMCPError("Caido response status is invalid")
        try:
            value.encode("latin-1", errors="strict")
        except UnicodeError:
            raise CaidoMCPError("Caido response line cannot be represented as HTTP bytes") from None
        return value, status

    @staticmethod
    def _body_size(detail: Mapping[str, Any], *, request_side: bool) -> int:
        fingerprint = detail.get("fingerprint")
        if not isinstance(fingerprint, Mapping) or not isinstance(fingerprint.get("kind"), str):
            raise CaidoMCPError("Caido body fingerprint is missing or unsupported")
        fingerprint_kind = fingerprint.get("kind")
        fingerprint_size = fingerprint.get("bodySize")
        size = fingerprint_size if request_side else detail.get("bodySize")
        # Caido MCP v4.3.0 may omit response.bodySize for an explicitly empty
        # response while retaining fingerprint.kind="empty" and size 0.
        # This is the only safe response-side fallback: other missing sizes
        # remain unknown and stop the page rather than silently losing bytes.
        if (
            not request_side
            and size is None
            and fingerprint_kind == "empty"
            and fingerprint_size == 0
        ):
            size = 0
        if type(size) is not int or size < 0:
            raise CaidoMCPError("Caido body size is unknown")
        declared_size = detail.get("bodySize")
        fingerprint_size = fingerprint.get("bodySize")
        for candidate in (declared_size, fingerprint_size):
            if candidate is not None and (type(candidate) is not int or candidate != size):
                raise CaidoMCPError("Caido body size declarations disagree")
        return size

    def _read_body_unlocked(
        self,
        request_id: str,
        side: str,
        initial_payload: Mapping[str, Any],
        total_size: int,
        headers: tuple[tuple[str, str], ...],
        *,
        expected_project_id: str,
        max_body_bytes: int = _BODY_MAX,
    ) -> tuple[bytes, bool | None]:
        if total_size == 0:
            side_record = initial_payload.get(side)
            if isinstance(side_record, Mapping):
                body = side_record.get("body")
                truncated = side_record.get("truncated")
                if body not in (None, "") or (truncated is not None and (type(truncated) is not bool or truncated)):
                    raise CaidoMCPError("Caido empty body metadata is inconsistent")
            return b"", False

        baseline_detail = initial_payload.get(side)
        if not isinstance(baseline_detail, Mapping):
            return b"", None
        baseline_headers = baseline_detail.get("headers")
        baseline_first_line = baseline_detail.get("firstLine")
        offset = 0
        assembled = bytearray()
        lossy_marker = False
        current_payload: Mapping[str, Any] = initial_payload
        can_page = self._can_page_utf8_body(headers)
        while offset < min(total_size, max_body_bytes):
            detail = current_payload.get(side)
            if not isinstance(detail, Mapping) or self._body_size(detail, request_side=(side == "request")) != total_size:
                raise CaidoMCPError("Caido body detail is incomplete or changed during paging")
            if detail.get("headers") != baseline_headers or detail.get("firstLine") != baseline_first_line:
                raise CaidoMCPError("Caido message metadata changed during body paging")
            body = detail.get("body")
            truncated = detail.get("truncated")
            if (
                not isinstance(body, str)
                or (truncated is not None and type(truncated) is not bool)
            ):
                return bytes(assembled), None
            if "\ufffd" in body:
                lossy_marker = True
            try:
                chunk = body.encode("utf-8", errors="strict")
            except UnicodeError:
                return bytes(assembled), None
            if not chunk:
                return bytes(assembled), True if truncated is True else None
            if offset + len(chunk) > total_size:
                # Some MCP responses contain decoded/transformed text whose
                # UTF-8 representation is larger than the source bodySize.
                # Preserve it as reconstructed data, but do not claim coverage.
                return bytes(assembled) + chunk, None
            expected_truncated = offset + len(chunk) < total_size
            if truncated is False and expected_truncated:
                return bytes(assembled) + chunk, None
            if truncated is True and not expected_truncated:
                return bytes(assembled) + chunk, None
            assembled.extend(chunk)
            offset += len(chunk)
            if offset >= max_body_bytes and offset < total_size:
                return bytes(assembled), True
            if not expected_truncated:
                return bytes(assembled), None if lossy_marker else False
            if not can_page:
                return bytes(assembled), True if truncated is True else None
            try:
                current_payload = self._get_detail_unlocked(
                    request_id, offset, expected_project_id=expected_project_id
                )
            except CaidoMCPError:
                return bytes(assembled), True

        return bytes(assembled), None

    @staticmethod
    def _parse_headers(value: Any) -> tuple[tuple[str, str], ...]:
        if not isinstance(value, list):
            raise CaidoMCPError("Caido exchange headers are missing or unsupported")
        headers: list[tuple[str, str]] = []
        for item in value:
            if not isinstance(item, Mapping):
                raise CaidoMCPError("Caido exchange headers have an unsupported shape")
            name = item.get("name")
            header_value = item.get("value")
            if (
                not isinstance(name, str)
                or not _HTTP_TOKEN.fullmatch(name)
                or not isinstance(header_value, str)
                or any(
                    ord(char) < 32 and char != "\t" or ord(char) == 127
                    for char in header_value
                )
            ):
                raise CaidoMCPError("Caido exchange headers have an unsupported value")
            try:
                name.encode("ascii", errors="strict")
                CaidoMCPReadSource._encode_header_value(header_value)
            except UnicodeError:
                raise CaidoMCPError("Caido exchange headers cannot be represented as HTTP bytes") from None
            headers.append((name, header_value))
        return tuple(headers)

    @staticmethod
    def _declared_utf8_text(headers: tuple[tuple[str, str], ...]) -> bool:
        content_types = [value for name, value in headers if name.lower() == "content-type"]
        if len(content_types) != 1:
            return False
        components = [component.strip() for component in content_types[0].split(";")]
        media_type = components[0].lower()
        charsets = []
        for parameter in components[1:]:
            key, separator, value = parameter.partition("=")
            if separator and key.strip().lower() == "charset":
                charsets.append(value.strip().strip('"').lower())
        if len(charsets) > 1 or (charsets and charsets[0] not in {"utf-8", "utf8"}):
            return False
        is_json = media_type == "application/json" or (
            media_type.startswith("application/") and media_type.endswith("+json")
        )
        if is_json:
            return True
        return bool(charsets) and (
            media_type.startswith("text/")
            or media_type in {"application/xml", "application/x-www-form-urlencoded"}
            or media_type.endswith("+xml")
        )

    @classmethod
    def _can_page_utf8_body(cls, headers: tuple[tuple[str, str], ...]) -> bool:
        if not cls._declared_utf8_text(headers):
            return False
        encodings = [value for name, value in headers if name.lower() == "content-encoding"]
        return all(value.strip().lower() in {"", "identity"} for value in encodings)

    @staticmethod
    def _serialize_message(
        first_line: str,
        headers: tuple[tuple[str, str], ...],
        body: bytes,
    ) -> bytes:
        try:
            head = first_line.encode("latin-1") + b"\r\n"
            head += b"".join(
                name.encode("ascii") + b": " + CaidoMCPReadSource._encode_header_value(value) + b"\r\n"
                for name, value in headers
            )
        except UnicodeError:
            raise CaidoMCPError("Caido message metadata cannot be serialized as HTTP bytes") from None
        return head + b"\r\n" + body

    @staticmethod
    def _encode_header_value(value: str) -> bytes:
        """Keep Latin-1 header bytes when possible; UTF-8 reconstructs wider text."""
        try:
            return value.encode("latin-1", errors="strict")
        except UnicodeEncodeError:
            return value.encode("utf-8", errors="strict")

    def _payload(
        self,
        tool_name: str,
        args: dict[str, Any],
        *,
        expected_project_id: str | None = None,
    ) -> Any:
        if expected_project_id is not None:
            self._assert_current_project_unlocked(expected_project_id)
        try:
            result = self._call_tool(tool_name, args)
        except Exception:
            raise CaidoMCPError("Caido MCP call failed") from None
        if expected_project_id is not None:
            self._assert_current_project_unlocked(expected_project_id)
        if not isinstance(result, Mapping):
            raise CaidoMCPError("Caido MCP result has an unsupported shape")
        is_error = result.get("isError", False)
        if type(is_error) is not bool or is_error:
            raise CaidoMCPError("Caido MCP call returned an error")

        if "structuredContent" in result and result["structuredContent"] is not None:
            payload = result["structuredContent"]
        else:
            content = result.get("content")
            if not isinstance(content, list) or len(content) != 1:
                raise CaidoMCPError("Caido MCP result has an unsupported shape")
            block = content[0]
            if not isinstance(block, Mapping) or block.get("type") != "text" or not isinstance(block.get("text"), str):
                raise CaidoMCPError("Caido MCP result has an unsupported shape")
            try:
                payload = json.loads(block["text"])
            except (json.JSONDecodeError, TypeError):
                raise CaidoMCPError("Caido MCP text result is not JSON") from None
        if not isinstance(payload, Mapping):
            raise CaidoMCPError("Caido MCP payload has an unsupported shape")
        return payload
