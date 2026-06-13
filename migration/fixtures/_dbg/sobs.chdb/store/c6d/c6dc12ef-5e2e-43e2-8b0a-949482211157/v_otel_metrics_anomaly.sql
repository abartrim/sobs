ATTACH VIEW _ UUID 'f54f3b85-52c9-4375-b27a-aeab86411cd3'
(
    `ServiceName` String,
    `MetricName` String,
    `AttrFingerprint` String,
    `MetricKind` String,
    `time` DateTime,
    `value` Float64,
    `SampleCount` UInt64,
    `baseline_mean` Float64,
    `baseline_stddev` Float64,
    `baseline_lower` Float64,
    `baseline_upper` Float64,
    `anomaly_score` Float64,
    `anomaly_state` String
)
AS SELECT
    ServiceName,
    MetricName,
    AttrFingerprint,
    MetricKind,
    MinuteBucket AS time,
    Value AS value,
    SampleCount,
    round(avg(Value) OVER w, 6) AS baseline_mean,
    round(sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))), 6) AS baseline_stddev,
    round(avg(Value) OVER w - (2. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)))), 6) AS baseline_lower,
    round(avg(Value) OVER w + (2. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)))), 6) AS baseline_upper,
    round(if(sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))) > 0, abs(Value - avg(Value) OVER w) / sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))), 0), 4) AS anomaly_score,
    multiIf((sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))) > 0) AND (abs(Value - avg(Value) OVER w) > (3. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))))), 'outlier', (sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))) > 0) AND (abs(Value - avg(Value) OVER w) > (2. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))))), 'warning', 'normal') AS anomaly_state
FROM default.v_otel_metrics_1m
WINDOW w AS (PARTITION BY ServiceName, MetricName, AttrFingerprint ORDER BY MinuteBucket ASC ROWS BETWEEN 59 PRECEDING AND CURRENT ROW)
