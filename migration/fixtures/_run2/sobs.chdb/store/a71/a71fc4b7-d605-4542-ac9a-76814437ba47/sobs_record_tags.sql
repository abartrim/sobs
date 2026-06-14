ATTACH TABLE _ UUID '2d72aeb8-c313-45be-ac79-81cd2f24f0b3'
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
