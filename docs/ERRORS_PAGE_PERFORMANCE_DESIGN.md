# Errors Page Performance & Memory Optimization

## Overview

The Errors page is the most query-intensive page in sobs. In its grouped mode it runs two
nested query passes over raw observability data, performs heavy string normalization and
grouping inside ClickHouse, and can re-execute the same expensive probe query a third time
to fan out trace IDs. This document captures the root-cause audit, external best-practice
research, and a prioritized set of options to discuss before implementation.

**Goal:** reduce peak memory usage and latency for `/errors` while preserving all existing
UI behavior (grouped/individual modes, resolved filtering, trace links, regex filter, first/last
seen, pagination).

**Constraint:** no changes were made during this research phase.

---

## Current Architecture

### Data sources

`ERROR_SOURCES_SQL` (`app.py` L9756) is a UNION ALL across two tables:

```sql
SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes
FROM otel_logs
WHERE EventName = 'exception'
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
UNION ALL
SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes
FROM hyperdx_sessions
WHERE EventName IN ('error', 'unhandledrejection', 'exception')
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
```

Every Errors page query — count, page, trace fanout, service list — wraps this UNION as a
subquery. This means every request re-scans both `otel_logs` and `hyperdx_sessions` from
scratch, including their `LogAttributes` map columns.

### Grouped mode query flow (`app.py` L13508)

The grouped path has three distinct database round-trips, all wrapping the same probe:

1. **Count query** — `SELECT COUNT(*) FROM (grouped_aggregate_sql)` where
   `grouped_aggregate_sql` itself wraps `grouped_probe_sql` (the full UNION filtered scan with a
   `LIMIT probe_limit` — up to 10 000 raw rows).
2. **Page query** — same `grouped_aggregate_sql` + `ORDER BY … LIMIT page OFFSET offset`.
3. **Trace fanout** — `grouped_probe_sql` executed a *third* time (`app.py` L13577) to collect
   all distinct trace IDs per visible group.

Each round-trip:
- Filters and deduplicates up to 10 000 raw rows.
- Applies `replaceRegexpAll(lower(…), '\\s+', ' ')` on service, exception type, and exception
  message to produce stable group keys.
- Builds MD5-based error IDs involving `Timestamp | ServiceName | type | message | TraceId | SpanId`.
- Carries full `LogAttributes` map through every layer — a wide, expensive-to-deserialize
  column.

### Non-grouped resolved filter flow (`app.py` L13620)

When `resolved=0` or `resolved=1` is set in non-grouped mode, the code iterates
in `scan_batch=200` row pages, calling `_build_error_item` on every row, checking
`item["id"] in resolved_ids` in Python, and accumulating matches until the page is filled.
For large datasets this can scan the entire history.

### Existing mitigations (`app.py` L1854)

Session-level settings applied at connection time:

| Setting | Default | Effect |
|---|---|---|
| `max_threads` | 1 via `SOBS_CHDB_MAX_THREADS` | Limits parallelism |
| `max_bytes_before_external_group_by` | 50 MB via `SOBS_CHDB_SPILL_GROUP_BY_MB` | Spills GROUP BY to disk above threshold |
| `max_bytes_before_external_sort` | 50 MB via `SOBS_CHDB_SPILL_SORT_MB` | Spills ORDER BY to disk above threshold |

These prevent OOM but do not reduce the amount of work done. Disk spill can make already-slow
queries take roughly 3× longer (documented in ClickHouse GROUP BY external memory docs).

---

## Hotspot Summary

