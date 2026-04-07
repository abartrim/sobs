#!/usr/bin/env python3
"""Emit continuous CPU/memory metrics plus periodic RUM/errors for demo walkthroughs.

Usage examples:
  python scripts/rum_walkthrough_load.py
  python scripts/rum_walkthrough_load.py --base-url http://127.0.0.1:44317
  python scripts/rum_walkthrough_load.py --duration-sec 60
  python scripts/rum_walkthrough_load.py --interval-sec 1.0 --error-every 6 --rum-every 3
"""

from __future__ import annotations

import argparse
import math
import os
import random
import secrets
import time
from collections import Counter
from datetime import datetime, timezone
from typing import Any

import requests


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Continuous RUM walkthrough telemetry generator")
    parser.add_argument("--base-url", default=os.environ.get("SOBS_BASE_URL", "http://127.0.0.1:44317"))
    parser.add_argument("--api-key", default=os.environ.get("SOBS_API_KEY", ""))
    parser.add_argument("--service", default="sobs-rum-replay-demo")
    parser.add_argument("--interval-sec", type=float, default=0.5, help="Seconds between metric emissions")
    parser.add_argument(
        "--duration-sec",
        type=int,
        default=0,
        help="Total run duration in seconds. 0 means run until Ctrl+C.",
    )
    parser.add_argument("--error-every", type=int, default=3, help="Emit one /v1/errors event every N cycles")
    parser.add_argument("--rum-every", type=int, default=2, help="Emit one /v1/rum event every N cycles")
    parser.add_argument("--trace-every", type=int, default=1, help="Emit one /v1/traces event every N cycles")
    parser.add_argument("--log-every", type=int, default=1, help="Emit one /v1/logs event every N cycles")
    parser.add_argument("--namespace", default="demo")
    parser.add_argument("--node", default="demo-node-1")
    parser.add_argument("--pod", default="demo-rum-pod")
    parser.add_argument("--seed", type=int, default=42)
    return parser.parse_args()


def _headers(api_key: str) -> dict[str, str]:
    out = {"Content-Type": "application/json"}
    if api_key:
        out["X-API-Key"] = api_key
    return out


def _post_json(base_url: str, path: str, payload: dict | list, api_key: str, timeout_sec: float = 8.0) -> int:
    response = requests.post(
        f"{base_url.rstrip('/')}{path}",
        json=payload,
        headers=_headers(api_key),
        timeout=timeout_sec,
    )
    return response.status_code


def _new_trace_context() -> tuple[str, str]:
    return secrets.token_hex(16), secrets.token_hex(8)


