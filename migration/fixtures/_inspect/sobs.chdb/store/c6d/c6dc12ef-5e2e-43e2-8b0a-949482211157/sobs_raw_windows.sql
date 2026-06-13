ATTACH TABLE _ UUID '55e39668-2661-4706-a7e6-2f30fc31cd75'
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
