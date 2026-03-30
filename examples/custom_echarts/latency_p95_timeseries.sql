-- IMPORTANT: keep explicit ORDER BY for deterministic chart point order.
-- Add tie-breakers when needed (example: ORDER BY ts, service).
SELECT
  toStartOfMinute(Timestamp) AS ts,
  quantile(0.95)(Duration) AS p95_ms
FROM otel_traces
WHERE Timestamp >= now() - INTERVAL 2 HOUR
GROUP BY ts
ORDER BY ts
LIMIT 240;
