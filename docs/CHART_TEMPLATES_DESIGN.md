# Chart Templates Design — Phase 1

## Decision Update (March 2026): Editor-First Single Model

This design is now superseded by an editor-first V2 plan with **no backward-compatibility requirement**.

### Product Decision

1. We can reset DB/history during this phase.
2. SQL authoring and visual authoring are equally important and must be built together.
3. Dashboard UX should optimize for fast state understanding, while deep analysis remains available in detail views.

### Why this change

- Iterating chart visuals without a first-class editor led to circular frontend tweaks.
- Backend contracts are clearer when driven by editor workflows (compile, dry-run, validate, render).
- The chart system should converge on one spec-first model with no compatibility shims.

### V2 Architecture (authoring and rendering)

Use one canonical `ChartSpec` object as the source of truth for both SQL and visual configuration.

```json
{
  "id": "uuid",
  "name": "Trace Volume",
  "template_id": "derived_signal_overlay",
  "data": {
    "source": "v_derived_signals_anomaly",
    "time_column": "time",
    "filters": [{"field": "SignalName", "op": "=", "value": "trace_volume"}],
    "window": "6h"
  },
  "sql": {
    "mode": "builder",
    "generated_sql": "SELECT ...",
    "override_sql": ""
  },
  "visual": {
    "series_type": "line",
    "encodings": {"x": "time", "y": "value", "state": "effective_state"},
    "show_band": true,
    "zoom": {"inside": true, "slider": false}
  }
}
```

### Required backend workflow endpoints

1. `compile_spec` -> build SQL from editor model.
2. `dry_run_sql` -> return column names/types/sample rows.
3. `validate_spec` -> mapping and semantic checks (roles, required columns, unsafe query checks).
4. `render_spec` -> produce final eCharts option.

These endpoints should be treated as editor APIs first, dashboard APIs second.

### Editor UX model (build SQL + visual simultaneously)

Dual-pane layout:

- Left pane: SQL builder + optional raw SQL override.
- Right pane: visual mappings (roles, template options, anomaly layers, zoom).
- Bottom panel: preview chart + result sample + validation messages.

Round-trip requirements:

1. SQL/data changes re-validate visual mappings immediately.
2. Visual role changes update required SQL columns and validation hints.
3. Save is blocked until both SQL and visual validation pass.

### V2 data model guidance

- Persist full `ChartSpec` JSON as the canonical chart representation.
- Keep denormalized lookup columns only when they are generated from `ChartSpec`.
- Do not carry compatibility shims during this phase.

### Delivery slices

1. Vertical Slice A: `ChartSpec` model + compile/dry-run/validate/render APIs.
2. Vertical Slice B: dual-pane editor skeleton wired to those APIs.
3. Vertical Slice C: role-mapping UX + save/apply to dashboard cards.
4. Vertical Slice D: visual polish and additional template capabilities.

### Non-goals during this phase

- Migrating old chart configs.
- Maintaining compatibility with legacy schema structures.
- Over-optimizing chart aesthetics before editor contracts stabilize.

## Overview

Implement a **template-driven chart system** that decouples chart configuration from code, enabling non-developers to create new chart types via visual editor in Phase 2.

```
Phase 1: Template Registry + Rendering Engine (code-defined templates)
Phase 2: Visual Template Editor (UI-defined templates)
Phase 3: Advanced features (marketplace, versioning, transformations)
```

---

## Core Concepts

### Template Schema

Each template defines:
- **Metadata**: name, description, icon
- **Authoring Guidance**: expected query shape and example SQL shown in the add-chart modal
- **Column Roles**: semantic meaning of each column (time, metric, percentile, etc.)
- **eCharts Option Template**: JSON with `{{role}}` placeholders
- **Drilldown**: optional source-view target and clicked-point binding rules
- **Validation**: min/max columns, type hints