| # | Hotspot | Location | Impact |
|---|---|---|---|
| H1 | Probe re-executed 3× per grouped request | `app.py` L13545, L13549, L13577 | High: 3× I/O + compute |
| H2 | Full `LogAttributes` map carried through all layers | `ERROR_SOURCES_SQL` | High: expensive column deserialization |
| H3 | `replaceRegexpAll(lower(…))` on every probe row | `app.py` L13511–13521 | Medium: per-row string ops on up to 10 000 rows |
| H4 | `match(Body, pattern)` regex on broad scan | `app.py` L13476 | Medium–High: RE2 on unindexed text column |
| H5 | Python-side resolved filter loop | `app.py` L13620–13636 | High for large resolved=0/1 pages |
| H6 | No default time bound enforced | `view_errors()` | High: unbounded scans possible |
| H7 | Service list re-queried against UNION | `app.py` L13643 | Low but redundant; has in-process cache |

---

## Research: ClickHouse/chDB Best Practices

### PREWHERE
ClickHouse moves selective predicates to PREWHERE automatically when
`optimize_move_to_prewhere = 1` (default on). PREWHERE reads only the filter column before
deciding which granules to load for the rest of the SELECT. This reduces I/O and memory
for wide tables like `otel_logs` when the predicate is selective. The current `ErrorSources`
predicates (SeverityNumber, EventName, LogAttributes key) are already candidates. You can
verify which predicates move with `EXPLAIN` and check actual granule reads in query logs.

Reference: https://clickhouse.com/docs/sql-reference/statements/select/prewhere

### GROUP BY in External Memory
When `max_bytes_before_external_group_by` is set, ClickHouse spills GROUP BY hash state to
disk once it exceeds the threshold. The ClickHouse docs recommend setting it to roughly the
same value as `max_memory_usage` and then doubling `max_memory_usage`, because the merge phase
after spill can consume the same amount of memory as the build phase. When spill occurs, query
time increases approximately 3× compared to the all-in-memory path. The current 50 MB threshold
is conservative; on larger datasets this will trigger spill frequently.

Reference: https://clickhouse.com/docs/sql-reference/statements/select/group-by#group-by-in-external-memory

### Incremental Materialized Views
A materialized view (MV) fires on INSERT and writes aggregate states to a target table using
`AggregatingMergeTree`. Queries then read from the pre-aggregated target instead of raw data.
For a UNION ALL source, you need one MV per UNION branch. The main tradeoffs:
- Query speed improves substantially (pre-grouped at ingest).
- Memory at query time drops because aggregate states are already computed.
- Write throughput decreases slightly due to MV firing on each INSERT batch.
- Historical data requires a backfill (`INSERT INTO mv_target SELECT … FROM source`).
- The schema must be versioned and migration-managed.
- Query must align grouping keys and order with what the MV computes.

Reference: https://clickhouse.com/docs/materialized-view/incremental-materialized-view

### AggregatingMergeTree
Stores aggregate function states (e.g. `countState`, `minState`, `maxState`, `argMaxState`)
rather than raw rows. On merge, ClickHouse combines states automatically. Queries use `-Merge`
combinator: `countMerge(count_state)`. This is the companion engine to incremental MVs for
pre-aggregated data.

Reference: https://clickhouse.com/docs/engines/table-engines/mergetree-family/aggregatingmergetree

### Projections
A projection is a hidden synchronized table embedded in the source table, maintained
automatically on INSERT, and selected by the query optimizer when the predicate/order matches.
Unlike MVs, projections cannot express a UNION source. Benefits over MVs:
- No separate DDL/backfill: a projection backfills on creation (with `MATERIALIZE PROJECTION`).
- Automatic query routing: no app code change needed.
Limitations:
- Cannot express cross-table JOINs or UNION sources.
- Less flexible than MVs for complex aggregations.
- Storage and write overhead.

Reference: https://clickhouse.com/docs/data-modeling/projections

### Data-Skipping Indexes
ClickHouse supports `minmax`, `set`, `bloom_filter`, and `tokenbf_v1` skip indexes on columns.
These allow the engine to skip granules where no rows satisfy a predicate. They are effective
only when the indexed column has good data locality (values are clustered, not random). For
observability data:
- `set(N)` or `minmax` on `SeverityNumber` / `EventName` can help if rows with the same
  severity are co-located (which depends on ORDER BY key).
