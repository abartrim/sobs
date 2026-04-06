#!/usr/bin/env python3
"""Generate deterministic OTEL + RUM validation data for SOBS.

This script emits correlated traces, logs, errors, metrics, and RUM events mapped to
fixture app/service names created by setup_repo_connect_validation.sh.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import random
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import UTC, datetime


@dataclass(frozen=True)
class FixtureService:
    name: str
    version: str
    repo_name: str
    profile: str


def _now_iso() -> str:
    return datetime.now(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _to_attr(key: str, value: str) -> dict:
    return {"key": key, "value": {"stringValue": value}}


def _post_json(base_url: str, path: str, payload: dict | list, api_key: str, timeout_sec: int) -> tuple[int, str]:
    url = f"{base_url.rstrip('/')}{path}"
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json",
    }
    if api_key:
        headers["X-API-Key"] = api_key

    req = urllib.request.Request(
        url,
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout_sec) as resp:  # noqa: S310
            body = resp.read().decode("utf-8", errors="replace")
            return resp.getcode(), body
    except urllib.error.HTTPError as exc:
        try:
            body = exc.read().decode("utf-8", errors="replace")
        except Exception:
            body = ""
        return exc.code, body


def _rand_hex(rng: random.Random, length: int) -> str:
    return "".join(rng.choice("0123456789abcdef") for _ in range(length))


def _resource_attrs(org: str, env: str, service: FixtureService) -> list[dict]:
    return [
        _to_attr("service.name", service.name),
        _to_attr("service.version", service.version),
        _to_attr("deployment.environment", env),
        _to_attr("service.namespace", f"{org}-validation"),
        _to_attr("service.git.repository_url", f"https://github.com/{org}/{service.repo_name}"),
        _to_attr("service.git.ref", f"refs/tags/v{service.version}"),
        _to_attr("telemetry.sdk.name", "opentelemetry"),
        _to_attr("telemetry.sdk.language", "python"),
        _to_attr("telemetry.sdk.version", "1.26.0"),
    ]


def _emit_trace_and_log(
    base_url: str,
    api_key: str,
    timeout_sec: int,
    org: str,
    env: str,
    service: FixtureService,
    rng: random.Random,
    route: str,
    status_code: int,
    latency_ms: int,
    severity: str,
) -> None:
    now_ns = int(time.time() * 1_000_000_000)
    start_ns = now_ns - (latency_ms * 1_000_000)
    trace_id = _rand_hex(rng, 32)
    span_id = _rand_hex(rng, 16)

    trace_payload = {
        "resourceSpans": [
            {
                "resource": {"attributes": _resource_attrs(org, env, service)},
                "scopeSpans": [
                    {
                        "spans": [
                            {
                                "traceId": trace_id,
                                "spanId": span_id,
                                "name": f"HTTP {route}",
                                "kind": 2,
                                "startTimeUnixNano": str(start_ns),
                                "endTimeUnixNano": str(now_ns),
                                "attributes": [
                                    _to_attr("http.method", "GET"),
                                    _to_attr("http.route", route),
                                    _to_attr("http.status_code", str(status_code)),
                                ],
                                "status": {"code": 1 if status_code < 500 else 2},
                            }
                        ]
                    }
                ],
            }
        ]
    }
    code, body = _post_json(base_url, "/v1/traces", trace_payload, api_key, timeout_sec)
    if code >= 300:
        raise RuntimeError(f"/v1/traces returned {code}: {body[:300]}")

    log_payload = {
        "resourceLogs": [
            {
                "resource": {"attributes": _resource_attrs(org, env, service)},
                "scopeLogs": [
                    {
                        "logRecords": [
                            {
                                "timeUnixNano": str(now_ns),
                                "severityText": severity,
                                "severityNumber": 17 if severity == "ERROR" else 9,
                                "traceId": trace_id,
                                "spanId": span_id,
                                "body": {
                                    "stringValue": (
                                        f"validation event service={service.name} version={service.version}"
                                    )
                                },
                                "attributes": [
                                    _to_attr("event.domain", "validation"),
                                    _to_attr("repo", service.repo_name),
                                ],
                            }
                        ]
                    }
                ],
            }
        ]
    }
    code, body = _post_json(base_url, "/v1/logs", log_payload, api_key, timeout_sec)
    if code >= 300:
        raise RuntimeError(f"/v1/logs returned {code}: {body[:300]}")


def _emit_error(base_url: str, api_key: str, timeout_sec: int, service: FixtureService, env: str) -> None:
    js_stack_url = f"https://cdn.local/static/{service.name}/app.min.js"
    payload = {
        "service": service.name,
        "type": "RuntimeError",
        "message": f"Validation failure path for {service.name}@{service.version}",
        "stack": (
            "RuntimeError: synthetic failure\n"
            f"  at renderCheckout ({js_stack_url}:1:1)\n"
            "  at handler (validation.py:42)"
        ),
        "attributes": {
            "deployment.environment": env,
            "service.version": service.version,
        },
    }
    code, body = _post_json(base_url, "/v1/errors", payload, api_key, timeout_sec)
    if code >= 300:
        raise RuntimeError(f"/v1/errors returned {code}: {body[:300]}")


def _emit_metrics(
    base_url: str,
    api_key: str,
    timeout_sec: int,
    org: str,
    env: str,
    service: FixtureService,
    req_total: int,
    p95_ms: float,
) -> None:
    now_ns = int(time.time() * 1_000_000_000)
    payload = {
        "resourceMetrics": [
            {
                "resource": {"attributes": _resource_attrs(org, env, service)},
                "scopeMetrics": [
                    {
                        "metrics": [
                            {
                                "name": "http.server.requests",
                                "description": "Synthetic request count for validation",
                                "unit": "1",
                                "sum": {
                                    "isMonotonic": True,
                                    "aggregationTemporality": 2,
                                    "dataPoints": [
                                        {
                                            "timeUnixNano": str(now_ns),
                                            "asDouble": float(req_total),
                                            "attributes": [_to_attr("service", service.name)],
                                        }
                                    ],
                                },
                            },
                            {
                                "name": "http.server.duration.p95",
                                "description": "Synthetic p95 latency for validation",
                                "unit": "ms",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "timeUnixNano": str(now_ns),
                                            "asDouble": float(p95_ms),
                                            "attributes": [_to_attr("service", service.name)],
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
    code, body = _post_json(base_url, "/v1/metrics", payload, api_key, timeout_sec)
    if code >= 300:
        raise RuntimeError(f"/v1/metrics returned {code}: {body[:300]}")


def _emit_rum(
    base_url: str,
    api_key: str,
    timeout_sec: int,
    org: str,
    env: str,
    service: FixtureService,
    rng: random.Random,
    sessions_per_service: int,
) -> None:
    events: list[dict] = []
    timestamp = _now_iso()
    for i in range(sessions_per_service):
        session_id = f"{service.name}-sess-{i+1}"
        route = "/checkout" if "vuln" in service.name else "/health"
        url = f"https://{service.name}.local{route}?v={service.version}"
        events.append(
            {
                "type": "pageview",
                "timestamp": timestamp,
                "sessionId": session_id,
                "url": url,
                "title": f"{service.name} {service.version}",
                "service": service.name,
                "serviceVersion": service.version,
                "environment": env,
                "repoUrl": f"https://github.com/{org}/{service.repo_name}",
            }
        )
        events.append(
            {
                "type": "resource",
                "timestamp": timestamp,
                "sessionId": session_id,
                "url": url,
                "service": service.name,
                "assetUrl": f"https://cdn.local/{service.name}/main.{service.version}.js",
                "durationMs": 20 + rng.randint(0, 30),
                "status": 200,
            }
        )
        if service.profile == "vuln":
            js_stack_url = f"https://cdn.local/static/{service.name}/app.min.js"
            events.append(
                {
                    "type": "console",
                    "timestamp": timestamp,
                    "sessionId": session_id,
                    "url": url,
                    "service": service.name,
                    "breadcrumbs": {
                        "console": [
                            {
                                "level": "error",
                                "message": "Synthetic frontend error",
                                "stack": (
                                    "Error: synthetic frontend failure\\n" f"    at renderCheckout ({js_stack_url}:1:1)"
                                ),
                            }
                        ]
                    },
                }
            )

    code, body = _post_json(base_url, "/v1/rum", events, api_key, timeout_sec)
    if code >= 300:
        raise RuntimeError(f"/v1/rum returned {code}: {body[:300]}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Generate correlated validation telemetry for SOBS")
    parser.add_argument("--base-url", default=os.environ.get("SOBS_BASE_URL", "http://127.0.0.1:44317"))
    parser.add_argument("--api-key", default=os.environ.get("SOBS_API_KEY", ""))
    parser.add_argument("--org", required=True, help="GitHub org/user used for fixture repos")
    parser.add_argument("--prefix", default="sobs-validation")
    parser.add_argument("--environment", default="prod")
    parser.add_argument("--seed", type=int, default=4242)
    parser.add_argument("--sessions-per-service", type=int, default=3)
    parser.add_argument("--timeout", type=int, default=20)
    parser.add_argument(
        "--source-map-dir",
        default=os.environ.get("SOBS_SOURCE_MAP_DIR", ""),
        help="Optional directory to seed fixture *.map files for remapping tests",
    )
    return parser.parse_args()


def _seed_source_map_fixture(source_map_dir: str, service: FixtureService) -> None:
    root = pathlib.Path(source_map_dir)
    target_dir = root / "static" / service.name
    target_dir.mkdir(parents=True, exist_ok=True)

    # Minimal source map: generated app.min.js line 1 col 1 maps to src/app.ts line 1 col 1.
    source_map = {
        "version": 3,
        "file": "app.min.js",
        "sources": ["src/app.ts"],
        "sourcesContent": ["export function renderCheckout() { throw new Error('boom'); }"],
        "names": ["renderCheckout"],
        "mappings": "AAAAA",
    }

    (target_dir / "app.min.js").write_text("function renderCheckout(){throw new Error('boom')}\n", encoding="utf-8")
    (target_dir / "app.min.js.map").write_text(json.dumps(source_map), encoding="utf-8")


def main() -> int:
    args = parse_args()
    rng = random.Random(args.seed)

    services = [
        FixtureService(
            name=f"{args.prefix}-vuln-fixture",
            version="0.1.0",
            repo_name=f"{args.prefix}-vuln-fixture",
            profile="vuln",
        ),
        FixtureService(
            name=f"{args.prefix}-fixed-fixture",
            version="1.0.0",
            repo_name=f"{args.prefix}-fixed-fixture",
            profile="fixed",
        ),
        FixtureService(
            name=f"{args.prefix}-agent-playground",
            version="0.1.0",
            repo_name=f"{args.prefix}-agent-playground",
            profile="agent",
        ),
    ]

    print("[INFO] Emitting correlated validation telemetry...")
    print(f"[INFO] Target: {args.base_url}")
    print(f"[INFO] Services: {', '.join(s.name for s in services)}")

    if args.source_map_dir:
        print(f"[INFO] Seeding source map fixtures in: {args.source_map_dir}")
        for svc in services:
            _seed_source_map_fixture(args.source_map_dir, svc)
    else:
        print("[TODO] Pass --source-map-dir (or set SOBS_SOURCE_MAP_DIR) to seed *.map fixtures for remap testing.")

    for svc in services:
        if svc.profile == "vuln":
            _emit_trace_and_log(
                args.base_url,
                args.api_key,
                args.timeout,
                args.org,
                args.environment,
                svc,
                rng,
                route="/checkout",
                status_code=500,
                latency_ms=1800,
                severity="ERROR",
            )
            _emit_error(args.base_url, args.api_key, args.timeout, svc, args.environment)
            _emit_metrics(args.base_url, args.api_key, args.timeout, args.org, args.environment, svc, 420, 1900.0)
        elif svc.profile == "fixed":
            _emit_trace_and_log(
                args.base_url,
                args.api_key,
                args.timeout,
                args.org,
                args.environment,
                svc,
                rng,
                route="/checkout",
                status_code=200,
                latency_ms=120,
                severity="INFO",
            )
            _emit_metrics(args.base_url, args.api_key, args.timeout, args.org, args.environment, svc, 640, 140.0)
        else:
            _emit_trace_and_log(
                args.base_url,
                args.api_key,
                args.timeout,
                args.org,
                args.environment,
                svc,
                rng,
                route="/agent/scan",
                status_code=200,
                latency_ms=250,
                severity="INFO",
            )
            _emit_metrics(args.base_url, args.api_key, args.timeout, args.org, args.environment, svc, 120, 280.0)

        _emit_rum(
            args.base_url,
            args.api_key,
            args.timeout,
            args.org,
            args.environment,
            svc,
            rng,
            sessions_per_service=args.sessions_per_service,
        )

    print("[INFO] Telemetry generation complete.")
    print("[TODO] Run CVE scan from SOBS UI or POST /api/enrichment/cve/scan")
    print("[TODO] Verify service.name/service.version correlations on CVE, Web Traffic, and Repo Health panels")
    return 0


if __name__ == "__main__":
    sys.exit(main())
