ATTACH VIEW _ UUID 'c8572c45-eeea-45f3-a563-b919f67b21e1'
(
    `WindowId` String,
    `SignalTs` DateTime64(9),
    `WindowStart` DateTime64(9),
    `WindowEnd` DateTime64(9),
    `SignalType` LowCardinality(String),
    `SignalRef` String,
    `SignalServiceName` LowCardinality(String),
    `Namespace` LowCardinality(String),
    `NodeName` LowCardinality(String),
    `TimeUnix` DateTime64(9),
    `MetricServiceName` LowCardinality(String),
    `MetricName` LowCardinality(String),
    `MetricDescription` String,
    `MetricUnit` LowCardinality(String),
    `MetricKind` String,
    `Attributes` Map(LowCardinality(String), String),
    `AttrFingerprint` String,
    `Value` Float64,
    `StorageTier` String
)
AS WITH
    metric_points AS
    (
        SELECT
            'gauge' AS MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            toFloat64(Value) AS Value,
            0 AS SourceRank
        FROM default.otel_metrics_gauge
        UNION ALL
        SELECT
            'gauge' AS MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            toFloat64(Value) AS Value,
            1 AS SourceRank
        FROM default.otel_metrics_gauge_pinned
        UNION ALL
        SELECT
            'sum' AS MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            toFloat64(Value) AS Value,
            0 AS SourceRank
        FROM default.otel_metrics_sum
        UNION ALL
        SELECT
            'sum' AS MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            toFloat64(Value) AS Value,
            1 AS SourceRank
        FROM default.otel_metrics_sum_pinned
        UNION ALL
        SELECT
            'histogram' AS MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            if(Count = 0, 0., toFloat64(Sum) / toFloat64(Count)) AS Value,
            0 AS SourceRank
        FROM default.otel_metrics_histogram
        UNION ALL
        SELECT
            'histogram' AS MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            if(Count = 0, 0., toFloat64(Sum) / toFloat64(Count)) AS Value,
            1 AS SourceRank
        FROM default.otel_metrics_histogram_pinned
    ),
    dedup_points AS
    (
        SELECT
            MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint,
            argMin(Value, SourceRank) AS Value,
            min(SourceRank) AS StorageRank
        FROM
        metric_points
        GROUP BY
            MetricKind,
            TimeUnix,
            ServiceName,
            MetricName,
            MetricDescription,
            MetricUnit,
            Attributes,
            AttrFingerprint
    )
SELECT
    w.Id AS WindowId,
    w.SignalTs,
    w.WindowStart,
    w.WindowEnd,
    w.SignalType,
    w.SignalRef,
    w.ServiceName AS SignalServiceName,
    w.Namespace,
    w.NodeName,
    m.TimeUnix,
    m.ServiceName AS MetricServiceName,
    m.MetricName,
    m.MetricDescription,
    m.MetricUnit,
    m.MetricKind,
    m.Attributes,
    m.AttrFingerprint,
    m.Value,
    multiIf(m.StorageRank = 0, 'raw', m.StorageRank = 1, 'pinned', 'mixed') AS StorageTier
FROM default.sobs_raw_windows AS w
INNER JOIN
dedup_points AS m ON (m.TimeUnix >= w.WindowStart) AND (m.TimeUnix <= w.WindowEnd) AND ((w.ServiceName = '') OR (m.ServiceName = w.ServiceName)) AND ((w.Namespace = '') OR ((m.Attributes['k8s.namespace.name']) = w.Namespace) OR ((m.Attributes['namespace']) = w.Namespace)) AND ((w.NodeName = '') OR ((m.Attributes['k8s.node.name']) = w.NodeName) OR ((m.Attributes['node']) = w.NodeName))
