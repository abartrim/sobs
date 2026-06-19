ATTACH MATERIALIZED VIEW _ UUID '2748b9a4-cdb0-41bb-b6fc-3b10b08d76a1' TO default.otel_metrics_1m_agg
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
    'sum' AS MetricKind,
    toStartOfMinute(TimeUnix) AS MinuteBucket,
    avgState(Value) AS Value,
    sumState(toUInt64(1)) AS SampleCount
FROM default.otel_metrics_sum
GROUP BY
    ServiceName,
    MetricName,
    AttrFingerprint,
    MinuteBucket
