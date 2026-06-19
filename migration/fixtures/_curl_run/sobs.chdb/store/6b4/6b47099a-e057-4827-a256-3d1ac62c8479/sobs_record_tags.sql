ATTACH TABLE _ UUID '89c213bf-6c19-4f2f-8d96-d899c5cc81c8'
(
    `RecordType` LowCardinality(String) CODEC(ZSTD(1)),
    `RecordId` String CODEC(ZSTD(1)),
    `TagKey` LowCardinality(String) CODEC(ZSTD(1)),
    `TagValue` String CODEC(ZSTD(1)),
    `IsAuto` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, RecordId, TagKey)
SETTINGS index_granularity = 8192
