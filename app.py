"""
SOBS - Simple Observe
A lightweight, single-user telemetry container supporting OpenTelemetry,
RUM, Logs, Errors, Traces, and AI transparency.
"""

import ast
import asyncio
import base64
import copy
import hashlib
import inspect
import json
import logging
import os
import queue
import re
import secrets
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zlib
from collections import Counter
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from functools import wraps
from typing import Callable, cast

import chdb.dbapi as chdb_driver
from google.protobuf.json_format import ParseDict
from hypercorn.asyncio import serve as hypercorn_serve
from hypercorn.config import Config as HypercornConfig
from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import ExportLogsServiceRequest
from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import ExportMetricsServiceRequest
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from quart import (
    Quart,
    Response,
    flash,
    jsonify,
    redirect,
    render_template,
    request,
    send_from_directory,
    url_for,
)

# ---------------------------------------------------------------------------
# App setup
# ---------------------------------------------------------------------------
app = Quart(__name__)


def _env_flag(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None:
        return default
    return str(raw).strip().lower() in {"1", "true", "yes", "on"}


def _normalize_base_path(value: str) -> str:
    """Normalize base path values to either '' or '/segment[/subsegment]'."""
    if not value:
        return ""
    normalized = re.sub(r"/+", "/", str(value).strip())
    if not normalized or normalized == "/":
        return ""
    if not normalized.startswith("/"):
        normalized = "/" + normalized
    normalized = normalized.rstrip("/")
    return normalized if normalized != "/" else ""


def _merge_script_name(script_name: str, base_path: str) -> str:
    """Append base path to SCRIPT_NAME once."""
    if not base_path:
        return script_name or ""
    current = script_name or ""
    if current.endswith(base_path):
        return current
    if not current:
        return base_path
    return current.rstrip("/") + base_path


BASE_PATH = _normalize_base_path(os.environ.get("SOBS_BASE_PATH", ""))
app.config["APPLICATION_ROOT"] = BASE_PATH or "/"
app.config["SECRET_KEY"] = os.environ.get("SOBS_SECRET_KEY", "sobs-dev-secret-key")
app.config["SESSION_COOKIE_NAME"] = os.environ.get("SOBS_SESSION_COOKIE_NAME", "sobs_session")
app.config["ENABLE_FIRST_RUN_TOUR"] = _env_flag("SOBS_ENABLE_FIRST_RUN_TOUR", True)


class BasePathMiddleware:
    """ASGI middleware for deployment behind a path prefix and proxy prefix headers."""

    def __init__(self, wrapped_app, configured_base_path: str):
        self.wrapped_app = wrapped_app
        self.configured_base_path = configured_base_path

    @staticmethod
    def _merge_root_path(root_path: str, base_path: str) -> str:
        if not base_path:
            return root_path or ""
        current = root_path or ""
        if current.endswith(base_path):
            return current
        if not current:
            return base_path
        return current.rstrip("/") + base_path

    @staticmethod
    def _header_value(scope, header_name: str) -> str:
        needle = header_name.lower().encode("latin-1")
        for key, value in scope.get("headers", []):
            if key.lower() == needle:
                return value.decode("latin-1")
        return ""

    async def __call__(self, scope, receive, send):
        if scope.get("type") not in ("http", "websocket"):
            return await self.wrapped_app(scope, receive, send)

        scope = dict(scope)
        forwarded = _normalize_base_path(self._header_value(scope, "x-forwarded-prefix"))
        effective_base = forwarded or BASE_PATH  # read module-level var so monkeypatch works in tests
        if effective_base:
            path_info = scope.get("path", "") or "/"
            root_path = scope.get("root_path", "")

            if path_info.startswith(effective_base + "/") or path_info == effective_base:
                # Prefix is present in PATH_INFO.
                # Set root_path and leave scope["path"] intact — Quart's ASGI handler
                # strips root_path from scope["path"] internally before routing.
                scope["root_path"] = self._merge_root_path(root_path, effective_base)
            else:
                # Proxy already stripped the prefix.  Re-prepend it so Quart can
                # strip correctly via root_path (and url_for generates prefixed links).
                scope["root_path"] = self._merge_root_path(root_path, effective_base)
                scope["path"] = effective_base + (path_info if path_info.startswith("/") else "/" + path_info)

        return await self.wrapped_app(scope, receive, send)


app.asgi_app = BasePathMiddleware(app.asgi_app, BASE_PATH)  # type: ignore[method-assign]

DATA_DIR = os.environ.get("SOBS_DATA_DIR", os.path.join(os.path.dirname(__file__), "data"))
DB_PATH = os.path.join(DATA_DIR, "sobs.chdb")
API_KEY = os.environ.get("SOBS_API_KEY", "")  # empty = no auth required
BASIC_AUTH_USERNAME = os.environ.get("SOBS_BASIC_AUTH_USERNAME", "")  # empty = no basic auth
BASIC_AUTH_PASSWORD = os.environ.get("SOBS_BASIC_AUTH_PASSWORD", "")
EXTERNAL_AUTH_URL = os.environ.get("SOBS_EXTERNAL_AUTH_URL", "")  # empty = disabled
CHDB_CONFIG_FILE_ENV = "SOBS_CLICKHOUSE_CONFIG_FILE"
CHDB_EXPECT_DISK_ENV = "SOBS_CHDB_EXPECT_DISK"
CHDB_EXPECT_POLICY_ENV = "SOBS_CHDB_EXPECT_STORAGE_POLICY"

os.makedirs(DATA_DIR, exist_ok=True)

logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
log = logging.getLogger("sobs")

# ---------------------------------------------------------------------------
# Database helpers
# ---------------------------------------------------------------------------
SCHEMA = """
CREATE TABLE IF NOT EXISTS otel_logs (
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimestampTime DateTime DEFAULT toDateTime(Timestamp) CODEC(Delta(4), ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    TraceFlags UInt8 CODEC(T64, ZSTD(1)),
    SeverityText LowCardinality(String) CODEC(ZSTD(1)),
    SeverityNumber UInt8 CODEC(T64, ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Body String CODEC(ZSTD(1)),
    ResourceSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion LowCardinality(String) CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    LogAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    EventName String CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(TimestampTime)
ORDER BY (ServiceName, TimestampTime, Timestamp)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS otel_traces (
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    ParentSpanId String CODEC(ZSTD(1)),
    TraceState String CODEC(ZSTD(1)),
    SpanName LowCardinality(String) CODEC(ZSTD(1)),
    SpanKind LowCardinality(String) CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    SpanAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    Duration UInt64 CODEC(T64, ZSTD(1)),
    StatusCode LowCardinality(String) CODEC(ZSTD(1)),
    StatusMessage String CODEC(ZSTD(1)),
    Events Nested (
        Timestamp DateTime64(9),
        Name LowCardinality(String),
        Attributes Map(LowCardinality(String), String)
    ) CODEC(ZSTD(1)),
    Links Nested (
        TraceId String,
        SpanId String,
        TraceState String,
        Attributes Map(LowCardinality(String), String)
    ) CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS hyperdx_sessions (
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimestampTime DateTime DEFAULT toDateTime(Timestamp) CODEC(Delta(4), ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    TraceFlags UInt8 CODEC(T64, ZSTD(1)),
    SeverityText LowCardinality(String) CODEC(ZSTD(1)),
    SeverityNumber UInt8 CODEC(T64, ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Body String CODEC(ZSTD(1)),
    ResourceSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion LowCardinality(String) CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    LogAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    EventName String CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(TimestampTime)
ORDER BY (ServiceName, TimestampTime, Timestamp)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS sobs_error_resolutions (
    ErrorId String CODEC(ZSTD(1)),
    ResolvedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
) ENGINE = MergeTree()
ORDER BY (ErrorId, ResolvedAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS sobs_dashboards (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    Description String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_chart_configs (
    Id String CODEC(ZSTD(1)),
    DashboardId String CODEC(ZSTD(1)),
    Title String CODEC(ZSTD(1)),
    ChartType LowCardinality(String) CODEC(ZSTD(1)),
    Query String CODEC(ZSTD(1)),
    OptionsJson String CODEC(ZSTD(1)),
    Position UInt16 DEFAULT 0 CODEC(T64, ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (DashboardId, Id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_anomaly_rules (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    RuleType LowCardinality(String) DEFAULT 'threshold' CODEC(ZSTD(1)),
    SignalSource LowCardinality(String) CODEC(ZSTD(1)),
    SignalName LowCardinality(String) CODEC(ZSTD(1)),
    ServiceName String CODEC(ZSTD(1)),
    AttrFingerprint String CODEC(ZSTD(1)),
    Comparator LowCardinality(String) CODEC(ZSTD(1)),
    WarningThreshold Float64 CODEC(ZSTD(1)),
    CriticalThreshold Float64 CODEC(ZSTD(1)),
    SecondarySignalSource LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    SecondarySignalName LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    SecondaryComparator LowCardinality(String) DEFAULT 'gt' CODEC(ZSTD(1)),
    SecondaryWarningThreshold Float64 DEFAULT 0 CODEC(ZSTD(1)),
    SecondaryCriticalThreshold Float64 DEFAULT 0 CODEC(ZSTD(1)),
    MinSampleCount UInt32 DEFAULT 1 CODEC(T64, ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (SignalSource, SignalName, ServiceName, AttrFingerprint, Id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS otel_metrics_gauge (
    TimeUnix DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimeUnixMs DateTime DEFAULT toDateTime(TimeUnix) CODEC(Delta(4), ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName LowCardinality(String) CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit LowCardinality(String) CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    Value Float64 CODEC(ZSTD(1)),
    Flags UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    AttrFingerprint String CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnixMs)
ORDER BY (ServiceName, MetricName, AttrFingerprint, TimeUnixMs, TimeUnix)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS otel_metrics_sum (
    TimeUnix DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimeUnixMs DateTime DEFAULT toDateTime(TimeUnix) CODEC(Delta(4), ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName LowCardinality(String) CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit LowCardinality(String) CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    Value Float64 CODEC(ZSTD(1)),
    Flags UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    IsMonotonic UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    AggregationTemporality Int32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    AttrFingerprint String CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnixMs)
ORDER BY (ServiceName, MetricName, AttrFingerprint, TimeUnixMs, TimeUnix)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS otel_metrics_histogram (
    TimeUnix DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TimeUnixMs DateTime DEFAULT toDateTime(TimeUnix) CODEC(Delta(4), ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName LowCardinality(String) CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit LowCardinality(String) CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    Count UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Sum Float64 CODEC(ZSTD(1)),
    BucketCounts Array(UInt64) CODEC(ZSTD(1)),
    ExplicitBounds Array(Float64) CODEC(ZSTD(1)),
    Flags UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    AggregationTemporality Int32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    AttrFingerprint String CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnixMs)
ORDER BY (ServiceName, MetricName, AttrFingerprint, TimeUnixMs, TimeUnix)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE VIEW IF NOT EXISTS v_otel_metrics_1m AS
SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'gauge' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avg(Value) AS Value,
    count() AS SampleCount
FROM otel_metrics_gauge
GROUP BY ServiceName, MetricName, AttrFingerprint, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'sum' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avg(Value) AS Value,
    count() AS SampleCount
FROM otel_metrics_sum
GROUP BY ServiceName, MetricName, AttrFingerprint, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'histogram' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avg(if(Count > 0, Sum / Count, 0)) AS Value,
    sum(Count) AS SampleCount
FROM otel_metrics_histogram
GROUP BY ServiceName, MetricName, AttrFingerprint, MinuteBucket;

CREATE VIEW IF NOT EXISTS v_otel_metrics_anomaly AS
SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    MetricKind,
    MinuteBucket AS time,
    Value AS value,
    SampleCount,
    round(avg(Value) OVER w, 6) AS baseline_mean,
    round(
        sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ),
        6
    ) AS baseline_stddev,
    round(
        avg(Value) OVER w - 2.0 * sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ),
        6
    ) AS baseline_lower,
    round(
        avg(Value) OVER w + 2.0 * sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ),
        6
    ) AS baseline_upper,
    round(
        if(
            sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w
                        * avg(Value) OVER w)
                )
            ) > 0,
            abs(Value - avg(Value) OVER w) / sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w
                        * avg(Value) OVER w)
                )
            ),
            0
        ),
        4
    ) AS anomaly_score,
    multiIf(
        sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value)
                    OVER w)
            )
        ) > 0
            AND abs(Value - avg(Value) OVER w) > 3.0 * sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w
                        * avg(Value) OVER w)
                )
            ),
        'outlier',
        sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value)
                    OVER w)
            )
        ) > 0
            AND abs(Value - avg(Value) OVER w) > 2.0 * sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w
                        * avg(Value) OVER w)
                )
            ),
        'warning',
        'normal'
    ) AS anomaly_state
FROM v_otel_metrics_1m
WINDOW w AS (
    PARTITION BY ServiceName, MetricName, AttrFingerprint
    ORDER BY MinuteBucket
    ROWS BETWEEN 59 PRECEDING AND CURRENT ROW
);

CREATE VIEW IF NOT EXISTS v_derived_signals_1m AS
SELECT
    ServiceName,
    'logs' AS SignalSource,
    'log_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'log_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(count()) AS Value,
    count() AS SampleCount
FROM otel_logs
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'logs' AS SignalSource,
    'error_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'error_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(countIf(SeverityText IN ('ERROR', 'FATAL', 'CRITICAL'))) AS Value,
    count() AS SampleCount
FROM otel_logs
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'logs' AS SignalSource,
    'error_ratio' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'error_ratio')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    if(count() > 0, toFloat64(countIf(SeverityText IN ('ERROR', 'FATAL', 'CRITICAL'))) / count(), 0.0) AS Value,
    count() AS SampleCount
FROM otel_logs
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'traces' AS SignalSource,
    'trace_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'trace_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(count()) AS Value,
    count() AS SampleCount
FROM otel_traces
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'traces' AS SignalSource,
    'trace_error_ratio' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'trace_error_ratio')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    if(count() > 0, toFloat64(countIf(StatusCode = 'STATUS_CODE_ERROR')) / count(), 0.0) AS Value,
    count() AS SampleCount
FROM otel_traces
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'traces' AS SignalSource,
    'latency_p95_ms' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'latency_p95_ms')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantile(0.95)(Duration)) / 1000000.0 AS Value,
    count() AS SampleCount
FROM otel_traces
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'errors' AS SignalSource,
    'exception_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'exception_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(count()) AS Value,
    count() AS SampleCount
FROM otel_logs
WHERE EventName = 'exception'
GROUP BY ServiceName, MinuteBucket;

CREATE VIEW IF NOT EXISTS v_derived_signals_anomaly AS
SELECT
    ServiceName,
    SignalSource,
    SignalName,
    AttrFingerprint,
    MinuteBucket AS time,
    Value AS value,
    SampleCount,
    round(avg(Value) OVER w, 6) AS baseline_mean,
    round(
        sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ),
        6
    ) AS baseline_stddev,
    round(
        avg(Value) OVER w - 2.0 * sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ),
        6
    ) AS baseline_lower,
    round(
        avg(Value) OVER w + 2.0 * sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ),
        6
    ) AS baseline_upper,
    round(
        if(
            sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
                )
            ) > 0,
            abs(Value - avg(Value) OVER w) / sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
                )
            ),
            0
        ),
        4
    ) AS anomaly_score,
    multiIf(
        sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ) > 0
            AND abs(Value - avg(Value) OVER w) > 3.0 * sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
                )
            ),
        'outlier',
        sqrt(
            greatest(
                0.0,
                avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
            )
        ) > 0
            AND abs(Value - avg(Value) OVER w) > 2.0 * sqrt(
                greatest(
                    0.0,
                    avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)
                )
            ),
        'warning',
        'normal'
    ) AS anomaly_state
FROM v_derived_signals_1m
WINDOW w AS (
    PARTITION BY ServiceName, SignalSource, SignalName, AttrFingerprint
    ORDER BY MinuteBucket
    ROWS BETWEEN 59 PRECEDING AND CURRENT ROW
);

CREATE TABLE IF NOT EXISTS sobs_tag_rules (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    RecordTypes String CODEC(ZSTD(1)),
    MatchField LowCardinality(String) CODEC(ZSTD(1)),
    MatchOperator LowCardinality(String) CODEC(ZSTD(1)),
    MatchValue String CODEC(ZSTD(1)),
    MatchAttrKey String CODEC(ZSTD(1)),
    TagKey String CODEC(ZSTD(1)),
    TagValue String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_record_tags (
    RecordType LowCardinality(String) CODEC(ZSTD(1)),
    RecordId String CODEC(ZSTD(1)),
    TagKey LowCardinality(String) CODEC(ZSTD(1)),
    TagValue String CODEC(ZSTD(1)),
    IsAuto UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, RecordId, TagKey)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_log_attr_keys (
    RecordType LowCardinality(String) CODEC(ZSTD(1)),
    AttrKey LowCardinality(String) CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, AttrKey)
SETTINGS index_granularity = 8192;
"""


def _build_chdb_connect_target(path: str) -> str:
    """Build chDB connect target, optionally adding startup args via query params."""
    config_file = os.environ.get(CHDB_CONFIG_FILE_ENV, "").strip()
    if not config_file:
        return path
    if not os.path.isabs(config_file):
        raise RuntimeError(f"{CHDB_CONFIG_FILE_ENV} must be an absolute path to a mounted ClickHouse config.xml")
    encoded = urllib.parse.quote(config_file, safe="/")
    return f"file:{path}?config-file={encoded}"


def _validate_chdb_startup_configuration(conn: "ChDbConnection") -> None:
    expected_disk = os.environ.get(CHDB_EXPECT_DISK_ENV, "").strip()
    expected_policy = os.environ.get(CHDB_EXPECT_POLICY_ENV, "").strip()
    if not expected_disk and not expected_policy:
        return

    disks = conn.execute("SELECT name FROM system.disks").fetchall()
    policies = conn.execute("SELECT DISTINCT policy_name FROM system.storage_policies").fetchall()

    disk_names = {str(row[0]) for row in disks}
    policy_names = {str(row[0]) for row in policies}
    missing = []
    if expected_disk and expected_disk not in disk_names:
        missing.append(f"disk '{expected_disk}'")
    if expected_policy and expected_policy not in policy_names:
        missing.append(f"storage policy '{expected_policy}'")
    if missing:
        raise RuntimeError(
            "chDB started but expected storage configuration was not applied; "
            f"missing {', '.join(missing)}. "
            "This usually means the config-file startup argument was ignored or invalid. "
            f"Current disks={sorted(disk_names)} policies={sorted(policy_names)}"
        )


class RowCompat(dict):
    """Row wrapper supporting both key and integer-index access."""

    def __init__(self, columns, values):
        super().__init__(zip(columns, values))
        self._values = tuple(values)

    def __getitem__(self, key):
        if isinstance(key, int):
            return self._values[key]
        return super().__getitem__(key)


class ChDbResult:
    """Pre-materialised query result; data fetched while the lock is held."""

    def __init__(self, columns, rows):
        self._columns = columns
        self._rows = rows
        self._idx = 0

    def fetchone(self):
        if self._idx >= len(self._rows):
            return None
        row = RowCompat(self._columns, self._rows[self._idx])
        self._idx += 1
        return row

    def fetchall(self):
        return [RowCompat(self._columns, r) for r in self._rows[self._idx :]]


class ChDbConnection:
    """Thread-safe global chDB connection wrapper."""

    def __init__(self, path: str):
        connect_target = _build_chdb_connect_target(path)
        log.info("chDB connect target: %s", connect_target)
        self._conn = chdb_driver.connect(connect_target)
        self._lock = threading.Lock()
        try:
            _validate_chdb_startup_configuration(self)
        except Exception:
            self._conn.close()
            raise

    def execute(self, query: str, params=None):
        with self._lock:
            cur = self._conn.cursor()
            if params:
                cur.execute(query, params)
            else:
                cur.execute(query)
            columns = [d[0] for d in (cur.description or [])]
            rows = cur.fetchall() or []
        return ChDbResult(columns, rows)

    def executescript(self, script: str):
        statements = [s.strip() for s in script.split(";") if s.strip()]
        with self._lock:
            cur = self._conn.cursor()
            for stmt in statements:
                cur.execute(stmt)

    def commit(self):
        return None  # ClickHouse auto-commits

    def close(self):
        self._conn.close()


_global_db: ChDbConnection | None = None
_db_init_lock = threading.Lock()
_schema_ready = False
_write_queue: queue.Queue["_WriteTask"] | None = None
_write_thread: threading.Thread | None = None
_write_worker_lock = threading.Lock()
_log_attr_keys_lock = threading.Lock()
_log_attr_keys_cache_loaded = False
_log_attr_keys_by_record_type: dict[str, set[str]] = {"log": set()}

WRITE_QUEUE_MAX = int(os.environ.get("SOBS_WRITE_QUEUE_MAX", 5000))
WRITE_BATCH_MAX = int(os.environ.get("SOBS_WRITE_BATCH_MAX", 200))
WRITE_BATCH_WAIT_MS = int(os.environ.get("SOBS_WRITE_BATCH_WAIT_MS", 20))
LOG_ATTR_KEYS_MAX = int(os.environ.get("SOBS_LOG_ATTR_KEYS_MAX", 20000))


@dataclass
class _WriteTask:
    op: Callable[[ChDbConnection], None]
    done: threading.Event | None = None
    error: Exception | None = None


class WriteQueueFullError(RuntimeError):
    """Raised when ingest cannot enqueue a write within timeout."""


def get_db() -> ChDbConnection:
    global _global_db, _schema_ready
    if _global_db is None or not _schema_ready:
        with _db_init_lock:
            if _global_db is None:
                _global_db = ChDbConnection(DB_PATH)
            if not _schema_ready:
                _global_db.executescript(SCHEMA)
                _ensure_post_schema_state(_global_db)
                _schema_ready = True
    return _global_db


def init_db():
    """(Re-)initialise the global DB connection and apply the schema."""
    global _global_db, _schema_ready
    with _db_init_lock:
        _global_db = ChDbConnection(DB_PATH)
        _global_db.executescript(SCHEMA)
        _ensure_post_schema_state(_global_db)
        _schema_ready = True


def ensure_db_schema():
    """Create schema if tables are missing (fallback for fresh DB directories)."""
    global _global_db, _schema_ready
    if _schema_ready:
        return
    with _db_init_lock:
        if _global_db is None:
            _global_db = ChDbConnection(DB_PATH)
        try:
            has_logs = _global_db.execute(
                "SELECT 1 FROM system.tables WHERE database='default' AND name='otel_logs'"
            ).fetchone()
        except Exception:
            has_logs = None
        if has_logs is None:
            _global_db.executescript(SCHEMA)
        _ensure_post_schema_state(_global_db)
        _schema_ready = True


def _ensure_post_schema_state(db: ChDbConnection) -> None:
    _ensure_anomaly_rule_schema(db)
    _prime_log_attr_key_cache(db)
    if not app.config.get("TESTING"):
        _seed_example_metrics_content(db)


def _load_log_attr_keys_from_db(db: ChDbConnection, record_type: str) -> set[str]:
    rows = db.execute(
        "SELECT DISTINCT AttrKey FROM sobs_log_attr_keys FINAL " "WHERE RecordType=? AND IsDeleted=0 ORDER BY AttrKey",
        [record_type],
    ).fetchall()
    return {str(r[0]) for r in rows if str(r[0]).strip()}


def _prime_log_attr_key_cache(db: ChDbConnection) -> None:
    global _log_attr_keys_cache_loaded
    with _log_attr_keys_lock:
        if _log_attr_keys_cache_loaded:
            return
        _log_attr_keys_by_record_type["log"] = _load_log_attr_keys_from_db(db, "log")
        _log_attr_keys_cache_loaded = True


def _get_cached_log_attr_keys(db: ChDbConnection, record_type: str = "log") -> list[str]:
    _prime_log_attr_key_cache(db)
    with _log_attr_keys_lock:
        keys = sorted(_log_attr_keys_by_record_type.get(record_type, set()))
    return keys


def _remember_log_attr_keys(db: ChDbConnection, attrs_maps: list[dict], record_type: str = "log") -> None:
    if not attrs_maps:
        return
    _prime_log_attr_key_cache(db)

    with _log_attr_keys_lock:
        existing = _log_attr_keys_by_record_type.setdefault(record_type, set())
        if len(existing) >= LOG_ATTR_KEYS_MAX:
            return

        candidates: set[str] = set()
        for attrs in attrs_maps:
            if not isinstance(attrs, dict):
                continue
            for raw_key in attrs.keys():
                key = str(raw_key).strip()
                if not key or key in existing or key in candidates:
                    continue
                if len(existing) + len(candidates) >= LOG_ATTR_KEYS_MAX:
                    break
                candidates.add(key)

        if not candidates:
            return

        version = int(time.time() * 1000)
        rows = [
            {
                "RecordType": record_type,
                "AttrKey": key,
                "IsDeleted": 0,
                "Version": version + idx,
            }
            for idx, key in enumerate(sorted(candidates))
        ]
        try:
            _insert_rows_json_each_row(db, "sobs_log_attr_keys", rows)
            existing.update(candidates)
        except Exception:
            app.logger.exception("failed to persist discovered log attribute keys")


def _extract_log_attr_maps(rows: list[dict]) -> list[dict]:
    maps: list[dict] = []
    for row in rows:
        raw_attrs = row.get("LogAttributes", {})
        if isinstance(raw_attrs, dict):
            maps.append(raw_attrs)
    return maps


def _ensure_anomaly_rule_schema(db: ChDbConnection) -> None:
    migration_statements = [
        (
            "ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS "
            "RuleType LowCardinality(String) DEFAULT 'threshold'"
        ),
        (
            "ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS "
            "SecondarySignalSource LowCardinality(String) DEFAULT ''"
        ),
        (
            "ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS "
            "SecondarySignalName LowCardinality(String) DEFAULT ''"
        ),
        (
            "ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS "
            "SecondaryComparator LowCardinality(String) DEFAULT 'gt'"
        ),
        "ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS SecondaryWarningThreshold Float64 DEFAULT 0",
        "ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS SecondaryCriticalThreshold Float64 DEFAULT 0",
    ]
    for statement in migration_statements:
        db.execute(statement)


def _seed_rule_if_missing(db: ChDbConnection, rule: dict[str, object]) -> None:
    existing = db.execute(
        "SELECT 1 FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Name = ? LIMIT 1",
        [str(rule["Name"])],
    ).fetchone()
    if existing:
        return
    _insert_rows_json_each_row(db, "sobs_anomaly_rules", [rule])


def _seed_dashboard_if_missing(db: ChDbConnection, dashboard_name: str, description: str) -> str:
    existing = db.execute(
        "SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Name = ? LIMIT 1",
        [dashboard_name],
    ).fetchone()
    if existing:
        return str(existing["Id"])

    dashboard_id = str(uuid.uuid4())
    _insert_rows_json_each_row(
        db,
        "sobs_dashboards",
        [
            {
                "Id": dashboard_id,
                "Name": dashboard_name,
                "Description": description,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    return dashboard_id


def _seed_chart_if_missing(
    db: ChDbConnection,
    dashboard_id: str,
    title: str,
    chart_type: str,
    query: str,
    position: int,
) -> None:
    existing = db.execute(
        "SELECT 1 FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ? AND Title = ? LIMIT 1",
        [dashboard_id, title],
    ).fetchone()
    if existing:
        return
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": str(uuid.uuid4()),
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": chart_type,
                "Query": query,
                "OptionsJson": json.dumps(
                    {"chart_spec": _build_raw_chart_spec(chart_type, query)},
                    ensure_ascii=False,
                ),
                "Position": position,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )


def _upsert_seed_chart(
    db: ChDbConnection,
    dashboard_id: str,
    title: str,
    chart_type: str,
    query: str,
    position: int,
) -> None:
    existing = db.execute(
        "SELECT Id, ChartType, Query, OptionsJson, Position "
        "FROM sobs_chart_configs FINAL "
        "WHERE IsDeleted = 0 AND DashboardId = ? AND Title = ? LIMIT 1",
        [dashboard_id, title],
    ).fetchone()
    if not existing:
        _seed_chart_if_missing(db, dashboard_id, title, chart_type, query, position)
        return

    if (
        str(existing["ChartType"]) == chart_type
        and str(existing["Query"]) == query
        and int(existing["Position"]) == position
    ):
        return

    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": str(existing["Id"]),
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": chart_type,
                "Query": query,
                "OptionsJson": json.dumps(
                    {"chart_spec": _build_raw_chart_spec(chart_type, query, str(existing["OptionsJson"]))},
                    ensure_ascii=False,
                ),
                "Position": position,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )


def _soft_delete_seed_chart_by_title(db: ChDbConnection, dashboard_id: str, title: str) -> None:
    row = db.execute(
        "SELECT Id, ChartType, Query, OptionsJson, Position "
        "FROM sobs_chart_configs FINAL "
        "WHERE IsDeleted = 0 AND DashboardId = ? AND Title = ? LIMIT 1",
        [dashboard_id, title],
    ).fetchone()
    if not row:
        return
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": str(row["Id"]),
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": str(row["ChartType"]),
                "Query": str(row["Query"]),
                "OptionsJson": str(row["OptionsJson"]),
                "Position": int(row["Position"]),
                "IsDeleted": 1,
                "Version": int(time.time() * 1000),
            }
        ],
    )


def _seed_example_metrics_content(db: ChDbConnection) -> None:
    version = int(time.time() * 1000)
    example_rules = [
        {
            "Id": str(uuid.uuid4()),
            "Name": "Trace latency elevated",
            "RuleType": "threshold",
            "SignalSource": "traces",
            "SignalName": "latency_p95_ms",
            "ServiceName": "trace-svc-0",
            "AttrFingerprint": "",
            "Comparator": "gt",
            "WarningThreshold": 250.0,
            "CriticalThreshold": 450.0,
            "SecondarySignalSource": "",
            "SecondarySignalName": "",
            "SecondaryComparator": "gt",
            "SecondaryWarningThreshold": 0.0,
            "SecondaryCriticalThreshold": 0.0,
            "MinSampleCount": 5,
            "IsDeleted": 0,
            "Version": version,
        },
        {
            "Id": str(uuid.uuid4()),
            "Name": "Trace error ratio elevated",
            "RuleType": "threshold",
            "SignalSource": "traces",
            "SignalName": "trace_error_ratio",
            "ServiceName": "trace-svc-0",
            "AttrFingerprint": "",
            "Comparator": "gt",
            "WarningThreshold": 0.04,
            "CriticalThreshold": 0.08,
            "SecondarySignalSource": "",
            "SecondarySignalName": "",
            "SecondaryComparator": "gt",
            "SecondaryWarningThreshold": 0.0,
            "SecondaryCriticalThreshold": 0.0,
            "MinSampleCount": 5,
            "IsDeleted": 0,
            "Version": version,
        },
        {
            "Id": str(uuid.uuid4()),
            "Name": "Exception volume elevated",
            "RuleType": "threshold",
            "SignalSource": "errors",
            "SignalName": "exception_volume",
            "ServiceName": "err-svc-0",
            "AttrFingerprint": "",
            "Comparator": "gt",
            "WarningThreshold": 1.0,
            "CriticalThreshold": 3.0,
            "SecondarySignalSource": "",
            "SecondarySignalName": "",
            "SecondaryComparator": "gt",
            "SecondaryWarningThreshold": 0.0,
            "SecondaryCriticalThreshold": 0.0,
            "MinSampleCount": 1,
            "IsDeleted": 0,
            "Version": version,
        },
        {
            "Id": str(uuid.uuid4()),
            "Name": "Composite trace distress",
            "RuleType": "composite",
            "SignalSource": "traces",
            "SignalName": "latency_p95_ms",
            "ServiceName": "trace-svc-0",
            "AttrFingerprint": "",
            "Comparator": "gt",
            "WarningThreshold": 250.0,
            "CriticalThreshold": 450.0,
            "SecondarySignalSource": "traces",
            "SecondarySignalName": "trace_error_ratio",
            "SecondaryComparator": "gt",
            "SecondaryWarningThreshold": 0.04,
            "SecondaryCriticalThreshold": 0.08,
            "MinSampleCount": 5,
            "IsDeleted": 0,
            "Version": version,
        },
    ]
    for rule in example_rules:
        _seed_rule_if_missing(db, rule)

    dashboard_id = _seed_dashboard_if_missing(
        db,
        "Example Derived Signals",
        "Seeded dashboard for load_example-derived log, trace, and error anomaly signals.",
    )
    charts = [
        (
            "Trace volume",
            "derived_signal_overlay",
            "SELECT\n"
            "  time,\n"
            "  ServiceName AS service,\n"
            "  SignalSource AS source,\n"
            "  SignalName AS signal,\n"
            "  AttrFingerprint AS attr_fp,\n"
            "  value,\n"
            "  SampleCount AS sample_count,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state,\n"
            "  anomaly_score\n"
            "FROM v_derived_signals_anomaly\n"
            "WHERE ServiceName = (\n"
            "  SELECT ServiceName\n"
            "  FROM v_derived_signals_anomaly\n"
            "  WHERE SignalSource = 'traces' AND SignalName = 'trace_volume'\n"
            "  ORDER BY time DESC\n"
            "  LIMIT 1\n"
            ")\n"
            "  AND SignalSource = 'traces'\n"
            "  AND SignalName = 'trace_volume'\n"
            "  AND time >= now() - INTERVAL 6 HOUR\n"
            "ORDER BY time",
        ),
        (
            "Trace error ratio",
            "derived_signal_overlay",
            "SELECT\n"
            "  time,\n"
            "  ServiceName AS service,\n"
            "  SignalSource AS source,\n"
            "  SignalName AS signal,\n"
            "  AttrFingerprint AS attr_fp,\n"
            "  value,\n"
            "  SampleCount AS sample_count,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state,\n"
            "  anomaly_score\n"
            "FROM v_derived_signals_anomaly\n"
            "WHERE ServiceName = (\n"
            "  SELECT ServiceName\n"
            "  FROM v_derived_signals_anomaly\n"
            "  WHERE SignalSource = 'traces' AND SignalName = 'trace_error_ratio'\n"
            "  ORDER BY time DESC\n"
            "  LIMIT 1\n"
            ")\n"
            "  AND SignalSource = 'traces'\n"
            "  AND SignalName = 'trace_error_ratio'\n"
            "  AND time >= now() - INTERVAL 6 HOUR\n"
            "ORDER BY time",
        ),
        (
            "Load log volume",
            "derived_signal_overlay",
            "SELECT\n"
            "  time,\n"
            "  ServiceName AS service,\n"
            "  SignalSource AS source,\n"
            "  SignalName AS signal,\n"
            "  AttrFingerprint AS attr_fp,\n"
            "  value,\n"
            "  SampleCount AS sample_count,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state,\n"
            "  anomaly_score\n"
            "FROM v_derived_signals_anomaly\n"
            "WHERE ServiceName = (\n"
            "  SELECT ServiceName\n"
            "  FROM v_derived_signals_anomaly\n"
            "  WHERE SignalSource = 'logs' AND SignalName = 'log_volume'\n"
            "  ORDER BY time DESC\n"
            "  LIMIT 1\n"
            ")\n"
            "  AND SignalSource = 'logs'\n"
            "  AND SignalName = 'log_volume'\n"
            "  AND time >= now() - INTERVAL 6 HOUR\n"
            "ORDER BY time",
        ),
        (
            "Exception volume",
            "derived_signal_overlay",
            "SELECT\n"
            "  time,\n"
            "  ServiceName AS service,\n"
            "  SignalSource AS source,\n"
            "  SignalName AS signal,\n"
            "  AttrFingerprint AS attr_fp,\n"
            "  value,\n"
            "  SampleCount AS sample_count,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state,\n"
            "  anomaly_score\n"
            "FROM v_derived_signals_anomaly\n"
            "WHERE ServiceName = (\n"
            "  SELECT ServiceName\n"
            "  FROM v_derived_signals_anomaly\n"
            "  WHERE SignalSource = 'errors' AND SignalName = 'exception_volume'\n"
            "  ORDER BY time DESC\n"
            "  LIMIT 1\n"
            ")\n"
            "  AND SignalSource = 'errors'\n"
            "  AND SignalName = 'exception_volume'\n"
            "  AND time >= now() - INTERVAL 6 HOUR\n"
            "ORDER BY time",
        ),
    ]
    for position, (title, chart_type, query) in enumerate(charts):
        _upsert_seed_chart(db, dashboard_id, title, chart_type, query, position)
    _soft_delete_seed_chart_by_title(db, dashboard_id, "Trace latency")


