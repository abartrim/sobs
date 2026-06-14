ATTACH MATERIALIZED VIEW _ UUID '3aac540f-4747-4a1f-a857-dbde66704c62' TO default.otel_metrics_1m_agg
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
    'histogram' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avgState(if(Count > 0, Sum / Count, 0)) AS Value,
    sumState(Count) AS SampleCount
FROM default.otel_metrics_histogram
GROUP BY
    ServiceName,
    MetricName,
    AttrFingerprint,
    MinuteBucket