- `tokenbf_v1` on `Body` can accelerate text-contains queries but adds index write cost and
  may have limited benefit if the log volume is noisy.
- Must be tested against real data distributions before deploying.

Reference: https://clickhouse.com/docs/optimize/skipping-indexes

### Custom Partitioning Key
The ClickHouse docs recommend coarse time-based partitioning (month or week for observability).
Fine-grained partitioning (e.g. by day with many services) multiplies the number of parts,
increases metadata overhead, and hurts merge throughput — it does not speed up queries unless
the partition key exactly matches a filter. The main benefit of a time partition key for errors
is that old partitions can be DROPped quickly during retention enforcement.

Reference: https://clickhouse.com/docs/engines/table-engines/mergetree-family/custom-partitioning-key

### Sparse Primary Index Design
The ORDER BY / primary key controls how data is physically sorted and indexed at the granule
level. For the Errors page:
- Dominant filter is time range → `Timestamp` must be leftmost in ORDER BY.
- Secondary filter is `ServiceName` → placing it second helps for per-service queries.
- Tertiary predicates (`SeverityNumber`, `EventName`) see diminishing benefit at deeper
  positions.
- Multiple physical orderings (e.g. also ordering by `ServiceName, Timestamp`) require either
  a second physical table (a MV into a separate MergeTree) or a projection.

Reference: https://clickhouse.com/docs/guides/best-practices/sparse-primary-indexes

### Query Cache
ClickHouse's server-side query cache returns cached SELECT results for the exact same query
AST within a TTL (default 60 s). For the Errors page this is useful when:
- Multiple users load the same Errors view within the cache window.
- A user reloads the page without changing filters.

Limitations:
- Cache is per-user by default (security isolation); cannot share across users without
  explicit opt-in.
- Non-deterministic functions (`now()`, `today()`) prevent caching unless overridden.
- Disabling caching of stale results requires TTL tuning.
- `use_query_cache = true` must be set at query level explicitly.

Reference: https://clickhouse.com/docs/operations/query-cache

### Query Condition Cache
Unlike the query cache (which caches full results), the query condition cache stores a single
bit per granule per filter condition: whether any row in that granule matches the predicate.
On subsequent runs with the same filter, matching granules can be skipped without reading data.
Effective when:
- The same predicate is evaluated repeatedly over largely immutable data (true for historical
  errors).
- The filter is highly selective (few rows match → many granules skipped).
- `use_query_condition_cache = true` is enabled at session or query level.

Reference: https://clickhouse.com/docs/operations/query-condition-cache

---

## Options Matrix

### Option 1 — Time-Bound Defaults + Query Shape Fixes (Phase 1, Low Risk)

**What:** Enforce a maximum default scan window (e.g. 7 days) if `from_ts`/`to_ts` are
unset. Push all WHERE conditions as deep as possible into `ERROR_SOURCES_SQL` subqueries.
Remove the third duplicate probe execution for trace fanout by reusing the in-aggregation
`groupUniqArray(64)(TraceId)` values that are already computed in `grouped_aggregate_sql`.

**Changes needed (code only, no schema):**
- Add `DEFAULT_ERRORS_LOOKBACK_HOURS` (e.g. 168 h / 7 d) that applies when no explicit
  time range is given to `view_errors`.
- Remove the second `grouped_probe_sql` execution for trace fanout; use `RepTraceIds` field
  from the aggregate result which already holds up to 64 unique trace IDs (`groupUniqArray`
  is already in `grouped_aggregate_sql`).
- Drop `LogAttributes` from the `grouped_probe_sql` projection until the aggregate layer
  needs it for the representative row (reduces bytes read per probe row).

**Expected gain:** eliminates 1 of 3 probe passes in grouped mode, cuts effective scan range
dramatically for the common case.

**Risk:** low. Behavior-compatible; resolved-ID path and trace links preserved.

---

### Option 2 — SQL-Level Resolved Filter (Phase 1, Low Risk)

