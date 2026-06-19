ATTACH TABLE _ UUID '342700b5-be57-4cd8-8fff-9e918cd07a5c'
(
    `ServiceName` String,
    `MetricName` String,
    `AttrFingerprint` String,
    `MetricKind` String,
    `MinuteBucket` DateTime,
    `Value` AggregateFunction(avg, Float64),
    `SampleCount` AggregateFunction(sum, UInt64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(MinuteBucket)
ORDER BY (ServiceName, MetricName, AttrFingerprint, MetricKind, MinuteBucket)
SETTINGS index_granularity = 8192
