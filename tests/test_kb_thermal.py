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

    def test_check_sustained_ignores_single_hot_spike(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("65000\n", encoding="ascii")
            readings = iter(("80000\n", "65000\n", "66000\n", "67000\n"))
            clock = FakeClock(on_sleep=lambda _now: sensor.write_text(next(readings), encoding="ascii"))
            result = ThermalMonitor(sensor).check_sustained(
                over_limit_samples=3,
                sample_interval_seconds=1,
                sleep=clock.sleep,
            )
            self.assertEqual(result, 67.0)
            self.assertEqual(clock.now, 4.0)

    def test_check_sustained_stops_after_three_consecutive_hot_readings(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("80000\n", encoding="ascii")
            clock = FakeClock()
            with self.assertRaises(ThermalLimitReached):
                ThermalMonitor(sensor).check_sustained(
                    over_limit_samples=3,
                    sample_interval_seconds=1,
                    sleep=clock.sleep,
                )
            self.assertEqual(clock.now, 2.0)

    def test_check_sustained_fails_closed_on_sensor_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            monitor = ThermalMonitor(Path(directory) / "missing")
            with self.assertRaisesRegex(RuntimeError, "sensor is unavailable"):
                monitor.check_sustained(sleep=lambda _seconds: self.fail("must not retry sensor errors"))

    def test_check_remains_an_instantaneous_limit_check(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("80000\n", encoding="ascii")
            with self.assertRaises(ThermalLimitReached):
                ThermalMonitor(sensor).check()

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

    def test_single_hot_spike_does_not_reset_cool_dwell(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("62.0\n", encoding="ascii")

            def change_temperature(now: float) -> None:
                if now == 2.0:
                    sensor.write_text("70000\n", encoding="ascii")
                elif now == 3.0:
                    sensor.write_text("65000\n", encoding="ascii")

            clock = FakeClock(on_sleep=change_temperature)
            result = ThermalMonitor(sensor).wait_for_cool_dwell(
                dwell_seconds=3,
                sample_interval_seconds=1,
                monotonic=clock.monotonic,
                sleep=clock.sleep,
            )
            self.assertEqual(result, 65.0)
            self.assertEqual(clock.now, 3.0)

    def test_three_consecutive_hot_readings_reset_cool_dwell(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("62.0\n", encoding="ascii")

            def change_temperature(now: float) -> None:
                if now in (2.0, 3.0, 4.0):
                    sensor.write_text("70000\n", encoding="ascii")
                elif now == 5.0:
                    sensor.write_text("65000\n", encoding="ascii")

            clock = FakeClock(on_sleep=change_temperature)
            ThermalMonitor(sensor).wait_for_cool_dwell(
                dwell_seconds=3,
                sample_interval_seconds=1,
                monotonic=clock.monotonic,
                sleep=clock.sleep,
            )
            self.assertEqual(clock.now, 8.0)

    def test_continuous_overheating_never_completes_cool_dwell(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sensor = Path(directory) / "temp"
            sensor.write_text("70000\n", encoding="ascii")
            clock = FakeClock()

            class StopAfterSamples(Exception):
                pass

            def bounded_sleep(seconds: float) -> None:
                clock.sleep(seconds)
                if clock.now >= 5.0:
                    raise StopAfterSamples

            with self.assertRaises(StopAfterSamples):
                ThermalMonitor(sensor).wait_for_cool_dwell(
                    dwell_seconds=3,
                    sample_interval_seconds=1,
                    monotonic=clock.monotonic,
                    sleep=bounded_sleep,
                )
            self.assertEqual(clock.now, 5.0)

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
                ThermalMonitor(Path(directory) / "temp", stop_celsius=81)
            with self.assertRaises(ValueError):
                ThermalMonitor(Path(directory) / "temp", stop_celsius=64)
            self.assertEqual(ThermalMonitor(Path(directory) / "temp", stop_celsius=80).stop_celsius, 80)
            sensor = Path(directory) / "temp"
            sensor.write_text("not-a-temperature", encoding="ascii")
            with self.assertRaisesRegex(RuntimeError, "stopping embedding"):
                ThermalMonitor(sensor).check()

    def test_embed_unit_keeps_cpu_memory_and_temperature_limits(self) -> None:
        project_root = Path(__file__).resolve().parents[1]
        unit = (project_root / "deploy/graph-kb-assistant-kb-embed.service").read_text(encoding="utf-8")
        self.assertIn("CPUQuota=40%", unit)
        self.assertIn("MemoryMax=3584M", unit)
        self.assertIn("Restart=no", unit)
        for name in ("OMP_NUM_THREADS", "MKL_NUM_THREADS", "OPENBLAS_NUM_THREADS"):
            self.assertRegex(unit, rf"(?m)^Environment={name}=1$")
        start = next(line for line in unit.splitlines() if line.startswith("ExecStart="))
        self.assertRegex(start, r"(?:^| )embed(?: |$)")
        self.assertRegex(start, r"(?:^| )--batch-size 2(?: |$)")
        self.assertRegex(start, r"(?:^| )--threads 1(?: |$)")
        match = re.search(r"(?:^| )--stop-temperature-c ([0-9.]+)(?: |$)", start)
        self.assertIsNotNone(match)
        self.assertEqual(float(match.group(1)), 80.0)

    def test_embedding_worker_lock_prevents_a_duplicate_process(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with embedding_worker_lock(root):
                with self.assertRaisesRegex(RuntimeError, "already active"):
                    with embedding_worker_lock(root):
                        self.fail("a second worker acquired the lock")

    def test_metadata_only_exploitdb_chunks_are_searchable_for_self_hosted_kb(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            document = SourceDocument.create(
                source_name="exploitdb", source_id="metadata.csv:1", title="SQL injection",
                text="SQL injection payload metadata", metadata={"document_role": "content"},
            )
            index = KBIndex(Path(directory) / "index.sqlite3")
            index.replace_snapshot(
                tuple(chunk_document(document)), "e" * 64, sources=(document,),
            )
            hits = index.search("SQL injection")
            self.assertEqual([hit.source_name for hit in hits], ["exploitdb"])
            filtered_hits = index.search("SQL injection", source_names=("exploitdb",))
            self.assertEqual([hit.source_name for hit in filtered_hits], ["exploitdb"])


if __name__ == "__main__":
    unittest.main()
