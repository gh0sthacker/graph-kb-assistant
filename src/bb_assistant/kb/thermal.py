"""Fail-closed thermal stop check for long, resumable CPU embedding runs."""
from __future__ import annotations

import time
from pathlib import Path
from typing import Callable


class ThermalLimitReached(RuntimeError):
    """Raised before a batch when the monitored sensor is at its stop limit."""


class ThermalMonitor:
    def __init__(self, sensor_path: Path, *, stop_celsius: float = 70.0) -> None:
        if not 65.0 <= stop_celsius <= 70.0:
            raise ValueError("stop_celsius must be between 65 and 70")
        self.sensor_path = sensor_path
        self.stop_celsius = stop_celsius

    def check(self) -> float:
        try:
            sensor = self._resolve_sensor() if self.sensor_path.is_dir() else self.sensor_path
            raw = sensor.read_text(encoding="ascii").strip()
            value = float(raw)
        except (OSError, UnicodeError, ValueError) as exc:
            raise RuntimeError("thermal sensor is unavailable; stopping embedding") from exc
        if not 0.0 < value < 200_000.0:
            raise RuntimeError("thermal sensor returned an invalid value; stopping embedding")
        celsius = value / 1000.0 if value > 200.0 else value
        if celsius >= self.stop_celsius:
            raise ThermalLimitReached(
                f"thermal stop reached ({celsius:.1f} C >= {self.stop_celsius:.1f} C)"
            )
        return celsius

    def wait_for_cool_dwell(
        self,
        *,
        dwell_seconds: float = 600.0,
        sample_interval_seconds: float = 1.0,
        monotonic: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
    ) -> float:
        """Wait until every sampled package temperature stays below the limit.

        Only a thermal-limit reading resets the dwell. Sensor/configuration
        errors propagate so callers fail closed instead of waiting forever.
        """
        if dwell_seconds < 0:
            raise ValueError("dwell_seconds must not be negative")
        if sample_interval_seconds <= 0:
            raise ValueError("sample_interval_seconds must be positive")

        cool_since: float | None = None
        while True:
            try:
                temperature = self.check()
            except ThermalLimitReached:
                cool_since = None
            else:
                now = monotonic()
                if cool_since is None:
                    cool_since = now
                if now - cool_since >= dwell_seconds:
                    return temperature
            sleep(sample_interval_seconds)

    def _resolve_sensor(self) -> Path:
        matches: list[Path] = []
        try:
            for name_path in self.sensor_path.glob("hwmon*/name"):
                if name_path.read_text(encoding="ascii").strip() != "coretemp":
                    continue
                for label_path in name_path.parent.glob("temp*_label"):
                    if label_path.read_text(encoding="ascii").strip() == "Package id 0":
                        matches.append(label_path.with_name(label_path.name.removesuffix("_label") + "_input"))
        except (OSError, UnicodeError) as exc:
            raise RuntimeError("thermal sensor labels are unavailable; stopping embedding") from exc
        if len(matches) != 1 or not matches[0].is_file():
            raise RuntimeError("unique coretemp Package id 0 sensor was not found; stopping embedding")
        return matches[0]
