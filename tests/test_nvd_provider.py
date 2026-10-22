"""Offline contract checks for the NVD source adapter."""
from __future__ import annotations

from datetime import datetime, timedelta, timezone
import hashlib
import json
import unittest

from bb_assistant.kb.nvd import RedAmonNVDProvider, _date_windows
from bb_assistant.kb.redamon_sources import RedAmonSourceAdapter, SourceRequest


_API = "https://services.nvd.nist.gov/rest/json/cves/2.0"
_FILTERS = {"profile": "full", "nvd_min_cvss": 0.0, "nvd_lookback_days": 730}


def _source_spec(*, eligible: bool) -> dict[str, object]:
    current_year = datetime.now(timezone.utc).year
    return {
        "id": "nvd",
        "kind": "paginated_api",
        "url": _API,
        "license_id": "NIST-public-data-plus-CVE-Program-Terms-of-Use",
        "license_evidence": "https://www.nist.gov/open/copyright-fair-use-and-licensing-statements-srd-data-software-and-technical-series-publications",
        "attribution": "NIST National Vulnerability Database",
        "nvd_citation": "National Vulnerability Database, NIST Public Data Repository, DOI 10.18434/M3436.",
        "cve_terms_evidence": "https://www.cve.org/legal/termsofuse",
        "cve_terms_reviewed_at": datetime.now(timezone.utc).date().isoformat(),
        "cve_copyright_designation": f"Copyright © 1999-{current_year}, The MITRE Corporation.",
        "license_status": "terms_confirmed_with_required_notices" if eligible else "attribution_and_embedded-content-review",
        "embedded_content_review": "complete" if eligible else "pending",
        "filter": dict(_FILTERS),
    }


def _page(*, start: int, total: int, items: list[dict[str, object]]) -> bytes:
    return json.dumps({
        "format": "NVD_CVE",
        "version": "2.0",
        "resultsPerPage": len(items),
        "startIndex": start,
        "totalResults": total,
        "vulnerabilities": items,
    }, separators=(",", ":")).encode()


def _vuln(cve_id: str, published: str) -> dict[str, object]:
    return {
        "cve": {
            "id": cve_id,
            "published": published,
            "lastModified": published,
            "descriptions": [{"lang": "en", "value": "fixture summary"}],
            "futureSchemaField": {"provider": "CISA ADP", "value": "retained"},
        }
    }


