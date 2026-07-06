package main

// schemaSql is the verbatim SCHEMA constant from app.py (lines 714-1873).
const schemaSql = `CREATE TABLE IF NOT EXISTS otel_logs (
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
    SeasonalBucketsJson String DEFAULT '' CODEC(ZSTD(1)),
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

-- Materialized table for pre-aggregated 1-minute metrics using AggregatingMergeTree.
-- This reduces memory pressure on trace context queries by pre-storing aggregated state.
CREATE TABLE IF NOT EXISTS otel_metrics_1m_agg (
    ServiceName String,
    MetricName String,
    AttrFingerprint String,
    MetricKind String,
    MinuteBucket DateTime,
    Value AggregateFunction(avg, Float64),
    SampleCount AggregateFunction(sum, UInt64)
) ENGINE = AggregatingMergeTree()
ORDER BY (ServiceName, MetricName, AttrFingerprint, MetricKind, MinuteBucket)
PARTITION BY toYYYYMM(MinuteBucket);

-- Materialized view to insert gauge metrics into the aggregated table.
CREATE MATERIALIZED VIEW IF NOT EXISTS mv_otel_metrics_1m_gauge
TO otel_metrics_1m_agg
AS SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'gauge' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avgState(Value) AS Value,
    sumState(toUInt64(1)) AS SampleCount
FROM otel_metrics_gauge
GROUP BY ServiceName, MetricName, AttrFingerprint, MinuteBucket;

-- Materialized view to insert sum metrics into the aggregated table.
CREATE MATERIALIZED VIEW IF NOT EXISTS mv_otel_metrics_1m_sum
TO otel_metrics_1m_agg
AS SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'sum' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avgState(Value) AS Value,
    sumState(toUInt64(1)) AS SampleCount
FROM otel_metrics_sum
GROUP BY ServiceName, MetricName, AttrFingerprint, MinuteBucket;

-- Materialized view to insert histogram metrics into the aggregated table.
CREATE MATERIALIZED VIEW IF NOT EXISTS mv_otel_metrics_1m_histogram
TO otel_metrics_1m_agg
AS SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'histogram' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avgState(if(Count > 0, Sum / Count, 0)) AS Value,
    sumState(Count) AS SampleCount
FROM otel_metrics_histogram
GROUP BY ServiceName, MetricName, AttrFingerprint, MinuteBucket;

-- Canonical 1-minute metrics view backed by aggregate-state rollups.
CREATE OR REPLACE VIEW v_otel_metrics_1m AS
SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    MetricKind,
    MinuteBucket,
    avgMerge(Value) AS Value,
    sumMerge(SampleCount) AS SampleCount
FROM otel_metrics_1m_agg
GROUP BY ServiceName, MetricName, AttrFingerprint, MetricKind, MinuteBucket;

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

CREATE OR REPLACE VIEW v_derived_signals_1m AS
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
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
        ServiceName,
        'rum_vitals' AS SignalSource,
        'LCP' AS SignalName,
        substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|LCP')))), 1, 16) AS AttrFingerprint,
        toStartOfMinute(Timestamp) AS MinuteBucket,
        toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
        count() AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
    AND JSONExtractString(Body, 'name') = 'LCP'
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
        ServiceName,
        'rum_vitals' AS SignalSource,
        'INP' AS SignalName,
        substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|INP')))), 1, 16) AS AttrFingerprint,
        toStartOfMinute(Timestamp) AS MinuteBucket,
        toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
        count() AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
    AND JSONExtractString(Body, 'name') = 'INP'
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
        ServiceName,
        'rum_vitals' AS SignalSource,
        'CLS' AS SignalName,
        substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|CLS')))), 1, 16) AS AttrFingerprint,
        toStartOfMinute(Timestamp) AS MinuteBucket,
        toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
        count() AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
    AND JSONExtractString(Body, 'name') = 'CLS'
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
        ServiceName,
        'rum_vitals' AS SignalSource,
        'TTFB' AS SignalName,
        substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|TTFB')))), 1, 16) AS AttrFingerprint,
        toStartOfMinute(Timestamp) AS MinuteBucket,
        toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
        count() AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
    AND JSONExtractString(Body, 'name') = 'TTFB'
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
        ServiceName,
        'rum_vitals' AS SignalSource,
        'FCP' AS SignalName,
        substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|FCP')))), 1, 16) AS AttrFingerprint,
        toStartOfMinute(Timestamp) AS MinuteBucket,
        toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
        count() AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
    AND JSONExtractString(Body, 'name') = 'FCP'
GROUP BY ServiceName, MinuteBucket
UNION ALL
SELECT
        ServiceName,
        'rum_vitals' AS SignalSource,
        'FID' AS SignalName,
        substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|FID')))), 1, 16) AS AttrFingerprint,
        toStartOfMinute(Timestamp) AS MinuteBucket,
        toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
        count() AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
    AND JSONExtractString(Body, 'name') = 'FID'
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
    ConditionsJson String DEFAULT '' CODEC(ZSTD(1)),
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

CREATE TABLE IF NOT EXISTS sobs_ai_settings (
    Key LowCardinality(String) CODEC(ZSTD(1)),
    Value String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Key
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_ai_memories (
    Id String CODEC(ZSTD(1)),
    ChatId String CODEC(ZSTD(1)),
    MemoryText String CODEC(ZSTD(1)),
    EmbeddingJson String CODEC(ZSTD(1)),
    SourceTurnId String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    UpdatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (ChatId, Id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_agent_rules (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    Description String CODEC(ZSTD(1)),
    TriggerType LowCardinality(String) CODEC(ZSTD(1)),
    TriggerRefId String CODEC(ZSTD(1)),
    TriggerState LowCardinality(String) CODEC(ZSTD(1)),
    Actions String CODEC(ZSTD(1)),
    RateLimitMinutes UInt32 DEFAULT 60 CODEC(T64, ZSTD(1)),
    IsEnabled UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_notification_channels (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    ChannelType LowCardinality(String) CODEC(ZSTD(1)),
    ConfigJson String CODEC(ZSTD(1)),
    Enabled UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_agent_runs (
    Id String CODEC(ZSTD(1)),
    RuleId String CODEC(ZSTD(1)),
    RuleName String CODEC(ZSTD(1)),
    TriggerContext String CODEC(ZSTD(1)),
    Status LowCardinality(String) CODEC(ZSTD(1)),
    GuardDecision LowCardinality(String) CODEC(ZSTD(1)),
    DlpResult LowCardinality(String) CODEC(ZSTD(1)),
    Analysis String CODEC(ZSTD(1)),
    Suggestion String CODEC(ZSTD(1)),
    GithubIssueUrl String CODEC(ZSTD(1)),
    ErrorMessage String CODEC(ZSTD(1)),
    CreatedAt DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    CompletedAt DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    IsDismissed UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_notification_rules (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    Enabled UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    LogicOperator LowCardinality(String) DEFAULT 'any' CODEC(ZSTD(1)),
    ConditionsJson String CODEC(ZSTD(1)),
    ChannelIds String CODEC(ZSTD(1)),
    Severity LowCardinality(String) DEFAULT 'warning' CODEC(ZSTD(1)),
    CooldownSeconds UInt32 DEFAULT 300 CODEC(T64, ZSTD(1)),
    LastFiredAt DateTime64(3) DEFAULT toDateTime64(0, 3) CODEC(Delta(8), ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_notification_log (
    Id String CODEC(ZSTD(1)),
    RuleId String CODEC(ZSTD(1)),
    RuleName String CODEC(ZSTD(1)),
    ChannelId String CODEC(ZSTD(1)),
    ChannelName String CODEC(ZSTD(1)),
    FiredAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    Status LowCardinality(String) CODEC(ZSTD(1)),
    ErrorMessage String CODEC(ZSTD(1)),
    Summary String CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(FiredAt)
ORDER BY (RuleId, FiredAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS sobs_app_settings (
    Key String,
    Value String CODEC(ZSTD(1)),
    UpdatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
) ENGINE = ReplacingMergeTree(UpdatedAt)
ORDER BY Key;

CREATE TABLE IF NOT EXISTS sobs_reports (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    Description String CODEC(ZSTD(1)),
    PageType LowCardinality(String) CODEC(ZSTD(1)),
    FiltersJson String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_cve_findings (
    Package String CODEC(ZSTD(1)),
    Ecosystem LowCardinality(String) CODEC(ZSTD(1)),
    Version String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    OsvId String CODEC(ZSTD(1)),
    CveIds String CODEC(ZSTD(1)),
    Summary String CODEC(ZSTD(1)),
    Severity LowCardinality(String) CODEC(ZSTD(1)),
    Published String CODEC(ZSTD(1)),
    ScannedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
) ENGINE = ReplacingMergeTree(ScannedAt)
ORDER BY (Package, Ecosystem, Version, OsvId)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_cve_dispositions (
    OsvId String CODEC(ZSTD(1)),
    Package String CODEC(ZSTD(1)),
    Ecosystem LowCardinality(String) CODEC(ZSTD(1)),
    Version String CODEC(ZSTD(1)),
    Disposition LowCardinality(String) CODEC(ZSTD(1)),
    Note String CODEC(ZSTD(1)),
    CreatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    UpdatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    Version_ UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version_)
ORDER BY (OsvId, Package, Ecosystem, Version)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_apps (
    Id String CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    Slug String CODEC(ZSTD(1)),
    OwnerTeam String CODEC(ZSTD(1)),
    RepoUrl String CODEC(ZSTD(1)),
    DefaultEnvironment String CODEC(ZSTD(1)),
    Enabled UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    MetadataJson String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    CreatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    UpdatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (Slug, Id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_app_releases (
    Id String CODEC(ZSTD(1)),
    AppId String CODEC(ZSTD(1)),
    ReleaseVersion String CODEC(ZSTD(1)),
    CommitSha String CODEC(ZSTD(1)),
    BuildId String CODEC(ZSTD(1)),
    Environment String CODEC(ZSTD(1)),
    ReleasedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    MetadataJson String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (AppId, ReleaseVersion, Id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_release_artifacts (
    Id String CODEC(ZSTD(1)),
    ReleaseId String CODEC(ZSTD(1)),
    ArtifactType LowCardinality(String) CODEC(ZSTD(1)),
    Name String CODEC(ZSTD(1)),
    ContentType String CODEC(ZSTD(1)),
    Size UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    StorageRef String CODEC(ZSTD(1)),
    ChecksumSha256 String CODEC(ZSTD(1)),
    Platform String CODEC(ZSTD(1)),
    Architecture String CODEC(ZSTD(1)),
    MetadataJson String CODEC(ZSTD(1)),
    UploadedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (ReleaseId, ArtifactType, Name, Id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_github_work_items (
    Id String CODEC(ZSTD(1)),
    CreatedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    CompletedAt DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    AgentRunId String CODEC(ZSTD(1)),
    AgentRuleId String CODEC(ZSTD(1)),
    AgentRuleName String CODEC(ZSTD(1)),
    AgentAction LowCardinality(String) CODEC(ZSTD(1)),
    ServiceName String CODEC(ZSTD(1)),
    AnomalyRuleId String CODEC(ZSTD(1)),
    AnomalyState LowCardinality(String) CODEC(ZSTD(1)),
    SignalSource String CODEC(ZSTD(1)),
    SignalName String CODEC(ZSTD(1)),
    SignalValue Float64 CODEC(ZSTD(1)),
    GithubRepo String CODEC(ZSTD(1)),
    DedupKey String CODEC(ZSTD(1)),
    DedupDecision LowCardinality(String) DEFAULT 'new_issue' CODEC(ZSTD(1)),
    DedupConfidence Float64 DEFAULT 0 CODEC(ZSTD(1)),
    IssueNumber UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    IssueUrl String CODEC(ZSTD(1)),
    CanonicalIssueNumber UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    CanonicalIssueUrl String CODEC(ZSTD(1)),
    RelatedIssueUrls String CODEC(ZSTD(1)),
    OccurrenceCount UInt32 DEFAULT 1 CODEC(T64, ZSTD(1)),
    IssueState LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    IssueTitle String CODEC(ZSTD(1)),
    AnalysisSummary String CODEC(ZSTD(1)),
    SuggestionSummary String CODEC(ZSTD(1)),
    CopilotAssignmentRequestedAt UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    CopilotAssignmentStatus LowCardinality(String) DEFAULT 'not_requested' CODEC(ZSTD(1)),
    CopilotAssignmentReason String CODEC(ZSTD(1)),
    PrLinked UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    PrNumber UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    PrUrl String CODEC(ZSTD(1)),
    IsDeleted UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    Version UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (CreatedAt, AgentRunId)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_raw_windows (
    Id String CODEC(ZSTD(1)),
    SignalTs DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    WindowStart DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    WindowEnd DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    SignalType LowCardinality(String) CODEC(ZSTD(1)),
    SignalRef String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Namespace LowCardinality(String) CODEC(ZSTD(1)),
    NodeName LowCardinality(String) CODEC(ZSTD(1)),
    CreatedAt DateTime64(9) DEFAULT now64(9) CODEC(Delta(8), ZSTD(1)),
    Version UInt64 DEFAULT toUnixTimestamp64Milli(now64(9)) CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (WindowStart, WindowEnd, SignalType, SignalRef, ServiceName)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS sobs_raw_window_copy_state (
    WindowId String CODEC(ZSTD(1)),
    SourceTable LowCardinality(String) CODEC(ZSTD(1)),
    LastCopiedAt DateTime64(9) DEFAULT now64(9) CODEC(Delta(8), ZSTD(1)),
    Version UInt64 DEFAULT toUnixTimestamp64Milli(now64(9)) CODEC(T64, ZSTD(1))
) ENGINE = ReplacingMergeTree(Version)
ORDER BY (WindowId, SourceTable)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS otel_metrics_gauge_pinned (
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

CREATE TABLE IF NOT EXISTS otel_metrics_sum_pinned (
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

CREATE TABLE IF NOT EXISTS otel_metrics_histogram_pinned (
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

CREATE VIEW IF NOT EXISTS v_otel_metrics_dedup AS
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    0 AS SourceRank
FROM otel_metrics_gauge
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    1 AS SourceRank
FROM otel_metrics_gauge_pinned
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    0 AS SourceRank
FROM otel_metrics_sum
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    1 AS SourceRank
FROM otel_metrics_sum_pinned
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    if(Count = 0, 0.0, toFloat64(Sum) / toFloat64(Count)) AS Value,
    0 AS SourceRank
FROM otel_metrics_histogram
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    if(Count = 0, 0.0, toFloat64(Sum) / toFloat64(Count)) AS Value,
    1 AS SourceRank
FROM otel_metrics_histogram_pinned;

CREATE VIEW IF NOT EXISTS v_otel_metrics_signal_context AS
WITH metric_points AS (
    SELECT
        'gauge' AS MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        toFloat64(Value) AS Value,
        0 AS SourceRank
    FROM otel_metrics_gauge
    UNION ALL
    SELECT
        'gauge' AS MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        toFloat64(Value) AS Value,
        1 AS SourceRank
    FROM otel_metrics_gauge_pinned
    UNION ALL
    SELECT
        'sum' AS MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        toFloat64(Value) AS Value,
        0 AS SourceRank
    FROM otel_metrics_sum
    UNION ALL
    SELECT
        'sum' AS MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        toFloat64(Value) AS Value,
        1 AS SourceRank
    FROM otel_metrics_sum_pinned
    UNION ALL
    SELECT
        'histogram' AS MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        if(Count = 0, 0.0, toFloat64(Sum) / toFloat64(Count)) AS Value,
        0 AS SourceRank
    FROM otel_metrics_histogram
    UNION ALL
    SELECT
        'histogram' AS MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        if(Count = 0, 0.0, toFloat64(Sum) / toFloat64(Count)) AS Value,
        1 AS SourceRank
    FROM otel_metrics_histogram_pinned
), dedup_points AS (
    SELECT
        MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint,
        argMin(Value, SourceRank) AS Value,
        min(SourceRank) AS StorageRank
    FROM metric_points
    GROUP BY
        MetricKind,
        TimeUnix,
        ServiceName,
        MetricName,
        MetricDescription,
        MetricUnit,
        Attributes,
        AttrFingerprint
)
SELECT
    w.Id AS WindowId,
    w.SignalTs,
    w.WindowStart,
    w.WindowEnd,
    w.SignalType,
    w.SignalRef,
    w.ServiceName AS SignalServiceName,
    w.Namespace,
    w.NodeName,
    m.TimeUnix,
    m.ServiceName AS MetricServiceName,
    m.MetricName,
    m.MetricDescription,
    m.MetricUnit,
    m.MetricKind,
    m.Attributes,
    m.AttrFingerprint,
    m.Value,
    multiIf(m.StorageRank = 0, 'raw', m.StorageRank = 1, 'pinned', 'mixed') AS StorageTier
FROM sobs_raw_windows AS w
INNER JOIN dedup_points AS m
    ON m.TimeUnix >= w.WindowStart
    AND m.TimeUnix <= w.WindowEnd
    AND (w.ServiceName = '' OR m.ServiceName = w.ServiceName)
    AND (
        w.Namespace = ''
        OR m.Attributes['k8s.namespace.name'] = w.Namespace
        OR m.Attributes['namespace'] = w.Namespace
    )
    AND (
        w.NodeName = ''
        OR m.Attributes['k8s.node.name'] = w.NodeName
        OR m.Attributes['node'] = w.NodeName
    );

`
