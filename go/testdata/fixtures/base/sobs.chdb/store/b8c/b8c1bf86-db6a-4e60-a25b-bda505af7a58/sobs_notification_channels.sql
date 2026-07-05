ATTACH TABLE _ UUID '48eb5735-f4ec-4283-9602-a82a07c850d9'
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
