#!/usr/bin/env python3
"""Replay the manifest against the Go server and diff vs the golden corpus.

This is the burndown loop's arbiter. Builds the Go binary, boots it in parity mode
against a fresh copy of the seeded fixtures, replays every request spec, normalizes
both sides, and diffs. Writes LEDGER.md + golden/RESULTS.json. Exits 0 iff every
non-excluded route is GREEN.

Usage:
  python migration/tools/parity_check.py                    # full corpus
  python migration/tools/parity_check.py --only get__root --bisect-body
  python migration/tools/parity_check.py --update-ledger
  python migration/tools/parity_check.py --max-diffs 5

Pure stdlib. Run from repo root.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
GOLDEN = REPO / "migration" / "golden"
MANIFEST = REPO / "migration" / "manifest" / "routes.yaml"
EXCLUSIONS = REPO / "migration" / "manifest" / "EXCLUSIONS.yaml"
FIXTURE_SRC = REPO / "migration" / "fixtures" / "data"
GO_DIR = REPO / "go"
HOST = "127.0.0.1"
PORT = int(os.environ.get("SOBS_PARITY_PORT", "8799"))

sys.path.insert(0, str(REPO / "migration" / "tools"))
import normalize as N  # noqa: E402


def _load_manifest() -> list[dict]:
    try:
        import yaml  # type: ignore

        return yaml.safe_load(MANIFEST.read_text())["routes"]
    except Exception:
        alt = MANIFEST.with_suffix(".json")
        if alt.exists():
            return json.loads(alt.read_text())["routes"]
        raise SystemExit(f"Cannot parse {MANIFEST} (install pyyaml or provide routes.json).")


def _load_exclusions() -> set[str]:
    if not EXCLUSIONS.exists():
        return set()
    try:
        import yaml  # type: ignore

        data = yaml.safe_load(EXCLUSIONS.read_text()) or {}
        return {e["id"] for e in data.get("exclusions", [])}
    except Exception:
        return set()


def _build_go() -> None:
    print("Building Go binary…")
    r = subprocess.run(["go", "build", "-o", str(GO_DIR / "sobs"), "./cmd/sobs"], cwd=GO_DIR)
    if r.returncode != 0:
        raise SystemExit("Go build failed — fix compilation before parity can run.")


def _boot_go(workdir: Path):
    env = dict(os.environ)
    env["SOBS_PARITY"] = "1"
    env["SOBS_DATA_DIR"] = str(workdir)
    env["SOBS_PORT"] = str(PORT)
    # Point chdb-go at the pinned libchdb (purego dlopen at runtime). See go/CHDB_PIN.md.
    libdefault = REPO / ".libchdb" / "libchdb.so"
    env.setdefault("CHDB_LIB_PATH", str(libdefault))
    _source_parity_env(env)
    proc = subprocess.Popen([str(GO_DIR / "sobs")], env=env, cwd=REPO)
    # wait for readiness
    for _ in range(100):
        try:
            urllib.request.urlopen(f"http://{HOST}:{PORT}/healthz", timeout=0.2)
            return proc
        except Exception:
            if proc.poll() is not None:
                raise SystemExit("Go server exited during startup.")
            time.sleep(0.1)
    proc.terminate()
    raise SystemExit("Go server did not become ready.")


def _source_parity_env(env: dict) -> None:
    f = REPO / "migration" / "tools" / "parity_env.sh"
    if not f.exists():
        return
    for line in f.read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            env.setdefault(k.strip(), v.strip().strip('"'))


def _replay(route: dict) -> dict:
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
    r = urllib.request.Request(f"http://{HOST}:{PORT}{path}", data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(r, timeout=10) as resp:
            return {
                "status": resp.status,
                "headers": [[k, v] for k, v in resp.headers.items()],
                "body": resp.read(),
            }
    except urllib.error.HTTPError as e:  # non-2xx is a valid response to compare
        return {"status": e.code, "headers": [[k, v] for k, v in e.headers.items()], "body": e.read()}


def _read_golden(route_id: str) -> dict | None:
    d = GOLDEN / route_id
    if not (d / "body.bin").exists():
        return None
    headers = []
    for line in (d / "headers.txt").read_text().splitlines():
        if ": " in line:
            k, v = line.split(": ", 1)
            headers.append([k, v])
    return {
        "status": int((d / "status").read_text().strip()),
        "headers": headers,
        "body": (d / "body.bin").read_bytes(),
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only")
    ap.add_argument("--bisect-body", action="store_true")
    ap.add_argument("--update-ledger", action="store_true")
    ap.add_argument("--max-diffs", type=int, default=10)
    args = ap.parse_args()

    routes = _load_manifest()
    excluded = _load_exclusions()

    _build_go()
    workdir = REPO / "migration" / "fixtures" / "_run"
    if workdir.exists():
        shutil.rmtree(workdir)
    shutil.copytree(FIXTURE_SRC, workdir)
    proc = _boot_go(workdir)

    results = {"green": [], "red": [], "missing_golden": [], "excluded": sorted(excluded)}
    diffs_shown = 0
    try:
        for route in routes:
            rid = route["id"]
            if rid in excluded:
                continue
            if args.only and rid != args.only:
                continue
            golden = _read_golden(rid)
            if golden is None:
                results["missing_golden"].append(rid)
                continue
            got = _replay(route)
            if N.equal(golden, got):
                results["green"].append(rid)
            else:
                results["red"].append(rid)
                if diffs_shown < args.max_diffs:
                    diffs_shown += 1
                    _print_diff(rid, golden, got, args.bisect_body)
    finally:
        proc.terminate()

    _write_results(results, routes, excluded)
    if args.update_ledger or not args.only:
        _write_ledger(results, routes, excluded)

    total = len(routes)
    print(
        f"\nGREEN {len(results['green'])} / RED {len(results['red'])} / "
        f"MISSING {len(results['missing_golden'])} / EXCLUDED {len(excluded)} / total {total}"
    )
    ok = not results["red"] and not results["missing_golden"]
    return 0 if ok else 1


def _print_diff(rid: str, golden: dict, got: dict, bisect: bool) -> None:
    print(f"\n=== RED: {rid} ===")
    if golden["status"] != got["status"]:
        print(f"  status: golden={golden['status']} go={got['status']}")
    ng, ngt = N.normalize(golden), N.normalize(got)
    if ng["headers"] != ngt["headers"]:
        gh = {k.lower() for k, _ in ng["headers"]}
        th = {k.lower() for k, _ in ngt["headers"]}
        print(f"  headers only-in-golden: {sorted(gh - th)}  only-in-go: {sorted(th - gh)}")
        for (gk, gv), (tk, tv) in zip(ng["headers"], ngt["headers"]):
            if [gk, gv] != [tk, tv]:
                print(f"    first header mismatch: golden[{gk}: {gv}] go[{tk}: {tv}]")
                break
    if golden["body"] != got["body"]:
        print(f"  body len: golden={len(golden['body'])} go={len(got['body'])}")
        if bisect:
            d = N.first_body_diff(golden["body"], got["body"])
            if d:
                off, ga, gb = d
                print(f"  first body diff @ byte {off}:")
                print(f"    golden: …{ga!r}")
                print(f"    go    : …{gb!r}")


def _write_results(results: dict, routes: list[dict], excluded: set) -> None:
    GOLDEN.mkdir(parents=True, exist_ok=True)
    (GOLDEN / "RESULTS.json").write_text(json.dumps(results, indent=2, sort_keys=True))


def _write_ledger(results: dict, routes: list[dict], excluded: set) -> None:
    status = {rid: "RED" for rid in results["red"]}
    status.update({rid: "GREEN" for rid in results["green"]})
    status.update({rid: "MISSING" for rid in results["missing_golden"]})
    status.update({rid: "EXCLUDED" for rid in excluded})
    lines = [
        "# Parity Ledger (auto-generated by parity_check.py — do not hand-edit)",
        "",
        f"**GREEN {len(results['green'])} / RED {len(results['red'])} / "
        f"MISSING {len(results['missing_golden'])} / EXCLUDED {len(excluded)} / total {len(routes)}**",
        "",
        "| status | id | path | methods | handler |",
        "|--------|----|------|---------|---------|",
    ]
    order = {"RED": 0, "MISSING": 1, "GREEN": 2, "EXCLUDED": 3}
    for route in sorted(routes, key=lambda r: (order.get(status.get(r["id"], "MISSING"), 9), r["path"])):
        rid = route["id"]
        st = status.get(rid, "MISSING")
        badge = {"GREEN": "🟢 GREEN", "RED": "🔴 RED", "MISSING": "⚪ MISSING", "EXCLUDED": "⚫ EXCLUDED"}[st]
        lines.append(
            f"| {badge} | `{rid}` | `{route['path']}` | {','.join(route['methods'])} | {route.get('handler', '')} |"
        )
    (REPO / "migration" / "LEDGER.md").write_text("\n".join(lines) + "\n")


if __name__ == "__main__":
    raise SystemExit(main())
