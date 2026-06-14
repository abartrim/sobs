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
import re
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
GOLDEN = REPO / "migration" / "golden"
MANIFEST = REPO / "migration" / "manifest" / "routes.yaml"
GENERATED = REPO / "migration" / "manifest" / "routes.generated.json"
EXCLUSIONS = REPO / "migration" / "manifest" / "EXCLUSIONS.yaml"
FIXTURE_SRC = REPO / "migration" / "fixtures" / "data"
GO_DIR = REPO / "go"
HOST = "127.0.0.1"
PORT = int(os.environ.get("SOBS_PARITY_PORT", "8799"))

sys.path.insert(0, str(REPO / "migration" / "tools"))
import normalize as N  # noqa: E402
import profiles as PROF  # noqa: E402


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


def _load_generated() -> list[dict]:
    """The AUTHORITATIVE route surface: the live url_map dump (routes.generated.json,
    227 rules). This is what the manifest must fully cover for the migration to be done."""
    if not GENERATED.exists():
        return []
    return json.loads(GENERATED.read_text()).get("routes", [])


def _route_key(path: str, methods: list) -> str:
    """Canonical coverage key = PRIMARY_METHOD + path. Primary = first method that isn't an
    auto-added HEAD/OPTIONS. Keying by path+method (not the route id) sidesteps the two
    different id-slug schemes in the toolchain (hyphen vs underscore)."""
    real = [str(m).upper() for m in (methods or []) if str(m).upper() not in ("HEAD", "OPTIONS")]
    primary = real[0] if real else "GET"
    return f"{primary} {path}"


def _route_regex(path: str):
    """Compile a Flask rule path (with <param>/<conv:param>) into a regex that matches the
    concrete paths a manifest entry would use. Literal paths match themselves."""
    parts = re.split(r"(<[^>]+>)", path)
    out = []
    for p in parts:
        if p.startswith("<") and p.endswith(">"):
            conv = p[1:-1].split(":")[0] if ":" in p[1:-1] else ""
            out.append(".+" if conv == "path" else "[^/]+")
        else:
            out.append(re.escape(p))
    return re.compile("^" + "".join(out) + "$")


def _compute_uncovered(routes: list, generated: list, excluded: set) -> list[str]:
    """Authoritative routes (minus exclusions) that no manifest entry covers. A manifest
    entry covers a generated route when the primary methods match AND the manifest's
    (concrete) path equals the rule, or matches the rule's <param> pattern. These are
    SILENT GAPS — not ported, not tested — and they fail the run."""
    # Index manifest entries by primary method -> list of concrete paths.
    by_method: dict = {}
    for r in routes:
        method = _route_key(r["path"], r.get("methods", [])).split(" ", 1)[0]
        by_method.setdefault(method, []).append(r["path"])
    uncovered = set()
    for g in generated:
        if g.get("id") in excluded:
            continue
        key = _route_key(g["path"], g.get("methods", []))
        method, gpath = key.split(" ", 1)
        candidates = by_method.get(method, [])
        if gpath in candidates:
            continue
        if "<" in gpath:
            rx = _route_regex(gpath)
            if any(rx.match(mp) for mp in candidates):
                continue
        uncovered.add(key)
    return sorted(uncovered)


def _build_go() -> None:
    print("Building Go binary…")
    r = subprocess.run(["go", "build", "-o", str(GO_DIR / "sobs"), "./cmd/sobs"], cwd=GO_DIR)
    if r.returncode != 0:
        raise SystemExit("Go build failed — fix compilation before parity can run.")


def _boot_go(workdir: Path, extra_env: dict | None = None):
    env = dict(os.environ)
    env["SOBS_PARITY"] = "1"
    env["SOBS_FAKE_EPOCH"] = "1704164645.0"  # FIXED_EPOCH (determinism.py) — freezes the Go clock for parity
    env["SOBS_DATA_DIR"] = str(workdir)
    env["SOBS_PORT"] = str(PORT)
    # Point chdb-go at the pinned libchdb (purego dlopen at runtime). See go/CHDB_PIN.md.
    libdefault = REPO / ".libchdb" / "libchdb.so"
    env.setdefault("CHDB_LIB_PATH", str(libdefault))
    _source_parity_env(env)
    # A profile's env overlay (gate flags) is applied last so it is authoritative; the Go
    # server reads the gates once at boot, so each profile needs its own server process.
    if extra_env:
        env.update(extra_env)
    # In a many-profile run the previous chdb embedded server can still hold the data dir when the
    # next boot opens it, so the server exits during startup. Retry the boot a few times.
    for attempt in range(4):
        proc = subprocess.Popen([str(GO_DIR / "sobs")], env=env, cwd=REPO)
        for _ in range(150):  # ~15s readiness window
            try:
                urllib.request.urlopen(f"http://{HOST}:{PORT}/healthz", timeout=0.2)
                return proc
            except Exception:
                if proc.poll() is not None:
                    break  # exited during startup -> retry
                time.sleep(0.1)
        try:
            proc.terminate()
            proc.wait(timeout=5)
        except Exception:
            proc.kill()
        time.sleep(2.0 * (attempt + 1))
    raise SystemExit("Go server did not become ready after retries.")


