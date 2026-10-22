"""Offline contract checks for the connected Caido MCP read adapter."""
from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Any
import unittest

from bb_assistant.caido_mcp import CaidoMCPError, CaidoMCPReadSource
from bb_assistant.domain import CaidoProjectRef


@dataclass(frozen=True)
class _ExchangeFixture:
    method: str
    url: str
    request_line: str
    request_headers: tuple[tuple[str, str], ...]
    request_body: bytes
    response_line: str
    response_headers: tuple[tuple[str, str], ...]
    response_body: bytes
    status: int


class _CaidoMCPFixture:
    def __init__(self, *, current_id: str | None, exchanges_by_project: dict[str, tuple[_ExchangeFixture, ...]]):
        self.current_id = current_id
        self.exchanges_by_project = exchanges_by_project
        self.calls: list[tuple[str, dict[str, Any]]] = []
        self.switch_after_list_requests: str | None = None

    def call_tool(self, name: str, args: dict[str, Any]) -> dict[str, Any]:
        self.calls.append((name, dict(args)))
        if name == "caido_list_projects":
            projects = []
            for project_id in self.exchanges_by_project:
                projects.append({
                    "id": project_id,
                    "name": f"Fixture {project_id}",
                    "status": "READY",
                    "version": "0.58.3",
                    "isCurrent": project_id == self.current_id,
                })
            payload = {"projects": projects}
        elif name == "caido_select_project":
            project_id = args["id"]
            if project_id not in self.exchanges_by_project:
                raise ValueError("unknown fixture project")
            self.current_id = project_id
            payload = {"id": project_id, "name": f"Fixture {project_id}"}
        elif name == "caido_list_requests":
            if self.current_id is None:
                raise ValueError("no selected project")
            exchange_rows = self.exchanges_by_project[self.current_id]
            cursor = args.get("after")
            start = 1 if cursor == "cursor-2" else 0
            rows = exchange_rows[start:start + args["limit"]]
            has_more = start + len(rows) < len(exchange_rows)
            payload = {
                "requests": [
                    {
                        "id": f"request-{index + start + 1}",
                        "method": row.method,
                        "url": row.url,
                        "statusCode": row.status,
                    }
                    for index, row in enumerate(rows)
                ],
                "hasMore": has_more,
                "nextCursor": "cursor-2" if has_more else None,
            }
            if self.switch_after_list_requests is not None:
                self.current_id = self.switch_after_list_requests
                self.switch_after_list_requests = None
        elif name == "caido_get_request":
            if self.current_id is None:
                raise ValueError("no selected project")
            request_ids = args["ids"]
            detail_rows = []
            for request_id in request_ids:
                index = int(request_id.removeprefix("request-")) - 1
                row = self.exchanges_by_project[self.current_id][index]
                detail_rows.append(self._detail(request_id, row, args["bodyOffset"], args["bodyLimit"]))
            payload = detail_rows[0] if len(request_ids) == 1 else {"requests": detail_rows}
        else:
            raise AssertionError(f"unexpected Caido tool: {name}")
        return {"structuredContent": payload, "isError": False}

    @staticmethod
    def _detail(request_id: str, row: _ExchangeFixture, offset: int, limit: int) -> dict[str, Any]:
        def body_slice(data: bytes) -> str:
            return data[offset:offset + limit].decode("utf-8", errors="strict")

        request_truncated = offset + limit < len(row.request_body)
        response_truncated = offset + limit < len(row.response_body)
        return {
            "id": request_id,
            "request": {
                "firstLine": row.request_line,
                "headers": [{"name": name, "value": value} for name, value in row.request_headers],
                "fingerprint": {"kind": "text", "bodySize": len(row.request_body)},
                "bodySize": len(row.request_body),
                "body": body_slice(row.request_body),
                "truncated": request_truncated,
            },
            "response": {
                "firstLine": row.response_line,
                "headers": [{"name": name, "value": value} for name, value in row.response_headers],
                "fingerprint": {"kind": "text", "bodySize": len(row.response_body)},
                "bodySize": len(row.response_body),
                "body": body_slice(row.response_body),
                "truncated": response_truncated,
            },
        }