def _run_write_batch(tasks: list[_WriteTask]) -> None:
    db = get_db()
    for task in tasks:
        try:
            task.op(db)
        except Exception as exc:
            task.error = exc
    db.commit()
    for task in tasks:
        if task.done is not None:
            task.done.set()


def _write_worker_main() -> None:
    assert _write_queue is not None
    while True:
        first = _write_queue.get()
        batch = [first]
        deadline = time.monotonic() + (max(1, WRITE_BATCH_WAIT_MS) / 1000.0)
        while len(batch) < max(1, WRITE_BATCH_MAX):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            try:
                batch.append(_write_queue.get(timeout=remaining))
            except queue.Empty:
                break
        _run_write_batch(batch)


def _ensure_write_worker() -> None:
    global _write_queue, _write_thread
    if _write_thread is not None and _write_thread.is_alive():
        return
    with _write_worker_lock:
        if _write_queue is None:
            _write_queue = queue.Queue(maxsize=max(1, WRITE_QUEUE_MAX))
        if _write_thread is None or not _write_thread.is_alive():
            _write_thread = threading.Thread(target=_write_worker_main, name="sobs-db-writer", daemon=True)
            _write_thread.start()


def _queue_write(op: Callable[[ChDbConnection], None], wait: bool = False) -> None:
    _ensure_write_worker()
    done = threading.Event() if wait else None
    task = _WriteTask(op=op, done=done)
    assert _write_queue is not None
    try:
        _write_queue.put(task, timeout=1)
    except queue.Full as exc:
        raise WriteQueueFullError("write queue is full") from exc
    if done is not None:
        # Intentionally best-effort wait: embedded chDB runs in single-process mode
        # and sustained bursts can delay writer completion. We avoid surfacing a hard
        # timeout to clients here to prevent avoidable 5xx responses under backpressure.
        done.wait(timeout=15)
        if task.error is not None:
            raise task.error


def _write_queue_depth() -> int:
    return _write_queue.qsize() if _write_queue is not None else 0


# ---------------------------------------------------------------------------
# SSE tail pub/sub
# ---------------------------------------------------------------------------
_sse_subscribers: set[asyncio.Queue] = set()
_SSE_QUEUE_MAXSIZE = int(os.environ.get("SOBS_SSE_QUEUE_MAX", 200))


async def _sse_broadcast(event: dict) -> None:
    """Deliver an event to every active SSE subscriber (non-blocking, drops on full)."""
    for q in list(_sse_subscribers):
        try:
            q.put_nowait(event)
        except asyncio.QueueFull:
            pass


# ---------------------------------------------------------------------------
# Compression helpers
# ---------------------------------------------------------------------------
def compress(text: str) -> str:
    """Compress text and return as a base64-encoded string (chDB-safe)."""
    return base64.b64encode(zlib.compress(text.encode("utf-8"), level=9)).decode("ascii")


def decompress(data) -> str:
    """Decompress a base64-encoded compressed value. Returns '' for None/empty."""
    if not data:
        return ""
    raw = base64.b64decode(data) if isinstance(data, str) else data
    return zlib.decompress(raw).decode("utf-8")


def compress_json(obj) -> str:
    return compress(json.dumps(obj, ensure_ascii=False))


def decompress_json(data):
    if data is None:
        return {}
    return json.loads(decompress(data))


# ---------------------------------------------------------------------------
# Auth decorator (optional API key)
# ---------------------------------------------------------------------------
def _check_external_auth(authorization: str) -> bool:
    """Validate a Bearer token against the configured external auth service.

    Makes a POST to ``{EXTERNAL_AUTH_URL}/internal/auth/validate`` forwarding
    the ``Authorization`` header.  Returns ``True`` only on an HTTP 200 reply.
    """
    if not EXTERNAL_AUTH_URL:
        return False
    try:
        url = EXTERNAL_AUTH_URL.rstrip("/") + "/internal/auth/validate"
        req = urllib.request.Request(url, method="POST")
        req.add_header("Authorization", authorization)
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status == 200
    except (urllib.error.URLError, OSError):
        return False


def _auth_mode() -> str:
    """Return auth mode: none, basic, external, or invalid."""
    has_user = bool(BASIC_AUTH_USERNAME)
    has_pass = bool(BASIC_AUTH_PASSWORD)
    has_external = bool(EXTERNAL_AUTH_URL)

    # Configuration is exclusive: use at most one auth type.
    if has_external and (has_user or has_pass):
        return "invalid"
    # Basic auth requires both username and password.
    if has_user != has_pass:
        return "invalid"
    if has_external:
        return "external"
    if has_user and has_pass:
        return "basic"
    return "none"


def require_api_key(f):
    @wraps(f)
    async def decorated(*args, **kwargs):
        if API_KEY:
            key = request.headers.get("X-API-Key") or request.args.get("api_key")
            if key != API_KEY:
                return jsonify({"error": "Unauthorized"}), 401
        result = f(*args, **kwargs)
        if inspect.isawaitable(result):
            return await result
        return result

    return decorated


# ---------------------------------------------------------------------------
# Auth decorator (optional Basic Auth for Web UI)
# ---------------------------------------------------------------------------
def require_basic_auth(f):
    @wraps(f)
    async def decorated(*args, **kwargs):
        mode = _auth_mode()
        if mode == "invalid":
            return jsonify({"error": "Server auth misconfiguration"}), 500
        if mode == "none":
            result = f(*args, **kwargs)
            if inspect.isawaitable(result):
                return await result
            return result
        auth = request.headers.get("Authorization", "")
        # Accept valid HTTP Basic credentials when configured.
        if mode == "basic" and auth.startswith("Basic "):
            try:
                decoded = base64.b64decode(auth[6:], validate=True).decode("utf-8")
                username, _, password = decoded.partition(":")
                user_ok = secrets.compare_digest(username, BASIC_AUTH_USERNAME)
                pass_ok = secrets.compare_digest(password, BASIC_AUTH_PASSWORD)
                if user_ok and pass_ok:
                    result = f(*args, **kwargs)
                    if inspect.isawaitable(result):
                        return await result
                    return result
            except Exception:
                pass
        # Accept a Bearer token validated by the external auth service.
        # Fall back to the `session` cookie for same-origin browser requests
        # that carry no explicit Authorization header.
        if mode == "external":
            if not auth.startswith("Bearer "):
                session_cookie = request.cookies.get("session")
                if session_cookie and "\r" not in session_cookie and "\n" not in session_cookie:
                    auth = "Bearer " + session_cookie
            if auth.startswith("Bearer ") and await asyncio.to_thread(_check_external_auth, auth):
                result = f(*args, **kwargs)
                if inspect.isawaitable(result):
                    return await result
                return result
        # Advertise the configured auth scheme.
        if mode == "basic":
            www_auth = 'Basic realm="SOBS"'
        else:
            www_auth = 'Bearer realm="SOBS"'
        return (
            "Unauthorized",
            401,
            {"WWW-Authenticate": www_auth},
        )

    return decorated


# ---------------------------------------------------------------------------
# Utility helpers
# ---------------------------------------------------------------------------
def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


def _ns_to_iso(nanos: int) -> str:
    """Convert OpenTelemetry nanosecond timestamp to ISO-8601."""
    try:
        secs = nanos / 1_000_000_000
        return datetime.fromtimestamp(secs, tz=timezone.utc).isoformat(timespec="milliseconds")
    except Exception:
        return _now_iso()


def _parse_limit(default=200) -> int:
    try:
        return max(1, min(int(request.args.get("limit", default)), 5000))
    except (TypeError, ValueError):
        return default


def _parse_offset() -> int:
    try:
        return max(0, int(request.args.get("offset", 0)))
    except (TypeError, ValueError):
        return 0


def _parse_sort(allowed: dict, default_col: str = "Timestamp") -> tuple:
    """Parse and validate ``sort_by`` / ``sort_dir`` query params.

    *allowed* maps URL param values to SQL column names.
    Returns ``(sort_by, sql_col, sort_dir)`` where ``sort_dir`` is ``'asc'`` or ``'desc'``.
    """
    sort_by = request.args.get("sort_by", default_col)
    sort_dir = request.args.get("sort_dir", "desc").lower()
    if sort_by not in allowed:
        sort_by = default_col
    if sort_dir not in ("asc", "desc"):
        sort_dir = "desc"
    return sort_by, allowed[sort_by], sort_dir


def _parse_time_window_args() -> tuple[str, str, str]:
    """Parse ``from_ts``/``to_ts`` query params and optional ``window_s``."""
    from_ts_raw = request.args.get("from_ts", "").strip()
    to_ts_raw = request.args.get("to_ts", "").strip()
    window_s_raw = request.args.get("window_s", "").strip()

    try:
        from_ts = _normalize_ch_timestamp(from_ts_raw) if from_ts_raw else ""
        to_ts = _normalize_ch_timestamp(to_ts_raw) if to_ts_raw else ""
        if from_ts and not to_ts and window_s_raw:
            window_s = max(1, int(window_s_raw))
            from_dt = datetime.fromisoformat(from_ts)
            to_ts = _normalize_ch_timestamp(from_dt + timedelta(seconds=window_s))
        if from_ts and to_ts:
            from_dt = datetime.fromisoformat(from_ts)
            to_dt = datetime.fromisoformat(to_ts)
            if to_dt <= from_dt:
                return "", "", "Invalid time window: to_ts must be later than from_ts"
        return from_ts, to_ts, ""
    except (TypeError, ValueError):
        return "", "", "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"


def _time_window_conditions(column: str, from_ts: str, to_ts: str) -> tuple[list[str], list[str]]:
    """Build time-window WHERE fragments for ClickHouse DateTime64 columns."""
    conditions: list[str] = []
    params: list[str] = []
    if from_ts:
        conditions.append(f"{column} >= parseDateTime64BestEffort(?, 9)")
        params.append(from_ts)
    if to_ts:
        conditions.append(f"{column} < parseDateTime64BestEffort(?, 9)")
        params.append(to_ts)
    return conditions, params


def _hex(b) -> str:
    """Convert bytes or hex string to hex string."""
    if isinstance(b, (bytes, bytearray)):
        return b.hex()
    return str(b) if b else ""


def _stringify_attrs(values: dict | None) -> dict[str, str]:
    """Convert arbitrary attribute values to a string map suitable for OTel Map columns."""
    if not values:
        return {}
    out: dict[str, str] = {}
    for key, value in values.items():
        if value is None:
            continue
        if isinstance(value, (str, int, float, bool)):
            out[str(key)] = str(value)
        else:
            out[str(key)] = json.dumps(value, ensure_ascii=False)
    return out


def _extract_messages_text(messages_str: str) -> str:
    """Extract readable text from gen_ai.input.messages or gen_ai.output.messages JSON.

    Accepts either a JSON array of message objects (OTel GenAI convention) or a plain
    string and returns a human-readable representation for UI display.
    """
    if not messages_str:
        return ""
    try:
        messages = json.loads(messages_str)
        if isinstance(messages, list):
            parts = []
            for msg in messages:
                if isinstance(msg, dict):
                    role = msg.get("role", "")
                    content = msg.get("content", "")
                    if isinstance(content, list):
                        # Content blocks (e.g. OpenAI vision API)
                        content = " ".join(
                            part.get("text", "") if isinstance(part, dict) else str(part) for part in content
                        )
                    if content:
                        parts.append(f"[{role}] {content}" if role else str(content))
                elif isinstance(msg, str):
                    parts.append(msg)
            return "\n".join(parts)
        return messages_str
    except (json.JSONDecodeError, TypeError):
        return messages_str


def _map_to_dict(value) -> dict:
    """Best-effort conversion of ClickHouse Map values to Python dicts."""
    if isinstance(value, dict):
        return value
    if not value:
        return {}
    if isinstance(value, str):
        s = value.strip()
        if not s:
            return {}
        try:
            parsed = json.loads(s)
            return parsed if isinstance(parsed, dict) else {}
        except json.JSONDecodeError:
            pass
        try:
            parsed = ast.literal_eval(s)
            return parsed if isinstance(parsed, dict) else {}
        except (ValueError, SyntaxError):
            return {}
    return {}


def _severity_number(level: str) -> int:
    norm = (level or "").upper()
    mapping = {
        "TRACE": 1,
        "DEBUG": 5,
        "INFO": 9,
        "WARN": 13,
        "WARNING": 13,
        "ERROR": 17,
        "CRITICAL": 21,
        "FATAL": 21,
        "METRIC": 9,
    }
    return mapping.get(norm, 9)


def _trace_status_code(status: str) -> str:
    norm = (status or "").upper()
    if norm == "ERROR":
        return "STATUS_CODE_ERROR"
    if norm == "OK":
        return "STATUS_CODE_OK"
    return "STATUS_CODE_UNSET"


def _error_id(ts: str, service: str, err_type: str, message: str, trace_id: str, span_id: str) -> str:
    raw = "|".join([ts or "", service or "", err_type or "", message or "", trace_id or "", span_id or ""])
    return hashlib.md5(raw.encode("utf-8")).hexdigest()


def _insert_rows_json_each_row(db, table_name: str, rows: list[dict]) -> int:
    if not rows:
        return 0
    normalized_rows = []
    for row in rows:
        item = dict(row)
        if "Timestamp" in item:
            item["Timestamp"] = _normalize_ch_timestamp(item["Timestamp"])
        if "TimeUnix" in item:
            item["TimeUnix"] = _normalize_ch_timestamp(item["TimeUnix"])
        if "Events" in item and isinstance(item["Events"], dict) and "Timestamp" in item["Events"]:
            item["Events"]["Timestamp"] = [_normalize_ch_timestamp(v) for v in item["Events"]["Timestamp"]]
        normalized_rows.append(item)
    payload = "\n".join(json.dumps(row, ensure_ascii=False) for row in normalized_rows)
    db.execute(f"INSERT INTO {table_name} FORMAT JSONEachRow\n" + payload)
    return len(normalized_rows)


def _normalize_ch_timestamp(value) -> str:
    """Convert common timestamp forms to ClickHouse DateTime64-compatible strings."""
    if isinstance(value, datetime):
        dt = value.astimezone(timezone.utc) if value.tzinfo else value
    else:
        raw = str(value or "").strip()
        if not raw:
            dt = datetime.now(timezone.utc)
        else:
            try:
                dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
            except ValueError:
                # Last resort: preserve value and hope ClickHouse parser accepts it.
                return raw.replace("T", " ")
            if dt.tzinfo:
                dt = dt.astimezone(timezone.utc)
    return dt.strftime("%Y-%m-%d %H:%M:%S.%f")


def _attr_list_to_dict(attr_list: list) -> dict:
    """Convert OTLP attribute list [{key, value}] to plain dict."""
    out = {}
    for item in attr_list:
        key = item.get("key", "")
        val_obj = item.get("value", {})
        # OTLP uses typed value wrappers
        for vtype in ("stringValue", "intValue", "doubleValue", "boolValue", "bytesValue"):
            if vtype in val_obj:
                out[key] = val_obj[vtype]
                break
    return out


def _proto_any_value_to_python(val):
    """Convert OTLP AnyValue proto object to a plain Python value."""
    kind = val.WhichOneof("value")
    if kind == "string_value":
        return val.string_value
    if kind == "int_value":
        return val.int_value
    if kind == "double_value":
        return val.double_value
    if kind == "bool_value":
        return val.bool_value
    if kind == "bytes_value":
        return base64.b64encode(bytes(val.bytes_value)).decode("ascii")
    if kind == "array_value":
        return [_proto_any_value_to_python(v) for v in val.array_value.values]
    if kind == "kvlist_value":
        return {kv.key: _proto_any_value_to_python(kv.value) for kv in val.kvlist_value.values}
    return None


def _proto_kvlist_to_dict(attributes) -> dict:
    return {kv.key: _proto_any_value_to_python(kv.value) for kv in attributes}


@dataclass
class LogEvent:
    ts: str
    level: str
    service: str
    body: str
    attrs: dict
    trace_id: str
    span_id: str


@dataclass
class SpanEvent:
    ts: str
    trace_id: str
    span_id: str
    parent_span_id: str
    name: str
    service: str
    duration_ms: float
    status: str
    attrs: dict


@dataclass
class ErrorEvent:
    ts: str
    service: str
    err_type: str
    message: str
    stack: str
    attrs: dict
    trace_id: str
    span_id: str


@dataclass
class MetricEvent:
    ts: str
    service: str
    name: str
    attrs: dict


# Attribute key prefixes excluded from the metric series fingerprint (high-cardinality
# resource attributes that do not differentiate metric series).
_FINGERPRINT_SKIP_PREFIXES = ("telemetry.", "process.", "os.", "runtime.")


def _attr_fingerprint(attrs: dict) -> str:
    """Compute a stable, low-cardinality fingerprint of data-point attributes.

    Excludes high-cardinality resource/runtime attribute prefixes and limits
    to the first 8 sorted key=value pairs to keep cardinality manageable.
    """
    pairs = sorted(
        f"{k}={v}" for k, v in attrs.items() if not any(k.startswith(p) for p in _FINGERPRINT_SKIP_PREFIXES)
    )[:8]
    # MD5 is used here for non-cryptographic cardinality reduction only (16-hex fingerprint).
    return hashlib.md5("|".join(pairs).encode()).hexdigest()[:16]


@dataclass
class TypedMetricEvent:
    """A single OTEL metric data point with type information and value extracted."""

    ts: str
    service: str
    metric_name: str
    metric_description: str
    metric_unit: str
    metric_kind: str  # 'gauge', 'sum', or 'histogram'
    value: float
    attrs: dict  # data-point-level attributes
    attr_fp: str  # stable fingerprint for series identity
    is_monotonic: int = 0
    aggregation_temporality: int = 0
    histogram_count: int = 0
    histogram_sum: float = 0.0
    histogram_buckets: list = field(default_factory=list)
    histogram_bounds: list = field(default_factory=list)


def _proto_logs_to_events(msg: ExportLogsServiceRequest) -> list[LogEvent]:
    events: list[LogEvent] = []
    for resource_log in msg.resource_logs:
        resource_attrs = _proto_kvlist_to_dict(resource_log.resource.attributes)
        service = str(resource_attrs.get("service.name", ""))
        for scope_log in resource_log.scope_logs:
            for record in scope_log.log_records:
                record_attrs = _proto_kvlist_to_dict(record.attributes)
                merged_attrs = {**resource_attrs, **record_attrs}
                body_val = _proto_any_value_to_python(record.body)
                body_str = body_val if isinstance(body_val, str) else json.dumps(body_val, ensure_ascii=False)
                events.append(
                    LogEvent(
                        ts=_ns_to_iso(int(record.time_unix_nano or 0)),
                        level=(record.severity_text or "INFO").upper(),
                        service=service,
                        body=body_str,
                        attrs=merged_attrs,
                        trace_id=record.trace_id.hex() if record.trace_id else "",
                        span_id=record.span_id.hex() if record.span_id else "",
                    )
                )
    return events


def _proto_traces_to_events(msg: ExportTraceServiceRequest) -> tuple[list[SpanEvent], list[ErrorEvent]]:
    span_events: list[SpanEvent] = []
    error_events: list[ErrorEvent] = []
    for resource_span in msg.resource_spans:
        resource_attrs = _proto_kvlist_to_dict(resource_span.resource.attributes)
        service = str(resource_attrs.get("service.name", ""))
        for scope_span in resource_span.scope_spans:
            for span in scope_span.spans:
                start_ns = int(span.start_time_unix_nano or 0)
                end_ns = int(span.end_time_unix_nano or 0)
                duration_ms = (end_ns - start_ns) / 1_000_000 if end_ns > start_ns else 0
                status = "OK" if span.status.code == 1 else ("ERROR" if span.status.code == 2 else "UNSET")
                span_attrs = _proto_kvlist_to_dict(span.attributes)
                merged_attrs = {**resource_attrs, **span_attrs}
                span_event = SpanEvent(
                    ts=_ns_to_iso(start_ns),
                    trace_id=span.trace_id.hex() if span.trace_id else "",
                    span_id=span.span_id.hex() if span.span_id else "",
                    parent_span_id=span.parent_span_id.hex() if span.parent_span_id else "",
                    name=span.name,
                    service=service,
                    duration_ms=duration_ms,
                    status=status,
                    attrs=merged_attrs,
                )
                span_events.append(span_event)
                if "ERROR" in status.upper():
                    error_events.append(
                        ErrorEvent(
                            ts=span_event.ts,
                            service=service,
                            err_type=str(span_attrs.get("exception.type", "SpanError")),
                            message=str(
                                span_attrs.get("exception.message", span_attrs.get("error.message", span.name))
                            ),
                            stack=str(span_attrs.get("exception.stacktrace", "")),
                            attrs=merged_attrs,
                            trace_id=span_event.trace_id,
                            span_id=span_event.span_id,
                        )
                    )
    return span_events, error_events


def _proto_metrics_to_events(msg: ExportMetricsServiceRequest) -> list[TypedMetricEvent]:
    """Parse OTLP ExportMetricsServiceRequest into typed data-point events.

    Supports gauge, sum, and histogram metric types with actual numeric values.
    """
    events: list[TypedMetricEvent] = []
    for resource_metric in msg.resource_metrics:
        resource_attrs = _proto_kvlist_to_dict(resource_metric.resource.attributes)
        service = str(resource_attrs.get("service.name", "metrics"))
        for scope_metric in resource_metric.scope_metrics:
            for metric in scope_metric.metrics:
                name = metric.name
                desc = metric.description
                unit = metric.unit
                which = metric.WhichOneof("data")

                if which == "gauge":
                    for dp in metric.gauge.data_points:
                        dp_attrs = _proto_kvlist_to_dict(dp.attributes)
                        vfield = dp.WhichOneof("value")
                        value = float(dp.as_int) if vfield == "as_int" else dp.as_double
                        ts = _ns_to_iso(int(dp.time_unix_nano)) if dp.time_unix_nano else _now_iso()
                        events.append(
                            TypedMetricEvent(
                                ts=ts,
                                service=service,
                                metric_name=name,
                                metric_description=desc,
                                metric_unit=unit,
                                metric_kind="gauge",
                                value=value,
                                attrs=dp_attrs,
                                attr_fp=_attr_fingerprint(dp_attrs),
                            )
                        )

                elif which == "sum":
                    for dp in metric.sum.data_points:
                        dp_attrs = _proto_kvlist_to_dict(dp.attributes)
                        vfield = dp.WhichOneof("value")
                        value = float(dp.as_int) if vfield == "as_int" else dp.as_double
                        ts = _ns_to_iso(int(dp.time_unix_nano)) if dp.time_unix_nano else _now_iso()
                        events.append(
                            TypedMetricEvent(
                                ts=ts,
                                service=service,
                                metric_name=name,
                                metric_description=desc,
                                metric_unit=unit,
                                metric_kind="sum",
                                value=value,
                                attrs=dp_attrs,
                                attr_fp=_attr_fingerprint(dp_attrs),
                                is_monotonic=1 if metric.sum.is_monotonic else 0,
                                aggregation_temporality=int(metric.sum.aggregation_temporality),
                            )
                        )

                elif which == "histogram":
                    for dp in metric.histogram.data_points:
                        dp_attrs = _proto_kvlist_to_dict(dp.attributes)
                        count = int(dp.count)
                        hist_sum = float(dp.sum)
                        mean_val = hist_sum / count if count > 0 else 0.0
                        ts = _ns_to_iso(int(dp.time_unix_nano)) if dp.time_unix_nano else _now_iso()
                        events.append(
                            TypedMetricEvent(
                                ts=ts,
                                service=service,
                                metric_name=name,
                                metric_description=desc,
                                metric_unit=unit,
                                metric_kind="histogram",
                                value=mean_val,
                                attrs=dp_attrs,
                                attr_fp=_attr_fingerprint(dp_attrs),
                                aggregation_temporality=int(metric.histogram.aggregation_temporality),
                                histogram_count=count,
                                histogram_sum=hist_sum,
                                histogram_buckets=list(dp.bucket_counts),
                                histogram_bounds=list(dp.explicit_bounds),
                            )
                        )

                else:
                    # Unsupported metric type (exponential histogram, summary):
                    # fall back to a minimal gauge-like entry at current time.
                    events.append(
                        TypedMetricEvent(
                            ts=_now_iso(),
                            service=service,
                            metric_name=name,
                            metric_description=desc,
                            metric_unit=unit,
                            metric_kind="gauge",
                            value=0.0,
                            attrs={},
                            attr_fp=_attr_fingerprint({}),
                        )
                    )

    return events


def _insert_log_events(db, events: list[LogEvent]) -> int:
    rows = []
    for event in events:
        rows.append(
            {
                "Timestamp": event.ts,
                "TraceId": event.trace_id,
                "SpanId": event.span_id,
                "TraceFlags": 0,
                "SeverityText": event.level,
                "SeverityNumber": _severity_number(event.level),
                "ServiceName": event.service,
                "Body": event.body,
                "ResourceSchemaUrl": "",
                "ResourceAttributes": {},
                "ScopeSchemaUrl": "",
                "ScopeName": "",
                "ScopeVersion": "",
                "ScopeAttributes": {},
                "LogAttributes": _stringify_attrs(event.attrs),
                "EventName": str(event.attrs.get("event.name", "")),
            }
        )
    count = _insert_rows_json_each_row(db, "otel_logs", rows)
    _remember_log_attr_keys(db, _extract_log_attr_maps(rows), record_type="log")
    try:
        rules = _load_tag_rules(db)
        if rules:
            _apply_tag_rules(db, "log", rows, rules)
    except Exception:
        app.logger.exception("auto-tag application failed for logs")
    return count


def _insert_span_events(db, span_events: list[SpanEvent]) -> int:
    rows = []
    for event in span_events:
        rows.append(
            {
                "Timestamp": event.ts,
                "TraceId": event.trace_id,
                "SpanId": event.span_id,
                "ParentSpanId": event.parent_span_id,
                "TraceState": "",
                "SpanName": event.name,
                "SpanKind": event.attrs.get("span.kind", "INTERNAL"),
                "ServiceName": event.service,
                "ResourceAttributes": {"service.name": event.service} if event.service else {},
                "ScopeName": "",
                "ScopeVersion": "",
                "SpanAttributes": _stringify_attrs(event.attrs),
                "Duration": max(0, int(event.duration_ms * 1_000_000)),
                "StatusCode": _trace_status_code(event.status),
                "StatusMessage": str(event.attrs.get("status.message", "")),
                "Events": {"Timestamp": [], "Name": [], "Attributes": []},
                "Links": {"TraceId": [], "SpanId": [], "TraceState": [], "Attributes": []},
            }
        )
    count = _insert_rows_json_each_row(db, "otel_traces", rows)
    try:
        rules = _load_tag_rules(db)
        if rules:
            _apply_tag_rules(db, "trace", rows, rules)
    except Exception:
        app.logger.exception("auto-tag application failed for traces")
    return count


def _insert_error_events(db, error_events: list[ErrorEvent]):
    rows = []
    for event in error_events:
        attrs = _stringify_attrs(event.attrs)
        attrs["exception.type"] = event.err_type
        attrs["exception.message"] = event.message
        if event.stack:
            attrs["exception.stacktrace"] = event.stack
        rows.append(
            {
                "Timestamp": event.ts,
                "TraceId": event.trace_id,
                "SpanId": event.span_id,
                "TraceFlags": 0,
                "SeverityText": "ERROR",
                "SeverityNumber": _severity_number("ERROR"),
                "ServiceName": event.service,
                "Body": event.message,
                "ResourceSchemaUrl": "",
                "ResourceAttributes": {},
                "ScopeSchemaUrl": "",
                "ScopeName": "",
                "ScopeVersion": "",
                "ScopeAttributes": {},
                "LogAttributes": attrs,
                "EventName": "exception",
            }
        )
    _insert_rows_json_each_row(db, "otel_logs", rows)
    _remember_log_attr_keys(db, _extract_log_attr_maps(rows), record_type="log")
    try:
        rules = _load_tag_rules(db)
        if rules:
            _apply_tag_rules(db, "error", rows, rules)
    except Exception:
        app.logger.exception("auto-tag application failed for errors")


def _insert_metric_events(db, events: list[TypedMetricEvent]) -> int:
    """Insert typed OTEL metric data points into the appropriate metric tables."""
    return _insert_typed_metric_events(db, events)


def _insert_typed_metric_events(db, events: list[TypedMetricEvent]) -> int:
    """Route typed metric events to their respective OTEL metric tables."""
    gauge_rows: list[dict] = []
    sum_rows: list[dict] = []
    histogram_rows: list[dict] = []

    for ev in events:
        base = {
            "TimeUnix": ev.ts,
            "ServiceName": ev.service,
            "MetricName": ev.metric_name,
            "MetricDescription": ev.metric_description,
            "MetricUnit": ev.metric_unit,
            "Attributes": _stringify_attrs(ev.attrs),
            "Value": float(ev.value),
            "Flags": 0,
            "AttrFingerprint": ev.attr_fp,
        }
        if ev.metric_kind == "gauge":
            gauge_rows.append(base)
        elif ev.metric_kind == "sum":
            sum_rows.append(
                {**base, "IsMonotonic": ev.is_monotonic, "AggregationTemporality": ev.aggregation_temporality}
            )
        elif ev.metric_kind == "histogram":
            histogram_rows.append(
                {
                    **{k: v for k, v in base.items() if k != "Value"},
                    "Count": ev.histogram_count,
                    "Sum": float(ev.histogram_sum),
                    "BucketCounts": ev.histogram_buckets or [],
                    "ExplicitBounds": ev.histogram_bounds or [],
                    "AggregationTemporality": ev.aggregation_temporality,
                }
            )

    inserted = 0
    if gauge_rows:
        inserted += _insert_rows_json_each_row(db, "otel_metrics_gauge", gauge_rows)
    if sum_rows:
        inserted += _insert_rows_json_each_row(db, "otel_metrics_sum", sum_rows)
    if histogram_rows:
        inserted += _insert_rows_json_each_row(db, "otel_metrics_histogram", histogram_rows)
    return inserted


_PROTOBUF_CONTENT_TYPE = "application/x-protobuf"


async def _parse_otlp_request(proto_class):
    """
    Parse an OTLP HTTP request body.

    Returns ``(proto_message, error_response)`` where ``error_response`` is
    ``None`` on success or a ``(flask_response, status_code)`` tuple on failure.

    - ``Content-Type: application/x-protobuf`` → deserialise with *proto_class*.
    - Any other content-type (including ``application/json``) → parse JSON and
      map into the same protobuf class via protobuf JSON mapping.
    """
    mimetype = (request.mimetype or "").lower()
    msg = proto_class()
    if mimetype == _PROTOBUF_CONTENT_TYPE:
        app.logger.debug("OTLP ingest: parse_path=protobuf endpoint=%s", request.path)
        try:
            msg.ParseFromString(await request.get_data())
        except Exception as exc:
            app.logger.warning("OTLP protobuf parse error [%s]: %s", request.path, exc)
            return None, (jsonify({"error": "failed to parse protobuf body"}), 400)
        return msg, None
    app.logger.debug("OTLP ingest: parse_path=json endpoint=%s", request.path)
    payload = await request.get_json(force=True, silent=True)
    if payload is None:
        payload = {}
    try:
        ParseDict(payload, msg)
    except Exception as exc:
        app.logger.warning("OTLP json parse error [%s]: %s", request.path, exc)
        return None, (jsonify({"error": "failed to parse json body"}), 400)
    return msg, None


# ---------------------------------------------------------------------------
# OTLP Ingest – Logs  POST /v1/logs
# ---------------------------------------------------------------------------
@app.route("/v1/logs", methods=["POST"])
@require_api_key
async def ingest_logs():
    msg, err = await _parse_otlp_request(ExportLogsServiceRequest)
    if err:
        return err
    events = _proto_logs_to_events(msg)
    wait = bool(app.config.get("TESTING", False))
    try:
        _queue_write(lambda db: _insert_log_events(db, events), wait=wait)
    except WriteQueueFullError as exc:
        return jsonify({"error": str(exc)}), 503
    except Exception as exc:
        app.logger.exception("log ingest write failed")
        return jsonify({"error": str(exc)}), 500
    for event in events:
        await _sse_broadcast(
            {
                "source": "logs",
                "ts": event.ts,
                "level": event.level,
                "service": event.service,
                "body": event.body,
                "trace_id": event.trace_id,
            }
        )
    count = len(events)
    return jsonify({"accepted": count}), 200


