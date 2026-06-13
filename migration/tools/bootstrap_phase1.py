#!/usr/bin/env python3
"""One-shot Phase-1 bootstrap: import the app ONCE, then (a) dump the authoritative
runtime route manifest and (b) capture goldens for the manifest subset. Consolidated to
avoid repeated 15s app imports during iteration.

Run from repo root:  .venv/bin/python migration/tools/bootstrap_phase1.py
"""

from __future__ import annotations

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
MANIFEST = REPO / "migration" / "manifest" / "routes.yaml"
GEN_JSON = REPO / "migration" / "manifest" / "routes.generated.json"

_IGNORED = {"HEAD", "OPTIONS"}


def boot():
    import determinism

    # pin parity env
    for line in (TOOLS / "parity_env.sh").read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))
    os.environ["SOBS_PARITY"] = "1"
    FIXTURE_DATA.mkdir(parents=True, exist_ok=True)
    os.environ["SOBS_DATA_DIR"] = str(FIXTURE_DATA)

    # Import the app (and pandas/numpy/pyarrow) under the REAL clock, THEN freeze.
    import app as app_module

    determinism.install()
    return app_module


def dump_manifest(app) -> int:
    routes = []
    for rule in app.url_map.iter_rules():
        methods = sorted(m for m in (rule.methods or set()) if m not in _IGNORED) or ["GET"]
        slug = rule.rule.strip("/").replace("/", "_").replace("<", "").replace(">", "").replace(":", "_") or "root"
        routes.append(
            {
                "id": f"{methods[0].lower()}__{slug}",
                "path": rule.rule,
                "methods": methods,
                "endpoint": rule.endpoint,
                "has_params": "<" in rule.rule,
            }
        )
    routes.sort(key=lambda r: (r["path"], r["methods"]))
    GEN_JSON.write_text(json.dumps({"routes": routes}, indent=2))
    return len(routes)


def load_subset() -> list[dict]:
    try:
        import yaml

        return yaml.safe_load(MANIFEST.read_text())["routes"]
    except Exception as e:  # pragma: no cover
        raise SystemExit(f"manifest parse failed: {e}")


def capture(app, routes: list[dict]) -> None:
    import asyncio

    async def run():
        client = app.test_client()
        for route in routes:
            req = route.get("request") or {}
            method = (req.get("method") or route["methods"][0]).upper()
            path = route["path"]
            if req.get("query"):
                from urllib.parse import urlencode

                path = f"{path}?{urlencode(req['query'])}"
            data = None
            headers = dict(req.get("headers") or {})
            if req.get("json") is not None:
                data = json.dumps(req["json"]).encode()
                headers["Content-Type"] = "application/json"
            resp = await client.open(path, method=method, headers=headers, data=data)
            body = await resp.get_data()
            d = GOLDEN / route["id"]
            d.mkdir(parents=True, exist_ok=True)
            (d / "status").write_text(str(resp.status_code))
            (d / "headers.txt").write_text("\n".join(f"{k}: {v}" for k, v in resp.headers.items()))
            (d / "body.bin").write_bytes(body)
            ctype = next((v for k, v in resp.headers.items() if k.lower() == "content-type"), "")
            print(f"  captured {route['id']:34s} status={resp.status_code} ctype={ctype!r} len={len(body)}")

    loop = asyncio.new_event_loop()
    try:
        loop.run_until_complete(run())
    finally:
        loop.close()


def main() -> int:
    app_module = boot()
    n = dump_manifest(app_module.app)
    print(f"manifest: {n} runtime routes -> {GEN_JSON.name}")
    subset = load_subset()
    print(f"capturing {len(subset)} subset routes:")
    capture(app_module.app, subset)
    print(f"goldens -> {GOLDEN}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
