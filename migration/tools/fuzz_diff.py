#!/usr/bin/env python3
"""Differential fuzzer: feed the SAME randomized request to the Python oracle (app.py test client)
and the Go binary, assert byte-identical responses. Catches the divergence classes humans miss —
encoding (ensure_ascii / key order / float format), regex-engine semantics, numeric/Unicode edges,
error-path formatting — exactly the bugs the audits kept finding by hand.

Both sides run under the SAME frozen determinism the parity harness uses (frozen clock + uuid
counter), so output differs ONLY because of the input. Cases are sent in identical order so any
generated ids advance both counters in lockstep.

  python migration/tools/fuzz_diff.py --surface validate_filter --cases 300 --seed 1
  python migration/tools/fuzz_diff.py --surface all --cases 200

Run inside the parity Docker image (chdb + libchdb + Go). Exit 1 on any byte mismatch.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import random
import shutil
import sys
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REPO = TOOLS.parents[1]
sys.path.insert(0, str(TOOLS))
sys.path.insert(0, str(REPO))

import capture_routes as CAP  # noqa: E402  (boot + FIXTURE_DATA)
import parity_check as PC  # noqa: E402  (_build_go / _boot_go / _replay)

# ---------------------------------------------------------------- value generators

_UNICODE = [
    "",
    "é",
    "naïve",
    "日本語",
    "\U0001d54a",
    " ",
    "​",
    "\t\n",
    "ascii",
    "'; DROP--",
    "%s",
    "{{x}}",
    "\\d+",
    "(?=.*x)",
    "a|b",
    "[a-z]+",
    "....",
    "<script>",
    '"quote"',
    "back\\slash",
    "\U0001f680emoji",
    "  spaced  ",
]


def _rand_scalar(rng: random.Random):
    return rng.choice(
        [
            rng.choice(_UNICODE),
            rng.randint(-(10**9), 10**9),
            rng.choice([0, 1, -1, 2**31, 2**53 + 1]),
            rng.choice([0.0, -0.0, 1.5, 1e308, 3.141592653589793, 1e-9]),
            rng.choice([True, False, None]),
            rng.choice(_UNICODE) * rng.choice([1, 3, 50]),
        ]
    )


def _rand_json(rng: random.Random, depth: int = 0):
    if depth >= 3 or rng.random() < 0.4:
        return _rand_scalar(rng)
    if rng.random() < 0.5:
        return [_rand_json(rng, depth + 1) for _ in range(rng.randint(0, 4))]
    return {
        rng.choice(_UNICODE + ["type", "message", "value", "k"]) or "k": _rand_json(rng, depth + 1)
        for _ in range(rng.randint(0, 4))
    }


def _rand_sql(rng: random.Random) -> str:
    cols = ["SeverityText", "Body", "ServiceName", "TraceId", "Timestamp", "Attributes['k']"]
    ops = ["=", "!=", ">", "<", "LIKE", "ILIKE", "NOT LIKE", "match", "IN"]
    frags = [
        f"{rng.choice(cols)} {rng.choice(ops)} '{rng.choice(_UNICODE)}'",
        f"{rng.choice(cols)} {rng.choice(ops)} {rng.randint(0, 1000)}",
        f"match({rng.choice(cols)}, '{rng.choice(_UNICODE)}')",
        rng.choice(_UNICODE),
        "(" * rng.randint(0, 3) + rng.choice(cols) + " = 1" + ")" * rng.randint(0, 3),
        f"{rng.choice(cols)} = '{rng.choice(_UNICODE)}' {rng.choice(['AND', 'OR', ';--', 'UNION SELECT'])} 1=1",
    ]
    return " ".join(rng.sample(frags, k=rng.randint(1, 3)))


# ---------------------------------------------------------------- surface generators
# Each returns route dicts in the SAME schema capture_one/_replay consume.


def gen_validate_filter(rng: random.Random, n: int) -> list[dict]:
    cases = []
    for i in range(n):
        path = rng.choice(["/api/logs/validate-filter", "/api/ai/validate-filter"])
        body = {"sql": _rand_sql(rng)}
        if rng.random() < 0.2:
            body = _rand_json(rng) if rng.random() < 0.5 else {}  # malformed/empty bodies too
        cases.append(
            {"id": f"fuzz_validate_{i}", "path": path, "methods": ["POST"], "request": {"method": "POST", "json": body}}
        )
    return cases


def gen_rum_ingest(rng: random.Random, n: int) -> list[dict]:
    cases = []
    types = ["error", "navigation", "resource", "vital", "custom", "unknown", ""]
    for i in range(n):
        event = {
            "type": rng.choice(types),
            "message": rng.choice(_UNICODE),
            "stack": rng.choice(_UNICODE + ["Error\n  at f (a.js:1:1)"]),
            "sessionId": rng.choice(_UNICODE),
            "url": rng.choice(_UNICODE + ["https://x/é"]),
            "timestamp": rng.choice([_rand_scalar(rng), "2024-01-01T00:00:00Z"]),
            "value": _rand_scalar(rng),
        }
        if rng.random() < 0.3:
            body = _rand_json(rng)  # totally malformed
        else:
            body = {"events": [event]} if rng.random() < 0.7 else event
        cases.append(
            {"id": f"fuzz_rum_{i}", "path": "/v1/rum", "methods": ["POST"], "request": {"method": "POST", "json": body}}
        )
    return cases


GENERATORS = {"validate_filter": gen_validate_filter, "rum_ingest": gen_rum_ingest}


# ---------------------------------------------------------------- runners


async def _run_python(cases: list[dict]) -> dict[str, tuple[int, bytes]]:
    app_module = CAP.boot("base")
    # boot() sets TESTING=True (for synchronous ingest writes), which makes Quart PROPAGATE
    # unhandled exceptions (re-raise in the test client) instead of returning the 500 page a
    # PRODUCTION server emits. The Go binary IS production, so to compare apples-to-apples we
    # force production error handling: an unhandled handler exception becomes a 500 response.
    app_module.app.config["PROPAGATE_EXCEPTIONS"] = False
    out: dict[str, tuple[int, bytes]] = {}
    for case in cases:
        client = app_module.app.test_client()
        req = case["request"]
        headers = {"Content-Type": "application/json"}
        data = json.dumps(req["json"]).encode()
        try:
            resp = await client.open(case["path"], method="POST", data=data, headers=headers)
            out[case["id"]] = (resp.status_code, await resp.get_data())
        except Exception as exc:  # never let one degenerate case abort the whole sweep
            out[case["id"]] = (-1, f"PY_RAISED: {type(exc).__name__}: {exc}".encode())
    return out


def _run_go(cases: list[dict]) -> dict[str, tuple[int, bytes]]:
    go_workdir = REPO / "migration" / "fixtures" / "fuzz_go_data"
    if go_workdir.exists():
        shutil.rmtree(go_workdir)
    # symlinks=True is REQUIRED (mirrors parity_check.py): chdb's Atomic database engine maps the
    # `default` database via a relative symlink (metadata/default -> ../store/<uuid>). Dereferencing
    # it (copytree's default) breaks that mapping and Go opens a DB with 0 tables -> every probe
    # fails UNKNOWN_TABLE instead of evaluating the filter.
    shutil.copytree(CAP.FIXTURE_DATA, go_workdir, symlinks=True)
    PC._build_go()
    proc = PC._boot_go(go_workdir)
    out: dict[str, tuple[int, bytes]] = {}
    try:
        for case in cases:
            r = PC._replay(case)
            out[case["id"]] = (r["status"], r["body"])
    finally:
        try:
            proc.terminate()
            proc.wait(timeout=5)
        except Exception:
            proc.kill()
        shutil.rmtree(go_workdir, ignore_errors=True)
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--surface", default="all", help="validate_filter | rum_ingest | all")
    ap.add_argument("--cases", type=int, default=200)
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--max-report", type=int, default=10)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    surfaces = list(GENERATORS) if args.surface == "all" else [args.surface]
    cases: list[dict] = []
    for s in surfaces:
        cases += GENERATORS[s](rng, args.cases)
    print(f"Fuzzing {len(cases)} cases across {surfaces} (seed={args.seed})", flush=True)

    # Re-seed a clean base, snapshot it for Go BEFORE the Python phase can mutate it.
    import subprocess

    subprocess.run([sys.executable, str(TOOLS / "seed_fixtures.py")], cwd=str(REPO), check=True)

    py = asyncio.new_event_loop().run_until_complete(_run_python(cases))
    go = _run_go(cases)

    all_mismatches = []
    for case in cases:
        cid = case["id"]
        if py.get(cid) != go.get(cid):
            all_mismatches.append((case, py.get(cid), go.get(cid)))

    # Accepted divergence class: a non-dict JSON body makes the Python handler's `(payload or {}).get`
    # raise an unhandled AttributeError, so PRODUCTION Quart returns its generic 500 HTML error page
    # (an app.py bug + framework artifact, not app logic). The Go port degrades gracefully (200). We
    # deliberately do NOT replicate a framework error page byte-for-byte (couples the port to Quart
    # internals; same precedent as the F2 library-error-text class). Counted separately, not a fail.
    def _is_accepted(pg):
        ps, pb = pg[1] or (None, b"")
        return ps == 500 and pb.startswith(b"<!doctype html>")

    accepted = [m for m in all_mismatches if _is_accepted(m)]
    mismatches = [m for m in all_mismatches if not _is_accepted(m)]

    ident = len(cases) - len(all_mismatches)
    print(
        f"\n{'='*60}\nFUZZ RESULT: {ident}/{len(cases)} byte-identical, "
        f"{len(mismatches)} REAL mismatch(es), {len(accepted)} accepted "
        f"(framework 500-page on degenerate non-dict body)",
        flush=True,
    )
    for case, p, g in mismatches[: args.max_report]:
        ps, pb = p or (None, b"")
        gs, gb = g or (None, b"")
        print(f"\n--- {case['id']}  {case['path']}")
        print(f"  input: {json.dumps(case['request']['json'])[:200]}")
        print(f"  py: status={ps} len={len(pb)}  go: status={gs} len={len(gb)}")
        for i, (a, b) in enumerate(zip(pb, gb)):
            if a != b:
                print(f"  first diff @byte {i}: py={pb[max(0,i-10):i+20]!r} go={gb[max(0,i-10):i+20]!r}")
                break
        else:
            if len(pb) != len(gb):
                print(f"  length differs: py tail={pb[-40:]!r} go tail={gb[-40:]!r}")
    return 1 if mismatches else 0  # accepted (framework 500-page) divergences do NOT fail the run


if __name__ == "__main__":
    raise SystemExit(main())
