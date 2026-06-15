#!/usr/bin/env python3
"""Full capture+replay parity run for CI.

Scripts the manual loop documented in the migration recipe so CI can gate the Go port on
byte-parity WITHOUT committing the (freely regenerable) golden corpus:

  1. for every profile (``base`` + each entry in ``profiles.PROFILES``):
       re-seed a clean base fixture, apply the profile's isolated seed (if any), then
       capture that profile's golden bytes from the frozen Python oracle.
  2. re-seed a clean base once more, then run ``parity_check.py`` — it boots the Go server
     per profile against a fresh fixture copy and byte-diffs the replay against the goldens.

``parity_check.py`` already exits non-zero on any RED / MISSING_GOLDEN / UNCOVERED, so this
wrapper just sequences the capture side (which has no single entrypoint — capture is one
process per profile so each gets an isolated gate env + uuid counter) and propagates failure.

A clean base is re-seeded before EACH capture because capturing a profile replays its
mutating routes, which dirty the shared source fixture; the isolated per-profile seeds must
never ripple into another profile's capture.
"""

from __future__ import annotations

import subprocess
import sys
import time
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parent.parent
sys.path.insert(0, str(TOOLS))

import profiles as P  # noqa: E402

PY = sys.executable

# chDB's embedded server intermittently fails to open with "recursive_mutex lock failed" /
# ASYNC_LOAD_WAIT_FAILED when many short-lived chdb processes boot in quick succession (the same
# contention server.go retries on the Go side). A fresh process almost always succeeds, so retry.
_MAX_ATTEMPTS = 5


def _run(*args: str) -> None:
    print("+", " ".join(args), flush=True)
    subprocess.run([PY, str(TOOLS / args[0]), *args[1:]], check=True, cwd=str(REPO))


def _capture_profile(profile: str) -> None:
    """Re-seed a clean base, apply the profile's isolated seed, and capture — retried as a UNIT
    from a clean base so a transient chdb open failure never leaves a half-seeded fixture."""
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            _run("seed_fixtures.py")  # clean base (a prior capture may have mutated the fixture)
            if profile in P.SEEDED_PROFILES:
                _run("seed_fixtures.py", "--only-profile", profile)
            _run("capture_routes.py", "--profile", profile)
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                raise
            print(f"  [retry {attempt}/{_MAX_ATTEMPTS - 1}] profile {profile} — chdb contention; resetting", flush=True)
            time.sleep(3)


def _replay() -> None:
    for attempt in range(1, _MAX_ATTEMPTS + 1):
        try:
            _run("seed_fixtures.py")  # clean base for the replay's fixture copies
            _run("parity_check.py")
            return
        except subprocess.CalledProcessError:
            if attempt == _MAX_ATTEMPTS:
                raise
            print(f"  [retry {attempt}/{_MAX_ATTEMPTS - 1}] replay — chdb contention; retrying", flush=True)
            time.sleep(3)


def main() -> int:
    profiles = ["base"] + sorted(P.PROFILES)
    print(f"Capturing goldens for {len(profiles)} profile(s): {', '.join(profiles)}", flush=True)
    for profile in profiles:
        _capture_profile(profile)

    print("\n=== Replaying Go server against captured goldens ===", flush=True)
    _replay()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
