"""Fail-closed thermal stop check for long, resumable CPU embedding runs."""
from __future__ import annotations

from pathlib import Path


class ThermalLimitReached(RuntimeError):
    """Raised before a batch when the monitored sensor is at its stop limit."""


class ThermalMonitor:
    def __init__(self, sensor_path: Path, *, stop_celsius: float = 80.0) -> None:
        if not 65.0 <= stop_celsius <= 85.0:
            raise ValueError("stop_celsius must be between 65 and 85")
        self.sensor_path = sensor_path
        self.stop_celsius = stop_celsius

    def check(self) -> float:
        try:
            raw = self.sensor_path.read_text(encoding="ascii").strip()
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
