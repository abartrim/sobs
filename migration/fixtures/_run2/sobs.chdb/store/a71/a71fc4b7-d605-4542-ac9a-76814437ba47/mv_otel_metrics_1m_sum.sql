ATTACH MATERIALIZED VIEW _ UUID '7af68687-3bfc-4bf5-9662-cc4bec1e64ee' TO default.otel_metrics_1m_agg
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