```python
@dataclass
class ChartTemplate:
    id: str                              # Unique ID: "time_series_percentiles"
    name: str                            # Display: "Time Series with Bands"
    description: str                     # Help text
    icon: str                            # Bootstrap icon: "bi-graph-up"
    query_shape: str                     # Human-readable expected columns for authoring
    sample_sql: str                      # Starter SQL shown in the modal and reusable as-is
    drilldown: dict | None               # Optional chart click → source view mapping
    min_columns: int                     # Minimum columns required
    max_columns: int | None              # Max columns (None = unlimited)
    column_roles: dict[str, int]         # {"time": 0, "value": 1, "p95": 2}
    echarts_option_template: dict        # eCharts option with {{role}} placeholders
```

### Rendering Pipeline

```
Query Results (columns, rows)
    ↓
Validate columns match template
    ↓
Extract data by column_roles → bindings {role: [values]}
    ↓
Substitute {{role}} in echarts_option_template
    ↓
Return complete eCharts option
```

When drilldown is configured, the rendered chart remains interactive:

```
User clicks chart point / heatmap cell
  ↓
Rendered point metadata provides canonical source-view filters
  ↓
Open /logs, /traces, or /errors with from_ts / to_ts / service filters
```

The dashboard card should also expose an explicit **Open Source View** action so users can jump to the underlying dataset even before clicking a specific point. When a point is clicked, the action can be narrowed to the clicked time bucket or service.

To avoid brittle parsing of chart labels, the render layer should attach drilldown metadata to each plotted point or cell, for example:

```json
{
  "value": 42,
  "drilldown": {
    "from_ts": "2026-03-29T12:00:00Z",
    "window_s": 300,
    "service": "checkout"
  }
}
```

The browser should prefer this metadata over reconstructing timestamps from ECharts axis labels.

---

## Authoring UX

Phase 1 now includes light authoring guidance in the dashboard UI:
- The add-chart modal shows template description, expected query shape, and starter SQL.
- A **Use Example** action copies the template SQL into the editor for quick iteration.
- Preview continues to render through `/api/dashboards/render`, so users validate both SQL shape and chart wiring before saving.

This keeps the implementation template-first while reducing the amount of schema knowledge users need to carry in their heads.

## Built-in Templates

### 1. Time Series with Percentile Bands

**Use case:** Detect anomalies when metric leaves normal range  
**Columns:** `[time, value, p95, p99]`

```json
{
  "id": "time_series_percentiles",
  "name": "Time Series with Normal Range",
  "description": "Show metric with percentile bands for anomaly detection",
  "icon": "bi-graph-up",
  "drilldown": {
    "target": "traces",
    "label": "Open source traces",
    "bucket_seconds": 60,
    "time_axis": "x"
  },
  "min_columns": 4,
  "max_columns": 4,
  "column_roles": {
    "time": 0,
    "value": 1,
    "p95": 2,
    "p99": 3
  },
  "echarts_option_template": {
    "tooltip": {"trigger": "axis"},
    "legend": {"data": ["Metric", "p95 Band", "p99 Band"], "bottom": 0},
    "xAxis": {"type": "time", "data": "{{time}}"},
    "yAxis": {"type": "value"},
    "grid": {"left": "3%", "right": "4%", "bottom": "15%", "containLabel": true},
    "series": [
      {
        "name": "Metric",
        "type": "line",
        "data": "{{value}}",
        "lineStyle": {"color": "#0d6efd"},
        "symbol": "none"
      },
      {
        "name": "p95 Band",
        "type": "line",
        "data": "{{p95}}",
        "lineStyle": {"type": "dashed", "color": "#ffc107"},
        "symbol": "none"
      },
      {
        "name": "p99 Band",
        "type": "line",
        "data": "{{p99}}",
        "lineStyle": {"type": "dashed", "color": "#dc3545"},
        "symbol": "none",
        "areaStyle": {"color": "rgba(220, 53, 69, 0.1)"}
      }
    ]
  }
}
```

**Example SQL:**
```sql
SELECT 
  toStartOfMinute(Timestamp) AS time,
  avg(Duration) AS value,
  quantile(0.95)(Duration) AS p95,
  quantile(0.99)(Duration) AS p99
FROM otel_traces
GROUP BY time
ORDER BY time
```

---

### 2. Heatmap (2D Correlation)

**Use case:** Find which service × time buckets have high error rates  
**Columns:** `[x_category, y_category, value]`

