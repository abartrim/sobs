ATTACH TABLE _ UUID 'c4c239d8-34fd-4a89-8cb4-26c54dfdf11e'
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