def _seed_profile(profile: str, workdir: Path) -> None:
    """Insert a profile's extra rows into the freshly-copied fixture (via
    `seed_fixtures.py --only-profile`) so the Go server for this profile sees the seeded state —
    isolated to this pass, never in the base corpus."""
    env = dict(os.environ)
    env.setdefault("CHDB_LIB_PATH", str(REPO / ".libchdb" / "libchdb.so"))
    cmd = [
        sys.executable,
        str(REPO / "migration" / "tools" / "seed_fixtures.py"),
        "--only-profile",
        profile,
        "--data-dir",
        str(workdir),
    ]
    # The seed boots its own chdb embedded server on _run; with many profiles per run the
    # previous server's teardown can still hold the dir, so retry a few times with backoff.
    for attempt in range(4):
        r = subprocess.run(cmd, cwd=str(REPO), env=env)
        if r.returncode == 0:
            return
        time.sleep(2.0 * (attempt + 1))
    raise SystemExit(f"Profile seed failed for '{profile}' after retries.")


def _source_parity_env(env: dict) -> None:
    f = REPO / "migration" / "tools" / "parity_env.sh"
    if not f.exists():
        return
    for line in f.read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            env.setdefault(k.strip(), v.strip().strip('"'))


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None  # treat 3xx as a terminal response (raises HTTPError, caught below)


_NO_REDIRECT_OPENER = urllib.request.build_opener(_NoRedirect)


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
    elif req.get("form") is not None:
        from urllib.parse import urlencode

        data = urlencode(req["form"], doseq=True).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    elif req.get("body_b64"):
        import base64

        data = base64.b64decode(req["body_b64"])
    r = urllib.request.Request(f"http://{HOST}:{PORT}{path}", data=data, method=method, headers=headers)
    # Do NOT follow redirects: the Quart test client returns the raw 3xx (Location + flash cookie),
    # so the Go replay must compare that response, not the redirect target. Timeout is generous:
    # some routes (e.g. the agent flow) issue chdb queries that hit a permanent error the chdb
    # wrapper still retries with backoff — faithful to Python (which catches them) but several
    # seconds of latency on a cold-booted per-profile server. With ~20 profiles each booting their
    # own embedded server the host can momentarily starve, so a transient read timeout / dropped
    # connection is retried a few times rather than crashing the whole run; the slow routes are
    # mutation-safe to replay (their non-deterministic bytes — run_id, finding rows — are masked or
    # recomputed deterministically from the seed).
    import socket
    import time as _time

    last_exc: BaseException = RuntimeError("replay failed without an exception")
    for attempt in range(4):
        try:
            with _NO_REDIRECT_OPENER.open(r, timeout=45) as resp:
                if route.get("stream"):
                    body = _read_first_sse_frame(resp)
                else:
                    body = resp.read()
                return {
                    "status": resp.status,
                    "headers": [[k, v] for k, v in resp.headers.items()],
                    "body": body,
                }
        except urllib.error.HTTPError as e:  # non-2xx is a valid response to compare
            return {"status": e.code, "headers": [[k, v] for k, v in e.headers.items()], "body": e.read()}
        except (TimeoutError, socket.timeout, ConnectionError, OSError) as exc:
            last_exc = exc
            _time.sleep(2.0 * (attempt + 1))
    raise last_exc


def _read_first_sse_frame(resp) -> bytes:
    """Bounded read of an SSE stream: collect bytes only up to the first complete frame (the
    deterministic opening, e.g. `retry: 5000\\n\\n`), then stop. Subsequent frames are
    timing-dependent keepalives, so we never read them."""
    import socket

    resp.fp.raw._sock.settimeout(8)
    buf = b""
    try:
        while b"\n\n" not in buf and len(buf) < 4096:
            chunk = resp.read(1)
            if not chunk:
                break
            buf += chunk
    except (socket.timeout, TimeoutError, OSError):
        pass
    return buf


