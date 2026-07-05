ATTACH TABLE _ UUID '5549be1f-1f7d-41c1-8b0e-5f91fa73459d'
(
    `WindowId` String CODEC(ZSTD(1)),
    `SourceTable` LowCardinality(String) CODEC(ZSTD(1)),
    `LastCopiedAt` DateTime64(9) DEFAULT now64(9) CODEC(Delta(8), ZSTD(1)),
    `Version` UInt64 DEFAULT toUnixTimestamp64Milli(now64(9)) CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (WindowId, SourceTable)
SETTINGS index_granularity = 8192
