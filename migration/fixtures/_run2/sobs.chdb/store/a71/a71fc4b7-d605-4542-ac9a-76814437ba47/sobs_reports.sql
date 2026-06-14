ATTACH TABLE _ UUID '9bac945b-8fa8-415c-b3ca-677f837a6867'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `Description` String CODEC(ZSTD(1)),
    `PageType` LowCardinality(String) CODEC(ZSTD(1)),
    `FiltersJson` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192
