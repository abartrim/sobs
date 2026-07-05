ATTACH TABLE _ UUID '38ac567c-13e2-4fc8-806a-cf1847e10f14'
(
    `Id` String CODEC(ZSTD(1)),
    `RuleId` String CODEC(ZSTD(1)),
    `RuleName` String CODEC(ZSTD(1)),
    `TriggerContext` String CODEC(ZSTD(1)),
    `Status` LowCardinality(String) CODEC(ZSTD(1)),
    `GuardDecision` LowCardinality(String) CODEC(ZSTD(1)),
    `DlpResult` LowCardinality(String) CODEC(ZSTD(1)),
    `Analysis` String CODEC(ZSTD(1)),
    `Suggestion` String CODEC(ZSTD(1)),
    `GithubIssueUrl` String CODEC(ZSTD(1)),
    `ErrorMessage` String CODEC(ZSTD(1)),
    `CreatedAt` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `CompletedAt` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `IsDismissed` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY Id
SETTINGS index_granularity = 8192
