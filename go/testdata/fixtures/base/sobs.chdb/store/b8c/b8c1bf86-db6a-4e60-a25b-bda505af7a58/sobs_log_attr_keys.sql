ATTACH TABLE _ UUID '597828a0-fb75-4eb2-8f17-55771d518d0c'
(
    `RecordType` LowCardinality(String) CODEC(ZSTD(1)),
    `AttrKey` LowCardinality(String) CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, AttrKey)
SETTINGS index_granularity = 8192
