#!/usr/bin/env python3
"""Fail if any Dockerfile's ARG GO_VERSION/CHDB_VERSION default has drifted from the
canonical pin in versions.env (repo root).

CI itself always builds with the canonical values passed explicitly as --build-arg (see
.github/workflows/ci.yml), so this doesn't catch a CI build using the wrong version — it
catches a Dockerfile's *default* (what a plain `docker build .` outside CI would use)
silently going stale, which is exactly how these three pins drifted out of sync before.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
VERSIONS_FILE = REPO_ROOT / "versions.env"
DOCKERFILES = ["Dockerfile.ci", "Dockerfile.go", "Dockerfile.e2e"]


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


def main() -> int:
    versions = load_versions()
    errors = []
    for filename in DOCKERFILES:
        path = REPO_ROOT / filename
        text = path.read_text()
        for name, expected in versions.items():
            actual = find_arg_default(text, name)
            if actual is None:
                errors.append(f"{filename}: no 'ARG {name}=' default found")
            elif actual != expected:
                errors.append(f"{filename}: ARG {name} default is {actual!r}, versions.env pins {expected!r}")

    if errors:
        print("Version pin drift detected (versions.env is the source of truth):", file=sys.stderr)
        for error in errors:
            print(f"  - {error}", file=sys.stderr)
        return 1

    print(f"OK: {', '.join(DOCKERFILES)} all match versions.env ({versions})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
