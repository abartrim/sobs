# Web Vitals Observability Design

## Goal

Elevate RUM web vitals from static p75 stat cards into a first-class observability signal:
sparklines showing trends over time, hotspot tables identifying the worst-performing URLs,
and anomaly-backed indicator badges that reuse the exact same rule infrastructure already
driving OTEL metrics and derived-signal alerts.

---

## Consistency Principle

The rule/anomaly pipeline is already the canonical path for threshold-based alerting in sobs.
Web vitals must flow through it, not alongside it.

| What already exists | How web vitals reuse it |
|---|---|
| `v_derived_signals_1m` — 1-min bucketed signal series | Add `SignalSource='rum_vitals'` UNION ALL branches querying `hyperdx_sessions` |
| `v_derived_signals_anomaly` — rolling-window scorer | Zero changes; vitals rows pass through automatically |
| `sobs_anomaly_rules` — per-signal threshold records | Pre-seed one rule per CWV metric at startup |
| Auto-candidate detection (`_build_auto_metric_rule_candidates`) | Surfaces vitals with recurring non-normal state, no changes needed |
| `/metrics/rules` UI | Lists vitals rules, allows override; no template changes needed |

The end result: a user can edit CLS thresholds from the Metrics Rules page and the RUM page
immediately reflects the updated badge state — identical to editing a latency rule.

---

## Data Sources

Web vitals land in `hyperdx_sessions` at ingest (`/v1/rum`):

```
EventName = 'web-vital'
Body      = '{"name":"LCP","value":1823.4,"rating":"good","delta":1823.4,...}'
LogAttributes = {
  'page.url':   'https://example.com/checkout',
  'session.id': '...',
  ...
}
ServiceName, Timestamp (DateTime64(9))
```

The vital **name** and **value** live inside `Body` as JSON.
`JSONExtractString(Body, 'name')` and `JSONExtractFloat(Body, 'value')` are the
access patterns used throughout this design.

---

## Phase 1 — Schema Extension + Seeded Rules + Indicator Badges

### 1.1 Add `rum_vitals` branches to `v_derived_signals_1m`

Replace the view definition with the existing UNION ALL plus six new branches —
one per Core Web Vital. Example shape (repeated for each metric):

```sql
SELECT
    ServiceName,
    'rum_vitals'                                              AS SignalSource,
    'LCP'                                                     AS SignalName,
    substring(lower(hex(MD5(concat(ServiceName,'|rum_vitals|LCP')))), 1, 16)
                                                              AS AttrFingerprint,
    toStartOfMinute(Timestamp)                                AS MinuteBucket,
    toFloat64(quantileExact(0.75)(JSONExtractFloat(Body,'value')))
                                                              AS Value,
    count()                                                   AS SampleCount
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
  AND JSONExtractString(Body, 'name') = 'LCP'
GROUP BY ServiceName, MinuteBucket
```

Signals added: `LCP`, `INP`, `CLS`, `TTFB`, `FCP`, `FID`.

`v_derived_signals_anomaly` does not change. The new rows satisfy its
`PARTITION BY ServiceName, SignalSource, SignalName, AttrFingerprint` contract
and will be scored automatically.

### 1.2 Pre-seed `sobs_anomaly_rules` at startup

Insert one threshold rule per metric on first run (skip if already present via
`ReplacingMergeTree` deduplication on the composite ORDER BY key).

CWV thresholds sourced from web.dev specification:

| Metric | WarningThreshold (Needs Improvement) | CriticalThreshold (Poor) | Comparator | Unit |
|--------|--------------------------------------|--------------------------|------------|------|
| LCP    | 2500                                 | 4000                     | gt         | ms   |
| INP    | 200                                  | 500                      | gt         | ms   |
| CLS    | 0.1                                  | 0.25                     | gt         | score|
| TTFB   | 800                                  | 1800                     | gt         | ms   |
| FCP    | 1800                                 | 3000                     | gt         | ms   |
| FID    | 100                                  | 300                      | gt         | ms   |

`MinSampleCount = 5` prevents flapping on sparse traffic.
`ServiceName = ''` (wildcard) applies the rule across all services.

### 1.3 Replace hardcoded Jinja thresholds with live anomaly state

Current `rum.html` computes `good`/`poor` inline in the template using hardcoded
threshold comparisons. After Phase 1 the `/rum` route queries
`v_derived_signals_anomaly` at page load:

```sql
SELECT
    SignalName,
    argMax(value, time)        AS latest_value,
    argMax(anomaly_state, time) AS latest_state,
    argMax(SampleCount, time)  AS latest_count
FROM v_derived_signals_anomaly
WHERE SignalSource = 'rum_vitals'
  AND (ServiceName = :service OR :service = '')
  AND time >= now() - INTERVAL 1 HOUR
GROUP BY SignalName
```

`anomaly_state` values (`normal`, `warning`, `outlier`) replace the Jinja
`vitals-good`/`vitals-needs`/`vitals-poor` classes:

| `anomaly_state` | CSS class | Badge label |
|---|---|---|
| `normal` | `vitals-good` | Good |
| `warning` | `vitals-needs` | Needs improvement |
| `outlier` | `vitals-poor` | Poor |

This means the badge colour is now driven by the user-editable rule threshold,
not a constant baked into the template.

---

## Phase 2 — Sparklines + Hotspot Table