# ---------------------------------------------------------------------------
# OTLP Ingest – Traces  POST /v1/traces
# ---------------------------------------------------------------------------
@app.route("/v1/traces", methods=["POST"])
@require_api_key
async def ingest_traces():
    msg, err = await _parse_otlp_request(ExportTraceServiceRequest)
    if err:
        return err
    span_events, error_events = _proto_traces_to_events(msg)
    wait = bool(app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_span_events(db, span_events)
        _insert_error_events(db, error_events)

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError as exc:
        return jsonify({"error": str(exc)}), 503
    except Exception as exc:
        app.logger.exception("trace ingest write failed")
        return jsonify({"error": str(exc)}), 500
    for event in span_events:
        await _sse_broadcast(
            {
                "source": "traces",
                "ts": event.ts,
                "trace_id": event.trace_id,
                "span_id": event.span_id,
                "name": event.name,
                "service": event.service,
                "duration_ms": event.duration_ms,
                "status": event.status,
            }
        )
        # Also broadcast as an AI event when the span carries GenAI attributes
        provider = event.attrs.get("gen_ai.provider.name") or event.attrs.get("gen_ai.system", "")
        if provider:
            await _sse_broadcast(
                {
                    "source": "ai",
                    "ts": event.ts,
                    "trace_id": event.trace_id,
                    "span_id": event.span_id,
                    "service": event.service,
                    "provider": provider,
                    "model": str(event.attrs.get("gen_ai.request.model", "")),
                    "operation": str(event.attrs.get("gen_ai.operation.name", "")),
                    "duration_ms": event.duration_ms,
                    "status": event.status,
                }
            )
    count = len(span_events)
    return jsonify({"accepted": count}), 200


# ---------------------------------------------------------------------------
# OTLP Ingest – Metrics  POST /v1/metrics
# ---------------------------------------------------------------------------
@app.route("/v1/metrics", methods=["POST"])
@require_api_key
async def ingest_metrics():
    msg, err = await _parse_otlp_request(ExportMetricsServiceRequest)
    if err:
        return err
    events = _proto_metrics_to_events(msg)
    wait = bool(app.config.get("TESTING", False))
    try:
        _queue_write(lambda db: _insert_metric_events(db, events), wait=wait)
    except WriteQueueFullError as exc:
        return jsonify({"error": str(exc)}), 503
    except Exception as exc:
        app.logger.exception("metric ingest write failed")
        return jsonify({"error": str(exc)}), 500
    count = len(events)
    return jsonify({"accepted": count}), 200


# ---------------------------------------------------------------------------
# RUM Ingest  POST /v1/rum
# ---------------------------------------------------------------------------
@app.route("/v1/rum", methods=["POST"])
@require_api_key
async def ingest_rum():
    payload = await request.get_json(force=True, silent=True)
    if payload is None:
        payload = {}
    if isinstance(payload, list):
        events = payload
    else:
        events = payload.get("events", [payload])
    session_rows = []
    error_rows = []
    for event in events:
        ts = event.get("timestamp", _now_iso())
        session_id = event.get("sessionId", "")
        event_type = event.get("type", "unknown")
        url = event.get("url", "")
        attrs = _stringify_attrs(event)
        session_rows.append(
            {
                "Timestamp": ts,
                "TraceId": str(event.get("traceId", "")),
                "SpanId": str(event.get("spanId", "")),
                "TraceFlags": 0,
                "SeverityText": "ERROR" if event_type in ("error", "unhandledrejection") else "INFO",
                "SeverityNumber": _severity_number(
                    "ERROR" if event_type in ("error", "unhandledrejection") else "INFO"
                ),
                "ServiceName": str(event.get("service", "browser")),
                "Body": json.dumps(event, ensure_ascii=False),
                "ResourceSchemaUrl": "",
                "ResourceAttributes": {},
                "ScopeSchemaUrl": "",
                "ScopeName": "browser-rum",
                "ScopeVersion": "",
                "ScopeAttributes": {},
                "LogAttributes": attrs,
                "EventName": event_type,
            }
        )

        # Also index browser exceptions into otel_logs for unified error views.
        if event_type in ("error", "unhandledrejection"):
            err_attrs = {
                "exception.type": str(event.get("errorType", "JSError")),
                "exception.message": str(event.get("message", "")),
                "url.full": url,
                "session.id": session_id,
            }
            if event.get("stack"):
                err_attrs["exception.stacktrace"] = str(event.get("stack"))
            error_rows.append(
                {
                    "Timestamp": ts,
                    "TraceId": str(event.get("traceId", "")),
                    "SpanId": str(event.get("spanId", "")),
                    "TraceFlags": 0,
                    "SeverityText": "ERROR",
                    "SeverityNumber": _severity_number("ERROR"),
                    "ServiceName": "rum",
                    "Body": str(event.get("message", "")),
                    "ResourceSchemaUrl": "",
                    "ResourceAttributes": {},
                    "ScopeSchemaUrl": "",
                    "ScopeName": "browser-rum",
                    "ScopeVersion": "",
                    "ScopeAttributes": {},
                    "LogAttributes": err_attrs,
                    "EventName": "exception",
                }
            )
    wait = bool(app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_rows_json_each_row(db, "hyperdx_sessions", session_rows)
        _insert_rows_json_each_row(db, "otel_logs", error_rows)
        _remember_log_attr_keys(db, _extract_log_attr_maps(error_rows), record_type="log")
        try:
            rules = _load_tag_rules(db)
            if rules:
                _apply_tag_rules(db, "rum", session_rows, rules)
                if error_rows:
                    _apply_tag_rules(db, "error", error_rows, rules)
        except Exception:
            app.logger.exception("auto-tag application failed for rum")

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError as exc:
        return jsonify({"error": str(exc)}), 503
    except Exception as exc:
        app.logger.exception("rum ingest write failed")
        return jsonify({"error": str(exc)}), 500
    return jsonify({"accepted": len(session_rows)}), 200


# ---------------------------------------------------------------------------
# AI Transparency  POST /v1/ai
# ---------------------------------------------------------------------------
@app.route("/v1/ai", methods=["POST"])
@require_api_key
async def ingest_ai():
    payload = await request.get_json(force=True, silent=True) or {}
    ts = payload.get("timestamp", _now_iso())
    model = str(payload.get("model", ""))
    # Canonicalize operation: default to "chat", normalise case/whitespace
    operation = (str(payload.get("operation", "")) or "chat").lower().strip()
    duration_ms = float(payload.get("duration_ms", 0) or 0)
    provider = str(payload.get("provider", ""))
    service = str(payload.get("service", ""))
    span_name = f"{operation} {model}".strip()
    span_attrs: dict = {
        "gen_ai.operation.name": operation,
        "gen_ai.provider.name": provider,
        "gen_ai.request.model": model,
        "gen_ai.usage.input_tokens": int(payload.get("tokens_in", 0) or 0),
        "gen_ai.usage.output_tokens": int(payload.get("tokens_out", 0) or 0),
    }
    # Standard OTel GenAI content attributes (primary)
    if payload.get("input_messages") is not None:
        raw = payload["input_messages"]
        span_attrs["gen_ai.input.messages"] = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False)
    if payload.get("output_messages") is not None:
        raw = payload["output_messages"]
        span_attrs["gen_ai.output.messages"] = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False)
    # Legacy sobs fields (kept for backward-compat / UI fallback)
    if payload.get("prompt"):
        span_attrs["sobs.gen_ai.prompt"] = str(payload["prompt"])
    if payload.get("response"):
        span_attrs["sobs.gen_ai.response"] = str(payload["response"])
    if payload.get("error_type"):
        span_attrs["error.type"] = str(payload["error_type"])
    row = {
        "Timestamp": ts,
        "TraceId": str(payload.get("trace_id", "")),
        "SpanId": str(payload.get("span_id", "")),
        "ParentSpanId": "",
        "TraceState": "",
        "SpanName": span_name,
        "SpanKind": "CLIENT",
        "ServiceName": service,
        "ResourceAttributes": {},
        "ScopeName": "sobs-ai",
        "ScopeVersion": "",
        "SpanAttributes": _stringify_attrs(span_attrs),
        "Duration": max(0, int(duration_ms * 1_000_000)),
        "StatusCode": "STATUS_CODE_OK",
        "StatusMessage": "",
        "Events": {"Timestamp": [], "Name": [], "Attributes": []},
        "Links": {"TraceId": [], "SpanId": [], "TraceState": [], "Attributes": []},
    }
    wait = bool(app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_rows_json_each_row(db, "otel_traces", [row])
        try:
            rules = _load_tag_rules(db)
            if rules:
                _apply_tag_rules(db, "ai", [row], rules)
        except Exception:
            app.logger.exception("auto-tag application failed for ai")

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError as exc:
        return jsonify({"error": str(exc)}), 503
    except Exception as exc:
        app.logger.exception("ai ingest write failed")
        return jsonify({"error": str(exc)}), 500
    await _sse_broadcast(
        {
            "source": "ai",
            "ts": ts,
            "service": service,
            "provider": provider,
            "model": model,
            "operation": operation,
            "duration_ms": round(duration_ms, 1),
            "tokens_in": span_attrs["gen_ai.usage.input_tokens"],
            "tokens_out": span_attrs["gen_ai.usage.output_tokens"],
        }
    )
    return jsonify({"ok": True}), 200


# ---------------------------------------------------------------------------
# Error ingest  POST /v1/errors  (direct error submission)
# ---------------------------------------------------------------------------
@app.route("/v1/errors", methods=["POST"])
@require_api_key
async def ingest_errors():
    payload = await request.get_json(force=True, silent=True) or {}
    ts = payload.get("timestamp", _now_iso())
    attrs = _stringify_attrs(payload.get("attributes", {}))
    attrs["exception.type"] = str(payload.get("type", "Error"))
    attrs["exception.message"] = str(payload.get("message", ""))
    if payload.get("stack"):
        attrs["exception.stacktrace"] = str(payload.get("stack"))
    row = {
        "Timestamp": ts,
        "TraceId": str(payload.get("trace_id", "")),
        "SpanId": str(payload.get("span_id", "")),
        "TraceFlags": 0,
        "SeverityText": "ERROR",
        "SeverityNumber": _severity_number("ERROR"),
        "ServiceName": str(payload.get("service", "")),
        "Body": str(payload.get("message", "")),
        "ResourceSchemaUrl": "",
        "ResourceAttributes": {},
        "ScopeSchemaUrl": "",
        "ScopeName": "",
        "ScopeVersion": "",
        "ScopeAttributes": {},
        "LogAttributes": attrs,
        "EventName": "exception",
    }
    wait = bool(app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_rows_json_each_row(db, "otel_logs", [row])
        _remember_log_attr_keys(db, _extract_log_attr_maps([row]), record_type="log")
        try:
            rules = _load_tag_rules(db)
            if rules:
                _apply_tag_rules(db, "error", [row], rules)
        except Exception:
            app.logger.exception("auto-tag application failed for direct errors")

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError as exc:
        return jsonify({"error": str(exc)}), 503
    except Exception as exc:
        app.logger.exception("error ingest write failed")
        return jsonify({"error": str(exc)}), 500
    return jsonify({"ok": True}), 200


ERROR_SOURCES_SQL = """
SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes
FROM otel_logs
WHERE EventName = 'exception'
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
UNION ALL
SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes
FROM hyperdx_sessions
WHERE EventName IN ('error', 'unhandledrejection', 'exception')
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
"""


def _build_error_item(row: dict) -> dict:
    attrs = _map_to_dict(row.get("LogAttributes"))
    ts = str(row.get("Timestamp", ""))
    service = str(row.get("ServiceName", ""))
    err_type = str(attrs.get("exception.type", "Error"))
    message = str(attrs.get("exception.message", row.get("Body", "")))
    stack = str(attrs.get("exception.stacktrace", ""))
    trace_id = str(row.get("TraceId", ""))
    span_id = str(row.get("SpanId", ""))
    eid = _error_id(ts, service, err_type, message, trace_id, span_id)
    return {
        "id": eid,
        "ts": ts,
        "service": service,
        "err_type": err_type,
        "message": message,
        "stack": stack,
        "trace_id": trace_id,
        "span_id": span_id,
    }


def _get_resolved_error_ids(db) -> set[str]:
    return {str(r[0]) for r in db.execute("SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId").fetchall()}


# ---------------------------------------------------------------------------
# Web UI – Summary
# ---------------------------------------------------------------------------
@app.route("/")
@require_basic_auth
async def summary():
    db = get_db()
    resolved_ids = _get_resolved_error_ids(db)
    error_items = []
    for row in db.execute(f"SELECT * FROM ({ERROR_SOURCES_SQL}) ORDER BY Timestamp DESC").fetchall():
        item = _build_error_item(dict(row))
        item["resolved"] = item["id"] in resolved_ids
        error_items.append(item)

    unresolved_count = sum(0 if item["resolved"] else 1 for item in error_items)
    stats = {
        "logs": db.execute("SELECT COUNT(*) FROM otel_logs").fetchone()[0],
        "errors": unresolved_count,
        "errors_total": len(error_items),
        "spans": db.execute("SELECT COUNT(*) FROM otel_traces").fetchone()[0],
        "rum": db.execute("SELECT COUNT(*) FROM hyperdx_sessions").fetchone()[0],
        "ai": db.execute(
            "SELECT COUNT(*) FROM otel_traces "
            "WHERE (SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '')"
        ).fetchone()[0],
        "services": [
            r[0]
            for r in db.execute(
                "SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' "
                "UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' "
                "UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName!=''"
            ).fetchall()
        ],
    }
    # Recent errors (last 5)
    recent_errors = [
        {
            "id": item["id"],
            "ts": item["ts"],
            "service": item["service"],
            "err_type": item["err_type"],
            "message": item["message"],
        }
        for item in error_items
        if not item["resolved"]
    ][:5]
    # Recent logs (last 10)
    recent_logs = []
    for r in db.execute(
        "SELECT Timestamp, SeverityText, ServiceName, Body FROM otel_logs ORDER BY Timestamp DESC LIMIT 10"
    ).fetchall():
        recent_logs.append(
            {
                "ts": str(r["Timestamp"]),
                "level": r["SeverityText"],
                "service": r["ServiceName"],
                "body": r["Body"],
            }
        )
    # RUM summary – page views last 24h
    rum_summary = db.execute(
        "SELECT EventName, COUNT(*) as cnt FROM hyperdx_sessions GROUP BY EventName ORDER BY cnt DESC"
    ).fetchall()
    # AI summary
    ai_summary = db.execute(
        "SELECT SpanAttributes['gen_ai.request.model'] AS model, "
        "COUNT(*) cnt, "
        "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])) ti, "
        "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])) to_ "
        "FROM otel_traces "
        "WHERE SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '' "
        "GROUP BY model"
    ).fetchall()
    return await render_template(
        "summary.html",
        stats=stats,
        recent_errors=recent_errors,
        recent_logs=recent_logs,
        rum_summary=rum_summary,
        ai_summary=ai_summary,
        signal_health=_get_signal_health_by_service(db),
    )


def _compute_log_stats(db, where_clause: str, params: list) -> tuple[dict, dict]:
    """Return (level_stats, service_stats) counts for the given WHERE clause."""
    level_query = (
        "SELECT SeverityText, COUNT(*) AS cnt "
        f"FROM otel_logs {where_clause} "
        "GROUP BY SeverityText ORDER BY cnt DESC"
    )
    level_stats = {(r["SeverityText"] or "UNKNOWN"): r["cnt"] for r in db.execute(level_query, params).fetchall()}

    svc_cond = "AND ServiceName!=''" if where_clause else "WHERE ServiceName!=''"
    service_query = (
        "SELECT ServiceName, COUNT(*) AS cnt "
        f"FROM otel_logs {where_clause} {svc_cond} "
        "GROUP BY ServiceName ORDER BY cnt DESC LIMIT 10"
    )
    service_stats = {r["ServiceName"]: r["cnt"] for r in db.execute(service_query, params).fetchall()}
    return level_stats, service_stats


def _fingerprint_log_message(message: str) -> str:
    """Normalize dynamic values so repeating message patterns can be grouped."""
    normalized = (message or "").strip().lower()
    if not normalized:
        return "(empty message)"

    patterns = [
        (r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b", "<uuid>"),
        (r"\b0x[0-9a-f]+\b", "<hex>"),
        (r"\b[0-9a-f]{16,}\b", "<hash>"),
        (r"\b\d{4,}\b", "<num>"),
        (r"\b\d+\b", "<n>"),
    ]
    for pattern, replacement in patterns:
        normalized = re.sub(pattern, replacement, normalized)

    normalized = re.sub(r"'[^']*'", "'<text>'", normalized)
    normalized = re.sub(r'"[^"]*"', '"<text>"', normalized)
    normalized = re.sub(r"\s+", " ", normalized).strip()
    return normalized[:160]


def _compute_advanced_log_analysis(rows: list[dict], level_stats: dict, service_stats: dict) -> dict:
    """Compute message intelligence for manual advanced analysis runs."""
    messages = [str(row["Body"] or "") for row in rows if row["Body"]]
    if not messages:
        return {
            "top_patterns": [],
            "top_keywords": [],
            "error_families": [],
            "hints": [],
        }

    fingerprint_counts: Counter[str] = Counter(_fingerprint_log_message(msg) for msg in messages)
    most_common_patterns = fingerprint_counts.most_common(8)
    top_patterns = [{"pattern": pattern, "count": count} for pattern, count in most_common_patterns]

    family_regex = re.compile(r"\b([A-Za-z_][A-Za-z0-9_]*(?:Error|Exception|Timeout|Refused|Unavailable|Failure))\b")
    family_counts: Counter[str] = Counter()

    # Prefer structured exception types when available, then fall back to message parsing.
    for row in rows:
        attrs = _map_to_dict(row.get("LogAttributes"))
        exc_type = str(attrs.get("exception.type", "")).strip()
        if exc_type:
            family_counts[exc_type] += 1

    for msg in messages:
        for family in set(family_regex.findall(msg)):
            family_counts[family] += 1
    error_families = [{"family": family, "count": count} for family, count in family_counts.most_common(8)]

    stop_words = {
        "the",
        "and",
        "for",
        "with",
        "from",
        "into",
        "this",
        "that",
        "http",
        "https",
        "failed",
        "error",
        "warn",
        "info",
        "debug",
        "trace",
        "service",
    }
    keyword_counts: Counter[str] = Counter()
    for msg in messages:
        for token in re.findall(r"[a-z][a-z0-9_\-]{2,}", msg.lower()):
            if token not in stop_words:
                keyword_counts[token] += 1
    top_keywords = [{"keyword": keyword, "count": count} for keyword, count in keyword_counts.most_common(10)]

    hints = []
    total = max(len(rows), 1)
    severe = sum(
        int(count)
        for level, count in level_stats.items()
        if str(level).upper() in {"ERROR", "FATAL", "CRITICAL", "ALERT", "EMERGENCY"}
    )
    severe_ratio = severe / total
    if severe_ratio >= 0.25:
        hints.append(
            f"High severe-log ratio ({severe_ratio:.0%}); prioritize stabilizing error paths before scaling traffic."
        )

    if most_common_patterns and most_common_patterns[0][1] >= 3:
        top_count = most_common_patterns[0][1]
        hints.append(
            "Most frequent message pattern repeats "
            f"{top_count} times; consider deduplication/sampling and shared remediation guidance."
        )

    timeout_hits = keyword_counts.get("timeout", 0) + keyword_counts.get("timed", 0)
    if timeout_hits >= 3:
        hints.append("Timeout-related logs are common; review dependency latency, retry budgets, and circuit breakers.")

    if service_stats:
        top_service, top_service_count = next(iter(service_stats.items()))
        if int(top_service_count) / total >= 0.6:
            hints.append(
                f"Most events come from {top_service}; investigate service-level hotspots and noisy call paths."
            )

    return {
        "top_patterns": top_patterns,
        "top_keywords": top_keywords,
        "error_families": error_families,
        "hints": hints,
    }


# ---------------------------------------------------------------------------
# Web UI – Logs
# ---------------------------------------------------------------------------
@app.route("/logs")
@require_basic_auth
async def view_logs():
    db = get_db()
    q = request.args.get("q", "").strip()
    level = request.args.get("level", "").strip().upper()
    service = request.args.get("service", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    sql_where = request.args.get("sql", "").strip()
    run_advanced_analysis = request.args.get("analyze", "").strip() == "1"
    limit = _parse_limit(200)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {"Timestamp": "Timestamp", "SeverityText": "SeverityText", "ServiceName": "ServiceName"},
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    rows = []
    log_rows = []
    total = 0
    error_msg = ""
    level_stats: dict = {}
    service_stats: dict = {}
    advanced_analysis = None
    stats_generated_at_iso = ""
    stats_generated_at_display = ""
    stats_generated_age_s = 0
    where = ""
    params: list = []

    if time_error:
        error_msg = time_error

    if q:
        try:
            re.compile(q, re.IGNORECASE)
        except re.error as exc:
            error_msg = f"Regex error: {exc}"

    if error_msg:
        pass
    elif sql_where:
        # Allow raw WHERE clause (SQL search)
        try:
            safe_sql = sql_where.replace(";", "")
            safe_sql = re.sub(r"\blevel\b", "SeverityText", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bservice\b", "ServiceName", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\btrace_id\b", "TraceId", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bspan_id\b", "SpanId", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bts\b", "Timestamp", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bbody\b", "Body", safe_sql, flags=re.IGNORECASE)

            # Translate has_tag('key', 'value') to a correlated subquery.
            # Supports SQL-escaped quotes inside key/value (e.g. O''Reilly).
            def _translate_has_tag(m: re.Match) -> str:
                tag_key = m.group(1).replace("''", "'").replace("'", "''")
                tag_val = m.group(2).replace("''", "'").replace("'", "''")
                return (
                    "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN ("
                    "SELECT RecordId FROM sobs_record_tags FINAL "
                    f"WHERE TagKey='{tag_key}' AND TagValue='{tag_val}' "
                    "AND IsDeleted=0 AND RecordType='log')"
                )

            safe_sql = re.sub(
                r"has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)",
                _translate_has_tag,
                safe_sql,
                flags=re.IGNORECASE,
            )
            where = f"WHERE {safe_sql}"
            time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
            if time_conditions:
                where = f"{where} AND " + " AND ".join(time_conditions)
                params.extend(time_params)
        except Exception as exc:
            error_msg = f"SQL error: {_public_dashboard_query_error(exc)}"
    else:
        conditions = []
        params = []
        if level:
            conditions.append("SeverityText=?")
            params.append(level)
        if service:
            conditions.append("ServiceName=?")
            params.append(service)
        time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
        conditions.extend(time_conditions)
        params.extend(time_params)
        where = ("WHERE " + " AND ".join(conditions)) if conditions else ""

    if not error_msg:
        try:
            query_where = where
            query_params = list(params)
            if q:
                query_where = f"{query_where} AND match(Body, ?)" if query_where else "WHERE match(Body, ?)"
                query_params.append(q)

            select_base = (
                "SELECT Timestamp, SeverityText, ServiceName, Body, TraceId, SpanId " f"FROM otel_logs {query_where} "
            )

            total = db.execute(f"SELECT COUNT(*) FROM otel_logs {query_where}", query_params).fetchone()[0]
            rows = db.execute(
                f"{select_base}{order_clause} LIMIT ? OFFSET ?",
                query_params + [limit, offset],
            ).fetchall()
            level_stats, service_stats = _compute_log_stats(db, query_where, query_params)
            if run_advanced_analysis:
                analysis_rows = db.execute(
                    f"SELECT SeverityText, ServiceName, Body, LogAttributes FROM otel_logs {query_where}",
                    query_params,
                ).fetchall()
                advanced_analysis = _compute_advanced_log_analysis(analysis_rows, level_stats, service_stats)

            generated_at = datetime.now(timezone.utc)
            snapshot_raw = db.execute(f"SELECT max(Timestamp) FROM otel_logs {query_where}", query_params).fetchone()[0]
            snapshot_at = generated_at
            if snapshot_raw is not None:
                if isinstance(snapshot_raw, datetime):
                    snapshot_at = snapshot_raw
                else:
                    parsed = datetime.fromisoformat(str(snapshot_raw).replace("Z", "+00:00"))
                    snapshot_at = parsed
                if snapshot_at.tzinfo is None:
                    snapshot_at = snapshot_at.replace(tzinfo=timezone.utc)
                else:
                    snapshot_at = snapshot_at.astimezone(timezone.utc)

            stats_generated_at_iso = snapshot_at.isoformat()
            stats_generated_at_display = snapshot_at.strftime("%Y-%m-%d %H:%M:%S UTC")
            stats_generated_age_s = max(0, int((generated_at - snapshot_at).total_seconds()))
        except Exception as exc:
            if sql_where:
                error_msg = f"SQL error: {_public_dashboard_query_error(exc)}"
            else:
                error_msg = f"Query error: {exc}"
            rows = []
            total = 0
            level_stats = {}
            service_stats = {}
            advanced_analysis = None

    # Compute record IDs for visible rows so tags can be batch-fetched
    row_record_ids = [
        _record_id_for_log(str(r["Timestamp"]), str(r["ServiceName"]), str(r["TraceId"]), str(r["SpanId"]))
        for r in rows
    ]
    # Batch-fetch tags for all visible rows in one query
    tags_by_record_id: dict[str, list[dict]] = {}
    tag_stats_count: dict[tuple[str, str], int] = {}
    if row_record_ids:
        try:
            placeholders = ",".join(["?"] * len(row_record_ids))
            tag_rows_raw = db.execute(
                f"SELECT RecordId, TagKey, TagValue, IsAuto "
                f"FROM sobs_record_tags FINAL "
                f"WHERE RecordType='log' AND RecordId IN ({placeholders}) AND IsDeleted=0 "
                f"ORDER BY RecordId, TagKey",
                row_record_ids,
            ).fetchall()
            for tr in tag_rows_raw:
                rid = str(tr["RecordId"])
                entry = {"key": str(tr["TagKey"]), "value": str(tr["TagValue"]), "is_auto": bool(tr["IsAuto"])}
                tags_by_record_id.setdefault(rid, []).append(entry)
                tag_key = str(tr["TagKey"])
                tag_value = str(tr["TagValue"])
                stats_key = (tag_key, tag_value)
                tag_stats_count[stats_key] = tag_stats_count.get(stats_key, 0) + 1
        except Exception:
            pass  # Tags are supplementary; ignore failures

    tag_stats = [
        {"key": k, "value": v, "count": cnt}
        for (k, v), cnt in sorted(tag_stats_count.items(), key=lambda item: (-item[1], item[0][0], item[0][1]))
    ]

    for r in rows:
        body = r["Body"]
        rid = _record_id_for_log(str(r["Timestamp"]), str(r["ServiceName"]), str(r["TraceId"]), str(r["SpanId"]))
        log_rows.append(
            {
                "ts": str(r["Timestamp"]),
                "level": r["SeverityText"],
                "service": r["ServiceName"],
                "body": body,
                "trace_id": r["TraceId"],
                "span_id": r["SpanId"],
                "record_id": rid,
                "tags": tags_by_record_id.get(rid, []),
            }
        )

    services = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' ORDER BY ServiceName"
        ).fetchall()
    ]
    levels = [
        row[0] for row in db.execute("SELECT DISTINCT SeverityText FROM otel_logs ORDER BY SeverityText").fetchall()
    ]

    return await render_template(
        "logs.html",
        logs=log_rows,
        total=total,
        limit=limit,
        offset=offset,
        q=q,
        level=level,
        service=service,
        sql_where=sql_where,
        from_ts=from_ts,
        to_ts=to_ts,
        services=services,
        levels=levels,
        error_msg=error_msg,
        sort_by=sort_by,
        sort_dir=sort_dir,
        run_advanced_analysis=run_advanced_analysis,
        level_stats=level_stats,
        service_stats=service_stats,
        tag_stats=tag_stats,
        advanced_analysis=advanced_analysis,
        stats_generated_at_iso=stats_generated_at_iso,
        stats_generated_at_display=stats_generated_at_display,
        stats_generated_age_s=stats_generated_age_s,
    )


# ---------------------------------------------------------------------------
# Derived Signals / Rules Helpers
# ---------------------------------------------------------------------------
_ANOMALY_SEVERITY_RANK = {"normal": 0, "warning": 1, "outlier": 2}


def _list_derived_signal_dimensions(db: ChDbConnection) -> tuple[list[str], list[str], list[str]]:
    services = [
        row[0]
        for row in db.execute("SELECT DISTINCT ServiceName FROM v_derived_signals_1m ORDER BY ServiceName").fetchall()
    ]
    signals = [
        row[0]
        for row in db.execute("SELECT DISTINCT SignalName FROM v_derived_signals_1m ORDER BY SignalName").fetchall()
    ]
    sources = [
        row[0]
        for row in db.execute("SELECT DISTINCT SignalSource FROM v_derived_signals_1m ORDER BY SignalSource").fetchall()
    ]
    return services, signals, sources


_AUTO_RULE_GT_HINTS = (
    "error",
    "latency",
    "duration",
    "timeout",
    "p95",
    "p99",
    "failure",
    "fail",
    "retry",
)
_AUTO_RULE_LT_HINTS = ("availability", "success", "throughput", "rps", "qps")
_AUTO_RULE_CREATE_MAX = 200
_AUTO_DASHBOARD_CREATE_MAX = 24
_AUTO_TAG_RULE_CREATE_MAX = 200


def _infer_auto_rule_comparator(signal_name: str) -> str:
    name = signal_name.lower()
    if any(token in name for token in _AUTO_RULE_LT_HINTS):
        return "lt"
    if any(token in name for token in _AUTO_RULE_GT_HINTS):
        return "gt"
    return "gt"


def _auto_rule_thresholds(
    comparator: str, q05: float, q20: float, q50: float, q80: float, q95: float
) -> tuple[float, float]:
    if comparator == "lt":
        warning = q20
        critical = q05
        if critical > warning:
            critical = min(warning, q50)
        if critical == warning:
            critical = warning * 0.9 if warning != 0 else -0.1
        return warning, critical

    warning = q80
    critical = q95
    if critical < warning:
        critical = max(warning, q50)
    if critical == warning:
        critical = warning * 1.1 if warning != 0 else 0.1
    return warning, critical


def _format_auto_rule_name(source: str, signal: str, service: str, attr_fp: str) -> str:
    suffix = service or "any"
    if attr_fp:
        suffix = f"{suffix} / {attr_fp}"
    return f"Auto {source}/{signal} [{suffix}]"


def _build_auto_metric_rule_candidates(
    db: ChDbConnection,
    *,
    hours: int,
    min_points: int,
    service_filter: str = "",
    include_attr_fp: bool = False,
) -> tuple[list[dict[str, object]], dict[str, int]]:
    where_parts: list[str] = ["time >= now() - INTERVAL ? HOUR"]
    params: list[object] = [hours]
    if service_filter:
        where_parts.append("ServiceName = ?")
        params.append(service_filter)

    where_sql = " WHERE " + " AND ".join(where_parts)
    attr_select = "AttrFingerprint" if include_attr_fp else "''"
    attr_group = ", AttrFingerprint" if include_attr_fp else ""
    stats_rows = db.execute(
        "SELECT ServiceName, SignalSource, SignalName, "
        f"{attr_select} AS AttrFingerprint, "
        "count() AS point_count, "
        "quantile(0.05)(toFloat64(value)) AS q05, "
        "quantile(0.20)(toFloat64(value)) AS q20, "
        "quantile(0.50)(toFloat64(value)) AS q50, "
        "quantile(0.80)(toFloat64(value)) AS q80, "
        "quantile(0.95)(toFloat64(value)) AS q95 "
        "FROM v_derived_signals_anomaly"
        f"{where_sql}"
        " GROUP BY ServiceName, SignalSource, SignalName"
        f"{attr_group}"
        " HAVING point_count >= ?"
        " ORDER BY point_count DESC",
        params + [min_points],
    ).fetchall()

    active_rules = _load_anomaly_rules(db)
    existing_series = {
        (
            str(rule.get("source", "")),
            str(rule.get("signal", "")),
            str(rule.get("service", "")),
            str(rule.get("attr_fp", "")),
        )
        for rule in active_rules
    }

    created_candidates: list[dict[str, object]] = []
    skipped_existing = 0
    skipped_invalid = 0
    for row in stats_rows:
        service = str(row["ServiceName"])
        source = str(row["SignalSource"])
        signal = str(row["SignalName"])
        attr_fp = str(row["AttrFingerprint"])
        key = (source, signal, service, attr_fp)
        if key in existing_series:
            skipped_existing += 1
            continue

        point_count = int(row["point_count"])
        q05 = float(row["q05"])
        q20 = float(row["q20"])
        q50 = float(row["q50"])
        q80 = float(row["q80"])
        q95 = float(row["q95"])
        comparator = _infer_auto_rule_comparator(signal)
        warning, critical = _auto_rule_thresholds(comparator, q05, q20, q50, q80, q95)

        if comparator == "gt" and critical < warning:
            skipped_invalid += 1
            continue
        if comparator == "lt" and critical > warning:
            skipped_invalid += 1
            continue

        created_candidates.append(
            {
                "name": _format_auto_rule_name(source, signal, service, attr_fp),
                "rule_type": "threshold",
                "source": source,
                "signal": signal,
                "service": service,
                "attr_fp": attr_fp,
                "comparator": comparator,
                "warning_threshold": warning,
                "critical_threshold": critical,
                "min_sample_count": 3,
                "point_count": point_count,
            }
        )

    return created_candidates, {
        "examined": len(stats_rows),
        "existing": skipped_existing,
        "invalid": skipped_invalid,
    }


def _default_auto_dashboard_name(service_filter: str) -> str:
    if service_filter:
        return f"Auto Metric Rules - {service_filter}"
    return "Auto Metric Rules Dashboard"


def _auto_tag_slug(value: str, fallback: str, max_len: int = 64) -> str:
    raw = str(value or "").strip().lower()
    slug = re.sub(r"[^a-z0-9]+", "_", raw).strip("_")
    if not slug:
        slug = fallback
    return slug[:max_len]


def _infer_env_from_service(service_name: str) -> str:
    name = str(service_name or "").strip().lower()
    if not name:
        return ""
    if re.search(r"(^|[-_\.])(prod|production)($|[-_\.])", name):
        return "production"
    if re.search(r"(^|[-_\.])(stg|stage|staging)($|[-_\.])", name):
        return "staging"
    if re.search(r"(^|[-_\.])(dev|development)($|[-_\.])", name):
        return "development"
    if re.search(r"(^|[-_\.])(qa|test|testing|uat)($|[-_\.])", name):
        return "test"
    return ""


def _list_tag_candidate_services(db: ChDbConnection) -> list[str]:
    rows = db.execute(
        "SELECT DISTINCT ServiceName FROM ("
        "  SELECT ServiceName FROM otel_logs "
        "  UNION DISTINCT SELECT ServiceName FROM otel_traces "
        "  UNION DISTINCT SELECT ServiceName FROM hyperdx_sessions"
        ") WHERE ServiceName != '' ORDER BY ServiceName"
    ).fetchall()
    return [str(row[0]) for row in rows]


def _build_auto_tag_rule_candidates(
    db: ChDbConnection,
    *,
    hours: int,
    min_count: int,
    service_filter: str = "",
    record_types: list[str] | None = None,
) -> tuple[list[dict[str, object]], dict[str, int]]:
    selected = set(record_types or ["log", "trace", "error", "ai", "rum"])
    selected &= {"log", "trace", "error", "ai", "rum"}
    if not selected:
        selected = {"log", "trace", "error", "ai", "rum"}

    existing_rules = _load_tag_rules(db)
    existing_keys = {
        (
            ",".join(sorted([str(t).strip() for t in rule.get("record_types", []) if str(t).strip()])),
            str(rule.get("match_field", "")),
            str(rule.get("match_operator", "")),
            str(rule.get("match_value", "")),
            str(rule.get("match_attr_key", "")),
            str(rule.get("tag_key", "")),
            str(rule.get("tag_value", "")),
        )
        for rule in existing_rules
    }

    candidates: list[dict[str, object]] = []
    examined = 0
    skipped_existing = 0
    skipped_invalid = 0

    def _append_candidate(
        *,
        record_type: str,
        name: str,
        match_field: str,
        match_operator: str,
        match_value: str,
        tag_key: str,
        tag_value: str,
        point_count: int,
        match_attr_key: str = "",
    ) -> None:
        nonlocal skipped_existing, skipped_invalid
        if not match_value.strip() or not tag_key.strip() or not tag_value.strip():
            skipped_invalid += 1
            return
        rule_key = (
            record_type,
            match_field,
            match_operator,
            match_value,
            match_attr_key,
            tag_key,
            tag_value,
        )
        if rule_key in existing_keys:
            skipped_existing += 1
            return
        candidates.append(
            {
                "name": name,
                "record_types": [record_type],
                "match_field": match_field,
                "match_operator": match_operator,
                "match_value": match_value,
                "match_attr_key": match_attr_key,
                "tag_key": tag_key,
                "tag_value": tag_value,
                "point_count": point_count,
            }
        )

    where_service = " AND ServiceName = ?" if service_filter else ""
    base_params: list[object] = [hours]
    if service_filter:
        base_params.append(service_filter)

    if "log" in selected:
        rows = db.execute(
            "SELECT ServiceName, count() AS c FROM otel_logs "
            "WHERE Timestamp >= now() - INTERVAL ? HOUR AND ServiceName != ''"
            f"{where_service} "
            "GROUP BY ServiceName HAVING c >= ? ORDER BY c DESC",
            base_params + [min_count],
        ).fetchall()
        examined += len(rows)
        for row in rows:
            service = str(row["ServiceName"])
            count = int(row["c"])
            inferred_env = _infer_env_from_service(service)
            if inferred_env:
                _append_candidate(
                    record_type="log",
                    name=f"log env={inferred_env}",
                    match_field="service_name",
                    match_operator="contains",
                    match_value=service,
                    tag_key="env",
                    tag_value=inferred_env,
                    point_count=count,
                )
                continue
            _append_candidate(
                record_type="log",
                name=f"log service={service}",
                match_field="service_name",
                match_operator="eq",
                match_value=service,
                tag_key="service",
                tag_value=service,
                point_count=count,
            )

    if "trace" in selected:
        rows = db.execute(
            "SELECT ServiceName, count() AS c FROM otel_traces "
            "WHERE Timestamp >= now() - INTERVAL ? HOUR AND ScopeName != 'sobs-ai' AND ServiceName != ''"
            f"{where_service} "
            "GROUP BY ServiceName HAVING c >= ? ORDER BY c DESC",
            base_params + [min_count],
        ).fetchall()
        examined += len(rows)
        for row in rows:
            service = str(row["ServiceName"])
            count = int(row["c"])
            inferred_env = _infer_env_from_service(service)
            if inferred_env:
                _append_candidate(
                    record_type="trace",
                    name=f"trace env={inferred_env}",
                    match_field="service_name",
                    match_operator="contains",
                    match_value=service,
                    tag_key="env",
                    tag_value=inferred_env,
                    point_count=count,
                )
                continue
            _append_candidate(
                record_type="trace",
                name=f"trace service={service}",
                match_field="service_name",
                match_operator="eq",
                match_value=service,
                tag_key="service",
                tag_value=service,
                point_count=count,
            )

    if "error" in selected:
        rows = db.execute(
            "SELECT coalesce(LogAttributes['exception.type'], '') AS ExceptionType, count() AS c "
            "FROM otel_logs "
            "WHERE Timestamp >= now() - INTERVAL ? HOUR "
            "AND (EventName = 'exception' OR SeverityNumber >= 17 OR SeverityText IN ('ERROR','CRITICAL','FATAL'))"
            f"{where_service} "
            "GROUP BY ExceptionType HAVING c >= ? ORDER BY c DESC",
            base_params + [min_count],
        ).fetchall()
        examined += len(rows)
        for row in rows:
            exception_type = str(row["ExceptionType"] or "").strip()
            if not exception_type:
                skipped_invalid += 1
                continue
            count = int(row["c"])
            _append_candidate(
                record_type="error",
                name=f"error type={_auto_tag_slug(exception_type, 'error')}",
                match_field="attribute",
                match_operator="eq",
                match_value=exception_type,
                match_attr_key="exception.type",
                tag_key="error_type",
                tag_value=_auto_tag_slug(exception_type, "error"),
                point_count=count,
            )

    if "ai" in selected:
        rows = db.execute(
            "SELECT coalesce(SpanAttributes['gen_ai.provider.name'], '') AS Provider, count() AS c "
            "FROM otel_traces "
            "WHERE Timestamp >= now() - INTERVAL ? HOUR AND ScopeName = 'sobs-ai'"
            f"{where_service} "
            "GROUP BY Provider HAVING c >= ? ORDER BY c DESC",
            base_params + [min_count],
        ).fetchall()
        examined += len(rows)
        for row in rows:
            provider = str(row["Provider"] or "").strip()
            if not provider:
                skipped_invalid += 1
                continue
            count = int(row["c"])
            _append_candidate(
                record_type="ai",
                name=f"ai provider={_auto_tag_slug(provider, 'provider')}",
                match_field="attribute",
                match_operator="eq",
                match_value=provider,
                match_attr_key="gen_ai.provider.name",
                tag_key="ai_provider",
                tag_value=_auto_tag_slug(provider, "provider"),
                point_count=count,
            )

    if "rum" in selected:
        rows = db.execute(
            "SELECT EventName, count() AS c FROM hyperdx_sessions "
            "WHERE Timestamp >= now() - INTERVAL ? HOUR AND EventName != ''"
            f"{where_service} "
            "GROUP BY EventName HAVING c >= ? ORDER BY c DESC",
            base_params + [min_count],
        ).fetchall()
        examined += len(rows)
        for row in rows:
            event_name = str(row["EventName"])
            count = int(row["c"])
            _append_candidate(
                record_type="rum",
                name=f"rum event={_auto_tag_slug(event_name, 'event')}",
                match_field="event_type",
                match_operator="eq",
                match_value=event_name,
                tag_key="rum_event",
                tag_value=_auto_tag_slug(event_name, "event"),
                point_count=count,
            )

    def _candidate_point_count(candidate: dict[str, object]) -> int:
        raw = candidate.get("point_count", 0)
        try:
            return int(str(raw))
        except (TypeError, ValueError):
            return 0

    candidates.sort(
        key=lambda c: (_candidate_point_count(c), str(c.get("name", ""))),
        reverse=True,
    )
    return candidates, {
        "examined": examined,
        "existing": skipped_existing,
        "invalid": skipped_invalid,
    }


def _build_auto_dashboard_chart_candidates(
    rules: list[dict[str, object]],
    *,
    service_filter: str,
    hours: int,
) -> list[dict[str, object]]:
    candidates: list[dict[str, object]] = []
    title_counts: dict[str, int] = {}
    for rule in rules:
        source = str(rule.get("source", "")).strip()
        signal = str(rule.get("signal", "")).strip()
        if not source or not signal:
            continue

        rule_service = str(rule.get("service", "")).strip()
        if service_filter and rule_service and rule_service != service_filter:
            continue

        attr_fp = str(rule.get("attr_fp", "")).strip()
        where_parts = [
            f"SignalSource = {_sql_literal(source)}",
            f"SignalName = {_sql_literal(signal)}",
            f"time >= now() - INTERVAL {hours} HOUR",
        ]
        if rule_service:
            where_parts.append(f"ServiceName = {_sql_literal(rule_service)}")
        if attr_fp:
            where_parts.append(f"AttrFingerprint = {_sql_literal(attr_fp)}")

        sql = (
            "SELECT time, "
            "ServiceName AS service, "
            "SignalSource AS source, "
            "SignalName AS signal, "
            "AttrFingerprint AS attr_fp, "
            "value, "
            "SampleCount AS sample_count, "
            "baseline_mean, "
            "baseline_lower, "
            "baseline_upper, "
            "anomaly_state, "
            "anomaly_score "
            "FROM v_derived_signals_anomaly "
            f"WHERE {' AND '.join(where_parts)} "
            "ORDER BY time"
        )

        base_title = str(rule.get("name", "")).strip() or f"{source}/{signal}"
        title_index = title_counts.get(base_title, 0)
        title_counts[base_title] = title_index + 1
        title = base_title if title_index == 0 else f"{base_title} ({title_index + 1})"

        candidates.append(
            {
                "title": title,
                "rule_name": str(rule.get("name", "")),
                "rule_type": str(rule.get("rule_type", "threshold")),
                "source": source,
                "signal": signal,
                "service": rule_service,
                "attr_fp": attr_fp,
                "chart_type": "derived_signal_overlay",
                "query": sql,
            }
        )

    candidates.sort(
        key=lambda item: (
            str(item.get("service", "")),
            str(item.get("source", "")),
            str(item.get("signal", "")),
            str(item.get("title", "")),
        )
    )
    return candidates


def _load_anomaly_rules(db: ChDbConnection) -> list[dict[str, object]]:
    rows = db.execute(
        "SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, "
        "WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, "
        "SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount "
        "FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY Name"
    ).fetchall()
    return [
        {
            "id": str(row["Id"]),
            "name": str(row["Name"]),
            "rule_type": str(row["RuleType"] or "threshold"),
            "source": str(row["SignalSource"]),
            "signal": str(row["SignalName"]),
            "service": str(row["ServiceName"]),
            "attr_fp": str(row["AttrFingerprint"]),
            "comparator": str(row["Comparator"]),
            "warning_threshold": float(row["WarningThreshold"]),
            "critical_threshold": float(row["CriticalThreshold"]),
            "secondary_source": str(row["SecondarySignalSource"]),
            "secondary_signal": str(row["SecondarySignalName"]),
            "secondary_comparator": str(row["SecondaryComparator"] or "gt"),
            "secondary_warning_threshold": float(row["SecondaryWarningThreshold"]),
            "secondary_critical_threshold": float(row["SecondaryCriticalThreshold"]),
            "min_sample_count": int(row["MinSampleCount"]),
        }
        for row in rows
    ]


# ---------------------------------------------------------------------------
# Tag rules helpers
# ---------------------------------------------------------------------------

_TAG_RULE_FIELDS = ("service_name", "severity", "body", "span_name", "event_type", "attribute")
_TAG_RULE_OPERATORS = ("eq", "contains", "regex")
_TAG_RULE_RECORD_TYPES = ("log", "trace", "error", "ai", "rum", "all")


def _record_id_for_log(ts: str, service: str, trace_id: str, span_id: str) -> str:
    """Compute a stable record ID for a log/rum/error event."""
    key = f"{service}|{ts}|{trace_id}|{span_id}"
    return hashlib.md5(key.encode()).hexdigest()


def _record_id_for_span(trace_id: str, span_id: str) -> str:
    """Compute a stable record ID for a trace span."""
    key = f"{trace_id}|{span_id}"
    return hashlib.md5(key.encode()).hexdigest()


def _load_tag_rules(db: ChDbConnection) -> list[dict]:
    """Load all active tag rules."""
    rows = db.execute(
        "SELECT Id, Name, RecordTypes, MatchField, MatchOperator, MatchValue, "
        "MatchAttrKey, TagKey, TagValue "
        "FROM sobs_tag_rules FINAL WHERE IsDeleted = 0 ORDER BY Name"
    ).fetchall()
    return [
        {
            "id": str(row["Id"]),
            "name": str(row["Name"]),
            "record_types": [t.strip() for t in str(row["RecordTypes"]).split(",") if t.strip()],
            "match_field": str(row["MatchField"]),
            "match_operator": str(row["MatchOperator"]),
            "match_value": str(row["MatchValue"]),
            "match_attr_key": str(row["MatchAttrKey"]),
            "tag_key": str(row["TagKey"]),
            "tag_value": str(row["TagValue"]),
        }
        for row in rows
    ]


def _match_tag_rule(
    rule: dict,
    record_type: str,
    service: str,
    severity: str,
    body: str,
    attrs: dict,
    span_name: str = "",
    event_type: str = "",
) -> bool:
    """Return True if the tag rule matches the given record fields."""
    rule_types = rule["record_types"]
    if rule_types and "all" not in rule_types and record_type not in rule_types:
        return False

    field = rule["match_field"]
    if field == "service_name":
        value = service
    elif field == "severity":
        value = severity
    elif field == "body":
        value = body
    elif field == "span_name":
        value = span_name
    elif field == "event_type":
        value = event_type
    elif field == "attribute":
        value = str(attrs.get(rule["match_attr_key"], "")) if isinstance(attrs, dict) else ""
    else:
        value = ""

    operator = rule["match_operator"]
    match_value = rule["match_value"]
    if operator == "eq":
        return value == match_value
    if operator == "contains":
        return match_value.lower() in value.lower()
    if operator == "regex":
        try:
            return bool(re.search(match_value, value))
        except re.error:
            return False
    return False


def _apply_tag_rules(
    db: ChDbConnection,
    record_type: str,
    rows_data: list[dict],
    rules: list[dict],
) -> None:
    """Apply tag rules to ingested rows and write matching tags to sobs_record_tags."""
    if not rules or not rows_data:
        return
    tag_rows = []
    version = int(time.time() * 1000)
    for row in rows_data:
        service = str(row.get("ServiceName", "") or "")
        severity = str(row.get("SeverityText", "") or "")
        body = str(row.get("Body", "") or "")
        attrs = row.get("LogAttributes") or row.get("SpanAttributes") or {}
        if not isinstance(attrs, dict):
            attrs = {}
        span_name = str(row.get("SpanName", "") or "")
        event_type = str(row.get("EventName", "") or "")
        trace_id = str(row.get("TraceId", "") or "")
        span_id = str(row.get("SpanId", "") or "")
        ts = str(row.get("Timestamp", "") or "")

        if record_type in ("trace", "ai"):
            record_id = _record_id_for_span(trace_id, span_id)
        else:
            record_id = _record_id_for_log(ts, service, trace_id, span_id)

        # Keep one value per tag key per record. If multiple rules match the same
        # key, last matching rule wins (deterministic by rule order).
        matched_by_key: dict[str, str] = {}
        for rule in rules:
            if _match_tag_rule(rule, record_type, service, severity, body, attrs, span_name, event_type):
                matched_by_key[str(rule["tag_key"])] = str(rule["tag_value"])
        for tag_key, tag_value in matched_by_key.items():
            tag_rows.append(
                {
                    "RecordType": record_type,
                    "RecordId": record_id,
                    "TagKey": tag_key,
                    "TagValue": tag_value,
                    "IsAuto": 1,
                    "IsDeleted": 0,
                    "Version": version,
                }
            )
            version += 1
    if tag_rows:
        _insert_rows_json_each_row(db, "sobs_record_tags", tag_rows)


def _get_record_tags(db: ChDbConnection, record_type: str, record_id: str) -> list[dict]:
    """Return all active tags for a given record."""
    rows = db.execute(
        "SELECT TagKey, TagValue, IsAuto "
        "FROM sobs_record_tags FINAL "
        "WHERE RecordType = ? AND RecordId = ? AND IsDeleted = 0 "
        "ORDER BY TagKey",
        [record_type, record_id],
    ).fetchall()
    return [
        {
            "key": str(row["TagKey"]),
            "value": str(row["TagValue"]),
            "is_auto": bool(row["IsAuto"]),
        }
        for row in rows
    ]


def _get_service_tags(db: ChDbConnection, record_type: str, service: str, hours: int = 24) -> list[str]:
    """Return distinct tag values applied to a service's records in the last N hours."""
    try:
        rows = db.execute(
            "SELECT DISTINCT concat(rt.TagKey, ':', rt.TagValue) AS tag "
            "FROM sobs_record_tags rt FINAL "
            "WHERE rt.RecordType = ? AND rt.IsDeleted = 0 "
            "AND rt.RecordId IN ("
            "  SELECT MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) "
            "  FROM otel_logs "
            "  WHERE ServiceName = ? AND Timestamp >= now() - INTERVAL ? HOUR "
            ") "
            "ORDER BY tag",
            [record_type, service, hours],
        ).fetchall()
        return [str(r["tag"]) for r in rows]
    except Exception:
        return []


def _get_def_tags_for_service(db: ChDbConnection, service: str) -> list[str]:
    """Return distinct auto-tags for a service from all record types (last 24 h)."""
    try:
        rows = db.execute(
            "SELECT DISTINCT concat(TagKey,'=',TagValue) AS tag "
            "FROM sobs_record_tags FINAL "
            "WHERE IsDeleted = 0 "
            "AND RecordId IN ("
            "  SELECT MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) "
            "  FROM otel_logs WHERE ServiceName = ? AND Timestamp >= now() - INTERVAL 24 HOUR"
            ") ORDER BY tag",
            [service],
        ).fetchall()
        return [str(r["tag"]) for r in rows]
    except Exception:
        return []


def _get_signal_health_by_service(db: ChDbConnection, hours: int = 24) -> list[dict[str, object]]:
    """Return worst effective_state per service for derived signals in the last `hours` hours."""
    try:
        rows = db.execute(
            "SELECT ServiceName, SignalSource, SignalName, AttrFingerprint, "
            "argMax(value, time) AS value, argMax(SampleCount, time) AS SampleCount "
            "FROM v_derived_signals_anomaly "
            "WHERE time >= now() - INTERVAL ? HOUR "
            "GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint",
            [hours],
        ).fetchall()
    except Exception:
        return []
    if not rows:
        return []
    dicts = [dict(r) for r in rows]
    rules = _load_anomaly_rules(db)
    _annotate_rows_with_rules(
        dicts,
        rules,
        source_key="SignalSource",
        signal_key="SignalName",
        service_key="ServiceName",
        attr_fp_key="AttrFingerprint",
        value_key="value",
        sample_count_key="SampleCount",
    )
    service_worst: dict[str, int] = {}
    service_count: dict[str, int] = {}
    for row in dicts:
        svc = str(row["ServiceName"])
        rank = _ANOMALY_SEVERITY_RANK.get(str(row.get("effective_state", "normal")), 0)
        service_worst[svc] = max(service_worst.get(svc, 0), rank)
        service_count[svc] = service_count.get(svc, 0) + 1
    rank_to_state = {v: k for k, v in _ANOMALY_SEVERITY_RANK.items()}
    return sorted(
        [
            {
                "service": svc,
                "worst_state": rank_to_state.get(service_worst[svc], "normal"),
                "signal_count": service_count[svc],
            }
            for svc in service_worst
        ],
        key=lambda x: (-_ANOMALY_SEVERITY_RANK.get(str(x["worst_state"]), 0), str(x["service"])),
    )


def _rule_matches_series(rule: dict[str, object], source: str, signal: str, service: str, attr_fp: str) -> bool:
    if str(rule.get("source", "")) != source:
        return False
    if str(rule.get("signal", "")) != signal:
        return False
    rule_service = str(rule.get("service", ""))
    if rule_service and rule_service != service:
        return False
    rule_attr_fp = str(rule.get("attr_fp", ""))
    if rule_attr_fp and rule_attr_fp != attr_fp:
        return False
    return True


def _evaluate_threshold_condition(
    name: str,
    comparator: str,
    warning_threshold: object,
    critical_threshold: object,
    value: object,
    sample_count: object,
    min_sample_count: object,
) -> dict[str, object] | None:
    try:
        value_num = float(str(value))
        sample_count_num = int(str(sample_count))
    except (TypeError, ValueError):
        return None

    min_samples = int(str(min_sample_count))
    if sample_count_num < min_samples:
        return None

    warning = float(str(warning_threshold))
    critical = float(str(critical_threshold))

    state = "normal"
    triggered_threshold = None
    if comparator == "gt":
        if value_num >= critical:
            state = "outlier"
            triggered_threshold = critical
        elif value_num >= warning:
            state = "warning"
            triggered_threshold = warning
    elif comparator == "lt":
        if value_num <= critical:
            state = "outlier"
            triggered_threshold = critical
        elif value_num <= warning:
            state = "warning"
            triggered_threshold = warning

    if state == "normal" or triggered_threshold is None:
        return None

    operator = ">=" if comparator == "gt" else "<="
    return {
        "rule_state": state,
        "rule_reason": f"{name}: value {round(value_num, 4)} {operator} {triggered_threshold}",
    }


def _evaluate_threshold_rule(rule: dict[str, object], value: object, sample_count: object) -> dict[str, object] | None:
    evaluation = _evaluate_threshold_condition(
        str(rule.get("name", "")),
        str(rule.get("comparator", "gt")),
        rule.get("warning_threshold", 0.0),
        rule.get("critical_threshold", 0.0),
        value,
        sample_count,
        rule.get("min_sample_count", 1),
    )
    if not evaluation:
        return None
    return {
        "rule_id": str(rule.get("id", "")),
        "rule_name": str(rule.get("name", "")),
        **evaluation,
    }


def _build_series_rule_lookups(
    rows: list[dict[str, object]],
    *,
    source_key: str,
    signal_key: str,
    service_key: str,
    attr_fp_key: str,
    time_key: str | None,
) -> tuple[dict[tuple[str, str, str, str], dict[str, object]], dict[tuple[str, str, str, str, str], dict[str, object]]]:
    latest_lookup: dict[tuple[str, str, str, str], dict[str, object]] = {}
    timed_lookup: dict[tuple[str, str, str, str, str], dict[str, object]] = {}
    for row in rows:
        base_key = (
            str(row.get(service_key, "")),
            str(row.get(attr_fp_key, "")),
            str(row.get(source_key, "")),
            str(row.get(signal_key, "")),
        )
        latest_lookup[base_key] = row
        if time_key:
            timed_lookup[base_key + (str(row.get(time_key, "")),)] = row
    return latest_lookup, timed_lookup


def _combine_rule_states(*states: str) -> str:
    ranked = max((_ANOMALY_SEVERITY_RANK.get(state, 0), state) for state in states)
    return ranked[1]


def _lookup_secondary_rule_row(
    service: str,
    attr_fp: str,
    secondary_source: str,
    secondary_signal: str,
    time_value: str,
) -> dict[str, object] | None:
    db = get_db()
    attr_filter = "AttrFingerprint = ?"
    params: list[object] = [service, secondary_source, secondary_signal, attr_fp]
    if time_value:
        row = db.execute(
            "SELECT time, value, SampleCount FROM v_derived_signals_anomaly "
            "WHERE ServiceName = ? AND SignalSource = ? AND SignalName = ? AND "
            f"{attr_filter} AND time = ? ORDER BY time DESC LIMIT 1",
            params + [time_value],
        ).fetchone()
        if row:
            return {"time": row["time"], "value": row["value"], "sample_count": row["SampleCount"]}
    row = db.execute(
        "SELECT time, value, SampleCount FROM v_derived_signals_anomaly "
        "WHERE ServiceName = ? AND SignalSource = ? AND SignalName = ? AND "
        f"{attr_filter} ORDER BY time DESC LIMIT 1",
        params,
    ).fetchone()
    if not row:
        return None
    return {"time": row["time"], "value": row["value"], "sample_count": row["SampleCount"]}


def _evaluate_composite_rule(
    rule: dict[str, object],
    row: dict[str, object],
    latest_lookup: dict[tuple[str, str, str, str], dict[str, object]],
    timed_lookup: dict[tuple[str, str, str, str, str], dict[str, object]],
    *,
    source_key: str,
    signal_key: str,
    service_key: str,
    attr_fp_key: str,
    value_key: str,
    sample_count_key: str,
    time_key: str | None,
) -> dict[str, object] | None:
    primary = _evaluate_threshold_condition(
        f"{rule.get('name', '')} primary",
        str(rule.get("comparator", "gt")),
        rule.get("warning_threshold", 0.0),
        rule.get("critical_threshold", 0.0),
        row.get(value_key),
        row.get(sample_count_key),
        rule.get("min_sample_count", 1),
    )
    if not primary:
        return None

    secondary_source = str(rule.get("secondary_source", ""))
    secondary_signal = str(rule.get("secondary_signal", ""))
    if not secondary_source or not secondary_signal:
        return None

    service = str(row.get(service_key, ""))
    attr_fp = str(row.get(attr_fp_key, ""))
    time_value = str(row.get(time_key, "")) if time_key else ""
    timed_key = (service, attr_fp, secondary_source, secondary_signal, time_value)
    secondary_row = timed_lookup.get(timed_key) if time_key else None
    if secondary_row is None:
        secondary_row = latest_lookup.get((service, attr_fp, secondary_source, secondary_signal))
    if secondary_row is None:
        secondary_row = _lookup_secondary_rule_row(
            service,
            attr_fp,
            secondary_source,
            secondary_signal,
            time_value,
        )
    if secondary_row is None:
        return None

    secondary = _evaluate_threshold_condition(
        f"{rule.get('name', '')} secondary",
        str(rule.get("secondary_comparator", "gt")),
        rule.get("secondary_warning_threshold", 0.0),
        rule.get("secondary_critical_threshold", 0.0),
        secondary_row.get(value_key, secondary_row.get("value")),
        secondary_row.get(sample_count_key, secondary_row.get("sample_count")),
        rule.get("min_sample_count", 1),
    )
    if not secondary:
        return None

    primary_state = str(primary.get("rule_state", "normal"))
    secondary_state = str(secondary.get("rule_state", "normal"))
    combined_state = _combine_rule_states(primary_state, secondary_state)
    secondary_value = secondary_row.get(value_key)
    return {
        "rule_id": str(rule.get("id", "")),
        "rule_name": str(rule.get("name", "")),
        "rule_state": combined_state,
        "rule_reason": (
            f"{rule.get('name', '')}: primary {str(row.get(signal_key, ''))}={row.get(value_key)} and "
            f"secondary {secondary_signal}={secondary_value} triggered"
        ),
    }


def _annotate_rows_with_rules(
    rows: list[dict[str, object]],
    rules: list[dict[str, object]],
    *,
    source_key: str,
    signal_key: str,
    service_key: str,
    attr_fp_key: str,
    value_key: str,
    sample_count_key: str,
    time_key: str | None = None,
) -> None:
    latest_lookup, timed_lookup = _build_series_rule_lookups(
        rows,
        source_key=source_key,
        signal_key=signal_key,
        service_key=service_key,
        attr_fp_key=attr_fp_key,
        time_key=time_key,
    )
    for row in rows:
        row["rule_name"] = ""
        row["rule_state"] = "normal"
        row["rule_reason"] = ""
        row["effective_state"] = str(row.get("anomaly_state", "normal"))
        best_match: dict[str, object] | None = None
        best_rank = -1
        row_source = str(row.get(source_key, ""))
        row_signal = str(row.get(signal_key, ""))
        row_service = str(row.get(service_key, ""))
        row_attr_fp = str(row.get(attr_fp_key, ""))
        for rule in rules:
            if not _rule_matches_series(rule, row_source, row_signal, row_service, row_attr_fp):
                continue
            if str(rule.get("rule_type", "threshold")) == "composite":
                evaluation = _evaluate_composite_rule(
                    rule,
                    row,
                    latest_lookup,
                    timed_lookup,
                    source_key=source_key,
                    signal_key=signal_key,
                    service_key=service_key,
                    attr_fp_key=attr_fp_key,
                    value_key=value_key,
                    sample_count_key=sample_count_key,
                    time_key=time_key,
                )
            else:
                evaluation = _evaluate_threshold_rule(rule, row.get(value_key), row.get(sample_count_key))
            if not evaluation:
                continue
            rank = _ANOMALY_SEVERITY_RANK.get(str(evaluation.get("rule_state", "normal")), 0)
            if rank > best_rank:
                best_match = evaluation
                best_rank = rank
        if best_match:
            row.update(best_match)
        row["effective_state"] = _combine_rule_states(
            str(row.get("anomaly_state", "normal")),
            str(row.get("rule_state", "normal")),
        )


# ---------------------------------------------------------------------------
# Web UI – Metrics (derived signal index)
# ---------------------------------------------------------------------------
@app.route("/metrics")
@require_basic_auth
async def view_metrics():
    db = get_db()
    service = request.args.get("service", "").strip()
    signal = request.args.get("signal", "").strip()
    source = request.args.get("source", "").strip()
    attr_fp = request.args.get("attr_fp", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    limit = _parse_limit(100)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {
            "last_time": "last_time",
            "service": "service",
            "source": "source",
            "signal": "signal",
            "last_value": "last_value",
            "last_anomaly_score": "last_anomaly_score",
            "last_anomaly_state": "last_anomaly_state",
            "last_sample_count": "last_sample_count",
            "point_count": "point_count",
        },
        "last_time",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    try:
        hours = max(1, min(168, int(request.args.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24

    where_parts: list[str] = []
    params: list[str] = []
    if service:
        where_parts.append("ServiceName = ?")
        params.append(service)
    if signal:
        where_parts.append("SignalName = ?")
        params.append(signal)
    if source:
        where_parts.append("SignalSource = ?")
        params.append(source)
    if attr_fp:
        where_parts.append("AttrFingerprint = ?")
        params.append(attr_fp)

    if not time_error:
        time_conditions, time_params = _time_window_conditions("time", from_ts, to_ts)
        where_parts.extend(time_conditions)
        params.extend(time_params)

    hour_clause = ""
    if not from_ts and not to_ts:
        hour_clause = "time >= now() - INTERVAL ? HOUR"
        params.append(hours)

    where_clause = ""
    if where_parts:
        where_clause = " WHERE " + " AND ".join(where_parts)
    if hour_clause:
        where_clause = f"{where_clause} AND {hour_clause}" if where_clause else f" WHERE {hour_clause}"

    rows: list[dict] = []
    total = 0
    error_msg = time_error
    if not error_msg:
        try:
            grouped_sql = (
                "SELECT"
                "  ServiceName AS service,"
                "  SignalSource AS source,"
                "  SignalName AS signal,"
                "  AttrFingerprint AS attr_fp,"
                "  max(time) AS last_time,"
                "  argMax(value, time) AS last_value,"
                "  argMax(anomaly_score, time) AS last_anomaly_score,"
                "  argMax(anomaly_state, time) AS last_anomaly_state,"
                "  argMax(SampleCount, time) AS last_sample_count,"
                "  count() AS point_count"
                " FROM v_derived_signals_anomaly"
                f"{where_clause}"
                " GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint"
            )

            total = db.execute(f"SELECT COUNT(*) FROM ({grouped_sql})", params).fetchone()[0]
            fetched = db.execute(
                f"SELECT * FROM ({grouped_sql}) {order_clause} LIMIT ? OFFSET ?",
                params + [limit, offset],
            ).fetchall()
            for row in fetched:
                rows.append(
                    {
                        "service": str(row["service"]),
                        "source": str(row["source"]),
                        "signal": str(row["signal"]),
                        "attr_fp": str(row["attr_fp"]),
                        "last_time": str(row["last_time"]),
                        "last_value": row["last_value"],
                        "last_anomaly_score": row["last_anomaly_score"],
                        "last_anomaly_state": str(row["last_anomaly_state"]),
                        "last_sample_count": row["last_sample_count"],
                        "point_count": row["point_count"],
                        "rule_name": "",
                    }
                )
        except Exception as exc:
            app.logger.exception("metrics index query failed")
            error_msg = _public_dashboard_query_error(exc)

    _annotate_rows_with_rules(
        rows,
        _load_anomaly_rules(db),
        source_key="source",
        signal_key="signal",
        service_key="service",
        attr_fp_key="attr_fp",
        value_key="last_value",
        sample_count_key="last_sample_count",
        time_key="last_time",
    )

    services, signals, sources = _list_derived_signal_dimensions(db)

    return await render_template(
        "metrics.html",
        rows=rows,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        signal=signal,
        source=source,
        attr_fp=attr_fp,
        from_ts=from_ts,
        to_ts=to_ts,
        hours=hours,
        error_msg=error_msg,
        services=services,
        signals=signals,
        sources=sources,
        sort_by=sort_by,
        sort_dir=sort_dir,
    )


# ---------------------------------------------------------------------------
# Web UI – Metrics Rules
# ---------------------------------------------------------------------------
@app.route("/metrics/rules")
@require_basic_auth
async def view_metrics_rules():
    db = get_db()
    open_panel = (request.args.get("open_panel") or "").strip().lower()
    if open_panel not in {"auto-rules", "auto-dashboard"}:
        open_panel = ""
    services, signals, sources = _list_derived_signal_dimensions(db)
    rules = _load_anomaly_rules(db)
    return await render_template(
        "metrics_rules.html",
        rules=rules,
        services=services,
        signals=signals,
        sources=sources,
        auto_preview=[],
        auto_summary=None,
        auto_dashboard_preview=[],
        auto_dashboard_summary=None,
        auto_open_panel=open_panel,
    )


@app.route("/metrics/rules", methods=["POST"])
@require_basic_auth
async def create_metrics_rule():
    form = await request.form
    name = (form.get("name") or "").strip()
    rule_type = (form.get("rule_type") or "threshold").strip().lower()
    source = (form.get("source") or "").strip()
    signal = (form.get("signal") or "").strip()
    service = (form.get("service") or "").strip()
    attr_fp = (form.get("attr_fp") or "").strip()
    comparator = (form.get("comparator") or "gt").strip().lower()
    secondary_source = (form.get("secondary_source") or "").strip()
    secondary_signal = (form.get("secondary_signal") or "").strip()
    secondary_comparator = (form.get("secondary_comparator") or "gt").strip().lower()

    if not name or not source or not signal:
        await flash("Rule name, source, and signal are required", "warning")
        return redirect(url_for("view_metrics_rules"))

    if rule_type not in {"threshold", "composite"}:
        await flash("Rule type must be 'threshold' or 'composite'", "warning")
        return redirect(url_for("view_metrics_rules"))

    if comparator not in {"gt", "lt"}:
        await flash("Comparator must be 'gt' or 'lt'", "warning")
        return redirect(url_for("view_metrics_rules"))
    if secondary_comparator not in {"gt", "lt"}:
        await flash("Secondary comparator must be 'gt' or 'lt'", "warning")
        return redirect(url_for("view_metrics_rules"))

    try:
        warning_threshold = float(form.get("warning_threshold") or "")
        critical_threshold = float(form.get("critical_threshold") or "")
        min_sample_count = max(1, int(form.get("min_sample_count") or 1))
        secondary_warning_threshold = float(form.get("secondary_warning_threshold") or 0)
        secondary_critical_threshold = float(form.get("secondary_critical_threshold") or 0)
    except (TypeError, ValueError):
        await flash("Thresholds must be numeric and sample count must be an integer", "warning")
        return redirect(url_for("view_metrics_rules"))

    if comparator == "gt" and critical_threshold < warning_threshold:
        await flash("For 'gt' rules, critical threshold must be >= warning threshold", "warning")
        return redirect(url_for("view_metrics_rules"))
    if comparator == "lt" and critical_threshold > warning_threshold:
        await flash("For 'lt' rules, critical threshold must be <= warning threshold", "warning")
        return redirect(url_for("view_metrics_rules"))
    if rule_type == "composite":
        if not secondary_source or not secondary_signal:
            await flash("Composite rules require a secondary source and signal", "warning")
            return redirect(url_for("view_metrics_rules"))
        if secondary_comparator == "gt" and secondary_critical_threshold < secondary_warning_threshold:
            await flash("For secondary 'gt' rules, critical threshold must be >= warning threshold", "warning")
            return redirect(url_for("view_metrics_rules"))
        if secondary_comparator == "lt" and secondary_critical_threshold > secondary_warning_threshold:
            await flash("For secondary 'lt' rules, critical threshold must be <= warning threshold", "warning")
            return redirect(url_for("view_metrics_rules"))
    else:
        secondary_source = ""
        secondary_signal = ""
        secondary_comparator = "gt"
        secondary_warning_threshold = 0.0
        secondary_critical_threshold = 0.0

    rule_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        get_db(),
        "sobs_anomaly_rules",
        [
            {
                "Id": rule_id,
                "Name": name,
                "RuleType": rule_type,
                "SignalSource": source,
                "SignalName": signal,
                "ServiceName": service,
                "AttrFingerprint": attr_fp,
                "Comparator": comparator,
                "WarningThreshold": warning_threshold,
                "CriticalThreshold": critical_threshold,
                "SecondarySignalSource": secondary_source,
                "SecondarySignalName": secondary_signal,
                "SecondaryComparator": secondary_comparator,
                "SecondaryWarningThreshold": secondary_warning_threshold,
                "SecondaryCriticalThreshold": secondary_critical_threshold,
                "MinSampleCount": min_sample_count,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    await flash(f"Rule '{name}' created", "success")
    return redirect(url_for("view_metrics_rules"))


@app.route("/metrics/rules/auto", methods=["POST"])
@require_basic_auth
async def auto_metrics_rules():
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    try:
        hours = max(1, min(168, int(form.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24
    try:
        min_points = max(1, min(5000, int(form.get("min_points") or 30)))
    except (TypeError, ValueError):
        min_points = 30

    service_filter = (form.get("service_filter") or "").strip()
    include_attr_fp = (form.get("include_attr_fp") or "") in {"1", "true", "on", "yes"}

    db = get_db()
    services, signals, sources = _list_derived_signal_dimensions(db)
    existing_rules = _load_anomaly_rules(db)

    candidates, stats = _build_auto_metric_rule_candidates(
        db,
        hours=hours,
        min_points=min_points,
        service_filter=service_filter,
        include_attr_fp=include_attr_fp,
    )

    summary = {
        "action": action,
        "hours": hours,
        "min_points": min_points,
        "service_filter": service_filter,
        "include_attr_fp": include_attr_fp,
        "examined": stats["examined"],
        "existing": stats["existing"],
        "invalid": stats["invalid"],
        "candidates": len(candidates),
        "create_cap": _AUTO_RULE_CREATE_MAX,
        "capped": len(candidates) > _AUTO_RULE_CREATE_MAX,
        "created": 0,
    }

    if action == "create":
        limited_candidates = candidates[:_AUTO_RULE_CREATE_MAX]
        now_version = int(time.time() * 1000)
        rows_to_insert: list[dict[str, object]] = []
        for idx, candidate in enumerate(limited_candidates):
            rows_to_insert.append(
                {
                    "Id": str(uuid.uuid4()),
                    "Name": str(candidate["name"]),
                    "RuleType": "threshold",
                    "SignalSource": str(candidate["source"]),
                    "SignalName": str(candidate["signal"]),
                    "ServiceName": str(candidate["service"]),
                    "AttrFingerprint": str(candidate["attr_fp"]),
                    "Comparator": str(candidate["comparator"]),
                    "WarningThreshold": float(candidate["warning_threshold"]),
                    "CriticalThreshold": float(candidate["critical_threshold"]),
                    "SecondarySignalSource": "",
                    "SecondarySignalName": "",
                    "SecondaryComparator": "gt",
                    "SecondaryWarningThreshold": 0.0,
                    "SecondaryCriticalThreshold": 0.0,
                    "MinSampleCount": int(candidate["min_sample_count"]),
                    "IsDeleted": 0,
                    "Version": now_version + idx,
                }
            )

        if rows_to_insert:
            _insert_rows_json_each_row(db, "sobs_anomaly_rules", rows_to_insert)
        summary["created"] = len(rows_to_insert)
        skipped_by_cap = max(0, len(candidates) - len(limited_candidates))
        cap_suffix = f", skipped {skipped_by_cap} by max cap ({_AUTO_RULE_CREATE_MAX})." if skipped_by_cap else "."
        await flash(
            (
                f"Auto rule generation complete: created {summary['created']} rule(s), "
                f"skipped {summary['existing']} existing, {summary['invalid']} invalid"
                f"{cap_suffix}"
            ),
            "success",
        )
        return redirect(url_for("view_metrics_rules", open_panel="auto-rules"))

    await flash(
        (
            f"Auto-rule preview: {summary['candidates']} candidate(s), "
            f"{summary['existing']} existing skipped, {summary['invalid']} invalid."
        ),
        "info",
    )
    return await render_template(
        "metrics_rules.html",
        rules=existing_rules,
        services=services,
        signals=signals,
        sources=sources,
        auto_preview=candidates,
        auto_summary=summary,
        auto_dashboard_preview=[],
        auto_dashboard_summary=None,
        auto_open_panel="auto-rules",
    )


@app.route("/metrics/rules/dashboard/auto", methods=["POST"])
@require_basic_auth
async def auto_metrics_rules_dashboard():
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    service_filter = (form.get("service_filter") or "").strip()
    hours = _coerce_positive_int(form.get("hours"), default_value=24, min_value=1, max_value=168)
    max_charts = _coerce_positive_int(
        form.get("max_charts"),
        default_value=12,
        min_value=1,
        max_value=_AUTO_DASHBOARD_CREATE_MAX,
    )
    dashboard_name = (form.get("dashboard_name") or "").strip() or _default_auto_dashboard_name(service_filter)

    db = get_db()
    services, signals, sources = _list_derived_signal_dimensions(db)
    rules = _load_anomaly_rules(db)
    candidates = _build_auto_dashboard_chart_candidates(
        rules,
        service_filter=service_filter,
        hours=hours,
    )
    capped_candidates = candidates[:max_charts]

    summary = {
        "action": action,
        "hours": hours,
        "service_filter": service_filter,
        "max_charts": max_charts,
        "create_cap": _AUTO_DASHBOARD_CREATE_MAX,
        "dashboard_name": dashboard_name,
        "rules_total": len(rules),
        "candidates": len(candidates),
        "capped": len(candidates) > max_charts,
        "created": 0,
        "existing": 0,
    }

    if action == "create":
        if not capped_candidates:
            await flash("No matching rules found for dashboard generation", "warning")
            return redirect(url_for("view_metrics_rules", open_panel="auto-dashboard"))

        dashboard_description = (
            "Auto-generated from active metric rules. "
            f"window={hours}h, scope={'all services' if not service_filter else service_filter}."
        )
        dashboard_id = _seed_dashboard_if_missing(db, dashboard_name, dashboard_description)

        existing_charts = _get_charts(db, dashboard_id)
        existing_titles = {str(chart["title"]) for chart in existing_charts}
        next_position = max((int(chart["position"]) for chart in existing_charts), default=-1) + 1
        next_version = int(time.time() * 1000)
        rows_to_insert: list[dict[str, object]] = []

        for idx, candidate in enumerate(capped_candidates):
            title = str(candidate["title"])
            if title in existing_titles:
                summary["existing"] += 1
                continue
            query = str(candidate["query"])
            chart_type = str(candidate["chart_type"])
            rows_to_insert.append(
                {
                    "Id": str(uuid.uuid4()),
                    "DashboardId": dashboard_id,
                    "Title": title,
                    "ChartType": chart_type,
                    "Query": query,
                    "OptionsJson": json.dumps(
                        {"chart_spec": _build_raw_chart_spec(chart_type, query)},
                        ensure_ascii=False,
                    ),
                    "Position": next_position + idx,
                    "IsDeleted": 0,
                    "Version": next_version + idx,
                }
            )
            existing_titles.add(title)

        if rows_to_insert:
            _insert_rows_json_each_row(db, "sobs_chart_configs", rows_to_insert)
        summary["created"] = len(rows_to_insert)

        skipped_by_max = max(0, len(candidates) - len(capped_candidates))
        cap_note = f", skipped {skipped_by_max} by selected max ({max_charts})" if skipped_by_max else ""
        await flash(
            (
                f"Auto dashboard ready: created {summary['created']} chart(s), "
                f"skipped {summary['existing']} existing{cap_note}."
            ),
            "success",
        )
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))

    await flash(
        (
            f"Auto-dashboard preview: {summary['candidates']} candidate chart(s) from "
            f"{summary['rules_total']} rule(s)."
        ),
        "info",
    )
    return await render_template(
        "metrics_rules.html",
        rules=rules,
        services=services,
        signals=signals,
        sources=sources,
        auto_preview=[],
        auto_summary=None,
        auto_dashboard_preview=candidates,
        auto_dashboard_summary=summary,
        auto_open_panel="auto-dashboard",
    )


@app.route("/metrics/rules/<rule_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_metrics_rule(rule_id: str):
    db = get_db()
    row = db.execute(
        "SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, "
        "WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, "
        "SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount "
        "FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Id = ?",
        [rule_id],
    ).fetchone()
    if not row:
        await flash("Rule not found", "warning")
        return redirect(url_for("view_metrics_rules"))

    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_anomaly_rules",
        [
            {
                "Id": str(row["Id"]),
                "Name": str(row["Name"]),
                "RuleType": str(row["RuleType"] or "threshold"),
                "SignalSource": str(row["SignalSource"]),
                "SignalName": str(row["SignalName"]),
                "ServiceName": str(row["ServiceName"]),
                "AttrFingerprint": str(row["AttrFingerprint"]),
                "Comparator": str(row["Comparator"]),
                "WarningThreshold": float(row["WarningThreshold"]),
                "CriticalThreshold": float(row["CriticalThreshold"]),
                "SecondarySignalSource": str(row["SecondarySignalSource"]),
                "SecondarySignalName": str(row["SecondarySignalName"]),
                "SecondaryComparator": str(row["SecondaryComparator"] or "gt"),
                "SecondaryWarningThreshold": float(row["SecondaryWarningThreshold"]),
                "SecondaryCriticalThreshold": float(row["SecondaryCriticalThreshold"]),
                "MinSampleCount": int(row["MinSampleCount"]),
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    await flash(f"Rule '{str(row['Name'])}' deleted", "success")
    return redirect(url_for("view_metrics_rules"))


# ---------------------------------------------------------------------------
# Web UI – Metrics Anomaly Details
# ---------------------------------------------------------------------------
@app.route("/metrics/anomaly")
@require_basic_auth
async def view_metrics_anomaly():
    db = get_db()
    service = request.args.get("service", "").strip()
    metric = request.args.get("metric", "").strip()
    signal = request.args.get("signal", "").strip()
    source = request.args.get("source", "").strip()
    attr_fp = request.args.get("attr_fp", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()

    # Optional metadata passed from chart click for point-level context.
    point_state = request.args.get("_anomaly_state", "").strip()
    point_score = request.args.get("_anomaly_score", "").strip()

    try:
        hours = max(1, min(168, int(request.args.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24

    where_parts: list[str] = []
    params: list[str] = []
    if service:
        where_parts.append("ServiceName = ?")
        params.append(service)
    if metric:
        where_parts.append("MetricName = ?")
        params.append(metric)
    if signal:
        where_parts.append("SignalName = ?")
        params.append(signal)
    if source:
        where_parts.append("SignalSource = ?")
        params.append(source)
    if attr_fp:
        where_parts.append("AttrFingerprint = ?")
        params.append(attr_fp)

    if not time_error:
        time_conditions, time_params = _time_window_conditions("time", from_ts, to_ts)
        where_parts.extend(time_conditions)
        params.extend(time_params)

    # Fallback to hour-based window only when explicit time window is not provided.
    hour_clause = ""
    if not from_ts and not to_ts:
        hour_clause = "time >= now() - INTERVAL ? HOUR"
        params.append(hours)

    where_clause = ""
    if where_parts:
        where_clause = " WHERE " + " AND ".join(where_parts)
    if hour_clause:
        where_clause = f"{where_clause} AND {hour_clause}" if where_clause else f" WHERE {hour_clause}"

    rows: list[dict] = []
    error_msg = time_error
    related_target = source if source in {"logs", "traces", "errors"} else ""
    active_rules = _load_anomaly_rules(db)
    use_otel_metrics_view = bool(metric) and not signal and not source
    if not error_msg:
        try:
            # Keep existing metric drilldown behavior and support derived signals.
            result = db.execute(
                (
                    (
                        "SELECT"
                        "  time,"
                        "  ServiceName,"
                        "  MetricName AS Name,"
                        "  MetricKind AS Kind,"
                        "  AttrFingerprint,"
                        "  value,"
                        "  SampleCount,"
                        "  baseline_mean,"
                        "  baseline_stddev,"
                        "  baseline_lower,"
                        "  baseline_upper,"
                        "  anomaly_score,"
                        "  anomaly_state"
                        " FROM v_otel_metrics_anomaly"
                    )
                    if use_otel_metrics_view
                    else (
                        "SELECT"
                        "  time,"
                        "  ServiceName,"
                        "  SignalName AS Name,"
                        "  SignalSource AS Kind,"
                        "  AttrFingerprint,"
                        "  value,"
                        "  SampleCount,"
                        "  baseline_mean,"
                        "  baseline_stddev,"
                        "  baseline_lower,"
                        "  baseline_upper,"
                        "  anomaly_score,"
                        "  anomaly_state"
                        " FROM v_derived_signals_anomaly"
                    )
                    + f"{where_clause}"
                    + " ORDER BY time DESC"
                    + " LIMIT 500"
                ),
                params,
            )
            fetched = result.fetchall()
            for row in fetched:
                rows.append(
                    {
                        "time": str(row["time"]),
                        "service": str(row["ServiceName"]),
                        "metric": str(row["Name"]),
                        "metric_kind": str(row["Kind"]),
                        "related_target": ("" if use_otel_metrics_view else str(row["Kind"])),
                        "attr_fp": str(row["AttrFingerprint"]),
                        "value": row["value"],
                        "sample_count": row["SampleCount"],
                        "baseline_mean": row["baseline_mean"],
                        "baseline_stddev": row["baseline_stddev"],
                        "baseline_lower": row["baseline_lower"],
                        "baseline_upper": row["baseline_upper"],
                        "anomaly_score": row["anomaly_score"],
                        "anomaly_state": str(row["anomaly_state"]),
                    }
                )
        except Exception as exc:
            app.logger.exception("metrics anomaly detail query failed")
            error_msg = _public_dashboard_query_error(exc)

    if not use_otel_metrics_view:
        _annotate_rows_with_rules(
            rows,
            active_rules,
            source_key="related_target",
            signal_key="metric",
            service_key="service",
            attr_fp_key="attr_fp",
            value_key="value",
            sample_count_key="sample_count",
            time_key="time",
        )

    services, signals, sources = _list_derived_signal_dimensions(db)

    return await render_template(
        "metrics_anomaly.html",
        rows=rows,
        total=len(rows),
        service=service,
        metric=metric,
        signal=signal,
        source=source,
        attr_fp=attr_fp,
        from_ts=from_ts,
        to_ts=to_ts,
        hours=hours,
        error_msg=error_msg,
        point_state=point_state,
        point_score=point_score,
        related_target=related_target,
        services=services,
        signals=signals,
        sources=sources,
    )


# ---------------------------------------------------------------------------
# Web UI – Errors
# ---------------------------------------------------------------------------
@app.route("/errors")
@require_basic_auth
async def view_errors():
    db = get_db()
    service = request.args.get("service", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    resolved = request.args.get("resolved", "0").strip()
    limit = _parse_limit(100)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {"Timestamp": "Timestamp", "ServiceName": "ServiceName"},
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"
    resolved_ids = _get_resolved_error_ids(db)
    where_parts = []
    where_params = []
    if service:
        where_parts.append("ServiceName=?")
        where_params.append(service)
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where_parts.extend(time_conditions)
    where_params.extend(time_params)
    where_sql = ("WHERE " + " AND ".join(where_parts)) if where_parts else ""
    source_sql = (
        "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes "
        f"FROM ({ERROR_SOURCES_SQL}) {where_sql} "
        f"{order_clause} LIMIT ? OFFSET ?"
    )

    if resolved not in ("0", "1"):
        total = db.execute(
            f"SELECT COUNT(*) FROM ({ERROR_SOURCES_SQL}) {where_sql}",
            where_params,
        ).fetchone()[0]
        rows = db.execute(source_sql, where_params + [limit, offset]).fetchall()
        errors = []
        for row in rows:
            item = _build_error_item(dict(row))
            item["resolved"] = item["id"] in resolved_ids
            errors.append(item)
    else:
        # Keep behavior identical while avoiding full in-memory materialization.
        target_resolved = resolved == "1"
        scan_batch = max(200, limit)
        scan_offset = 0
        total = 0
        errors = []
        while True:
            batch = db.execute(source_sql, where_params + [scan_batch, scan_offset]).fetchall()
            if not batch:
                break
            for row in batch:
                item = _build_error_item(dict(row))
                item["resolved"] = item["id"] in resolved_ids
                if item["resolved"] != target_resolved:
                    continue
                if total >= offset and len(errors) < limit:
                    errors.append(item)
                total += 1
            scan_offset += scan_batch

    services = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT ServiceName FROM (" + ERROR_SOURCES_SQL + ") WHERE ServiceName!='' ORDER BY ServiceName"
        ).fetchall()
    ]

    return await render_template(
        "errors.html",
        errors=errors,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        from_ts=from_ts,
        to_ts=to_ts,
        error_msg=time_error,
        resolved=resolved,
        services=services,
        sort_by=sort_by,
        sort_dir=sort_dir,
    )


@app.route("/errors/<string:error_id>/resolve", methods=["POST"])
@require_basic_auth
async def resolve_error(error_id: str):
    try:

        def _op(db: ChDbConnection) -> None:
            db.execute("INSERT INTO sobs_error_resolutions(ErrorId) VALUES(?)", (error_id,))

        _queue_write(_op, wait=True)
    except Exception as exc:
        app.logger.exception("resolve error write failed")
        return jsonify({"error": str(exc)}), 500
    return jsonify({"ok": True})


# ---------------------------------------------------------------------------
# Web UI – Traces
# ---------------------------------------------------------------------------


def _ts_str_to_epoch_ms(ts: str) -> float:
    """Parse a DateTime64 timestamp string to epoch milliseconds."""
    ts = ts.strip()
    if "." in ts:
        base, frac = ts.split(".", 1)
        frac = frac[:6].ljust(6, "0")
        ts = f"{base}.{frac}"
    try:
        dt = datetime.fromisoformat(ts.replace(" ", "T"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp() * 1000.0
    except (ValueError, OverflowError) as exc:
        log.warning("_ts_str_to_epoch_ms: could not parse %r: %s", ts, exc)
        return 0.0


def _build_span_tree(spans: list[dict]) -> list[dict]:
    """Return spans ordered depth-first with ``depth`` and ``has_children`` fields."""
    by_id = {s["span_id"]: s for s in spans}
    children: dict[str, list[dict]] = {}
    roots: list[dict] = []
    for span in spans:
        pid = span.get("parent_span_id", "")
        if pid and pid in by_id:
            children.setdefault(pid, []).append(span)
        else:
            roots.append(span)
    for clist in children.values():
        clist.sort(key=lambda s: s["ts"])
    roots.sort(key=lambda s: s["ts"])
    result: list[dict] = []
    stack = [(root, 0) for root in reversed(roots)]
    while stack:
        span, depth = stack.pop()
        has_children = span["span_id"] in children
        result.append({**span, "depth": depth, "has_children": has_children})
        for child in reversed(children.get(span["span_id"], [])):
            stack.append((child, depth + 1))
    return result


@app.route("/traces")
@require_basic_auth
async def view_traces():
    db = get_db()
    service = request.args.get("service", "").strip()
    trace_id = request.args.get("trace_id", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    limit = _parse_limit(100)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {
            "Timestamp": "Timestamp",
            "SpanName": "SpanName",
            "ServiceName": "ServiceName",
            "Duration": "Duration",
        },
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    conditions = []
    params = []
    if service:
        conditions.append("ServiceName=?")
        params.append(service)
    if trace_id:
        conditions.append("TraceId=?")
        params.append(trace_id)
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    conditions.extend(time_conditions)
    params.extend(time_params)
    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""
    total = db.execute(f"SELECT COUNT(*) FROM otel_traces {where}", params).fetchone()[0]
    rows = db.execute(
        f"SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, Duration, StatusCode, SpanAttributes "
        f"FROM otel_traces {where} {order_clause} LIMIT ? OFFSET ?",
        params + [limit, offset],
    ).fetchall()

    spans = []
    for r in rows:
        attrs = _map_to_dict(r["SpanAttributes"])
        spans.append(
            {
                "ts": str(r["Timestamp"]),
                "trace_id": r["TraceId"],
                "span_id": r["SpanId"],
                "parent_span_id": r["ParentSpanId"],
                "name": r["SpanName"],
                "service": r["ServiceName"],
                "duration_ms": round(float(r["Duration"]) / 1_000_000, 2),
                "status": r["StatusCode"],
                "http_method": attrs.get("http.method", attrs.get("http.request.method", "")),
                "http_url": attrs.get("http.url", attrs.get("url.full", "")),
                "http_status": attrs.get("http.status_code", attrs.get("http.response.status_code", "")),
            }
        )

    services = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' ORDER BY ServiceName"
        ).fetchall()
    ]

    # When a specific trace is selected build an enriched detail view.
    trace_detail: dict | None = None
    if trace_id and not time_error:
        detail_rows = db.execute(
            "SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, "
            "Duration, StatusCode, SpanAttributes "
            "FROM otel_traces WHERE TraceId=? ORDER BY Timestamp ASC",
            [trace_id],
        ).fetchall()
        if detail_rows:
            all_trace_spans = []
            for r in detail_rows:
                attrs = _map_to_dict(r["SpanAttributes"])
                ts_str = str(r["Timestamp"])
                start_ms = _ts_str_to_epoch_ms(ts_str)
                dur_ms = round(float(r["Duration"]) / 1_000_000, 2)
                all_trace_spans.append(
                    {
                        "ts": ts_str,
                        "trace_id": str(r["TraceId"]),
                        "span_id": str(r["SpanId"]),
                        "parent_span_id": str(r["ParentSpanId"]),
                        "name": str(r["SpanName"]),
                        "service": str(r["ServiceName"]),
                        "start_ms": start_ms,
                        "duration_ms": dur_ms,
                        "status": str(r["StatusCode"]),
                        "http_method": str(attrs.get("http.method", attrs.get("http.request.method", ""))),
                        "http_url": str(attrs.get("http.url", attrs.get("url.full", ""))),
                        "http_status": str(attrs.get("http.status_code", attrs.get("http.response.status_code", ""))),
                    }
                )

            # Compute relative timeline positions.
            trace_start_ms = min(s["start_ms"] for s in all_trace_spans)
            trace_end_ms = max(s["start_ms"] + s["duration_ms"] for s in all_trace_spans)
            trace_total_ms = max(trace_end_ms - trace_start_ms, 1.0)
            for span in all_trace_spans:
                span["offset_pct"] = round((span["start_ms"] - trace_start_ms) / trace_total_ms * 100, 2)
                # 0.5 minimum keeps very short spans visible in the timeline bar
                span["width_pct"] = round(max(0.5, span["duration_ms"] / trace_total_ms * 100), 2)

            # Fetch related errors for this trace (capped at 50; flag truncation for the UI).
            _TRACE_ERROR_LIMIT = 50
            trace_errors: list[dict] = []
            errors_truncated = False
            try:
                err_rows = db.execute(
                    "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes "
                    f"FROM ({ERROR_SOURCES_SQL}) WHERE TraceId=? LIMIT ?",
                    [trace_id, _TRACE_ERROR_LIMIT + 1],
                ).fetchall()
                resolved_ids = _get_resolved_error_ids(db)
                if len(err_rows) > _TRACE_ERROR_LIMIT:
                    errors_truncated = True
                    err_rows = err_rows[:_TRACE_ERROR_LIMIT]
                for row in err_rows:
                    item = _build_error_item(dict(row))
                    item["resolved"] = item["id"] in resolved_ids
                    trace_errors.append(item)
            except Exception as exc:
                log.warning("view_traces: failed to fetch errors for trace %s: %s", trace_id, exc)

            error_span_ids = {e["span_id"] for e in trace_errors if e.get("span_id")}

            # Fetch log counts per span for this trace.
            log_counts: dict[str, int] = {}
            try:
                log_rows = db.execute(
                    "SELECT SpanId, count() AS cnt FROM otel_logs " "WHERE TraceId=? AND SpanId!='' GROUP BY SpanId",
                    [trace_id],
                ).fetchall()
                for r in log_rows:
                    log_counts[str(r["SpanId"])] = int(r["cnt"])
            except Exception as exc:
                log.warning("view_traces: failed to fetch log counts for trace %s: %s", trace_id, exc)

            # Fetch anomaly state for the primary service.
            trace_anomaly_state: str | None = None
            try:
                svc = all_trace_spans[0]["service"] if all_trace_spans else ""
                if svc:
                    anomaly_row = db.execute(
                        "SELECT anomaly_state FROM v_derived_signals_anomaly "
                        "WHERE ServiceName=? AND SignalSource='traces' "
                        "ORDER BY time DESC LIMIT 1",
                        [svc],
                    ).fetchone()
                    if anomaly_row:
                        trace_anomaly_state = str(anomaly_row["anomaly_state"])
            except Exception as exc:
                log.warning("view_traces: failed to fetch anomaly state for trace %s: %s", trace_id, exc)

            trace_detail = {
                "span_tree": _build_span_tree(all_trace_spans),
                "errors": trace_errors,
                "errors_truncated": errors_truncated,
                "error_span_ids": error_span_ids,
                "log_counts": log_counts,
                "anomaly_state": trace_anomaly_state,
                "total_ms": round(trace_total_ms, 2),
            }

    return await render_template(
        "traces.html",
        spans=spans,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        trace_id=trace_id,
        from_ts=from_ts,
        to_ts=to_ts,
        error_msg=time_error,
        services=services,
        sort_by=sort_by,
        sort_dir=sort_dir,
        trace_detail=trace_detail,
    )


# ---------------------------------------------------------------------------
# Web UI – RUM
# ---------------------------------------------------------------------------
@app.route("/rum")
@require_basic_auth
async def view_rum():
    db = get_db()
    event_type = request.args.get("type", "").strip()
    limit = _parse_limit(200)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {"Timestamp": "Timestamp", "EventName": "EventName"},
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"
    from_ts, to_ts, time_error = _parse_time_window_args()

    conditions = []
    params = []
    if event_type:
        conditions.append("EventName=?")
        params.append(event_type)
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    conditions.extend(time_conditions)
    params.extend(time_params)
    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""

    total = db.execute(f"SELECT COUNT(*) FROM hyperdx_sessions {where}", params).fetchone()[0]
    rows = db.execute(
        f"SELECT Timestamp, EventName, Body, LogAttributes FROM hyperdx_sessions {where} "
        f"{order_clause} LIMIT ? OFFSET ?",
        params + [limit, offset],
    ).fetchall()

    events = []
    for r in rows:
        attrs = _map_to_dict(r["LogAttributes"])
        try:
            body_data = json.loads(r["Body"]) if r["Body"] else {}
        except json.JSONDecodeError:
            body_data = {}
        data = body_data if isinstance(body_data, dict) else {"value": body_data}
        events.append(
            {
                "ts": str(r["Timestamp"]),
                "session_id": str(attrs.get("sessionId", attrs.get("session.id", "")))[:8],
                "event_type": r["EventName"],
                "url": str(attrs.get("url", attrs.get("url.full", ""))),
                "data": data,
            }
        )

    event_types = [
        row[0] for row in db.execute("SELECT DISTINCT EventName FROM hyperdx_sessions ORDER BY EventName").fetchall()
    ]

    # Web vitals summary
    vitals_rows = db.execute(
        "SELECT Body, LogAttributes FROM hyperdx_sessions WHERE EventName='web-vital' "
        "ORDER BY Timestamp DESC LIMIT 500"
    ).fetchall()
    vitals = {}
    for vr in vitals_rows:
        attrs = _map_to_dict(vr["LogAttributes"])
        try:
            d = json.loads(vr["Body"]) if vr["Body"] else {}
        except json.JSONDecodeError:
            d = {}
        if not isinstance(d, dict):
            d = {}
        name = d.get("name", "")
        val = d.get("value", attrs.get("value"))
        try:
            val = float(val) if val is not None else None
        except (TypeError, ValueError):
            val = None
        if name and val is not None:
            vitals.setdefault(name, []).append(val)
    vitals_summary = {}
    for name, vals in vitals.items():
        vitals_summary[name] = {
            "avg": round(sum(vals) / len(vals), 1),
            "p75": round(sorted(vals)[int(len(vals) * 0.75)], 1),
            "count": len(vals),
        }

    return await render_template(
        "rum.html",
        events=events,
        total=total,
        limit=limit,
        offset=offset,
        event_type=event_type,
        event_types=event_types,
        vitals_summary=vitals_summary,
        sort_by=sort_by,
        sort_dir=sort_dir,
        from_ts=from_ts,
        to_ts=to_ts,
        error_msg=time_error,
    )


# ---------------------------------------------------------------------------
# Web UI – AI Transparency
# ---------------------------------------------------------------------------
@app.route("/ai")
@require_basic_auth
async def view_ai():
    db = get_db()
    service = request.args.get("service", "").strip()
    model = request.args.get("model", "").strip()
    operation_filter = request.args.get("operation", "").strip()
    view_mode = request.args.get("view", "flat").strip().lower()
    if view_mode not in ("flat", "trace"):
        view_mode = "flat"
    limit = _parse_limit(50)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {"Timestamp": "Timestamp", "Duration": "Duration", "ServiceName": "ServiceName"},
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    conditions = []
    params = []
    if service:
        conditions.append("ServiceName=?")
        params.append(service)
    if model:
        conditions.append("SpanAttributes['gen_ai.request.model']=?")
        params.append(model)
    if operation_filter:
        if operation_filter.lower() == "chat":
            conditions.append(
                "(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')"
            )
            params.append("chat")
        else:
            conditions.append("SpanAttributes['gen_ai.operation.name']=?")
            params.append(operation_filter)
    conditions.append("(SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '')")
    where = ("WHERE " + " AND ".join(conditions)) if conditions else ""

    trace_ids: list[str] = []
    if view_mode == "trace":
        trace_conditions = list(conditions)
        trace_conditions.append("TraceId != ''")
        trace_where = "WHERE " + " AND ".join(trace_conditions)
        total = db.execute(f"SELECT COUNT(DISTINCT TraceId) FROM otel_traces {trace_where}", params).fetchone()[0]
        trace_rows = db.execute(
            f"SELECT TraceId, MAX(Timestamp) AS LastTs FROM otel_traces "
            f"{trace_where} GROUP BY TraceId ORDER BY LastTs {'ASC' if sort_dir == 'asc' else 'DESC'} LIMIT ? OFFSET ?",
            params + [limit, offset],
        ).fetchall()
        trace_ids = [str(r["TraceId"]) for r in trace_rows if str(r["TraceId"])]
        if trace_ids:
            placeholders = ",".join(["?"] * len(trace_ids))
            rows = db.execute(
                f"SELECT Timestamp, ServiceName, TraceId, Duration, SpanAttributes "
                f"FROM otel_traces WHERE TraceId IN ({placeholders}) "
                "AND (SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '') "
                "ORDER BY Timestamp ASC",
                trace_ids,
            ).fetchall()
        else:
            rows = []
    else:
        total = db.execute(f"SELECT COUNT(*) FROM otel_traces {where}", params).fetchone()[0]
        rows = db.execute(
            f"SELECT Timestamp, ServiceName, TraceId, Duration, SpanAttributes "
            f"FROM otel_traces {where} {order_clause} LIMIT ? OFFSET ?",
            params + [limit, offset],
        ).fetchall()

    ai_items = []
    for r in rows:
        attrs = _map_to_dict(r["SpanAttributes"])
        ts = str(r["Timestamp"])
        # Coalesce provider: canonical gen_ai.provider.name with legacy gen_ai.system fallback
        provider = str(attrs.get("gen_ai.provider.name") or attrs.get("gen_ai.system", ""))
        req_model = str(attrs.get("gen_ai.request.model", ""))
        operation = str(attrs.get("gen_ai.operation.name", "chat"))
        # Coalesce prompt/response: OTel standard fields first, sobs legacy fields as fallback
        input_messages_raw = str(attrs.get("gen_ai.input.messages", ""))
        output_messages_raw = str(attrs.get("gen_ai.output.messages", ""))
        prompt = _extract_messages_text(input_messages_raw) or str(attrs.get("sobs.gen_ai.prompt", ""))
        response = _extract_messages_text(output_messages_raw) or str(attrs.get("sobs.gen_ai.response", ""))
        tokens_in = int(float(attrs.get("gen_ai.usage.input_tokens", "0") or 0))
        tokens_out = int(float(attrs.get("gen_ai.usage.output_tokens", "0") or 0))
        err_type = str(attrs.get("error.type", ""))
        msg = str(attrs.get("exception.message", ""))
        duration_ms = round(float(r["Duration"]) / 1_000_000, 1)
        tokens_per_sec = round(tokens_out / (duration_ms / 1000), 1) if duration_ms > 0 and tokens_out > 0 else 0
        # Additional OTel GenAI attributes
        finish_reason = str(attrs.get("gen_ai.response.finish_reason", ""))
        temperature = str(attrs.get("gen_ai.request.temperature", ""))
        max_tokens = str(attrs.get("gen_ai.request.max_tokens", ""))
        thinking_tokens = int(float(attrs.get("gen_ai.usage.thinking_tokens", "0") or 0))
        # Build structured messages for conversation view
        input_messages = []
        output_messages = []
        try:
            if input_messages_raw:
                parsed = json.loads(input_messages_raw)
                if isinstance(parsed, list):
                    input_messages = parsed
        except (json.JSONDecodeError, TypeError):
            pass
        try:
            if output_messages_raw:
                parsed = json.loads(output_messages_raw)
                if isinstance(parsed, list):
                    output_messages = parsed
        except (json.JSONDecodeError, TypeError):
            pass
        # Build raw attributes dict for JSON inspector
        raw_attrs = dict(attrs)
        row_id = _error_id(ts, r["ServiceName"], provider, req_model + err_type + msg, r["TraceId"], "")
        ai_items.append(
            {
                "id": row_id,
                "ts": ts,
                "service": r["ServiceName"],
                "provider": provider,
                "model": req_model,
                "operation": operation,
                "prompt": prompt,
                "response": response,
                "input_messages": input_messages,
                "output_messages": output_messages,
                "input_messages_json": input_messages_raw,
                "output_messages_json": output_messages_raw,
                "tokens_in": tokens_in,
                "tokens_out": tokens_out,
                "thinking_tokens": thinking_tokens,
                "duration_ms": duration_ms,
                "tokens_per_sec": tokens_per_sec,
                "trace_id": r["TraceId"],
                "error_type": err_type,
                "error_message": msg,
                "finish_reason": finish_reason,
                "temperature": temperature,
                "max_tokens": max_tokens,
                "raw_attrs": json.dumps(raw_attrs, ensure_ascii=False, indent=2),
            }
        )

    trace_groups = []
    if view_mode == "trace":
        by_trace: dict[str, dict] = {
            tid: {
                "id": _error_id("", "", "trace", tid, tid, ""),
                "trace_id": tid,
                "spans": [],
                "calls": 0,
                "tokens_in": 0,
                "tokens_out": 0,
                "errors": 0,
                "services": set(),
                "models": set(),
                "operations": set(),
                "first_ts": "",
                "last_ts": "",
            }
            for tid in trace_ids
        }
        for item in ai_items:
            tid = str(item.get("trace_id", ""))
            if not tid or tid not in by_trace:
                continue
            grp = by_trace[tid]
            grp["spans"].append(item)
            grp["calls"] += 1
            grp["tokens_in"] += int(item.get("tokens_in", 0) or 0)
            grp["tokens_out"] += int(item.get("tokens_out", 0) or 0)
            if item.get("error_type"):
                grp["errors"] += 1
            svc = str(item.get("service", ""))
            mdl = str(item.get("model", ""))
            op = str(item.get("operation", ""))
            if svc:
                grp["services"].add(svc)
            if mdl:
                grp["models"].add(mdl)
            if op:
                grp["operations"].add(op)
            ts = str(item.get("ts", ""))
            if ts:
                if not grp["first_ts"] or ts < grp["first_ts"]:
                    grp["first_ts"] = ts
                if not grp["last_ts"] or ts > grp["last_ts"]:
                    grp["last_ts"] = ts

        for tid in trace_ids:
            grp = by_trace[tid]
            if not grp["spans"]:
                continue
            grp["services"] = sorted(grp["services"])
            grp["models"] = sorted(grp["models"])
            grp["operations"] = sorted(grp["operations"])
            trace_groups.append(grp)

    services = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT ServiceName FROM otel_traces "
            "WHERE (SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '') "
            "AND ServiceName!='' ORDER BY ServiceName"
        ).fetchall()
    ]
    models = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT SpanAttributes['gen_ai.request.model'] AS model FROM otel_traces "
            "WHERE (SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '') "
            "AND SpanAttributes['gen_ai.request.model'] != '' ORDER BY model"
        ).fetchall()
    ]
    operations = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT SpanAttributes['gen_ai.operation.name'] AS op FROM otel_traces "
            "WHERE (SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '') "
            "AND SpanAttributes['gen_ai.operation.name'] != '' ORDER BY op"
        ).fetchall()
    ]

    # Token usage totals
    totals = db.execute(
        "SELECT "
        "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])) ti, "
        "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])) to_, "
        "COUNT(*) cnt, "
        "countIf(SpanAttributes['error.type'] != '') errors "
        "FROM otel_traces WHERE (SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '')"
    ).fetchone()

    return await render_template(
        "ai.html",
        ai_items=ai_items,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        model=model,
        operation=operation_filter,
        view_mode=view_mode,
        services=services,
        models=models,
        operations=operations,
        trace_groups=trace_groups,
        total_tokens_in=totals["ti"] or 0,
        total_tokens_out=totals["to_"] or 0,
        total_calls=totals["cnt"] or 0,
        total_errors=totals["errors"] or 0,
        sort_by=sort_by,
        sort_dir=sort_dir,
    )


# ---------------------------------------------------------------------------
# AI training data export  GET /api/ai/export
# ---------------------------------------------------------------------------
@app.route("/api/ai/export")
@require_basic_auth
async def export_ai_training():
    """Export AI call data as JSONL for training dataset creation."""
    db = get_db()
    service = request.args.get("service", "").strip()
    model = request.args.get("model", "").strip()
    operation_filter = request.args.get("operation", "").strip()
    fmt = request.args.get("format", "jsonl").strip().lower()
    try:
        max_rows = max(1, min(int(request.args.get("limit", 1000)), 5000))
    except (ValueError, TypeError):
        max_rows = 1000

    conditions = [
        "(SpanAttributes['gen_ai.provider.name'] != '' OR SpanAttributes['gen_ai.system'] != '')",
    ]
    params: list = []
    if service:
        conditions.append("ServiceName=?")
        params.append(service)
    if model:
        conditions.append("SpanAttributes['gen_ai.request.model']=?")
        params.append(model)
    if operation_filter:
        if operation_filter.lower() == "chat":
            conditions.append(
                "(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')"
            )
            params.append("chat")
        else:
            conditions.append("SpanAttributes['gen_ai.operation.name']=?")
            params.append(operation_filter)
    where = "WHERE " + " AND ".join(conditions)

    rows = db.execute(
        f"SELECT Timestamp, ServiceName, TraceId, Duration, SpanAttributes "
        f"FROM otel_traces {where} ORDER BY Timestamp DESC LIMIT ?",
        params + [max_rows],
    ).fetchall()

    records = []
    for r in rows:
        attrs = _map_to_dict(r["SpanAttributes"])
        provider = str(attrs.get("gen_ai.provider.name") or attrs.get("gen_ai.system", ""))
        req_model = str(attrs.get("gen_ai.request.model", ""))
        input_messages_raw = str(attrs.get("gen_ai.input.messages", ""))
        output_messages_raw = str(attrs.get("gen_ai.output.messages", ""))
        prompt = _extract_messages_text(input_messages_raw) or str(attrs.get("sobs.gen_ai.prompt", ""))
        response = _extract_messages_text(output_messages_raw) or str(attrs.get("sobs.gen_ai.response", ""))
        tokens_in = int(float(attrs.get("gen_ai.usage.input_tokens", "0") or 0))
        tokens_out = int(float(attrs.get("gen_ai.usage.output_tokens", "0") or 0))

        # Build messages array for training format
        messages: list = []
        try:
            if input_messages_raw:
                parsed = json.loads(input_messages_raw)
                if isinstance(parsed, list):
                    messages.extend(parsed)
        except (json.JSONDecodeError, TypeError):
            if prompt:
                messages.append({"role": "user", "content": prompt})
        try:
            if output_messages_raw:
                parsed = json.loads(output_messages_raw)
                if isinstance(parsed, list):
                    messages.extend(parsed)
        except (json.JSONDecodeError, TypeError):
            if response:
                messages.append({"role": "assistant", "content": response})

        record = {
            "messages": messages,
            "metadata": {
                "timestamp": str(r["Timestamp"]),
                "service": r["ServiceName"],
                "provider": provider,
                "model": req_model,
                "tokens_in": tokens_in,
                "tokens_out": tokens_out,
                "duration_ms": round(float(r["Duration"]) / 1_000_000, 1),
                "trace_id": r["TraceId"],
            },
        }
        records.append(record)

    if fmt == "json":
        body = json.dumps(records, ensure_ascii=False, indent=2)
        mime = "application/json"
        filename = "ai_training_data.json"
    else:
        lines = [json.dumps(rec, ensure_ascii=False) for rec in records]
        body = "\n".join(lines)
        mime = "application/x-ndjson"
        filename = "ai_training_data.jsonl"

    return Response(
        body,
        mimetype=mime,
        headers={"Content-Disposition": f'attachment; filename="{filename}"'},
    )


# ---------------------------------------------------------------------------
# Custom Dashboards (Template-driven eCharts)
# ---------------------------------------------------------------------------

# Chart Templates: Define structure, column roles, and eCharts rendering
CHART_TEMPLATES = {
    "time_series_percentiles": {
        "id": "time_series_percentiles",
        "name": "Time Series with Normal Range",
        "description": "Show metric with percentile bands for anomaly detection",
        "icon": "bi-graph-up",
        "query_shape": "Columns: time, value, p95, p99",
        "sample_sql": (
            "SELECT\n"
            "  toStartOfMinute(Timestamp) AS time,\n"
            "  avg(Duration) AS value,\n"
            "  quantile(0.95)(Duration) AS p95,\n"
            "  quantile(0.99)(Duration) AS p99\n"
            "FROM otel_traces\n"
            "GROUP BY time\n"
            "ORDER BY time"
        ),
        "drilldown": {
            "target": "traces",
            "label": "Open source traces",
            "bucket_seconds": 60,
            "time_axis": "x",
        },
        "min_columns": 4,
        "max_columns": 4,
        "column_roles": {"time": 0, "value": 1, "p95": 2, "p99": 3},
        "echarts_option_template": {
            "tooltip": {"trigger": "axis"},
            "legend": {"data": ["Metric", "p95 Band", "p99 Band"], "bottom": 0},
            "xAxis": {"type": "time", "data": "{{time}}"},
            "yAxis": {"type": "value"},
            "grid": {"left": "3%", "right": "4%", "bottom": "15%", "containLabel": True},
            "series": [
                {
                    "name": "Metric",
                    "type": "line",
                    "data": "{{value}}",
                    "lineStyle": {"color": "#0d6efd"},
                    "symbol": "none",
                },
                {
                    "name": "p95 Band",
                    "type": "line",
                    "data": "{{p95}}",
                    "lineStyle": {"type": "dashed", "color": "#ffc107"},
                    "symbol": "none",
                },
                {
                    "name": "p99 Band",
                    "type": "line",
                    "data": "{{p99}}",
                    "lineStyle": {"type": "dashed", "color": "#dc3545"},
                    "symbol": "none",
                    "areaStyle": {"color": "rgba(220, 53, 69, 0.1)"},
                },
            ],
        },
    },
    "heatmap": {
        "id": "heatmap",
        "name": "Heatmap",
        "description": "2D heatmap for correlating errors across dimensions",
        "icon": "bi-fire",
        "query_shape": "Columns: x category, y time bucket, numeric value",
        "sample_sql": (
            "SELECT\n"
            "  ServiceName AS x_category,\n"
            "  toStartOfFiveMinutes(Timestamp) AS y_category,\n"
            "  round(100.0 * countIf(StatusCode = 'STATUS_CODE_ERROR') / count(), 2) AS value\n"
            "FROM otel_traces\n"
            "GROUP BY ServiceName, y_category\n"
            "ORDER BY ServiceName, y_category"
        ),
        "drilldown": {
            "target": "traces",
            "label": "Open source traces",
            "bucket_seconds": 300,
            "time_axis": "y",
            "service_axis": "x",
        },
        "min_columns": 3,
        "max_columns": 3,
        "column_roles": {"x_category": 0, "y_category": 1, "value": 2},
        "echarts_option_template": {
            "tooltip": {"trigger": "item", "formatter": "{b}: {c}"},
            "xAxis": {"type": "category", "data": "{{x_unique_values}}"},
            "yAxis": {"type": "category", "data": "{{y_unique_values}}"},
            "visualMap": {
                "min": "{{value_min}}",
                "max": "{{value_max}}",
                "inRange": {"color": ["#ebedf0", "#c6e48b", "#7bc96f", "#239a3b", "#196127"]},
                "text": ["High", "Low"],
                "bottom": 0,
            },
            "grid": {"left": "15%", "right": "10%", "bottom": "15%", "top": "10%", "containLabel": True},
            "series": [
                {
                    "type": "heatmap",
                    "data": "{{heatmap_data}}",
                    "emphasis": {"itemStyle": {"borderColor": "#fff", "borderWidth": 2}},
                }
            ],
        },
    },
    "box_plot": {
        "id": "box_plot",
        "name": "Distribution Box Plot",
        "description": "Show distribution, quartiles, and outliers",
        "icon": "bi-boxes",
        "query_shape": "Columns: dimension, min, q1, median, q3, max",
        "sample_sql": (
            "SELECT\n"
            "  HTTPMethod AS dimension,\n"
            "  min(Duration) AS min,\n"
            "  quantile(0.25)(Duration) AS q1,\n"
            "  quantile(0.5)(Duration) AS median,\n"
            "  quantile(0.75)(Duration) AS q3,\n"
            "  max(Duration) AS max\n"
            "FROM otel_traces\n"
            "GROUP BY HTTPMethod\n"
            "ORDER BY median DESC"
        ),
        "drilldown": {
            "target": "traces",
            "label": "Open traces view",
        },
        "min_columns": 6,
        "max_columns": 6,
        "column_roles": {"dimension": 0, "min": 1, "q1": 2, "median": 3, "q3": 4, "max": 5},
        "echarts_option_template": {
            "tooltip": {"trigger": "item"},
            "xAxis": {"type": "category", "data": "{{dimension_values}}", "nameGap": 30},
            "yAxis": {"type": "value", "name": "Value"},
            "grid": {"left": "10%", "right": "10%", "bottom": "15%", "containLabel": True},
            "series": [
                {
                    "type": "boxplot",
                    "data": "{{boxplot_data}}",
                    "itemStyle": {"color": "#0d6efd", "borderColor": "#0d6efd"},
                }
            ],
        },
    },
    "dual_axis_anomaly": {
        "id": "dual_axis_anomaly",
        "name": "Metric + Anomaly Score",
        "description": "Compare metric vs anomaly detection signal on dual axes",
        "icon": "bi-graph-up-arrow",
        "query_shape": "Columns: time, metric, anomaly_score",
        "sample_sql": (
            "SELECT\n"
            "  time,\n"
            "  value AS metric,\n"
            "  anomaly_score\n"
            "FROM v_otel_metrics_anomaly\n"
            "WHERE ServiceName = 'my-service'\n"
            "  AND MetricName = 'my.metric'\n"
            "  AND time >= now() - INTERVAL 1 HOUR\n"
            "ORDER BY time"
        ),
        "drilldown": {
            "target": "logs",
            "label": "Open source logs",
            "bucket_seconds": 60,
            "time_axis": "x",
            "extra": {"analyze": "1", "stats": "1"},
        },
        "min_columns": 3,
        "max_columns": 3,
        "column_roles": {"time": 0, "metric": 1, "anomaly_score": 2},
        "echarts_option_template": {
            "tooltip": {"trigger": "axis"},
            "legend": {"data": ["Metric", "Anomaly Score"], "bottom": 0},
            "xAxis": {"type": "time", "data": "{{time}}"},
            "yAxis": [
                {
                    "type": "value",
                    "name": "Metric",
                    "position": "left",
                    "axisLine": {"lineStyle": {"color": "#0d6efd"}},
                },
                {
                    "type": "value",
                    "name": "Anomaly Score",
                    "position": "right",
                    "axisLine": {"lineStyle": {"color": "#dc3545"}},
                },
            ],
            "grid": {"left": "3%", "right": "4%", "bottom": "15%", "containLabel": True},
            "series": [
                {
                    "name": "Metric",
                    "type": "line",
                    "data": "{{metric}}",
                    "yAxisIndex": 0,
                    "lineStyle": {"color": "#0d6efd"},
                    "symbol": "none",
                },
                {
                    "name": "Anomaly Score",
                    "type": "bar",
                    "data": "{{anomaly_score}}",
                    "yAxisIndex": 1,
                    "itemStyle": {"color": "rgba(220, 53, 69, 0.5)"},
                },
            ],
        },
    },
    "anomaly_overlay": {
        "id": "anomaly_overlay",
        "name": "Anomaly Overlay",
        "description": "Metric with baseline band and per-point anomaly state markers (normal/warning/outlier)",
        "icon": "bi-activity",
        "query_shape": "Columns: time, value, baseline_mean, baseline_lower, baseline_upper, anomaly_state",
        "sample_sql": (
            "SELECT\n"
            "  time,\n"
            "  value,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state\n"
            "FROM v_otel_metrics_anomaly\n"
            "WHERE ServiceName = 'my-service'\n"
            "  AND MetricName = 'my.metric'\n"
            "  AND time >= now() - INTERVAL 6 HOUR\n"
            "ORDER BY time"
        ),
        "drilldown": {
            "target": "metrics",
            "label": "Open anomaly details",
            "bucket_seconds": 60,
            "time_axis": "x",
        },
        "min_columns": 6,
        "max_columns": 6,
        "column_roles": {
            "time": 0,
            "value": 1,
            "baseline_mean": 2,
            "baseline_lower": 3,
            "baseline_upper": 4,
            "anomaly_state": 5,
        },
        "echarts_option_template": {
            "tooltip": {"trigger": "axis"},
            "legend": {"data": ["Value", "Baseline", "Normal Band"], "bottom": 0},
            "xAxis": {"type": "time", "data": "{{time}}"},
            "yAxis": {"type": "value"},
            "grid": {"left": "3%", "right": "4%", "bottom": "15%", "containLabel": True},
            "series": [
                {
                    "name": "Normal Band",
                    "type": "line",
                    "data": "{{baseline_upper}}",
                    "lineStyle": {"opacity": 0},
                    "areaStyle": {"color": "rgba(13, 110, 253, 0.08)"},
                    "symbol": "none",
                    "stack": "band",
                },
                {
                    "name": "Baseline",
                    "type": "line",
                    "data": "{{baseline_mean}}",
                    "lineStyle": {"type": "dashed", "color": "#6c757d"},
                    "symbol": "none",
                },
                {
                    "name": "Value",
                    "type": "line",
                    "data": "{{value}}",
                    "lineStyle": {"color": "#0d6efd"},
                    "symbol": "circle",
                    "symbolSize": "{{anomaly_symbol_size}}",
                    "itemStyle": {"color": "{{anomaly_point_color}}"},
                },
            ],
        },
    },
    "derived_signal_overlay": {
        "id": "derived_signal_overlay",
        "name": "Derived Signal Overlay",
        "description": "At-a-glance signal health view with recent focus, anomaly windows, and status summary",
        "icon": "bi-soundwave",
        "query_shape": (
            "Columns: time, service, source, signal, attr_fp, value, sample_count, baseline_mean, "
            "baseline_lower, baseline_upper, anomaly_state, anomaly_score"
        ),
        "sample_sql": (
            "SELECT\n"
            "  time,\n"
            "  ServiceName AS service,\n"
            "  SignalSource AS source,\n"
            "  SignalName AS signal,\n"
            "  AttrFingerprint AS attr_fp,\n"
            "  value,\n"
            "  SampleCount AS sample_count,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state,\n"
            "  anomaly_score\n"
            "FROM v_derived_signals_anomaly\n"
            "WHERE ServiceName = 'trace-svc-0'\n"
            "  AND SignalSource = 'traces'\n"
            "  AND SignalName = 'latency_p95_ms'\n"
            "  AND time >= now() - INTERVAL 6 HOUR\n"
            "ORDER BY time"
        ),
        "drilldown": {
            "target": "metrics",
            "label": "Open signal details",
            "bucket_seconds": 60,
            "time_axis": "x",
        },
        "min_columns": 12,
        "max_columns": 16,
        "column_roles": {
            "time": 0,
            "service": 1,
            "source": 2,
            "signal": 3,
            "attr_fp": 4,
            "value": 5,
            "sample_count": 6,
            "baseline_mean": 7,
            "baseline_lower": 8,
            "baseline_upper": 9,
            "anomaly_state": 10,
            "anomaly_score": 11,
            "rule_state": 12,
            "rule_name": 13,
            "rule_reason": 14,
            "effective_state": 15,
        },
        "echarts_option_template": {
            "title": {
                "left": 8,
                "top": 2,
                "text": "",
                "subtext": "{{signal_summary}}",
                "textStyle": {"fontSize": 11, "color": "#adb5bd"},
                "subtextStyle": {"fontSize": 11, "color": "#9ca3af"},
            },
            "tooltip": {"trigger": "axis"},
            "legend": {"data": ["Value", "Baseline", "Expected Band"], "bottom": 0},
            "xAxis": {"type": "time", "axisLabel": {"hideOverlap": True}},
            "yAxis": {
                "type": "value",
                "name": "{{y_axis_name}}",
                "nameTextStyle": {"color": "#9ca3af", "fontSize": 11},
                "min": "{{value_axis_min}}",
                "max": "{{value_axis_max}}",
            },
            "dataZoom": [
                {"type": "inside", "xAxisIndex": 0, "filterMode": "none", "start": "{{zoom_start_pct}}", "end": 100}
            ],
            "visualMap": {
                "show": False,
                "dimension": 2,
                "seriesIndex": 3,
                "pieces": [
                    {"value": 2, "color": "#dc3545"},
                    {"value": 1, "color": "#ffc107"},
                    {"value": 0, "color": "#20c997"},
                ],
            },
            "grid": {"left": "3%", "right": "4%", "bottom": "15%", "containLabel": True},
            "series": [
                {
                    "name": "Band Lower",
                    "type": "line",
                    "data": "{{baseline_lower_points}}",
                    "lineStyle": {"opacity": 0},
                    "symbol": "none",
                    "stack": "expected_band",
                },
                {
                    "name": "Expected Band",
                    "type": "line",
                    "data": "{{baseline_upper_points}}",
                    "lineStyle": {"opacity": 0},
                    "areaStyle": {"color": "rgba(13, 110, 253, 0.12)"},
                    "symbol": "none",
                    "stack": "expected_band",
                },
                {
                    "name": "Baseline",
                    "type": "line",
                    "data": "{{baseline_mean_points}}",
                    "lineStyle": {"type": "dashed", "color": "#6c757d"},
                    "symbol": "none",
                },
                {
                    "name": "Value",
                    "type": "line",
                    "smooth": True,
                    "data": "{{value_points}}",
                    "encode": {"x": 0, "y": 1},
                    "lineStyle": {"width": 2, "color": "#20c997"},
                    "symbol": "circle",
                    "symbolSize": 4,
                    "itemStyle": {"color": "#20c997"},
                    "connectNulls": True,
                    "markArea": {"silent": True, "label": {"show": False}, "data": "{{anomaly_mark_areas}}"},
                },
                {
                    "name": "Warnings",
                    "type": "scatter",
                    "data": "{{warning_points}}",
                    "symbolSize": 8,
                    "itemStyle": {"color": "#ffc107"},
                    "encode": {"x": 0, "y": 1},
                },
                {
                    "name": "Outliers",
                    "type": "scatter",
                    "data": "{{outlier_points}}",
                    "symbolSize": 10,
                    "itemStyle": {"color": "#dc3545"},
                    "encode": {"x": 0, "y": 1},
                },
            ],
        },
    },
    "gauge_kpi": {
        "id": "gauge_kpi",
        "name": "KPI Gauge",
        "description": "Single-value gauge for KPI monitoring (SLA %, uptime %)",
        "icon": "bi-speedometer",
        "query_shape": "Columns: single numeric value",
        "sample_sql": (
            "SELECT\n"
            "  round(100.0 * countIf(StatusCode = 'STATUS_CODE_OK') / count(), 2) AS value\n"
            "FROM otel_traces\n"
            "WHERE Timestamp > now() - interval 1 hour"
        ),
        "drilldown": {
            "target": "traces",
            "label": "Open source traces",
        },
        "min_columns": 1,
        "max_columns": 1,
        "column_roles": {"value": 0},
        "echarts_option_template": {
            "series": [
                {
                    "type": "gauge",
                    "progress": {"itemStyle": {"color": "#0d6efd"}},
                    "axisLine": {
                        "lineStyle": {
                            "color": [[0.3, "#dc3545"], [0.7, "#ffc107"], [1, "#28a745"]],
                            "width": 30,
                        }
                    },
                    "splitLine": {"distance": 8},
                    "axisTick": {"distance": 8},
                    "axisLabel": {"color": "#adb5bd"},
                    "detail": {"valueAnimation": True, "formatter": "{value}%", "color": "#adb5bd"},
                    "data": [{"value": "{{value_first}}", "name": "Current"}],
                    "min": 0,
                    "max": 100,
                }
            ]
        },
    },
    "custom_echarts": {
        "id": "custom_echarts",
        "name": "Custom ECharts",
        "description": "Bring your own SQL, mapping JSON, and raw ECharts option JSON.",
        "icon": "bi-code-slash",
        "query_shape": "Any SELECT result set",
        "sample_sql": "SELECT toDateTime('2024-01-01 00:00:00') AS time, 1 AS value",
        "min_columns": 0,
        "column_roles": {},
        "echarts_option_template": {
            "tooltip": {"trigger": "axis"},
            "xAxis": {"type": "time"},
            "yAxis": {"type": "value"},
            "series": [
                {
                    "name": "Value",
                    "type": "line",
                    "data": "{{points}}",
                    "showSymbol": False,
                    "smooth": True,
                }
            ],
        },
    },
}

_QUERY_DENY_PATTERN = re.compile(
    r"\b(INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|TRUNCATE|REPLACE|RENAME|ATTACH|DETACH|GRANT|REVOKE)\b",
    re.IGNORECASE,
)


def _validate_chart_query(query: str) -> str | None:
    """Return an error message if the query is not a safe SELECT, otherwise None."""
    stripped = query.strip()
    if not stripped:
        return "Query cannot be empty"
    upper = stripped.upper()
    if not (upper.startswith("SELECT") or upper.startswith("WITH")):
        return "Only SELECT queries are allowed"
    if _QUERY_DENY_PATTERN.search(stripped):
        return "Query contains a disallowed keyword"
    return None


def _sql_literal(value: object) -> str:
    return "'" + str(value).replace("'", "''") + "'"


def _coerce_positive_int(raw: object, default_value: int, min_value: int, max_value: int) -> int:
    try:
        parsed = int(str(raw))
    except (TypeError, ValueError):
        return default_value
    return max(min_value, min(max_value, parsed))


def _default_chart_spec(template_id: str = "derived_signal_overlay") -> dict[str, object]:
    if template_id == "custom_echarts":
        return {
            "template_id": template_id,
            "sql": {
                "mode": "raw",
                "override_sql": "SELECT toDateTime('2024-01-01 00:00:00') AS time, 1 AS value",
            },
            "data": {
                "source_view": "v_derived_signals_anomaly",
                "service": "",
                "signal_source": "traces",
                "signal_name": "trace_volume",
                "metric_name": "",
                "attr_fp": "",
                "window_hours": 6,
                "limit": 1000,
            },
            "visual": {
                "zoom_inside": True,
                "zoom_slider": False,
                "zoom_start_pct": 0,
                "zoom_end_pct": 100,
                "legend_show": True,
                "smooth_line": True,
                "value_color": "",
                "role_map": {},
                "custom_mapping_json": json.dumps({"points": {"from": "rows"}}, ensure_ascii=False),
                "custom_option_json": json.dumps(
                    {
                        "tooltip": {"trigger": "axis"},
                        "xAxis": {"type": "time"},
                        "yAxis": {"type": "value"},
                        "series": [
                            {
                                "name": "Value",
                                "type": "line",
                                "data": "{{points}}",
                                "showSymbol": False,
                                "smooth": True,
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
            },
        }

    return {
        "template_id": template_id,
        "sql": {"mode": "builder", "override_sql": ""},
        "data": {
            "source_view": "v_derived_signals_anomaly",
            "service": "",
            "signal_source": "traces",
            "signal_name": "trace_volume",
            "metric_name": "",
            "attr_fp": "",
            "window_hours": 6,
            "limit": 1000,
        },
        "visual": {
            "zoom_inside": True,
            "zoom_slider": False,
            "zoom_start_pct": 0,
            "zoom_end_pct": 100,
            "legend_show": True,
            "smooth_line": True,
            "value_color": "",
            "role_map": {},
        },
    }


def _build_raw_chart_spec(template_id: str, query: str, options_json: str = "") -> dict[str, object]:
    try:
        parsed = json.loads(options_json) if options_json else {}
        if isinstance(parsed, dict):
            spec_candidate = parsed.get("chart_spec")
            if isinstance(spec_candidate, dict):
                return _normalize_chart_spec(spec_candidate)
    except Exception:
        pass

    spec = _default_chart_spec(template_id)
    spec["template_id"] = template_id
    spec["sql"] = {"mode": "raw", "override_sql": query}
    return spec


def _normalize_chart_spec(spec_raw: object) -> dict[str, object]:
    base = _default_chart_spec()
    raw = spec_raw if isinstance(spec_raw, dict) else {}

    template_id = str(raw.get("template_id") or base.get("template_id") or "time_series_percentiles").strip()
    if template_id not in CHART_TEMPLATES:
        raise ValueError(f"Unknown template: {template_id}")

    normalized = _default_chart_spec(template_id)
    normalized["template_id"] = template_id

    sql_raw = raw.get("sql") if isinstance(raw.get("sql"), dict) else {}
    sql_mode = str(sql_raw.get("mode") if isinstance(sql_raw, dict) else "builder").strip().lower()
    if sql_mode not in {"builder", "raw"}:
        raise ValueError("sql.mode must be 'builder' or 'raw'")
    normalized["sql"] = {
        "mode": sql_mode,
        "override_sql": str(sql_raw.get("override_sql") if isinstance(sql_raw, dict) else ""),
    }

    data_raw = raw.get("data") if isinstance(raw.get("data"), dict) else {}
    normalized_data = normalized.get("data")
    if isinstance(normalized_data, dict) and isinstance(data_raw, dict):
        merged_data: dict[str, object] = dict(cast(dict[str, object], normalized_data))
        merged_data.update(data_raw)
        normalized["data"] = merged_data

    visual_raw = raw.get("visual") if isinstance(raw.get("visual"), dict) else {}
    normalized_visual = normalized.get("visual")
    merged_visual: dict[str, object] = (
        dict(cast(dict[str, object], normalized_visual)) if isinstance(normalized_visual, dict) else {}
    )
    if isinstance(visual_raw, dict):
        merged_visual.update(visual_raw)

    role_map_raw = merged_visual.get("role_map")
    role_map: dict[str, str] = {}
    if isinstance(role_map_raw, dict):
        role_map_raw_dict = cast(dict[object, object], role_map_raw)
        for role, col_name in role_map_raw_dict.items():
            role_name = str(role).strip()
            mapped = str(col_name).strip()
            if role_name and mapped:
                role_map[role_name] = mapped
    merged_visual["role_map"] = role_map
    normalized["visual"] = merged_visual

    return normalized


def _compile_builder_sql(template_id: str, data: dict[str, object]) -> str:
    if template_id == "custom_echarts":
        raise ValueError("custom_echarts requires sql.mode='raw'")

    source_view = str(data.get("source_view") or "v_derived_signals_anomaly").strip()
    supported_sources = {
        "v_derived_signals_anomaly",
        "v_otel_metrics_anomaly",
        "otel_metrics_gauge",
        "otel_metrics_sum",
        "otel_metrics_histogram",
        "otel_logs",
        "otel_traces",
        "sobs_error_resolutions",
    }
    if source_view not in supported_sources:
        raise ValueError("Unsupported source for builder mode")

    service = str(data.get("service") or "").strip()
    signal_source = str(data.get("signal_source") or "").strip()
    signal_name = str(data.get("signal_name") or "").strip()
    metric_name = str(data.get("metric_name") or "").strip()
    attr_fp = str(data.get("attr_fp") or "").strip()
    window_hours = _coerce_positive_int(data.get("window_hours"), 6, 1, 168)
    limit = _coerce_positive_int(data.get("limit"), 1000, 1, 2000)

    def _default_source_label() -> str:
        if source_view in {"otel_logs"}:
            return "logs"
        if source_view in {"otel_traces"}:
            return "traces"
        if source_view in {"sobs_error_resolutions"}:
            return "errors"
        if source_view == "v_derived_signals_anomaly":
            return signal_source or "derived"
        return "metrics"

    def _default_signal_label() -> str:
        if signal_name:
            return signal_name
        if metric_name:
            return metric_name
        if source_view == "otel_logs":
            return "log_volume"
        if source_view == "otel_traces":
            return "trace_volume"
        if source_view == "sobs_error_resolutions":
            return "resolved_error_volume"
        return "value"

    def _build_series_sql() -> str:
        if source_view == "v_derived_signals_anomaly":
            where_parts: list[str] = [f"time >= now() - INTERVAL {window_hours} HOUR"]
            if service:
                where_parts.append(f"ServiceName = {_sql_literal(service)}")
            if attr_fp:
                where_parts.append(f"AttrFingerprint = {_sql_literal(attr_fp)}")
            if signal_source:
                where_parts.append(f"SignalSource = {_sql_literal(signal_source)}")
            if signal_name:
                where_parts.append(f"SignalName = {_sql_literal(signal_name)}")
            where_clause = " AND\n    ".join(where_parts)
            return (
                "SELECT\n"
                "  time,\n"
                "  value,\n"
                "  baseline_mean,\n"
                "  baseline_lower,\n"
                "  baseline_upper,\n"
                "  anomaly_state,\n"
                "  anomaly_score\n"
                "FROM v_derived_signals_anomaly\n"
                f"WHERE {where_clause}"
            )

        if source_view == "v_otel_metrics_anomaly":
            where_parts = [f"time >= now() - INTERVAL {window_hours} HOUR"]
            if service:
                where_parts.append(f"ServiceName = {_sql_literal(service)}")
            if metric_name:
                where_parts.append(f"MetricName = {_sql_literal(metric_name)}")
            if attr_fp:
                where_parts.append(f"AttrFingerprint = {_sql_literal(attr_fp)}")
            where_clause = " AND\n    ".join(where_parts)
            return (
                "SELECT\n"
                "  time,\n"
                "  value,\n"
                "  baseline_mean,\n"
                "  baseline_lower,\n"
                "  baseline_upper,\n"
                "  anomaly_state,\n"
                "  anomaly_score\n"
                "FROM v_otel_metrics_anomaly\n"
                f"WHERE {where_clause}"
            )

        if source_view in {"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"}:
            if source_view == "otel_metrics_histogram":
                value_expr = "if(Count = 0, 0.0, Sum / toFloat64(Count))"
            else:
                value_expr = "Value"
            where_parts = [f"TimeUnixMs >= now() - INTERVAL {window_hours} HOUR"]
            if service:
                where_parts.append(f"ServiceName = {_sql_literal(service)}")
            if metric_name:
                where_parts.append(f"MetricName = {_sql_literal(metric_name)}")
            if attr_fp:
                where_parts.append(f"AttrFingerprint = {_sql_literal(attr_fp)}")
            where_clause = " AND\n    ".join(where_parts)
            return (
                "WITH per_minute AS (\n"
                "  SELECT\n"
                "    toStartOfMinute(TimeUnixMs) AS time,\n"
                "    avg(toFloat64(" + value_expr + ")) AS value\n"
                f"  FROM {source_view}\n"
                f"  WHERE {where_clause}\n"
                "  GROUP BY time\n"
                "), scored AS (\n"
                "  SELECT\n"
                "    time,\n"
                "    value,\n"
                "    avg(value) OVER (\n"
                "      ORDER BY time\n"
                "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
                "    ) AS baseline_mean,\n"
                "    stddevPop(value) OVER (\n"
                "      ORDER BY time\n"
                "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
                "    ) AS baseline_stddev\n"
                "  FROM per_minute\n"
                ")\n"
                "SELECT\n"
                "  time,\n"
                "  value,\n"
                "  baseline_mean,\n"
                "  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n"
                "  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n"
                "  if(\n"
                "    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n"
                "    'outlier',\n"
                "    'normal'\n"
                "  ) AS anomaly_state,\n"
                "  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n"
                "FROM scored"
            )

        if source_view == "otel_logs":
            where_parts = [f"TimestampTime >= now() - INTERVAL {window_hours} HOUR"]
            if service:
                where_parts.append(f"ServiceName = {_sql_literal(service)}")
            where_clause = " AND\n    ".join(where_parts)
            return (
                "WITH per_minute AS (\n"
                "  SELECT\n"
                "    toStartOfMinute(TimestampTime) AS time,\n"
                "    count() AS value\n"
                "  FROM otel_logs\n"
                f"  WHERE {where_clause}\n"
                "  GROUP BY time\n"
                "), scored AS (\n"
                "  SELECT\n"
                "    time,\n"
                "    toFloat64(value) AS value,\n"
                "    avg(toFloat64(value)) OVER (\n"
                "      ORDER BY time\n"
                "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
                "    ) AS baseline_mean,\n"
                "    stddevPop(toFloat64(value)) OVER (\n"
                "      ORDER BY time\n"
                "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
                "    ) AS baseline_stddev\n"
                "  FROM per_minute\n"
                ")\n"
                "SELECT\n"
                "  time,\n"
                "  value,\n"
                "  baseline_mean,\n"
                "  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n"
                "  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n"
                "  if(\n"
                "    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n"
                "    'outlier',\n"
                "    'normal'\n"
                "  ) AS anomaly_state,\n"
                "  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n"
                "FROM scored"
            )

        if source_view == "otel_traces":
            where_parts = [f"TimestampTime >= now() - INTERVAL {window_hours} HOUR"]
            if service:
                where_parts.append(f"ServiceName = {_sql_literal(service)}")
            where_clause = " AND\n    ".join(where_parts)
            return (
                "WITH per_minute AS (\n"
                "  SELECT\n"
                "    toStartOfMinute(TimestampTime) AS time,\n"
                "    count() AS value\n"
                "  FROM otel_traces\n"
                f"  WHERE {where_clause}\n"
                "  GROUP BY time\n"
                "), scored AS (\n"
                "  SELECT\n"
                "    time,\n"
                "    toFloat64(value) AS value,\n"
                "    avg(toFloat64(value)) OVER (\n"
                "      ORDER BY time\n"
                "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
                "    ) AS baseline_mean,\n"
                "    stddevPop(toFloat64(value)) OVER (\n"
                "      ORDER BY time\n"
                "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
                "    ) AS baseline_stddev\n"
                "  FROM per_minute\n"
                ")\n"
                "SELECT\n"
                "  time,\n"
                "  value,\n"
                "  baseline_mean,\n"
                "  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n"
                "  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n"
                "  if(\n"
                "    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n"
                "    'outlier',\n"
                "    'normal'\n"
                "  ) AS anomaly_state,\n"
                "  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n"
                "FROM scored"
            )

        where_clause = f"ResolvedAt >= now() - INTERVAL {window_hours} HOUR"
        return (
            "WITH per_minute AS (\n"
            "  SELECT\n"
            "    toStartOfMinute(ResolvedAt) AS time,\n"
            "    count() AS value\n"
            "  FROM sobs_error_resolutions\n"
            f"  WHERE {where_clause}\n"
            "  GROUP BY time\n"
            "), scored AS (\n"
            "  SELECT\n"
            "    time,\n"
            "    toFloat64(value) AS value,\n"
            "    avg(toFloat64(value)) OVER (\n"
            "      ORDER BY time\n"
            "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
            "    ) AS baseline_mean,\n"
            "    stddevPop(toFloat64(value)) OVER (\n"
            "      ORDER BY time\n"
            "      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n"
            "    ) AS baseline_stddev\n"
            "  FROM per_minute\n"
            ")\n"
            "SELECT\n"
            "  time,\n"
            "  value,\n"
            "  baseline_mean,\n"
            "  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n"
            "  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n"
            "  if(\n"
            "    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n"
            "    'outlier',\n"
            "    'normal'\n"
            "  ) AS anomaly_state,\n"
            "  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n"
            "FROM scored"
        )

    series_sql = _build_series_sql()

    if template_id == "derived_signal_overlay":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT\n"
            "  time,\n"
            f"  {_sql_literal(service or 'all')} AS service,\n"
            f"  {_sql_literal(_default_source_label())} AS source,\n"
            f"  {_sql_literal(_default_signal_label())} AS signal,\n"
            f"  {_sql_literal(attr_fp)} AS attr_fp,\n"
            "  value,\n"
            "  toUInt32(1) AS sample_count,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state,\n"
            "  anomaly_score\n"
            "FROM series\n"
            "ORDER BY time\n"
            f"LIMIT {limit}"
        )

    if template_id == "anomaly_overlay":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT\n"
            "  time,\n"
            "  value,\n"
            "  baseline_mean,\n"
            "  baseline_lower,\n"
            "  baseline_upper,\n"
            "  anomaly_state\n"
            "FROM series\n"
            "ORDER BY time\n"
            f"LIMIT {limit}"
        )

    if template_id == "dual_axis_anomaly":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT\n"
            "  time,\n"
            "  value AS metric,\n"
            "  anomaly_score\n"
            "FROM series\n"
            "ORDER BY time\n"
            f"LIMIT {limit}"
        )

    if template_id == "time_series_percentiles":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT\n"
            "  time,\n"
            "  value,\n"
            "  baseline_upper AS p95,\n"
            "  greatest(baseline_upper, value) AS p99\n"
            "FROM series\n"
            "ORDER BY time\n"
            f"LIMIT {limit}"
        )

    if template_id == "heatmap":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT\n"
            f"  {_sql_literal(service or 'all')} AS x_category,\n"
            "  toStartOfFiveMinutes(time) AS y_category,\n"
            "  avg(value) AS value\n"
            "FROM series\n"
            "GROUP BY y_category\n"
            "ORDER BY y_category\n"
            f"LIMIT {limit}"
        )

    if template_id == "box_plot":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT\n"
            f"  {_sql_literal(_default_signal_label())} AS dimension,\n"
            "  min(value) AS min,\n"
            "  quantile(0.25)(value) AS q1,\n"
            "  quantile(0.5)(value) AS median,\n"
            "  quantile(0.75)(value) AS q3,\n"
            "  max(value) AS max\n"
            "FROM series"
        )

    if template_id == "gauge_kpi":
        return (
            "WITH series AS (\n"
            f"{series_sql}\n"
            ")\n"
            "SELECT round(100.0 * avg(if(anomaly_state = 'normal', 1.0, 0.0)), 2) AS value\n"
            "FROM series"
        )

    raise ValueError(f"Builder mode does not support template: {template_id}")


def _compile_chart_spec(spec_raw: object) -> tuple[str, str, dict[str, object]]:
    spec = _normalize_chart_spec(spec_raw)

    template_id = str(spec.get("template_id") or "time_series_percentiles").strip()

    sql_block = spec.get("sql") if isinstance(spec.get("sql"), dict) else {}
    sql_mode = str(sql_block.get("mode") if isinstance(sql_block, dict) else "builder").strip().lower()

    if sql_mode == "raw":
        query = str(sql_block.get("override_sql") if isinstance(sql_block, dict) else "").strip()
    else:
        if template_id == "custom_echarts":
            raise ValueError("custom_echarts requires sql.mode='raw'")
        data = spec.get("data") if isinstance(spec.get("data"), dict) else {}
        query = _compile_builder_sql(template_id, data if isinstance(data, dict) else {})

    err = _validate_chart_query(query)
    if err:
        raise ValueError(err)

    return template_id, query, spec


def _resolve_template_role_indices(
    template_id: str,
    template: dict[str, object],
    columns: list[str],
    spec: dict[str, object] | None,
) -> dict[str, int]:
    raw_roles_raw = template.get("column_roles") if isinstance(template.get("column_roles"), dict) else {}
    raw_roles = cast(dict[object, object], raw_roles_raw)
    role_indices: dict[str, int] = {}
    for role, idx_raw in raw_roles.items():
        role_name = str(role)
        if isinstance(idx_raw, (int, float)):
            role_indices[role_name] = int(idx_raw)

    if not spec:
        return role_indices

    visual = spec.get("visual") if isinstance(spec.get("visual"), dict) else {}
    role_map_raw = visual.get("role_map") if isinstance(visual, dict) else {}
    if not isinstance(role_map_raw, dict):
        return role_indices
    role_map_raw_dict = cast(dict[object, object], role_map_raw)

    col_index_by_name = {name: idx for idx, name in enumerate(columns)}
    lower_name_to_index: dict[str, int] = {}
    for idx, name in enumerate(columns):
        lower = name.lower()
        if lower not in lower_name_to_index:
            lower_name_to_index[lower] = idx

    for role, mapped_col in role_map_raw_dict.items():
        role_name = str(role).strip()
        col_name = str(mapped_col).strip()
        if not role_name or not col_name:
            continue
        if role_name not in role_indices:
            raise ValueError(f"Unknown role '{role_name}' for template {template_id}")

        if col_name in col_index_by_name:
            role_indices[role_name] = col_index_by_name[col_name]
            continue

        lowered = col_name.lower()
        if lowered in lower_name_to_index:
            role_indices[role_name] = lower_name_to_index[lowered]
            continue

        raise ValueError(f"Role '{role_name}' maps to unknown column '{col_name}'")

    return role_indices


def _parse_bool(value: object, default_value: bool) -> bool:
    if isinstance(value, bool):
        return value
    if value is None:
        return default_value
    raw = str(value).strip().lower()
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    return default_value


def _apply_chart_spec_visual_overrides(template_id: str, option: dict, spec: dict[str, object]) -> dict:
    if template_id == "custom_echarts":
        return option

    visual = spec.get("visual") if isinstance(spec.get("visual"), dict) else {}
    if not isinstance(visual, dict):
        return option

    legend_show = _parse_bool(visual.get("legend_show"), True)
    if isinstance(option.get("legend"), dict):
        option["legend"]["show"] = legend_show

    zoom_inside = _parse_bool(visual.get("zoom_inside"), True)
    zoom_slider = _parse_bool(visual.get("zoom_slider"), False)
    data_zoom = option.get("dataZoom") if isinstance(option.get("dataZoom"), list) else []
    zoom_start = _coerce_positive_int(visual.get("zoom_start_pct"), 0, 0, 100)
    zoom_end = _coerce_positive_int(visual.get("zoom_end_pct"), 100, 0, 100)
    next_data_zoom: list[dict[str, object]] = []
    if zoom_inside:
        next_data_zoom.append(
            {
                "type": "inside",
                "xAxisIndex": 0,
                "filterMode": "none",
                "start": zoom_start,
                "end": max(zoom_start, zoom_end),
            }
        )
    if zoom_slider:
        next_data_zoom.append(
            {
                "type": "slider",
                "xAxisIndex": 0,
                "start": zoom_start,
                "end": max(zoom_start, zoom_end),
                "height": 16,
                "bottom": 30,
                "borderColor": "#495057",
                "fillerColor": "rgba(13, 110, 253, 0.20)",
                "handleStyle": {"color": "#0d6efd"},
            }
        )
    option["dataZoom"] = next_data_zoom if next_data_zoom else data_zoom

    smooth_line = _parse_bool(visual.get("smooth_line"), True)
    value_color = str(visual.get("value_color") or "").strip()
    series = option.get("series")
    if isinstance(series, list):
        for s in series:
            if not isinstance(s, dict):
                continue
            if str(s.get("name", "")) != "Value":
                continue
            if "type" in s and str(s.get("type")) == "line":
                s["smooth"] = smooth_line
            if value_color:
                line_style: dict[str, object] = {}
                item_style: dict[str, object] = {}
                existing_line_style = s.get("lineStyle")
                existing_item_style = s.get("itemStyle")
                if isinstance(existing_line_style, dict):
                    for key, val in existing_line_style.items():
                        line_style[str(key)] = val
                if isinstance(existing_item_style, dict):
                    for key, val in existing_item_style.items():
                        item_style[str(key)] = val
                line_style["color"] = value_color
                item_style["color"] = value_color
                s["lineStyle"] = line_style
                s["itemStyle"] = item_style

    # Template guard for future template-specific visual overrides.
    _ = template_id
    return option


def _infer_column_types(columns: list[str], rows: list[list[object]]) -> list[str]:
    inferred: list[str] = []
    for idx, _col in enumerate(columns):
        detected = "null"
        for row in rows:
            if idx >= len(row):
                continue
            value = row[idx]
            if value is None:
                continue
            detected = type(value).__name__
            break
        inferred.append(detected)
    return inferred


def _public_dashboard_query_error(exc: Exception) -> str:
    """Extract a concise, user-safe error message from a database exception."""
    raw = str(exc).strip()
    message = raw.splitlines()[0].strip()
    message = re.sub(r"^Code:\s*\d+\.\s*DB::Exception:\s*", "", message)
    message = re.sub(r"^DB::Exception:\s*", "", message)
    message = message.split(": while executing function", 1)[0].strip()
    message = message.split(". Stack trace", 1)[0].strip()
    if not message:
        return "Query execution failed"
    if (
        any(code in raw for code in ("NO_COMMON_TYPE", "TYPE_MISMATCH"))
        and "Check casts and column types." not in message
    ):
        message = f"{message}. Check casts and column types."
    if len(message) > 280:
        message = message[:277].rstrip() + "..."
    return message


def _deep_substitute(obj: object, bindings: dict) -> object:
    """Recursively substitute {{key}} placeholders in a JSON object."""
    if isinstance(obj, dict):
        return {k: _deep_substitute(v, bindings) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_deep_substitute(item, bindings) for item in obj]
    if isinstance(obj, str):
        # Replace {{key}} with binding value
        for key, value in bindings.items():
            placeholder = f"{{{{{key}}}}}"
            if placeholder in obj:
                return value if value is not None else obj
        return obj
    return obj


def _extract_bindings(
    template: dict,
    columns: list[str],
    rows: list,
    role_indices: dict[str, int] | None = None,
) -> dict:  # type: ignore
    """Extract data bindings from query results based on column roles."""
    column_roles = role_indices if isinstance(role_indices, dict) else template.get("column_roles", {})
    bindings: dict[str, object] = {}

    # Basic extraction: for each role, get the column data
    for role, col_idx_raw in column_roles.items():
        col_idx = int(col_idx_raw) if isinstance(col_idx_raw, (int, float)) else 0
        if col_idx < len(columns):
            values = [row[col_idx] if isinstance(row, (list, tuple)) else row.get(columns[col_idx]) for row in rows]
            bindings[role] = values

    # Special bindings for common patterns
    if "time" in bindings:
        bindings["time"] = bindings["time"]

    # For heatmap: extract unique X and Y, build matrix
    if "x_category" in bindings and "y_category" in bindings and "value" in bindings:
        x_vals = bindings["x_category"]
        y_vals = bindings["y_category"]
        v_vals = bindings["value"]
        if isinstance(x_vals, list) and isinstance(y_vals, list) and isinstance(v_vals, list):
            x_unique = sorted(set(x_vals))
            y_unique = sorted(set(y_vals))
            bindings["x_unique_values"] = x_unique
            bindings["y_unique_values"] = y_unique

            # Build heatmap matrix: row[x_idx][y_idx] = value
            heatmap_data = []
            for i, x_val in enumerate(x_unique):
                for j, y_val in enumerate(y_unique):
                    # Find row where x_category == x_val and y_category == y_val
                    for x, y, val in zip(x_vals, y_vals, v_vals):
                        if x == x_val and y == y_val:
                            heatmap_data.append([i, j, val])
                            break
            bindings["heatmap_data"] = heatmap_data
            v_nums = [v for v in v_vals if isinstance(v, (int, float))]
            bindings["value_min"] = min(v_nums) if v_nums else 0
            bindings["value_max"] = max(v_nums) if v_nums else 1

    # For box plot: build [min, q1, median, q3, max] array
    if "min" in bindings and "max" in bindings:
        min_vals = bindings["min"]
        q1_vals = bindings["q1"]
        med_vals = bindings["median"]
        q3_vals = bindings["q3"]
        max_vals = bindings["max"]
        if all(isinstance(v, list) for v in [min_vals, q1_vals, med_vals, q3_vals, max_vals]):
            boxplot_data = [
                [_v[0], _v[1], _v[2], _v[3], _v[4]]  # type: ignore
                for _v in zip(min_vals, q1_vals, med_vals, q3_vals, max_vals)  # type: ignore
            ]
            bindings["boxplot_data"] = boxplot_data
            bindings["dimension_values"] = bindings.get("dimension", [])

    # For gauge: get first value
    if "value" in bindings and isinstance(bindings["value"], list) and bindings["value"]:
        v_list = bindings["value"]
        if isinstance(v_list, list) and v_list:
            bindings["value_first"] = v_list[0]

    # For anomaly overlays: build per-point symbol sizes and colors from the effective or statistical state.
    state_binding = bindings.get("effective_state", bindings.get("anomaly_state"))
    if isinstance(state_binding, list):
        states = state_binding
        _state_colors = {"outlier": "#dc3545", "warning": "#ffc107", "normal": "#0d6efd"}
        _state_sizes = {"outlier": 10, "warning": 7, "normal": 4}
        bindings["anomaly_point_color"] = [_state_colors.get(str(s), "#0d6efd") for s in states]
        bindings["anomaly_symbol_size"] = [_state_sizes.get(str(s), 4) for s in states]

    # Derived signal overlays: choose chart style by signal semantics.
    if str(template.get("id", "")) == "derived_signal_overlay":
        bindings["value_axis_min"] = "dataMin"
        bindings["value_axis_max"] = "dataMax"
        bindings["zoom_start_pct"] = 0
        bindings["signal_summary"] = ""
        bindings["y_axis_name"] = "Value"

        signal_binding = bindings.get("signal")
        signal_name = ""
        if isinstance(signal_binding, list) and signal_binding:
            signal_name = str(signal_binding[0]).lower()

        if "ratio" in signal_name:
            bindings["value_axis_min"] = 0
            bindings["value_axis_max"] = 1
        elif any(token in signal_name for token in ("volume", "count", "latency", "duration", "p95", "p99")):
            bindings["value_axis_min"] = 0

        time_values = bindings.get("time")
        value_values = bindings.get("value")
        baseline_mean_values = bindings.get("baseline_mean")
        baseline_lower_values = bindings.get("baseline_lower")
        baseline_upper_values = bindings.get("baseline_upper")
        effective_states = bindings.get("effective_state", bindings.get("anomaly_state"))

        if (
            isinstance(time_values, list)
            and isinstance(value_values, list)
            and isinstance(baseline_mean_values, list)
            and isinstance(baseline_lower_values, list)
            and isinstance(baseline_upper_values, list)
        ):
            state_to_rank = {"normal": 0, "warning": 1, "outlier": 2}
            rank_series: list[int] = []
            if isinstance(effective_states, list):
                rank_series = [state_to_rank.get(str(s), 0) for s in effective_states]
            if not rank_series:
                rank_series = [0 for _ in value_values]

            use_delta_mode = "ratio" not in signal_name
            plot_values: list[float] = []
            plot_baseline: list[float] = []
            plot_lower: list[float] = []
            plot_upper: list[float] = []
            if use_delta_mode:
                bindings["y_axis_name"] = "Delta %"
                for idx in range(
                    min(
                        len(value_values),
                        len(baseline_mean_values),
                        len(baseline_lower_values),
                        len(baseline_upper_values),
                    )
                ):
                    base = float(baseline_mean_values[idx])
                    val = float(value_values[idx])
                    low = float(baseline_lower_values[idx])
                    up = float(baseline_upper_values[idx])
                    if abs(base) < 1e-9:
                        plot_values.append(0.0)
                        plot_baseline.append(0.0)
                        plot_lower.append(0.0)
                        plot_upper.append(0.0)
                    else:
                        denom = abs(base)
                        plot_values.append(((val - base) / denom) * 100.0)
                        plot_baseline.append(0.0)
                        plot_lower.append(((low - base) / denom) * 100.0)
                        plot_upper.append(((up - base) / denom) * 100.0)
                if plot_values:
                    min_bound = min(plot_lower + plot_values)
                    max_bound = max(plot_upper + plot_values)
                    span = max(5.0, (max_bound - min_bound) * 0.15)
                    bindings["value_axis_min"] = round(min_bound - span, 2)
                    bindings["value_axis_max"] = round(max_bound + span, 2)
            else:
                plot_values = [float(v) for v in value_values]
                plot_baseline = [float(v) for v in baseline_mean_values]
                plot_lower = [max(0.0, float(v)) for v in baseline_lower_values]
                plot_upper = [float(v) for v in baseline_upper_values]

            value_points = [
                [time_values[idx], plot_values[idx], rank_series[idx] if idx < len(rank_series) else 0]
                for idx in range(min(len(time_values), len(plot_values)))
            ]
            baseline_mean_points = [
                [time_values[idx], plot_baseline[idx]] for idx in range(min(len(time_values), len(plot_baseline)))
            ]
            baseline_lower_points = [
                [time_values[idx], plot_lower[idx]] for idx in range(min(len(time_values), len(plot_lower)))
            ]
            baseline_upper_points = [
                [
                    time_values[idx],
                    max(0.0, float(plot_upper[idx]) - float(plot_lower[idx])),
                ]
                for idx in range(min(len(time_values), len(plot_upper), len(plot_lower)))
            ]

            mark_areas: list[list[dict[str, object]]] = []
            warning_points = [pt[:2] for pt in value_points if len(pt) >= 3 and int(pt[2]) == 1]
            outlier_points = [pt[:2] for pt in value_points if len(pt) >= 3 and int(pt[2]) == 2]
            if isinstance(effective_states, list) and time_values:
                i = 0
                while i < min(len(effective_states), len(time_values)):
                    state = str(effective_states[i])
                    if state == "normal":
                        i += 1
                        continue
                    start_idx = i
                    while i + 1 < len(effective_states) and str(effective_states[i + 1]) == state:
                        i += 1
                    end_idx = i
                    shade = "rgba(255, 193, 7, 0.15)" if state == "warning" else "rgba(220, 53, 69, 0.15)"
                    mark_areas.append(
                        [
                            {
                                "name": state.title(),
                                "itemStyle": {"color": shade},
                                "xAxis": time_values[start_idx],
                            },
                            {"xAxis": time_values[end_idx]},
                        ]
                    )
                    i += 1

            latest_value = float(value_values[-1]) if value_values else 0.0
            latest_baseline = float(baseline_mean_values[-1]) if baseline_mean_values else 0.0
            delta_pct = 0.0
            if abs(latest_baseline) > 1e-9:
                delta_pct = ((latest_value - latest_baseline) / abs(latest_baseline)) * 100.0
            warning_count = len(warning_points)
            outlier_count = len(outlier_points)
            bindings["signal_summary"] = (
                f"now {latest_value:.1f} | baseline {latest_baseline:.1f} | "
                f"Δ {delta_pct:+.0f}% | warn {warning_count} | outlier {outlier_count}"
            )

            bindings["value_points"] = value_points
            bindings["baseline_mean_points"] = baseline_mean_points
            bindings["baseline_lower_points"] = baseline_lower_points
            bindings["baseline_upper_points"] = baseline_upper_points
            bindings["anomaly_mark_areas"] = mark_areas
            bindings["warning_points"] = warning_points
            bindings["outlier_points"] = outlier_points

    return bindings  # type: ignore


def _format_drilldown_time(value: object) -> str:
    """Return a canonical ISO-8601 UTC timestamp string for drilldown URLs."""
    if isinstance(value, datetime):
        dt = value.astimezone(timezone.utc) if value.tzinfo else value.replace(tzinfo=timezone.utc)
        return dt.strftime("%Y-%m-%dT%H:%M:%SZ")

    raw = str(value or "").strip()
    if not raw:
        return ""

    try:
        dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        normalized = _normalize_ch_timestamp(raw)
        try:
            dt = datetime.fromisoformat(normalized)
        except ValueError:
            return raw

    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    else:
        dt = dt.astimezone(timezone.utc)
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def _attach_drilldown_metadata(template: dict, bindings: dict[str, object], option: dict) -> dict:
    """Annotate rendered series data with canonical drilldown metadata."""
    drilldown = template.get("drilldown")
    if not isinstance(drilldown, dict):
        return option

    series = option.get("series")
    if not isinstance(series, list):
        return option

    template_id = str(template.get("id", ""))
    bucket_seconds = drilldown.get("bucket_seconds")

    if template_id in {"time_series_percentiles", "dual_axis_anomaly", "anomaly_overlay", "derived_signal_overlay"}:
        time_values = bindings.get("time")
        if isinstance(time_values, list):
            # For anomaly_overlay, also inject per-point anomaly state and score
            is_anomaly_template = template_id in {"anomaly_overlay", "derived_signal_overlay"}
            anomaly_states = bindings.get("anomaly_state") if is_anomaly_template else None
            anomaly_scores = bindings.get("anomaly_score") if is_anomaly_template else None
            rule_states = bindings.get("rule_state") if template_id == "derived_signal_overlay" else None
            rule_names = bindings.get("rule_name") if template_id == "derived_signal_overlay" else None
            rule_reasons = bindings.get("rule_reason") if template_id == "derived_signal_overlay" else None
            effective_states = bindings.get("effective_state") if template_id == "derived_signal_overlay" else None
            services = bindings.get("service") if template_id == "derived_signal_overlay" else None
            sources = bindings.get("source") if template_id == "derived_signal_overlay" else None
            signals = bindings.get("signal") if template_id == "derived_signal_overlay" else None
            attr_fps = bindings.get("attr_fp") if template_id == "derived_signal_overlay" else None
            for series_entry in series:
                if not isinstance(series_entry, dict):
                    continue
                data = series_entry.get("data")
                if not isinstance(data, list) or len(data) != len(time_values):
                    continue
                # For anomaly_overlay, inject state/score into Value series only
                is_value_series = is_anomaly_template and series_entry.get("name") == "Value"
                series_entry["data"] = [
                    {
                        "value": value,
                        "drilldown": {
                            "from_ts": _format_drilldown_time(time_values[idx]),
                            "window_s": bucket_seconds,
                            **(  # Inject anomaly metadata for Value series
                                {
                                    "_anomaly_state": (
                                        (
                                            anomaly_states[idx]  # type: ignore[index]
                                            if isinstance(anomaly_states, list) and idx < len(anomaly_states)
                                            else "normal"
                                        )
                                    ),
                                    "_anomaly_score": (
                                        (
                                            anomaly_scores[idx]  # type: ignore[index]
                                            if isinstance(anomaly_scores, list) and idx < len(anomaly_scores)
                                            else 0
                                        )
                                    ),
                                    **(
                                        {
                                            "_rule_state": (
                                                rule_states[idx]  # type: ignore[index]
                                                if isinstance(rule_states, list) and idx < len(rule_states)
                                                else "normal"
                                            ),
                                            "_rule_name": (
                                                rule_names[idx]  # type: ignore[index]
                                                if isinstance(rule_names, list) and idx < len(rule_names)
                                                else ""
                                            ),
                                            "_rule_reason": (
                                                rule_reasons[idx]  # type: ignore[index]
                                                if isinstance(rule_reasons, list) and idx < len(rule_reasons)
                                                else ""
                                            ),
                                            "_effective_state": (
                                                effective_states[idx]  # type: ignore[index]
                                                if isinstance(effective_states, list) and idx < len(effective_states)
                                                else "normal"
                                            ),
                                            "service": (
                                                services[idx]  # type: ignore[index]
                                                if isinstance(services, list) and idx < len(services)
                                                else ""
                                            ),
                                            "source": (
                                                sources[idx]  # type: ignore[index]
                                                if isinstance(sources, list) and idx < len(sources)
                                                else ""
                                            ),
                                            "signal": (
                                                signals[idx]  # type: ignore[index]
                                                if isinstance(signals, list) and idx < len(signals)
                                                else ""
                                            ),
                                            "attr_fp": (
                                                attr_fps[idx]  # type: ignore[index]
                                                if isinstance(attr_fps, list) and idx < len(attr_fps)
                                                else ""
                                            ),
                                        }
                                        if template_id == "derived_signal_overlay"
                                        else {}
                                    ),
                                }
                                if is_value_series
                                else {}
                            ),
                        },
                    }
                    for idx, value in enumerate(data)
                ]
        return option

    if template_id == "heatmap" and series:
        x_unique = bindings.get("x_unique_values")
        y_unique = bindings.get("y_unique_values")
        first_series = series[0]
        if isinstance(first_series, dict) and isinstance(x_unique, list) and isinstance(y_unique, list):
            data = first_series.get("data")
            if isinstance(data, list):
                drilldown_data = []
                for item in data:
                    if not (isinstance(item, list) and len(item) >= 3):
                        drilldown_data.append(item)
                        continue
                    x_idx = int(item[0])
                    y_idx = int(item[1])
                    from_value = y_unique[y_idx] if 0 <= y_idx < len(y_unique) else ""
                    service_value = x_unique[x_idx] if 0 <= x_idx < len(x_unique) else ""
                    drilldown_data.append(
                        {
                            "value": item,
                            "drilldown": {
                                "from_ts": _format_drilldown_time(from_value),
                                "window_s": bucket_seconds,
                                "service": service_value,
                            },
                        }
                    )
                first_series["data"] = drilldown_data
        return option

    return option


def _prepare_template_rows(
    template_id: str,
    columns: list[str],
    rows: list[dict[str, object]],
    role_indices: dict[str, int] | None = None,
) -> tuple[list[str], list[dict[str, object]]]:
    if template_id != "derived_signal_overlay":
        return columns, rows

    required_columns = [
        "time",
        "service",
        "source",
        "signal",
        "attr_fp",
        "value",
        "sample_count",
        "baseline_mean",
        "baseline_lower",
        "baseline_upper",
        "anomaly_state",
        "anomaly_score",
    ]
    if len(columns) < len(required_columns):
        return columns, rows

    def _col_for_role(role: str, fallback_idx: int) -> str:
        idx = fallback_idx
        if isinstance(role_indices, dict) and role in role_indices:
            idx = role_indices[role]
        if 0 <= idx < len(columns):
            return columns[idx]
        return columns[fallback_idx]

    role_columns = {
        "time": _col_for_role("time", 0),
        "service": _col_for_role("service", 1),
        "source": _col_for_role("source", 2),
        "signal": _col_for_role("signal", 3),
        "attr_fp": _col_for_role("attr_fp", 4),
        "value": _col_for_role("value", 5),
        "sample_count": _col_for_role("sample_count", 6),
        "baseline_mean": _col_for_role("baseline_mean", 7),
        "baseline_lower": _col_for_role("baseline_lower", 8),
        "baseline_upper": _col_for_role("baseline_upper", 9),
        "anomaly_state": _col_for_role("anomaly_state", 10),
        "anomaly_score": _col_for_role("anomaly_score", 11),
    }

    normalized_rows: list[dict[str, object]] = []
    for raw_row in rows:
        normalized_rows.append(
            {
                "time": raw_row.get(role_columns["time"]),
                "service": raw_row.get(role_columns["service"]),
                "source": raw_row.get(role_columns["source"]),
                "signal": raw_row.get(role_columns["signal"]),
                "attr_fp": raw_row.get(role_columns["attr_fp"]),
                "value": raw_row.get(role_columns["value"]),
                "sample_count": raw_row.get(role_columns["sample_count"]),
                "baseline_mean": raw_row.get(role_columns["baseline_mean"]),
                "baseline_lower": raw_row.get(role_columns["baseline_lower"]),
                "baseline_upper": raw_row.get(role_columns["baseline_upper"]),
                "anomaly_state": raw_row.get(role_columns["anomaly_state"]),
                "anomaly_score": raw_row.get(role_columns["anomaly_score"]),
            }
        )

    _annotate_rows_with_rules(
        normalized_rows,
        _load_anomaly_rules(get_db()),
        source_key="source",
        signal_key="signal",
        service_key="service",
        attr_fp_key="attr_fp",
        value_key="value",
        sample_count_key="sample_count",
        time_key="time",
    )

    prepared_columns = required_columns + ["rule_state", "rule_name", "rule_reason", "effective_state"]
    prepared_rows = [{column: row.get(column, "") for column in prepared_columns} for row in normalized_rows]
    return prepared_columns, prepared_rows


def _render_chart_from_template(
    template_id: str,
    columns: list[str],
    rows: list,
    spec: dict[str, object] | None = None,
) -> dict:  # type: ignore
    """
    Render chart option by substituting query results into template.

    Raises ValueError if template not found or columns don't match.
    """
    template = CHART_TEMPLATES.get(template_id)
    if not template:
        raise ValueError(f"Unknown template: {template_id}")

    if template_id == "custom_echarts":
        return _render_custom_echarts(template, columns, rows, spec)

    if not rows:
        return {
            "backgroundColor": "transparent",
            "textStyle": {"color": "#adb5bd"},
            "title": {
                "text": "No data for selected query/time window",
                "left": "center",
                "top": "middle",
                "textStyle": {"color": "#6c757d", "fontSize": 13, "fontWeight": 500},
            },
            "series": [],
            "xAxis": {"show": False},
            "yAxis": {"show": False},
        }

    # Validate column count
    min_cols_raw = template.get("min_columns", 0)
    min_cols = int(min_cols_raw) if isinstance(min_cols_raw, (int, float)) else 0
    max_cols_raw = template.get("max_columns")
    max_cols: int | None = int(max_cols_raw) if isinstance(max_cols_raw, (int, float)) else None
    if len(columns) < min_cols:
        raise ValueError(f"Template {template_id} requires at least {min_cols} columns, got {len(columns)}")
    if max_cols and len(columns) > max_cols:
        raise ValueError(f"Template {template_id} accepts maximum {max_cols} columns, got {len(columns)}")

    role_indices = _resolve_template_role_indices(template_id, template, columns, spec)

    if rows and isinstance(rows[0], dict):
        columns, rows = _prepare_template_rows(template_id, columns, rows, role_indices)

    # Extract bindings
    bindings = _extract_bindings(template, columns, rows, role_indices)

    # Substitute into template and add dark theme styling
    option = _deep_substitute(template["echarts_option_template"], bindings)
    if isinstance(option, dict):
        option = _attach_drilldown_metadata(template, bindings, option)
        # Ensure consistent transparent background across all templates
        if "backgroundColor" not in option:
            option["backgroundColor"] = "transparent"
        if "textStyle" not in option:
            option["textStyle"] = {"color": "#adb5bd"}
    return option  # type: ignore


def _parse_custom_json_config(raw: object, field_name: str) -> object:
    if isinstance(raw, (dict, list)):
        return raw
    if raw is None:
        return {}
    text = str(raw).strip()
    if not text:
        return {}
    try:
        return json.loads(text)
    except Exception as exc:
        raise ValueError(f"{field_name} must be valid JSON") from exc


def _resolve_custom_binding_expr(
    expr: object, columns: list[str], records: list[dict[str, object]], rows: list[list]
) -> object:
    if isinstance(expr, str):
        key = expr.strip()
        if not key:
            return None
        if key == "columns":
            return columns
        if key == "rows":
            return rows
        if key == "records":
            return records
        return [record.get(key) for record in records]

    if not isinstance(expr, dict):
        raise ValueError("custom_mapping_json values must be strings or objects")

    mode = str(expr.get("from") or "column").strip().lower()
    if mode == "columns":
        return columns
    if mode == "rows":
        return rows
    if mode == "records":
        return records
    if mode == "literal":
        return expr.get("value")
    if mode == "column":
        name = str(expr.get("name") or "").strip()
        if not name:
            raise ValueError("custom_mapping_json column mapping requires a non-empty 'name'")
        return [record.get(name) for record in records]

    raise ValueError(f"Unsupported custom mapping mode: {mode}")


def _resolve_template_string(value: str, record: dict[str, object]) -> str:
    def _replace(match: re.Match[str]) -> str:
        key = match.group(1).strip()
        resolved = record.get(key)
        if resolved is None:
            return ""
        return str(resolved)

    return re.sub(r"\{\{\s*([a-zA-Z0-9_]+)\s*\}\}", _replace, value)


def _build_custom_drilldown(mapping: dict[str, object], records: list[dict[str, object]]) -> dict[str, object] | None:
    drilldown_raw = mapping.get("_drilldown")
    if not isinstance(drilldown_raw, dict):
        return None

    target = str(drilldown_raw.get("target") or "").strip()
    if target not in {"logs", "metrics", "traces", "errors"}:
        return None

    first_record = records[0] if records else {}
    label = str(drilldown_raw.get("label") or "Open Source View").strip() or "Open Source View"

    extra_raw = drilldown_raw.get("extra")
    extra: dict[str, object] = {}
    if isinstance(extra_raw, dict):
        for k, v in cast(dict[object, object], extra_raw).items():
            key = str(k).strip()
            if not key:
                continue
            if isinstance(v, str):
                extra[key] = _resolve_template_string(v, first_record)
            else:
                extra[key] = v

    out: dict[str, object] = {"target": target, "label": label}
    for optional_key in ["bucket_seconds", "time_axis", "service_axis"]:
        if optional_key in drilldown_raw:
            out[optional_key] = cast(dict[str, object], drilldown_raw)[optional_key]
    if extra:
        out["extra"] = extra
    return out


def _normalize_custom_series_point_order(option: dict[str, object]) -> None:
    """Ensure deterministic ordering for tuple-like series points in custom ECharts."""
    series = option.get("series")
    if not isinstance(series, list):
        return

    def _to_sort_key(value: object) -> tuple[int, object]:
        if isinstance(value, datetime):
            return (0, value)
        if isinstance(value, (int, float)):
            return (1, float(value))
        if isinstance(value, str):
            text = value.strip()
            try:
                return (0, datetime.fromisoformat(text.replace("Z", "+00:00")))
            except ValueError:
                return (2, text)
        return (3, str(value))

    for entry in series:
        if not isinstance(entry, dict):
            continue
        data = entry.get("data")
        if not isinstance(data, list) or len(data) < 2:
            continue
        if not all(isinstance(point, (list, tuple)) and len(point) >= 2 for point in data):
            continue
        try:
            data.sort(key=lambda point: _to_sort_key(point[0]))
        except Exception:
            continue


def _render_custom_echarts(
    template: dict[str, object],
    columns: list[str],
    rows: list,
    spec: dict[str, object] | None,
) -> dict:
    visual = spec.get("visual") if isinstance(spec, dict) and isinstance(spec.get("visual"), dict) else {}
    visual_dict = cast(dict[str, object], visual) if isinstance(visual, dict) else {}

    mapping_raw = _parse_custom_json_config(visual_dict.get("custom_mapping_json"), "visual.custom_mapping_json")
    mapping = cast(dict[str, object], mapping_raw) if isinstance(mapping_raw, dict) else {}
    if not isinstance(mapping_raw, dict):
        raise ValueError("visual.custom_mapping_json must be a JSON object")

    option_raw_cfg = visual_dict.get("custom_option_json")
    if option_raw_cfg is None or (isinstance(option_raw_cfg, str) and not option_raw_cfg.strip()):
        option_template = copy.deepcopy(template.get("echarts_option_template", {}))
    else:
        option_template = _parse_custom_json_config(option_raw_cfg, "visual.custom_option_json")
    if not isinstance(option_template, dict):
        raise ValueError("visual.custom_option_json must be a JSON object")

    records: list[dict[str, object]] = []
    for row in rows:
        if isinstance(row, dict):
            records.append({str(k): row.get(k) for k in columns})
            continue
        if isinstance(row, (list, tuple)):
            records.append({col: row[idx] if idx < len(row) else None for idx, col in enumerate(columns)})

    rows_2d = [[record.get(col) for col in columns] for record in records]

    bindings: dict[str, object] = {
        "columns": columns,
        "records": records,
        "rows": rows_2d,
    }
    for key, expr in mapping.items():
        binding_key = str(key).strip()
        if not binding_key:
            continue
        if binding_key.startswith("_"):
            continue
        bindings[binding_key] = _resolve_custom_binding_expr(expr, columns, records, rows_2d)

    option = _deep_substitute(option_template, bindings)
    if not isinstance(option, dict):
        raise ValueError("Custom ECharts option must resolve to a JSON object")

    if "backgroundColor" not in option:
        option["backgroundColor"] = "transparent"
    if "textStyle" not in option:
        option["textStyle"] = {"color": "#adb5bd"}

    _normalize_custom_series_point_order(option)

    drilldown = _build_custom_drilldown(mapping, records)
    if drilldown:
        option["_customDrilldown"] = drilldown
    return option


def _get_dashboards(db: ChDbConnection) -> list[dict]:
    rows = db.execute(
        "SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 ORDER BY Name"
    ).fetchall()
    return [{"id": str(r["Id"]), "name": str(r["Name"]), "description": str(r["Description"])} for r in rows]


def _get_dashboard(db: ChDbConnection, dashboard_id: str) -> dict | None:
    row = db.execute(
        "SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?",
        [dashboard_id],
    ).fetchone()
    if not row:
        return None
    return {"id": str(row["Id"]), "name": str(row["Name"]), "description": str(row["Description"])}


def _get_charts(db: ChDbConnection, dashboard_id: str) -> list[dict]:
    rows = db.execute(
        "SELECT Id, Title, ChartType, Query, OptionsJson, Position "
        "FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ? "
        "ORDER BY Position, Id",
        [dashboard_id],
    ).fetchall()
    charts: list[dict] = []
    for r in rows:
        chart_type = str(r["ChartType"])
        query = str(r["Query"])
        options_json = str(r["OptionsJson"])
        chart_spec = _build_raw_chart_spec(chart_type, query, options_json)
        options_json = json.dumps({"chart_spec": chart_spec}, ensure_ascii=False)

        charts.append(
            {
                "id": str(r["Id"]),
                "title": str(r["Title"]),
                "chart_type": chart_type,
                "query": query,
                "options_json": options_json,
                "position": int(r["Position"]),
                "chart_spec": chart_spec,
            }
        )
    return charts


@app.route("/dashboards")
@require_basic_auth
async def list_dashboards():
    db = get_db()
    dashboards = _get_dashboards(db)
    return await render_template("custom_dashboards.html", dashboards=dashboards)


@app.route("/dashboards/new", methods=["GET"])
@require_basic_auth
async def new_dashboard_form():
    return await render_template("custom_dashboards.html", dashboards=[], show_new_form=True)


@app.route("/dashboards", methods=["POST"])
@require_basic_auth
async def create_dashboard():
    form = await request.form
    name = (form.get("name") or "").strip()
    description = (form.get("description") or "").strip()
    if not name:
        await flash("Dashboard name is required", "warning")
        return redirect(url_for("list_dashboards"))
    dashboard_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    db = get_db()
    _insert_rows_json_each_row(
        db,
        "sobs_dashboards",
        [{"Id": dashboard_id, "Name": name, "Description": description, "IsDeleted": 0, "Version": version}],
    )
    return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))


@app.route("/dashboards/<dashboard_id>")
@require_basic_auth
async def view_custom_dashboard(dashboard_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("list_dashboards"))
    charts = _get_charts(db, dashboard_id)
    # Convert chart_type to template metadata for frontend
    templates = [
        {
            "id": tid,
            "name": t["name"],
            "description": t["description"],
            "icon": t["icon"],
            "query_shape": t.get("query_shape", ""),
            "sample_sql": t.get("sample_sql", ""),
            "drilldown": t.get("drilldown"),
            "default_spec": _default_chart_spec(tid),
        }
        for tid, t in sorted(CHART_TEMPLATES.items())
    ]
    return await render_template(
        "custom_dashboard_view.html",
        dashboard=dashboard,
        charts=charts,
        templates=templates,
    )


@app.route("/dashboards/help/chart-editor")
@require_basic_auth
async def chart_editor_help():
    return await render_template("chart_editor_help.html")


@app.route("/metrics/help/rules")
@require_basic_auth
async def metrics_rules_help():
    return await render_template("metrics_rules_help.html")


@app.route("/metrics/help/rules/auto")
@require_basic_auth
async def auto_metrics_rules_help():
    return await render_template("auto_metrics_rules_help.html")


@app.route("/dashboards/<dashboard_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_dashboard(dashboard_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("list_dashboards"))
    version = int(time.time() * 1000)
    # Soft-delete dashboard
    _insert_rows_json_each_row(
        db,
        "sobs_dashboards",
        [
            {
                "Id": dashboard_id,
                "Name": dashboard["name"],
                "Description": dashboard["description"],
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    # Soft-delete all charts in this dashboard
    charts = _get_charts(db, dashboard_id)
    if charts:
        tombstones = [
            {
                "Id": c["id"],
                "DashboardId": dashboard_id,
                "Title": c["title"],
                "ChartType": c["chart_type"],
                "Query": c["query"],
                "OptionsJson": c["options_json"],
                "Position": c["position"],
                "IsDeleted": 1,
                "Version": version,
            }
            for c in charts
        ]
        _insert_rows_json_each_row(db, "sobs_chart_configs", tombstones)
    await flash(f"Dashboard '{dashboard['name']}' deleted", "success")
    return redirect(url_for("list_dashboards"))


@app.route("/dashboards/<dashboard_id>/charts", methods=["POST"])
@require_basic_auth
async def add_chart(dashboard_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("list_dashboards"))
    form = await request.form
    try:
        title, template_id, query, options_json = _parse_chart_form_submission(form)
    except ValueError as ve:
        await flash(str(ve), "warning")
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))
    existing = _get_charts(db, dashboard_id)
    position = max((c["position"] for c in existing), default=-1) + 1
    chart_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": position,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))


def _parse_chart_form_submission(form) -> tuple[str, str, str, str]:
    title = (form.get("title") or "").strip()
    chart_spec_json = (form.get("chart_spec_json") or "").strip()

    if not title:
        raise ValueError("Chart title is required")
    if not chart_spec_json:
        raise ValueError("Chart spec is required")

    try:
        spec_raw = json.loads(chart_spec_json)
        template_id, query, normalized_spec = _compile_chart_spec(spec_raw)
    except Exception as exc:
        raise ValueError(f"Chart spec error: {exc}") from exc

    options_json = json.dumps({"chart_spec": normalized_spec}, ensure_ascii=False)
    return title, template_id, query, options_json


@app.route("/dashboards/<dashboard_id>/charts/<chart_id>/edit", methods=["POST"])
@require_basic_auth
async def edit_chart(dashboard_id: str, chart_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("list_dashboards"))

    charts = _get_charts(db, dashboard_id)
    chart = next((c for c in charts if c["id"] == chart_id), None)
    if not chart:
        await flash("Chart not found", "warning")
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))

    form = await request.form
    try:
        title, template_id, query, options_json = _parse_chart_form_submission(form)
    except ValueError as ve:
        await flash(str(ve), "warning")
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))

    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": chart["position"],
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))


@app.route("/dashboards/<dashboard_id>/charts/<chart_id>/clone", methods=["POST"])
@require_basic_auth
async def clone_chart(dashboard_id: str, chart_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("list_dashboards"))

    charts = _get_charts(db, dashboard_id)
    source_chart = next((c for c in charts if c["id"] == chart_id), None)
    if not source_chart:
        await flash("Chart not found", "warning")
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))

    form = await request.form
    try:
        title, template_id, query, options_json = _parse_chart_form_submission(form)
    except ValueError as ve:
        await flash(str(ve), "warning")
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))

    position = max((c["position"] for c in charts), default=-1) + 1
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": str(uuid.uuid4()),
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": position,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))


