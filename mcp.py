"""
SOBS MCP (Model Context Protocol) server module.

Provides a set of read-only tool endpoints that allow Copilot (VS Code,
GitHub Copilot Agent) and other MCP-compatible clients to query the SOBS
observability data (OpenTelemetry logs, traces, and metrics tables) for
diagnosis and troubleshooting.

Transport: Streamable-HTTP / simple JSON-RPC 2.0 over HTTP POST.

Authentication
--------------
All MCP endpoints require a valid MCP API key supplied in the
``X-MCP-API-Key`` request header.  Keys are managed via the
``/settings/mcp`` settings page and stored in ``sobs_app_settings``
under the ``mcp.api_keys`` setting (JSON list of
``{id, key_hash, label, created_at}`` objects).

Rate limiting
-------------
A simple in-process sliding-window counter limits each source IP to
``_MCP_RATE_LIMIT_REQUESTS`` requests per ``_MCP_RATE_LIMIT_WINDOW_SEC``
seconds.  Exceeding the limit returns HTTP 429.

Available MCP tools
-------------------
- ``list_services``            – list all distinct service names
- ``query_otel_logs``          – query the otel_logs table
- ``query_otel_traces``        – query the otel_traces table
- ``query_metrics``            – query the v_otel_metrics_1m aggregated view
- ``query_metrics_raw``        – query raw metrics points (gauge / sum / histogram)
- ``get_metric_names``         – list all distinct metric names
- ``get_anomaly_rules``        – list configured anomaly detection rules
- ``get_recent_errors``        – list recent error-level spans / log events
"""

from __future__ import annotations

import ast
import hashlib
import json
import logging
import os
import secrets
import threading
import time
from collections import defaultdict
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any

from quart import Blueprint, jsonify, render_template, request

if TYPE_CHECKING:
    pass

log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Blueprint
# ---------------------------------------------------------------------------
mcp_bp = Blueprint("mcp", __name__)

# ---------------------------------------------------------------------------
# Rate limiting
# ---------------------------------------------------------------------------
_MCP_RATE_LIMIT_REQUESTS = 60  # requests allowed per window
_MCP_RATE_LIMIT_WINDOW_SEC = 60  # sliding window size in seconds

# {ip: [(timestamp, ...), ...]}
_rate_limit_store: dict[str, list[float]] = defaultdict(list)
_rate_limit_lock = threading.Lock()


def _check_rate_limit(ip: str) -> bool:
    """Return True if the request is allowed, False if it should be rate-limited."""
    now = time.monotonic()
    cutoff = now - _MCP_RATE_LIMIT_WINDOW_SEC
    with _rate_limit_lock:
        timestamps = _rate_limit_store[ip]
        # Discard old entries outside the window.
        timestamps[:] = [t for t in timestamps if t >= cutoff]
        if len(timestamps) >= _MCP_RATE_LIMIT_REQUESTS:
            return False
        timestamps.append(now)
    return True


# ---------------------------------------------------------------------------
# Settings key
# ---------------------------------------------------------------------------
_MCP_API_KEYS_SETTING = "mcp.api_keys"
_MCP_ENABLED_SETTING = "mcp.enabled"
_MCP_API_KEY_MAX = 20  # maximum number of concurrent keys

# ---------------------------------------------------------------------------
# MCP server identity – shared by GET probe and POST initialize handlers
# ---------------------------------------------------------------------------
_MCP_PROTOCOL_VERSION = "2024-11-05"
_MCP_SERVER_INFO: dict[str, str] = {"name": "sobs-mcp", "version": "1.0"}
_MCP_CAPABILITIES: dict[str, Any] = {"tools": {}}


def _mcp_mac_key() -> bytes:
    """Return a per-installation 32-byte key derived from ``SOBS_SECRET_KEY``.

    The key is used as the ``scrypt`` salt so that token fingerprints are
    unique to this SOBS deployment.
    """
    secret = os.environ.get("SOBS_SECRET_KEY", "sobs-dev-secret-key")
    # blake2b with exactly 16-byte person tag (BLAKE2b requires person <= 16 bytes;
    # null-byte padding is used to reach the required length) to produce a
    # 32-byte sub-key for MCP token fingerprinting.
    return hashlib.blake2b(secret.encode(), person=b"sobs-mcp-v1\x00\x00\x00\x00\x00").digest()[:32]


