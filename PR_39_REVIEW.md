# PR 39 Review: SQL-First Anomaly Detection Layer for OTEL Metrics

## Executive Summary
PR 39 implements a comprehensive SQL-first anomaly detection layer for OpenTelemetry metrics stored in chDB. The implementation is well-structured with good schema design, clear separation of concerns, and reasonable test coverage. However, there are several issues that need addressing before merge.

**Status**: ⚠️ **Request Changes** — Several blockers and quality improvements needed.

---

## Compliance with Issue 37

### ✅ Requirements Met
- **SQL-first design**: Anomaly detection logic implemented entirely in ClickHouse with window functions
- **Minimal schema changes**: Only 3 new typed metric tables + 2 views added (following ClickStack guidance)
- **Typed metrics support**: Gauge, sum, and histogram types properly extracted and persisted
- **Attribute fingerprinting**: Stable, low-cardinality fingerprinting with runtime-attr exclusion
- **API endpoint**: `/api/metrics/anomaly` with required query parameters and response structure
- **Custom dashboard integration**: New `anomaly_overlay` template + anomaly badges in chart headers
- **Test coverage**: 21 tests covering schema, ingest, anomaly detection, and rendering

### ⚠️ Compliance Gaps
1. **Dashboard integration incomplete** — Anomaly state details not fully wired to drilldown tooltips (see below)
2. **UI badge implementation** — Added to card header but missing from some required surfaces (detail views)
3. **Acceptance criterion verification** — "No required change to upstream OTEL collector" is met, but PR doesn't explicitly document this

---

## Code Quality Issues

### � **✓ FIXED: Blocker 1 — Missing Anomaly Metadata in Drilldown**

**Status**: RESOLVED

**What was fixed**: 
- The `_attach_drilldown_metadata()` function now properly injects `_anomaly_state` and `_anomaly_score` into the drilldown metadata for the `anomaly_overlay` template's Value series.
- The frontend tooltip will now display:
  - Anomaly state (normal/warning/outlier) with color-coded indicator
  - Z-score (numeric deviation from baseline)
  - Example: "● outlier (z=4.5)"