# ---------------------------------------------------------------------------
# Multi-span trace scenarios
# ---------------------------------------------------------------------------
# Each entry in a scenario describes one span. "label" is used internally to
# reference a span as a parent. "svc_suffix" is appended to the base service
# name so each component appears as its own service in the UI.
# "off_ms" is a (min, max) start-offset range relative to the root span start.
# "dur_ms" is a (min, max) duration range in milliseconds.
# ---------------------------------------------------------------------------
_TRACE_SCENARIOS: list[list[dict[str, Any]]] = [
    # 0 — Checkout flow (4 spans, ~400–1200 ms)
    [
        {
            "svc_suffix": "",
            "label": "root",
            "parent": None,
            "name": "POST /checkout",
            "off_ms": (0, 0),
            "dur_ms": (400, 1200),
            "attrs": {"http.method": "POST", "http.url": "https://demo.local/checkout", "http.status_code": "200"},
        },
        {
            "svc_suffix": "-auth",
            "label": "auth",
            "parent": "root",
            "name": "auth.validate_token",
            "off_ms": (5, 15),
            "dur_ms": (20, 80),
            "attrs": {"rpc.service": "AuthService", "rpc.method": "ValidateToken"},
        },
        {
            "svc_suffix": "-cart",
            "label": "cart",
            "parent": "root",
            "name": "cart.fetch_items",
            "off_ms": (30, 60),
            "dur_ms": (50, 200),
            "attrs": {"db.system": "redis", "db.operation": "GET"},
        },
        {
            "svc_suffix": "-payment",
            "label": "pay",
            "parent": "root",
            "name": "payment.charge",
            "off_ms": (250, 350),
            "dur_ms": (200, 700),
            "attrs": {"payment.provider": "stripe", "payment.currency": "USD"},
        },
    ],
    # 1 — Search flow (4 spans, ~300–1000 ms)
    [
        {
            "svc_suffix": "",
            "label": "root",
            "parent": None,
            "name": "GET /search",
            "off_ms": (0, 0),
            "dur_ms": (300, 1000),
            "attrs": {"http.method": "GET", "http.url": "https://demo.local/search", "http.status_code": "200"},
        },
        {
            "svc_suffix": "-cache",
            "label": "cache",
            "parent": "root",
            "name": "cache.lookup",
            "off_ms": (5, 10),
            "dur_ms": (8, 45),
            "attrs": {"db.system": "memcached", "cache.hit": "false"},
        },
        {
            "svc_suffix": "-search",
            "label": "srch",
            "parent": "root",
            "name": "search.execute_query",
            "off_ms": (55, 90),
            "dur_ms": (100, 500),
            "attrs": {"search.index": "products"},
        },
        {
            "svc_suffix": "-db",
            "label": "db",
            "parent": "srch",
            "name": "db.select_products",
            "off_ms": (80, 120),
            "dur_ms": (80, 300),
            "attrs": {"db.system": "postgresql", "db.operation": "SELECT"},
        },
    ],
    # 2 — User profile (3 spans, ~150–600 ms)
    [
        {
            "svc_suffix": "",
            "label": "root",
            "parent": None,
            "name": "GET /profile",
            "off_ms": (0, 0),
            "dur_ms": (150, 600),
            "attrs": {"http.method": "GET", "http.url": "https://demo.local/profile", "http.status_code": "200"},
        },
        {
            "svc_suffix": "-auth",
            "label": "auth",
            "parent": "root",
            "name": "auth.check_session",
            "off_ms": (5, 12),
            "dur_ms": (15, 60),
            "attrs": {"rpc.method": "CheckSession"},
        },
        {
            "svc_suffix": "-db",
            "label": "db",
            "parent": "root",
            "name": "db.fetch_user",
            "off_ms": (25, 50),
            "dur_ms": (50, 200),
            "attrs": {"db.system": "postgresql", "db.operation": "SELECT"},
        },
    ],
    # 3 — Simple status check (2 spans, ~40–150 ms)
    [
        {
            "svc_suffix": "",
            "label": "root",
            "parent": None,
            "name": "GET /api/status",
            "off_ms": (0, 0),
            "dur_ms": (40, 150),
            "attrs": {"http.method": "GET", "http.url": "https://demo.local/api/status", "http.status_code": "200"},
        },
        {
            "svc_suffix": "-db",
            "label": "db",
            "parent": "root",
            "name": "db.ping",
            "off_ms": (8, 15),
            "dur_ms": (5, 30),
            "attrs": {"db.system": "postgresql"},
        },
    ],
    # 4 — Background job (5 spans, ~800–3000 ms)
    [
        {
            "svc_suffix": "-worker",
            "label": "root",
            "parent": None,
            "name": "job.process_queue",
            "off_ms": (0, 0),
            "dur_ms": (800, 3000),
            "attrs": {"messaging.system": "rabbitmq", "messaging.destination": "order-events"},
        },
        {
            "svc_suffix": "-db",
            "label": "db1",
            "parent": "root",
            "name": "db.fetch_pending_orders",
            "off_ms": (10, 20),
            "dur_ms": (50, 200),
            "attrs": {"db.system": "postgresql", "db.operation": "SELECT"},
        },
        {
            "svc_suffix": "-payment",
            "label": "pay",
            "parent": "root",
            "name": "payment.process_batch",
            "off_ms": (220, 300),
            "dur_ms": (300, 900),
            "attrs": {"payment.provider": "stripe", "batch.size": "10"},
        },
        {
            "svc_suffix": "-notify",
            "label": "notify",
            "parent": "root",
            "name": "notification.send_email",
            "off_ms": (550, 700),
            "dur_ms": (80, 250),
            "attrs": {"messaging.destination": "email-queue"},
        },
        {
            "svc_suffix": "-db",
            "label": "db2",
            "parent": "root",
            "name": "db.mark_orders_complete",
            "off_ms": (700, 850),
            "dur_ms": (30, 120),
            "attrs": {"db.system": "postgresql", "db.operation": "UPDATE"},
        },
    ],
]


