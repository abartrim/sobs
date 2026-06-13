ATTACH TABLE _ UUID 'e205619c-438e-462a-8a29-fb42006057e6'
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
