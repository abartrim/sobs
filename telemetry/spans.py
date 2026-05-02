"""
Span helpers for Sobs self-telemetry.

Provides a safe context-manager ``span(name, **attributes)`` and an async
decorator ``traced_view(span_name, **attributes)`` that are no-ops when
telemetry is disabled or OpenTelemetry packages are not installed.
"""

import functools
import inspect
import time
from contextlib import contextmanager
from typing import Any, Generator

from .setup import get_tracer


@contextmanager
def span(name: str, **attributes: Any) -> Generator[Any, None, None]:
    """Context manager that wraps a code block in an OpenTelemetry span.

    Safe to call even when telemetry is disabled – the block will execute
    normally without any tracing overhead.

    Example::

        with span("sobs.ingest.normalize", event_type="rum", event_count=3):
            normalize_event(event)

    Parameters
    ----------
    name:
        Span name (e.g. ``"sobs.ingest.request"``).
    **attributes:
        Key/value span attributes.  Values must be ``str``, ``int``, ``float``,
        or ``bool`` to be compatible with OpenTelemetry attribute requirements.
        Raw payloads, secrets, or PII must **never** be passed here.
    """
    tracer = get_tracer()
    started_at = time.perf_counter()
    try:
        with tracer.start_as_current_span(name) as current_span:
            for key, value in attributes.items():
                try:
                    current_span.set_attribute(key, value)
                except Exception:  # noqa: BLE001
                    pass
            try:
                yield current_span
            except Exception as exc:
                try:
                    current_span.record_exception(exc)
                    try:
                        from opentelemetry.trace import Status, StatusCode

                        current_span.set_status(Status(StatusCode.ERROR, str(exc)))
                    except ImportError:
                        pass
                except Exception:  # noqa: BLE001
                    pass
                raise
    except AttributeError:
        # Tracer stub doesn't support context-manager protocol; fall through.
        yield None
    finally:
        try:
            from .metrics import record_span_duration

            record_span_duration(name, (time.perf_counter() - started_at) * 1000.0, attributes)
        except Exception:  # noqa: BLE001
            pass


def traced_view(span_name: str, **attributes: Any):
    """Decorator that wraps an async view function in a telemetry span.

    Designed for Quart/Flask route handlers where wrapping the entire body in
    a ``with span(...)`` block would require re-indenting large functions.

    Example::

        @app.route("/errors")
        @traced_view("sobs.dashboard.query", **{"dashboard.name": "errors", "route": "/errors"})
        async def view_errors():
            ...
    """

    def decorator(func):
        if inspect.iscoroutinefunction(func):

            @functools.wraps(func)
            async def async_wrapper(*args, **kwargs):
                with span(span_name, **attributes):
                    return await func(*args, **kwargs)

            return async_wrapper
        else:

            @functools.wraps(func)
            def sync_wrapper(*args, **kwargs):
                with span(span_name, **attributes):
                    return func(*args, **kwargs)

            return sync_wrapper

    return decorator
