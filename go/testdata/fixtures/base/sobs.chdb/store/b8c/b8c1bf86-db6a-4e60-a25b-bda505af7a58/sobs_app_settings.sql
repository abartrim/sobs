ATTACH TABLE _ UUID '453e177b-a52c-4630-b087-70bbf6539b04'
(
    `Key` String,
    `Value` String CODEC(ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(UpdatedAt)
ORDER BY Key
SETTINGS index_granularity = 8192
