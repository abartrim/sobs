ATTACH TABLE _ UUID '31450970-e4b7-46ba-a5a8-af34fa0d7561'
(
    `Id` String CODEC(ZSTD(1)),
    `DashboardId` String CODEC(ZSTD(1)),
    `Title` String CODEC(ZSTD(1)),
    `ChartType` LowCardinality(String) CODEC(ZSTD(1)),
    `Query` String CODEC(ZSTD(1)),
    `OptionsJson` String CODEC(ZSTD(1)),
    `Position` UInt16 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (DashboardId, Id)
SETTINGS index_granularity = 8192
