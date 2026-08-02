#!/usr/bin/env python3
"""Boot a real sobs server against the frozen "base" fixture corpus, for the Playwright
E2E suite (e2e/) to point at. Mirrors go/goldenreplay/replay_test.go's env pin, fixture
extraction, and readiness-poll logic exactly, so the server this starts behaves like the
one the golden-corpus harness already validates against — see that file (and
go/testdata/fixtures/profile_env.json's "base" entry, which is empty) for the source of
truth this replicates.

Intended to be invoked as Playwright's `webServer.command` (see playwright.config.ts):
this process execs the sobs binary in place, so Playwright's process management (which
sends the command process a termination signal on teardown) reaches the server directly.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
GO_DIR = REPO_ROOT / "go"
FIXTURES_DIR = GO_DIR / "testdata" / "fixtures"

# Mirrors go/goldenreplay/replay_test.go's pinnedEnv verbatim — the frozen environment the
# golden corpus was captured under. Kept in sync by hand; if that map changes, update this too.
PINNED_ENV = {
    "SOBS_PARITY": "1",
    "SOBS_SECRET_KEY": "parity-fixed-secret-key",
    "SOBS_SESSION_COOKIE_NAME": "sobs_session",
    "SOBS_SESSION_COOKIE_SAMESITE": "Lax",
    "SOBS_BASE_PATH": "",
    "SOBS_ENABLE_FIRST_RUN_TOUR": "0",
    "SOBS_SUMMARY_STATS_CACHE_TTL_SEC": "0",
    "SOBS_FAKE_EPOCH": "1704164645.0",
    "SOURCE_MAP_ENABLE": "0",
    "TZ": "America/Phoenix",
}

DEFAULT_PORT = 48173


def _build_binary(workdir: Path) -> Path:
    binary = workdir / "sobs"
    result = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/sobs"],
        cwd=GO_DIR,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        sys.exit(f"go build ./cmd/sobs failed:\n{result.stdout}\n{result.stderr}")
    return binary


def _extract_fixture(archive: Path, dest: Path) -> None:
    dest.mkdir(parents=True, exist_ok=True)
    result = subprocess.run(
        ["tar", "-xzf", str(archive), "-C", str(dest)],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        sys.exit(f"extracting {archive} failed:\n{result.stderr}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=int(os.environ.get("SOBS_E2E_PORT", DEFAULT_PORT)))
    parser.add_argument(
        "--workdir",
        type=Path,
        default=None,
        help="Reuse this directory instead of a fresh temp dir (useful for manual debugging).",
    )
    args = parser.parse_args()

    lib_path = os.environ.get("CHDB_LIB_PATH")
    if not lib_path:
        sys.exit(
            "CHDB_LIB_PATH is not set — see go/CHDB_PIN.md for the pinned native chdb lib to "
            "download, then export CHDB_LIB_PATH=/path/to/libchdb.so"
        )

    workdir = args.workdir or Path(tempfile.mkdtemp(prefix="sobs-e2e-"))
    workdir.mkdir(parents=True, exist_ok=True)

    binary = _build_binary(workdir)

    data_dir = workdir / "data"
    _extract_fixture(FIXTURES_DIR / "base.tar.gz", data_dir)

    upstream_dir = workdir / "upstream"
    _extract_fixture(FIXTURES_DIR / "upstream.tar.gz", upstream_dir)

    env = os.environ.copy()
    env.update(PINNED_ENV)
    env["CHDB_LIB_PATH"] = lib_path
    env["SOBS_DATA_DIR"] = str(data_dir)
    env["SOBS_PORT"] = str(args.port)
    # Unused by the "base" profile itself (its env overlay is empty — see profile_env.json),
    # but set for any future spec that exercises an upstream-dependent route.
    env["SOBS_UPSTREAM_FIXTURES"] = str(upstream_dir)

    print(f"[e2e_server] booting sobs on port {args.port}, data dir {data_dir}", file=sys.stderr)
    os.chdir(REPO_ROOT)  # templates/ and static/ resolve relative to cwd
    os.execve(str(binary), [str(binary)], env)


if __name__ == "__main__":
    main()
