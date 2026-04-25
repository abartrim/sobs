# Sobs → jinja-bootstrap-spa Migration Audit

> **Framework:** `abartrim/jinja-bootstrap-spa` branch `codex/framework-backlog-foundation`
>
> **Date:** 2026-04-25  
> **Status:** First slice implemented (Work Items table)

---

## Summary

Sobs has ~25 non-help Jinja2 templates, each extending `base.html`. Nearly every
data page follows the same structural pattern:

```
Page Header (render_page_header macro)
└── Filter Accordion (render_filter_accordion macro)
    └── Table / list body rendered from backend rows
        └── Pagination controls (inline HTML, no shared macro)
```

All data tables are currently server-rendered on full-page load.  Several pages
add client-side polling or SSE on top of a full-page-rendered baseline.  There
are no fragment endpoints today.

---

## Candidate Surface Inventory

### 1. Work Items (`/work-items`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/work_items.html` |
| Backend route | `view_work_items()` — `app.py:18061` |
| Repeated UI pattern | Filterable table with pagination; cached backend queries |
| Recommended primitive | `jbs-component` (table region) + `jbs-refresh-trigger` |
| Streaming mode | `replace` (no SSE needed – work items are low-frequency) |
| State to preserve | active filters (form params), pagination offset, modal disclosure |
| Backend changes | Add `/fragments/work-items/table` fragment endpoint with ETag/304 |
| **Migration priority** | **High** |
| **Migration risk** | **Low** |

**Why first:** Simplest data table, no SSE, no live-mode, already has a
backend-owned cache (`_work_items_page_cache`).  ETag can be derived from
cache key + TTL stamp.

---

### 2. Logs (`/logs`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/logs.html` |
| Backend route | `view_logs()` — `app.py:11188` |
| Repeated UI pattern | Filterable table with sort + live-polling SSE mode |
| Recommended primitive | `jbs-component` + SSE row-append |
| Streaming mode | `append` (SSE) in live mode; `replace` on manual refresh |
| State to preserve | live-mode toggle, filter values, TZ badge, stats panel open state |
| Backend changes | Add `/fragments/logs/table` + SSE live-log stream endpoint |
| **Migration priority** | **High** |
| **Migration risk** | **Medium** (live-mode complicates fragment state) |

---

### 3. Errors (`/errors`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/errors.html` |
| Backend route | `view_errors()` — `app.py:14153` |
| Repeated UI pattern | Filterable accordion list; grouped/deduplicated toggle |
| Recommended primitive | `jbs-component` + `jbs-tab-state` for grouped/detail toggle |
| Streaming mode | `replace` |
| State to preserve | grouped mode toggle, error accordion open state, filters |
| Backend changes | Add `/fragments/errors/list` + grouped variant |
| **Migration priority** | **Medium** |
| **Migration risk** | **Medium** (accordion disclosure state is complex) |

---

### 4. Traces (`/traces`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/traces.html` |
| Backend route | `view_traces()` — `app.py:15254` |
| Repeated UI pattern | Filterable table + expandable span waterfall |
| Recommended primitive | `jbs-component` + `jbs-drawer` for trace detail |
| Streaming mode | `replace` |
| State to preserve | selected trace, span detail drawer open, filters |
| Backend changes | Add `/fragments/traces/table`; span detail already has API endpoint |
| **Migration priority** | **Medium** |
| **Migration risk** | **Medium** |

---

### 5. Metrics Anomaly (`/metrics/anomaly`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/metrics_anomaly.html` |
| Backend route | `view_metrics_anomaly()` |
| Repeated UI pattern | Table with polling refresh (auto-refreshes every N seconds) |
| Recommended primitive | `jbs-component` + `jbs-poll` |
| Streaming mode | `replace` with short poll interval |
| State to preserve | sort column, page, time window |
| Backend changes | Add `/fragments/metrics/anomaly/table` |
| **Migration priority** | **Medium** |
| **Migration risk** | **Low** |

---

### 6. Custom Dashboards (`/dashboards`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/custom_dashboards.html` |
| Backend route | `view_custom_dashboards()` |
| Repeated UI pattern | Card grid + chart editor modal |
| Recommended primitive | `jbs-card-grid` + `jbs-modal-form` |
| Streaming mode | SSE for live chart data |
| State to preserve | active dashboard, chart edit state |
| Backend changes | Add per-chart fragment endpoints |
| **Migration priority** | **Low** |
| **Migration risk** | **High** (complex eCharts wiring) |

---

