# chDB Memory Optimization for Low-Memory Kubernetes Deployments

## Background

Sobs embeds chDB (ClickHouse in-process) as its storage engine. ClickHouse defaults are tuned
for dedicated servers with tens of gigabytes of RAM. When running in a container on Kubernetes
with a 256 MB memory limit, those defaults cause OOM kills. This document records the audit
findings, root causes, and concrete fixes.

**Target profiles:**

| Profile | Memory limit | Notes |
|---|---|---|
| Minimal (k8s sidecar / edge) | ~256 MB | Recommended: `SOBS_CHDB_MAX_SERVER_MB=200` |
| Standard (small cluster) | ~1 GB | Recommended: `SOBS_CHDB_MAX_SERVER_MB=800` |

---

## Root Causes

### 1. `mark_cache_size` defaults to 5 GB

ClickHouse maintains an in-process cache of MergeTree index marks. The default is **5 GB**,
allocated at startup regardless of dataset size. On a container with a 256 MB limit this alone
causes an immediate OOM before any query runs.

Verified by querying `system.server_settings`:

```
mark_cache_size                  = 5368709120   (5 GB)
max_server_memory_usage          = ~90% of host RAM
```

### 2. `max_threads` defaults to `auto` (10 threads observed)

ClickHouse allocates per-thread read buffers and worker stacks. At 10 threads, every query
multiplies its intermediate memory usage 10×. Even simple GROUP BY queries can spike to
hundreds of MB.

### 3. External spill disabled

`max_bytes_before_external_group_by` and `max_bytes_before_external_sort` both default to `0`
(disabled). ClickHouse will hold the entire intermediate GROUP BY or ORDER BY result set in RAM.
When it exceeds available memory, the process is OOM-killed rather than spilling to disk.

### 4. Unbounded view scans in visual paths

`v_derived_signals_1m` is a runtime view with a 12-way UNION ALL scanning `otel_logs`,
`otel_traces`, `hyperdx_sessions`, `otel_metrics_gauge/sum/histogram` — **all rows, all time**.
`v_derived_signals_anomaly` wraps that with a sliding window function, materialising the entire
aggregated dataset in memory before applying any WHERE filter.

Every metrics/anomaly page load triggers this full scan.

### 5. `ERROR_SOURCES_SQL` on the summary page is unbounded

The home page runs:
```python
db.execute(f"SELECT * FROM ({ERROR_SOURCES_SQL}) ORDER BY Timestamp DESC").fetchall()
```
This scans and sorts **all errors ever recorded** from two tables into Python memory on every
page load.

### 6. `COUNT(DISTINCT TraceId)` holds a full hash set in RAM

The traces page runs `COUNT(DISTINCT TraceId)` which ClickHouse implements as an exact hash set.
For millions of traces this is a significant per-query allocation.

### 7. Write queue holds large in-flight payloads

`WRITE_QUEUE_MAX=5000` with `WRITE_BATCH_MAX=200` means up to 5000 Python closures, each
capturing a JSON payload, can reside in the queue simultaneously under burst ingest.

---

## Fix 1 — chDB/ClickHouse Configuration (P0)

**Testing confirmed** that URL query parameters passed to `chdb_driver.connect()` set server-level
settings (equivalent to `config.xml`). Session-level settings can be applied with `SET` after
connection.

### 1a. Server-level: mark cache and server memory cap

These must be set at connection time. Modify `_build_chdb_connect_target` in `app.py`:

```python
def _build_chdb_connect_target(path: str) -> str:
    """Build chDB connect target, optionally adding startup args via query params."""
    config_file = os.environ.get(CHDB_CONFIG_FILE_ENV, "").strip()
    if config_file:
        # User-supplied config.xml — they own all memory settings
        if not os.path.isabs(config_file):
            raise RuntimeError(
                f"{CHDB_CONFIG_FILE_ENV} must be an absolute path to a mounted ClickHouse config.xml"
            )
        encoded = urllib.parse.quote(config_file, safe="/")
        return f"{path}?config-file={encoded}"

    # Apply sane low-memory defaults; override via env vars for larger deployments
    max_server_mb = int(os.environ.get("SOBS_CHDB_MAX_SERVER_MB", "512"))
    mark_cache_mb = int(os.environ.get("SOBS_CHDB_MARK_CACHE_MB", "32"))
    params = urllib.parse.urlencode({
        "max_server_memory_usage": max_server_mb * 1024 * 1024,
        "mark_cache_size": mark_cache_mb * 1024 * 1024,
    })
    return f"{path}?{params}"
```

