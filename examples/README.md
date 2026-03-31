# SOBS Examples

This directory contains examples for sending telemetry to SOBS using various clients.

## Prerequisites

Start SOBS:

```bash
# Docker
docker run -p 4317:4317 ghcr.io/abartrim/sobs:latest

# or docker-compose
docker-compose up -d
```

## Examples

### Python – OpenTelemetry SDK

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
python python/otel_example.py
```

### Python – Flask with auto-instrumentation

```bash
pip install flask opentelemetry-sdk opentelemetry-instrumentation-flask \
    opentelemetry-exporter-otlp-proto-http requests
python python/flask_example.py
```

### Node.js – Express with OpenTelemetry SDK

```bash
cd nodejs
npm install
node example.js
```

### curl – No SDK required

```bash
bash curl_examples.sh
```

### Load example (repo script)

From the repository root:

```bash
# Default high-throughput mode
python scripts/load_example.py --base http://localhost:44317 --total 420 --workers 28

# Realistic paced mode (useful for observing Logs Live mode)
python scripts/load_example.py --base http://localhost:44317 --mode realistic --rps 3 --jitter-ms 250 --total 180 --workers 8
```

## RUM (Client-side)

Embed in your HTML:

```html
<script src="http://localhost:44317/static/rum.js"></script>
<script>
  SOBS.init({
    endpoint: 'http://localhost:44317/v1/rum',
    appName: 'my-app'
  });
</script>
```

## OTLP Endpoint Reference

| Endpoint         | Method | Description                        |
|------------------|--------|------------------------------------|
| `/v1/logs`       | POST   | OTLP logs (JSON or protobuf)       |
| `/v1/traces`     | POST   | OTLP traces (JSON or protobuf)     |
| `/v1/metrics`    | POST   | OTLP metrics (JSON or protobuf, typed metric tables + anomaly views) |
| `/v1/rum`        | POST   | RUM events (JSON array)            |
| `/v1/errors`     | POST   | Direct error submission            |
| `/v1/ai`         | POST   | AI/LLM call transparency           |
| `/health`        | GET    | Health check                       |

## Authentication

Set `SOBS_API_KEY` environment variable to enable API key auth.
Send key in `X-API-Key` header or `?api_key=` query parameter.
