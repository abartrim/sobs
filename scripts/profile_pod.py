#!/usr/bin/env python3
"""Pod memory/CPU profiling script for SOBS.

Starts the SOBS server under the production pod constraints, seeds it with
realistic telemetry (matching the same traffic mix as load_example.py), waits
for memory to settle, then replays a second traffic burst and reports the peak
process RSS.

Phases
------
1. Start the app subprocess (unless --no-start is passed).
2. Wait for /health to respond.
3. Seed phase: send --seed-requests mixed OTEL/RUM/error/AI requests.
4. Settle phase: poll RSS every second; declare settled when the rolling
   10-sample std-dev drops below --settle-mib MiB.
5. Traffic phase: replay --traffic-requests to represent normal use.
6. Sample phase: poll RSS for --sample-sec seconds, record peak.
7. Report and exit.

Exit codes
----------
  0  peak RSS within --budget-mib
  1  peak RSS exceeded --budget-mib
  2  startup timeout or subprocess failure

Usage examples
--------------
    python scripts/profile_pod.py
    python scripts/profile_pod.py --budget-mib 700
    # Profiling a server that's already running (skip start):
    python scripts/profile_pod.py --no-start --base-url http://127.0.0.1:44317
"""

from __future__ import annotations

import argparse
import os
import statistics
import subprocess
import sys
import tempfile
import time
from collections import deque
from concurrent.futures import ThreadPoolExecutor, as_completed

import requests

try:
    import psutil
except ImportError:
    print("ERROR: psutil is required. Run: pip install psutil", file=sys.stderr)
    sys.exit(2)

# ---------------------------------------------------------------------------
# Pod constraint env profile
# ---------------------------------------------------------------------------
_POD_ENV = {
    "SOBS_CHDB_MAX_SERVER_MB": "256",
    "SOBS_CHDB_MARK_CACHE_MB": "8",
    "SOBS_CHDB_MAX_THREADS": "1",
    "SOBS_CHDB_SPILL_GROUP_BY_MB": "32",
    "SOBS_CHDB_SPILL_SORT_MB": "32",
}


def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="SOBS pod memory profiler")
    p.add_argument("--base-url", default="http://127.0.0.1:15420")
    p.add_argument("--port", type=int, default=15420)
    p.add_argument("--budget-mib", type=float, default=float(os.environ.get("SOBS_PROFILE_BUDGET_MIB", "600")))
    p.add_argument("--seed-requests", type=int, default=120, help="Requests sent during seeding phase")
    p.add_argument("--seed-workers", type=int, default=8, help="Concurrent threads for seeding")
    p.add_argument("--traffic-requests", type=int, default=80, help="Requests sent during profiling phase")
    p.add_argument("--traffic-workers", type=int, default=4, help="Concurrent threads for profiling traffic")
    p.add_argument("--startup-timeout", type=float, default=20.0, help="Seconds to wait for /health")
    p.add_argument("--settle-sec", type=float, default=15.0, help="Max seconds to wait for RSS to settle")
    p.add_argument("--settle-mib", type=float, default=10.0, help="RSS std-dev threshold (MiB) to declare settled")
    p.add_argument("--sample-sec", type=float, default=10.0, help="Seconds to sample RSS during profiling phase")
    p.add_argument("--no-start", action="store_true", help="Skip starting the server subprocess")
    return p.parse_args()


# ---------------------------------------------------------------------------
# Traffic generation (mirrors load_example.py mix)
# ---------------------------------------------------------------------------


