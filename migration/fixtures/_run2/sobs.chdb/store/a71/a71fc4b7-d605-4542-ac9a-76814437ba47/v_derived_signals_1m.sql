ATTACH VIEW _ UUID '2bbdbb9a-ebe8-452c-b9a9-4e7549fda6ff'
(
    `ServiceName` LowCardinality(String),
    `SignalSource` String,
    `SignalName` String,
    `AttrFingerprint` LowCardinality(String),
    `MinuteBucket` DateTime,
    `Value` Float64,
    `SampleCount` UInt64
)
AS SELECT
    ServiceName,
    'logs' AS SignalSource,
    'log_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'log_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(count()) AS Value,
    count() AS SampleCount
FROM default.otel_logs
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'logs' AS SignalSource,
    'error_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'error_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(countIf(SeverityText IN ('ERROR', 'FATAL', 'CRITICAL'))) AS Value,
    count() AS SampleCount
FROM default.otel_logs
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'logs' AS SignalSource,
    'error_ratio' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'error_ratio')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    if(count() > 0, toFloat64(countIf((SeverityText IN ('ERROR', 'FATAL', 'CRITICAL')))) / count(), 0.) AS Value,
    count() AS SampleCount
FROM default.otel_logs
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'traces' AS SignalSource,
    'trace_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'trace_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(count()) AS Value,
    count() AS SampleCount
FROM default.otel_traces
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'traces' AS SignalSource,
    'trace_error_ratio' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'trace_error_ratio')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    if(count() > 0, toFloat64(countIf(StatusCode = 'STATUS_CODE_ERROR')) / count(), 0.) AS Value,
    count() AS SampleCount
FROM default.otel_traces
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'traces' AS SignalSource,
    'latency_p95_ms' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'latency_p95_ms')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantile(0.95)(Duration)) / 1000000. AS Value,
    count() AS SampleCount
FROM default.otel_traces
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'errors' AS SignalSource,
    'exception_volume' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|', 'exception_volume')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(count()) AS Value,
    count() AS SampleCount
FROM default.otel_logs
WHERE EventName = 'exception'
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'rum_vitals' AS SignalSource,
    'LCP' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|LCP')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
    count() AS SampleCount
FROM default.hyperdx_sessions
WHERE (EventName = 'web-vital') AND (JSONExtractString(Body, 'name') = 'LCP')
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'rum_vitals' AS SignalSource,
    'INP' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|INP')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
    count() AS SampleCount
FROM default.hyperdx_sessions
WHERE (EventName = 'web-vital') AND (JSONExtractString(Body, 'name') = 'INP')
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'rum_vitals' AS SignalSource,
    'CLS' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|CLS')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
    count() AS SampleCount
FROM default.hyperdx_sessions
WHERE (EventName = 'web-vital') AND (JSONExtractString(Body, 'name') = 'CLS')
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'rum_vitals' AS SignalSource,
    'TTFB' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|TTFB')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
    count() AS SampleCount
FROM default.hyperdx_sessions
WHERE (EventName = 'web-vital') AND (JSONExtractString(Body, 'name') = 'TTFB')
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'rum_vitals' AS SignalSource,
    'FCP' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|FCP')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
    count() AS SampleCount
FROM default.hyperdx_sessions
WHERE (EventName = 'web-vital') AND (JSONExtractString(Body, 'name') = 'FCP')
GROUP BY
    ServiceName,
    MinuteBucket
UNION ALL
SELECT
    ServiceName,
    'rum_vitals' AS SignalSource,
    'FID' AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName, '|rum_vitals|FID')))), 1, 16) AS AttrFingerprint,
    toStartOfMinute(Timestamp) AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body, 'value'))) AS Value,
    count() AS SampleCount
FROM default.hyperdx_sessions
WHERE (EventName = 'web-vital') AND (JSONExtractString(Body, 'name') = 'FID')
GROUP BY
    ServiceName,
    MinuteBucket
