#!/usr/bin/env python3
"""Ratchet gate: fail if app.py oracle coverage regressed below the committed floor.

Reads migration/coverage_app.json (written by coverage_capture.py) and migration/COVERAGE_FLOOR.
As corpus expansion raises coverage, bump COVERAGE_FLOOR so it can only go up.
"""

from __future__ import annotations

import json
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]


def main() -> int:
    cov = json.loads((REPO / "migration" / "coverage_app.json").read_text())
    key = next(k for k in cov["files"] if k.endswith("app.py"))
    pct = float(cov["files"][key]["summary"]["percent_covered"])
    floor = float((REPO / "migration" / "COVERAGE_FLOOR").read_text().strip())
    print(f"app.py oracle coverage: {pct:.2f}%  (floor {floor:.2f}%)")
    if pct + 1e-6 < floor:
        print(
            f"::error::oracle coverage regressed: {pct:.2f}% < floor {floor:.2f}% — "
            "a fixture/profile likely broke, or covered behavior was removed."
        )
        return 1
    if pct - floor >= 1.0:
        print(f"::notice::coverage is {pct - floor:.1f} pts above the floor — bump migration/COVERAGE_FLOOR.")
    print("OK — coverage did not regress.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
