"""Read-only Caido source contract.

The production implementation should call Caido's public MCP/Client SDK
interface. It must not read Caido's internal database directly.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol, Sequence

from .domain import CaidoExchangeRef, CaidoProjectRef, HttpExchange


@dataclass(frozen=True, slots=True)
class HistoryPage:
    exchanges: tuple[HttpExchange, ...]
    next_cursor: str | None
    has_more: bool


@dataclass(frozen=True, slots=True)
class RequestMetadata:
    """A bounded Caido list row, before any request/response body is fetched."""

    request_id: str
    method: str
    url: str
    status_code: int | None


@dataclass(frozen=True, slots=True)
class RequestMetadataPage:
    requests: tuple[RequestMetadata, ...]
    next_cursor: str | None
    has_more: bool


class CaidoReadSource(Protocol):
    """Read one open project's history, retaining opaque cursor semantics."""

    def list_projects(self) -> Sequence[CaidoProjectRef]: ...

    def list_history(
        self,
        project: CaidoProjectRef,
        *,
        cursor: str | None = None,
        page_size: int = 100,
    ) -> HistoryPage: ...

    def list_request_page(
        self,
        project: CaidoProjectRef,
        *,
        cursor: str | None = None,
        page_size: int = 100,
        httpql: str | None = None,
    ) -> RequestMetadataPage: ...

    def fetch_history_page(
        self,
        project: CaidoProjectRef,
        page: RequestMetadataPage,
        *,
        skip_request_ids: frozenset[str] = frozenset(),
        max_body_bytes: int = 128 * 1024 * 1024,
    ) -> HistoryPage: ...

    def get_exchange(self, ref: CaidoExchangeRef) -> HttpExchange | None: ...


def validate_page(page: HistoryPage, project: CaidoProjectRef, previous_cursor: str | None) -> None:
    """Reject cross-project rows and non-advancing cursors before persistence."""
    for exchange in page.exchanges:
        if not exchange.ref.project.same_project(project):
            raise ValueError("Caido page contains an exchange from a different project")
    if page.has_more and (not page.next_cursor or page.next_cursor == previous_cursor):
        raise ValueError("Caido pagination did not advance")
