# SOBS – Prometheus & OTEL Metrics Integration

This directory provides example configurations that let you forward **Prometheus** metrics
(and any other OpenTelemetry-compatible metric source) into SOBS for unified application
observability.

---

## Overview

SOBS accepts metrics through the standard **OTLP/HTTP** endpoint (`POST /v1/metrics`).
Prometheus metrics can be bridged into SOBS via one of two paths:

| Path | Best for |
|------|----------|
| **OpenTelemetry Collector** (recommended) | Any app already exposing a `/metrics` Prometheus scrape endpoint |
| **Python OTLP SDK** (direct) | Python services that want to push metrics without exposing a scrape endpoint |

Both paths produce the same typed metric tables in SOBS:
- `otel_metrics_gauge` — instantaneous values (e.g. CPU %, memory bytes)
- `otel_metrics_sum` — monotonic counters or delta accumulators (e.g. request counts)
- `otel_metrics_histogram` — distribution summaries (e.g. request latencies)

---

## Option 1 – OpenTelemetry Collector bridge (recommended)

The **OpenTelemetry Collector** scrapes existing Prometheus `/metrics` endpoints and
forwards the data as OTLP metrics to SOBS.  No changes are needed to the instrumented
application.

### Files

| File | Description |
|------|-------------|
| `otel-collector-config.yaml` | Collector pipeline: Prometheus receiver → OTLP/HTTP exporter |
| `docker-compose.yml` | Full local stack: SOBS + Collector + demo app |

### Quick start

```bash
# 1. Start the full stack
docker compose -f examples/prometheus/docker-compose.yml up -d

# 2. Open SOBS
open http://localhost:44317/metrics
```

The demo stack:
1. Runs SOBS on `localhost:44317`.
2. Runs the OpenTelemetry Collector, which scrapes the demo app's `/metrics` endpoint
   every 15 seconds and forwards data to `http://sobs:4317`.
3. Runs a tiny Python app that exposes synthetic Prometheus metrics on port `8000`.

### Adapting to your own app

Edit `otel-collector-config.yaml` → `receivers.prometheus.config.scrape_configs` and point
it at your existing Prometheus target(s).

```yaml
scrape_configs:
  - job_name: 'my-service'
    static_configs:
      - targets: ['my-service:8080']
```

### Authentication

If SOBS is running with `SOBS_API_KEY` set, add the key to the collector exporter:

```yaml
exporters:
  otlphttp:
    endpoint: http://sobs:4317
    headers:
      X-API-Key: "your-api-key"
```

---

## Option 2 – Python OTLP SDK (direct push)

Use the `opentelemetry-sdk` metrics API to emit gauge, counter, and histogram metrics
directly from your Python application to SOBS.

### File

| File | Description |
|------|-------------|
| `python_metrics_example.py` | Gauge, counter, and histogram via OpenTelemetry Python SDK |

### Install

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
```

### Run

```bash
# Start SOBS first
docker run -p 44317:4317 ghcr.io/abartrim/sobs:latest

python examples/prometheus/python_metrics_example.py
```

---

## Option 3 – curl (manual / scripting)

Send OTLP metrics directly with `curl` — no SDK or collector required.

```bash
bash examples/curl_examples.sh
```

The `curl_examples.sh` file includes a gauge and a counter example at the end of the
script.

---

## Configuration reference

### OTel Collector – key parameters

| Parameter | Description |
|-----------|-------------|
| `receivers.prometheus.config.scrape_configs[*].job_name` | Logical name for the scrape job |
| `receivers.prometheus.config.scrape_configs[*].scrape_interval` | How often to scrape (default `1m`); `15s` is common for development |
| `receivers.prometheus.config.scrape_configs[*].static_configs[*].targets` | `host:port` list of Prometheus targets |
| `exporters.otlphttp.endpoint` | SOBS base URL (collector appends `/v1/metrics` automatically) |
| `exporters.otlphttp.headers.X-API-Key` | Required only when `SOBS_API_KEY` is set |
| `exporters.otlphttp.tls.insecure` | Set `true` when SOBS is on plain HTTP (local development) |

### SOBS – relevant environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `4317` | SOBS listen port |
| `SOBS_API_KEY` | _(empty)_ | Optional API key; if set, collectors must send `X-API-Key` header |
| `SOBS_WRITE_QUEUE_MAX` | `5000` | Max buffered writes; increase for high-frequency scrapes |
| `SOBS_WRITE_BATCH_MAX` | `200` | Writes processed per DB batch |

---

## Limitations and considerations

- **Single-user / local-first**: SOBS is designed for local development and small-team
  deployments.  Do not expose it to the public internet without authentication.
- **OTLP only**: SOBS ingests metrics via OTLP/HTTP JSON.  The OTel Collector acts as a
  translation layer for Prometheus exposition format; SOBS does not natively scrape
  Prometheus endpoints.
- **No remote_write**: Prometheus `remote_write` speaks a different protocol.  Use the
  OTel Collector with a `prometheusreceiver` instead.
- **Cardinality**: Each unique combination of `service.name` + metric name + attribute set
  creates a separate time series.  Keep label cardinality low to avoid large table scans.
- **Scrape interval**: The Collector's `scrape_interval` directly controls how often new
  data points land in SOBS.  Values below `10s` are rarely useful for local observability.
- **Security**: In production, place SOBS behind a reverse proxy with TLS and set
  `SOBS_API_KEY`.  Never expose port 4317 directly to untrusted networks.

---

## Viewing metrics in SOBS

After data starts flowing:

1. Open **http://localhost:44317/metrics** to see all ingested metric series.
2. Use **Metrics → Metrics Rules** to define alert thresholds or derived signals.
3. Use **Metrics → Anomaly** to explore automatically detected outliers.
4. Create custom dashboards with the Auto Generate Dashboard feature.
