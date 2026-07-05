ATTACH TABLE _ UUID 'd733fe9d-617e-4652-bc12-c514a8b8d0ad'
(
    `ErrorId` String CODEC(ZSTD(1)),
    `ResolvedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = MergeTree
ORDER BY (ErrorId, ResolvedAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