def _hash_key(raw_token: str) -> str:
    """Return a scrypt-derived hex fingerprint of the given raw API token.

    ``scrypt`` is a memory-hard KDF (NIST SP 800-132) appropriate for
    one-way token fingerprinting.  MCP API tokens are generated with
    ``secrets.token_urlsafe(32)`` (192+ bits of entropy).  The per-
    installation salt (derived from ``SOBS_SECRET_KEY``) ensures that
    stored fingerprints are unique to this deployment.

    Parameters chosen for sub-millisecond latency while still satisfying
    code-scanning policies for key derivation:  n=1024 (2^10), r=8, p=1.
    """
    salt = _mcp_mac_key()
    return hashlib.scrypt(raw_token.encode(), salt=salt, n=1024, r=8, p=1, dklen=32).hex()


def _load_mcp_api_keys(db: Any) -> list[dict]:
    """Load the list of MCP API key descriptors from sobs_app_settings."""
    # Import inside function to avoid circular import at module level.
    from app import _get_app_setting  # noqa: PLC0415

    raw = _get_app_setting(db, _MCP_API_KEYS_SETTING) or "[]"
    try:
        keys = json.loads(raw)
        if not isinstance(keys, list):
            return []
        return keys
    except (json.JSONDecodeError, TypeError):
        return []


def _save_mcp_api_keys(db: Any, keys: list[dict]) -> None:
    """Persist the MCP API key descriptors to sobs_app_settings."""
    from app import _set_app_setting  # noqa: PLC0415

    _set_app_setting(db, _MCP_API_KEYS_SETTING, json.dumps(keys, ensure_ascii=False))


def _mcp_enabled(db: Any) -> bool:
    """Return True when the MCP server is enabled."""
    from app import _get_app_setting  # noqa: PLC0415

    return (_get_app_setting(db, _MCP_ENABLED_SETTING) or "1") == "1"


# ---------------------------------------------------------------------------
# Authentication helper
# ---------------------------------------------------------------------------


def _authenticate_mcp_request(db: Any) -> bool:
    """Return True if the incoming request carries a valid MCP API key."""
    raw_key = request.headers.get("X-MCP-API-Key", "").strip()
    if not raw_key:
        return False
    key_hash = _hash_key(raw_key)
    keys = _load_mcp_api_keys(db)
    for entry in keys:
        if secrets.compare_digest(entry.get("key_hash", ""), key_hash):
            return True
    return False


# ---------------------------------------------------------------------------
# MCP protocol helpers
# ---------------------------------------------------------------------------


def _mcp_error(code: int, message: str, req_id: Any = None) -> Any:
    """Return a JSON-RPC 2.0 error response."""
    return jsonify(
        {
            "jsonrpc": "2.0",
            "id": req_id,
            "error": {"code": code, "message": message},
        }
    )


def _mcp_result(result: Any, req_id: Any = None) -> Any:
    """Return a JSON-RPC 2.0 success response."""
    return jsonify({"jsonrpc": "2.0", "id": req_id, "result": result})


# ---------------------------------------------------------------------------
# MCP Tool definitions (schema + implementation)
# ---------------------------------------------------------------------------

