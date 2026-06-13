ATTACH VIEW _ UUID '9e38af08-1044-473a-9812-a90f971e19d4'
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
