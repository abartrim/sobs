ATTACH TABLE _ UUID '5b79dd11-d175-4e67-ae91-05d8fad88e91'
(
    `RecordType` LowCardinality(String) CODEC(ZSTD(1)),
    `AttrKey` LowCardinality(String) CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, AttrKey)
SETTINGS index_granularity = 8192