#: Full schema for every tool exported by this MCP server.
MCP_TOOLS: list[dict] = [
    {
        "name": "list_services",
        "description": (
            "List all distinct service names that have sent telemetry to SOBS. "
            "Useful as a first step to discover what services are being observed."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {},
            "required": [],
        },
    },
    {
        "name": "query_otel_logs",
        "description": (
            "Query the otel_logs table.  Returns log records matching the given "
            "filters.  Useful for diagnosing application errors, warning events, "
            "and general operational log data."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Filter by ServiceName (exact match).",
                },
                "severity": {
                    "type": "string",
                    "description": "Filter by SeverityText, e.g. ERROR, WARN, INFO.",
                },
                "search": {
                    "type": "string",
                    "description": "Case-insensitive substring search applied to the Body field.",
                },
                "trace_id": {
                    "type": "string",
                    "description": "Filter by TraceId (exact match).",
                },
                "from_ts": {
                    "type": "string",
                    "description": (
                        "Start of the time window as an ISO-8601 timestamp "
                        "(e.g. 2024-01-15T10:00:00Z).  Defaults to 1 hour ago."
                    ),
                },
                "to_ts": {
                    "type": "string",
                    "description": ("End of the time window as an ISO-8601 timestamp.  " "Defaults to now."),
                },
                "limit": {
                    "type": "integer",
                    "description": "Maximum number of records to return (1–500, default 100).",
                    "minimum": 1,
                    "maximum": 500,
                },
            },
            "required": [],
        },
    },
    {
        "name": "query_otel_traces",
        "description": (
            "Query the otel_traces table for distributed trace spans. "
            "Useful for performance analysis, error tracing, and understanding "
            "service dependencies."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Filter by ServiceName (exact match).",
                },
                "span_name": {
                    "type": "string",
                    "description": "Filter by SpanName (exact match).",
                },
                "trace_id": {
                    "type": "string",
                    "description": "Filter by TraceId (exact match).",
                },
                "status_code": {
                    "type": "string",
                    "description": "Filter by StatusCode, e.g. STATUS_CODE_ERROR.",
                },
                "from_ts": {
                    "type": "string",
                    "description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
                },
                "to_ts": {
                    "type": "string",
                    "description": "End of the time window (ISO-8601).  Defaults to now.",
                },
                "limit": {
                    "type": "integer",
                    "description": "Maximum number of records to return (1–500, default 100).",
                    "minimum": 1,
                    "maximum": 500,
                },
            },
            "required": [],
        },
    },
    {
        "name": "query_metrics",
        "description": (
            "Query the v_otel_metrics_1m pre-aggregated 1-minute metrics view. "
            "Returns average values and sample counts for the requested metric(s). "
            "Useful for understanding service health and performance trends."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Filter by ServiceName (exact match).",
                },
                "metric_name": {
                    "type": "string",
                    "description": "Filter by MetricName (exact match).",
                },
                "metric_kind": {
                    "type": "string",
                    "description": "Filter by MetricKind: gauge, sum, or histogram.",
                },
                "from_ts": {
                    "type": "string",
                    "description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
                },
                "to_ts": {
                    "type": "string",
                    "description": "End of the time window (ISO-8601).  Defaults to now.",
                },
                "limit": {
                    "type": "integer",
                    "description": "Maximum number of records to return (1–1000, default 200).",
                    "minimum": 1,
                    "maximum": 1000,
                },
            },
            "required": [],
        },
    },
    {
        "name": "query_metrics_raw",
        "description": (
            "Query raw metric data points from otel_metrics_gauge, otel_metrics_sum, "
            "or otel_metrics_histogram tables. Useful when you need individual data "
            "points rather than pre-aggregated 1-minute rollups."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "metric_kind": {
                    "type": "string",
                    "description": "The table to query: gauge, sum, or histogram.  Required.",
                    "enum": ["gauge", "sum", "histogram"],
                },
                "service": {
                    "type": "string",
                    "description": "Filter by ServiceName (exact match).",
                },
                "metric_name": {
                    "type": "string",
                    "description": "Filter by MetricName (exact match).",
                },
                "from_ts": {
                    "type": "string",
                    "description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
                },
                "to_ts": {
                    "type": "string",
                    "description": "End of the time window (ISO-8601).  Defaults to now.",
                },
                "limit": {
                    "type": "integer",
                    "description": "Maximum number of records to return (1–500, default 100).",
                    "minimum": 1,
                    "maximum": 500,
                },
            },
            "required": ["metric_kind"],
        },
    },
    {
        "name": "get_metric_names",
        "description": (
            "Return a list of all distinct metric names currently stored in SOBS "
            "along with the last seen timestamp and the service that reported them. "
            "Useful for discovering which metrics are available to query."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Optional service name filter.",
                },
            },
            "required": [],
        },
    },
    {
        "name": "get_anomaly_rules",
        "description": (
            "Return the list of configured anomaly detection rules in SOBS. "
            "Useful for understanding which metrics are being monitored for "
            "anomalies and what the configured thresholds are."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {},
            "required": [],
        },
    },
    {
        "name": "get_recent_errors",
        "description": (
            "Return recent error-level log events and error-status trace spans. "
            "Useful for quickly surfacing recent failures across all services."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Filter by ServiceName (exact match).",
                },
                "from_ts": {
                    "type": "string",
                    "description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
                },
                "to_ts": {
                    "type": "string",
                    "description": "End of the time window (ISO-8601).  Defaults to now.",
                },
                "limit": {
                    "type": "integer",
                    "description": "Maximum number of records to return (1–200, default 50).",
                    "minimum": 1,
                    "maximum": 200,
                },
            },
            "required": [],
        },
    },
]


# ---------------------------------------------------------------------------
# Time window helpers
# ---------------------------------------------------------------------------
_DEFAULT_WINDOW_HOURS = 1