def _exchange_fixture(*, request_size: int = 12, response_size: int = 18) -> _ExchangeFixture:
    request_body = b"a" * request_size
    response_body = b"b" * response_size
    return _ExchangeFixture(
        method="POST",
        url="https://app.fixture.test/api/items/search?q=alpha",
        request_line="POST /api/items/search?q=alpha HTTP/1.1",
        request_headers=(
            ("Host", "app.fixture.test"),
            ("Content-Type", "application/json; charset=utf-8"),
        ),
        request_body=request_body,
        response_line="HTTP/1.1 200 OK",
        response_headers=(
            ("Content-Type", "application/json; charset=utf-8"),
        ),
        response_body=response_body,
        status=200,
    )


class CaidoMCPAdapterTests(unittest.TestCase):
    def setUp(self) -> None:
        self.project_a = "project-a"
        self.project_b = "project-b"
        self.project_c = "project-c"
        self.projects = {
            self.project_a: (_exchange_fixture(),),
            self.project_b: (_exchange_fixture(request_size=65539, response_size=65538),),
            self.project_c: (_exchange_fixture(),),
        }

    def test_switches_selected_project_pages_bodies_and_restores_original(self) -> None:
        fixture = _CaidoMCPFixture(current_id=self.project_a, exchanges_by_project=self.projects)
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")
        project = next(row for row in source.list_projects() if row.project_id == self.project_b)

        page = source.list_history(project, page_size=10)

        self.assertEqual(fixture.current_id, self.project_a)
        self.assertFalse(page.has_more)
        self.assertEqual(len(page.exchanges), 1)
        exchange = page.exchanges[0]
        self.assertEqual(exchange.ref.project.project_id, self.project_b)
        self.assertEqual(exchange.request_raw.split(b"\r\n\r\n", 1)[1], self.projects[self.project_b][0].request_body)
        self.assertEqual(exchange.response_raw.split(b"\r\n\r\n", 1)[1], self.projects[self.project_b][0].response_body)
        self.assertFalse(exchange.request_truncated)
        self.assertFalse(exchange.response_truncated)
        self.assertEqual(exchange.request_wire_fidelity, "reconstructed_text")
        self.assertEqual(exchange.response_wire_fidelity, "reconstructed_text")
        detail_offsets = [args["bodyOffset"] for name, args in fixture.calls if name == "caido_get_request"]
        self.assertEqual(detail_offsets, [0, 65536, 65536])

    def test_utf8_body_is_paged_by_bytes_without_corrupting_multibyte_text(self) -> None:
        original = _exchange_fixture()
        utf8_body = "é".encode("utf-8") * 32769
        row = replace(original, request_body=utf8_body)
        fixture = _CaidoMCPFixture(
            current_id=self.project_a,
            exchanges_by_project={self.project_a: (row,)},
        )
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")

        exchange = source.list_history(source.list_projects()[0], page_size=1).exchanges[0]

        self.assertEqual(exchange.request_raw.split(b"\r\n\r\n", 1)[1], utf8_body)
        self.assertFalse(exchange.request_truncated)
        offsets = [args["bodyOffset"] for name, args in fixture.calls if name == "caido_get_request"]
        self.assertEqual(offsets, [0, 65536])

    def test_utf8_replacement_expansion_does_not_claim_complete_body(self) -> None:
        row = _exchange_fixture(request_size=1, response_size=0)
        fixture = _CaidoMCPFixture(
            current_id=self.project_a,
            exchanges_by_project={self.project_a: (row,)},
        )

        def transformed_text_call(name: str, args: dict[str, Any]) -> dict[str, Any]:
            result = fixture.call_tool(name, args)
            if name == "caido_get_request":
                result["structuredContent"]["request"]["body"] = "\ufffd"
            return result

        source = CaidoMCPReadSource(transformed_text_call, instance_key="fixture-instance")
        exchange = source.list_history(source.list_projects()[0], page_size=1).exchanges[0]

        self.assertEqual(exchange.request_raw.split(b"\r\n\r\n", 1)[1], "\ufffd".encode("utf-8"))
        self.assertIsNone(exchange.request_truncated)
        self.assertEqual(exchange.request_wire_fidelity, "reconstructed_text")

    def test_empty_selection_is_not_changed_by_cross_project_read(self) -> None:
        fixture = _CaidoMCPFixture(current_id=None, exchanges_by_project=self.projects)
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")
        project = next(row for row in source.list_projects() if row.project_id == self.project_b)

        with self.assertRaisesRegex(CaidoMCPError, "empty selection"):
            source.list_history(project)

        self.assertIsNone(fixture.current_id)
        self.assertFalse(any(name == "caido_select_project" for name, _args in fixture.calls))

    def test_external_selection_change_is_reported_without_overwriting_it(self) -> None:
        fixture = _CaidoMCPFixture(current_id=self.project_a, exchanges_by_project=self.projects)
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")
        project = next(row for row in source.list_projects() if row.project_id == self.project_b)
        fixture.switch_after_list_requests = self.project_c

        with self.assertRaises(CaidoMCPError):
            source.list_history(project)

        self.assertEqual(fixture.current_id, self.project_c)

    def test_nonadvancing_cursor_is_rejected(self) -> None:
        fixture = _CaidoMCPFixture(current_id=self.project_a, exchanges_by_project=self.projects)
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")
        project = next(row for row in source.list_projects() if row.project_id == self.project_a)

        with self.assertRaisesRegex(CaidoMCPError, "did not advance"):
            source._parse_request_page({"requests": [], "hasMore": True, "nextCursor": "cursor-1"}, "cursor-1")

        self.assertEqual(project.project_id, self.project_a)

    def test_multi_id_detail_envelope_is_correlated_to_each_request(self) -> None:
        exchanges = (
            _exchange_fixture(),
            _exchange_fixture(request_size=21, response_size=31),
        )
        fixture = _CaidoMCPFixture(
            current_id=self.project_a,
            exchanges_by_project={self.project_a: exchanges},
        )
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")
        project = source.list_projects()[0]

        page = source.list_history(project, page_size=2)

        self.assertEqual([exchange.ref.request_id for exchange in page.exchanges], ["request-1", "request-2"])
        detail_calls = [args for name, args in fixture.calls if name == "caido_get_request"]
        self.assertEqual(len(detail_calls), 1)
        self.assertEqual(detail_calls[0]["ids"], ["request-1", "request-2"])

    def test_cursor_chain_returns_next_page_and_terminates(self) -> None:
        exchanges = (
            _exchange_fixture(),
            _exchange_fixture(request_size=21, response_size=31),
        )
        fixture = _CaidoMCPFixture(
            current_id=self.project_a,
            exchanges_by_project={self.project_a: exchanges},
        )
        source = CaidoMCPReadSource(fixture.call_tool, instance_key="fixture-instance")
        project = source.list_projects()[0]

        first = source.list_history(project, page_size=1)
        second = source.list_history(project, cursor=first.next_cursor, page_size=1)

        self.assertTrue(first.has_more)
        self.assertEqual(first.next_cursor, "cursor-2")
        self.assertFalse(second.has_more)
        self.assertIsNone(second.next_cursor)
        self.assertEqual([first.exchanges[0].ref.request_id, second.exchanges[0].ref.request_id], ["request-1", "request-2"])
        page_args = [args for name, args in fixture.calls if name == "caido_list_requests"]
        self.assertEqual(page_args[0], {"limit": 1})
        self.assertEqual(page_args[1], {"limit": 1, "after": "cursor-2"})


if __name__ == "__main__":
    unittest.main()
