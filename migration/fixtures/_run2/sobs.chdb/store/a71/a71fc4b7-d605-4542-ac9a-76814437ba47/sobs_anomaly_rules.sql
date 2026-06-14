ATTACH TABLE _ UUID 'e378f110-58d5-463b-8c75-e0ec1ce3d54d'
(
    `Id` String CODEC(ZSTD(1)),
    `Name` String CODEC(ZSTD(1)),
    `RuleType` LowCardinality(String) DEFAULT 'threshold' CODEC(ZSTD(1)),
    `SignalSource` LowCardinality(String) CODEC(ZSTD(1)),
    `SignalName` LowCardinality(String) CODEC(ZSTD(1)),
    `ServiceName` String CODEC(ZSTD(1)),
    `AttrFingerprint` String CODEC(ZSTD(1)),
    `Comparator` LowCardinality(String) CODEC(ZSTD(1)),
    `WarningThreshold` Float64 CODEC(ZSTD(1)),
    `CriticalThreshold` Float64 CODEC(ZSTD(1)),
    `SecondarySignalSource` LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    `SecondarySignalName` LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    `SecondaryComparator` LowCardinality(String) DEFAULT 'gt' CODEC(ZSTD(1)),
    `SecondaryWarningThreshold` Float64 DEFAULT 0 CODEC(ZSTD(1)),
    `SecondaryCriticalThreshold` Float64 DEFAULT 0 CODEC(ZSTD(1)),
    `MinSampleCount` UInt32 DEFAULT 1 CODEC(T64, ZSTD(1)),
    `SeasonalBucketsJson` String DEFAULT '' CODEC(ZSTD(1)),
    `IsDeleted` UInt8 DEFAULT 0 CODEC(T64, ZSTD(1)),
    `Version` UInt64 DEFAULT 0 CODEC(T64, ZSTD(1))
)
ENGINE = ReplacingMergeTree(Version)
ORDER BY (SignalSource, SignalName, ServiceName, AttrFingerprint, Id)
SETTINGS index_granularity = 8192
