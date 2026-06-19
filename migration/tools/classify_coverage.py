#!/usr/bin/env python3
"""Classify app.py's uncovered lines into an actionable, sized backlog.

Input:  migration/coverage_app.json  (from coverage_capture.py)
        app.py                        (the frozen oracle)
Output: migration/coverage_backlog.json  (machine-readable, per-function)
        migration/coverage_backlog.md    (human/agent-readable, ranked)

Each uncovered line is attributed to its innermost enclosing function (via AST), and every function
with uncovered lines is bucketed so the work is sized and routable:

  route        — has an @app.route/@app.get/@app.post/... decorator. Uncovered => needs a fixture
                 (a seeded/feature-on profile) so capture drives it and parity byte-verifies Go.
  lifecycle    — @app.before_serving/@app.after_serving/@app.while_serving, or a name that reads as a
                 background worker (loop/dispatch/scan/poll/scheduler/...). The test_client capture
                 CANNOT reach these => needs a function-level differential test, not an HTTP fixture.
  helper       — a plain def/async def. Usually reachable via its callers, so it tends to get covered
                 for free when the right route fixture is added; otherwise needs a difftest.
  module       — top-level statements not inside any function (imports run at import; __main__/serve
                 tail and defensive branches do not). Mostly dead/startup => classify+exclude.

This converts "42% unknown" into "here are N functions, here is exactly what each needs."
"""
from __future__ import annotations

import ast
import json
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]
APP = REPO / "app.py"
COV = REPO / "migration" / "coverage_app.json"

ROUTE_DECORATORS = {"route", "get", "post", "put", "delete", "patch", "websocket"}
LIFECYCLE_DECORATORS = {"before_serving", "after_serving", "while_serving",
                        "before_request", "after_request", "errorhandler", "teardown_request"}
BACKGROUND_HINTS = ("loop", "worker", "dispatch", "scan", "poll", "consume", "background",
                    "scheduler", "_tick", "lifespan", "_cron", "drain", "flush_queue",
                    "_task", "ensure_", "_run_agent", "watchdog", "reconcile")


def _decorator_name(dec: ast.expr) -> str | None:
    """Return the attribute name of an @app.<x> / @<x> decorator, else None."""
    node = dec.func if isinstance(dec, ast.Call) else dec
    if isinstance(node, ast.Attribute):
        return node.attr
    if isinstance(node, ast.Name):
        return node.id
    return None


def _route_info(func: ast.AST) -> tuple[str, dict | None]:
    """Classify a function by its decorators; return (bucket, route_meta_or_None)."""
    decs = getattr(func, "decorator_list", [])
    for dec in decs:
        name = _decorator_name(dec)
        if name in ROUTE_DECORATORS:
            path, methods = None, None
            if isinstance(dec, ast.Call):
                if dec.args and isinstance(dec.args[0], ast.Constant):
                    path = dec.args[0].value
                for kw in dec.keywords:
                    if kw.arg == "methods" and isinstance(kw.value, (ast.List, ast.Tuple)):
                        methods = [e.value for e in kw.value.elts if isinstance(e, ast.Constant)]
            if methods is None:
                methods = ["GET"] if name in ("route", "get") else [name.upper()]
            return "route", {"path": path, "methods": methods, "decorator": name}
    for dec in decs:
        if _decorator_name(dec) in LIFECYCLE_DECORATORS:
            return "lifecycle", None
    return "", None


