ATTACH TABLE _ UUID '9968b9fd-15ce-4a26-b1e5-fcfe62af0cc9'
(
    `ErrorId` String CODEC(ZSTD(1)),
    `ResolvedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = MergeTree
ORDER BY (ErrorId, ResolvedAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
