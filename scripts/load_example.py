"""
Load example for SOBS — fires mixed OTEL/RUM/AI/Error requests against a running instance.

Usage:
    python scripts/load_example.py [--base URL] [--total N] [--workers N]
    python scripts/load_example.py [--mode realistic --rps 4 --jitter-ms 250]

Defaults:
    --base      http://127.0.0.1:44317
    --total     420
    --workers   28
    --mode      load
"""

import argparse
import random
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

import requests


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="SOBS load example")
    p.add_argument("--base", default="http://127.0.0.1:44317", help="Base URL of the SOBS instance")
    p.add_argument("--total", type=int, default=420, help="Total number of requests to send")
    p.add_argument("--workers", type=int, default=28, help="Number of concurrent sender threads")
    p.add_argument(
        "--mode",
        choices=["load", "realistic"],
        default="load",
        help="load: saturate workers, realistic: pace submissions to approximate user traffic",
    )
    p.add_argument(
        "--rps",
        type=float,
        default=4.0,
        help="Target requests/second in realistic mode",
    )
    p.add_argument(
        "--jitter-ms",
        type=float,
        default=200.0,
        help="Random pacing jitter in milliseconds (+/-) around the realistic interval",
    )
    return p.parse_args()


args = parse_args()
BASE = args.base
TOTAL = args.total
WORKERS = args.workers
MODE = args.mode
RPS = args.rps
JITTER_MS = args.jitter_ms

# ---------------------------------------------------------------------------
# Metric simulation constants (Prometheus-style system metrics)
# ---------------------------------------------------------------------------
_MEM_TOTAL_BYTES = 8 * 1024**3  # 8 GiB simulated total RAM
_DISK_TOTAL_BYTES = 100 * 1024**3  # 100 GiB simulated filesystem
_DISK_READ_BYTES_PER_CYCLE = 512 * 1024  # ~512 KiB read per metrics cycle
_DISK_WRITE_BYTES_PER_CYCLE = 256 * 1024  # ~256 KiB write per metrics cycle
_DISK_READ_OPS_PER_CYCLE = 128
_DISK_WRITE_OPS_PER_CYCLE = 64
_NET_RX_BYTES_PER_CYCLE = 1024 * 1024  # ~1 MiB received per metrics cycle
_NET_TX_BYTES_PER_CYCLE = 256 * 1024  # ~256 KiB sent per metrics cycle
_NET_RX_PKTS_PER_CYCLE = 1024
_NET_TX_PKTS_PER_CYCLE = 256