def _parse_ts(value: str | None) -> str:
    """Normalise an ISO-8601 timestamp string for use in ClickHouse queries."""
    if not value:
        return ""
    try:
        # Attempt to parse and re-serialise to a canonical form.
        if value.endswith("Z"):
            value = value[:-1] + "+00:00"
        dt = datetime.fromisoformat(value)
        return dt.astimezone(timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
    except (ValueError, TypeError):
        return ""


def _build_time_where(
    column: str,
    from_ts: str,
    to_ts: str,
    conditions: list[str],
    params: list[str],
) -> None:
    """Append time-range conditions (and params) to the provided lists."""
    if from_ts:
        conditions.append(f"{column} >= ?")
        params.append(from_ts)
    else:
        conditions.append(f"{column} >= now() - INTERVAL {_DEFAULT_WINDOW_HOURS} HOUR")
    if to_ts:
        conditions.append(f"{column} <= ?")
        params.append(to_ts)


def _clamp(value: int | None, lo: int, hi: int, default: int) -> int:
    if value is None:
        return default
    try:
        return max(lo, min(hi, int(value)))
    except (ValueError, TypeError):
        return default


def _normalize_map_value(raw: Any) -> dict[str, Any]:
    """Return a dict for map-like chDB values across runtime/test representations."""
    if raw is None:
        return {}
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        text = raw.strip()
        if not text:
            return {}
        try:
            parsed_json = json.loads(text)
            if isinstance(parsed_json, dict):
                return parsed_json
        except (json.JSONDecodeError, TypeError):
            pass
        try:
            parsed_literal = ast.literal_eval(text)
            if isinstance(parsed_literal, dict):
                return parsed_literal
        except (ValueError, SyntaxError):
            pass
        return {}
    try:
        return dict(raw)
    except (TypeError, ValueError):
        return {}


# ---------------------------------------------------------------------------
# Tool implementations
# ---------------------------------------------------------------------------


def _tool_list_services(db: Any, _args: dict) -> dict:
    rows = db.execute(
        "SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName != '' "
        "UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != '' "
        "UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_metrics_gauge WHERE ServiceName != '' "
        "ORDER BY ServiceName"
    ).fetchall()
    return {"services": [r[0] for r in rows]}


def _tool_query_otel_logs(db: Any, args: dict) -> dict:
    service = (args.get("service") or "").strip()
    severity = (args.get("severity") or "").strip().upper()
    search = (args.get("search") or "").strip()
    trace_id = (args.get("trace_id") or "").strip()
    from_ts = _parse_ts(args.get("from_ts"))
    to_ts = _parse_ts(args.get("to_ts"))
    limit = _clamp(args.get("limit"), 1, 500, 100)

    conditions: list[str] = []
    params: list[str] = []

    _build_time_where("Timestamp", from_ts, to_ts, conditions, params)

    if service:
        conditions.append("ServiceName = ?")
        params.append(service)
    if severity:
        conditions.append("SeverityText = ?")
        params.append(severity)
    if trace_id:
        conditions.append("TraceId = ?")
        params.append(trace_id)
    if search:
        conditions.append("Body ILIKE ?")
        params.append(f"%{search}%")

    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""
    sql = (
        f"SELECT toString(Timestamp) AS ts, ServiceName, SeverityText, "
        f"Body, TraceId, SpanId, LogAttributes "
        f"FROM otel_logs {where} "
        f"ORDER BY Timestamp DESC LIMIT {limit}"
    )
    rows = db.execute(sql, params if params else None).fetchall()
    result = []
    for row in rows:
        result.append(
            {
                "ts": row[0],
                "service": row[1],
                "severity": row[2],
                "body": row[3],
                "trace_id": row[4],
                "span_id": row[5],
                "attributes": _normalize_map_value(row[6]),
            }
        )
    return {"count": len(result), "rows": result}


def _tool_query_otel_traces(db: Any, args: dict) -> dict:
    service = (args.get("service") or "").strip()
    span_name = (args.get("span_name") or "").strip()
    trace_id = (args.get("trace_id") or "").strip()
    status_code = (args.get("status_code") or "").strip()
    from_ts = _parse_ts(args.get("from_ts"))
    to_ts = _parse_ts(args.get("to_ts"))
    limit = _clamp(args.get("limit"), 1, 500, 100)

    conditions: list[str] = []
    params: list[str] = []

    _build_time_where("Timestamp", from_ts, to_ts, conditions, params)

    if service:
        conditions.append("ServiceName = ?")
        params.append(service)
    if span_name:
        conditions.append("SpanName = ?")
        params.append(span_name)
    if trace_id:
        conditions.append("TraceId = ?")
        params.append(trace_id)
    if status_code:
        conditions.append("StatusCode = ?")
        params.append(status_code)

    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""
    sql = (
        f"SELECT toString(Timestamp) AS ts, ServiceName, TraceId, SpanId, "
        f"SpanName, SpanKind, StatusCode, StatusMessage, "
        f"toUInt64(Duration / 1000000) AS duration_ms "
        f"FROM otel_traces {where} "
        f"ORDER BY Timestamp DESC LIMIT {limit}"
    )
    rows = db.execute(sql, params if params else None).fetchall()
    result = []
    for row in rows:
        result.append(
            {
                "ts": row[0],
                "service": row[1],
                "trace_id": row[2],
                "span_id": row[3],
                "span_name": row[4],
                "span_kind": row[5],
                "status_code": row[6],
                "status_message": row[7],
                "duration_ms": row[8],
            }
        )
    return {"count": len(result), "rows": result}


def _tool_query_metrics(db: Any, args: dict) -> dict:
    service = (args.get("service") or "").strip()
    metric_name = (args.get("metric_name") or "").strip()
    metric_kind = (args.get("metric_kind") or "").strip().lower()
    from_ts = _parse_ts(args.get("from_ts"))
    to_ts = _parse_ts(args.get("to_ts"))
    limit = _clamp(args.get("limit"), 1, 1000, 200)

    conditions: list[str] = []
    params: list[str] = []

    _build_time_where("MinuteBucket", from_ts, to_ts, conditions, params)

    if service:
        conditions.append("ServiceName = ?")
        params.append(service)
    if metric_name:
        conditions.append("MetricName = ?")
        params.append(metric_name)
    if metric_kind in {"gauge", "sum", "histogram"}:
        conditions.append("MetricKind = ?")
        params.append(metric_kind)

    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""
    sql = (
        f"SELECT toString(MinuteBucket) AS ts, ServiceName, MetricName, "
        f"MetricKind, Value, SampleCount "
        f"FROM v_otel_metrics_1m {where} "
        f"ORDER BY MinuteBucket DESC LIMIT {limit}"
    )
    rows = db.execute(sql, params if params else None).fetchall()
    result = []
    for row in rows:
        result.append(
            {
                "ts": row[0],
                "service": row[1],
                "metric_name": row[2],
                "metric_kind": row[3],
                "value": row[4],
                "sample_count": row[5],
            }
        )
    return {"count": len(result), "rows": result}


_RAW_METRIC_TABLES = {
    "gauge": "otel_metrics_gauge",
    "sum": "otel_metrics_sum",
    "histogram": "otel_metrics_histogram",
}


def _tool_query_metrics_raw(db: Any, args: dict) -> dict:
    metric_kind = (args.get("metric_kind") or "").strip().lower()
    if metric_kind not in _RAW_METRIC_TABLES:
        return {"error": "metric_kind must be one of: gauge, sum, histogram"}

    table = _RAW_METRIC_TABLES[metric_kind]
    service = (args.get("service") or "").strip()
    metric_name = (args.get("metric_name") or "").strip()
    from_ts = _parse_ts(args.get("from_ts"))
    to_ts = _parse_ts(args.get("to_ts"))
    limit = _clamp(args.get("limit"), 1, 500, 100)

    conditions: list[str] = []
    params: list[str] = []

    _build_time_where("TimeUnix", from_ts, to_ts, conditions, params)

    if service:
        conditions.append("ServiceName = ?")
        params.append(service)
    if metric_name:
        conditions.append("MetricName = ?")
        params.append(metric_name)

    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""

    if metric_kind == "histogram":
        sql = (
            f"SELECT toString(TimeUnix) AS ts, ServiceName, MetricName, "
            f"MetricUnit, Attributes, Count, Sum "
            f"FROM {table} {where} "
            f"ORDER BY TimeUnix DESC LIMIT {limit}"
        )
        rows = db.execute(sql, params if params else None).fetchall()
        result = []
        for row in rows:
            result.append(
                {
                    "ts": row[0],
                    "service": row[1],
                    "metric_name": row[2],
                    "metric_unit": row[3],
                    "attributes": _normalize_map_value(row[4]),
                    "count": row[5],
                    "sum": row[6],
                }
            )
    else:
        sql = (
            f"SELECT toString(TimeUnix) AS ts, ServiceName, MetricName, "
            f"MetricUnit, Attributes, Value "
            f"FROM {table} {where} "
            f"ORDER BY TimeUnix DESC LIMIT {limit}"
        )
        rows = db.execute(sql, params if params else None).fetchall()
        result = []
        for row in rows:
            result.append(
                {
                    "ts": row[0],
                    "service": row[1],
                    "metric_name": row[2],
                    "metric_unit": row[3],
                    "attributes": _normalize_map_value(row[4]),
                    "value": row[5],
                }
            )
    return {"count": len(result), "rows": result}


def _tool_get_metric_names(db: Any, args: dict) -> dict:
    service = (args.get("service") or "").strip()
    conditions: list[str] = []
    params: list[str] = []
    if service:
        conditions.append("ServiceName = ?")
        params.append(service)
    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""
    sql = (
        "SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen "
        f"FROM otel_metrics_gauge {where} "
        "GROUP BY MetricName, ServiceName "
        "UNION ALL "
        "SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen "
        f"FROM otel_metrics_sum {where} "
        "GROUP BY MetricName, ServiceName "
        "UNION ALL "
        "SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen "
        f"FROM otel_metrics_histogram {where} "
        "GROUP BY MetricName, ServiceName "
        "ORDER BY MetricName, ServiceName"
    )
    # Each UNION branch uses the same WHERE clause with one param per branch.
    all_params = params * 3 if params else None
    rows = db.execute(sql, all_params).fetchall()
    result = []
    for row in rows:
        result.append({"metric_name": row[0], "service": row[1], "last_seen": row[2]})
    return {"count": len(result), "metrics": result}


def _tool_get_anomaly_rules(db: Any, _args: dict) -> dict:
    sql = (
        "SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, "
        "Comparator, WarningThreshold, CriticalThreshold "
        "FROM sobs_anomaly_rules FINAL "
        "WHERE IsDeleted = 0 "
        "ORDER BY SignalSource, SignalName"
    )
    rows = db.execute(sql).fetchall()
    result = []
    for row in rows:
        result.append(
            {
                "id": row[0],
                "name": row[1],
                "rule_type": row[2],
                "signal_source": row[3],
                "signal_name": row[4],
                "service": row[5],
                "comparator": row[6],
                "warning_threshold": row[7],
                "critical_threshold": row[8],
            }
        )
    return {"count": len(result), "rules": result}


def _tool_get_recent_errors(db: Any, args: dict) -> dict:
    service = (args.get("service") or "").strip()
    from_ts = _parse_ts(args.get("from_ts"))
    to_ts = _parse_ts(args.get("to_ts"))
    limit = _clamp(args.get("limit"), 1, 200, 50)

    log_conditions: list[str] = []
    log_params: list[str] = []
    _build_time_where("Timestamp", from_ts, to_ts, log_conditions, log_params)
    log_conditions.append("SeverityText IN ('ERROR', 'FATAL', 'CRITICAL')")
    if service:
        log_conditions.append("ServiceName = ?")
        log_params.append(service)
    log_where = "WHERE " + " AND ".join(log_conditions)

    trace_conditions: list[str] = []
    trace_params: list[str] = []
    _build_time_where("Timestamp", from_ts, to_ts, trace_conditions, trace_params)
    trace_conditions.append("StatusCode = 'STATUS_CODE_ERROR'")
    if service:
        trace_conditions.append("ServiceName = ?")
        trace_params.append(service)
    trace_where = "WHERE " + " AND ".join(trace_conditions)

    half = limit // 2 or 1
    log_sql = (
        f"SELECT toString(Timestamp) AS ts, ServiceName, 'log' AS source, "
        f"SeverityText AS level_or_status, Body AS message, TraceId "
        f"FROM otel_logs {log_where} "
        f"ORDER BY Timestamp DESC LIMIT {half}"
    )
    trace_sql = (
        f"SELECT toString(Timestamp) AS ts, ServiceName, 'trace' AS source, "
        f"StatusCode AS level_or_status, SpanName AS message, TraceId "
        f"FROM otel_traces {trace_where} "
        f"ORDER BY Timestamp DESC LIMIT {half}"
    )

    log_rows = db.execute(log_sql, log_params if log_params else None).fetchall()
    trace_rows = db.execute(trace_sql, trace_params if trace_params else None).fetchall()

    def _row_to_dict(row: Any) -> dict:
        return {
            "ts": row[0],
            "service": row[1],
            "source": row[2],
            "level_or_status": row[3],
            "message": row[4],
            "trace_id": row[5],
        }

    result = [_row_to_dict(r) for r in log_rows] + [_row_to_dict(r) for r in trace_rows]
    result.sort(key=lambda r: r["ts"], reverse=True)
    return {"count": len(result), "errors": result}


# ---------------------------------------------------------------------------
# Tool dispatch table
# ---------------------------------------------------------------------------
_TOOL_HANDLERS: dict[str, Any] = {
    "list_services": _tool_list_services,
    "query_otel_logs": _tool_query_otel_logs,
    "query_otel_traces": _tool_query_otel_traces,
    "query_metrics": _tool_query_metrics,
    "query_metrics_raw": _tool_query_metrics_raw,
    "get_metric_names": _tool_get_metric_names,
    "get_anomaly_rules": _tool_get_anomaly_rules,
    "get_recent_errors": _tool_get_recent_errors,
}


# ---------------------------------------------------------------------------
# MCP HTTP endpoints
# ---------------------------------------------------------------------------


@mcp_bp.route("/mcp/tools", methods=["GET"])
async def mcp_list_tools():
    """Return the list of MCP tools this server exposes (no auth required)."""
    return jsonify(
        {
            "jsonrpc": "2.0",
            "id": None,
            "result": {
                "tools": MCP_TOOLS,
            },
        }
    )


@mcp_bp.route("/mcp", methods=["GET"])
async def mcp_endpoint_get():
    """
    GET /mcp – MCP transport compatibility probe.

    Per the MCP Streamable HTTP transport specification, clients (including
    VS Code) may send a ``GET`` request to the endpoint before establishing
    a session.  Returning ``405`` here breaks those clients even when the
    ``POST`` endpoint works correctly.

    This handler returns a lightweight ``200 OK`` response with the server
    capability descriptor so that clients can discover the server without
    starting a full JSON-RPC session.  No authentication is required.
    """
    from app import get_db  # noqa: PLC0415

    db = get_db()
    if not _mcp_enabled(db):
        return (
            jsonify(
                {
                    "jsonrpc": "2.0",
                    "id": None,
                    "error": {"code": -32001, "message": "MCP server is disabled."},
                }
            ),
            503,
        )

    return jsonify(
        {
            "protocolVersion": _MCP_PROTOCOL_VERSION,
            "capabilities": _MCP_CAPABILITIES,
            "serverInfo": _MCP_SERVER_INFO,
        }
    )


@mcp_bp.route("/mcp", methods=["POST"])
async def mcp_endpoint():
    """
    Main MCP JSON-RPC 2.0 endpoint.

    Accepts ``initialize``, ``tools/list``, and ``tools/call`` method calls.

    Authentication
    --------------
    Set ``X-MCP-API-Key: <key>`` in the request header.

    Rate limiting
    -------------
    Each IP is limited to 60 requests per minute.
    """
    from app import _mask_value_for_output, get_db  # noqa: PLC0415

    # Rate limiting.
    client_ip = request.headers.get("X-Forwarded-For", "").split(",")[0].strip() or request.remote_addr or "unknown"
    if not _check_rate_limit(client_ip):
        return (
            jsonify(
                {
                    "jsonrpc": "2.0",
                    "id": None,
                    "error": {"code": -32000, "message": "Rate limit exceeded. Try again later."},
                }
            ),
            429,
        )

    db = get_db()

    # Require MCP to be enabled.
    if not _mcp_enabled(db):
        return (
            jsonify(
                {
                    "jsonrpc": "2.0",
                    "id": None,
                    "error": {"code": -32001, "message": "MCP server is disabled."},
                }
            ),
            503,
        )

    # Parse the JSON-RPC body.
    try:
        body = await request.get_json(force=True, silent=False) or {}
    except Exception:
        return (
            jsonify({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}}),
            400,
        )

    req_id = body.get("id")
    method = body.get("method", "")

    # The ``initialize`` method is used by MCP clients to negotiate capabilities.
    # It does NOT require an API key so that clients can discover the server.
    if method == "initialize":
        return jsonify(
            {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "protocolVersion": _MCP_PROTOCOL_VERSION,
                    "capabilities": _MCP_CAPABILITIES,
                    "serverInfo": _MCP_SERVER_INFO,
                },
            }
        )

    # All other methods require authentication.
    if not _authenticate_mcp_request(db):
        return (
            jsonify(
                {
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "error": {"code": -32002, "message": "Unauthorized: missing or invalid X-MCP-API-Key header."},
                }
            ),
            401,
        )

    if method == "tools/list":
        return jsonify(
            {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {"tools": MCP_TOOLS},
            }
        )

    if method == "tools/call":
        params = body.get("params", {})
        tool_name = params.get("name", "")
        tool_args = params.get("arguments", {})
        if not isinstance(tool_args, dict):
            tool_args = {}

        handler = _TOOL_HANDLERS.get(tool_name)
        if handler is None:
            return (
                jsonify(
                    {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "error": {
                            "code": -32601,
                            "message": f"Unknown tool: '{tool_name}'. " f"Available: {sorted(_TOOL_HANDLERS)}",
                        },
                    }
                ),
                404,
            )

        try:
            tool_result = handler(db, tool_args)
            # Apply the same output masking used across SOBS UI routes so that
            # PII / secrets in log bodies, span names, and attributes are
            # redacted before they leave the server.
            tool_result = _mask_value_for_output(tool_result, db)
        except Exception as exc:
            log.exception("MCP tool '%s' raised an error", tool_name)
            return (
                jsonify(
                    {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "error": {"code": -32603, "message": f"Internal error: {type(exc).__name__}"},
                    }
                ),
                500,
            )

        return jsonify(
            {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "content": [{"type": "text", "text": json.dumps(tool_result, ensure_ascii=False, default=str)}],
                    "isError": False,
                },
            }
        )

    # Unknown method.
    return (
        jsonify(
            {
                "jsonrpc": "2.0",
                "id": req_id,
                "error": {"code": -32601, "message": f"Method not found: '{method}'"},
            }
        ),
        404,
    )


