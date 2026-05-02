"""
OpenTelemetry SDK setup for Sobs self-telemetry.

Initialises TracerProvider and MeterProvider based on configuration.
All imports are guarded so this module is safe to use even when
opentelemetry packages are not installed.
"""

import logging

from .config import (
    get_environment,
    get_exporter_type,
    get_otlp_endpoint,
    get_sample_rate,
    get_service_name,
    telemetry_enabled,
)

_log = logging.getLogger("sobs.telemetry")

_APP_INSTRUMENTED_ATTR = "_sobs_telemetry_instrumented"
_APP_INSTRUMENTATION_KIND_ATTR = "_sobs_telemetry_instrumentation_kind"
_APP_ORIGINAL_ASGI_ATTR = "_sobs_telemetry_original_asgi_app"

# Module-level flag indicating whether SDK was successfully initialised.
_sdk_initialised = False

# Cached tracer / meter (set when SDK is active).
_tracer = None
_meter = None
_tracer_provider = None


def is_sdk_initialised() -> bool:
    return _sdk_initialised


def configure_telemetry(app=None) -> None:  # noqa: ANN001
    """Initialise the OpenTelemetry SDK.

    Safe to call even when telemetry packages are not installed or when
    telemetry is disabled – in those cases this is a no-op.

    Parameters
    ----------
    app:
        Optional Quart/Flask application object (not used directly but
        kept for forward-compatibility with instrumentation helpers).
    """
    global _sdk_initialised

    if not telemetry_enabled():
        _reset_runtime_state(app=app)
        _log.info("Sobs telemetry disabled; using no-op telemetry.")
        return

    exporter_type = get_exporter_type()
    if exporter_type == "none":
        _reset_runtime_state(app=app)
        _log.info("Sobs telemetry enabled but SOBS_TELEMETRY_EXPORTER=none; using no-op telemetry.")
        return

    if _sdk_initialised:
        if app is not None:
            _instrument_app(app, _tracer_provider)
        return

    try:
        _setup_sdk(exporter_type, app=app)
        _sdk_initialised = True
    except Exception as exc:  # noqa: BLE001
        _reset_runtime_state(app=app)
        _log.warning(
            "Sobs telemetry configuration invalid; continuing with no-op telemetry. Error: %s",
            exc,
        )


def _reset_runtime_state(app=None) -> None:  # noqa: ANN001
    global _sdk_initialised

    _sdk_initialised = False

    if app is None or not getattr(app, _APP_INSTRUMENTED_ATTR, False):
        return

    instrumentation_kind = getattr(app, _APP_INSTRUMENTATION_KIND_ATTR, "")
    if instrumentation_kind == "asgi" and hasattr(app, _APP_ORIGINAL_ASGI_ATTR):
        app.asgi_app = getattr(app, _APP_ORIGINAL_ASGI_ATTR)
    elif instrumentation_kind == "flask":
        try:
            from opentelemetry.instrumentation.flask import FlaskInstrumentor

            FlaskInstrumentor().uninstrument_app(app)
        except Exception:  # noqa: BLE001
            pass

    for attr_name in (_APP_INSTRUMENTED_ATTR, _APP_INSTRUMENTATION_KIND_ATTR, _APP_ORIGINAL_ASGI_ATTR):
        if hasattr(app, attr_name):
            delattr(app, attr_name)


def _setup_sdk(exporter_type: str, app=None) -> None:  # noqa: ANN001
    """Internal: build and register TracerProvider + MeterProvider."""
    global _tracer, _tracer_provider

    from opentelemetry import trace as otel_trace
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.sampling import TraceIdRatioBased

    service_name = get_service_name()
    environment = get_environment()
    sample_rate = get_sample_rate()

    resource = Resource.create(
        {
            "service.name": service_name,
            "deployment.environment": environment,
        }
    )

    sampler = TraceIdRatioBased(sample_rate) if sample_rate < 1.0 else None

    tracer_provider_kwargs: dict = {"resource": resource}
    if sampler is not None:
        tracer_provider_kwargs["sampler"] = sampler

    tracer_provider = TracerProvider(**tracer_provider_kwargs)

    # ---- Span exporters ----
    if exporter_type == "console":
        _add_console_span_exporter(tracer_provider)
        _log.info("Sobs telemetry enabled with console exporter.")
    elif exporter_type == "otlp":
        endpoint = get_otlp_endpoint()
        if not endpoint:
            raise ValueError("SOBS_TELEMETRY_EXPORTER=otlp but SOBS_TELEMETRY_OTLP_ENDPOINT is not set")
        _add_otlp_span_exporter(tracer_provider, endpoint)
        _log.info("Sobs telemetry enabled with OTLP exporter (endpoint=%s).", endpoint)

    otel_trace.set_tracer_provider(tracer_provider)
    _tracer_provider = tracer_provider
    _tracer = tracer_provider.get_tracer(service_name)

    # ---- Meter provider ----
    try:
        _setup_meter_provider(resource, exporter_type)
    except Exception as exc:  # noqa: BLE001
        _log.warning("Sobs metrics setup failed (traces still active): %s", exc)

    # ---- Flask/ASGI auto-instrumentation ----
    if app is not None:
        _instrument_app(app, tracer_provider)


def _add_console_span_exporter(tracer_provider) -> None:  # noqa: ANN001
    from opentelemetry.sdk.trace.export import BatchSpanProcessor, ConsoleSpanExporter

    tracer_provider.add_span_processor(BatchSpanProcessor(ConsoleSpanExporter()))


