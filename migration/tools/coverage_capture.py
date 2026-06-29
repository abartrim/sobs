#!/usr/bin/env python3
"""Oracle coverage gate — measure which app.py lines the differential corpus actually exercises.

The byte-parity suite only verifies Go == Python on the inputs the golden corpus contains. This
tool runs the SAME capture phase as run_parity_ci.py (every profile: seed -> optional per-profile
seed -> capture via the Quart test client) but under coverage.py, so the union of app.py lines hit
across all profiles == exactly the behavior parity can verify. The *uncovered* lines are the
remaining risk surface: each is either dead/unreachable, needs a new fixture/profile, or is
intentionally deferred. This turns "are we done?" into a finite, classifiable list.

Parallel, mirroring run_parity_ci.py: seed a pristine base ONCE, then capture every profile
CONCURRENTLY against its own isolated data-dir copy under `coverage run -p` (which already writes a
unique data file per process), then `coverage combine`. Worker count = SOBS_PARITY_WORKERS or
min(8, cpu); pair with SOBS_CHDB_MAX_SERVER_MB=0 (chdb's per-instance cap is cgroup-aware and trips
under parallelism). Run in Docker (big memory) — coverage instrumentation makes each chdb heavier.

Outputs:
    migration/coverage_app.json  — machine-readable per-line coverage of app.py
    stdout                       — coverage.py report (Stmts/Miss/Cover + missing line ranges)
"""

from __future__ import annotations

import concurrent.futures
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]
FIXTURE_SRC = REPO / "migration" / "fixtures" / "data"
PY = sys.executable

sys.path.insert(0, str(TOOLS))
import profiles as P  # noqa: E402

_MAX_ATTEMPTS = 4


def _run(args: list[str]) -> None:
    subprocess.run(args, check=True, cwd=str(REPO))


def _seed_base() -> None:
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            _run([PY, str(TOOLS / "seed_fixtures.py")])
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                raise
            time.sleep(3)


def _capture_under_coverage(idx: int, profile: str) -> None:
    """Capture one profile under coverage against its OWN copy of the base snapshot — retried as a
    unit from a fresh copy (chdb's embedded server contends across the many-profile run)."""
    workdir = REPO / "migration" / "fixtures" / f"_cov_p{idx}"
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            if workdir.exists():
                shutil.rmtree(workdir)
            shutil.copytree(FIXTURE_SRC, workdir, symlinks=True)
            if profile in P.SEEDED_PROFILES:
                _run([PY, str(TOOLS / "seed_fixtures.py"), "--only-profile", profile, "--data-dir", str(workdir)])
                time.sleep(0.5)
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
                    "--data-dir",
                    str(workdir),
                ]
            )
            shutil.rmtree(workdir, ignore_errors=True)
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                shutil.rmtree(workdir, ignore_errors=True)
                raise
            print(f"  [retry {attempt}/{_MAX_ATTEMPTS - 1}] coverage {profile} — chdb contention", flush=True)
            time.sleep(2.0 * attempt)


def main() -> int:
    # Erase any leftover .coverage / .coverage.* from a prior run FIRST (CI is a fresh container, but
    # locally a bind-mounted worktree would otherwise re-report the previous run's numbers).
    _run([PY, "-m", "coverage", "erase"])
    profiles = ["base"] + sorted(P.PROFILES)
    workers = int(os.environ.get("SOBS_PARITY_WORKERS", "0") or 0) or min(8, (os.cpu_count() or 4))
    print(f"Coverage capture across {len(profiles)} profile(s) — {workers} parallel workers", flush=True)
    _seed_base()
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as ex:
        futs = {ex.submit(_capture_under_coverage, i, p): p for i, p in enumerate(profiles)}
        for f in concurrent.futures.as_completed(futs):
            f.result()  # re-raise the first capture failure

    _run([PY, "-m", "coverage", "combine"])
    print("\n==================== app.py ORACLE COVERAGE ====================", flush=True)
    _run([PY, "-m", "coverage", "report", "--include=*app.py", "-m"])
    _run([PY, "-m", "coverage", "json", "-o", "migration/coverage_app.json", "--include=*app.py"])
    print("\nWrote migration/coverage_app.json", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
