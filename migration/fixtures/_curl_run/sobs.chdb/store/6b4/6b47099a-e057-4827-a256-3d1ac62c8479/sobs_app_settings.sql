ATTACH TABLE _ UUID '64dafcfa-9359-4d0f-8510-42834ad91962'
(
    `Key` String,
    `Value` String CODEC(ZSTD(1)),
    `UpdatedAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1))
)
ENGINE = ReplacingMergeTree(UpdatedAt)
ORDER BY Key
SETTINGS index_granularity = 8192
