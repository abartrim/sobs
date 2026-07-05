ATTACH VIEW _ UUID 'a05767cb-825b-495e-be18-740daf84b8b2'
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
