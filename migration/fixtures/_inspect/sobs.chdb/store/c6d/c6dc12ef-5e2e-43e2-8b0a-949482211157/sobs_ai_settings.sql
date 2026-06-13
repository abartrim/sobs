ATTACH TABLE _ UUID 'c1fd06de-f8ec-42e0-aa3d-2df2a9601179'
(
    `Key` LowCardinality(String) CODEC(ZSTD(1)),
    `Value` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Key
SETTINGS index_granularity = 8192
