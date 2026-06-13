ATTACH VIEW _ UUID 'bdf66b13-1c79-4d9c-a313-16c61c85d716'
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