def _emit_trace(base_url: str, api_key: str, service: str, trace_id: str, root_span_id: str, cycle: int) -> int:
    """Emit a realistic multi-span OTLP trace.

    Rotates through _TRACE_SCENARIOS each cycle.  Every 8th cycle inflates all
    span durations 2-4x to simulate a slow / degraded request.
    """
    now_ns = int(time.time() * 1_000_000_000)
    slow = cycle % 8 == 0
    scenario = _TRACE_SCENARIOS[cycle % len(_TRACE_SCENARIOS)]

    # Pre-assign span IDs. "root" always uses the caller-supplied root_span_id
    # so that RUM / error / log signals can reference the same entry span.
    span_ids: dict[str, str] = {"root": root_span_id}
    for defn in scenario[1:]:
        label = str(defn["label"])
        if label not in span_ids:
            span_ids[label] = secrets.token_hex(8)

    # Collect spans grouped by service name so we can build resourceSpans.
    by_svc: dict[str, list[dict[str, Any]]] = {}
    for defn in scenario:
        svc_name = service + str(defn["svc_suffix"])
        label = str(defn["label"])
        parent_label = defn["parent"]
        span_id = span_ids[label]
        parent_span_id = span_ids[str(parent_label)] if parent_label else ""

        off_ms_range = defn["off_ms"]
        dur_ms_range = defn["dur_ms"]
        off_ms = random.randint(off_ms_range[0], off_ms_range[1])
        dur_ms = random.randint(dur_ms_range[0], dur_ms_range[1])
        if slow:
            dur_ms = int(dur_ms * random.uniform(2.5, 4.0))

        start_ns = now_ns + off_ms * 1_000_000
        end_ns = start_ns + dur_ms * 1_000_000
        # Mark payment span as ERROR on slow cycles to simulate a degraded path.
        status_code = 2 if (slow and label in ("pay",)) else 1

        raw_attrs: dict[str, str] = defn["attrs"]
        span: dict[str, Any] = {
            "traceId": trace_id,
            "spanId": span_id,
            "name": str(defn["name"]),
            "startTimeUnixNano": str(start_ns),
            "endTimeUnixNano": str(end_ns),
            "status": {"code": status_code},
            "attributes": [{"key": k, "value": {"stringValue": v}} for k, v in raw_attrs.items()],
        }
        if parent_span_id:
            span["parentSpanId"] = parent_span_id

        by_svc.setdefault(svc_name, []).append(span)

    resource_spans = [
        {
            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": svc}}]},
            "scopeSpans": [{"spans": svc_spans}],
        }
        for svc, svc_spans in by_svc.items()
    ]

    return _post_json(base_url, "/v1/traces", {"resourceSpans": resource_spans}, api_key)


def _emit_log(base_url: str, api_key: str, service: str, trace_id: str, span_id: str, cycle: int) -> int:
    now_ns = int(time.time() * 1_000_000_000)
    payload: dict[str, Any] = {
        "resourceLogs": [
            {
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                "scopeLogs": [
                    {
                        "logRecords": [
                            {
                                "timeUnixNano": str(now_ns),
                                "severityText": "ERROR" if cycle % 6 == 0 else "INFO",
                                "body": {"stringValue": f"demo walkthrough log cycle={cycle}"},
                                "traceId": trace_id,
                                "spanId": span_id,
                            }
                        ]
                    }
                ],
            }
        ]
    }
    return _post_json(base_url, "/v1/logs", payload, api_key)


def _emit_metrics(base_url: str, api_key: str, service: str, ns: str, node: str, pod: str, cycle: int) -> int:
    now_ns = int(time.time() * 1_000_000_000)

    # Smoothly varying values with small random noise so charts look realistic.
    t = cycle / 6.0
    cpu_util = max(2.0, min(98.0, 45.0 + (math.sin(t) * 25.0) + random.uniform(-4.0, 4.0)))
    mem_used = int((2.5 + (math.sin(t / 1.8) * 0.8) + random.uniform(-0.1, 0.1)) * 1024**3)
    mem_used = max(256 * 1024**2, min(mem_used, 7 * 1024**3))

    payload: dict[str, Any] = {
        "resourceMetrics": [
            {
                "resource": {
                    "attributes": [
                        {"key": "service.name", "value": {"stringValue": service}},
                        {"key": "k8s.namespace.name", "value": {"stringValue": ns}},
                        {"key": "k8s.node.name", "value": {"stringValue": node}},
                        {"key": "k8s.pod.name", "value": {"stringValue": pod}},
                    ]
                },
                "scopeMetrics": [
                    {
                        "metrics": [
                            {
                                "name": "system.cpu.utilization",
                                "description": "CPU utilisation",
                                "unit": "%",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "timeUnixNano": str(now_ns),
                                            "asDouble": round(cpu_util, 2),
                                            "attributes": [
                                                {"key": "cpu", "value": {"stringValue": "cpu0"}},
                                                {"key": "state", "value": {"stringValue": "user"}},
                                            ],
                                        }
                                    ]
                                },
                            },
                            {
                                "name": "system.memory.usage",
                                "description": "Memory usage by state",
                                "unit": "By",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "timeUnixNano": str(now_ns),
                                            "asDouble": float(mem_used),
                                            "attributes": [{"key": "state", "value": {"stringValue": "used"}}],
                                        }
                                    ]
                                },
                            },
                            {
                                "name": "k8s.node.cpu.usage",
                                "description": "Node CPU usage percentage",
                                "unit": "%",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "timeUnixNano": str(now_ns),
                                            "asDouble": round(cpu_util, 2),
                                            "attributes": [
                                                {"key": "k8s.node.name", "value": {"stringValue": node}},
                                                {"key": "k8s.cluster.name", "value": {"stringValue": "demo"}},
                                            ],
                                        }
                                    ]
                                },
                            },
                            {
                                "name": "k8s.node.memory.usage",
                                "description": "Node memory usage bytes",
                                "unit": "By",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "timeUnixNano": str(now_ns),
                                            "asDouble": float(mem_used),
                                            "attributes": [
                                                {"key": "k8s.node.name", "value": {"stringValue": node}},
                                                {"key": "state", "value": {"stringValue": "used"}},
                                            ],
                                        }
                                    ]
                                },
                            },
                        ]
                    }
                ],
            }
        ]
    }
    return _post_json(base_url, "/v1/metrics", payload, api_key)