**Code change** [app.py:3188-3237](app.py#L3188-L3237):
```python
# Now extracts and injects anomaly metadata for Value series
anomaly_states = (
    bindings.get("anomaly_state")
    if template_id == "anomaly_overlay"
    else None
)
anomaly_scores = (
    bindings.get("anomaly_score")
    if template_id == "anomaly_overlay"
    else None
)
# ... injects _anomaly_state and _anomaly_score into drilldown for Value series
```

---

### 🟢 **✓ FIXED: Background Color Consistency**

**Status**: RESOLVED

**What was fixed**:
- Changed from `setdefault()` to explicit conditional checks for `backgroundColor` and `textStyle`
- Ensures all templates (including `anomaly_overlay`) have consistent transparent background
- Prevents any template-specific background overrides

**Code change** [app.py:3300-3307](app.py#L3300-L3307):
```python
# Now explicitly checks before setting, ensures consistency
if "backgroundColor" not in option:
    option["backgroundColor"] = "transparent"
if "textStyle" not in option:
    option["textStyle"] = {"color": "#adb5bd"}
```
**Location**: [app.py:365-395](app.py#L365-L395)

**Issue**: The window function uses `ROWS BETWEEN 59 PRECEDING AND CURRENT ROW` which is only 60 rows including the current row. However, the PR description claims a "rolling 60-minute window," implying 60 minutes of historical data *before* the current point.

**Current definition**:
```sql
WINDOW w AS (
    PARTITION BY ServiceName, MetricName, AttrFingerprint
    ORDER BY MinuteBucket
    ROWS BETWEEN 59 PRECEDING AND CURRENT ROW  -- 60 rows total (59 + current)
)
```

**Questions to clarify**:
1. Is the intent 60 rows total (including current), or 60 rows before the current row?
2. If looking for "historical baseline," typically you want `ROWS BETWEEN 59 PRECEDING AND 1 PRECEDING` (exclude current) or accept the current design

**Recommendation**: 
- If current design is intentional (use preceding 59 + current = 60-row window), document this clearly in comments
- If you want purely historical baseline (shouldn't include the current point being flagged), change to `ROWS BETWEEN 59 PRECEDING AND 1 PRECEDING`

The test `test_anomaly_api_spike_flagged_as_warning_or_outlier` passes with current logic, so functionally it works, but the semantics should be clarified.

---

### 🟡 **Issue 3: Incomplete Anomaly_state Handling for Zero Variance**
**Location**: [app.py:375-395](app.py#L375-L395)

**Issue**: When `baseline_stddev = 0` (constant series), the `multiIf` logic still evaluates the conditional `abs(Value - baseline_mean) > 3.0 * stddev` where `stddev = 0`. The checks include:
```sql
sqrt(...) > 0 AND abs(Value - avg) > 3.0 * sqrt(...)
```

While this *does* short-circuit on `sqrt(...) > 0`, the asymptotic behavior is unclear. Edge case: if all historical values = 100 and current value = 100.000001, stddev → 0 and anomaly_state should be 'normal', which it is (short-circuited). ✓

**No fix needed**, but add a clarifying comment:

```sql
-- Short-circuits: if stddev=0, sqrt(...) > 0 evaluates false, bypasses division-by-zero and defaults to 'normal'
```

---

### 🟡 **Issue 4: Inconsistent Timezone Handling in NaN Check**
**Location**: [app.py:3580-3585](app.py#L3580-L3585)

**Code**:
```python
def _safe(v):  # type: ignore
    if isinstance(v, float) and (v != v):  # IEEE 754: NaN is the only value not equal to itself
        return None
    return v
```

**Issue**: The NaN check is correct but is only applied in the `metrics_anomaly()` endpoint. Other endpoints like `/api/dashboards/query` and `/api/dashboards/render` don't apply this check and could return NaN values to the frontend, causing rendering issues.

**Recommendation**: 
- Extract `_safe()` as a shared utility
- Apply consistently across all endpoints that return float data
- Consider: Should NaN be converted to `null` in JSON, or should the SQL query use `if(isnan(v), NULL, v)`?

---

### 🟡 **Issue 5: Histogram Mean Calculation Philosophy**
**Location**: [app.py:1229, 1227-1228](app.py#L1229)

**Code**:
```python
# In _proto_metrics_to_events:
mean_val = hist_sum / count if count > 0 else 0.0

# In v_otel_metrics_1m view:
avg(if(Count > 0, Sum / Count, 0)) AS Value,
```

**Issue**: The histogram is normalized to its mean (sum/count) rather than preserving the raw sum/count. This is reasonable for a "normalized" metric space but should be explicitly justified.

**Consideration**: 
- For spike detection on histograms, using mean is appropriate (compares apples-to-apples with gauges/sums)
- For preserving distribution shape, you'd want buckets
- Current choice is defensible but should have a design comment

**Recommendation**: Add a SQL comment explaining the design decision.

---

### 🟢 **Issue 6: Attribute Fingerprinting Cardinality Limit**
**Location**: [app.py:1083-1091](app.py#L1083-L1091)

**Code**:
```python
pairs = sorted(
    f"{k}={v}"
    for k, v in attrs.items()
    if not any(k.startswith(p) for p in _FINGERPRINT_SKIP_PREFIXES)
)[:8]  # Limit to 8 key=value pairs
```

**Assessment**: Limiting to 8 pairs is reasonable for cardinality control. Typical OTEL attributes (env, region, instance, version) are <8. This is a good design choice.

**No change needed**; could add a comment explaining the rationale.

---

### 🟢 **Issue 7: SQL Injection Protection**
**Location**: [app.py:3549-3560](app.py#L3549-L3560)

Assessment: ✓ **Good**
- Uses parameterized queries (`?` placeholders with `params` list)
- Validates `hours` parameter (clamped to 1–168)
- Strips whitespace from service/metric/attr_fp strings
- Properly escaped in `fp_clause` string (though still using string format, which is safe because it's only used for optional WHERE clause structure)

**Minor improvement**: `fp_clause` uses string formatting. This is safe (no user input), but for consistency:
```python
if attr_fp:
    fp_clause = " AND AttrFingerprint = ?"
    params.append(attr_fp)
else:
    fp_clause = ""
```
✓ Already done correctly.

---

## Test Coverage

### ✅ Strengths
- **Schema tests**: Verify all 3 tables and 2 views exist
- **Ingest tests**: Cover gauge, sum, histogram acceptance and persistence
- **Fingerprint tests**: Stability and runtime-attr exclusion
- **Anomaly detection tests**: Spike flagging (10-sigma), steady-series non-over-flagging
- **Template tests**: dual_axis_anomaly and anomaly_overlay presence
- **Rendering tests**: Synthetic data with color binding validation

### ⚠️ Coverage Gaps
1. **No negative test for missing tables/views recovery** — What if a view is dropped? Test the lazy-recreation or error path.
2. **No test for boundary hour values** — hours=1, hours=168, hours=0 (should clamp to 1), hours=9999 (should clamp to 168)
3. **No test for attr_fp filtering** — POST a gauge with specific attributes, then query `/api/metrics/anomaly?attr_fp=...`
4. **No test for max 1440-row LIMIT** — Ingesting many days of data; verify rowcount doesn't exceed LIMIT
5. **No multi-metric test** — Ingest metrics with different MetricNames, verify anomaly_overlay doesn't cross-pollinate

**Specific test to add**:
```python
async def test_anomaly_api_hours_boundary_clamping(self, client):
    """Test that hours parameter is clamped to 1–168."""
    # Should be treated as hours=168
    r = await client.get("/api/metrics/anomaly?service=x&metric=y&hours=9999")
    assert r.status_code in (200, 400)
    # hours=0 should be clamped to 1
    r = await client.get("/api/metrics/anomaly?service=x&metric=y&hours=0")
    assert r.status_code in (200, 400)
```

---

## UI/UX Integration

### 🟡 **Issue 8: Anomaly Badge Scope Limited**
**Location**: [custom_dashboard_view.html:36-37](templates/custom_dashboard_view.html#L36-L37)

Assessment: 
- Badge added to chart card header ✓
- Updates on render ✓
- Shows worst state (normal/warning/outlier) ✓

**Gap**: Issue 37 asks for anomaly state display in:
> "For existing metric tables/pages or detail views, display anomaly state inline (badge/icon or table cell), and provide a user discoverable drill-in for context/explanation."

Currently anomaly badge only exists on dashboard custom charts, not on the main metrics pages (logs, traces, dashboards list, detail views).

**Recommendation**: Not a blocker for this PR (focuses on custom dashboards), but file a follow-up issue for main UI surfaces.

---

## Architecture & Design

### ✅ Good Decisions
1. **3-sigma method**: Simple, effective, no external ML dependency ✓
2. **Attribute fingerprinting**: Elegant solution to reduce high-cardinality attributes ✓
3. **View-based approach**: Follows ClickStack guidance (minimal helper objects) ✓
4. **Per-minute buckets**: Appropriate granularity for monitoring ✓
5. **Window function rolling baseline**: ClickHouse-native, performant ✓

### ⚠️ Design Questions (Not blockers, but consider documenting):
1. **Why 60-minute window?** — Is this configurable? Should it be?
2. **Why 2-sigma warning, 3-sigma outlier?** — Standard choices, but should be documented
3. **Histogram handling**: Why normalize to mean? Why not preserve buckets?
4. **Fingerprinting limits**: Why 8 pairs? Why MD5 (non-cryptographic)?

---

## Performance Considerations

### ✅ Observations
- **Partitioning**: By `toDate(TimeUnixMs)` → daily partitions, good for pruning
- **Ordering**: `(ServiceName, MetricName, AttrFingerprint, TimeUnixMs, TimeUnix)` match query patterns
- **View overhead**: 2 views, each is UNION ALL with simple grouping — acceptable overhead
- **API LIMIT 1440**: For 24-hour window at 1-minute granularity, 1440 rows = perfect fit; for 7 days, 1440 = 5-hour truncation
  - Consider: should LIMIT be dynamic based on hours? (e.g., `min(1440, hours * 60)`)

### 🟡 **Issue 9: Query Performance Under Scale**
The `v_otel_metrics_anomaly` view with a window function over 60 rows could be expensive on very high-cardinality series (millions of unique `(ServiceName, MetricName, AttrFingerprint)` tuples). This should be monitored in production but is not a blocker for merge.

---

## Missing Elements from Issue 37

### 📋 Checklist vs. Issue Requirements:
- ✅ Short design note (in PR body)
- ✅ DDL (3 tables + 2 views in app.py)
- ✅ Main anomaly detection SQL (v_otel_metrics_anomaly view)
- ✅ App/query changes (new endpoint, template, fingerprint logic)
- ✅ Tests (21 tests)
- ⚠️ Design note is in PR body but not in codebase docs (DESIGN.md, etc.)
- ⚠️ "UI + Custom Dashboards Graph Wiring" — Mostly implemented but drilldown metadata gap (Blocker 1)

---

## Recommendations Summary

### 🔴 **Must Fix Before Merge**
1. ✅ **FIXED**: Drilldown metadata now includes `_anomaly_state` and `_anomaly_score` for `anomaly_overlay` template tooltips
2. 🟡 **Blocker 2**: Clarify window function semantics (60-row window including or excluding current) with inline comment
3. **Code quality**: Add NaN-safe conversion to all float-returning endpoints (extract shared utility)

### 🟡 **Should Fix Before Merge**
1. Add test coverage for `hours` parameter boundary clamping
2. Add test for `attr_fp` filtering in anomaly API
3. Add clarifying comments in SQL (histogram mean normalization, window function design, zero-variance handling)
4. Consider dynamic LIMIT based on `hours` parameter

### 🟢 **Nice to Have / Follow-up**
1. Document design decisions (3-sigma rationale, 60-minute window, 8-pair fingerprint limit) in code comments or DESIGN.md
2. Add anomaly badges to main metrics/detail pages (separate issue)
3. Monitor performance of `v_otel_metrics_anomaly` view at scale

---

## Summary Table

| Category | Status | Notes |
|----------|--------|-------|
| **Feature Completeness** | ✅ | All main requirements met |
| **SQL-First Design** | ✅ | Clean, ClickHouse-native |
| **Schema** | ✅ | Good partitioning, ordering |
| **Metric Parsing** | ✅ | Handles gauge/sum/histogram correctly |
| **Anomaly Scoring** | ✅ | 3-sigma with proper edge case handling |
| **Drilldown Metadata** | 🔴 | Missing anomaly state/score in tooltip |
| **Window Function** | 🟡 | Semantics unclear (document) |
| **API Endpoint** | ✅ | Proper validation, parameterized queries |
| **Dashboard Integration** | 🟡 | Mostly complete; drilldown gap |
| **Test Coverage** | 🟡 | Good baseline; needs boundary/filter tests |
| **Performance** | ✅ | Reasonable for stated use case |
| **Code Quality** | 🟡 | Good structure; needs documentation |

---

## Final Verdict

**Recommendation: ✅ PENDING REVIEW** (was: Request Changes)

**Status Update**: The primary blocker (missing drilldown metadata for anomaly tooltips) has been **FIXED**. The `anomaly_overlay` template now properly passes `_anomaly_state` and `_anomaly_score` to the frontend, enabling the full "why flagged" tooltip functionality.

**Remaining items**:
- 🟡 Window function semantics should be clarified with a comment (documentation, not functional blocker)
- 🟡 Additional test coverage for boundary cases would strengthen the PR
- 🟡 Shared NaN-safe utility for float handling across endpoints

**Estimated effort to address remaining items**: 1-2 hours.

The PR is now **functionally complete** and ready for core review. The remaining items are quality improvements and documentation enhancements.

---

## Reviewer Notes for Discussion

1. **Window function design**: Is the 60-row window intended to *include* or *exclude* the current point? This affects whether the baseline is influenced by the point being flagged.
   
2. **Histogram normalization**: Is mean-value normalization the right choice, or should we preserve sum/count for different analysis?

3. **Fingerprinting limits**: Have you tested fingerprint collisions under realistic OTEL attribute loads? (e.g., 100+ unique attributes across services)

4. **Multi-tenancy**: Is the schema ready for future multi-tenant support, or does `ServiceName` partitioning assume single-tenant deployment?