```json
{
  "id": "heatmap",
  "name": "Heatmap",
  "description": "2D heatmap for error rates by dimension × time",
  "icon": "bi-fire",
  "drilldown": {
    "target": "traces",
    "label": "Open source traces",
    "bucket_seconds": 300,
    "time_axis": "y",
    "service_axis": "x"
  },
  "min_columns": 3,
  "max_columns": 3,
  "column_roles": {
    "x_category": 0,
    "y_category": 1,
    "value": 2
  },
  "echarts_option_template": {
    "tooltip": {"trigger": "item", "formatter": "{b}: {c}"},
    "xAxis": {"type": "category", "data": "{{x_unique_values}}"},
    "yAxis": {"type": "category", "data": "{{y_unique_values}}"},
    "visualMap": {
      "min": "{{value_min}}",
      "max": "{{value_max}}",
      "inRange": {"color": ["#ebedf0", "#c6e48b", "#7bc96f", "#239a3b", "#196127"]},
      "text": ["High", "Low"],
      "bottom": 0
    },
    "grid": {"left": "15%", "right": "10%", "bottom": "15%", "top": "10%", "containLabel": true},
    "series": [
      {
        "type": "heatmap",
        "data": "{{heatmap_data}}",
        "emphasis": {"itemStyle": {"borderColor": "#fff", "borderWidth": 2}}
      }
    ]
  }
}
```

**Example SQL:**
```sql
SELECT 
  ServiceName AS x_category,
  toStartOfFiveMinutes(Timestamp) AS y_category,
  round(100.0 * countIf(StatusCode = 'STATUS_CODE_ERROR') / count(), 2) AS value
FROM otel_traces
GROUP BY ServiceName, y_category
ORDER BY ServiceName, y_category
```

---

### 3. Box Plot (Distribution + Outliers)

**Use case:** Visualize distribution and detect outliers  
**Columns:** `[dimension, min, q1, median, q3, max]`

```json
{
  "id": "box_plot",
  "name": "Distribution Box Plot",
  "description": "Show distribution, quartiles, and outliers",
  "icon": "bi-boxes",
  "min_columns": 6,
  "max_columns": 6,
  "column_roles": {
    "dimension": 0,
    "min": 1,
    "q1": 2,
    "median": 3,
    "q3": 4,
    "max": 5
  },
  "echarts_option_template": {
    "tooltip": {"trigger": "item"},
    "xAxis": {"type": "category", "data": "{{dimension_values}}", "nameGap": 30},
    "yAxis": {"type": "value", "name": "Value"},
    "grid": {"left": "10%", "right": "10%", "bottom": "15%", "containLabel": true},
    "series": [
      {
        "type": "boxplot",
        "data": "{{boxplot_data}}",
        "itemStyle": {"color": "#0d6efd", "borderColor": "#0d6efd"}
      }
    ]
  }
}
```

**Example SQL:**
```sql
SELECT 
  HTTPMethod AS dimension,
  min(Duration) AS min,
  quantile(0.25)(Duration) AS q1,
  quantile(0.5)(Duration) AS median,
  quantile(0.75)(Duration) AS q3,
  max(Duration) AS max
FROM otel_traces
GROUP BY HTTPMethod
ORDER BY median DESC
```

---

### 4. Dual-Axis (Metric + Anomaly Score)

**Use case:** Compare live metric against anomaly detection signal  
**Columns:** `[time, metric, anomaly_score]`