### 1c. Important connect-target format caveat (directory-backed chDB)

  During local validation, a runtime-specific path behavior was observed:

  - `file:/.../data/sobs.chdb?...` opened a different logical DB state for a directory-backed store
  - `/.../data/sobs.chdb?...` opened the expected existing dataset and still applied server settings

  For SOBS with `sobs.chdb` as a directory, the connect target should use the plain directory path
  with query parameters (no `file:` scheme prefix).

| Env var | 256 MB profile | 1 GB profile | Description |
|---|---|---|---|
| `SOBS_CHDB_MAX_SERVER_MB` | `200` | `800` | Hard cap on ClickHouse server memory |
| `SOBS_CHDB_MARK_CACHE_MB` | `16` | `64` | MergeTree mark cache (replaces 5 GB default) |

### 1b. Session-level: threads and disk spill

Apply immediately after connection in `ChDbConnection.__init__`, before the connection is
used for any query. Add after `self._conn = chdb_driver.connect(connect_target)`:

```python
max_threads = int(os.environ.get("SOBS_CHDB_MAX_THREADS", "2"))
spill_group_by_mb = int(os.environ.get("SOBS_CHDB_SPILL_GROUP_BY_MB", "50"))
spill_sort_mb = int(os.environ.get("SOBS_CHDB_SPILL_SORT_MB", "50"))
cur = self._conn.cursor()
cur.execute(f"SET max_threads = {max_threads}")
cur.execute(f"SET max_bytes_before_external_group_by = {spill_group_by_mb * 1024 * 1024}")
cur.execute(f"SET max_bytes_before_external_sort = {spill_sort_mb * 1024 * 1024}")
```

| Env var | Default | Purpose |
|---|---|---|
| `SOBS_CHDB_MAX_THREADS` | `2` | Query parallelism; 2 is safe for 256 MB, 4 for 1 GB |
| `SOBS_CHDB_SPILL_GROUP_BY_MB` | `50` | Spill GROUP BY to disk above this threshold instead of OOMing |
| `SOBS_CHDB_SPILL_SORT_MB` | `50` | Spill ORDER BY to disk above this threshold instead of OOMing |

> **Important:** disk spill makes visual-path queries slower (acceptable per requirements)
> but prevents OOM kills. Ingest writes are not affected because they use INSERT, not GROUP BY /
> ORDER BY aggregations.

---

## Fix 2 — Add Time Bounds to Anomaly View Queries (P1)

`v_derived_signals_anomaly` and `v_otel_metrics_anomaly` are window-function views over
`v_derived_signals_1m` / `v_otel_metrics_1m`, which themselves are runtime UNIONs over raw
tables. ClickHouse can push a `time >=` predicate through the view back into the source table
partition pruning — but only if the predicate is present.

**Rule:** Every query against `v_derived_signals_anomaly` or `v_otel_metrics_anomaly` that
does not already have user-supplied time bounds should add:

```sql
AND time >= now() - INTERVAL 24 HOUR
```

The sliding window in the anomaly view is 60 minutes (`ROWS BETWEEN 59 PRECEDING AND CURRENT ROW`).
24 hours is sufficient to render meaningful charts and compute accurate baselines while limiting
the scan to roughly 1440 rows per signal series rather than unbounded history.

Apply this to all call sites in `app.py` that query these views without an explicit time bound,
including:
- `_list_derived_signal_dimensions` (lines ~10225–10233)
- Metrics index page grouped query (line ~11402)
- Anomaly rule evaluation loops (lines ~5690, 6238–6328)
- AI/trace context queries against `v_derived_signals_anomaly` (line ~13056)

---

## Fix 3 — Bound `ERROR_SOURCES_SQL` on Summary Page (P1)

Current code (summary route, line ~9578):
```python
for row in db.execute(f"SELECT * FROM ({ERROR_SOURCES_SQL}) ORDER BY Timestamp DESC").fetchall():
```

This is an unbounded scan + sort + full Python materialisation. With millions of error events
it will OOM in ClickHouse (sort) and again in Python (`.fetchall()`).

**Fix:** Add time bound and hard LIMIT:

```python
rows = db.execute(
    f"SELECT * FROM ({ERROR_SOURCES_SQL})"
    " WHERE Timestamp >= now() - INTERVAL 48 HOUR"
    " ORDER BY Timestamp DESC"
    " LIMIT 500"
).fetchall()
```