@app.route("/dashboards/<dashboard_id>/charts/<chart_id>/delete", methods=["POST"])
@require_basic_auth
async def remove_chart(dashboard_id: str, chart_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("list_dashboards"))
    charts = _get_charts(db, dashboard_id)
    chart = next((c for c in charts if c["id"] == chart_id), None)
    if not chart:
        await flash("Chart not found", "warning")
        return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": chart["title"],
                "ChartType": chart["chart_type"],
                "Query": chart["query"],
                "OptionsJson": chart["options_json"],
                "Position": chart["position"],
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("view_custom_dashboard", dashboard_id=dashboard_id))


@app.route("/api/dashboards/query", methods=["POST"])
@require_basic_auth
async def execute_chart_query():
    """Execute a ClickHouse SELECT query and return raw results for eChart rendering."""
    body = await request.get_json(silent=True) or {}
    query = (body.get("query") or "").strip()
    err = _validate_chart_query(query)
    if err:
        return jsonify({"error": err}), 400
    # Inject a row limit to prevent runaway queries
    if not re.search(r"\bLIMIT\b", query, re.IGNORECASE):
        query = query.rstrip(";") + " LIMIT 1000"
    db = get_db()
    try:
        result = db.execute(query)
        rows = result.fetchall()
        columns = list(rows[0].keys()) if rows else []
        data = [[row[col] for col in columns] for row in rows]
        return jsonify({"columns": columns, "rows": data})
    except Exception as exc:
        app.logger.exception("Chart query execution failed: %s", query)
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400


