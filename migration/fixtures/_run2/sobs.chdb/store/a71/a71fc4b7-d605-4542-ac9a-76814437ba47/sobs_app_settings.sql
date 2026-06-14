ATTACH TABLE _ UUID '2f82c174-3cfa-4a7a-9fe4-2d609b16cc5c'
(
    `Key` String,
    `Value` String CODEC(ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(UpdatedAt)
ORDER BY Key
SETTINGS index_granularity = 8192
