#!/usr/bin/env python3
"""Full capture+replay parity run for CI — parallel.

Gates the Go port on byte-parity WITHOUT committing the (freely regenerable) golden corpus:

  1. Seed a pristine base fixture ONCE, then for every profile (``base`` + each entry in
     ``profiles.PROFILES``) CONCURRENTLY: copy the base snapshot to an isolated data dir, apply the
     profile's seed (if any) to that copy, and capture its golden bytes from the frozen Python
     oracle against that copy.
  2. Re-seed a clean base, then run ``parity_check.py`` — which boots a Go server per profile (each
     on its own port + fixture copy) CONCURRENTLY and byte-diffs the replay against the goldens.

Both phases are parallel: each profile is fully isolated (its own data-dir copy, its own oracle
process / Go server + port), so there is no shared state to serialize on. Worker count defaults to
min(8, cpu) and is overridable via SOBS_PARITY_WORKERS (chdb is memory-heavy and contends on
concurrent opens, so it is bounded rather than unleashed across every core). capture_routes writes
each route's golden under its own subdir (exist_ok), so concurrent profiles never collide.
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


def _run(*args: str) -> None:
    subprocess.run([PY, str(TOOLS / args[0]), *args[1:]], check=True, cwd=str(REPO))


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
    fresh copy so a transient chdb open failure never leaves a half-seeded fixture."""
    workdir = REPO / "migration" / "fixtures" / f"_cap_p{idx}"
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            if workdir.exists():
                shutil.rmtree(workdir)
            # symlinks=True: chdb's Atomic engine maps `default` to its store via a relative symlink.
            shutil.copytree(FIXTURE_SRC, workdir, symlinks=True)
            if profile in P.SEEDED_PROFILES:
                _run("seed_fixtures.py", "--only-profile", profile, "--data-dir", str(workdir))
                time.sleep(0.5)  # let the seed's chdb release the dir before capture opens it
            _run("capture_routes.py", "--profile", profile, "--data-dir", str(workdir))
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


def main() -> int:
    profiles = ["base"] + sorted(P.PROFILES)
    workers = int(os.environ.get("SOBS_PARITY_WORKERS", "0") or 0) or min(8, (os.cpu_count() or 4))
    print(f"Capturing goldens for {len(profiles)} profile(s) — {workers} parallel workers", flush=True)
    _seed_base()
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as ex:
        futs = {ex.submit(_capture_profile, i, p): p for i, p in enumerate(profiles)}
        for f in concurrent.futures.as_completed(futs):
            f.result()  # re-raise the first capture failure

    print("\n=== Replaying Go server against captured goldens (parallel) ===", flush=True)
    _replay()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
