"""
SOBS Flask auto-instrumentation example.

Install:
    pip install flask opentelemetry-sdk \
                opentelemetry-instrumentation-flask \
                opentelemetry-exporter-otlp-proto-http \
                requests

Run SOBS first:
    docker run -p 44317:4317 sobs:latest

Then run this app:
    python flask_example.py
"""

import logging
import os
import time

import requests as http_requests
from flask import Flask, jsonify
from opentelemetry import trace
from opentelemetry._logs import set_logger_provider
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.flask import FlaskInstrumentor
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

SOBS_ENDPOINT = "http://localhost:44317"
SERVICE_NAME = "flask-demo"

resource = Resource.create({"service.name": SERVICE_NAME})

# ---- Traces ----
tracer_provider = TracerProvider(resource=resource)
tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=f"{SOBS_ENDPOINT}/v1/traces")))
trace.set_tracer_provider(tracer_provider)

# ---- Logs ----
logger_provider = LoggerProvider(resource=resource)
logger_provider.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter(endpoint=f"{SOBS_ENDPOINT}/v1/logs")))
set_logger_provider(logger_provider)

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(SERVICE_NAME)
logger.addHandler(LoggingHandler(logger_provider=logger_provider))

# ---- Flask app ----
app = Flask(__name__)
FlaskInstrumentor().instrument_app(app)


@app.route("/")
def index():
    logger.info("Root endpoint called")
    return jsonify({"status": "ok"})


@app.route("/error")
def trigger_error():
    try:
        result = 1 / 0
    except ZeroDivisionError as exc:
        # Send error directly to SOBS
        http_requests.post(
            f"{SOBS_ENDPOINT}/v1/errors",
            json={
                "service": SERVICE_NAME,
                "type": type(exc).__name__,
                "message": str(exc),
                "stack": repr(exc),
            },
        )
        return jsonify({"error": "division by zero"}), 500
    return jsonify({"result": result})


@app.route("/ai-demo")
def ai_demo():
    """Simulate an AI call and send it to SOBS for transparency."""
    start = time.monotonic()
    fake_prompt = "Summarise the user's request in one sentence."
    fake_response = "The user wants a summary of their request."
    time.sleep(0.1)  # simulate LLM latency
    http_requests.post(
        f"{SOBS_ENDPOINT}/v1/ai",
        json={
            "service": SERVICE_NAME,
            "provider": "openai",
            "model": "gpt-4o-mini",
            "prompt": fake_prompt,
            "response": fake_response,
            "tokens_in": 12,
            "tokens_out": 10,
            "duration_ms": (time.monotonic() - start) * 1000,
        },
    )
    return jsonify({"response": fake_response})


if __name__ == "__main__":
    app.run(port=5000, debug=os.environ.get("FLASK_DEBUG", "0") == "1")
