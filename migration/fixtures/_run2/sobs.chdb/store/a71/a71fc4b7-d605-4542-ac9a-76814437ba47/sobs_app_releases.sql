ATTACH TABLE _ UUID '45b5282c-89ea-404f-9d23-7d78c29658c3'
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
