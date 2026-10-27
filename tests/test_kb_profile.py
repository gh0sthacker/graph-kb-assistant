from __future__ import annotations

import unittest
from unittest.mock import patch

from bb_assistant.kb.redamon_sources import (
    RedAmonSourceAdapter, SOURCE_FAMILIES, SourceBatch, SourceRecord, SourceRequest,
)

LITE = ("tool_docs", "gtfobins", "lolbas")
PROFILES = {"lite": LITE, "full": SOURCE_FAMILIES}


def _manifest(profile: str = "lite", selected=None):
    return {
        "profile": profile,
        "profiles": {**PROFILES, **({profile: selected} if selected is not None else {})},
        "sources": [
            {"id": family,
             **({"include_categories": ["tooling"], "exclude": []} if family == "tool_docs" else {}),
             **({"filter": {"profile": profile}} if family == "nvd" else {})}
            for family in SOURCE_FAMILIES
        ],
    }


class KBProfileTests(unittest.TestCase):
    def test_lite_selects_exactly_three_families_and_never_visits_other_sources(self):
        snapshot = RedAmonSourceAdapter(
            _manifest(), {}, source_manifest_sha256="a" * 64,
        ).build()
        self.assertEqual([row["source_name"] for row in snapshot.source_coverage], list(LITE))
        self.assertEqual(snapshot.profile, "lite")
        self.assertEqual(snapshot.coverage_report()["profile"], "lite")

    def test_full_keeps_seven_family_snapshot_selection(self):
        snapshot = RedAmonSourceAdapter(
            _manifest("full"), {}, source_manifest_sha256="b" * 64,
        ).build()
        self.assertEqual([row["source_name"] for row in snapshot.source_coverage], list(SOURCE_FAMILIES))
        self.assertEqual(snapshot.profile, "full")
        self.assertFalse(snapshot.manifest_blockers)

    def test_lite_and_full_reject_incorrect_profile_family_rosters(self):
        lite = RedAmonSourceAdapter(
            _manifest("lite", SOURCE_FAMILIES), {}, source_manifest_sha256="c" * 64,
        ).build()
        full = RedAmonSourceAdapter(
            _manifest("full", LITE), {}, source_manifest_sha256="d" * 64,
        ).build()
        self.assertIn("manifest_profile_lite_must_select_exactly_three_families", lite.manifest_blockers)
        self.assertIn("manifest_profile_full_must_select_all_seven_families", full.manifest_blockers)
        self.assertNotIn("manifest_profile_must_be_full_for_seven_family_snapshot", full.manifest_blockers)

    def test_coverage_state_is_calculated_from_selected_profile_families(self):
        def make_provider(family):
            def provider(request: SourceRequest) -> SourceBatch:
                return SourceBatch(
                    source_revision="r1",
                    records=(SourceRecord(
                        source_id="doc", title=family, original_text="fixture", source_locator="fixture",
                        metadata={
                            "category_dir": "tooling",
                            **({"description": "fixture"} if family == "exploitdb" else {}),
                        },
                    ),),
                    expected_count=1, fetched_count=1, parsed_count=1,
                    complete=True, notices_complete=True, applied_filters=request.filters,
                )
            return provider

        def build(profile, *, omit=()):
            providers = {name: make_provider(name) for name in SOURCE_FAMILIES if name not in omit}
            adapter = RedAmonSourceAdapter(_manifest(profile), providers, source_manifest_sha256="f" * 64)
            with patch.object(RedAmonSourceAdapter, "_rights_blockers", return_value=()), \
                    patch.object(RedAmonSourceAdapter, "_revision", return_value=("r1", None)), \
                    patch.object(RedAmonSourceAdapter, "_nvd_allowed", return_value=True), \
                    patch("bb_assistant.kb.redamon_sources.chunk_document", return_value=()):
                return adapter.build()

        self.assertEqual(build("lite").coverage_state, "DONE")
        self.assertEqual(build("lite", omit=(LITE[1],)).coverage_state, "PARTIAL")
        self.assertEqual(build("full").coverage_state, "DONE")

    def test_missing_profile_uses_full_default(self):
        manifest = _manifest("full")
        manifest.pop("profile")
        snapshot = RedAmonSourceAdapter(
            manifest, {}, source_manifest_sha256="e" * 64,
        ).build()
        self.assertEqual(snapshot.profile, "full")
        self.assertEqual(len(snapshot.source_coverage), 7)


if __name__ == "__main__":
    unittest.main()
