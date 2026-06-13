ATTACH TABLE _ UUID '5d957c79-4295-40f5-9fa1-b157c9a3720f'
(
    `Id` String CODEC(ZSTD(1)),
    `ChatId` String CODEC(ZSTD(1)),
    `MemoryText` String CODEC(ZSTD(1)),
    `EmbeddingJson` String CODEC(ZSTD(1)),
    `SourceTurnId` String CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (ChatId, Id)
SETTINGS index_granularity = 8192