def _send_one(base: str, i: int) -> tuple[str, int]:
    """Send one request from the mixed telemetry pattern, return (kind, status)."""
    m = i % 12
    ns = int(time.time() * 1_000_000_000) + i
    trace = f"{i:032x}"[-32:]
    span = f"{i:016x}"[-16:]
    parent = f"{(i - 1):016x}"[-16:]

    if m == 0:
        r = requests.post(
            f"{base}/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {
                            "attributes": [{"key": "service.name", "value": {"stringValue": f"load-svc-{i % 5}"}}]
                        },
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(ns),
                                        "severityText": "INFO",
                                        "body": {"stringValue": f"profile log {i}"},
                                        "traceId": trace,
                                        "spanId": span,
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
            timeout=8,
        )
        return ("logs", r.status_code)

    if m == 1:
        r = requests.post(
            f"{base}/v1/traces",
            json={
                "resourceSpans": [
                    {
                        "resource": {
                            "attributes": [{"key": "service.name", "value": {"stringValue": f"trace-svc-{i % 4}"}}]
                        },
                        "scopeSpans": [
                            {
                                "spans": [
                                    {
                                        "traceId": trace,
                                        "spanId": span,
                                        "parentSpanId": parent,
                                        "name": f"profile-span-{i}",
                                        "startTimeUnixNano": str(ns),
                                        "endTimeUnixNano": str(ns + 25_000_000),
                                        "status": {"code": 1},
                                        "attributes": [
                                            {"key": "http.method", "value": {"stringValue": "GET"}},
                                            {"key": "http.url", "value": {"stringValue": f"/profile/{i}"}},
                                        ],
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
            timeout=8,
        )
        return ("traces", r.status_code)

    if m == 2:
        r = requests.post(
            f"{base}/v1/errors",
            json={
                "service": f"err-svc-{i % 3}",
                "type": "RuntimeError",
                "message": f"profile error {i}",
                "stack": f"line {i}",
            },
            timeout=8,
        )
        return ("errors", r.status_code)

    if m == 3:
        r = requests.post(
            f"{base}/v1/rum",
            json=[
                {
                    "type": "pageview",
                    "timestamp": "2026-03-28T12:00:00Z",
                    "sessionId": f"sess-{i % 60}",
                    "url": f"https://example.test/page/{i}",
                    "title": f"Profile Page {i}",
                }
            ],
            timeout=8,
        )
        return ("rum", r.status_code)

    if m == 4:
        r = requests.post(
            f"{base}/v1/ai",
            json={
                "service": f"ai-svc-{i % 3}",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": f"Prompt {i}",
                "response": f"Response {i}",
                "tokens_in": 10 + (i % 7),
                "tokens_out": 3 + (i % 5),
                "duration_ms": 90 + (i % 40),
                "trace_id": trace,
                "span_id": span,
            },
            timeout=8,
        )
        return ("ai", r.status_code)

    # m 5-11: UI page browses to represent user navigating the UI
    pages = ["/", "/logs", "/traces", "/errors", "/rum", "/metrics", "/ai"]
    r = requests.get(f"{base}{pages[m % len(pages)]}", timeout=8)
    return ("ui", r.status_code)


def _blast(base: str, n: int, workers: int) -> dict[str, int]:
    """Send n requests concurrently, return counts per kind."""
    counts: dict[str, int] = {}
    with ThreadPoolExecutor(max_workers=workers) as ex:
        futs = {ex.submit(_send_one, base, i): i for i in range(n)}
        for fut in as_completed(futs):
            try:
                kind, _ = fut.result()
                counts[kind] = counts.get(kind, 0) + 1
            except Exception:
                counts["error"] = counts.get("error", 0) + 1
    return counts


# ---------------------------------------------------------------------------
# Process RSS helpers
# ---------------------------------------------------------------------------


def _rss_mib(pid: int) -> float:
    """Return total process-tree RSS for pid in MiB."""
    try:
        root = psutil.Process(pid)
        total = root.memory_info().rss
        for child in root.children(recursive=True):
            try:
                total += child.memory_info().rss
            except psutil.Error:
                pass
        return total / (1024 * 1024)
    except psutil.Error:
        return 0.0


def _wait_settled(pid: int, max_sec: float, std_threshold_mib: float) -> float:
    """Poll RSS until the 10-sample rolling std-dev drops below threshold.

    Returns the RSS (MiB) at the point we declare settled, or the last sample
    if max_sec elapsed without settling.
    """
    window: deque[float] = deque(maxlen=10)
    deadline = time.monotonic() + max_sec
    last_rss = _rss_mib(pid)
    while time.monotonic() < deadline:
        rss = _rss_mib(pid)
        window.append(rss)
        last_rss = rss
        if len(window) == 10:
            std = statistics.stdev(window)
            if std < std_threshold_mib:
                print(f"  settled at {rss:.1f} MiB (std-dev {std:.2f} MiB over last 10 samples)")
                return rss
        time.sleep(1.0)
    print(f"  settle timeout — last RSS {last_rss:.1f} MiB (may not have fully settled)")
    return last_rss


# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------


def _start_server(port: int, data_dir: str) -> subprocess.Popen[bytes]:
    env = os.environ.copy()
    env.update(_POD_ENV)
    env["PORT"] = str(port)
    env["SOBS_DATA_DIR"] = data_dir
    env["SOBS_ENABLE_FIRST_RUN_TOUR"] = "0"
    # No TESTING flag — app will auto-seed example content
    return subprocess.Popen(
        [sys.executable, "app.py"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
    )


def _wait_ready(base: str, pid: int, timeout: float) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not psutil.pid_exists(pid):
            raise RuntimeError("Server process exited before becoming ready")
        try:
            resp = requests.get(f"{base}/health", timeout=1)
            if resp.status_code == 200:
                return
        except Exception:
            pass
        time.sleep(0.5)
    raise RuntimeError(f"Server did not respond at {base}/health within {timeout}s")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> int:
    args = _parse_args()
    base = args.base_url.rstrip("/")

    proc: subprocess.Popen[bytes] | None = None
    data_dir: str | None = None

    try:
        if not args.no_start:
            data_dir = tempfile.mkdtemp(prefix="sobs-profile-")
            print(f"[1/5] Starting server on port {args.port} (data_dir={data_dir})")
            print(f"      Pod env: {_POD_ENV}")
            proc = _start_server(args.port, data_dir)
            print(f"      pid={proc.pid}")

            print(f"[2/5] Waiting for /health (timeout={args.startup_timeout}s) …")
            _wait_ready(base, proc.pid, args.startup_timeout)
            print(f"      ready — startup RSS: {_rss_mib(proc.pid):.1f} MiB")
        else:
            print("[1/5] Skipping server start (--no-start)")
            print("[2/5] Skipping startup wait (--no-start)")

        pid = proc.pid if proc else None

        # Phase 3: seed
        print(f"[3/5] Seeding with {args.seed_requests} requests ({args.seed_workers} workers) …")
        seed_counts = _blast(base, args.seed_requests, args.seed_workers)
        print(f"      sent: {dict(sorted(seed_counts.items()))}")
        if pid:
            print(f"      post-seed RSS: {_rss_mib(pid):.1f} MiB")

        # Phase 4: wait for RSS to settle
        if pid:
            print(
                f"[4/5] Waiting for RSS to settle (max {args.settle_sec}s, threshold {args.settle_mib} MiB std-dev) …"
            )
            settled_rss = _wait_settled(pid, args.settle_sec, args.settle_mib)
        else:
            print("[4/5] Skipping settle (no managed process)")
            settled_rss = 0.0

        # Phase 5: profiling traffic burst + RSS sampling
        print(
            f"[5/5] Profiling: {args.traffic_requests} requests"
            f" ({args.traffic_workers} workers) + {args.sample_sec}s RSS sampling \u2026"
        )
        peak_rss = settled_rss
        sample_end = time.monotonic() + args.sample_sec

        with ThreadPoolExecutor(max_workers=args.traffic_workers + 1) as ex:
            traffic_fut = ex.submit(_blast, base, args.traffic_requests, args.traffic_workers)

            while time.monotonic() < sample_end:
                if pid:
                    rss = _rss_mib(pid)
                    peak_rss = max(peak_rss, rss)
                time.sleep(0.25)

            traffic_counts = traffic_fut.result()

        # One final sample after traffic completes
        if pid:
            peak_rss = max(peak_rss, _rss_mib(pid))

        print(f"      traffic sent: {dict(sorted(traffic_counts.items()))}")

        # ---------------------------------------------------------------------------
        # Report
        # ---------------------------------------------------------------------------
        print()
        print("=" * 60)
        print("SOBS Pod Memory Profile")
        print("=" * 60)
        print(f"  Settled RSS (post-seed):  {settled_rss:.1f} MiB")
        print(f"  Peak RSS (during traffic): {peak_rss:.1f} MiB")
        print(f"  Budget:                    {args.budget_mib:.0f} MiB")
        print()

        if peak_rss <= args.budget_mib:
            print(f"  PASS  {peak_rss:.1f} MiB <= {args.budget_mib:.0f} MiB budget")
            print("=" * 60)
            return 0
        else:
            overage = peak_rss - args.budget_mib
            print(f"  FAIL  {peak_rss:.1f} MiB exceeds {args.budget_mib:.0f} MiB budget by {overage:.1f} MiB")
            print()
            print("  Review recent changes for memory regressions or raise the budget")
            print("  if growth is intentional: --budget-mib <new-value>")
            print("=" * 60)
            return 1

    except Exception as exc:
        print(f"\nERROR: {exc}", file=sys.stderr)
        return 2
    finally:
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
            if data_dir:
                import shutil

                shutil.rmtree(data_dir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
