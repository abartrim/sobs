ATTACH TABLE _ UUID 'c99cb2a3-0264-454d-9e05-62d2fe06b640'
(
    `TimeUnix` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `TimeUnixMs` DateTime DEFAULT toDateTime(TimeUnix) CODEC(Delta(4), ZSTD(1)),
    `ServiceName` LowCardinality(String) CODEC(ZSTD(1)),
    `MetricName` LowCardinality(String) CODEC(ZSTD(1)),
    `MetricDescription` String CODEC(ZSTD(1)),
    `MetricUnit` LowCardinality(String) CODEC(ZSTD(1)),
    `Attributes` Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    `Value` Float64 CODEC(ZSTD(1)),
    `Flags` UInt32 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `AttrFingerprint` String CODEC(ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(TimeUnixMs)
ORDER BY (ServiceName, MetricName, AttrFingerprint, TimeUnixMs, TimeUnix)
TTL TimeUnixMs + toIntervalDay(14)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
