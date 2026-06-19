ATTACH TABLE _ UUID 'eee3a4d5-79bb-4758-9973-d40139925e48'
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
