# Custom ECharts Test Examples

This folder contains end-to-end examples for the `custom_echarts` chart mode.

## Files

- `latency_p95_timeseries.sql`: time series SQL returning `ts` + `p95_ms`
- `latency_p95_mapping.json`: mapping for series placeholder bindings
- `latency_p95_option.json`: ECharts option JSON using `{{points}}`
- `payload_latency_p95_render.json`: full API payload for `/api/dashboards/spec/render`
- `error_rate_by_service.sql`: bar chart SQL returning `service` + `error_rate`
- `error_rate_mapping.json`: mapping JSON for bar chart
- `error_rate_option.json`: ECharts option JSON for category bar
- `payload_error_rate_render.json`: full API payload for `/api/dashboards/spec/render`

## Deterministic Ordering Requirement

For `custom_echarts`, your SQL should explicitly define row order when sequence matters.

- Always include `ORDER BY` for time series, ranked bars, and top-N outputs.
- Add tie-breaker columns when values can tie (for example: `ORDER BY ts, service`).
- Do not rely on implicit database row order.

## Custom Source Button For custom_echarts

In `custom_mapping_json`, add a reserved `_drilldown` object:

{
  "_drilldown": {
    "target": "logs|metrics|traces|errors",
    "label": "Open source traces",
    "extra": {
      "service": "{{service}}",
      "from_ts": "{{ts}}"
    }
  }
}

Notes:
- `extra` values support `{{column_name}}` placeholders resolved from the first result row.
- The chart card source button is shown when this block is present and valid.

## Quick API Test

From repository root:

```bash
curl -sS -X POST http://localhost:44317/api/dashboards/spec/render \
  -H 'Content-Type: application/json' \
  --data @examples/custom_echarts/payload_latency_p95_render.json | jq '.option.series[0].data[0:3]'
```

```bash
curl -sS -X POST http://localhost:44317/api/dashboards/spec/render \
  -H 'Content-Type: application/json' \
  --data @examples/custom_echarts/payload_error_rate_render.json | jq '.option.series[0].data[0:5]'
```

## UI Test

1. Open summary page and click Add Chart.
2. Set template to `Custom ECharts`.
3. Paste SQL from one `.sql` file into Raw SQL Override.
4. Paste mapping JSON into Custom Mapping JSON.
5. Paste option JSON into Custom ECharts Option JSON.
6. Click Preview, then Save.