```json
{
  "id": "dual_axis_anomaly",
  "name": "Metric + Anomaly Score",
  "description": "Compare metric vs anomaly detection signal on dual axes",
  "icon": "bi-graph-up-arrow",
  "drilldown": {
    "target": "logs",
    "label": "Open source logs",
    "bucket_seconds": 60,
    "time_axis": "x",
    "extra": {"analyze": "1", "stats": "1"}
  },
  "min_columns": 3,
  "max_columns": 3,
  "column_roles": {
    "time": 0,
    "metric": 1,
    "anomaly_score": 2
  },
  "echarts_option_template": {
    "tooltip": {"trigger": "axis"},
    "legend": {"data": ["Metric", "Anomaly Score"], "bottom": 0},
    "xAxis": {"type": "time", "data": "{{time}}"},
    "yAxis": [
      {
        "type": "value",
        "name": "Metric",
        "position": "left",
        "axisLine": {"lineStyle": {"color": "#0d6efd"}}
      },
      {
        "type": "value",
        "name": "Anomaly Score",
        "position": "right",
        "axisLine": {"lineStyle": {"color": "#dc3545"}}
      }
    ],
    "grid": {"left": "3%", "right": "4%", "bottom": "15%", "containLabel": true},
    "series": [
      {
        "name": "Metric",
        "type": "line",
        "data": "{{metric}}",
        "yAxisIndex": 0,
        "lineStyle": {"color": "#0d6efd"},
        "symbol": "none"
      },
      {
        "name": "Anomaly Score",
        "type": "bar",
        "data": "{{anomaly_score}}",
        "yAxisIndex": 1,
        "itemStyle": {"color": "rgba(220, 53, 69, 0.5)"}
      }
    ]
  }
}
```

**Example SQL (post-anomaly-detection):**
```sql
SELECT 
  toStartOfMinute(Timestamp) AS time,
  avg(Duration) AS metric,
  max(AnomalyScore) AS anomaly_score
FROM otel_traces_with_anomalies
GROUP BY time
ORDER BY time
```

---

### 5. Gauge (KPI with Thresholds)

**Use case:** Single-value KPI monitoring (SLA %, uptime %, satisfaction)  
**Columns:** `[value]`

```json
{
  "id": "gauge_kpi",
  "name": "KPI Gauge",
  "description": "Single-value gauge for KPI monitoring",
  "icon": "bi-speedometer",
  "min_columns": 1,
  "max_columns": 1,
  "column_roles": {
    "value": 0
  },
  "echarts_option_template": {
    "series": [
      {
        "type": "gauge",
        "progress": {"itemStyle": {"color": "#0d6efd"}},
        "axisLine": {
          "lineStyle": {
            "color": [[0.3, "#dc3545"], [0.7, "#ffc107"], [1, "#28a745"]],
            "width": 30
          }
        },
        "splitLine": {"distance": 8},
        "axisTick": {"distance": 8},
        "axisLabel": {"color": "#adb5bd"},
        "detail": {"valueAnimation": true, "formatter": "{value}%", "color": "#adb5bd"},
        "data": [{"value": "{{value_first}}", "name": "Current"}],
        "min": 0,
        "max": 100
      }
    ]
  }
}
```

**Example SQL:**
```sql
SELECT 
  round(sum(case when StatusCode >= 200 AND StatusCode < 400 then 1 else 0 end) / count(*) * 100, 2) AS value
FROM otel_traces
WHERE Timestamp > now() - interval 1 hour
```

---

## Implementation Details

### Template Storage

Templates stored in `CHART_TEMPLATES` dict (hardcoded in Phase 1, migrated to DB in Phase 2):

```python
CHART_TEMPLATES: dict[str, ChartTemplate] = {
    "time_series_percentiles": ChartTemplate(...),
    "heatmap": ChartTemplate(...),
    "box_plot": ChartTemplate(...),
    "dual_axis_anomaly": ChartTemplate(...),
    "gauge_kpi": ChartTemplate(...),
}
```

### Rendering Function

```python
def render_chart_from_template(
    template_id: str,
    columns: list[str],
    rows: list[dict | tuple]
) -> dict:
    """
    Render a chart by substituting query results into a template.
    
    Raises ValueError if template not found or columns don't match.
    Returns complete eCharts option dict.
    """
    template = CHART_TEMPLATES.get(template_id)
    if not template:
        raise ValueError(f"Unknown template: {template_id}")
    
    if len(rows) == 0:
        return {"series": [], "xAxis": {}, "yAxis": {}}
    
    # Validate column count
    if len(columns) < template.min_columns:
        raise ValueError(
            f"Template {template_id} requires at least {template.min_columns} columns, got {len(columns)}"
        )
    if template.max_columns and len(columns) > template.max_columns:
        raise ValueError(
            f"Template {template_id} accepts maximum {template.max_columns} columns, got {len(columns)}"
        )
    
    # Extract data bindings
    bindings = _extract_bindings(template, columns, rows)
    
    # Substitute into template
    option = _deep_substitute(template.echarts_option_template, bindings)
    
    return option
```

