ATTACH TABLE _ UUID 'cb6e0c45-de2c-404d-b66d-fea43924e79d'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `Enabled` UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    `LogicOperator` LowCardinality(String) DEFAULT 'any' CODEC(ZSTD(1)),
    `ConditionsJson` String CODEC(ZSTD(1)),
    `ChannelIds` String CODEC(ZSTD(1)),
    `Severity` LowCardinality(String) DEFAULT 'warning' CODEC(ZSTD(1)),
    `CooldownSeconds` UInt32 DEFAULT 300 CODEC(T64, ZSTD(1)),
    `LastFiredAt` DateTime64(3) DEFAULT toDateTime64(0, 3) CODEC(Delta(8), ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192
