#!/usr/bin/env python3
"""Re-capture golden responses for specific manifest routes against the seeded fixture DB.

One app boot, deterministic. Reads request specs from manifest/routes.yaml and writes
golden/<id>/{status,headers.txt,body.bin} — the exact layout parity_check.py replays.
Use after seed_fixtures.py changes the data a route reads.

  .venv/bin/python migration/tools/capture_routes.py --only get__api_reports,get__api_web_traffic_os
  .venv/bin/python migration/tools/capture_routes.py            # all manifest routes

Pure stdlib + pyyaml. Run from repo root.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import os
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
TOOLS = Path(__file__).resolve().parent
FIXTURE_DATA = REPO / "migration" / "fixtures" / "data"
GOLDEN = REPO / "migration" / "golden"
ROUTES_YAML = REPO / "migration" / "manifest" / "routes.yaml"

sys.path.insert(0, str(REPO))
sys.path.insert(0, str(TOOLS))


def boot(profile: str = "base"):
    for line in (TOOLS / "parity_env.sh").read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))
    os.environ["SOBS_PARITY"] = "1"
    os.environ["SOBS_DATA_DIR"] = str(FIXTURE_DATA)
    # A profile's env overlay must be applied BEFORE `import app` so the module-level gate
    # reads (and _AI_ENV_OVERRIDES fallbacks) see it. Direct assignment (not setdefault):
    # the profile is authoritative for its keys.
    import profiles as P  # local module (sys.path has TOOLS)

    for k, v in P.profile_env(profile).items():
        os.environ[k] = v
    import determinism

    import app as app_module

    determinism.install()
    return app_module


def _load_routes() -> list[dict]:
    import yaml  # type: ignore

    return yaml.safe_load(ROUTES_YAML.read_text())["routes"]


async def capture_one(client, route: dict) -> tuple[str, int, int]:
    req = route.get("request") or {}
    method = (req.get("method") or route["methods"][0]).upper()
    path = route["path"]
    if req.get("query"):
        from urllib.parse import urlencode

        path = f"{path}?{urlencode(req['query'])}"
    kwargs: dict = {"method": method}
    headers = dict(req.get("headers") or {})
    if req.get("json") is not None:
        kwargs["data"] = json.dumps(req["json"]).encode()
        headers["Content-Type"] = "application/json"
    elif req.get("form") is not None:
        from urllib.parse import urlencode

        kwargs["data"] = urlencode(req["form"], doseq=True).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    elif req.get("body_b64"):
        kwargs["data"] = base64.b64decode(req["body_b64"])
    if headers:
        kwargs["headers"] = headers

    resp = await client.open(path, **kwargs)
    body = await resp.get_data()
    d = GOLDEN / route["id"]
    d.mkdir(parents=True, exist_ok=True)
    (d / "status").write_text(str(resp.status_code))
    (d / "headers.txt").write_text("\n".join(f"{k}: {v}" for k, v in resp.headers.items()))
    (d / "body.bin").write_bytes(body)
    return route["id"], resp.status_code, len(body)


async def run(app, routes: list[dict]) -> None:
    # A FRESH test client per route: each request starts with an empty session, so flash()
    # messages do not accumulate across routes (Quart flashes persist in the session until a
    # render consumes them — a shared client would pile redirect-route flashes into later
    # cookies, making the goldens capture-order-dependent).
    for route in routes:
        # SSE/streaming routes never terminate under the frozen-monotonic clock (the keepalive
        # loop's asyncio timeout never fires), so `await resp.get_data()` would hang forever.
        # Their golden (the deterministic opening frame) is captured out-of-band and is
        # independent of fixture data, so skip them here. parity_check reads the bounded frame.
        if route.get("stream"):
            print(f"  skipped {route['id']}: stream (golden preserved)")
            continue
        client = app.test_client()
        rid, status, n = await capture_one(client, route)
        print(f"  captured {rid}: {status} {n}B")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", help="comma-separated route ids; default = all manifest routes")
    ap.add_argument(
        "--profile",
        default="base",
        help="capture profile (env overlay flipping a feature gate); default 'base'. "
        "Only routes whose `profile:` matches are captured — run once per profile.",
    )
    args = ap.parse_args()

    import profiles as P  # local module (sys.path has TOOLS)

    app_module = boot(args.profile)
    routes = _load_routes()
    # A profile captures ONLY its own routes (each in a fresh process so the gate env and the
    # determinism counter are isolated). The base profile carries every untagged route.
    routes = [r for r in routes if P.route_profile(r) == args.profile]
    if args.only:
        wanted = {s.strip() for s in args.only.split(",") if s.strip()}
        routes = [r for r in routes if r["id"] in wanted]
        missing = wanted - {r["id"] for r in routes}
        if missing:
            raise SystemExit(f"Unknown route ids (or not in profile '{args.profile}'): {sorted(missing)}")
    print(f"Capturing {len(routes)} route(s) [profile={args.profile}] against {FIXTURE_DATA}…")
    asyncio.new_event_loop().run_until_complete(run(app_module.app, routes))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
