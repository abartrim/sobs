ATTACH TABLE _ UUID 'a9e5d364-27c5-4844-b222-1a365273a5c7'
(
    `Package` String CODEC(ZSTD(1)),
    `Ecosystem` LowCardinality(String) CODEC(ZSTD(1)),
    `Version` String CODEC(ZSTD(1)),
    `ServiceName` LowCardinality(String) CODEC(ZSTD(1)),
    `OsvId` String CODEC(ZSTD(1)),
    `CveIds` String CODEC(ZSTD(1)),
    `Summary` String CODEC(ZSTD(1)),
    `Severity` LowCardinality(String) CODEC(ZSTD(1)),
    `Published` String CODEC(ZSTD(1)),
    `ScannedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(ScannedAt)
ORDER BY (Package, Ecosystem, Version, OsvId)
SETTINGS index_granularity = 8192
