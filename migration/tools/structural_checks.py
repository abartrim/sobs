#!/usr/bin/env python3
"""Cheap, continuous structural checks that catch the recurring migration failure classes the
byte-diff can miss. Read-only static analysis (no chdb / no capture needed).

  1. ROUTE MATRIX   — every app.py @app.route/@app.get/... path must have a Go counterpart
                      (exact, or covered by a Go trailing-slash subtree route). Unmapped = suspect
                      missing handler. This is the HARD check (non-zero exit on a real gap).
  2. CONSTANT DIFF  — module-level allowlists / sensitive-key sets / status maps in app.py whose
                      string members do NOT appear anywhere in the Go source = suspect divergence
                      (a dropped allowlist entry silently changes behavior). Advisory.
  3. STUB SIGNATURES— Go deferral comments + hardcoded-empty-shaped returns = places a handler may
                      be a stub where Python computes. Advisory (heuristic, expect some benign hits).

Output: migration/structural_report.md + stdout summary. Exit 1 iff the route matrix has a gap.
"""
from __future__ import annotations

import ast
import re
import subprocess
import sys
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]
APP = REPO / "app.py"
GO_DIR = REPO / "go"

ROUTE_DECORATORS = {"route", "get", "post", "put", "delete", "patch", "websocket"}
CONST_NAME_HINT = re.compile(r"(ALLOW|SENSITIVE|VALID|STATUS|_KEYS|_FIELDS|_TYPES|_NAMES|REDACT|MASK|_SET)")


def _go_source() -> str:
    parts = []
    for p in sorted(GO_DIR.rglob("*.go")):
        if "/vendor/" in str(p):
            continue
        parts.append(p.read_text(errors="ignore"))
    return "\n".join(parts)


def _python_routes(tree: ast.AST) -> list[tuple[str, str]]:
    routes = []
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        for dec in getattr(node, "decorator_list", []):
            call = dec if isinstance(dec, ast.Call) else None
            fn = call.func if call else dec
            name = fn.attr if isinstance(fn, ast.Attribute) else (fn.id if isinstance(fn, ast.Name) else None)
            if name in ROUTE_DECORATORS and call and call.args and isinstance(call.args[0], ast.Constant):
                routes.append((call.args[0].value, node.name))
    return routes


def _canon(path: str) -> str:
    """Canonicalize a route: Python <x> and Go {x} dynamic segments -> '*'; Go '/{$}' (exact root)
    -> '/'."""
    path = path.replace("/{$}", "/")
    path = re.sub(r"\{[^}]*\}", "*", path)   # Go {param}
    path = re.sub(r"<[^>]*>", "*", path)     # Python <param>
    return path


def _go_routes(go_src: str) -> set[str]:
    return set(re.findall(r's\.route\("([^"]+)"', go_src))


def check_route_matrix(tree, go_src) -> tuple[list, list]:
    py = _python_routes(tree)
    go = _go_routes(go_src)
    go_canon = {_canon(r) for r in go}
    go_prefixes = {r for r in go_canon if r.endswith("/")}
    missing = []
    for path, fn in py:
        cpath = _canon(path)
        if cpath in go_canon:
            continue
        static_prefix = cpath.split("*")[0]  # up to first dynamic segment
        if any(static_prefix.startswith(gp) or cpath.startswith(gp) for gp in go_prefixes):
            continue
        if static_prefix.rstrip("/") in go_canon:
            continue
        missing.append((path, fn))
    return missing, py


def check_constants(tree, go_src) -> list[tuple[str, list[str]]]:
    findings = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Assign):
            continue
        targets = [t.id for t in node.targets if isinstance(t, ast.Name)]
        name = next((t for t in targets if t.isupper() and CONST_NAME_HINT.search(t)), None)
        if not name:
            continue
        members: list[str] = []
        val = node.value
        if isinstance(val, (ast.Set, ast.List, ast.Tuple)):
            members = [e.value for e in val.elts if isinstance(e, ast.Constant) and isinstance(e.value, str)]
        elif isinstance(val, ast.Dict):
            members = [k.value for k in val.keys if isinstance(k, ast.Constant) and isinstance(k.value, str)]
        if not members:
            continue
        # substring match: an allowlist member counts as present if its text appears anywhere in the
        # Go source — including inside a larger SQL string literal (e.g. "... IN ('name', ...)").
        absent = [m for m in members if m and m not in go_src]
        if absent:
            findings.append((name, absent))
    return findings


def check_stubs() -> list[str]:
    # High-signal markers only — "placeholder"/"simplified" match legit SQL-building code, too noisy.
    pat = r"\bTODO\b|\bFIXME\b|\bXXX\b|\bHACK\b|not ported|NOT PORTED|not implemented|wired separately"
    try:
        out = subprocess.run(
            ["grep", "-rniE", pat, str(GO_DIR / "cmd" / "sobs"), str(GO_DIR / "internal")],
            capture_output=True, text=True, cwd=str(REPO))
        lines = [ln for ln in out.stdout.splitlines() if ln.strip()]
    except Exception:
        lines = []
    return lines


def main() -> int:
    tree = ast.parse(APP.read_text(), filename="app.py")
    go_src = _go_source()

    missing_routes, all_py = check_route_matrix(tree, go_src)
    const_findings = check_constants(tree, go_src)
    stub_lines = check_stubs()

    md = ["# Structural checks", "",
          f"- Python routes: **{len(all_py)}**, unmapped in Go: **{len(missing_routes)}**",
          f"- Constant collections with members absent from Go source: **{len(const_findings)}**",
          f"- Go stub/deferral markers: **{len(stub_lines)}**", ""]

    md.append("## 1. Route matrix (HARD)")
    if not missing_routes:
        md.append("\n✅ Every app.py route has a Go counterpart (exact or subtree).")
    else:
        md.append("\n⚠️ Python routes with no Go handler:\n")
        for path, fn in missing_routes:
            md.append(f"- `{path}` → `{fn}`")

    md.append("\n## 2. Constant/allowlist diff (advisory)")
    if not const_findings:
        md.append("\n✅ No module-level allowlist/sensitive-key members missing from Go.")
    else:
        md.append("\nMembers present in app.py constants but NOT found in Go source (verify each):\n")
        for name, absent in const_findings:
            md.append(f"- `{name}`: {', '.join(repr(a) for a in absent[:20])}")

    md.append("\n## 3. Stub / deferral signatures in Go (advisory)")
    md.append(f"\n{len(stub_lines)} markers (review for real gaps; many are benign):\n")
    for ln in stub_lines[:60]:
        md.append(f"- `{ln.strip()[:160]}`")
    (REPO / "migration" / "structural_report.md").write_text("\n".join(md) + "\n")

    print(f"route matrix:   {len(all_py)} py routes, {len(missing_routes)} unmapped")
    print(f"constant diff:  {len(const_findings)} collections with absent members")
    print(f"stub markers:   {len(stub_lines)} (advisory)")
    print("Wrote migration/structural_report.md")
    if missing_routes:
        print("::error::route matrix gap — Python routes without a Go handler (see report)")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
