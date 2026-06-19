ATTACH TABLE _ UUID 'c3475fb5-d791-4c6e-8ae2-89e3c6d1a5a2'
(
    `WindowId` String CODEC(ZSTD(1)),
    `SourceTable` LowCardinality(String) CODEC(ZSTD(1)),
    `LastCopiedAt` DateTime64(9) DEFAULT now64(9) CODEC(Delta(8), ZSTD(1)),
    `Version` UInt64 DEFAULT toUnixTimestamp64Milli(now64(9)) CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (WindowId, SourceTable)
SETTINGS index_granularity = 8192
