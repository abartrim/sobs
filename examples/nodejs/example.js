/**
 * SOBS Node.js / Express example with OpenTelemetry SDK.
 *
 * Install:
 *   npm install @opentelemetry/sdk-node \
 *               @opentelemetry/exporter-trace-otlp-http \
 *               @opentelemetry/exporter-logs-otlp-http \
 *               @opentelemetry/instrumentation-express \
 *               @opentelemetry/instrumentation-http \
 *               express axios
 *
 * Run SOBS first:
 *   docker run -p 44317:4317 sobs:latest
 *
 * Then:
 *   node example.js
 */

'use strict';

const { NodeSDK } = require('@opentelemetry/sdk-node');
const { OTLPTraceExporter } = require('@opentelemetry/exporter-trace-otlp-http');
const { OTLPLogExporter } = require('@opentelemetry/exporter-logs-otlp-http');
const { SimpleLogRecordProcessor } = require('@opentelemetry/sdk-logs');
const { HttpInstrumentation } = require('@opentelemetry/instrumentation-http');
const { ExpressInstrumentation } = require('@opentelemetry/instrumentation-express');
const { Resource } = require('@opentelemetry/resources');
const { ATTR_SERVICE_NAME } = require('@opentelemetry/semantic-conventions');

const SOBS_ENDPOINT = 'http://localhost:44317';
const SERVICE_NAME  = 'node-demo';

const sdk = new NodeSDK({
  resource: new Resource({ [ATTR_SERVICE_NAME]: SERVICE_NAME }),
  traceExporter: new OTLPTraceExporter({ url: `${SOBS_ENDPOINT}/v1/traces` }),
  logRecordProcessors: [
    new SimpleLogRecordProcessor(new OTLPLogExporter({ url: `${SOBS_ENDPOINT}/v1/logs` })),
  ],
  instrumentations: [new HttpInstrumentation(), new ExpressInstrumentation()],
});
sdk.start();

// ---- Express App ----
const express = require('express');
const axios   = require('axios');
const app     = express();

app.get('/', (req, res) => {
  res.json({ status: 'ok' });
});

app.get('/error', async (req, res) => {
  try {
    throw new Error('Something went wrong');
  } catch (err) {
    // Send error to SOBS
    await axios.post(`${SOBS_ENDPOINT}/v1/errors`, {
      service: SERVICE_NAME,
      type: err.name,
      message: err.message,
      stack: err.stack,
    }).catch(() => {});
    res.status(500).json({ error: err.message });
  }
});

app.get('/ai-demo', async (req, res) => {
  const start = Date.now();
  const prompt = 'Translate "hello world" to Spanish.';
  // ... real LLM call would go here ...
  const response = '"hola mundo"';
  await axios.post(`${SOBS_ENDPOINT}/v1/ai`, {
    service: SERVICE_NAME,
    provider: 'openai',
    model: 'gpt-4o-mini',
    prompt,
    response,
    tokens_in: 8,
    tokens_out: 3,
    duration_ms: Date.now() - start,
  }).catch(() => {});
  res.json({ response });
});

const PORT = 3000;
app.listen(PORT, () => {
  console.log(`Demo app listening on http://localhost:${PORT}`);
  console.log(`SOBS UI: http://localhost:44317`);
});

process.on('SIGTERM', () => sdk.shutdown().finally(() => process.exit(0)));
