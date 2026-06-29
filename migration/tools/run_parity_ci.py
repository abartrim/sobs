#!/usr/bin/env python3
"""Full capture+replay parity run for CI — parallel, with optional folded-in coverage gate.

Gates the Go port on byte-parity WITHOUT committing the (freely regenerable) golden corpus:

  1. Seed a pristine base fixture ONCE, then for every profile (``base`` + each entry in
     ``profiles.PROFILES``) CONCURRENTLY: copy the base snapshot to an isolated data dir, apply the
     profile's seed (if any) to that copy, and capture its golden bytes from the frozen Python
     oracle against that copy.
  2. Re-seed a clean base, then run ``parity_check.py`` — which boots a Go server per profile (each
     on its own port + fixture copy) CONCURRENTLY and byte-diffs the replay against the goldens.

When SOBS_PARITY_WITH_COVERAGE=1 (set in CI), step 1's capture runs UNDER coverage.py, so the SAME
single capture produces both the goldens (for the byte-diff) AND app.py line coverage. After the
replay, the per-profile coverage data is combined and the floor gate enforced. coverage.py is a line
tracer and never changes app.py's output, so the goldens are byte-identical with or without it —
which is why the formerly-separate oracle-coverage job (a second, redundant full capture) is folded
in here instead. Locally, run without the flag for a pure parity check.

Both phases are parallel: each profile is fully isolated (its own data-dir copy, its own oracle
process / Go server + port). Worker count = SOBS_PARITY_WORKERS or min(8, cpu); pair with
SOBS_CHDB_MAX_SERVER_MB=0 (chdb's per-instance cap is cgroup-aware and trips under parallelism).
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
REPO = TOOLS.parent.parent
FIXTURE_SRC = REPO / "migration" / "fixtures" / "data"
sys.path.insert(0, str(TOOLS))

import profiles as P  # noqa: E402

PY = sys.executable

# chDB's embedded server intermittently fails to open ("recursive_mutex lock failed" /
# ASYNC_LOAD_WAIT_FAILED) when many short-lived chdb processes boot at once. A fresh process almost
# always succeeds, so retry the affected unit.
_MAX_ATTEMPTS = 5

# When set (CI), the capture runs under coverage.py so ONE capture produces the goldens (for the
# byte-diff) AND app.py line coverage (for the floor gate) — no separate, redundant coverage job.
WITH_COVERAGE = os.environ.get("SOBS_PARITY_WITH_COVERAGE") == "1"


def _run(*args: str) -> None:
    subprocess.run([PY, str(TOOLS / args[0]), *args[1:]], check=True, cwd=str(REPO))


def _run_raw(cmd: list[str]) -> None:
    subprocess.run(cmd, check=True, cwd=str(REPO))


def _seed_base() -> None:
    """Seed the pristine base fixture ONCE (not per profile). Every profile capture then works
    against an isolated COPY of this snapshot, so the slow full re-seed runs a single time."""
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            _run("seed_fixtures.py")
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                raise
            time.sleep(3)


def _capture_profile(idx: int, profile: str) -> None:
    """Capture one profile against its OWN copy of the base snapshot — retried as a unit from a
    fresh copy so a transient chdb open failure never leaves a half-seeded fixture. Under coverage
    when WITH_COVERAGE (coverage -p writes a unique data file per process, combined later)."""
    workdir = REPO / "migration" / "fixtures" / f"_cap_p{idx}"
    capture = [str(TOOLS / "capture_routes.py"), "--profile", profile, "--data-dir", str(workdir)]
    if WITH_COVERAGE:
        cmd = [PY, "-m", "coverage", "run", "-p", "--source=app"] + capture
    else:
        cmd = [PY] + capture
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            if workdir.exists():
                shutil.rmtree(workdir)
            # symlinks=True: chdb's Atomic engine maps `default` to its store via a relative symlink.
            shutil.copytree(FIXTURE_SRC, workdir, symlinks=True)
            if profile in P.SEEDED_PROFILES:
                _run("seed_fixtures.py", "--only-profile", profile, "--data-dir", str(workdir))
                time.sleep(0.5)  # let the seed's chdb release the dir before capture opens it
            _run_raw(cmd)
            shutil.rmtree(workdir, ignore_errors=True)
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                shutil.rmtree(workdir, ignore_errors=True)
                raise
            print(f"  [retry {attempt}/{_MAX_ATTEMPTS - 1}] capture {profile} — chdb contention", flush=True)
            time.sleep(2.0 * attempt)


def _replay() -> None:
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            _run("seed_fixtures.py")  # pristine base for parity_check's per-profile copies
            _run("parity_check.py")
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                raise
            print(f"  [retry {attempt}/{_MAX_ATTEMPTS - 1}] replay — chdb contention; retrying", flush=True)
            time.sleep(3)


def _coverage_gate() -> None:
    """Combine the per-profile coverage data the capture already produced, write the report + JSON,
    and enforce the floor — folded in here so the single capture serves both parity and coverage."""
    _run_raw([PY, "-m", "coverage", "combine"])
    print("\n==================== app.py ORACLE COVERAGE ====================", flush=True)
    _run_raw([PY, "-m", "coverage", "report", "--include=*app.py", "-m"])
    _run_raw([PY, "-m", "coverage", "json", "-o", "migration/coverage_app.json", "--include=*app.py"])
    _run("coverage_gate.py")


def main() -> int:
    profiles = ["base"] + sorted(P.PROFILES)
    workers = int(os.environ.get("SOBS_PARITY_WORKERS", "0") or 0) or min(8, (os.cpu_count() or 4))
    mode = "parity+coverage" if WITH_COVERAGE else "parity"
    print(f"Capturing {len(profiles)} profile(s) [{mode}] — {workers} parallel workers", flush=True)
    if WITH_COVERAGE:
        _run_raw([PY, "-m", "coverage", "erase"])
    _seed_base()
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as ex:
        futs = {ex.submit(_capture_profile, i, p): p for i, p in enumerate(profiles)}
        for f in concurrent.futures.as_completed(futs):
            f.result()  # re-raise the first capture failure

    print("\n=== Replaying Go server against captured goldens (parallel) ===", flush=True)
    _replay()

    if WITH_COVERAGE:
        _coverage_gate()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