### 2.1 Sparklines (per-metric trend, inline on vitals cards)

One sparkline per vital card. Width ~120 px, height ~32 px, rendered as inline SVG
in the template (or ECharts mini-chart). No additional API endpoint needed; the
data can be embedded in the `/rum` page response as a JSON block.

**Query — last 24 one-minute buckets:**

```sql
SELECT
    SignalName,
    groupArray(MinuteBucket) AS times,
    groupArray(Value)        AS values
FROM v_derived_signals_1m
WHERE SignalSource = 'rum_vitals'
  AND (ServiceName = :service OR :service = '')
  AND MinuteBucket >= now() - INTERVAL 24 MINUTE
GROUP BY SignalName
ORDER BY SignalName
```

For a longer window (e.g. 1-hour sparkline) increase the interval to
`INTERVAL 60 MINUTE`; bucketing remains 1-minute granularity.

**Rendering approach:**
- Use ECharts `line` type with `symbol: 'none'`, `smooth: true`, axes hidden.
- Colour the line by the current `anomaly_state` (green / amber / red).
- Clip to the card width so no layout change is needed.

### 2.2 Hotspot Table (worst-performing URLs per metric)

A collapsible panel below the vitals cards listing the top-10 URLs driving the
most "poor" readings for each selected metric period.

**Query:**

```sql
SELECT
    JSONExtractString(Body, 'name')                  AS metric,
    LogAttributes['page.url']                        AS url,
    count()                                          AS total,
    countIf(JSONExtractFloat(Body,'value') > :crit)  AS poor_count,
    round(poor_count / total, 3)                     AS poor_rate,
    round(quantileExact(0.75)(JSONExtractFloat(Body,'value')), 1) AS p75
FROM hyperdx_sessions
WHERE EventName = 'web-vital'
  AND metric = :metric_name
  AND (ServiceName = :service OR :service = '')
  AND Timestamp >= :from_ts AND Timestamp < :to_ts
GROUP BY metric, url
HAVING total >= 5
ORDER BY poor_rate DESC, total DESC
LIMIT 10
```

The `:crit` parameter is read from the matching `sobs_anomaly_rules` row so the
poor/good cut-off stays in sync with the rule thresholds.

**UX:**
- Tabbed or metric-selector dropdown to switch between LCP / INP / CLS etc.
- Poor-rate rendered as a small inline bar coloured: < 25 % green, 25–75 % amber, > 75 % red.
- Clicking a URL row filters the event list below to that URL.

---

## Phase 3 — Dedicated Web Vitals Page (future)

A `/web-vitals` route providing a full breakdown beyond what fits on the RUM overview:

| Section | Details |
|---|---|
| **CWV Scorecard** | Pass/Fail per metric, % of sessions Good/NI/Poor |
| **Trend Charts** | 24 h line chart per metric with anomaly bands (same as `v_derived_signals_anomaly` overlay used on metrics charts) |
| **Breakdown Dimensions** | Route URL, browser, device class, country |
| **Regression Panel** | Any `rum_vitals` signal currently in `warning` or `outlier` state sorted by anomaly_score descending |
| **Correlation Tab** | Overlay web vitals trend against backend latency_p95_ms for the same service — reuses existing `v_derived_signals_anomaly` query pattern |

The Regression Panel is powered by the same `_build_auto_metric_rule_candidates`
function already used for the Metrics auto-rule suggestions — no new detection logic.

---

## Delivery Slices

| Phase | Work items |
|---|---|
| **1a** | Add 6 UNION ALL branches to `v_derived_signals_1m`; no HTTP route changes |
| **1b** | Startup seeding of `sobs_anomaly_rules` for CWV thresholds |
| **1c** | `/rum` route: replace Python in-memory p75 loop with `v_derived_signals_anomaly` query; pass `anomaly_state` per metric to template |
| **1d** | `rum.html`: replace hardcoded Jinja threshold logic with `anomaly_state`-driven CSS classes |
| **2a** | `/rum` route: add sparkline time-series query; embed as JSON in template context |
| **2b** | `rum.html`: render sparklines inside vitals cards using ECharts mini-chart |
| **2c** | `/rum` route: add hotspot query (parameterised crit threshold from rules); pass top-10 URLs per metric |
| **2d** | `rum.html`: add collapsible hotspot panel below vitals cards |
| **3** | New `/web-vitals` route + `templates/web_vitals.html` |

Phases 1a–1d are purely additive and have no UI risk — existing page behaviour is
preserved until 1d lands.

---

## Open Questions

1. **CLS unit** — CLS values are dimensionless scores (0–1+), not milliseconds.
   The `vitals-*` CSS classes and units label ("ms") need a per-metric unit map in the template.

2. **Multi-service RUM** — `ServiceName=''` wildcard makes sense for single-service deploys.
   For multi-service, Phase 2 should filter by the service selector already present in the RUM filter bar.

3. **Bucket granularity for sparklines** — 1-minute buckets may be too granular for low-traffic
   sites. Consider `toStartOfFiveMinutes` as a fallback when `SampleCount < 10` per bucket.

4. **Body vs LogAttributes storage** — vital `name`/`value` are in `Body` JSON today.
   A future ingest change could promote them to typed `LogAttributes` keys for faster
   `JSONExtractFloat` access, but that is an ingest concern not a blocker for this design.