def _add_otlp_span_exporter(tracer_provider, endpoint: str) -> None:  # noqa: ANN001
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    try:
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import (
            OTLPSpanExporter,
        )

        tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint)))
    except ImportError:
        try:
            from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter as OTLPSpanExporterHTTP

            tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporterHTTP(endpoint=endpoint)))
        except ImportError as exc:
            raise ImportError(
                "OTLP exporter requires 'opentelemetry-exporter-otlp'. "
                "Install with: pip install opentelemetry-exporter-otlp"
            ) from exc


def _setup_meter_provider(resource, exporter_type: str) -> None:  # noqa: ANN001
    from opentelemetry import metrics as otel_metrics
    from opentelemetry.sdk.metrics import MeterProvider

    global _meter

    readers = []

    if exporter_type == "console":
        try:
            from opentelemetry.sdk.metrics.export import (
                ConsoleMetricExporter,
                PeriodicExportingMetricReader,
            )

            readers.append(PeriodicExportingMetricReader(ConsoleMetricExporter()))
        except Exception:  # noqa: BLE001
            pass
    elif exporter_type == "otlp":
        endpoint = get_otlp_endpoint()
        if endpoint:
            try:
                from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import (
                    OTLPMetricExporter,
                )
                from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader

                readers.append(PeriodicExportingMetricReader(OTLPMetricExporter(endpoint=endpoint)))
            except ImportError:
                try:
                    from opentelemetry.exporter.otlp.proto.http.metric_exporter import (
                        OTLPMetricExporter as OTLPMetricExporterHTTP,
                    )
                    from opentelemetry.sdk.metrics.export import (
                        PeriodicExportingMetricReader,
                    )

                    readers.append(PeriodicExportingMetricReader(OTLPMetricExporterHTTP(endpoint=endpoint)))
                except ImportError:
                    pass

    meter_provider = MeterProvider(resource=resource, metric_readers=readers)
    otel_metrics.set_meter_provider(meter_provider)
    _meter = meter_provider.get_meter(get_service_name())


def _instrument_app(app, tracer_provider) -> None:  # noqa: ANN001
    """Apply ASGI/request auto-instrumentation to the Quart app."""
    if getattr(app, _APP_INSTRUMENTED_ATTR, False):
        return

    # Quart uses ASGI; try opentelemetry-instrumentation-asgi first.
    excluded_urls = "/health,/healthz,/static,/favicon.ico"
    try:
        from opentelemetry.instrumentation.asgi import OpenTelemetryMiddleware

        setattr(app, _APP_ORIGINAL_ASGI_ATTR, app.asgi_app)
        app.asgi_app = OpenTelemetryMiddleware(
            app.asgi_app,
            tracer_provider=tracer_provider,
            excluded_urls=excluded_urls,
        )
        setattr(app, _APP_INSTRUMENTED_ATTR, True)
        setattr(app, _APP_INSTRUMENTATION_KIND_ATTR, "asgi")
        _log.debug("Sobs: ASGI OpenTelemetry middleware applied.")
        return
    except ImportError:
        pass

    # Fallback: try Flask instrumentation (works for Flask-compatible apps).
    try:
        from opentelemetry.instrumentation.flask import FlaskInstrumentor

        FlaskInstrumentor().instrument_app(app, tracer_provider=tracer_provider)
        setattr(app, _APP_INSTRUMENTED_ATTR, True)
        setattr(app, _APP_INSTRUMENTATION_KIND_ATTR, "flask")
        _log.debug("Sobs: Flask OpenTelemetry instrumentation applied.")
    except (ImportError, Exception) as exc:  # noqa: BLE001
        _log.debug("Sobs: auto-instrumentation unavailable: %s", exc)


def get_tracer(name: str = "sobs"):
    """Return the active tracer, or a no-op tracer when telemetry is disabled."""
    if not _sdk_initialised:
        return _NoOpTracer()
    if _tracer is not None:
        return _tracer
    try:
        from opentelemetry import trace

        return trace.get_tracer(name)
    except ImportError:
        return _NoOpTracer()


def get_meter(name: str = "sobs"):
    """Return the active meter, or a no-op meter when telemetry is disabled."""
    if not _sdk_initialised:
        return _NoOpMeter()
    if _meter is not None:
        return _meter
    try:
        from opentelemetry import metrics

        return metrics.get_meter(name)
    except ImportError:
        return _NoOpMeter()


# ---------------------------------------------------------------------------
# Minimal no-op stubs (used when opentelemetry is not installed)
# ---------------------------------------------------------------------------


class _NoOpSpan:
    def set_attribute(self, key: str, value: object) -> None:
        pass

    def record_exception(self, exc: BaseException) -> None:
        pass

    def set_status(self, status: object) -> None:
        pass

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False


class _NoOpTracer:
    def start_as_current_span(self, name: str, **kwargs):
        return _NoOpSpan()

    def start_span(self, name: str, **kwargs):
        return _NoOpSpan()


class _NoOpCounter:
    def add(self, amount: float, attributes: "dict | None" = None) -> None:
        pass


class _NoOpHistogram:
    def record(self, amount: float, attributes: "dict | None" = None) -> None:
        pass


class _NoOpMeter:
    def create_counter(self, name: str, **kwargs) -> _NoOpCounter:
        return _NoOpCounter()

    def create_histogram(self, name: str, **kwargs) -> _NoOpHistogram:
        return _NoOpHistogram()

    def create_up_down_counter(self, name: str, **kwargs) -> _NoOpCounter:
        return _NoOpCounter()
