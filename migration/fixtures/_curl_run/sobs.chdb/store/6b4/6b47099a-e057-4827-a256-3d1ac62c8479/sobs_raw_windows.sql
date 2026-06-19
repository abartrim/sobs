ATTACH TABLE _ UUID '1d5aacad-a0a9-49a6-b67a-376e1b5eb294'
(
    `Id` String CODEC(ZSTD(1)),
    `SignalTs` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `WindowStart` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `WindowEnd` DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    `SignalType` LowCardinality(String) CODEC(ZSTD(1)),
    `SignalRef` String CODEC(ZSTD(1)),
    `ServiceName` LowCardinality(String) CODEC(ZSTD(1)),
    `Namespace` LowCardinality(String) CODEC(ZSTD(1)),
    `NodeName` LowCardinality(String) CODEC(ZSTD(1)),
    `CreatedAt` DateTime64(9) DEFAULT now64(9) CODEC(Delta(8), ZSTD(1)),
    `Version` UInt64 DEFAULT toUnixTimestamp64Milli(now64(9)) CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (WindowStart, WindowEnd, SignalType, SignalRef, ServiceName)
SETTINGS index_granularity = 8192
