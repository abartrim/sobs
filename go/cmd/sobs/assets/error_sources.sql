
SELECT
    Timestamp,
    ServiceName,
    TraceId,
    SpanId,
    toValidUTF8(Body) AS Body,
    mapApply((k, v) -> (toValidUTF8(k), toValidUTF8(v)), LogAttributes) AS LogAttributes
FROM otel_logs
WHERE EventName = 'exception'
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
UNION ALL
SELECT
    Timestamp,
    ServiceName,
    TraceId,
    SpanId,
    toValidUTF8(Body) AS Body,
    mapApply((k, v) -> (toValidUTF8(k), toValidUTF8(v)), LogAttributes) AS LogAttributes
FROM hyperdx_sessions
WHERE EventName IN ('error', 'unhandledrejection', 'exception')
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
