"""
SOBS Prometheus & OTEL metrics examples.

Demonstrates two integration modes:

  1. --mode push  (default)
     Sends gauge, counter, and histogram metrics directly to SOBS using the
     OpenTelemetry Python SDK over OTLP/HTTP.  No scrape endpoint required.

  2. --mode expose
     Runs a Prometheus exposition HTTP server on --port (default 8000).
     Intended to be scraped by the OpenTelemetry Collector (see
     otel-collector-config.yaml) which then forwards data to SOBS.

Install:
    pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http prometheus_client

Run SOBS first:
    docker run -p 44317:4317 ghcr.io/abartrim/sobs:latest

Push metrics directly to SOBS:
    python python_metrics_example.py --mode push

Expose a Prometheus /metrics endpoint (for collector scraping):
    python python_metrics_example.py --mode expose --port 8000
"""

import argparse
import math
import random
import time

SOBS_ENDPOINT = "http://localhost:44317"
SERVICE_NAME = "prometheus-demo"


# ---------------------------------------------------------------------------
# Mode 1 – Direct OTLP push via OpenTelemetry Python SDK
# ---------------------------------------------------------------------------


def run_push_mode(sobs_endpoint: str, iterations: int = 10, interval: float = 2.0) -> None:
    """Push gauge, counter, and histogram metrics directly to SOBS via OTLP/HTTP."""

    from opentelemetry import metrics
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
    from opentelemetry.sdk.resources import Resource

    resource = Resource.create({"service.name": SERVICE_NAME})

    exporter = OTLPMetricExporter(endpoint=f"{sobs_endpoint}/v1/metrics")
    reader = PeriodicExportingMetricReader(exporter, export_interval_millis=int(interval * 1000))
    provider = MeterProvider(resource=resource, metric_readers=[reader])
    metrics.set_meter_provider(provider)

    meter = metrics.get_meter(__name__)

    # ---- Gauge – CPU utilisation (observable) ----
    cpu_usage: list[float] = [0.0]

    def observe_cpu(_options: metrics.CallbackOptions):  # type: ignore[name-defined]
        cpu_usage[0] = random.uniform(10.0, 90.0)
        yield metrics.Observation(cpu_usage[0], {"core": "0"})

    meter.create_observable_gauge(
        name="system.cpu.utilization",
        callbacks=[observe_cpu],
        description="Simulated CPU utilisation",
        unit="%",
    )

    # ---- Counter – HTTP request count ----
    request_counter = meter.create_counter(
        name="http.server.requests",
        description="Total HTTP requests handled",
        unit="1",
    )

    # ---- Histogram – request latency ----
    latency_histogram = meter.create_histogram(
        name="http.server.request_duration",
        description="HTTP request duration",
        unit="ms",
    )

    print(f"[push] Sending metrics to {sobs_endpoint} for {iterations} iterations …")
    for i in range(iterations):
        endpoint = random.choice(["/api/users", "/api/orders", "/health"])
        status = random.choice(["200", "200", "200", "404", "500"])

        request_counter.add(1, {"http.route": endpoint, "http.status_code": status})

        # Simulate log-normal latency distribution
        latency = math.exp(random.gauss(mu=4.5, sigma=0.5))  # ~90 ms median
        latency_histogram.record(latency, {"http.route": endpoint})

        print(f"  [{i + 1}/{iterations}] route={endpoint} status={status} latency={latency:.1f}ms")
        time.sleep(interval)

    provider.shutdown()
    print(f"Done – open {sobs_endpoint}/metrics in your browser.")


# ---------------------------------------------------------------------------
# Mode 2 – Expose Prometheus /metrics endpoint for collector scraping
# ---------------------------------------------------------------------------


def run_expose_mode(port: int, duration: float = 300.0) -> None:
    """
    Expose synthetic metrics on a Prometheus /metrics HTTP endpoint.

    The OpenTelemetry Collector (see otel-collector-config.yaml) scrapes this
    endpoint and forwards the data to SOBS via OTLP.
    """

    from prometheus_client import (
        Counter,
        Gauge,
        Histogram,
        start_http_server,
    )

    # ---- Define metrics ----
    cpu_gauge = Gauge(
        "system_cpu_utilization_percent",
        "Simulated CPU utilisation",
        ["core"],
    )
    request_counter = Counter(
        "http_server_requests_total",
        "Total HTTP requests handled",
        ["route", "status_code"],
    )
    latency_histogram = Histogram(
        "http_server_request_duration_ms",
        "HTTP request duration in milliseconds",
        ["route"],
        buckets=[10, 25, 50, 100, 250, 500, 1000, 2500],
    )

    start_http_server(port)
    print(f"[expose] Prometheus /metrics endpoint running on :{port}")
    print(f"  Scrape with: curl http://localhost:{port}/metrics")
    print("  Or let the OTel Collector forward data to SOBS (see otel-collector-config.yaml).")
    print("  Ctrl-C to stop.\n")

    deadline = time.time() + duration
    while time.time() < deadline:
        # Update CPU gauge for each simulated core
        for core in ["0", "1"]:
            cpu_gauge.labels(core=core).set(random.uniform(5.0, 95.0))

        # Simulate HTTP traffic
        for _ in range(random.randint(1, 5)):
            route = random.choice(["/api/users", "/api/orders", "/health"])
            status = random.choice(["200", "200", "200", "404", "500"])
            latency = math.exp(random.gauss(mu=4.5, sigma=0.5))

            request_counter.labels(route=route, status_code=status).inc()
            latency_histogram.labels(route=route).observe(latency)

        time.sleep(5)


# ---------------------------------------------------------------------------
# CLI entry-point
# ---------------------------------------------------------------------------


def main() -> None:
    parser = argparse.ArgumentParser(description="SOBS Prometheus / OTEL metrics demo")
    parser.add_argument(
        "--mode",
        choices=["push", "expose"],
        default="push",
        help="push: direct OTLP push to SOBS | expose: Prometheus /metrics endpoint",
    )
    parser.add_argument(
        "--endpoint",
        default=SOBS_ENDPOINT,
        help="SOBS base URL (push mode only)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=8000,
        help="HTTP port for the Prometheus /metrics endpoint (expose mode only)",
    )
    parser.add_argument(
        "--iterations",
        type=int,
        default=10,
        help="Number of metric emission cycles (push mode only)",
    )
    parser.add_argument(
        "--interval",
        type=float,
        default=2.0,
        help="Seconds between cycles (push mode only)",
    )
    parser.add_argument(
        "--duration",
        type=float,
        default=300.0,
        help="Seconds to run the exposition server (expose mode only)",
    )
    args = parser.parse_args()

    if args.mode == "push":
        run_push_mode(args.endpoint, args.iterations, args.interval)
    else:
        run_expose_mode(args.port, args.duration)


if __name__ == "__main__":
    main()
