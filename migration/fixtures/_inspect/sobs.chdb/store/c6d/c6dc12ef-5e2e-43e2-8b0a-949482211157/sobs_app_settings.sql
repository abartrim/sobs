ATTACH TABLE _ UUID '4acb1567-b462-4a4d-8f98-1aac69dc5459'
(
    `Key` String,
    `Value` String CODEC(ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(UpdatedAt)
ORDER BY Key
SETTINGS index_granularity = 8192
