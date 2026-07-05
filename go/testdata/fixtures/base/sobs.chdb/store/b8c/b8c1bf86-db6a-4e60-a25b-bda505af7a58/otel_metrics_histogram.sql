ATTACH TABLE _ UUID '25d7e6d4-d8f4-4d54-84bd-f5e403efa445'
(
    `TimeUnix` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `TimeUnixMs` DateTime DEFAULT toDateTime(TimeUnix) CODEC(Delta(4), ZSTD(1)),
    `ServiceName` LowCardinality(String) CODEC(ZSTD(1)),
    `MetricName` LowCardinality(String) CODEC(ZSTD(1)),
    `MetricDescription` String CODEC(ZSTD(1)),
    `MetricUnit` LowCardinality(String) CODEC(ZSTD(1)),
    `Attributes` Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    `Count` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Sum` Float64 CODEC(ZSTD(1)),
    `BucketCounts` Array(UInt64) CODEC(ZSTD(1)),
    `ExplicitBounds` Array(Float64) CODEC(ZSTD(1)),
    `Flags` UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `AggregationTemporality` Int32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `AttrFingerprint` String CODEC(ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(TimeUnixMs)
ORDER BY (ServiceName, MetricName, AttrFingerprint, TimeUnixMs, TimeUnix)
TTL TimeUnixMs + toIntervalHour(48)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
