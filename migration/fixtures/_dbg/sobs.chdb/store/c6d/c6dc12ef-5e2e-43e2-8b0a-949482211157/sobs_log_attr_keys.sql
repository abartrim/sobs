ATTACH TABLE _ UUID '74285653-a41a-46b1-a4e7-4f34c8dbd161'
(
    `RecordType` LowCardinality(String) CODEC(ZSTD(1)),
    `AttrKey` LowCardinality(String) CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, AttrKey)
SETTINGS index_granularity = 8192
