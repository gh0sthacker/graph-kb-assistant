from __future__ import annotations

import re
import tempfile
import unittest
from pathlib import Path

from bb_assistant.kb.thermal import ThermalLimitReached, ThermalMonitor
from bb_assistant.kb.worker_lock import embedding_worker_lock
from bb_assistant.kb.canonical import SourceDocument, chunk_document
from bb_assistant.kb.index import KBIndex


class FakeClock:
    def __init__(self, on_sleep=None) -> None:
        self.now = 0.0
        self.on_sleep = on_sleep

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.now += seconds
        if self.on_sleep is not None:
            self.on_sleep(self.now)


class ThermalMonitorTests(unittest.TestCase):
    def test_reads_celsius_and_millidegrees(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            monitor = ThermalMonitor(sensor)
            sensor.write_text("62.5\n", encoding="ascii")
            self.assertEqual(monitor.check(), 62.5)
            sensor.write_text("62500\n", encoding="ascii")
            self.assertEqual(monitor.check(), 62.5)

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

    def test_waits_for_full_cool_dwell(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("62.0\n", encoding="ascii")
            clock = FakeClock()
            monitor = ThermalMonitor(sensor)
            result = monitor.wait_for_cool_dwell(
                dwell_seconds=3,
                sample_interval_seconds=1,
                monotonic=clock.monotonic,
                sleep=clock.sleep,
            )
            self.assertEqual(result, 62.0)
            self.assertEqual(clock.now, 3.0)

    def test_hot_reading_resets_cool_dwell(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("62.0\n", encoding="ascii")

            def change_temperature(now: float) -> None:
                if now == 2.0:
                    sensor.write_text("70000\n", encoding="ascii")
                elif now == 4.0:
                    sensor.write_text("65000\n", encoding="ascii")

            clock = FakeClock(on_sleep=change_temperature)
            monitor = ThermalMonitor(sensor)
            monitor.wait_for_cool_dwell(
                dwell_seconds=3,
                sample_interval_seconds=1,
                monotonic=clock.monotonic,
                sleep=clock.sleep,
            )
            self.assertEqual(clock.now, 7.0)

    def test_sensor_failure_aborts_cool_dwell(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            monitor = ThermalMonitor(sensor)
            clock = FakeClock()
            with self.assertRaisesRegex(RuntimeError, "sensor is unavailable"):
                monitor.wait_for_cool_dwell(
                    dwell_seconds=3,
                    sample_interval_seconds=1,
                    monotonic=clock.monotonic,
                    sleep=clock.sleep,
                )
            self.assertEqual(clock.now, 0.0)

    def test_rejects_aggressive_or_invalid_limits(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                ThermalMonitor(Path(directory) / "temp", stop_celsius=90)
            with self.assertRaises(ValueError):
                ThermalMonitor(Path(directory) / "temp", stop_celsius=71)
            sensor = Path(directory) / "temp"
            sensor.write_text("not-a-temperature", encoding="ascii")
            with self.assertRaisesRegex(RuntimeError, "stopping embedding"):
                ThermalMonitor(sensor).check()

    def test_ct108_embed_unit_keeps_cpu_and_temperature_limits(self) -> None:
        project_root = Path(__file__).resolve().parents[1]
        unit = (project_root / "deploy/graph-kb-assistant-kb-embed.service").read_text(encoding="utf-8")
        self.assertIn("CPUQuota=20%", unit)
        self.assertIn("Restart=no", unit)
        for name in ("OMP_NUM_THREADS", "MKL_NUM_THREADS", "OPENBLAS_NUM_THREADS"):
            self.assertRegex(unit, rf"(?m)^Environment={name}=1$")
        start = next(line for line in unit.splitlines() if line.startswith("ExecStart="))
        self.assertRegex(start, r"(?:^| )embed(?: |$)")
        self.assertRegex(start, r"(?:^| )--batch-size 2(?: |$)")
        self.assertRegex(start, r"(?:^| )--threads 1(?: |$)")
        match = re.search(r"(?:^| )--stop-temperature-c ([0-9.]+)(?: |$)", start)
        self.assertIsNotNone(match)
        self.assertLessEqual(float(match.group(1)), 70.0)

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
