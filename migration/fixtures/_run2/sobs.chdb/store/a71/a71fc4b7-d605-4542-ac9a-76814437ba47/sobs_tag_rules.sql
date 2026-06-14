ATTACH TABLE _ UUID '821a2350-715d-48c3-8c3e-89af11ca92fd'
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