**What:** Replace the Python-scan loop for `resolved=0/1` in non-grouped mode with a SQL
predicate pushed into the source query.

`sobs_error_resolutions` already contains `ErrorId` values. The error ID computation is
currently done at Python time via `_build_error_item`, but the same MD5 hash is already
expressed in SQL as `error_id_sql` (L13493) for grouped mode. The same expression can be
used in a subquery predicate for non-grouped mode.

**Changes needed (code only, no schema):**
- Use the same `error_id_sql` expression already defined in `view_errors` grouped path in
  the non-grouped `WHERE` clause: `WHERE error_id_sql [NOT] IN (SELECT ErrorId …)`.
- Remove the scan-batch Python loop.

**Expected gain:** non-grouped resolved/open filter becomes a single-pass SQL query instead
of O(N) Python iteration over all historical errors.

**Risk:** low-medium. Need to verify `error_id_sql` expression matches `_build_error_item`
ID generation exactly; a mismatch would break resolved state display.

---

### Option 3 — Two-Phase Grouped Execution (Phase 2, Medium Risk)

**What:** Split the grouped query path into two distinct phases:

- **Phase A (cheap):** compute only group keys + counts + first/last + representative row
  identifier (a single trace ID or timestamp). Return just the paginated slice.
- **Phase B (targeted):** for the N visible groups, do a targeted lookup to fetch full
  `LogAttributes` and trace IDs only for those groups.

Currently Phase A and Phase B are computed together by running the heavy UNION + aggregate
twice (count + page), then the UNION a third time for traces. With a two-phase approach:
- Phase A scans and aggregates, but projects only lightweight columns.
- Phase B re-queries only for the ≤100 representative rows by key.

**Changes needed:** restructure `view_errors` grouped path; two separate SQL queries instead
of three.

**Expected gain:** Phase A processes a narrower projection (no `LogAttributes` transfer);
Phase B is minimal. Overall memory and time reduced significantly.

**Risk:** medium. Requires careful handling of page/sort stability between phases.

---

### Option 4 — Pre-Aggregated Errors Fact Table via Incremental MVs (Phase 2–3, High Impact)

**What:** At INSERT time, maintain an `AggregatingMergeTree` table `sobs_error_groups` keyed
by `(ServiceName, GroupType, GroupMessage, toStartOfHour(Timestamp))`. The MV fires for each
INSERT into `otel_logs` and `hyperdx_sessions` and writes:
- `countState()` per group per hour bucket.
- `minState(Timestamp)`, `maxState(Timestamp)` for first/last seen.
- `argMaxState(TraceId, Timestamp)` for representative trace.
- `argMaxState(LogAttributes, Timestamp)` for representative attributes.

Grouped Errors queries read from `sobs_error_groups` instead of raw tables.

**Changes needed:** DDL for MV + target table; backfill for existing data; updated query path
in `view_errors` grouped mode.

**Expected gain:** query latency drops from O(raw rows) to O(group count); memory drops
proportionally. This is the largest single improvement available.

**Risk:** medium-high. Schema migration, backfill, and versioning required. MVs in chDB are
supported but tested less in embedded mode. Must handle UNION ALL by creating two MVs (one
per source table).

---

### Option 5 — Projections for Alternate Physical Ordering (Phase 3, Situational)

**What:** Add projections to `otel_logs` and `hyperdx_sessions` ordered by
`(ServiceName, Timestamp)` so per-service Errors page loads read a physically co-located
slice rather than scanning all services then filtering.

**Changes needed:** `ALTER TABLE … ADD PROJECTION … MATERIALIZE PROJECTION`.

**Expected gain:** useful when a specific service is selected in the Errors filter and data
volume is large. Less impactful for the "all services" default view.

**Risk:** low after testing. Storage + write overhead. Must verify projection actually engages
with `EXPLAIN`.

---

### Option 6 — Data-Skipping Indexes on Error Predicates (Phase 3, Situational)