@app.route("/api/dashboards/spec/templates", methods=["GET"])
@require_basic_auth
async def list_chart_spec_templates():
    templates = [
        {
            "id": tid,
            "name": t["name"],
            "description": t["description"],
            "query_shape": t.get("query_shape", ""),
            "sample_sql": t.get("sample_sql", ""),
            "default_spec": _default_chart_spec(tid),
            "min_columns": t.get("min_columns", 0),
            "max_columns": t.get("max_columns"),
            "column_roles": t.get("column_roles", {}),
        }
        for tid, t in sorted(CHART_TEMPLATES.items())
    ]
    return jsonify({"templates": templates})


@app.route("/api/dashboards/spec/options", methods=["GET"])
@require_basic_auth
async def chart_spec_options_api():
    source_view = str(request.args.get("source_view") or "v_derived_signals_anomaly").strip()
    signal_source = str(request.args.get("signal_source") or "").strip()
    limit = _coerce_positive_int(request.args.get("limit"), 100, 1, 500)

    supported_sources = {
        "v_derived_signals_anomaly",
        "v_otel_metrics_anomaly",
        "otel_metrics_gauge",
        "otel_metrics_sum",
        "otel_metrics_histogram",
        "otel_logs",
        "otel_traces",
        "sobs_error_resolutions",
    }
    if source_view not in supported_sources:
        return jsonify({"error": "Unsupported source for options"}), 400

    db = get_db()

    def _distinct_values(query: str) -> list[str]:
        rows = db.execute(query).fetchall()
        values: list[str] = []
        for row in rows:
            val = str(row["v"] or "").strip()
            if val:
                values.append(val)
        return values

    services: list[str] = []
    signals: list[str] = []
    metrics: list[str] = []

    if source_view == "v_derived_signals_anomaly":
        services = _distinct_values(
            "SELECT DISTINCT ServiceName AS v " "FROM v_derived_signals_anomaly " "ORDER BY v " f"LIMIT {limit}"
        )
        signal_where = ""
        if signal_source:
            signal_where = f"WHERE SignalSource = {_sql_literal(signal_source)} "
        signals = _distinct_values(
            "SELECT DISTINCT SignalName AS v "
            "FROM v_derived_signals_anomaly "
            f"{signal_where}"
            "ORDER BY v "
            f"LIMIT {limit}"
        )
    elif source_view in {"otel_logs", "otel_traces"}:
        services = _distinct_values(
            "SELECT DISTINCT ServiceName AS v " f"FROM {source_view} " "ORDER BY v " f"LIMIT {limit}"
        )
        signals = ["log_volume"] if source_view == "otel_logs" else ["trace_volume"]
    elif source_view == "sobs_error_resolutions":
        signals = ["resolved_error_volume"]
    elif source_view in {"v_otel_metrics_anomaly", "otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"}:
        services = _distinct_values(
            "SELECT DISTINCT ServiceName AS v " f"FROM {source_view} " "ORDER BY v " f"LIMIT {limit}"
        )
        metrics = _distinct_values(
            "SELECT DISTINCT MetricName AS v " f"FROM {source_view} " "ORDER BY v " f"LIMIT {limit}"
        )

    return jsonify(
        {
            "source_view": source_view,
            "services": services,
            "signals": signals,
            "metrics": metrics,
        }
    )