# ---------------------------------------------------------------------------
# Settings API endpoints (key management)
# ---------------------------------------------------------------------------


@mcp_bp.route("/api/mcp/keys", methods=["GET"])
async def mcp_api_list_keys():
    """List MCP API key descriptors (hashes are not exposed; only metadata)."""
    from app import get_db, require_basic_auth  # noqa: PLC0415

    @require_basic_auth
    async def _inner():
        db = get_db()
        keys = _load_mcp_api_keys(db)
        # Return metadata only – never expose raw keys or hashes.
        safe = [
            {
                "id": k.get("id", ""),
                "label": k.get("label", ""),
                "created_at": k.get("created_at", ""),
                "expires_at": k.get("expires_at"),
            }
            for k in keys
        ]
        return jsonify({"ok": True, "keys": safe})

    return await _inner()


@mcp_bp.route("/api/mcp/keys", methods=["POST"])
async def mcp_api_create_key():
    """Generate a new MCP API key."""
    from app import get_db, require_basic_auth  # noqa: PLC0415

    @require_basic_auth
    async def _inner():
        db = get_db()
        keys = _load_mcp_api_keys(db)
        if len(keys) >= _MCP_API_KEY_MAX:
            return jsonify({"ok": False, "error": f"Maximum of {_MCP_API_KEY_MAX} keys reached."}), 400

        body = await request.get_json(silent=True) or {}
        label = str(body.get("label", "")).strip()[:128] or "API Key"
        expires_at = body.get("expires_at")  # Optional ISO 8601 expiry date

        raw_key = "smcp_" + secrets.token_urlsafe(32)
        key_id = secrets.token_hex(8)
        keys.append(
            {
                "id": key_id,
                "label": label,
                "key_hash": _hash_key(raw_key),
                "created_at": datetime.now(timezone.utc).isoformat(),
                "expires_at": expires_at,
            }
        )
        _save_mcp_api_keys(db, keys)
        return jsonify({"ok": True, "id": key_id, "key": raw_key, "label": label, "expires_at": expires_at})

    return await _inner()


