#!/usr/bin/env python3
"""Enumerate every *_help route from the live url_map, capture its golden, and emit the
(path, endpoint, template) table the Go side needs to register handlers. One app boot.

Writes:
  migration/golden/<id>/...                  goldens for each help route
  migration/manifest/help_routes.json        [{path, endpoint, template}, ...]
and appends the help routes to migration/manifest/routes.yaml (idempotent).

Run from repo root:  .venv/bin/python migration/tools/capture_help.py
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
HELP_JSON = REPO / "migration" / "manifest" / "help_routes.json"
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


def help_routes(app_module) -> list[dict]:
    # Template mapping from the explicit registry, plus endpoint->endpoint.html fallback.
    registry = {}
    try:
        for path, endpoint, template in app_module._HELP_ROUTE_REGISTRY:
            registry[endpoint] = template
    except Exception:
        pass
    out = []
    for rule in app_module.app.url_map.iter_rules():
        ep = rule.endpoint
        if not ep.endswith("_help"):
            continue
        if "<" in rule.rule:  # help routes are param-less
            continue
        template = registry.get(ep, ep + ".html")
        if not (REPO / "templates" / template).exists():
            continue
        out.append({"path": rule.rule, "endpoint": ep, "template": template})
    out.sort(key=lambda r: r["path"])
    return out


def route_id(path: str) -> str:
    slug = path.strip("/").replace("/", "_").replace("-", "_") or "root"
    return f"get__{slug}"


async def capture(app, routes: list[dict]):
    client = app.test_client()
    for r in routes:
        resp = await client.open(r["path"], method="GET")
        body = await resp.get_data()
        d = GOLDEN / route_id(r["path"])
        d.mkdir(parents=True, exist_ok=True)
        (d / "status").write_text(str(resp.status_code))
        (d / "headers.txt").write_text("\n".join(f"{k}: {v}" for k, v in resp.headers.items()))
        (d / "body.bin").write_bytes(body)
        print(f"  {route_id(r['path']):40s} {resp.status_code} len={len(body)} <- {r['template']}")


def update_routes_yaml(routes: list[dict]):
    text = ROUTES_YAML.read_text()
    existing = text
    add = []
    for r in routes:
        rid = route_id(r["path"])
        if f"id: {rid}\n" in existing or f"id: {rid} " in existing:
            continue
        add.append(
            f"  - id: {rid}\n" f'    path: "{r["path"]}"\n' f"    methods: [GET]\n" f"    request: {{method: GET}}\n"
        )
    if add:
        ROUTES_YAML.write_text(text.rstrip() + "\n" + "".join(add))
    print(f"routes.yaml: added {len(add)} help routes")


def main() -> int:
    app_module = boot()
    routes = help_routes(app_module)
    print(f"{len(routes)} help routes:")
    HELP_JSON.write_text(json.dumps(routes, indent=2))
    asyncio.new_event_loop().run_until_complete(capture(app_module.app, routes))
    update_routes_yaml(routes)
    print(f"help_routes.json + goldens written; {len(routes)} routes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