The summary page only shows recent errors. 48 hours + 500 rows captures any realistic incident
window while keeping memory bounded.

Apply the same pattern to all other `ERROR_SOURCES_SQL` call sites (`errors` page, AI context
queries) — each already has its own `WHERE`/`LIMIT` logic but should be audited to confirm
time bounds are always present.

---

## Fix 4 — Replace `v_derived_signals_1m` DISTINCT Dimension Queries (P2)

`_list_derived_signal_dimensions` (line ~10225) issues three separate full-view scans just to
enumerate dimension values for filter dropdowns:

```python
db.execute("SELECT DISTINCT ServiceName FROM v_derived_signals_1m ORDER BY ServiceName")
db.execute("SELECT DISTINCT SignalName FROM v_derived_signals_1m ORDER BY SignalName")
db.execute("SELECT DISTINCT SignalSource FROM v_derived_signals_1m ORDER BY SignalSource")
```

**Fix for `ServiceName`:** Query raw tables with `LowCardinality` columns instead — these use
the low-cardinality dictionary and avoid a full scan:

```python
db.execute(
    "SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName != ''"
    " UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != ''"
    " UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName != ''"
    " ORDER BY ServiceName"
)
```

**Fix for `SignalName` and `SignalSource`:** These are a static enumeration defined by the view's
UNION branches. They will never contain values beyond what the code defines. Return them as
constants from Python without any DB query:

```python
_KNOWN_SIGNAL_NAMES = [
    "log_volume", "error_volume", "error_ratio",
    "trace_volume", "trace_error_ratio", "latency_p95_ms",
    "exception_volume", "LCP", "FID", "CLS", "INP", "TTFB",
    "FCP",
]
_KNOWN_SIGNAL_SOURCES = ["errors", "logs", "rum_vitals", "traces"]
```

---

## Fix 5 — Replace `COUNT(DISTINCT TraceId)` with `uniq()` (P2)

Line ~15381:
```python
total = db.execute(
    f"SELECT COUNT(DISTINCT TraceId) FROM otel_traces {trace_where}", params
).fetchone()[0]
```

`COUNT(DISTINCT)` in ClickHouse uses an exact hash set proportional to the number of distinct
values. Replace with the HyperLogLog-based approximate function:

```sql
SELECT uniq(TraceId) FROM otel_traces WHERE ...
```

`uniq()` uses ~2.5 KB of memory per call regardless of cardinality and is accurate to ~2.2%.
For pagination totals shown in the UI this is indistinguishable from an exact count.

---

## Fix 6 — Cache Summary Page Stats (P2)

The summary route (line ~9585) runs five COUNT queries on every page load:

```python
"logs":  db.execute("SELECT COUNT(*) FROM otel_logs").fetchone()[0],
"spans": db.execute("SELECT COUNT(*) FROM otel_traces").fetchone()[0],
"rum":   db.execute("SELECT COUNT(*) FROM hyperdx_sessions").fetchone()[0],
"ai":    db.execute("SELECT COUNT(*) FROM otel_traces WHERE ...").fetchone()[0],
```

While `COUNT(*)` on MergeTree tables without a WHERE clause reads from part metadata and is
fast, `COUNT(*) WHERE <AI_SPAN_CONDITION>` still scans spans. Under concurrent page loads this
generates N×5 simultaneous queries against chDB, which holds a single global lock in the
`ChDbConnection` wrapper.

Wrap these in a short TTL cache (60s) using the same pattern already used for
`_errors_services_cache`:

```python
_summary_stats_cache: dict[str, Any] = {"expires_at": 0.0, "stats": {}}
_summary_stats_cache_lock = threading.Lock()
SUMMARY_STATS_CACHE_TTL_SEC = int(os.environ.get("SOBS_SUMMARY_STATS_CACHE_TTL_SEC", "60"))
```

---

## Fix 7 — Reduce Write Queue Size for 256 MB Profile (P3)

The default `WRITE_QUEUE_MAX=5000` with `WRITE_BATCH_MAX=200` allows up to 5000 Python closures
(each capturing a JSON-serialised batch) to queue in memory during ingest bursts. On the 256 MB
profile this can consume 20–50 MB of Python heap under load.

For the 256 MB k8s deployment, set:
```yaml
- name: SOBS_WRITE_QUEUE_MAX
  value: "500"
- name: SOBS_WRITE_BATCH_MAX
  value: "50"
```

