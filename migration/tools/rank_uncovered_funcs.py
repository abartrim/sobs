#!/usr/bin/env python3
"""Rank app.py functions by how many uncovered (missing) statement lines they contain, so corpus
expansion can target the highest-yield handlers first. Reads migration/coverage_app.json (produced
by coverage_capture.py) and walks app.py's AST. Output: a descending table of
function · uncovered-lines · span. Usage: python migration/tools/rank_uncovered_funcs.py [N]
"""

from __future__ import annotations

import ast
import json
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
APP = REPO / "app.py"
COV = REPO / "migration" / "coverage_app.json"


def main() -> int:
    top = int(sys.argv[1]) if len(sys.argv) > 1 else 40
    cov = json.loads(COV.read_text())
    fkey = next(k for k in cov["files"] if k.endswith("app.py"))
    missing = set(cov["files"][fkey]["missing_lines"])

    tree = ast.parse(APP.read_text())
    rows = []
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        start = node.lineno
        end = getattr(node, "end_lineno", start)
        miss = sum(1 for ln in range(start, end + 1) if ln in missing)
        if miss:
            rows.append((miss, node.name, start, end))
    rows.sort(reverse=True)
    print(f"{'uncovered':>9}  {'span':>13}  function")
    for miss, name, start, end in rows[:top]:
        print(f"{miss:>9}  {start:>5}-{end:<7}  {name}")
    print(f"\ntotal functions with uncovered lines: {len(rows)}")
    print(f"sum of uncovered-in-functions: {sum(r[0] for r in rows)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
