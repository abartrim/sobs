ATTACH TABLE _ UUID '575fed47-d594-4087-8a2e-3a98634ba9e2'
(
    `Id` String CODEC(ZSTD(1)),
    `RuleId` String CODEC(ZSTD(1)),
    `RuleName` String CODEC(ZSTD(1)),
    `ChannelId` String CODEC(ZSTD(1)),
    `ChannelName` String CODEC(ZSTD(1)),
    `FiredAt` DateTime64(3) DEFAULT now64(3) CODEC(Delta(8), ZSTD(1)),
    `Status` LowCardinality(String) CODEC(ZSTD(1)),
    `ErrorMessage` String CODEC(ZSTD(1)),
    `Summary` String CODEC(ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(FiredAt)
ORDER BY (RuleId, FiredAt)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
