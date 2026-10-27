from __future__ import annotations

import unittest

from bb_assistant.kb.redamon_sources import RedAmonSourceAdapter, SOURCE_FAMILIES

LITE = ("tool_docs", "gtfobins", "lolbas")
PROFILES = {"lite": LITE, "full": SOURCE_FAMILIES}


def _manifest(profile: str = "lite", selected=None):
    return {
        "profile": profile,
        "profiles": {**PROFILES, **({profile: selected} if selected is not None else {})},
        "sources": [{"id": family} for family in SOURCE_FAMILIES],
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
