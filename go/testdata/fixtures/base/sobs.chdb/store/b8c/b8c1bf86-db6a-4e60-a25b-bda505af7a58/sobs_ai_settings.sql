ATTACH TABLE _ UUID '1ffaffef-0812-4ea5-875b-bef79855de5c'
(
    `Key` LowCardinality(String) CODEC(ZSTD(1)),
    `Value` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Key
SETTINGS index_granularity = 8192
