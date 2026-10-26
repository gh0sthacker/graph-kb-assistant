from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from bb_assistant.kb.thermal import ThermalLimitReached, ThermalMonitor
from bb_assistant.kb.worker_lock import embedding_worker_lock
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex


class ThermalMonitorTests(unittest.TestCase):
    def test_reads_celsius_and_millidegrees(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            monitor = ThermalMonitor(sensor)
            sensor.write_text("72.5\n", encoding="ascii")
            self.assertEqual(monitor.check(), 72.5)
            sensor.write_text("72500\n", encoding="ascii")
            self.assertEqual(monitor.check(), 72.5)

    def test_resolves_package_sensor_from_hwmon_labels(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            hwmon = Path(directory) / "hwmon9"
            hwmon.mkdir()
            (hwmon / "name").write_text("coretemp\n", encoding="ascii")
            (hwmon / "temp1_label").write_text("Package id 0\n", encoding="ascii")
            (hwmon / "temp1_input").write_text("69000\n", encoding="ascii")
            (hwmon / "temp2_label").write_text("Core 0\n", encoding="ascii")
            (hwmon / "temp2_input").write_text("70000\n", encoding="ascii")
            self.assertEqual(ThermalMonitor(Path(directory)).check(), 69.0)

    def test_hwmon_path_fails_closed_when_package_sensor_is_ambiguous(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            for number in (1, 2):
                hwmon = Path(directory) / f"hwmon{number}"
                hwmon.mkdir()
                (hwmon / "name").write_text("coretemp\n", encoding="ascii")
                (hwmon / "temp1_label").write_text("Package id 0\n", encoding="ascii")
                (hwmon / "temp1_input").write_text("69000\n", encoding="ascii")
            with self.assertRaisesRegex(RuntimeError, "unique coretemp"):
                ThermalMonitor(Path(directory)).check()

    def test_stops_at_limit_and_fails_closed_when_unreadable(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            monitor = ThermalMonitor(sensor)
            sensor.write_text("80000\n", encoding="ascii")
            with self.assertRaises(ThermalLimitReached):
                monitor.check()
            sensor.unlink()
            with self.assertRaisesRegex(RuntimeError, "stopping embedding"):
                monitor.check()

    def test_rejects_aggressive_or_invalid_limits(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                ThermalMonitor(Path(directory) / "temp", stop_celsius=90)
            sensor = Path(directory) / "temp"
            sensor.write_text("not-a-temperature", encoding="ascii")
            with self.assertRaisesRegex(RuntimeError, "stopping embedding"):
                ThermalMonitor(sensor).check()

    def test_embedding_worker_lock_prevents_a_duplicate_process(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with embedding_worker_lock(root):
                with self.assertRaisesRegex(RuntimeError, "already active"):
                    with embedding_worker_lock(root):
                        self.fail("a second worker acquired the lock")

    def test_rights_blocked_exploitdb_chunks_never_appear_in_search(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            document = SourceDocument.create(
                source_name="exploitdb", source_id="metadata.csv:1", title="SQL injection",
                text="SQL injection payload metadata", metadata={"document_role": "content"},
            )
            index = KBIndex(Path(directory) / "index.sqlite3")
            index.replace_snapshot(
                tuple(chunk_document(document)), "e" * 64, sources=(document,),
            )
            self.assertEqual(index.search("SQL injection"), ())
            self.assertEqual(index.search("SQL injection", source_names=("exploitdb",)), ())


if __name__ == "__main__":
    unittest.main()
