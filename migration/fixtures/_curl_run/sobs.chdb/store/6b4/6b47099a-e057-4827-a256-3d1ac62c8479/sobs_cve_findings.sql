ATTACH TABLE _ UUID '63e28b29-f383-4fad-b152-0cb943ea0828'
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
