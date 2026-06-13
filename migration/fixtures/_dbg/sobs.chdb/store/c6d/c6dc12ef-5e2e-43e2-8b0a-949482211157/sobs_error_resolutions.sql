ATTACH TABLE _ UUID '50c61b02-f1af-4a3d-9900-f925185921bd'
(
    `ErrorId` String CODEC(ZSTD(1)),
    `ResolvedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = MergeTree
ORDER BY (ErrorId, ResolvedAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