class NVDProviderTests(unittest.TestCase):
    def test_date_windows_are_contiguous_non_overlapping_and_below_120_days(self) -> None:
        start = datetime(2025, 1, 1, tzinfo=timezone.utc)
        end = start + timedelta(days=730)
        windows = _date_windows(start, end)

        self.assertEqual(windows[0][0], start)
        self.assertEqual(windows[-1][1], end)
        self.assertTrue(all(right[0] - left[1] == timedelta(milliseconds=1) for left, right in zip(windows, windows[1:])))
        self.assertTrue(all(right - left < timedelta(days=120) for left, right in windows))

    def test_full_profile_pages_reconcile_and_preserve_unknown_cve_fields(self) -> None:
        calls: list[dict[str, str]] = []

        def fetcher(params):
            query = dict(params)
            calls.append(query)
            start_index = int(query["startIndex"])
            start_date = datetime.fromisoformat(query["pubStartDate"].replace("Z", "+00:00"))
            if start_date == datetime.fromisoformat(calls[0]["pubStartDate"].replace("Z", "+00:00")):
                first = start_date + timedelta(seconds=1)
                records = [
                    _vuln("CVE-2024-10001", first.isoformat().replace("+00:00", "Z")),
                    _vuln("CVE-2024-10002", (first + timedelta(seconds=1)).isoformat().replace("+00:00", "Z")),
                ]
                if start_index == 0:
                    return _page(start=0, total=2, items=records[:1])
                return _page(start=1, total=2, items=records[1:])
            return _page(start=start_index, total=0, items=[])

        provider = RedAmonNVDProvider(fetcher=fetcher, min_interval_seconds=0)
        request = SourceRequest("nvd", "full", _source_spec(eligible=True), None, _FILTERS)
        batch = provider(request)

        content = [record for record in batch.records if record.document_role == "content"]
        notices = [record for record in batch.records if record.document_role == "license_notice"]
        self.assertEqual(len(calls), len(_date_windows(
            datetime.fromisoformat(calls[0]["pubStartDate"].replace("Z", "+00:00")),
            datetime.fromisoformat(calls[-1]["pubEndDate"].replace("Z", "+00:00")),
        )) + 1)
        self.assertEqual(batch.expected_count, 2)
        self.assertEqual(batch.fetched_count, 2)
        self.assertEqual(batch.parsed_count, 2)
        self.assertTrue(batch.complete)
        self.assertTrue(batch.notices_complete)
        self.assertEqual({record.source_id for record in notices}, {"NOTICE:nvd-attribution", "NOTICE:cve-program-terms"})
        nvd_notice = next(record for record in notices if record.source_id == "NOTICE:nvd-attribution")
        cve_notice = next(record for record in notices if record.source_id == "NOTICE:cve-program-terms")
        self.assertIn("10.18434/M3436", nvd_notice.original_text)
        self.assertIn("National Institute of Standards and Technology", nvd_notice.original_text)
        self.assertIn("The MITRE Corporation", cve_notice.original_text)
        self.assertIn("provided that you reproduce MITRE's copyright designation and this license", cve_notice.original_text)
        self.assertEqual(batch.applied_filters, _FILTERS)
        self.assertRegex(batch.response_sha256 or "", r"^[0-9a-f]{64}$")
        self.assertRegex(batch.artifact_sha256 or "", r"^[0-9a-f]{64}$")
        self.assertTrue(batch.source_revision.endswith(f"response_sha256={batch.response_sha256}"))
        self.assertEqual(json.loads(content[0].original_text)["cve"]["futureSchemaField"]["value"], "retained")
        self.assertEqual(content[0].metadata["page_sha256"], hashlib.sha256(_page(
            start=0,
            total=2,
            items=[_vuln("CVE-2024-10001", json.loads(content[0].original_text)["cve"]["published"])],
        )).hexdigest())

    def test_unresolved_rights_gate_does_not_call_provider(self) -> None:
        called = False

        def fetcher(_params):
            nonlocal called
            called = True
            return b"{}"

        provider = RedAmonNVDProvider(fetcher=fetcher, min_interval_seconds=0)
        manifest = {"profile": "full", "sources": [_source_spec(eligible=False)]}
        adapter = RedAmonSourceAdapter(
            manifest,
            {"nvd": provider},
            source_manifest_sha256="a" * 64,
        )
        snapshot = adapter.build()

        nvd_coverage = next(row for row in snapshot.source_coverage if row["source_name"] == "nvd")
        self.assertEqual(nvd_coverage["state"], "blocked")
        self.assertFalse(called)

    def test_public_domain_label_alone_cannot_bypass_cve_notice_gate(self) -> None:
        called = False

        def fetcher(_params):
            nonlocal called
            called = True
            return b"{}"

        provider = RedAmonNVDProvider(fetcher=fetcher, min_interval_seconds=0)
        spec = _source_spec(eligible=True)
        spec["license_status"] = "public_domain_confirmed"
        manifest = {"profile": "full", "sources": [spec]}
        adapter = RedAmonSourceAdapter(
            manifest,
            {"nvd": provider},
            source_manifest_sha256="b" * 64,
        )
        snapshot = adapter.build()

        nvd_coverage = next(row for row in snapshot.source_coverage if row["source_name"] == "nvd")
        self.assertEqual(nvd_coverage["state"], "blocked")
        self.assertIn("nvd_requires_composite_terms_and_notice_status", nvd_coverage["blockers"])
        self.assertFalse(called)

    def test_stale_or_wrong_year_cve_notice_is_rejected(self) -> None:
        spec = _source_spec(eligible=True)
        spec["cve_terms_reviewed_at"] = "2025-10-21"
        spec["cve_copyright_designation"] = "Copyright © 1999-2025, The MITRE Corporation."

        with self.assertRaisesRegex(ValueError, "review is stale|rechecked for the current year"):
            RedAmonNVDProvider._require_resolved_source_terms(spec)

    def test_duplicate_cve_across_pages_aborts_the_batch(self) -> None:
        def fetcher(params):
            start_index = int(params["startIndex"])
            published = datetime.fromisoformat(params["pubStartDate"].replace("Z", "+00:00"))
            published = (published + timedelta(seconds=1)).isoformat().replace("+00:00", "Z")
            duplicate = _vuln("CVE-2024-10001", published)
            if start_index == 0:
                return _page(start=0, total=2, items=[duplicate])
            return _page(start=1, total=2, items=[duplicate])

        provider = RedAmonNVDProvider(fetcher=fetcher, min_interval_seconds=0)
        request = SourceRequest("nvd", "full", _source_spec(eligible=True), None, _FILTERS)
        with self.assertRaisesRegex(ValueError, "duplicate CVE ID"):
            provider(request)


if __name__ == "__main__":
    unittest.main()
