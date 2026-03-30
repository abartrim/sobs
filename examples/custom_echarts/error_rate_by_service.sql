-- IMPORTANT: keep explicit ORDER BY for deterministic bar ranking.
-- Add tie-breakers when needed (example: ORDER BY error_rate DESC, service).
SELECT
  ServiceName AS service,
  round(100.0 * countIf(StatusCode = 'STATUS_CODE_ERROR') / greatest(count(), 1), 2) AS error_rate
FROM otel_traces
WHERE Timestamp >= now() - INTERVAL 1 HOUR
GROUP BY service
ORDER BY error_rate DESC
LIMIT 20;
