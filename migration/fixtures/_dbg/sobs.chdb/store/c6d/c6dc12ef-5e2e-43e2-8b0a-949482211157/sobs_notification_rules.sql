ATTACH TABLE _ UUID '73ca2d9b-0cc6-4be8-9cac-5e26c3e5e81b'
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
