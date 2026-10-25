"""Pinned-policy NVD CVE API provider for the RedAmon KB rebuild.

This provider deliberately does not import RedAmon's NVD client: that client
can merge stale cache entries into a failed live fetch. NVD is a live source,
so this adapter only returns a complete, freshly fetched set of pages whose
pagination and CVE identities reconcile. Its rights gate must be resolved in
the source manifest before any network request is made.
"""
from __future__ import annotations

from collections.abc import Callable, Mapping
from datetime import datetime, timedelta, timezone
import hashlib
import json
import math
import re
import time
from typing import Any
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener

from .redamon_sources import SourceBatch, SourceRecord, SourceRequest


_API_BASE = "https://services.nvd.nist.gov/rest/json/cves/2.0"
_PAGE_SIZE = 2000
_MAX_DATE_SPAN = timedelta(days=120) - timedelta(milliseconds=1)
_MAX_PAGE_BYTES = 64 * 1024 * 1024
_MAX_TOTAL_BYTES = 2 * 1024 * 1024 * 1024
_NO_KEY_MIN_INTERVAL_SECONDS = 6.5
_FETCH_TIMEOUT_SECONDS = 90
_FETCH_ATTEMPTS = 3
_CVE_ID = re.compile(r"^CVE-\d{4}-\d{4,}$")
_REDAMON_NVD_MAX_CHARS = 480 * 4
_ELIGIBLE_LICENSE_STATUSES = frozenset({"terms_confirmed_with_required_notices"})
_CVE_TERMS_URL = "https://www.cve.org/legal/termsofuse"

PageFetcher = Callable[[Mapping[str, str]], bytes]
Sleeper = Callable[[float], None]
MonotonicClock = Callable[[], float]


class NVDProviderError(ValueError):
    """A fixed, non-sensitive NVD failure with a reportable coverage code."""

    def __init__(self, message: str) -> None:
        super().__init__(message)
        code = "_".join(re.findall(r"[a-z0-9]+", message.casefold()))
        if code.startswith("nvd_"):
            code = code[4:]
        self.coverage_code = f"nvd_{code[:70]}"


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        return None


def _fetch_page(params: Mapping[str, str]) -> bytes:
    """Fetch one bounded NVD page, retrying transient timeouts and throttles."""
    url = f"{_API_BASE}?{urlencode(params)}"
    retryable_statuses = {408, 429, 500, 502, 503, 504}
    last_transient_error: str | None = None
    for attempt in range(_FETCH_ATTEMPTS):
        request = Request(url, headers={
            "Accept": "application/json",
            "User-Agent": "bb-graph-assistant/0.1",
        })
        opener = build_opener(_NoRedirect())
        try:
            response = opener.open(request, timeout=_FETCH_TIMEOUT_SECONDS)
            with response:
                final_url = response.geturl()
                if final_url != url or response.headers.get_content_type().casefold() != "application/json":
                    raise NVDProviderError("NVD API returned an unexpected URL or content type")
                body = response.read(_MAX_PAGE_BYTES + 1)
            if len(body) > _MAX_PAGE_BYTES:
                raise NVDProviderError("NVD API page exceeds the configured byte limit")
            return body
        except HTTPError as exc:
            if exc.code in {301, 302, 303, 307, 308}:
                raise NVDProviderError("NVD API attempted an unexpected redirect") from None
            if exc.code not in retryable_statuses or attempt + 1 >= _FETCH_ATTEMPTS:
                raise NVDProviderError(f"NVD API returned HTTP {exc.code}") from None
            last_transient_error = f"HTTP {exc.code}"
        except (TimeoutError, OSError):
            if attempt + 1 >= _FETCH_ATTEMPTS:
                raise NVDProviderError("NVD API response timed out or the connection failed") from None
            last_transient_error = "timeout_or_connection_error"

        time.sleep(_NO_KEY_MIN_INTERVAL_SECONDS * (attempt + 1))

    raise NVDProviderError(f"NVD API retries exhausted ({last_transient_error or 'unknown'})")


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise NVDProviderError("NVD JSON contains a duplicate object key")
        result[key] = value
    return result


