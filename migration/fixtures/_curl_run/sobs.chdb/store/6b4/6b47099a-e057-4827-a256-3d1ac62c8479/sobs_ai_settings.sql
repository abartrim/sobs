ATTACH TABLE _ UUID '0fb858d9-e85f-4dd1-8771-508c79af00d9'
(
    `Key` LowCardinality(String) CODEC(ZSTD(1)),
    `Value` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Key
SETTINGS index_granularity = 8192
