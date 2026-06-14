ATTACH TABLE _ UUID 'c8307261-4258-410b-9121-0d96510ec015'
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
