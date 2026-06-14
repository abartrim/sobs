ATTACH TABLE _ UUID '61365080-5bb7-492c-9bd3-3466a49ed77e'
(
    `RecordType` LowCardinality(String) CODEC(ZSTD(1)),
    `AttrKey` LowCardinality(String) CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (RecordType, AttrKey)
SETTINGS index_granularity = 8192