### 7. AI Transparency (`/ai`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/ai.html` |
| Backend route | `view_ai()` |
| Repeated UI pattern | Filterable table with expandable conversation detail |
| Recommended primitive | `jbs-component` + `jbs-drawer` |
| Streaming mode | `replace` |
| State to preserve | open conversation, filters |
| Backend changes | Add `/fragments/ai/table` |
| **Migration priority** | **Low** |
| **Migration risk** | **Low** |

---

### 8. Reports (`/reports`)

| Attribute | Value |
|-----------|-------|
| Template | `templates/reports.html` |
| Backend route | `list_reports()` |
| Repeated UI pattern | Card list with save/load/delete actions |
| Recommended primitive | `jbs-list` + `jbs-action-confirm` |
| Streaming mode | `replace` |
| State to preserve | active filter page-type tab |
| Backend changes | Add `/fragments/reports/list` |
| **Migration priority** | **Low** |
| **Migration risk** | **Low** |

---

## Recommended Migration Order

| Order | Surface | Priority | Risk | Notes |
|-------|---------|----------|------|-------|
| 1 | **Work Items** | High | Low | ✅ Implemented in this PR |
| 2 | Metrics Anomaly | Medium | Low | Simple poll pattern |
| 3 | Logs | High | Medium | SSE + live mode needed |
| 4 | Errors | Medium | Medium | Accordion disclosure |
| 5 | Traces | Medium | Medium | Span waterfall |
| 6 | AI Transparency | Low | Low | Straightforward table |
| 7 | Reports | Low | Low | Small card list |
| 8 | Custom Dashboards | Low | High | Complex chart wiring |

---

## First Implemented Slice: Work Items Table

### What was implemented

- New fragment endpoint: `GET /fragments/work-items/table`
  - Accepts the same filter query params as `/work-items`
  - Returns only the `<div class="card">…table…</div>` HTML fragment
  - Computes a strong ETag from the serialized table content
  - Returns `304 Not Modified` when the client's `If-None-Match` matches
- Partial template: `templates/_work_items_table_fragment.html`
- Minimal JBS runtime: `static/jbs-runtime.js`
  - Handles `data-jbs-component`, `data-jbs-src`, `data-jbs-trigger`
  - Manages `data-jbs-phase` lifecycle: `idle → loading → swapping/not-modified → idle`
  - Implements POC visual instrumentation (configurable via `data-jbs-poc-instrumentation`)
- Updated `templates/work_items.html` to wire the fragment region

### Visual instrumentation (POC only)

| Event | Visual |
|-------|--------|
| New content swapped (200) | Blue-green flash (`jbs-swap-pulse`) |
| Cache hit / unchanged (304) | Amber flash (`jbs-not-modified-pulse`) |
| Loading in flight | Pulsing opacity (`jbs-is-loading`) |

### Framework primitives used

Because `abartrim/jinja-bootstrap-spa` is a private repository not available
in this CI environment, the following primitives were implemented locally as a
**minimal shim** in `static/jbs-runtime.js`.  Once the framework package is
installable, these shims should be replaced by the framework's canonical
implementations:

| Shim | Framework primitive |
|------|---------------------|
| `data-jbs-component` region tracking | `jbs-component` directive |
| ETag fetch + `If-None-Match` header | framework fetch helper |
| `data-jbs-phase` state machine | framework phase system |
| `jbs-swap-pulse` / `jbs-not-modified-pulse` | framework instrumentation CSS |
| `jbs-is-loading` class | framework loading system |

---

## Missing Framework Primitives (Follow-up Backlog)

The following generic primitives were not discoverable from the framework docs
included in the issue, and should be added to the framework backlog:

1. **`jbs-poll`** – periodic auto-refresh for a component region (needed for
   Metrics Anomaly migration).  Currently implemented inline in each page JS.

2. **`jbs-tab-state`** – preserve active Bootstrap tab across fragment swaps
   (needed for Errors grouped/detail toggle).

3. **`jbs-drawer`** – detail panel / inspector drawer bound to a table row
   click (needed for Traces and AI pages).  Currently implemented as Bootstrap
   modals with per-page `data-*` attribute wiring.

4. **`jbs-action-confirm`** – inline confirmation before a destructive row
   action (needed for Reports delete flow).

5. **`jbs-list`** – ordered card list with keyboard-navigable rows (needed for
   Reports page).

6. **Server-Sent Events (SSE) `append` mode** – appending new rows to an
   existing table body without replacing the whole component (needed for Logs
   live mode).  The Sobs backend already has the SSE infrastructure; only the
   client-side fragment integration is missing.
