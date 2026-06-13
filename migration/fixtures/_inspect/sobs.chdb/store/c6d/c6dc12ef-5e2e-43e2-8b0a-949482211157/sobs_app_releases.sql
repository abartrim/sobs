ATTACH TABLE _ UUID 'c1c2b22a-e357-4275-8532-19e05c88385b'
(
    `Id` String CODEC(ZSTD(1)),
    `AppId` String CODEC(ZSTD(1)),
    `ReleaseVersion` String CODEC(ZSTD(1)),
    `CommitSha` String CODEC(ZSTD(1)),
    `BuildId` String CODEC(ZSTD(1)),
    `Environment` String CODEC(ZSTD(1)),
    `ReleasedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    `MetadataJson` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (AppId, ReleaseVersion, Id)
SETTINGS index_granularity = 8192