def main() -> int:
    cov = json.loads(COV.read_text())
    app_key = next(k for k in cov["files"] if k.endswith("app.py"))
    missing = set(cov["files"][app_key]["missing_lines"])
    tree = ast.parse(APP.read_text(), filename="app.py")

    # Collect every function with its line span + bucket.
    funcs = []  # (start, end, name, bucket, route_meta)
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            start, end = node.lineno, (node.end_lineno or node.lineno)
            bucket, route_meta = _route_info(node)
            if not bucket:
                lname = node.name.lower()
                bucket = "lifecycle" if any(h in lname for h in BACKGROUND_HINTS) else "helper"
            funcs.append([start, end, node.name, bucket, route_meta])

    # Attribute each missing line to the innermost (largest start) function containing it.
    per_func: dict[int, dict] = {}
    module_missing = []
    funcs_sorted = sorted(range(len(funcs)), key=lambda i: funcs[i][0])
    for line in sorted(missing):
        owner = None
        for i in funcs_sorted:
            s, e = funcs[i][0], funcs[i][1]
            if s <= line <= e:
                owner = i if owner is None or funcs[i][0] >= funcs[owner][0] else owner
        if owner is None:
            module_missing.append(line)
            continue
        rec = per_func.setdefault(owner, {"missing": []})
        rec["missing"].append(line)

    rows = []
    for i, rec in per_func.items():
        s, e, name, bucket, route_meta = funcs[i]
        rows.append({
            "function": name,
            "bucket": bucket,
            "start": s,
            "end": e,
            "uncovered": len(rec["missing"]),
            "span": e - s + 1,
            "route": route_meta,
            "missing_sample": rec["missing"][:8],
        })
    rows.sort(key=lambda r: r["uncovered"], reverse=True)

    by_bucket: dict[str, dict] = {}
    for r in rows:
        b = by_bucket.setdefault(r["bucket"], {"functions": 0, "uncovered": 0})
        b["functions"] += 1
        b["uncovered"] += r["uncovered"]
    module_bucket = {"functions": 0, "uncovered": len(module_missing)}

    total_uncovered = sum(r["uncovered"] for r in rows) + len(module_missing)
    backlog = {
        "total_uncovered": total_uncovered,
        "summary_pct": cov["files"][app_key]["summary"]["percent_covered_display"],
        "by_bucket": {**by_bucket, "module": module_bucket},
        "functions": rows,
        "module_missing_count": len(module_missing),
        "module_missing_sample": module_missing[:40],
    }
    (REPO / "migration" / "coverage_backlog.json").write_text(json.dumps(backlog, indent=2))

    # Markdown report.
    lines = ["# app.py uncovered-line backlog",
             "",
             f"Oracle coverage **{backlog['summary_pct']}%** · **{total_uncovered}** uncovered statements.",
             "",
             "## By bucket",
             "",
             "| bucket | functions | uncovered lines | meaning |",
             "|---|---:|---:|---|"]
    meaning = {
        "route": "needs a fixture/profile (corpus expansion; byte-verifiable)",
        "lifecycle": "background/lifecycle — needs a function-level difftest (capture can't reach)",
        "helper": "usually covered when a calling route's fixture is added; else difftest",
        "module": "top-level/startup/defensive — mostly dead, classify+exclude",
    }
    for b in ("route", "helper", "lifecycle", "module"):
        if b == "module":
            lines.append(f"| module | — | {module_bucket['uncovered']} | {meaning[b]} |")
        elif b in by_bucket:
            lines.append(f"| {b} | {by_bucket[b]['functions']} | {by_bucket[b]['uncovered']} | {meaning[b]} |")

    for b in ("route", "lifecycle", "helper"):
        sub = [r for r in rows if r["bucket"] == b]
        if not sub:
            continue
        lines += ["", f"## {b} — top by uncovered lines ({len(sub)} functions)", "",
                  "| function | lines | uncovered | route |", "|---|---|---:|---|"]
        for r in sub[:40]:
            rt = ""
            if r["route"]:
                rt = f"`{','.join(r['route']['methods'])} {r['route']['path']}`"
            lines.append(f"| `{r['function']}` | {r['start']}–{r['end']} | {r['uncovered']} | {rt} |")
    (REPO / "migration" / "coverage_backlog.md").write_text("\n".join(lines) + "\n")

    print(f"Total uncovered: {total_uncovered}")
    for b in ("route", "helper", "lifecycle"):
        if b in by_bucket:
            print(f"  {b:10s}: {by_bucket[b]['functions']:4d} functions, {by_bucket[b]['uncovered']:5d} lines")
    print(f"  {'module':10s}: {'   -':>4s} functions, {module_bucket['uncovered']:5d} lines")
    print("Wrote migration/coverage_backlog.json + .md")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
