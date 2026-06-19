ATTACH VIEW _ UUID 'd261c905-98bb-4585-8d58-bfb572bdf013'
(
    `TimeUnix` DateTime64(9),
    `ServiceName` LowCardinality(String),
    `MetricName` LowCardinality(String),
    `Attributes` Map(LowCardinality(String), String),
    `AttrFingerprint` String,
    `Value` Float64,
    `SourceRank` UInt8
)
AS SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    0 AS SourceRank
FROM default.otel_metrics_gauge
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    1 AS SourceRank
FROM default.otel_metrics_gauge_pinned
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    0 AS SourceRank
FROM default.otel_metrics_sum
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    toFloat64(Value) AS Value,
    1 AS SourceRank
FROM default.otel_metrics_sum_pinned
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    if(Count = 0, 0., toFloat64(Sum) / toFloat64(Count)) AS Value,
    0 AS SourceRank
FROM default.otel_metrics_histogram
UNION ALL
SELECT
    TimeUnix,
    ServiceName,
    MetricName,
    Attributes,
    AttrFingerprint,
    if(Count = 0, 0., toFloat64(Sum) / toFloat64(Count)) AS Value,
    1 AS SourceRank
FROM default.otel_metrics_histogram_pinned
