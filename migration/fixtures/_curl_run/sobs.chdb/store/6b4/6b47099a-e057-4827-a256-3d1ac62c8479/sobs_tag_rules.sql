ATTACH TABLE _ UUID 'fa32f1b3-daa7-4669-b41f-c0c9edb2f911'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `RecordTypes` String CODEC(ZSTD(1)),
    `MatchField` LowCardinality(String) CODEC(ZSTD(1)),
    `MatchOperator` LowCardinality(String) CODEC(ZSTD(1)),
    `MatchValue` String CODEC(ZSTD(1)),
    `MatchAttrKey` String CODEC(ZSTD(1)),
    `TagKey` String CODEC(ZSTD(1)),
    `TagValue` String CODEC(ZSTD(1)),
    `ConditionsJson` String DEFAULT '' CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192