**What:** Add skip indexes on `otel_logs` and `hyperdx_sessions` for common error predicates:
- `minmax` or `set(8)` on `SeverityNumber`.
- `set(16)` on `EventName`.
- Optionally `tokenbf_v1` on `Body` for regex/text filter acceleration.

**Changes needed:** `ALTER TABLE … ADD INDEX … TYPE … GRANULARITY … MATERIALIZE INDEX`.

**Expected gain:** can skip many granules for the SeverityNumber ≥ 17 predicate if data is
somewhat correlated by time. The tokenized Body index *may* help regex `match(Body, …)` for
common short patterns, but this must be measured.

**Risk:** low-medium. Must benchmark on real data. Unhelpful if severity distribution is
uniform across granules.

---

### Option 7 — Query Cache + Query Condition Cache (Phase 1, Low Risk, Additive)

**What:** Enable `use_query_cache = true` and `use_query_condition_cache = true` at session
level (or for specific Errors queries). Set a short TTL (30–60 s) appropriate for an errors
dashboard.

**Expected gain:** repeated page loads or multi-user access to the same time window / filter
return cached or granule-pruned results cheaply.

**Risk:** low. Requires TTL tuning to keep results acceptably fresh. The query cache does not
work with `now()` / `today()` in queries unless `query_cache_nondeterministic_function_handling`
is overridden. The query condition cache requires `enable_analyzer = 1` (default on).

---

## Recommended Rollout Order

| Phase | Options | Rationale |
|---|---|---|
| **Phase 0** | Instrument query metrics | Baseline elapsed, rows read, bytes, spill events per Errors endpoint before any change. |
| **Phase 1** | Options 1, 2, 7 | Pure code changes; no schema migration; high confidence, low risk. |
| **Phase 2** | Options 3, 4 | Restructured query logic + optional MV pre-aggregation for grouped path. |
| **Phase 3** | Options 5, 6 | Physical tuning; only if Phase 1–2 leaves a measurable gap. |

---

## Functional Compatibility Checklist

All options must preserve the following behaviors:

| Behavior | Location | Notes |
|---|---|---|
| Grouped "best-effort" deduplication toggle | `templates/errors.html` L21 | Keep both modes. |
| Regex filter on Body | `app.py` L13476 | SQL-level; must remain. |
| First seen / Last seen | `templates/_error_panels.html` L123–124 | Must come from aggregate, not approximate. |
| Trace links (multi-trace) | `templates/_error_panels.html` L203–209 | `trace_ids_csv` must still be populated. |
| Resolved / Open filter | `templates/_error_panels.html` L244–253 | Must work in both grouped and individual modes. |
| Pagination (limit + offset) | `view_errors()` | Stable ordering required across pages. |
| Service filter (multi-select) | `view_errors()` | Pushdown into source query. |

---

## Related Documents

- [CHDB_MEMORY_OPTIMIZATION.md](CHDB_MEMORY_OPTIMIZATION.md) — Session-level memory settings and global mitigations.
- ClickHouse GROUP BY external memory: https://clickhouse.com/docs/sql-reference/statements/select/group-by#group-by-in-external-memory
- ClickHouse incremental MVs: https://clickhouse.com/docs/materialized-view/incremental-materialized-view
- ClickHouse AggregatingMergeTree: https://clickhouse.com/docs/engines/table-engines/mergetree-family/aggregatingmergetree
- ClickHouse projections: https://clickhouse.com/docs/data-modeling/projections
- ClickHouse skipping indexes: https://clickhouse.com/docs/optimize/skipping-indexes
- ClickHouse custom partitioning: https://clickhouse.com/docs/engines/table-engines/mergetree-family/custom-partitioning-key
- ClickHouse sparse primary indexes: https://clickhouse.com/docs/guides/best-practices/sparse-primary-indexes
- ClickHouse query cache: https://clickhouse.com/docs/operations/query-cache
- ClickHouse query condition cache: https://clickhouse.com/docs/operations/query-condition-cache
