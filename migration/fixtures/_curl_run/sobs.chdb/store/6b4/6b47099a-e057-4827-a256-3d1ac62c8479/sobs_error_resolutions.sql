ATTACH TABLE _ UUID '2d20d996-0920-4492-86f2-5adc04056694'
(
    `ErrorId` String CODEC(ZSTD(1)),
    `ResolvedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = MergeTree
ORDER BY (ErrorId, ResolvedAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
