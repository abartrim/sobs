ATTACH TABLE _ UUID 'b583d4a0-2deb-4257-a2bf-4dc33bd98240'
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
