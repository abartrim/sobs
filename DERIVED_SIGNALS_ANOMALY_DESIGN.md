# Derived Signals Anomaly Design (Option C)

## Goal

Enable anomaly detection on operational behavior derived from logs, traces, and errors, not only native OTEL metrics payloads.

## Problem

Native metrics submitted to `/v1/metrics` may be sparse or incomplete in local/dev scenarios.
For useful anomaly detection and rules, we need a first-class derived-signal pipeline.

## Design Principles

1. SQL-first execution in ClickHouse/chDB.
2. Minimal object count (views over schema rewrites).
3. Unified signal contract so one scorer can handle all sources.
4. Discoverable UI entry point (Metrics page), not drilldown-only.

## Unified Signal Contract

Each minute-bucketed signal row has:

- `ServiceName`
- `SignalSource` (`logs`, `traces`, `errors`, later `metrics`)
- `SignalName`
- `AttrFingerprint`
- `MinuteBucket`
- `Value`
- `SampleCount`

This contract is exposed in `v_derived_signals_1m`.

## First Implementation Slice

### Derived signals (`v_derived_signals_1m`)

From logs:
- `log_volume`
- `error_volume`
- `error_ratio`

From traces:
- `trace_volume`
- `trace_error_ratio`
- `latency_p95_ms`

From errors/log exceptions:
- `exception_volume`

### Scored signals (`v_derived_signals_anomaly`)

Apply rolling-window scoring over each series partition:

- `baseline_mean`
- `baseline_stddev`
- `baseline_lower`, `baseline_upper`
- `anomaly_score`
- `anomaly_state` (`normal`, `warning`, `outlier`)

Window: 60 rows (`ROWS BETWEEN 59 PRECEDING AND CURRENT ROW`).

## UX Access Model

- Add top-level Metrics nav route `/metrics`.
- Show distinct series list with latest value/state and links to details.
- Details route `/metrics/anomaly` supports source/signal/service/fingerprint/time filtering.
- Chart click drilldown remains supported, but not required for discovery.

## Plan

1. Implement derived-signal and anomaly views.
2. Implement `/metrics` index page with selectors and latest-state table.
3. Extend `/metrics/anomaly` to support derived signals in addition to native OTEL metric drilldowns.
4. Add tests for view existence and page availability.
5. Add template/rule authoring next:
   - user-defined thresholds
   - baseline deviations
   - composite conditions

## Follow-up (next phase)

- Derived dimensions beyond service-level (route, operation, error fingerprint).
- Rule storage and evaluation policy (cooldowns, min sample count).
- Unified incident timeline that correlates logs/traces/errors around anomalies.

## March 2026 Update

The next implementation phase is editor-first and single-model:

1. No backward-compatibility requirement for chart/dashboard schema during this phase.
2. SQL builder and visual builder are developed together as one workflow.
3. Dashboard rendering contracts should be driven by `ChartSpec` compile/dry-run/validate/render APIs.

See the V2 decision section in [CHART_TEMPLATES_DESIGN.md](CHART_TEMPLATES_DESIGN.md) for the detailed authoring model and delivery slices.