@app.route("/api/dashboards/spec/compile", methods=["POST"])
@require_basic_auth
async def compile_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"error": str(ve)}), 400
    except Exception as exc:
        app.logger.exception("Chart spec compile failed")
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400
    return jsonify({"template_id": template_id, "query": query, "spec": normalized_spec})


@app.route("/api/dashboards/spec/dry-run", methods=["POST"])
@require_basic_auth
async def dry_run_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"error": str(ve)}), 400

    run_query = query
    if not re.search(r"\bLIMIT\b", run_query, re.IGNORECASE):
        run_query = run_query.rstrip(";") + " LIMIT 20"
    db = get_db()
    try:
        result = db.execute(run_query)
        rows = result.fetchall()
        columns = list(rows[0].keys()) if rows else []
        data = [[row[col] for col in columns] for row in rows]
        column_types = _infer_column_types(columns, data)
    except Exception as exc:
        app.logger.exception("Chart spec dry-run failed")
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400
    return jsonify(
        {
            "template_id": template_id,
            "query": query,
            "spec": normalized_spec,
            "columns": columns,
            "column_types": column_types,
            "rows": data,
        }
    )


@app.route("/api/dashboards/spec/validate", methods=["POST"])
@require_basic_auth
async def validate_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"valid": False, "error": str(ve)}), 400

    db = get_db()
    try:
        run_query = query
        if not re.search(r"\bLIMIT\b", run_query, re.IGNORECASE):
            run_query = run_query.rstrip(";") + " LIMIT 200"
        result = db.execute(run_query)
        raw_rows = result.fetchall()
        columns = list(raw_rows[0].keys()) if raw_rows else []
        data = [dict(row) for row in raw_rows]
        _render_chart_from_template(template_id, columns, data, normalized_spec)
    except Exception as exc:
        return jsonify({"valid": False, "error": _public_dashboard_query_error(exc)}), 400

    return jsonify(
        {
            "valid": True,
            "template_id": template_id,
            "query": query,
            "spec": normalized_spec,
            "columns": columns,
            "row_count": len(data),
        }
    )