### DB Schema Changes

Update `sobs_chart_configs` table:
- Replace `ChartType` column with `TemplateId` column
- `TemplateId` → foreign key reference to template ID
- Keep existing `OptionsJson` for future custom overrides

```sql
ALTER TABLE sobs_chart_configs 
MODIFY COLUMN ChartType DROP,
ADD COLUMN TemplateId LowCardinality(String) CODEC(ZSTD(1));
```

### Frontend Changes

- Chart type picker becomes template picker
- Show template name + description + icon
- When template changes, validate that query columns match
- Preview updates dynamically
- Chart click events use template drilldown metadata to open source views

### Source View Time Window Filters

Source views accept the following query params for chart drilldown:

- `from_ts`: start of the selected bucket/window
- `to_ts`: exclusive end of the selected bucket/window
- `window_s`: optional window size in seconds; if provided with `from_ts`, the backend derives `to_ts`

Examples:

```text
/traces?service=checkout&from_ts=2026-03-29T12:00:00Z&window_s=300
/logs?from_ts=2026-03-29T12:05:00Z&to_ts=2026-03-29T12:06:00Z&analyze=1&stats=1
```

---

## Validation & Error Handling

1. **Column count mismatch** → User error, show helpful message
2. **Binding type mismatch** (e.g., non-numeric value in min/max) → Log + use defaults
3. **Template not found** → 500 error (shouldn't happen if properly deployed)
4. **Placeholder not substituted** (typo in template) → Log warning, return with `{{placeholder}}` intact

---

## Testing Strategy

- Unit tests for each template's bindings extraction
- Integration tests: real SQL queries → rendered eCharts options
- Snapshot tests: verify rendered options match expected structure
- UI tests: template picker, preview, validation

---

## Phase 2 Readiness

Once Phase 1 is solid:
- Move `CHART_TEMPLATES` to DB table: `sobs_templates`
- Add template CRUD API: `GET /api/templates`, `POST /api/templates` (admin only)
- Build visual template editor: form builder UI for eCharts JSON
- Add template validation: schema checking, column count validation

---

## Success Criteria (Phase 1)

- [ ] All 5 templates render correctly with real data
- [ ] All tests pass (linting, type-checking, unit tests)
- [ ] Column validation prevents bad queries
- [ ] Error messages are helpful
- [ ] Can easily add new templates without code refactor

---

## Future Enhancements

### Phase 1.5: Query Authoring Tools

To improve the user experience of writing ClickHouse queries in the dashboard modal, add:

#### SQL Autocomplete & DDL Helper
- When users type in the query textarea, provide autocomplete suggestions for:
  - Table names from available data sources (otel_logs, otel_traces, otel_spans, etc.)
  - Column names for each table (introspected from schema)
  - Common aggregation functions (avg, count, countIf, quantile, etc.)
  - Keywords and syntax hints
- Show a **DDL Inspector** pane that displays:
  - Available tables with column names and types
  - Quick reference for common column roles expected by templates
  - Example sub-queries for each table

#### SQL Syntax Highlighting
- Add lightweight syntax highlighting to the query textarea:
  - Keywords (SELECT, FROM, GROUP BY, ORDER BY) in one color
  - Function names in another
  - String literals and numbers in distinct styles
  - Comments in muted color
- Can use a simple library like [Prism.js](https://prismjs.com/) or hand-rolled regex-based highlighting for ClickHouse SQL

#### Integration Points
- Emit DDL schema on dashboard page load (or fetch via `GET /api/schema`)
- Attach autocomplete handler to the textarea element
- Update highlighting as user types
- Maintain chart preview functionality alongside new authoring tools

### Phase 2: Visual Template Editor

Move templates to the database and add a full visual template builder:
- Drag-and-drop eCharts option JSON builder
- Live preview as template is edited
- Template marketplace and sharing

### Phase 3: Advanced Features

- Template versioning and rollback
- Data transformations and custom functions
- Conditional rendering based on row count or data shape
- Template permissions and approval workflow