def _emit_error(base_url: str, api_key: str, service: str, cycle: int, trace_id: str, span_id: str) -> int:
    payload = {
        "service": service,
        "type": "RuntimeError",
        "message": f"demo rum walkthrough error {cycle}",
        "stack": f'Traceback (most recent call last):\\n  File "demo.py", line {10 + (cycle % 40)}, in click_handler',
        "trace_id": trace_id,
        "span_id": span_id,
    }
    return _post_json(base_url, "/v1/errors", payload, api_key)


def _emit_rum(
    base_url: str,
    api_key: str,
    service: str,
    session_id: str,
    cycle: int,
    trace_id: str,
    span_id: str,
) -> int:
    now_iso = datetime.now(timezone.utc).isoformat()
    event_type = "error" if cycle % 2 == 0 else "click"
    payload: list[dict[str, Any]] = [
        {
            "type": event_type,
            "timestamp": now_iso,
            "sessionId": session_id,
            "url": f"https://demo.local/{'checkout' if cycle % 3 == 0 else 'search'}",
            "title": "Demo RUM Walkthrough",
            "app": service,
            "errorSource": "demo-click-flow" if event_type == "error" else "",
            "traceId": trace_id,
            "spanId": span_id,
            "traceFlags": 1,
        }
    ]
    return _post_json(base_url, "/v1/rum", payload, api_key)


def main() -> int:
    args = _parse_args()
    random.seed(args.seed)

    if args.interval_sec <= 0:
        raise SystemExit("--interval-sec must be > 0")
    if args.error_every <= 0:
        raise SystemExit("--error-every must be > 0")
    if args.rum_every <= 0:
        raise SystemExit("--rum-every must be > 0")
    if args.trace_every <= 0:
        raise SystemExit("--trace-every must be > 0")
    if args.log_every <= 0:
        raise SystemExit("--log-every must be > 0")

    start = time.time()
    session_id = f"walkthrough-{int(start)}"
    statuses: Counter[str] = Counter()
    cycle = 0

    print(
        f"starting walkthrough load: base={args.base_url} service={args.service} "
        f"interval={args.interval_sec}s duration={args.duration_sec or 'until-ctrl-c'}s"
    )

    try:
        while True:
            if args.duration_sec > 0 and (time.time() - start) >= args.duration_sec:
                break

            cycle += 1
            trace_id, span_id = _new_trace_context()
            metrics_status = _emit_metrics(
                args.base_url,
                args.api_key,
                args.service,
                args.namespace,
                args.node,
                args.pod,
                cycle,
            )
            statuses[f"metrics:{metrics_status}"] += 1

            if cycle % args.trace_every == 0:
                statuses[
                    f"traces:{_emit_trace(args.base_url, args.api_key, args.service, trace_id, span_id, cycle)}"
                ] += 1

            if cycle % args.log_every == 0:
                statuses[f"logs:{_emit_log(args.base_url, args.api_key, args.service, trace_id, span_id, cycle)}"] += 1

            if cycle % args.error_every == 0:
                statuses[
                    f"errors:{_emit_error(args.base_url, args.api_key, args.service, cycle, trace_id, span_id)}"
                ] += 1

            if cycle % args.rum_every == 0:
                statuses[
                    f"rum:{_emit_rum(args.base_url, args.api_key, args.service, session_id, cycle, trace_id, span_id)}"
                ] += 1

            if cycle % 10 == 0:
                elapsed = round(time.time() - start, 1)
                print(f"cycle={cycle} elapsed={elapsed}s status_counts={dict(sorted(statuses.items()))}")

            time.sleep(args.interval_sec)
    except KeyboardInterrupt:
        print("stopped by user")

    elapsed = round(time.time() - start, 1)
    print(f"done cycles={cycle} elapsed={elapsed}s status_counts={dict(sorted(statuses.items()))}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