def _apply_masks(resp: dict, masks) -> dict:
    """Replace inherently-non-deterministic byte runs (random keys, wall-clock-derived
    timestamps, storage sizes) with a fixed placeholder on BOTH golden and replay before
    diffing. Each route's masks are documented in the manifest with a `mask_reason`. This is a
    surgical alternative to excluding a whole route: everything outside the masked runs is still
    compared byte-for-byte."""
    if not masks:
        return resp
    body = resp["body"]
    for pat in masks:
        body = re.sub(pat.encode("utf-8"), b"<MASKED>", body)

    def _mask_header(k: str, v: str) -> str:
        # Content-Length is rewritten to the masked body length (otherwise a masked-out
        # '12.6 KB' vs '5.0 KB' leaves a header mismatch). Other header VALUES get the same
        # masks applied (e.g. a redirect Location carrying a fresh uuid).
        if k.lower() == "content-length":
            return str(len(body))
        for pat in masks:
            v = re.sub(pat, "<MASKED>", v)
        return v

    headers = [[k, _mask_header(k, v)] for k, v in resp["headers"]]
    return {**resp, "body": body, "headers": headers}


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
    generated = _load_generated()
    uncovered = _compute_uncovered(routes, generated, excluded)
    authoritative_total = len([g for g in generated if g.get("id") not in excluded]) or len(routes)

    _build_go()

    results: dict = {
        "green": [],
        "red": [],
        "missing_golden": [],
        "excluded": sorted(excluded),
        "uncovered": uncovered,
        "authoritative_total": authoritative_total,
    }

    # Group the replayable routes by profile. Each profile is a distinct gate state (e.g. the
    # query page enabled) that the Go server reads once at boot, so each profile needs its own
    # server process against its own fresh fixture copy. "base" runs first; others sorted.
    def _wanted(route: dict) -> bool:
        rid = route["id"]
        if rid in excluded:
            return False
        if args.only and rid != args.only:
            return False
        return True

    by_profile: dict[str, list] = {}
    for route in routes:
        if _wanted(route):
            by_profile.setdefault(PROF.route_profile(route), []).append(route)
    profile_order = ["base"] + sorted(p for p in by_profile if p != "base")

    diffs_state = {"shown": 0}
    for profile in profile_order:
        prof_routes = by_profile.get(profile)
        if not prof_routes:
            continue
        workdir = REPO / "migration" / "fixtures" / "_run"
        if workdir.exists():
            shutil.rmtree(workdir)
        # symlinks=True is REQUIRED: chdb's Atomic database engine maps the `default` database
        # to its on-disk store via a relative symlink (metadata/default -> ../store/<uuid>).
        # Dereferencing it (copytree's default) breaks that mapping and the copy sees 0 tables.
        # Re-copied per profile so a profile pass never sees a prior profile's mutations.
        shutil.copytree(FIXTURE_SRC, workdir, symlinks=True)
        if profile in PROF.SEEDED_PROFILES:
            _seed_profile(profile, workdir)
            time.sleep(1.0)  # let the seed subprocess' chdb fully release before Go opens _run
        proc = _boot_go(workdir, PROF.profile_env(profile))
        try:
            for route in prof_routes:
                rid = route["id"]
                golden = _read_golden(rid)
                if golden is None:
                    results["missing_golden"].append(rid)
                    continue
                got = _replay(route)
                masks = route.get("mask")
                if N.equal(_apply_masks(golden, masks), _apply_masks(got, masks)):
                    results["green"].append(rid)
                else:
                    results["red"].append(rid)
                    if diffs_state["shown"] < args.max_diffs:
                        diffs_state["shown"] += 1
                        _print_diff(rid, golden, got, args.bisect_body)
        finally:
            # Fully release the chdb embedded server (lock + ~768MB) before the next profile's
            # copytree/seed/boot reuses the _run dir — terminate alone races the next chdb open.
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except Exception:
                proc.kill()
            time.sleep(1.0)

    _write_results(results, routes, excluded)
    if args.update_ledger or not args.only:
        _write_ledger(results, routes, excluded)

    print(
        f"\nGREEN {len(results['green'])} / RED {len(results['red'])} / "
        f"MISSING_GOLDEN {len(results['missing_golden'])} / "
        f"UNCOVERED {len(uncovered)} / EXCLUDED {len(excluded)}"
        f"  —  authoritative surface: {authoritative_total} routes"
    )
    if uncovered:
        shown = uncovered[: args.max_diffs]
        print(f"\n{len(uncovered)} route(s) NOT covered by the manifest (silent gaps — not ported/tested):")
        for k in shown:
            print(f"  UNCOVERED  {k}")
        if len(uncovered) > len(shown):
            print(f"  … and {len(uncovered) - len(shown)} more (see migration/LEDGER.md)")
    # "Migration complete" requires: every replayed route GREEN, no missing goldens, and
    # the manifest covering the entire authoritative surface (no silent gaps).
    ok = not results["red"] and not results["missing_golden"] and not uncovered
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
    uncovered = results.get("uncovered", [])
    authoritative = results.get("authoritative_total") or (
        len(results["green"]) + len(results["red"]) + len(results["missing_golden"]) + len(uncovered)
    )
    lines = [
        "# Parity Ledger (auto-generated by parity_check.py — do not hand-edit)",
        "",
        f"**GREEN {len(results['green'])} / RED {len(results['red'])} / "
        f"MISSING_GOLDEN {len(results['missing_golden'])} / UNCOVERED {len(uncovered)} / "
        f"EXCLUDED {len(excluded)}  —  authoritative surface: {authoritative} routes**",
        "",
        "UNCOVERED = an authoritative url_map route with no manifest entry: NOT ported and",
        "NOT tested. Migration is complete only when RED, MISSING_GOLDEN and UNCOVERED are 0.",
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
    for key in uncovered:
        method, _, path = key.partition(" ")
        lines.append(f"| 🟡 UNCOVERED | `—` | `{path}` | {method} | _not in manifest_ |")
    (REPO / "migration" / "LEDGER.md").write_text("\n".join(lines) + "\n")


if __name__ == "__main__":
    raise SystemExit(main())
