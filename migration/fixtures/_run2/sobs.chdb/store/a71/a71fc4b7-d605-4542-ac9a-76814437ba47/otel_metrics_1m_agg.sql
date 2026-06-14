ATTACH TABLE _ UUID 'd189d908-a8c0-4257-968e-63680d93ba7b'
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
