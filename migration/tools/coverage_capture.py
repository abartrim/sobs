#!/usr/bin/env python3
"""Oracle coverage gate — measure which app.py lines the differential corpus actually exercises.

The byte-parity suite only verifies Go == Python on the inputs the golden corpus contains. This
tool runs the SAME capture phase as run_parity_ci.py (every profile: clean base seed -> optional
per-profile seed -> capture via the Quart test client) but under coverage.py, so the union of
app.py lines hit across all profiles == exactly the behavior parity can verify. The *uncovered*
lines are the remaining risk surface: each is either dead/unreachable, needs a new fixture/profile,
or is intentionally deferred. This turns "are we done?" into a finite, classifiable list instead of
another human audit.

Run inside the parity Docker image (which has app.py's deps + chdb), e.g.:
    pip install coverage && python migration/tools/coverage_capture.py

Outputs:
    migration/coverage_app.json  — machine-readable per-line coverage of app.py
    stdout                       — coverage.py report (Stmts/Miss/Cover + missing line ranges)
"""

from __future__ import annotations

import subprocess
import sys
import time
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]
PY = sys.executable

sys.path.insert(0, str(TOOLS))
import profiles as P  # noqa: E402

_MAX_ATTEMPTS = 3


def _run(args: list[str]) -> None:
    subprocess.run(args, check=True, cwd=str(REPO))


def _capture_under_coverage(profile: str) -> None:
    """Re-seed a clean base, apply the profile's isolated seed, then capture under coverage -p.

    Retried as a unit (chdb's embedded server contends across the many-profile run), mirroring
    run_parity_ci._capture_profile.
    """
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            _run([PY, str(TOOLS / "seed_fixtures.py")])
            if profile in P.SEEDED_PROFILES:
                _run([PY, str(TOOLS / "seed_fixtures.py"), "--only-profile", profile])
            # -p: parallel mode (unique data file per run, combined at the end).
            # --source=app: only instrument app.py (the oracle), not the tooling/libs.
            _run(
                [
                    PY,
                    "-m",
                    "coverage",
                    "run",
                    "-p",
                    "--source=app",
                    str(TOOLS / "capture_routes.py"),
                    "--profile",
                    profile,
                ]
            )
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                raise
            print(f"  [retry {attempt}/{_MAX_ATTEMPTS - 1}] profile {profile} — chdb contention; resetting", flush=True)
            time.sleep(2)


def main() -> int:
    # Erase any leftover .coverage / .coverage.* from a prior run FIRST. `coverage run -p` only
    # appends parallel data and `coverage combine` folds in a stale singular .coverage, so without
    # this a repeated local run (bind-mounted worktree) silently re-reports the PREVIOUS run's
    # numbers. CI is unaffected (fresh container), but locally this masked real coverage gains.
    _run([PY, "-m", "coverage", "erase"])
    profiles = ["base"] + sorted(P.PROFILES)
    print(f"Coverage capture across {len(profiles)} profile(s): {', '.join(profiles)}", flush=True)
    for profile in profiles:
        _capture_under_coverage(profile)
        print(f"  covered profile {profile}", flush=True)

    _run([PY, "-m", "coverage", "combine"])
    print("\n==================== app.py ORACLE COVERAGE ====================", flush=True)
    _run([PY, "-m", "coverage", "report", "--include=*app.py", "-m"])
    _run([PY, "-m", "coverage", "json", "-o", "migration/coverage_app.json", "--include=*app.py"])
    print("\nWrote migration/coverage_app.json", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
