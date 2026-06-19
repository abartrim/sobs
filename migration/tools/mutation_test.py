#!/usr/bin/env python3
"""Mutation testing against the differential corpus — verify the verifier.

The corpus's job is to detect when app.py's behavior changes. A *mutant* is a small perturbation of
app.py source on a COVERED line. We re-capture the relevant goldens from the mutated oracle and
compare to the unmutated baseline:
  - goldens CHANGED  -> mutant KILLED   (the corpus observes this line — good)
  - goldens IDENTICAL -> mutant SURVIVED (a covered-but-UNASSERTED line — a corpus hole)

Surviving mutants are exactly the holes the coverage gate can't see (the line runs but its effect
never reaches any captured response). Only COVERED lines are mutated — uncovered lines survive
trivially and are already tracked by the coverage backlog.

Safety: the pristine app.py is read into memory once at startup; each mutant is applied IN PLACE
then restored by rewriting that in-memory copy (in a finally). This works inside the parity Docker
container, where the worktree's git gitdir link is unresolvable so `git checkout` cannot. The golden
corpus is regenerated scratch, so it needs no restore. If interrupted hard, restore with
`git checkout -- app.py` on the host.

  python migration/tools/mutation_test.py --function summary --only get__root --sample 6
  python migration/tools/mutation_test.py --lines 10841-10971 --only get__root --profile base

Run inside the parity Docker image. Exit 0 always (it's a report); non-zero only on harness error.
"""
from __future__ import annotations

import argparse
import ast
import filecmp
import random
import shutil
import subprocess
import sys
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]
APP = REPO / "app.py"
GOLDEN = REPO / "migration" / "golden"
PY = sys.executable


def _covered_lines() -> set[int]:
    import json
    c = json.loads((REPO / "migration" / "coverage_app.json").read_text())
    k = next(x for x in c["files"] if x.endswith("app.py"))
    return set(c["files"][k]["executed_lines"])


def _target_span(tree, function: str | None, lines: str | None) -> tuple[int, int]:
    if lines:
        a, b = lines.split("-")
        return int(a), int(b)
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == function:
            return node.lineno, (node.end_lineno or node.lineno)
    raise SystemExit(f"function {function!r} not found")


def _docstring_nodes(tree) -> set[int]:
    """line numbers of bare-string Expr statements (docstrings) — never mutate these."""
    out = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant) and isinstance(node.value.value, str):
            for ln in range(node.value.lineno, (node.value.end_lineno or node.value.lineno) + 1):
                out.add(ln)
    return out


def _mutants(tree, lo: int, hi: int, covered: set[int], docs: set[int]):
    """Yield (lineno, col, end_col, original_text, mutated_text) for Constant nodes on covered lines."""
    for node in ast.walk(tree):
        if not isinstance(node, ast.Constant):
            continue
        if node.lineno != node.end_lineno or node.lineno in docs:
            continue
        if not (lo <= node.lineno <= hi and node.lineno in covered):
            continue
        v = node.value
        if isinstance(v, bool):
            new = "False" if v else "True"
        elif isinstance(v, int):
            new = str(v + 1)
        elif isinstance(v, str) and 0 < len(v) < 60:
            new = repr(v + "X")
        else:
            continue
        yield (node.lineno, node.col_offset, node.end_col_offset, repr(v), new)


def _apply(src_lines: list[str], m) -> str:
    ln, c0, c1, _orig, new = m
    line = src_lines[ln - 1]
    return "".join(src_lines[: ln - 1]) + line[:c0] + new + line[c1:] + "".join(src_lines[ln:])


def _seed_and_capture(profile: str, only: str) -> None:
    subprocess.run([PY, str(TOOLS / "seed_fixtures.py")], cwd=str(REPO), check=True,
                   stdout=subprocess.DEVNULL)
    subprocess.run([PY, str(TOOLS / "capture_routes.py"), "--profile", profile, "--only", only],
                   cwd=str(REPO), check=True, stdout=subprocess.DEVNULL)


def _snapshot(route_ids: list[str], dst: Path) -> None:
    if dst.exists():
        shutil.rmtree(dst)
    dst.mkdir(parents=True)
    for rid in route_ids:
        s = GOLDEN / rid
        if s.exists():
            shutil.copytree(s, dst / rid)


def _differs(route_ids: list[str], baseline: Path) -> bool:
    for rid in route_ids:
        a, b = baseline / rid, GOLDEN / rid
        if not b.exists():
            return True
        for f in ("status", "body.bin"):
            fa, fb = a / f, b / f
            if not fa.exists() or not fb.exists() or not filecmp.cmp(fa, fb, shallow=False):
                return True
    return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--function")
    ap.add_argument("--lines")
    ap.add_argument("--only", required=True, help="comma route ids to capture+compare")
    ap.add_argument("--profile", default="base")
    ap.add_argument("--sample", type=int, default=8)
    ap.add_argument("--seed", type=int, default=1)
    args = ap.parse_args()

    route_ids = [s.strip() for s in args.only.split(",") if s.strip()]
    # src is the pristine oracle; we restore app.py from THIS in-memory copy after every mutant
    # (works inside the Docker container, where the worktree's git gitdir link is unresolvable).
    src = APP.read_text()
    src_lines = src.splitlines(keepends=True)
    tree = ast.parse(src, filename="app.py")
    lo, hi = _target_span(tree, args.function, args.lines)
    covered = _covered_lines()
    docs = _docstring_nodes(tree)
    cands = list(_mutants(tree, lo, hi, covered, docs))
    if not cands:
        print(f"No mutable covered constants in {args.function or args.lines}.")
        return 0
    rng = random.Random(args.seed)
    rng.shuffle(cands)
    cands = cands[: args.sample]
    print(f"{len(cands)} mutant(s) in {args.function or args.lines}, comparing {route_ids}", flush=True)

    baseline = REPO / "migration" / "fixtures" / "mut_baseline"
    killed = survived = 0
    try:
        _seed_and_capture(args.profile, args.only)
        _snapshot(route_ids, baseline)
        for m in cands:
            ln, _c0, _c1, orig, new = m
            APP.write_text(_apply(src_lines, m))
            try:
                _seed_and_capture(args.profile, args.only)
                dead = _differs(route_ids, baseline)
            finally:
                APP.write_text(src)  # restore the pristine oracle
            tag = "KILLED " if dead else "SURVIVED"
            killed += dead
            survived += (not dead)
            print(f"  [{tag}] L{ln}: {orig} -> {new}", flush=True)
    finally:
        APP.write_text(src)  # guarantee the pristine oracle is restored
        shutil.rmtree(baseline, ignore_errors=True)

    total = killed + survived
    score = (killed / total * 100) if total else 0.0
    print(f"\nMutation score: {killed}/{total} killed ({score:.0f}%). "
          f"{survived} survivor(s) = covered-but-unasserted holes.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
