ATTACH TABLE _ UUID 'ad3cd90f-1139-42d3-8872-46f7d930e9eb'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `Description` String CODEC(ZSTD(1)),
    `TriggerType` LowCardinality(String) CODEC(ZSTD(1)),
    `TriggerRefId` String CODEC(ZSTD(1)),
    `TriggerState` LowCardinality(String) CODEC(ZSTD(1)),
    `Actions` String CODEC(ZSTD(1)),
    `RateLimitMinutes` UInt32 DEFAULT 60 CODEC(T64, ZSTD(1)),
    `IsEnabled` UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192
