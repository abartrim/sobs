#!/usr/bin/env python3
"""Fail if any Dockerfile's ARG GO_VERSION/CHDB_VERSION default has drifted from the
canonical pin in versions.env (repo root).

CI itself always builds Dockerfile.ci/.go/.e2e with the canonical values passed explicitly
as --build-arg (see .github/workflows/ci.yml), so this doesn't catch a CI build using the
wrong version there — it catches a Dockerfile's *default* (what a plain `docker build .`
outside CI would use) silently going stale, which is exactly how these pins drifted out of
sync before. For the local-only dev Dockerfiles (Dockerfile.capture, Dockerfile.go.dev),
which nothing ever passes --build-arg to, the default IS the value actually used, so this
check is the only thing keeping them pinned correctly at all.

Every repo-root `Dockerfile*` that declares `ARG GO_VERSION=` or `ARG CHDB_VERSION=` is
checked automatically — nothing to update here when a new one is added.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
VERSIONS_FILE = REPO_ROOT / "versions.env"


def load_versions() -> dict[str, str]:
    versions: dict[str, str] = {}
    for line in VERSIONS_FILE.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, _, value = line.partition("=")
        versions[key] = value
    return versions


def find_arg_default(dockerfile_text: str, name: str) -> str | None:
    match = re.search(rf"^ARG {re.escape(name)}=(\S+)", dockerfile_text, re.MULTILINE)
    return match.group(1) if match else None


def find_pinned_dockerfiles(versions: dict[str, str]) -> list[Path]:
    candidates = sorted(p for p in REPO_ROOT.glob("Dockerfile*") if p.is_file())
    return [path for path in candidates if any(find_arg_default(path.read_text(), name) for name in versions)]


def main() -> int:
    versions = load_versions()
    dockerfiles = find_pinned_dockerfiles(versions)
    errors = []
    for path in dockerfiles:
        text = path.read_text()
        for name, expected in versions.items():
            actual = find_arg_default(text, name)
            if actual is None:
                errors.append(f"{path.name}: declares another pinned ARG but no 'ARG {name}=' default found")
            elif actual != expected:
                errors.append(f"{path.name}: ARG {name} default is {actual!r}, versions.env pins {expected!r}")

    if errors:
        print("Version pin drift detected (versions.env is the source of truth):", file=sys.stderr)
        for error in errors:
            print(f"  - {error}", file=sys.stderr)
        return 1

    names = ", ".join(path.name for path in dockerfiles)
    print(f"OK: {names} all match versions.env ({versions})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