Ingest will backpressure earlier (clients receive a 503 instead of queuing indefinitely) which
is the correct behaviour for a memory-constrained container.

---

## Long-Term: Materialise the Derived Signals Views (P4)

The highest-leverage architectural change is converting `v_derived_signals_1m` and
`v_otel_metrics_1m` from runtime views to **Materialized Views backed by AggregatingMergeTree**.

Instead of scanning all raw rows at query time, each new row inserted into `otel_logs`,
`otel_traces`, etc. incrementally updates the aggregation state. Visual queries then read from
the pre-aggregated table which is proportional to `(services × signals × time_buckets)` not
`(total raw rows)`.

This eliminates the dominant memory consumer in the visual path entirely and is the recommended
approach once the P0–P2 fixes stabilise the container.

---

## Local Profiling Results (April 8, 2026)

Profile run: local SOBS + ajba tenant snapshot, interactive usage for ~207 seconds.

- Samples: 195 (1 second interval)
- RSS MB: avg 599.70, p50 521.17, p95 850.02, peak 863.25, min 424.19
- CPU %: avg 21.57, p50 0.0, p95 196.48, peak 200.3

Interpretation:

- Memory remains well above the 256 MB target under interactive usage.
- Workload is bursty: mostly idle between requests, with short heavy multi-core spikes on demand.
- This is consistent with expensive visual-path query bursts (aggregation/windowed scans).

Immediate follow-up profiling plan:

1. Correlate resource spikes with specific endpoints/pages by capturing request logs and resource
   CSV in the same run.
2. Focus reproduction on detailed trace metric context and metrics/anomaly pages.
3. Rank top endpoint/query contributors by peak RSS deltas and apply targeted SQL rewrites.

Related regression note:

- Detailed trace metric sparklines (Kubernetes metrics group) regressed due to timestamp parsing
  mode mismatch.
- Fix applied in `app.py`: sparkline timeseries query now prefers `parseDateTime64BestEffort(..., 'UTC')`
  and only falls back to default parser when UTC mode returns no rows.

---

## Recommended Kubernetes Deployment Env Vars

### 256 MB profile

```yaml
env:
  - name: SOBS_CHDB_MAX_SERVER_MB
    value: "200"
  - name: SOBS_CHDB_MARK_CACHE_MB
    value: "16"
  - name: SOBS_CHDB_MAX_THREADS
    value: "2"
  - name: SOBS_CHDB_SPILL_GROUP_BY_MB
    value: "32"
  - name: SOBS_CHDB_SPILL_SORT_MB
    value: "32"
  - name: SOBS_WRITE_QUEUE_MAX
    value: "500"
  - name: SOBS_WRITE_BATCH_MAX
    value: "50"
```

### 1 GB profile (millions of rows)

```yaml
env:
  - name: SOBS_CHDB_MAX_SERVER_MB
    value: "800"
  - name: SOBS_CHDB_MARK_CACHE_MB
    value: "64"
  - name: SOBS_CHDB_MAX_THREADS
    value: "4"
  - name: SOBS_CHDB_SPILL_GROUP_BY_MB
    value: "150"
  - name: SOBS_CHDB_SPILL_SORT_MB
    value: "150"
```

---

## Prioritised Implementation Order

| Priority | Change | Memory impact | Ingest path affected? |
|---|---|---|---|
| **P0** | `mark_cache_size` + `max_server_memory_usage` via URL params | Very high (saves ~5 GB default) | No |
| **P0** | Session `SET max_threads=2` on connect | High | No |
| **P1** | Session `SET max_bytes_before_external_group_by/sort` | High (prevents OOM on heavy queries) | No |
| **P1** | Add `time >= now() - INTERVAL 24 HOUR` to all anomaly view queries | High | No |
| **P1** | Add time bound + LIMIT to `ERROR_SOURCES_SQL` on summary page | Medium-High | No |
| **P2** | Replace DISTINCT dimension queries with raw table / constants | Medium | No |
| **P2** | Cache summary COUNT(*) stats (60s TTL) | Small | No |
| **P2** | `COUNT(DISTINCT TraceId)` → `uniq(TraceId)` | Medium | No |
| **P3** | Reduce `WRITE_QUEUE_MAX` / `WRITE_BATCH_MAX` for 256 MB profile | Small | Watch backpressure |
| **P4** | Materialise `v_derived_signals_1m` as AggregatingMergeTree | Very high (eliminates full scans) | Incremental ingest cost |
