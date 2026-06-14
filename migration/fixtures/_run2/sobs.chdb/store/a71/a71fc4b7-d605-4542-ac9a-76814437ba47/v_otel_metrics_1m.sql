ATTACH VIEW _ UUID 'd3bc524e-3536-40ff-8c2c-f483a41844e0'
(
    `ServiceName` String,
    `MetricName` String,
    `AttrFingerprint` String,
    `MetricKind` String,
    `MinuteBucket` DateTime,
    `Value` Float64,
    `SampleCount` UInt64
)
AS SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    MetricKind,
    MinuteBucket,
    avgMerge(Value) AS Value,
    sumMerge(SampleCount) AS SampleCount
FROM default.otel_metrics_1m_agg
GROUP BY
    ServiceName,
    MetricName,
    AttrFingerprint,
    MetricKind,
    MinuteBucket