def send(i: int) -> tuple[str, int]:
    m = i % 6
    ns = int(time.time() * 1_000_000_000) + i
    trace = f"{i:032x}"[-32:]
    span = f"{i:016x}"[-16:]
    parent = f"{(i - 1):016x}"[-16:]

    if m == 0:
        payload_logs: dict[str, Any] = {
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
                                    "body": {"stringValue": f"concurrent log {i}"},
                                    "traceId": trace,
                                    "spanId": span,
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r = requests.post(f"{BASE}/v1/logs", json=payload_logs, timeout=8)
        return ("logs", r.status_code)

    if m == 1:
        payload_traces: dict[str, Any] = {
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
                                    "name": f"load-span-{i}",
                                    "startTimeUnixNano": str(ns),
                                    "endTimeUnixNano": str(ns + 25_000_000),
                                    "status": {"code": 1},
                                    "attributes": [
                                        {"key": "http.method", "value": {"stringValue": "GET"}},
                                        {"key": "http.url", "value": {"stringValue": f"/load/{i}"}},
                                    ],
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r = requests.post(f"{BASE}/v1/traces", json=payload_traces, timeout=8)
        return ("traces", r.status_code)

    if m == 2:
        payload_error: dict[str, Any] = {
            "service": f"err-svc-{i % 3}",
            "type": "RuntimeError",
            "message": f"simulated error {i}",
            "stack": f"Traceback line {i}",
        }
        r = requests.post(f"{BASE}/v1/errors", json=payload_error, timeout=8)
        return ("errors", r.status_code)

    if m == 3:
        payload_rum: list[dict[str, str]] = [
            {
                "type": "pageview",
                "timestamp": "2026-03-28T12:00:00Z",
                "sessionId": f"sess-{i % 60}",
                "url": f"https://example.test/page/{i}",
                "title": f"Load Page {i}",
            }
        ]
        r = requests.post(f"{BASE}/v1/rum", json=payload_rum, timeout=8)
        return ("rum", r.status_code)

    if m == 4:
        payload_ai: dict[str, Any] = {
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
        }
        r = requests.post(f"{BASE}/v1/ai", json=payload_ai, timeout=8)
        return ("ai", r.status_code)

    # Prometheus-style system metrics: rotate through 5 families per metrics slot.
    # Families: CPU utilisation, memory usage, disk I/O, network I/O,
    #           filesystem usage + load average.
    mi = i % 5
    svc = f"host-{i % 3}"
    metrics_list: list[dict[str, Any]] = []

    if mi == 0:
        # CPU utilisation per core (gauge, %)
        metrics_list.append(
            {
                "name": "system.cpu.utilization",
                "description": "CPU utilisation by core and state",
                "unit": "%",
                "gauge": {
                    "dataPoints": [
                        {
                            "timeUnixNano": str(ns),
                            "asDouble": round(20 + (i * 7 + core * 13) % 70, 1),
                            "attributes": [
                                {"key": "cpu", "value": {"stringValue": f"cpu{core}"}},
                                {"key": "state", "value": {"stringValue": "user"}},
                            ],
                        }
                        for core in range(4)
                    ]
                },
            }
        )

    elif mi == 1:
        # Memory usage by state (gauge, bytes)
        used = int(_MEM_TOTAL_BYTES * (0.3 + (i % 40) / 100))
        cached = int(_MEM_TOTAL_BYTES * 0.15)
        free = _MEM_TOTAL_BYTES - used - cached
        metrics_list.append(
            {
                "name": "system.memory.usage",
                "description": "Memory usage by state",
                "unit": "By",
                "gauge": {
                    "dataPoints": [
                        {
                            "timeUnixNano": str(ns),
                            "asDouble": float(used),
                            "attributes": [{"key": "state", "value": {"stringValue": "used"}}],
                        },
                        {
                            "timeUnixNano": str(ns),
                            "asDouble": float(free),
                            "attributes": [{"key": "state", "value": {"stringValue": "free"}}],
                        },
                        {
                            "timeUnixNano": str(ns),
                            "asDouble": float(cached),
                            "attributes": [{"key": "state", "value": {"stringValue": "cached"}}],
                        },
                    ]
                },
            }
        )

    elif mi == 2:
        # Disk I/O bytes + operations count (sum, monotonic)
        base_read_bytes = float((i // 5) * _DISK_READ_BYTES_PER_CYCLE)
        base_write_bytes = float((i // 5) * _DISK_WRITE_BYTES_PER_CYCLE)
        metrics_list.extend(
            [
                {
                    "name": "system.disk.io",
                    "description": "Disk I/O bytes by device and direction",
                    "unit": "By",
                    "sum": {
                        "isMonotonic": True,
                        "aggregationTemporality": 2,
                        "dataPoints": [
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": base_read_bytes,
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "sda"}},
                                    {"key": "direction", "value": {"stringValue": "read"}},
                                ],
                            },
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": base_write_bytes,
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "sda"}},
                                    {"key": "direction", "value": {"stringValue": "write"}},
                                ],
                            },
                        ],
                    },
                },
                {
                    "name": "system.disk.operations",
                    "description": "Disk operations by device and direction",
                    "unit": "1",
                    "sum": {
                        "isMonotonic": True,
                        "aggregationTemporality": 2,
                        "dataPoints": [
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": float((i // 5) * _DISK_READ_OPS_PER_CYCLE),
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "sda"}},
                                    {"key": "direction", "value": {"stringValue": "read"}},
                                ],
                            },
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": float((i // 5) * _DISK_WRITE_OPS_PER_CYCLE),
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "sda"}},
                                    {"key": "direction", "value": {"stringValue": "write"}},
                                ],
                            },
                        ],
                    },
                },
            ]
        )

    elif mi == 3:
        # Network I/O bytes + packets (sum, monotonic)
        rx_bytes = float((i // 5) * _NET_RX_BYTES_PER_CYCLE)
        tx_bytes = float((i // 5) * _NET_TX_BYTES_PER_CYCLE)
        metrics_list.extend(
            [
                {
                    "name": "system.network.io",
                    "description": "Network I/O bytes by device and direction",
                    "unit": "By",
                    "sum": {
                        "isMonotonic": True,
                        "aggregationTemporality": 2,
                        "dataPoints": [
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": rx_bytes,
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "eth0"}},
                                    {"key": "direction", "value": {"stringValue": "receive"}},
                                ],
                            },
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": tx_bytes,
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "eth0"}},
                                    {"key": "direction", "value": {"stringValue": "transmit"}},
                                ],
                            },
                        ],
                    },
                },
                {
                    "name": "system.network.packets",
                    "description": "Network packets by device and direction",
                    "unit": "1",
                    "sum": {
                        "isMonotonic": True,
                        "aggregationTemporality": 2,
                        "dataPoints": [
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": float((i // 5) * _NET_RX_PKTS_PER_CYCLE),
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "eth0"}},
                                    {"key": "direction", "value": {"stringValue": "receive"}},
                                ],
                            },
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": float((i // 5) * _NET_TX_PKTS_PER_CYCLE),
                                "attributes": [
                                    {"key": "device", "value": {"stringValue": "eth0"}},
                                    {"key": "direction", "value": {"stringValue": "transmit"}},
                                ],
                            },
                        ],
                    },
                },
            ]
        )

    else:
        # mi == 4: filesystem usage (gauge, bytes) + CPU load average (gauge)
        used_disk = int(_DISK_TOTAL_BYTES * (0.4 + (i % 20) / 100))
        load_1m = round(0.5 + (i % 30) / 10, 2)
        metrics_list.extend(
            [
                {
                    "name": "system.filesystem.usage",
                    "description": "Filesystem space usage by mount point and state",
                    "unit": "By",
                    "gauge": {
                        "dataPoints": [
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": float(used_disk),
                                "attributes": [
                                    {"key": "mountpoint", "value": {"stringValue": "/"}},
                                    {"key": "state", "value": {"stringValue": "used"}},
                                ],
                            },
                            {
                                "timeUnixNano": str(ns),
                                "asDouble": float(_DISK_TOTAL_BYTES - used_disk),
                                "attributes": [
                                    {"key": "mountpoint", "value": {"stringValue": "/"}},
                                    {"key": "state", "value": {"stringValue": "free"}},
                                ],
                            },
                        ]
                    },
                },
                {
                    "name": "system.cpu.load_average.1m",
                    "description": "System load average over 1 minute",
                    "unit": "1",
                    "gauge": {"dataPoints": [{"timeUnixNano": str(ns), "asDouble": load_1m}]},
                },
            ]
        )

    payload_metrics: dict[str, Any] = {
        "resourceMetrics": [
            {
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": svc}}]},
                "scopeMetrics": [{"metrics": metrics_list}],
            }
        ]
    }
    r = requests.post(f"{BASE}/v1/metrics", json=payload_metrics, timeout=8)
    return ("metrics", r.status_code)