@mcp_bp.route("/api/mcp/keys/<key_id>", methods=["DELETE"])
async def mcp_api_delete_key(key_id: str):
    """Revoke (delete) an MCP API key by its ID."""
    from app import get_db, require_basic_auth  # noqa: PLC0415

    @require_basic_auth
    async def _inner():
        db = get_db()
        keys = _load_mcp_api_keys(db)
        new_keys = [k for k in keys if k.get("id") != key_id]
        if len(new_keys) == len(keys):
            return jsonify({"ok": False, "error": "Key not found."}), 404
        _save_mcp_api_keys(db, new_keys)
        return jsonify({"ok": True})

    return await _inner()


@mcp_bp.route("/api/mcp/enabled", methods=["POST"])
async def mcp_api_set_enabled():
    """Enable or disable the MCP server."""
    from app import get_db, require_basic_auth  # noqa: PLC0415

    @require_basic_auth
    async def _inner():
        db = get_db()
        body = await request.get_json(silent=True) or {}
        enabled = bool(body.get("enabled", True))
        from app import _set_app_setting  # noqa: PLC0415

        _set_app_setting(db, _MCP_ENABLED_SETTING, "1" if enabled else "0")
        return jsonify({"ok": True, "enabled": enabled})

    return await _inner()


# ---------------------------------------------------------------------------
# Settings UI page
# ---------------------------------------------------------------------------


@mcp_bp.route("/settings/mcp", methods=["GET"])
async def mcp_settings_page():
    """Render the MCP API key management settings page."""
    from app import get_db, require_basic_auth  # noqa: PLC0415

    @require_basic_auth
    async def _inner():
        db = get_db()
        keys = _load_mcp_api_keys(db)
        enabled = _mcp_enabled(db)
        safe_keys = [
            {
                "id": k.get("id", ""),
                "label": k.get("label", ""),
                "created_at": k.get("created_at", ""),
                "expires_at": k.get("expires_at"),
            }
            for k in keys
        ]
        now_iso = datetime.now(timezone.utc).isoformat()
        return await render_template("settings_mcp.html", mcp_keys=safe_keys, mcp_enabled=enabled, now_iso=now_iso)

    return await _inner()
