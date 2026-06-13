ATTACH TABLE _ UUID '8bba4b70-ebab-4846-a6cd-d2ed0f5a9643'
(
    `OsvId` String CODEC(ZSTD(1)),
    `Package` String CODEC(ZSTD(1)),
    `Ecosystem` LowCardinality(String) CODEC(ZSTD(1)),
    `Version` String CODEC(ZSTD(1)),
    `Disposition` LowCardinality(String) CODEC(ZSTD(1)),
    `Note` String CODEC(ZSTD(1)),
    `CreatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    `Version_` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version_)
ORDER BY (OsvId, Package, Ecosystem, Version)
SETTINGS index_granularity = 8192
