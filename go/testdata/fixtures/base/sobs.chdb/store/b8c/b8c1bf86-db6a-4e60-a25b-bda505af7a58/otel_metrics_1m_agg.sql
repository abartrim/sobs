ATTACH TABLE _ UUID 'f0c13ee3-c7d0-4e3e-92f1-2bac9d907ac4'
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
