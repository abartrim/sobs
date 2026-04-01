"""
SOBS Python examples – sending telemetry via OpenTelemetry SDK.

Install:
    pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http

Run SOBS first:
    docker run -p 4317:4317 sobs:latest
"""

import logging
import time

# ---------------------------------------------------------------------------
# 1. OpenTelemetry Logs
# ---------------------------------------------------------------------------
from opentelemetry import trace
from opentelemetry._logs import set_logger_provider
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

SOBS_ENDPOINT = "http://localhost:44317"
SERVICE_NAME = "my-python-app"

resource = Resource.create({"service.name": SERVICE_NAME})

# ---- Traces ----
tracer_provider = TracerProvider(resource=resource)
tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=f"{SOBS_ENDPOINT}/v1/traces")))
trace.set_tracer_provider(tracer_provider)
tracer = trace.get_tracer(__name__)

# ---- Logs ----
logger_provider = LoggerProvider(resource=resource)
logger_provider.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter(endpoint=f"{SOBS_ENDPOINT}/v1/logs")))
set_logger_provider(logger_provider)

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("my-app")
handler = LoggingHandler(level=logging.DEBUG, logger_provider=logger_provider)
logger.addHandler(handler)


# ---------------------------------------------------------------------------
# Example usage
# ---------------------------------------------------------------------------
def example_request(user_id: str):
    with tracer.start_as_current_span("handle_request") as span:
        span.set_attribute("user.id", user_id)
        span.set_attribute("http.method", "GET")
        span.set_attribute("http.url", "/api/users")

        logger.info("Handling request for user %s", user_id)
        time.sleep(0.05)

        # Simulate a sub-operation
        with tracer.start_as_current_span("db_query"):
            logger.debug("Querying database")
            time.sleep(0.02)

        logger.info("Request completed")


if __name__ == "__main__":
    example_request("user-123")
    # Flush before exit
    tracer_provider.shutdown()
    logger_provider.shutdown()
    print("Done – check SOBS at http://localhost:44317")
