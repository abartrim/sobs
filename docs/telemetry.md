# Sobs Self-Telemetry

Sobs includes optional OpenTelemetry-based self-telemetry to help measure
performance of hot paths during ingest, storage writes, rule evaluation, and
dashboard queries.

Telemetry is **disabled by default**. No external collector is required.

> **Runtime.** The published **Go** server implements this in `go/cmd/sobs/telemetry.go` using only
> the standard library: the `console` exporter writes to stderr and the `otlp` exporter POSTs
> OTLP/HTTP-JSON to `SOBS_TELEMETRY_OTLP_ENDPOINT`. No OpenTelemetry SDK is needed. The optional
> `requirements-telemetry.txt` (OTel SDK) applies only to the Python oracle. Every `SOBS_TELEMETRY_*`
> variable below is honored by both runtimes; both are a strict no-op when disabled.

---

## Configuration

Set environment variables before starting Sobs.

| Variable | Default | Description |
|---|---|---|
| `SOBS_TELEMETRY_ENABLED` | `false` | Set to `true` to enable telemetry. |
| `SOBS_TELEMETRY_SERVICE_NAME` | `sobs` | Service name reported in traces and metrics. |
| `SOBS_TELEMETRY_ENVIRONMENT` | `local` | Deployment environment label. |
| `SOBS_TELEMETRY_EXPORTER` | `none` | Exporter type: `none`, `console`, or `otlp`. |
| `SOBS_TELEMETRY_OTLP_ENDPOINT` | _(empty)_ | OTLP endpoint URL, required when `SOBS_TELEMETRY_EXPORTER=otlp`. |
| `SOBS_TELEMETRY_CONSOLE_EXPORT` | `false` | Also emit traces to console when `true`. |
| `SOBS_TELEMETRY_SAMPLE_RATE` | `1.0` | Trace sampling rate in `[0.0, 1.0]`. |

Standard OpenTelemetry variable also respected:

| Variable | Behaviour |
|---|---|
| `OTEL_SDK_DISABLED=true` | Disables telemetry even when `SOBS_TELEMETRY_ENABLED=true`. |

---

## Startup Modes

### Disabled (default)

Telemetry is disabled by default. No packages, collectors, or endpoints are needed.

```bash
python -m app
# or explicitly:
SOBS_TELEMETRY_ENABLED=false python -m app
```

Startup log:
```
Sobs telemetry disabled; using no-op telemetry.
```

---

### OpenTelemetry SDK globally disabled

```bash
OTEL_SDK_DISABLED=true SOBS_TELEMETRY_ENABLED=true python -m app
```

Startup log:
```
Sobs telemetry disabled; using no-op telemetry.
```

---

### Console telemetry (local debugging)

Emits traces and metrics to stdout. Useful for local development and debugging
without an external collector.

**Install optional dependencies first:**

```bash
pip install -r requirements-telemetry.txt
```

**Run:**

```bash
SOBS_TELEMETRY_ENABLED=true \
SOBS_TELEMETRY_EXPORTER=console \
SOBS_TELEMETRY_CONSOLE_EXPORT=true \
python -m app
```

Startup log:
```
Sobs telemetry enabled with console exporter.
```

---

### OTLP telemetry (Grafana Tempo, Jaeger, etc.)

Send traces and metrics to an OpenTelemetry collector via OTLP.

**Install optional dependencies first:**

```bash
pip install -r requirements-telemetry.txt
```

**Run:**

```bash
SOBS_TELEMETRY_ENABLED=true \
SOBS_TELEMETRY_EXPORTER=otlp \
SOBS_TELEMETRY_OTLP_ENDPOINT=http://localhost:4317 \
python -m app
```

Startup log:
```
Sobs telemetry enabled with OTLP exporter (endpoint=http://localhost:4317).
```

---

## Optional Dependencies

Telemetry packages are **not** required for core Sobs operation. Install them
only when you want to enable telemetry:

```bash
pip install -r requirements-telemetry.txt
```

This installs:
- `opentelemetry-api`
- `opentelemetry-sdk`
- `opentelemetry-exporter-otlp`
- `opentelemetry-instrumentation-asgi`

---

## Instrumented Hot Paths

When telemetry is enabled, the following spans are emitted:

| Span Name | Where | Key Attributes |
|---|---|---|
| `sobs.ingest.request` | Ingest route handlers (`/v1/logs`, `/v1/traces`, `/v1/metrics`) | `event.type`, `route` |
| `sobs.ingest.parse` | OTLP protobuf parsing | `event.type`, `parser` |
| `sobs.storage.write` | `_insert_rows_json_each_row` | `storage.engine=chdb`, `table`, `row.count` |
| `sobs.rules.evaluate` | `_apply_tag_rules` | `rule.count`, `event.count` |
| `sobs.dashboard.query` | Dashboard view routes (`/logs`, `/errors`, `/traces`, `/metrics`) | `dashboard.name`, `route` |

Metrics recorded:

| Metric Name | Unit | Description |
|---|---|---|
| `sobs.ingest.events.total` | count | Total ingested events |
| `sobs.ingest.batch.size` | count | Ingest batch size |
| `sobs.ingest.duration.ms` | ms | Ingest request duration |
| `sobs.storage.write.duration.ms` | ms | Storage write duration |
| `sobs.storage.query.duration.ms` | ms | Storage query duration |
| `sobs.dashboard.request.duration.ms` | ms | Dashboard request duration |
| `sobs.rules.evaluate.duration.ms` | ms | Rule evaluation duration |

---

## Security

- Raw event payloads, log bodies, SQL queries, headers, tokens, and PII are
  **never** included in span attributes.
- Only safe scalar metadata (counts, types, table names, route paths) is
  recorded.
- If telemetry configuration is invalid, Sobs logs a warning and continues
  startup without telemetry.

---

## Adding Spans to New Code

Use the `span` context manager from the `telemetry` module:

```python
import telemetry as _telemetry

with _telemetry.span("sobs.ingest.normalize", **{"event.type": "rum", "event.count": len(events)}):
    normalize_events(events)
```

Use the `traced_view` decorator for async route handlers:

```python
@app.route("/my-page")
@require_basic_auth
@_telemetry.traced_view("sobs.dashboard.query", **{"dashboard.name": "my-page", "route": "/my-page"})
async def view_my_page():
    ...
```

Both helpers are **no-ops** when telemetry is disabled. No imports or
conditional guards are needed in calling code.