@app.route("/api/dashboards/spec/render", methods=["POST"])
@require_basic_auth
async def render_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"error": str(ve)}), 400

    db = get_db()
    try:
        run_query = query
        if not re.search(r"\bLIMIT\b", run_query, re.IGNORECASE):
            run_query = run_query.rstrip(";") + " LIMIT 1000"
        result = db.execute(run_query)
        raw_rows = result.fetchall()
        columns = list(raw_rows[0].keys()) if raw_rows else []
        data = [dict(row) for row in raw_rows]
        option = _render_chart_from_template(template_id, columns, data, normalized_spec)
        option = _apply_chart_spec_visual_overrides(template_id, option, normalized_spec)
    except Exception as exc:
        app.logger.exception("Chart spec render failed")
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400
    return jsonify({"template_id": template_id, "query": query, "spec": normalized_spec, "option": option})


@app.route("/api/dashboards/render", methods=["POST"])
@require_basic_auth
async def render_chart():
    """Execute a query and render with a template to produce eCharts option."""
    body = await request.get_json(silent=True) or {}
    query = (body.get("query") or "").strip()
    template_id = (body.get("template_id") or "time_series_percentiles").strip()

    err = _validate_chart_query(query)
    if err:
        return jsonify({"error": err}), 400

    if template_id not in CHART_TEMPLATES:
        return jsonify({"error": f"Unknown template: {template_id}"}), 400

    # Inject a row limit to prevent runaway queries
    if not re.search(r"\bLIMIT\b", query, re.IGNORECASE):
        query = query.rstrip(";") + " LIMIT 1000"

    db = get_db()
    try:
        result = db.execute(query)
        raw_rows = result.fetchall()
        columns = list(raw_rows[0].keys()) if raw_rows else []
        data = [dict(row) for row in raw_rows]

        # Render using template
        option = _render_chart_from_template(template_id, columns, data)
        return jsonify({"option": option})
    except ValueError as ve:
        # Template column mismatch
        return jsonify({"error": str(ve)}), 400
    except Exception as exc:
        app.logger.exception("Chart render failed: template=%s query=%s", template_id, query)
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400


