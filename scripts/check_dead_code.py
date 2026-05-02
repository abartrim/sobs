#!/usr/bin/env python3
"""
check_dead_code.py – Conservative Vulture dead-code audit.

Runs Vulture at 100% confidence only.  Low-confidence findings are omitted
intentionally; they produce too many false positives for dynamic wrappers,
Jinja2 globals, and dependency-injection helpers.

Usage:
    python3 scripts/check_dead_code.py          # exits 0 when clean
    python3 scripts/check_dead_code.py --report  # print all findings, exit 0

Minimum confidence: 100 (only findings Vulture is certain about).

Exit codes:
    0 – no high-confidence dead code found
    1 – at least one high-confidence unused symbol found
"""

from __future__ import annotations

import argparse
import subprocess
import sys

TARGETS = [
    "app.py",
    "masking.py",
    "mcp.py",
    "routes/",
]

# Symbols that appear dead to static analysis but are used dynamically
# (Jinja2 globals, Flask/Quart context vars, pytest fixtures, etc.).
# Add the bare symbol name (without module prefix) to suppress false positives.
WHITELIST: set[str] = set()


def main() -> int:
    parser = argparse.ArgumentParser(description="Dead-code audit via Vulture (100% confidence).")
    parser.add_argument("--report", action="store_true", help="Print findings and exit 0 (informational mode).")
    args = parser.parse_args()

    cmd = [
        sys.executable,
        "-m",
        "vulture",
        "--min-confidence",
        "100",
        *TARGETS,
    ]

    result = subprocess.run(cmd, capture_output=True, text=True)
    output = result.stdout.strip()

    if not output:
        print("check_dead_code: no high-confidence dead code found.")
        return 0

    # Filter whitelist entries
    lines = [line for line in output.splitlines() if not any(w in line for w in WHITELIST)]
    if not lines:
        print("check_dead_code: no high-confidence dead code found (all findings whitelisted).")
        return 0

    for line in lines:
        print(line)

    if args.report:
        print(f"\ncheck_dead_code: {len(lines)} finding(s) reported (--report mode, not failing).")
        return 0

    print(f"\ncheck_dead_code: {len(lines)} high-confidence unused symbol(s) found. Review before deleting.")
    print("Use --report to print without failing.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
