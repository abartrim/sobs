ATTACH TABLE _ UUID 'bb20a77e-8c63-476b-9c51-8b353ab496fd'
(
    `Key` LowCardinality(String) CODEC(ZSTD(1)),
    `Value` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Key
SETTINGS index_granularity = 8192