# ---------------------------------------------------------------------------
# Metrics Anomaly API  GET /api/metrics/anomaly
# ---------------------------------------------------------------------------
@app.route("/api/metrics/anomaly", methods=["GET"])
@require_basic_auth
async def metrics_anomaly():
    """Return per-minute anomaly detection data for a specific metric series.

    Query parameters:
    - ``service``: ServiceName (required)
    - ``metric``: MetricName (required)
    - ``hours``: look-back window in hours, 1–168 (default: 24)
    - ``attr_fp``: optional AttrFingerprint to select a single series

    Response JSON::

        {
          "service": "...",
          "metric": "...",
          "columns": ["time", "value", "sample_count", "baseline_mean",
                      "baseline_stddev", "baseline_lower", "baseline_upper",
                      "anomaly_score", "anomaly_state", "metric_kind", "attr_fp"],
          "rows": [[...], ...]
        }
    """
    service = (request.args.get("service") or "").strip()
    metric = (request.args.get("metric") or "").strip()
    if not service or not metric:
        return jsonify({"error": "service and metric query parameters are required"}), 400

    try:
        hours = max(1, min(168, int(request.args.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24

    attr_fp = (request.args.get("attr_fp") or "").strip()

    db = get_db()
    try:
        fp_clause = " AND AttrFingerprint = ?" if attr_fp else ""
        params: list = [service, metric, hours]
        if attr_fp:
            params.append(attr_fp)
        result = db.execute(
            "SELECT"
            "  time,"
            "  value,"
            "  SampleCount AS sample_count,"
            "  baseline_mean,"
            "  baseline_stddev,"
            "  baseline_lower,"
            "  baseline_upper,"
            "  anomaly_score,"
            "  anomaly_state,"
            "  MetricKind AS metric_kind,"
            "  AttrFingerprint AS attr_fp"
            " FROM v_otel_metrics_anomaly"
            " WHERE ServiceName = ?"
            "   AND MetricName = ?"
            f"   AND time >= now() - INTERVAL ? HOUR"
            f"{fp_clause}"
            " ORDER BY time"
            " LIMIT 1440",
            params,
        )
        rows = result.fetchall()
        columns = (
            list(rows[0].keys())
            if rows
            else [
                "time",
                "value",
                "sample_count",
                "baseline_mean",
                "baseline_stddev",
                "baseline_lower",
                "baseline_upper",
                "anomaly_score",
                "anomaly_state",
                "metric_kind",
                "attr_fp",
            ]
        )

        def _safe(v):  # type: ignore
            if isinstance(v, float) and (v != v):  # IEEE 754: NaN is the only value not equal to itself
                return None
            return v

        data = [[_safe(row[col]) for col in columns] for row in rows]
        return jsonify({"service": service, "metric": metric, "columns": columns, "rows": data})
    except Exception as exc:
        app.logger.exception("metrics_anomaly query failed: service=%s metric=%s", service, metric)
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400


# ---------------------------------------------------------------------------
# Static RUM script
# ---------------------------------------------------------------------------
@app.route("/static/rum.js")
async def rum_js():
    return await send_from_directory(
        os.path.join(os.path.dirname(__file__), "static"), "rum.js", mimetype="application/javascript"
    )


# ---------------------------------------------------------------------------
# Settings / Config  GET /settings
# ---------------------------------------------------------------------------
@app.route("/settings")
@require_basic_auth
async def view_settings():
    """Settings/config hub page linking to tag rules, metrics rules, and other config."""
    db = get_db()
    tag_rules = _load_tag_rules(db)
    anomaly_rules = _load_anomaly_rules(db)
    return await render_template(
        "settings.html",
        tag_rule_count=len(tag_rules),
        anomaly_rule_count=len(anomaly_rules),
    )


# ---------------------------------------------------------------------------
# Tag Rules  GET/POST /settings/tags
# ---------------------------------------------------------------------------
@app.route("/settings/tags")
@require_basic_auth
async def view_tag_rules():
    db = get_db()
    open_panel = (request.args.get("open_panel") or "").strip().lower()
    if open_panel not in {"auto-tags"}:
        open_panel = ""
    rules = _load_tag_rules(db)
    services = _list_tag_candidate_services(db)
    return await render_template(
        "settings_tags.html",
        rules=rules,
        record_types=_TAG_RULE_RECORD_TYPES,
        match_fields=_TAG_RULE_FIELDS,
        match_operators=_TAG_RULE_OPERATORS,
        services=services,
        auto_preview=[],
        auto_summary=None,
        auto_open_panel=open_panel,
    )


@app.route("/settings/tags/auto", methods=["POST"])
@require_basic_auth
async def auto_tag_rules():
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    try:
        hours = max(1, min(168, int(form.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24
    try:
        min_count = max(1, min(5000, int(form.get("min_count") or 30)))
    except (TypeError, ValueError):
        min_count = 30

    service_filter = (form.get("service_filter") or "").strip()
    selected_record_types = [rt.strip().lower() for rt in form.getlist("auto_record_types") if rt and rt.strip()]
    if not selected_record_types:
        selected_record_types = ["log", "trace", "error", "ai", "rum"]

    db = get_db()
    rules = _load_tag_rules(db)
    services = _list_tag_candidate_services(db)

    candidates, stats = _build_auto_tag_rule_candidates(
        db,
        hours=hours,
        min_count=min_count,
        service_filter=service_filter,
        record_types=selected_record_types,
    )

    summary = {
        "action": action,
        "hours": hours,
        "min_count": min_count,
        "service_filter": service_filter,
        "record_types": selected_record_types,
        "examined": stats["examined"],
        "existing": stats["existing"],
        "invalid": stats["invalid"],
        "candidates": len(candidates),
        "create_cap": _AUTO_TAG_RULE_CREATE_MAX,
        "capped": len(candidates) > _AUTO_TAG_RULE_CREATE_MAX,
        "created": 0,
    }

    if action == "create":
        limited_candidates = candidates[:_AUTO_TAG_RULE_CREATE_MAX]
        version = int(time.time() * 1000)
        rows_to_insert: list[dict[str, object]] = []
        for idx, candidate in enumerate(limited_candidates):
            rows_to_insert.append(
                {
                    "Id": str(uuid.uuid4()),
                    "Name": str(candidate["name"]),
                    "RecordTypes": ",".join([str(rt) for rt in candidate["record_types"]]),
                    "MatchField": str(candidate["match_field"]),
                    "MatchOperator": str(candidate["match_operator"]),
                    "MatchValue": str(candidate["match_value"]),
                    "MatchAttrKey": str(candidate["match_attr_key"]),
                    "TagKey": str(candidate["tag_key"]),
                    "TagValue": str(candidate["tag_value"]),
                    "IsDeleted": 0,
                    "Version": version + idx,
                }
            )
        if rows_to_insert:
            _insert_rows_json_each_row(db, "sobs_tag_rules", rows_to_insert)
        summary["created"] = len(rows_to_insert)
        skipped_by_cap = max(0, len(candidates) - len(limited_candidates))
        cap_suffix = f", skipped {skipped_by_cap} by max cap ({_AUTO_TAG_RULE_CREATE_MAX})." if skipped_by_cap else "."
        await flash(
            (
                f"Auto tag rule generation complete: created {summary['created']} rule(s), "
                f"skipped {summary['existing']} existing, {summary['invalid']} invalid"
                f"{cap_suffix}"
            ),
            "success",
        )
        return redirect(url_for("view_tag_rules", open_panel="auto-tags"))

    await flash(
        (
            f"Auto-tag preview: {summary['candidates']} candidate(s), "
            f"{summary['existing']} existing skipped, {summary['invalid']} invalid."
        ),
        "info",
    )
    return await render_template(
        "settings_tags.html",
        rules=rules,
        record_types=_TAG_RULE_RECORD_TYPES,
        match_fields=_TAG_RULE_FIELDS,
        match_operators=_TAG_RULE_OPERATORS,
        services=services,
        auto_preview=candidates,
        auto_summary=summary,
        auto_open_panel="auto-tags",
    )


@app.route("/settings/tags", methods=["POST"])
@require_basic_auth
async def create_tag_rule():
    form = await request.form
    name = (form.get("name") or "").strip()
    record_types_list = form.getlist("record_types")
    match_field = (form.get("match_field") or "").strip().lower()
    match_operator = (form.get("match_operator") or "eq").strip().lower()
    match_value = (form.get("match_value") or "").strip()
    match_attr_key = (form.get("match_attr_key") or "").strip()
    tag_key = (form.get("tag_key") or "").strip()
    tag_value = (form.get("tag_value") or "").strip()

    if not name or not match_field or not tag_key or not tag_value:
        await flash("Name, match field, tag key, and tag value are required", "warning")
        return redirect(url_for("view_tag_rules"))
    if match_field not in _TAG_RULE_FIELDS:
        await flash(f"Invalid match field: {match_field}", "warning")
        return redirect(url_for("view_tag_rules"))
    if match_operator not in _TAG_RULE_OPERATORS:
        await flash(f"Invalid match operator: {match_operator}", "warning")
        return redirect(url_for("view_tag_rules"))
    if match_field == "attribute" and not match_attr_key:
        await flash("Attribute key is required when match field is 'attribute'", "warning")
        return redirect(url_for("view_tag_rules"))
    if match_operator == "regex":
        try:
            re.compile(match_value)
        except re.error as exc:
            await flash(f"Invalid regex pattern: {exc}", "warning")
            return redirect(url_for("view_tag_rules"))

    # Normalise record types
    valid_types = set(_TAG_RULE_RECORD_TYPES)
    chosen = [t.strip() for t in record_types_list if t.strip() in valid_types]
    record_types_str = ",".join(chosen) if chosen else "all"

    rule_id = str(uuid.uuid4())
    _insert_rows_json_each_row(
        get_db(),
        "sobs_tag_rules",
        [
            {
                "Id": rule_id,
                "Name": name,
                "RecordTypes": record_types_str,
                "MatchField": match_field,
                "MatchOperator": match_operator,
                "MatchValue": match_value,
                "MatchAttrKey": match_attr_key,
                "TagKey": tag_key,
                "TagValue": tag_value,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    await flash(f"Tag rule '{name}' created", "success")
    return redirect(url_for("view_tag_rules"))


@app.route("/settings/tags/<rule_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_tag_rule(rule_id: str):
    db = get_db()
    row = db.execute(
        "SELECT Id, Name FROM sobs_tag_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
        [rule_id],
    ).fetchone()
    if not row:
        await flash("Tag rule not found", "warning")
        return redirect(url_for("view_tag_rules"))
    _insert_rows_json_each_row(
        db,
        "sobs_tag_rules",
        [
            {
                "Id": rule_id,
                "Name": str(row["Name"]),
                "RecordTypes": "",
                "MatchField": "",
                "MatchOperator": "eq",
                "MatchValue": "",
                "MatchAttrKey": "",
                "TagKey": "",
                "TagValue": "",
                "IsDeleted": 1,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    await flash(f"Tag rule '{row['Name']}' deleted", "success")
    return redirect(url_for("view_tag_rules"))


# ---------------------------------------------------------------------------
# Record Tags API  GET/POST /api/tags/<record_type>/<record_id>
#                  DELETE /api/tags/<record_type>/<record_id>/<tag_key>
# ---------------------------------------------------------------------------
@app.route("/api/tags/<record_type>/<record_id>", methods=["GET"])
@require_api_key
async def api_get_tags(record_type: str, record_id: str):
    db = get_db()
    tags = _get_record_tags(db, record_type, record_id)
    return jsonify({"tags": tags})


@app.route("/api/tags/<record_type>/<record_id>", methods=["POST"])
@require_api_key
async def api_add_tag(record_type: str, record_id: str):
    payload = await request.get_json(force=True, silent=True) or {}
    tag_key = str(payload.get("key", "")).strip()
    tag_value = str(payload.get("value", "")).strip()
    if not tag_key:
        return jsonify({"error": "key is required"}), 400
    if len(tag_key) > 128 or len(tag_value) > 512:
        return jsonify({"error": "tag key or value too long"}), 400
    _insert_rows_json_each_row(
        get_db(),
        "sobs_record_tags",
        [
            {
                "RecordType": record_type,
                "RecordId": record_id,
                "TagKey": tag_key,
                "TagValue": tag_value,
                "IsAuto": 0,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    return jsonify({"ok": True}), 201


@app.route("/api/tags/<record_type>/<record_id>/<tag_key>", methods=["DELETE"])
@require_api_key
async def api_delete_tag(record_type: str, record_id: str, tag_key: str):
    db = get_db()
    rows = db.execute(
        "SELECT TagKey, TagValue, IsAuto FROM sobs_record_tags FINAL "
        "WHERE RecordType = ? AND RecordId = ? AND TagKey = ? AND IsDeleted = 0",
        [record_type, record_id, tag_key],
    ).fetchall()
    if not rows:
        return jsonify({"error": "tag not found"}), 404
    tombstones = []
    version = int(time.time() * 1000)
    seen_values: set[tuple[str, int]] = set()
    for row in rows:
        tag_value = str(row["TagValue"])
        is_auto = int(row["IsAuto"])
        dedupe_key = (tag_value, is_auto)
        if dedupe_key in seen_values:
            continue
        seen_values.add(dedupe_key)
        tombstones.append(
            {
                "RecordType": record_type,
                "RecordId": record_id,
                "TagKey": tag_key,
                "TagValue": tag_value,
                "IsAuto": is_auto,
                "IsDeleted": 1,
                "Version": version,
            }
        )
        version += 1
    _insert_rows_json_each_row(
        db,
        "sobs_record_tags",
        tombstones,
    )
    return jsonify({"ok": True}), 200


# ---------------------------------------------------------------------------
# Log Field Hints API  GET /api/logs/field-hints
# Returns available otel_logs field names (with user-friendly aliases),
# sample values for enum-like fields, and active tag keys for the log type.
# Used by the SQL filter autocomplete on the Logs page.
# ---------------------------------------------------------------------------
@app.route("/api/logs/field-hints", methods=["GET"])
@require_basic_auth
async def api_logs_field_hints():
    db = get_db()

    fields = [
        {"name": "level", "column": "SeverityText", "type": "string", "values": []},
        {"name": "service", "column": "ServiceName", "type": "string", "values": []},
        {"name": "body", "column": "Body", "type": "string", "values": []},
        {"name": "trace_id", "column": "TraceId", "type": "string", "values": []},
        {"name": "span_id", "column": "SpanId", "type": "string", "values": []},
        {"name": "ts", "column": "Timestamp", "type": "datetime", "values": []},
        {"name": "EventName", "column": "EventName", "type": "string", "values": []},
        {"name": "ScopeName", "column": "ScopeName", "type": "string", "values": []},
    ]

    attr_keys = _get_cached_log_attr_keys(db, record_type="log")

    # Active tag keys for logs (used in has_tag() suggestions)
    try:
        tag_key_rows = db.execute(
            "SELECT DISTINCT TagKey FROM sobs_record_tags FINAL "
            "WHERE RecordType='log' AND IsDeleted=0 ORDER BY TagKey LIMIT 100"
        ).fetchall()
        tag_keys = [str(r[0]) for r in tag_key_rows]
        # For each tag key, also fetch distinct values (cap at 20)
        tag_values: dict[str, list[str]] = {}
        for tk in tag_keys:
            val_rows = db.execute(
                "SELECT DISTINCT TagValue FROM sobs_record_tags FINAL "
                "WHERE RecordType='log' AND TagKey=? AND IsDeleted=0 ORDER BY TagValue LIMIT 20",
                [tk],
            ).fetchall()
            tag_values[tk] = [str(r[0]) for r in val_rows]
    except Exception:
        tag_keys = []
        tag_values = {}

    operators = ["=", "!=", "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE", "IN", "NOT IN", ">", "<", ">=", "<="]
    keywords = ["AND", "OR", "NOT", "IS NULL", "IS NOT NULL", "TRUE", "FALSE", "NULL"]
    functions = [
        {"name": "has_tag", "signature": "has_tag('key','value')", "kind": "tag"},
        {"name": "match", "signature": "match(body, 'regex')", "kind": "string"},
        {"name": "positionCaseInsensitive", "signature": "positionCaseInsensitive(body, 'needle')", "kind": "string"},
        {"name": "startsWith", "signature": "startsWith(service, 'api')", "kind": "string"},
        {"name": "endsWith", "signature": "endsWith(service, 'worker')", "kind": "string"},
        {"name": "lower", "signature": "lower(service)", "kind": "string"},
        {"name": "upper", "signature": "upper(level)", "kind": "string"},
        {"name": "toString", "signature": "toString(ts)", "kind": "cast"},
        {"name": "toDateTime", "signature": "toDateTime('2026-03-30 12:00:00')", "kind": "datetime"},
    ]
    snippets = [
        {"label": "level='ERROR'", "insert": "level='ERROR'", "kind": "predicate"},
        {"label": "service IN ('api','worker')", "insert": "service IN ('api','worker')", "kind": "predicate"},
        {"label": "has_tag('env','prod')", "insert": "has_tag('env','prod')", "kind": "predicate"},
        {"label": "match(body, 'timeout')", "insert": "match(body, 'timeout')", "kind": "predicate"},
        {
            "label": "ts >= toDateTime('2026-03-30 00:00:00')",
            "insert": "ts >= toDateTime('2026-03-30 00:00:00')",
            "kind": "predicate",
        },
    ]

    return jsonify(
        {
            "fields": fields,
            "attr_keys": attr_keys,
            "tag_keys": tag_keys,
            "tag_values": tag_values,
            "operators": operators,
            "keywords": keywords,
            "functions": functions,
            "snippets": snippets,
        }
    )


@app.route("/api/logs/validate-filter", methods=["POST"])
@require_basic_auth
async def api_logs_validate_filter():
    """Validate a SQL WHERE fragment used by /logs?sql=... and return actionable feedback."""
    payload = await request.get_json(silent=True)
    sql_where = str((payload or {}).get("sql", "") or "").strip()
    if not sql_where:
        return jsonify({"ok": True, "normalized": "", "issues": []})

    issues: list[dict[str, str]] = []

    # Lightweight structural checks for instant, helpful feedback.
    quote_open = False
    paren_depth = 0
    i = 0
    while i < len(sql_where):
        ch = sql_where[i]
        if ch == "'":
            if i + 1 < len(sql_where) and sql_where[i + 1] == "'":
                i += 2
                continue
            quote_open = not quote_open
        elif not quote_open:
            if ch == "(":
                paren_depth += 1
            elif ch == ")":
                paren_depth -= 1
                if paren_depth < 0:
                    issues.append({"level": "error", "message": "Unexpected ')' in filter."})
                    break
        i += 1

    if quote_open:
        issues.append({"level": "error", "message": "Unclosed single quote in filter."})
    if paren_depth > 0:
        issues.append({"level": "error", "message": "Unclosed '(' in filter."})
    if re.search(r"\b(AND|OR|NOT|IN|LIKE|ILIKE)\s*$", sql_where, re.IGNORECASE):
        issues.append({"level": "warning", "message": "Filter ends with an operator or keyword."})

    try:
        safe_sql = sql_where.replace(";", "")
        safe_sql = re.sub(r"\blevel\b", "SeverityText", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bservice\b", "ServiceName", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\btrace_id\b", "TraceId", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bspan_id\b", "SpanId", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bts\b", "Timestamp", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bbody\b", "Body", safe_sql, flags=re.IGNORECASE)

        def _translate_has_tag(m: re.Match) -> str:
            tag_key = m.group(1).replace("''", "'").replace("'", "''")
            tag_val = m.group(2).replace("''", "'").replace("'", "''")
            return (
                "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN ("
                "SELECT RecordId FROM sobs_record_tags FINAL "
                f"WHERE TagKey='{tag_key}' AND TagValue='{tag_val}' "
                "AND IsDeleted=0 AND RecordType='log')"
            )

        safe_sql = re.sub(
            r"has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)",
            _translate_has_tag,
            safe_sql,
            flags=re.IGNORECASE,
        )

        db = get_db()
        # Existence probe is much cheaper than aggregate count() for live typing validation.
        db.execute(f"SELECT 1 FROM otel_logs WHERE {safe_sql} LIMIT 1").fetchone()
    except Exception as exc:
        issues.append({"level": "error", "message": _public_dashboard_query_error(exc)})
        return jsonify({"ok": False, "normalized": "", "issues": issues}), 200

    return jsonify({"ok": True, "normalized": safe_sql, "issues": issues})


# ---------------------------------------------------------------------------
# SSE live tail  GET /tail
# ---------------------------------------------------------------------------
@app.route("/tail")
@require_basic_auth
async def tail_stream():
    """Live-tail logs and traces as a Server-Sent Events stream.

    Query parameters:
    - ``source``: ``logs``, ``traces``, or ``all`` (default: ``all``)
    - ``service``: optional service name filter (exact match)

    SSE event format::

        data: {"source": "logs", "ts": "...", "level": "INFO", "service": "...", "body": "..."}

    Example usage::

        curl -N http://localhost:4317/tail
        curl -N "http://localhost:4317/tail?source=logs&service=myapp"
    """
    source = request.args.get("source", "all").strip().lower()
    service_filter = request.args.get("service", "").strip()

    async def _generate():
        q: asyncio.Queue = asyncio.Queue(maxsize=_SSE_QUEUE_MAXSIZE)
        _sse_subscribers.add(q)
        try:
            yield "retry: 5000\n\n"
            while True:
                try:
                    event = await asyncio.wait_for(q.get(), timeout=15.0)
                except asyncio.TimeoutError:
                    yield ": keepalive\n\n"
                    continue
                if source != "all" and event.get("source") != source:
                    continue
                if service_filter and event.get("service") != service_filter:
                    continue
                yield f"data: {json.dumps(event, ensure_ascii=False)}\n\n"
        finally:
            _sse_subscribers.discard(q)

    return Response(
        _generate(),
        mimetype="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


# ---------------------------------------------------------------------------
# Health check
# ---------------------------------------------------------------------------
@app.route("/health")
async def health():
    return jsonify({"status": "ok", "version": "1.0.0"})


@app.route("/health/db")
async def health_db():
    started = time.perf_counter()
    try:
        ensure_db_schema()
        get_db().execute("SELECT 1").fetchone()
    except Exception as exc:
        app.logger.exception("DB readiness probe failed")
        return (
            jsonify(
                {
                    "status": "degraded",
                    "db": "error",
                    "error": str(exc),
                    "write_queue_depth": _write_queue_depth(),
                    "version": "1.0.0",
                }
            ),
            503,
        )

    latency_ms = round((time.perf_counter() - started) * 1000, 2)
    return jsonify(
        {
            "status": "ok",
            "db": "ok",
            "latency_ms": latency_ms,
            "write_queue_depth": _write_queue_depth(),
            "version": "1.0.0",
        }
    )


# ---------------------------------------------------------------------------
# Entrypoint
# ---------------------------------------------------------------------------
if __name__ == "__main__":
    port = int(os.environ.get("PORT", 4317))
    requested_workers = max(
        1,
        int(
            os.environ.get(
                "HYPERCORN_WORKERS",
                os.environ.get("GUNICORN_WORKERS", "1"),
            )
        ),
    )
    if requested_workers != 1:
        log.warning("Embedded chDB requires single-process mode; forcing worker count to 1")
    bind = os.environ.get("HYPERCORN_BIND", os.environ.get("GUNICORN_BIND", f"0.0.0.0:{port}"))

    config = HypercornConfig()
    config.bind = [bind]
    config.workers = 1
    config.use_reloader = False

    asyncio.run(hypercorn_serve(app, config))
