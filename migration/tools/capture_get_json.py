#!/usr/bin/env python3
"""Capture goldens for all param-less GET routes and print a triage table (status,
content-type, body length, body preview) so we can pick the simplest data routes to port
next. Also appends them to routes.yaml. One app boot.

Run:  .venv/bin/python migration/tools/capture_get_json.py
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
TOOLS = Path(__file__).resolve().parent
sys.path.insert(0, str(REPO))
sys.path.insert(0, str(TOOLS))

FIXTURE_DATA = REPO / "migration" / "fixtures" / "data"
GOLDEN = REPO / "migration" / "golden"
ROUTES_YAML = REPO / "migration" / "manifest" / "routes.yaml"


def boot():
    import determinism

    for line in (TOOLS / "parity_env.sh").read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))
    os.environ["SOBS_PARITY"] = "1"
    FIXTURE_DATA.mkdir(parents=True, exist_ok=True)
    os.environ["SOBS_DATA_DIR"] = str(FIXTURE_DATA)
    import app as app_module

    determinism.install()
    return app_module


def route_id(path: str, method: str) -> str:
    slug = path.strip("/").replace("/", "_").replace("-", "_").replace("<", "").replace(">", "").replace(":", "_")
    return f"{method.lower()}__{slug or 'root'}"


def candidates(app):
    out = []
    seen = set()
    for rule in app.url_map.iter_rules():
        if "GET" not in (rule.methods or set()):
            continue
        if "<" in rule.rule:
            continue
        if rule.endpoint == "static" or rule.endpoint.endswith("_help"):
            continue
        if rule.rule in ("/health", "/health/db"):
            continue
        if rule.rule in seen:
            continue
        seen.add(rule.rule)
        out.append({"path": rule.rule, "endpoint": rule.endpoint})
    out.sort(key=lambda r: r["path"])
    return out


async def run(app, routes):
    client = app.test_client()
    table = []
    for r in routes:
        resp = await client.open(r["path"], method="GET")
        body = await resp.get_data()
        ct = next((v for k, v in resp.headers.items() if k.lower() == "content-type"), "")
        rid = route_id(r["path"], "GET")
        d = GOLDEN / rid
        d.mkdir(parents=True, exist_ok=True)
        (d / "status").write_text(str(resp.status_code))
        (d / "headers.txt").write_text("\n".join(f"{k}: {v}" for k, v in resp.headers.items()))
        (d / "body.bin").write_bytes(body)
        is_json = "json" in ct
        preview = body[:80].decode("utf-8", "replace") if is_json else f"<{ct}>"
        table.append((rid, r["path"], resp.status_code, "json" if is_json else "html", len(body), preview))
    return table


def update_routes_yaml(routes):
    text = ROUTES_YAML.read_text()
    add = []
    for r in routes:
        rid = route_id(r["path"], "GET")
        if f"id: {rid}\n" in text:
            continue
        add.append(f'  - id: {rid}\n    path: "{r["path"]}"\n    methods: [GET]\n    request: {{method: GET}}\n')
    if add:
        ROUTES_YAML.write_text(text.rstrip() + "\n" + "".join(add))
    return len(add)


def main() -> int:
    app_module = boot()
    routes = candidates(app_module.app)
    table = asyncio.new_event_loop().run_until_complete(run(app_module.app, routes))
    added = update_routes_yaml(routes)
    # Print JSON routes sorted by body length (smallest = simplest first)
    jrows = sorted([t for t in table if t[3] == "json"], key=lambda t: t[4])
    print(f"\n{len(jrows)} param-less GET JSON routes (smallest first):")
    for rid, path, st, kind, n, prev in jrows:
        print(f"  {st} {n:6d}  {path:42s} {prev!r}")
    hrows = [t for t in table if t[3] == "html"]
    print(f"\n{len(hrows)} param-less GET HTML routes (need templates+data):")
    for rid, path, st, kind, n, prev in sorted(hrows, key=lambda t: t[4]):
        print(f"  {st} {n:7d}  {path}")
    print(f"\nroutes.yaml: added {added}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
