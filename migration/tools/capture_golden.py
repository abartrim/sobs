#!/usr/bin/env python3
"""Capture the golden corpus from the FROZEN Python app — the parity oracle.

Imports determinism BEFORE app, points the app at the seeded fixture DB, drives
Quart's test client over every request spec in the manifest, and records the exact
(status, headers, body) for each into migration/golden/<route_id>/.

Usage:
  python migration/tools/capture_golden.py                  # capture all
  python migration/tools/capture_golden.py --only get__root
  python migration/tools/capture_golden.py --verify-stable  # capture twice, diff (no write)

The --verify-stable mode is the harness self-test: a corpus that differs between two
captures of the same frozen app has a determinism leak — fix determinism.py, never
normalize around it.

Manifest format (migration/manifest/routes.yaml), per route:
  - id: get__root
    path: "/"
    methods: [GET]
    request:                 # optional; defaults to GET path with no body
      method: GET
      query: {from_ts: "1704160000", to_ts: "1704164645"}
      headers: {Accept: "text/html"}
      body_b64: null         # base64 body for POST/PATCH, or
      json: {...}            # convenience: json-encoded body + content-type
    state: []                # optional pre-request setup steps (see _apply_state)

Pure stdlib + Quart (already a project dep). Run from repo root.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
GOLDEN = REPO / "migration" / "golden"
MANIFEST = REPO / "migration" / "manifest" / "routes.yaml"
FIXTURE_DATA = REPO / "migration" / "fixtures" / "data"

sys.path.insert(0, str(REPO))
sys.path.insert(0, str(Path(__file__).resolve().parent))  # sibling tool modules


def _load_manifest() -> list[dict]:
    # Tiny YAML subset parser to avoid a pyyaml dependency would be fragile; if pyyaml
    # is available use it, else require routes.json. Keep deps minimal but robust.
    try:
        import yaml  # type: ignore

        data = yaml.safe_load(MANIFEST.read_text())
        return data["routes"]
    except Exception:
        alt = MANIFEST.with_suffix(".json")
        if alt.exists():
            return json.loads(alt.read_text())["routes"]
        raise SystemExit(
            f"Could not parse {MANIFEST}. Install pyyaml (pip) or provide routes.json. "
            "routes.yaml is the human-authored manifest with request fixtures."
        )


def _boot_app():
    # Pin the parity environment, then freeze, then import the app.
    _source_parity_env()
    os.environ["SOBS_PARITY"] = "1"
    os.environ["SOBS_DATA_DIR"] = str(FIXTURE_DATA)
    import determinism

    determinism.install()
    import app as app_module  # noqa: E402

    determinism.patch_module(app_module)
    return app_module.app


def _source_parity_env() -> None:
    env_file = REPO / "migration" / "tools" / "parity_env.sh"
    if not env_file.exists():
        return
    for line in env_file.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or not line.startswith("export "):
            continue
        kv = line[len("export ") :]
        if "=" in kv:
            k, v = kv.split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))


def _spec_request(route: dict) -> dict:
    req = route.get("request") or {}
    method = (req.get("method") or route["methods"][0]).upper()
    out = {
        "method": method,
        "path": route["path"],
        "query_string": _qs(req.get("query")),
        "headers": req.get("headers") or {},
        "data": None,
    }
    if req.get("json") is not None:
        out["data"] = json.dumps(req["json"]).encode()
        out["headers"] = {**out["headers"], "Content-Type": "application/json"}
    elif req.get("body_b64"):
        out["data"] = base64.b64decode(req["body_b64"])
    return out


def _qs(query: dict | None) -> str:
    if not query:
        return ""
    from urllib.parse import urlencode

    return urlencode(query)


async def _do_request(client, spec: dict) -> dict:
    path = spec["path"]
    if spec["query_string"]:
        path = f"{path}?{spec['query_string']}"
    resp = await client.open(
        path,
        method=spec["method"],
        headers=spec["headers"],
        data=spec["data"],
    )
    body = await resp.get_data()
    headers = [[k, v] for k, v in resp.headers.items()]
    return {"status": resp.status_code, "headers": headers, "body": body}


def _capture_all(app, routes: list[dict], only: str | None) -> dict[str, dict]:
    import asyncio

    results: dict[str, dict] = {}

    async def run():
        client = app.test_client()
        for route in routes:
            if only and route["id"] != only:
                continue
            spec = _spec_request(route)
            results[route["id"]] = await _do_request(client, spec)

    asyncio.get_event_loop().run_until_complete(run())
    return results


def _write_golden(route_id: str, resp: dict) -> str:
    d = GOLDEN / route_id
    d.mkdir(parents=True, exist_ok=True)
    (d / "status").write_text(str(resp["status"]))
    (d / "headers.txt").write_text("\n".join(f"{k}: {v}" for k, v in resp["headers"]))
    (d / "body.bin").write_bytes(resp["body"])
    sha = hashlib.sha256(resp["body"]).hexdigest()
    ctype = next((v for k, v in resp["headers"] if k.lower() == "content-type"), "")
    (d / "meta.json").write_text(
        json.dumps(
            {"content_type": ctype, "body_sha256": sha, "body_len": len(resp["body"])},
            indent=2,
        )
    )
    return sha


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only")
    ap.add_argument("--verify-stable", action="store_true")
    args = ap.parse_args()

    routes = _load_manifest()
    app = _boot_app()

    if args.verify_stable:
        a = _capture_all(app, routes, args.only)
        b = _capture_all(app, routes, args.only)
        unstable = [rid for rid in a if a[rid]["body"] != b[rid]["body"] or a[rid]["status"] != b[rid]["status"]]
        if unstable:
            print(f"DETERMINISM LEAK in {len(unstable)} routes: {unstable[:20]}")
            print("Fix migration/tools/determinism.py — do NOT normalize around this.")
            return 1
        print(f"Stable: {len(a)} routes identical across two captures.")
        return 0

    results = _capture_all(app, routes, args.only)
    index = {}
    for rid, resp in results.items():
        index[rid] = _write_golden(rid, resp)
    (GOLDEN / "INDEX.json").write_text(json.dumps(index, indent=2, sort_keys=True))
    print(f"Captured {len(results)} goldens → {GOLDEN}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
