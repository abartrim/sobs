ATTACH VIEW _ UUID '11ed7d25-1569-47d1-86b0-3608de68a60d'
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
