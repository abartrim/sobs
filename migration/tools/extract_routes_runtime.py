#!/usr/bin/env python3
"""Authoritative route extractor: dump the LIVE app.url_map.

The AST extractor (extract_routes.py) only sees literal `@app.route(...)` decorators.
The app also registers routes dynamically (e.g. _register_help_route adds 37 help
pages), so the AST count (188) is short of the real surface (227 url rules). The
runtime url_map is the source of truth for coverage accounting.

Boots the frozen app in parity mode against a throwaway data dir, enumerates every
rule, and emits routes.generated.json (consumed by capture/parity as the manifest
fallback) plus a human summary.

Usage:
  python migration/tools/extract_routes_runtime.py            # summary
  python migration/tools/extract_routes_runtime.py --json     # routes.generated.json to stdout

Run from repo root with the app's deps installed.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO))
sys.path.insert(0, str(Path(__file__).resolve().parent))

_IGNORED_METHODS = {"HEAD", "OPTIONS"}


def _route_id(rule: str, methods: list[str]) -> str:
    slug = rule.strip("/").replace("/", "_").replace("<", "").replace(">", "").replace(":", "_") or "root"
    return f"{methods[0].lower()}__{slug}"


def _boot_app():
    os.environ.setdefault("SOBS_PARITY", "1")
    os.environ.setdefault("SOBS_DATA_DIR", tempfile.mkdtemp(prefix="sobs_routes_"))
    import determinism

    determinism.install()
    import app as app_module

    determinism.patch_module(app_module)
    return app_module.app


def extract(app) -> list[dict]:
    routes = []
    for rule in app.url_map.iter_rules():
        methods = sorted(m for m in (rule.methods or set()) if m not in _IGNORED_METHODS)
        if not methods:
            methods = ["GET"]
        has_params = "<" in rule.rule
        routes.append(
            {
                "id": _route_id(rule.rule, methods),
                "path": rule.rule,
                "methods": methods,
                "endpoint": rule.endpoint,
                "has_params": has_params,
            }
        )
    routes.sort(key=lambda r: (r["path"], r["methods"]))
    return routes


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    app = _boot_app()
    routes = extract(app)

    if args.json:
        print(json.dumps({"routes": routes}, indent=2))
        return 0

    static = [r for r in routes if r["endpoint"] == "static" or r["path"].startswith("/static")]
    params = [r for r in routes if r["has_params"]]
    print(f"Total runtime rules: {len(routes)}")
    print(f"  with path params (<...>): {len(params)}")
    print(f"  static: {len(static)}")
    by_method: dict[str, int] = {}
    for r in routes:
        by_method[r["methods"][0]] = by_method.get(r["methods"][0], 0) + 1
    print(f"  by primary method: {by_method}")
    print("\nWrite routes.generated.json with --json for the authoritative manifest.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
