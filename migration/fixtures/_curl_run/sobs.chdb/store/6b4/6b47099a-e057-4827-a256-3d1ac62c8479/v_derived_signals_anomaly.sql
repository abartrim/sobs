ATTACH VIEW _ UUID 'aeb67bcd-10f5-40d2-bd38-3130d5aa6574'
(
    `ServiceName` LowCardinality(String),
    `SignalSource` String,
    `SignalName` String,
    `AttrFingerprint` LowCardinality(String),
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
    SignalSource,
    SignalName,
    AttrFingerprint,
    MinuteBucket AS time,
    Value AS value,
    SampleCount,
    round(avg(Value) OVER w, 6) AS baseline_mean,
    round(sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))), 6) AS baseline_stddev,
    round(avg(Value) OVER w - (2. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)))), 6) AS baseline_lower,
    round(avg(Value) OVER w + (2. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w)))), 6) AS baseline_upper,
    round(if(sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))) > 0, abs(Value - avg(Value) OVER w) / sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))), 0), 4) AS anomaly_score,
    multiIf((sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))) > 0) AND (abs(Value - avg(Value) OVER w) > (3. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))))), 'outlier', (sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))) > 0) AND (abs(Value - avg(Value) OVER w) > (2. * sqrt(greatest(0., avg(Value * Value) OVER w - (avg(Value) OVER w * avg(Value) OVER w))))), 'warning', 'normal') AS anomaly_state
FROM default.v_derived_signals_1m
WINDOW w AS (PARTITION BY ServiceName, SignalSource, SignalName, AttrFingerprint ORDER BY MinuteBucket ASC ROWS BETWEEN 59 PRECEDING AND CURRENT ROW)