if __name__ == "__main__":
    print(f"base={BASE}  total={TOTAL}  workers={WORKERS}  mode={MODE}" f"  rps={RPS}  jitter_ms={JITTER_MS}")
    start = time.time()
    endpoint_counts: Counter[str] = Counter()
    status_counts: Counter[int] = Counter()
    errors: list[str] = []

    with ThreadPoolExecutor(max_workers=WORKERS) as ex:
        futures = []
        if MODE == "realistic":
            interval_sec = 1.0 / max(RPS, 0.001)
            jitter_sec = max(0.0, JITTER_MS) / 1000.0
            for i in range(1, TOTAL + 1):
                futures.append(ex.submit(send, i))
                # Pace submissions to mimic steadier real-world traffic.
                sleep_sec = max(0.0, interval_sec + random.uniform(-jitter_sec, jitter_sec))
                time.sleep(sleep_sec)
        else:
            futures = [ex.submit(send, i) for i in range(1, TOTAL + 1)]
        for fut in as_completed(futures):
            try:
                endpoint, status = fut.result()
                endpoint_counts[endpoint] += 1
                status_counts[status] += 1
            except Exception as exc:
                errors.append(str(exc))

    print("elapsed_sec", round(time.time() - start, 2))
    print("endpoint_counts", dict(sorted(endpoint_counts.items())))
    print("status_counts", dict(sorted(status_counts.items())))
    print("errors", len(errors))
    if errors:
        print("sample_error", errors[0])