def _parse_page(raw: bytes, *, expected_start: int) -> tuple[dict[str, Any], list[Mapping[str, Any]]]:
    try:
        payload = json.loads(raw.decode("utf-8", errors="strict"), object_pairs_hook=_unique_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise NVDProviderError("NVD API page is not valid UTF-8 JSON") from exc
    if not isinstance(payload, dict):
        raise NVDProviderError("NVD API response is not a JSON object")
    if payload.get("format") != "NVD_CVE":
        raise NVDProviderError("NVD API response format is not NVD_CVE")
    if payload.get("version") not in {None, "2.0"}:
        raise NVDProviderError("NVD API response version is unsupported")

    total = payload.get("totalResults")
    start = payload.get("startIndex")
    count = payload.get("resultsPerPage")
    vulnerabilities = payload.get("vulnerabilities")
    if type(total) is not int or total < 0:
        raise NVDProviderError("NVD API totalResults is missing or invalid")
    if type(start) is not int or start != expected_start:
        raise NVDProviderError("NVD API startIndex does not match the requested page")
    if type(count) is not int or count < 0:
        raise NVDProviderError("NVD API resultsPerPage is missing or invalid")
    if not isinstance(vulnerabilities, list) or len(vulnerabilities) != count:
        raise NVDProviderError("NVD API page count does not match vulnerabilities")
    if start + count > total:
        raise NVDProviderError("NVD API page extends beyond totalResults")
    if total and not count:
        raise NVDProviderError("NVD API returned an empty page before totalResults")
    if not all(isinstance(item, Mapping) for item in vulnerabilities):
        raise NVDProviderError("NVD API vulnerabilities contains a non-object record")
    return payload, vulnerabilities


def _millisecond_iso(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _redamon_search_text(vulnerability: Mapping[str, Any]) -> tuple[str, dict[str, object]]:
    """Match the pinned RedAmon NVD normalization and ``to_chunks`` text."""
    cve = vulnerability.get("cve")
    if not isinstance(cve, Mapping):
        raise NVDProviderError("NVD vulnerability record has no cve object")
    descriptions = cve.get("descriptions", [])
    description = ""
    if isinstance(descriptions, list):
        for item in descriptions:
            if isinstance(item, Mapping) and item.get("lang") == "en":
                description = item.get("value", "") if isinstance(item.get("value", ""), str) else ""
                break

    cvss_score: object = None
    severity = ""
    metrics = cve.get("metrics", {})
    if isinstance(metrics, Mapping):
        for metric_name in ("cvssMetricV31", "cvssMetricV30"):
            rows = metrics.get(metric_name, [])
            if rows:
                first = rows[0] if isinstance(rows, list) else None
                cvss = first.get("cvssData", {}) if isinstance(first, Mapping) else {}
                if isinstance(cvss, Mapping):
                    cvss_score = cvss.get("baseScore")
                    raw_severity = cvss.get("baseSeverity", "")
                    severity = raw_severity.lower() if isinstance(raw_severity, str) else ""
                break

    products: list[str] = []
    configurations = cve.get("configurations", [])
    if isinstance(configurations, list):
        for config in configurations:
            nodes = config.get("nodes", []) if isinstance(config, Mapping) else []
            if not isinstance(nodes, list):
                continue
            for node in nodes:
                matches = node.get("cpeMatch", []) if isinstance(node, Mapping) else []
                if not isinstance(matches, list):
                    continue
                for match in matches:
                    criteria = match.get("criteria", "") if isinstance(match, Mapping) else ""
                    if isinstance(criteria, str) and criteria:
                        products.append(criteria)

    content = description
    if cvss_score is not None:
        content += f"\nCVSS: {cvss_score} ({severity})"
    if products:
        content += f"\nAffected: {', '.join(products[:10])}"
    # RedAmon's ChunkStrategy.chunk_structured clips atomic CVE chunks at
    # max_tokens * 4 chars and appends an ellipsis after the last whole word.
    if len(content) // 4 > 480:
        content = content[:_REDAMON_NVD_MAX_CHARS].rsplit(" ", 1)[0] + "..."
    published = cve.get("published", "")
    published_date = published[:10] if isinstance(published, str) and published else None
    metadata: dict[str, object] = {
        "cvss_score": cvss_score,
        "severity": severity,
        "affected_products": ", ".join(products[:10]),
        "published_date": published_date,
    }
    return content, metadata


def _date_windows(start: datetime, end: datetime) -> list[tuple[datetime, datetime]]:
    """Return non-overlapping millisecond windows below NVD's 120-day limit."""
    if start.tzinfo is None or end.tzinfo is None or start > end:
        raise NVDProviderError("NVD date window must be ordered and timezone-aware")
    windows: list[tuple[datetime, datetime]] = []
    current = start
    while current <= end:
        window_end = min(current + _MAX_DATE_SPAN, end)
        windows.append((current, window_end))
        current = window_end + timedelta(milliseconds=1)
    return windows


class RedAmonNVDProvider:
    """Fetch all CVEs in the explicitly requested publication-date window.

    The provider keeps each original NVD CVE JSON object (including fields it
    does not yet understand), while the response and artifact digests bind the
    snapshot to the exact pages received. It does not use or fall back to a
    local cache. A failed page aborts this source batch so partial data cannot
    masquerade as the complete NVD subset.
    """

    def __init__(
        self,
        *,
        fetcher: PageFetcher = _fetch_page,
        sleeper: Sleeper = time.sleep,
        monotonic: MonotonicClock = time.monotonic,
        min_interval_seconds: float = _NO_KEY_MIN_INTERVAL_SECONDS,
    ) -> None:
        if not callable(fetcher) or not callable(sleeper) or not callable(monotonic):
            raise TypeError("fetcher, sleeper, and monotonic must be callable")
        if (
            isinstance(min_interval_seconds, bool)
            or not isinstance(min_interval_seconds, (int, float))
            or not math.isfinite(min_interval_seconds)
            or min_interval_seconds < 0
        ):
            raise NVDProviderError("min_interval_seconds must be a non-negative number")
        self._fetcher = fetcher
        self._sleeper = sleeper
        self._monotonic = monotonic
        self._min_interval = float(min_interval_seconds)

    def __call__(self, request: SourceRequest) -> SourceBatch:
        if request.source_name != "nvd":
            raise NVDProviderError("provider only supports the nvd source")
        if request.source_spec.get("url") != _API_BASE:
            raise NVDProviderError("NVD endpoint differs from the audited source")
        if request.source_spec.get("kind") != "paginated_api":
            raise NVDProviderError("NVD source is not declared as a paginated API")
        self._require_resolved_source_terms(request.source_spec)

        filters = dict(request.filters)
        profile = filters.get("profile")
        min_cvss = filters.get("nvd_min_cvss")
        lookback_days = filters.get("nvd_lookback_days")
        if profile != "full":
            raise NVDProviderError("NVD provider requires the full RedAmon profile")
        if isinstance(min_cvss, bool) or not isinstance(min_cvss, (int, float)) or float(min_cvss) != 0.0:
            raise NVDProviderError("NVD full profile must not apply a positive CVSS cutoff")
        if type(lookback_days) is not int or lookback_days < 1:
            raise NVDProviderError("NVD lookback window must be a positive integer")

        fetched_at = datetime.now(timezone.utc)
        fetched_at = fetched_at.replace(microsecond=(fetched_at.microsecond // 1000) * 1000)
        range_start = fetched_at - timedelta(days=lookback_days)
        windows = _date_windows(range_start, fetched_at)
        response_digest = hashlib.sha256()
        artifact_digest = hashlib.sha256()
        artifacts_size = 0
        expected_count = 0
        fetched_count = 0
        seen_ids: set[str] = set()
        pending_records: list[tuple[Mapping[str, Any], datetime, datetime, int, int, str]] = []
        last_call_at: float | None = None

        def fetch(params: Mapping[str, str]) -> bytes:
            nonlocal last_call_at, artifacts_size
            if last_call_at is not None:
                delay = self._min_interval - (self._monotonic() - last_call_at)
                if delay > 0:
                    self._sleeper(delay)
            raw = self._fetcher(params)
            last_call_at = self._monotonic()
            if not isinstance(raw, bytes) or not raw:
                raise NVDProviderError("NVD API returned an empty or non-byte page")
            if len(raw) > _MAX_PAGE_BYTES:
                raise NVDProviderError("NVD API page exceeds the configured byte limit")
            artifacts_size += len(raw)
            if artifacts_size > _MAX_TOTAL_BYTES:
                raise NVDProviderError("NVD response set exceeds the configured total byte limit")
            encoded_params = json.dumps(dict(params), sort_keys=True, separators=(",", ":")).encode()
            response_digest.update(len(raw).to_bytes(8, "big"))
            response_digest.update(raw)
            artifact_digest.update(len(encoded_params).to_bytes(8, "big"))
            artifact_digest.update(encoded_params)
            artifact_digest.update(len(raw).to_bytes(8, "big"))
            artifact_digest.update(raw)
            return raw

        for window_start, window_end in windows:
            start_text = _millisecond_iso(window_start)
            end_text = _millisecond_iso(window_end)
            start_index = 0
            window_total: int | None = None
            while True:
                params = {
                    "pubStartDate": start_text,
                    "pubEndDate": end_text,
                    "resultsPerPage": str(_PAGE_SIZE),
                    "startIndex": str(start_index),
                }
                raw = fetch(params)
                payload, vulnerabilities = _parse_page(raw, expected_start=start_index)
                total = payload["totalResults"]
                if window_total is None:
                    window_total = total
                    expected_count += total
                elif window_total != total:
                    raise NVDProviderError("NVD totalResults changed while paging a date window")
                if payload["resultsPerPage"] > _PAGE_SIZE:
                    raise NVDProviderError("NVD API returned more records than the requested page size")

                page_sha256 = hashlib.sha256(raw).hexdigest()
                for position, vulnerability in enumerate(vulnerabilities):
                    cve = vulnerability.get("cve")
                    if not isinstance(cve, Mapping):
                        raise NVDProviderError("NVD vulnerability record has no cve object")
                    cve_id = cve.get("id")
                    published = cve.get("published")
                    if not isinstance(cve_id, str) or not _CVE_ID.fullmatch(cve_id):
                        raise NVDProviderError("NVD vulnerability record has an invalid CVE ID")
                    if cve_id in seen_ids:
                        raise NVDProviderError("NVD pagination returned a duplicate CVE ID")
                    if not isinstance(published, str):
                        raise NVDProviderError("NVD CVE record has no publication timestamp")
                    try:
                        published_at = datetime.fromisoformat(published.replace("Z", "+00:00"))
                    except ValueError as exc:
                        raise NVDProviderError("NVD CVE publication timestamp is invalid") from exc
                    # NVD's live JSON currently emits some ISO date-time fields
                    # without an explicit offset. Its publication-date API
                    # filters are UTC, so interpret those naive values as UTC;
                    # normalize explicit offsets before checking the same window.
                    if published_at.tzinfo is None:
                        published_at = published_at.replace(tzinfo=timezone.utc)
                    else:
                        published_at = published_at.astimezone(timezone.utc)
                    if not range_start <= published_at <= fetched_at:
                        raise NVDProviderError("NVD CVE publication timestamp is outside the requested window")
                    seen_ids.add(cve_id)
                    pending_records.append((
                        vulnerability,
                        window_start,
                        window_end,
                        start_index,
                        position,
                        page_sha256,
                    ))
                fetched_count += len(vulnerabilities)
                start_index += len(vulnerabilities)
                if start_index >= window_total:
                    break
                if not vulnerabilities:
                    raise NVDProviderError("NVD pagination stopped before the advertised total")

        if fetched_count != expected_count or len(seen_ids) != expected_count:
            raise NVDProviderError("NVD fetched CVE count does not match reconciled API totals")

        response_sha256 = response_digest.hexdigest()
        artifact_sha256 = artifact_digest.hexdigest()
        revision = f"{fetched_at.isoformat()};response_sha256={response_sha256}"
        base_url = "https://nvd.nist.gov/vuln/detail/"
        records: list[SourceRecord] = []
        for vulnerability, win_start, win_end, page_start, position, page_hash in pending_records:
            cve = vulnerability["cve"]
            cve_id = str(cve["id"])
            search_text, normalized_metadata = _redamon_search_text(vulnerability)
            records.append(SourceRecord(
                source_id=cve_id,
                title=cve_id,
                original_text=json.dumps(
                    vulnerability, ensure_ascii=False, sort_keys=True, separators=(",", ":")
                ),
                source_locator=(
                    f"pub-window:{_millisecond_iso(win_start)}..{_millisecond_iso(win_end)};"
                    f"startIndex:{page_start};item:{position};id:{cve_id}"
                ),
                source_url=base_url + cve_id,
                source_revision=revision,
                metadata={
                    "cve_id": cve_id,
                    "published": cve.get("published"),
                    "last_modified": cve.get("lastModified"),
                    "source_identifier": cve.get("sourceIdentifier"),
                    "window_start": _millisecond_iso(win_start),
                    "window_end": _millisecond_iso(win_end),
                    "page_start_index": page_start,
                    "page_record_index": position,
                    "page_sha256": page_hash,
                    **normalized_metadata,
                },
                search_text=search_text,
            ))

        attribution = str(request.source_spec.get("attribution") or "NIST National Vulnerability Database")
        evidence_url = str(request.source_spec.get("license_evidence") or _API_BASE)
        nvd_citation = str(request.source_spec.get("nvd_citation") or "National Vulnerability Database, NIST.")
        cve_terms_url = str(request.source_spec.get("cve_terms_evidence") or _CVE_TERMS_URL)
        cve_copyright = str(request.source_spec.get("cve_copyright_designation") or "")
        cve_terms_reviewed_at = str(request.source_spec.get("cve_terms_reviewed_at") or "")
        records.append(SourceRecord(
            source_id="NOTICE:nvd-attribution",
            title="NVD attribution, citation, and data notice",
            original_text=(
                f"Source: NIST National Vulnerability Database.\n"
                f"Attribution: {attribution}\n"
                f"Dataset citation: {nvd_citation} (retrieved {fetched_at.date().isoformat()}).\n"
                "NIST non-SRD data notice: This data/work was created by employees of the National Institute of Standards and Technology (NIST), an agency of the Federal Government. Pursuant to title 17 United States Code Section 105, works of NIST employees are not subject to copyright protection in the United States. This data/work may be subject to foreign copyright.\n"
                "NIST requests appropriate acknowledgement as the source of its data/work.\n"
                f"Source endpoint: {_API_BASE}\n"
                f"License and attribution evidence: {evidence_url}\n"
            ),
            source_locator="source_manifest:attribution",
            source_url=evidence_url,
            source_revision=revision,
            metadata={
                "notice_name": "NVD attribution",
                "attribution": attribution,
                "dataset_citation": nvd_citation,
                "retrieved_at": fetched_at.isoformat(),
                "nvd_terms_url": evidence_url,
            },
            document_role="license_notice",
        ))
        records.append(SourceRecord(
            source_id="NOTICE:cve-program-terms",
            title="CVE Program copyright designation and terms notice",
            original_text=(
                "Source: CVE Program Terms of Use.\n"
                f"Required MITRE copyright designation: {cve_copyright}\n"
                "CVE Usage license: MITRE hereby grants you a perpetual, worldwide, non-exclusive, no-charge, royalty-free, irrevocable copyright license to reproduce, prepare derivative works of, publicly display, publicly perform, sublicense, and distribute Common Vulnerabilities and Exposures (CVE). Any copy you make for such purposes is authorized provided that you reproduce MITRE's copyright designation and this license in any such copy.\n"
                f"Terms URL: {cve_terms_url}\n"
                f"Terms reviewed on: {cve_terms_reviewed_at}\n"
                "Scope note: NVD records include CVE List content such as CNA-supplied descriptions and references, and may include CISA-ADP data. Provider/source identifiers are preserved in the full record; linked reference pages are not fetched or copied.\n"
            ),
            source_locator="source_manifest:cve_terms",
            source_url=cve_terms_url,
            source_revision=revision,
            metadata={
                "notice_name": "CVE Program terms",
                "copyright_designation": cve_copyright,
                "terms_url": cve_terms_url,
                "terms_reviewed_at": cve_terms_reviewed_at,
                "retrieved_at": fetched_at.isoformat(),
                "embedded_content_review": "complete",
            },
            document_role="license_notice",
        ))

        return SourceBatch(
            source_revision=revision,
            records=tuple(records),
            expected_count=expected_count,
            fetched_count=fetched_count,
            parsed_count=fetched_count,
            rejected_count=0,
            complete=True,
            artifact_sha256=artifact_sha256,
            fetched_at=fetched_at.isoformat(),
            response_sha256=response_sha256,
            applied_filters=filters,
            notices_complete=True,
            limitations=(
                "Only the manifest's publication-date window is included; this does not represent the full lifetime NVD corpus.",
                "NVD is a live API without a transaction snapshot token; page totals, identities, and response hashes were reconciled during this fetch.",
                "Original CVE JSON objects are retained as canonical JSON; exact response bytes are bound by response and artifact SHA-256 digests.",
            ),
        )

    @staticmethod
    def _require_resolved_source_terms(spec: Mapping[str, object]) -> None:
        status = spec.get("license_status")
        if status not in _ELIGIBLE_LICENSE_STATUSES:
            raise NVDProviderError("NVD license and attribution review is not complete")
        if spec.get("embedded_content_review") != "complete":
            raise NVDProviderError("NVD embedded-content review is not complete")
        if status == "terms_confirmed_with_required_notices":
            if spec.get("cve_terms_evidence") != _CVE_TERMS_URL:
                raise NVDProviderError("NVD CVE Program terms evidence is missing or unexpected")
            designation = spec.get("cve_copyright_designation")
            if not isinstance(designation, str) or "The MITRE Corporation" not in designation:
                raise NVDProviderError("NVD CVE copyright designation is missing")
            reviewed_at = spec.get("cve_terms_reviewed_at")
            try:
                reviewed_date = datetime.fromisoformat(str(reviewed_at)).date()
            except ValueError:
                raise NVDProviderError("NVD CVE terms review date is invalid") from None
            current_date = datetime.now(timezone.utc).date()
            if reviewed_date > current_date or (current_date - reviewed_date).days > 365:
                raise NVDProviderError("NVD CVE terms review is stale")
            if str(current_date.year) not in designation or reviewed_date.year != current_date.year:
                raise NVDProviderError("NVD CVE copyright designation must be rechecked for the current year")
            if not isinstance(spec.get("nvd_citation"), str) or "10.18434/M3436" not in spec["nvd_citation"]:
                raise NVDProviderError("NVD dataset citation is missing")
        if not spec.get("license_id") or not spec.get("license_evidence") or not spec.get("attribution"):
            raise NVDProviderError("NVD license evidence or attribution is incomplete")
