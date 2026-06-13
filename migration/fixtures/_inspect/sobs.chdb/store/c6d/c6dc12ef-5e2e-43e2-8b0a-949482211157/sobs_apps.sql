ATTACH TABLE _ UUID '46131e48-b797-4919-aad2-c8cf2ff4b644'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `Slug` String CODEC(ZSTD(1)),
    `OwnerTeam` String CODEC(ZSTD(1)),
    `RepoUrl` String CODEC(ZSTD(1)),
    `DefaultEnvironment` String CODEC(ZSTD(1)),
    `Enabled` UInt8 DEFAULT 1 CODEC(T64, ZSTD(1)),
    `MetadataJson` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `CreatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (Slug, Id)
SETTINGS index_granularity = 8192
