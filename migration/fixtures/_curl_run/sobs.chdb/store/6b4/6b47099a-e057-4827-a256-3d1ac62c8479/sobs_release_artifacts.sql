ATTACH TABLE _ UUID '007ac075-ab01-4c96-8de9-6eb9f2008cdb'
(
    `Id` String CODEC(ZSTD(1)),
    `ReleaseId` String CODEC(ZSTD(1)),
    `ArtifactType` LowCardinality(String) CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `ContentType` String CODEC(ZSTD(1)),
    `Size` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `StorageRef` String CODEC(ZSTD(1)),
    `ChecksumSha256` String CODEC(ZSTD(1)),
    `Platform` String CODEC(ZSTD(1)),
    `Architecture` String CODEC(ZSTD(1)),
    `MetadataJson` String CODEC(ZSTD(1)),
    `UploadedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (ReleaseId, ArtifactType, Name, Id)
SETTINGS index_granularity = 8192
