ATTACH MATERIALIZED VIEW _ UUID 'f596487e-ff7f-4d39-afcc-0c8f8ab1e51c' TO default.otel_metrics_1m_agg
(
    `ServiceName` LowCardinality(String),
    `MetricName` LowCardinality(String),
    `AttrFingerprint` String,
    `MetricKind` String,
    `MinuteBucket` DateTime,
    `Value` AggregateFunction(avg, Float64),
    `SampleCount` AggregateFunction(sum, UInt64)
)
AS SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    'gauge' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avgState(Value) AS Value,
    sumState(toUInt64(1)) AS SampleCount
FROM default.otel_metrics_gauge
GROUP BY
    ServiceName,
    MetricName,
    AttrFingerprint,
    MinuteBucket
