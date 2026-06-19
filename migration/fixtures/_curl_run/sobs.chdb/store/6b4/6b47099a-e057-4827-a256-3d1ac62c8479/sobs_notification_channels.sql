ATTACH TABLE _ UUID '694c40a4-c615-4bbf-9103-f32ad3b2cd49'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `ChannelType` LowCardinality(String) CODEC(ZSTD(1)),
    `ConfigJson` String CODEC(ZSTD(1)),
    `Enabled` UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192
