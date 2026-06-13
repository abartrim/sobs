#!/usr/bin/env python3
"""Extract the route surface of app.py from source (AST, not runtime import).

Source of truth for the migration's coverage accounting. Emits a YAML-ish manifest of
every @app.route, its methods, the handler name, whether it renders a template (and
which), and whether it returns JSON. The parity harness asserts every extracted route
is either captured+green or explicitly excluded.

Usage:
  python migration/tools/extract_routes.py                       # human summary to stdout
  python migration/tools/extract_routes.py --yaml > routes.generated.yaml
  python migration/tools/extract_routes.py --json-call-sites     # list json.dumps options per call

Pure stdlib. Run from repo root.
"""

from __future__ import annotations

import argparse
import ast
import json
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
APP = REPO / "app.py"


def _str(node: ast.AST) -> str | None:
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    return None


def _route_decorator(dec: ast.AST):
    """Return (path, methods) if dec is an @app.route(...)-style decorator else None."""
    if not isinstance(dec, ast.Call):
        return None
    func = dec.func
    # match app.route / app.get / app.post / bp.route ...
    if not isinstance(func, ast.Attribute):
        return None
    verb = func.attr
    if verb not in {"route", "get", "post", "put", "delete", "patch", "websocket"}:
        return None
    path = _str(dec.args[0]) if dec.args else None
    methods = None
    for kw in dec.keywords:
        if kw.arg == "methods" and isinstance(kw.value, (ast.List, ast.Tuple)):
            methods = [m for m in (_str(e) for e in kw.value.elts) if m]
    if methods is None:
        methods = ["GET"] if verb in {"route", "get"} else [verb.upper()]
    # de-dup, stable order
    seen, ordered = set(), []
    for m in methods:
        mu = m.upper()
        if mu not in seen:
            seen.add(mu)
            ordered.append(mu)
    return path, ordered


def _scan_handler(fn: ast.AST):
    """Inspect a handler body for render_template targets and json returns."""
    templates: list[str] = []
    computed_template = False
    returns_json = False
    for node in ast.walk(fn):
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            name = node.func.id
            if name in {"render_template", "render_template_string"}:
                t = _str(node.args[0]) if node.args else None
                if t:
                    templates.append(t)
                else:
                    computed_template = True
            if name in {"jsonify", "masked_jsonify"}:
                returns_json = True
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
            if node.func.attr == "dumps" and isinstance(node.func.value, ast.Name) and node.func.value.id == "json":
                returns_json = True
    return templates, computed_template, returns_json


def extract(tree: ast.AST):
    routes = []
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        decs = []
        for dec in node.decorator_list:
            r = _route_decorator(dec)
            if r:
                decs.append(r)
        if not decs:
            continue
        templates, computed, returns_json = _scan_handler(node)
        for path, methods in decs:
            if path is None:
                continue
            if templates:
                kind = "html"
            elif computed:
                kind = "html"  # computed template name still renders HTML
            elif returns_json:
                kind = "json"
            else:
                kind = "other"
            routes.append(
                {
                    "id": _route_id(path, methods),
                    "path": path,
                    "methods": methods,
                    "handler": node.name,
                    "lineno": node.lineno,
                    "kind": kind,
                    "templates": sorted(set(templates)),
                    "computed_template": computed,
                }
            )
    routes.sort(key=lambda r: (r["path"], r["methods"]))
    return routes


def _route_id(path: str, methods: list[str]) -> str:
    slug = path.strip("/").replace("/", "_").replace("<", "").replace(">", "").replace(":", "_") or "root"
    return f"{methods[0].lower()}__{slug}"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--yaml", action="store_true", help="emit manifest YAML to stdout")
    ap.add_argument("--json", action="store_true", help="emit manifest JSON to stdout")
    args = ap.parse_args()

    tree = ast.parse(APP.read_text(encoding="utf-8"), filename=str(APP))
    routes = extract(tree)

    if args.json:
        print(json.dumps(routes, indent=2))
        return 0
    if args.yaml:
        # Minimal YAML emitter (no pyyaml dep).
        print("# AUTO-GENERATED by extract_routes.py. Reconcile into routes.yaml; do not")
        print("# hand-edit this file. Request fixtures (body/query/headers/state) live in routes.yaml.")
        print("routes:")
        for r in routes:
            print(f"  - id: {r['id']}")
            print(f"    path: {json.dumps(r['path'])}")
            print(f"    methods: [{', '.join(r['methods'])}]")
            print(f"    handler: {r['handler']}")
            print(f"    lineno: {r['lineno']}")
            print(f"    kind: {r['kind']}")
            if r["templates"]:
                print(f"    templates: [{', '.join(json.dumps(t) for t in r['templates'])}]")
            if r["computed_template"]:
                print("    computed_template: true")
        return 0

    # Human summary
    by_kind: dict[str, int] = {}
    by_method: dict[str, int] = {}
    computed = 0
    for r in routes:
        by_kind[r["kind"]] = by_kind.get(r["kind"], 0) + 1
        by_method[r["methods"][0]] = by_method.get(r["methods"][0], 0) + 1
        computed += 1 if r["computed_template"] else 0
    print(f"Total routes: {len(routes)}")
    print(f"By kind: {by_kind}")
    print(f"By primary method: {by_method}")
    print(f"Routes with computed template names: {computed}")
    tmpls = sorted({t for r in routes for t in r["templates"]})
    print(f"Distinct literal templates ({len(tmpls)}): {tmpls}")
    print("\nRun with --yaml to generate migration/manifest/routes.generated.yaml")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
