"""
Integration tests for SOBS – Simple Observe.

Each test class simulates a different example (curl, Python OTel SDK, Flask
auto-instrumentation, Node.js Express) posting telemetry to a live SOBS
server, then verifies the data is visible in every UI page.  A final class
captures full-page Playwright screenshots for visual-regression checking.

Run standalone:
    pytest tests/test_integration.py -v

Run as part of the full suite (unit tests excluded from integration marker):
    pytest tests/ -v -m "not integration"   # unit tests only
    pytest tests/test_integration.py -v     # integration tests only
"""

import os
import subprocess
import sys
import tempfile
import time
from typing import Any

import pytest
import requests
from playwright.sync_api import Page, expect

# ---------------------------------------------------------------------------
# Screenshots output directory
# ---------------------------------------------------------------------------
SCREENSHOTS_DIR = os.path.join(os.path.dirname(__file__), "screenshots")

# ---------------------------------------------------------------------------
# Live-server configuration
# ---------------------------------------------------------------------------
SERVER_HOST = "127.0.0.1"
SERVER_PORT = 15317
BASE_URL = f"http://{SERVER_HOST}:{SERVER_PORT}"

# ---------------------------------------------------------------------------
# Ensure a data directory is configured before importing the app so these
# tests can run standalone *or* alongside tests/test_app.py in one session.
# ---------------------------------------------------------------------------
if "SOBS_DATA_DIR" not in os.environ:
    os.environ["SOBS_DATA_DIR"] = tempfile.mkdtemp()

# ---------------------------------------------------------------------------
# Session-scoped live-server fixture
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session", autouse=True)
def live_server():
    """Start a live SOBS server in a subprocess for the session."""
    os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    data_dir = tempfile.mkdtemp(prefix="sobs-integration-")

    env = os.environ.copy()
    env["PORT"] = str(SERVER_PORT)
    env["SOBS_DATA_DIR"] = data_dir
    env["SOBS_ENABLE_FIRST_RUN_TOUR"] = "0"

    proc = subprocess.Popen(
        [sys.executable, "app.py"],
        cwd=repo_root,
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    # Wait up to 10 s for the server to become ready.
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            pytest.fail("Live SOBS server process exited before becoming ready")
        try:
            resp = requests.get(f"{BASE_URL}/health", timeout=1)
            if resp.status_code == 200:
                break
        except Exception:
            pass
        time.sleep(0.2)
    else:
        proc.terminate()
        pytest.fail("Live SOBS server did not start within 10 seconds")

    yield BASE_URL

    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()


# ---------------------------------------------------------------------------
# Payload-builder helpers
# ---------------------------------------------------------------------------


def _ts_ns() -> str:
    """Current time as a nanosecond UNIX timestamp string."""
    return str(int(time.time() * 1_000_000_000))


def _otlp_log_payload(message: str, service: str, level: str = "INFO") -> dict:
    return {
        "resourceLogs": [
            {
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                "scopeLogs": [
                    {
                        "logRecords": [
                            {
                                "timeUnixNano": _ts_ns(),
                                "severityText": level,
                                "body": {"stringValue": message},
                            }
                        ]
                    }
                ],
            }
        ]
    }


def _otlp_trace_payload(service: str, spans: list) -> dict:
    return {
        "resourceSpans": [
            {
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                "scopeSpans": [{"spans": spans}],
            }
        ]
    }


def _span(
    name: str,
    trace_id: str,
    span_id: str,
    parent_span_id: str = "",
    status_code: int = 1,
    attrs: list[Any] | None = None,
) -> dict:
    start_ns = int(time.time() * 1_000_000_000)
    s: dict = {
        "traceId": trace_id,
        "spanId": span_id,
        "parentSpanId": parent_span_id,
        "name": name,
        "startTimeUnixNano": str(start_ns),
        "endTimeUnixNano": str(start_ns + 50_000_000),
        "status": {"code": status_code},
    }
    if attrs:
        s["attributes"] = attrs
    return s


# ---------------------------------------------------------------------------
# Example simulations
# ---------------------------------------------------------------------------


@pytest.mark.integration
class TestCurlExamples:
    """Simulate the curl_examples.sh script posting telemetry data to SOBS."""

    def test_curl_log(self, live_server):
        """curl example 1 – POST a log in OTLP/JSON format."""
        r = requests.post(
            f"{live_server}/v1/logs",
            json=_otlp_log_payload("Hello from curl!", "curl-demo"),
        )
        assert r.status_code == 200
        assert r.json()["accepted"] == 1

    def test_curl_trace(self, live_server):
        """curl example 2 – POST a trace span."""
        r = requests.post(
            f"{live_server}/v1/traces",
            json=_otlp_trace_payload(
                "curl-demo",
                [_span("curl-span", "abcdef1234567890abcdef1234567890", "1234567890abcdef")],
            ),
        )
        assert r.status_code == 200
        assert r.json()["accepted"] == 1

    def test_curl_error(self, live_server):
        """curl example 3 – POST an error."""
        r = requests.post(
            f"{live_server}/v1/errors",
            json={
                "service": "curl-demo",
                "type": "RuntimeError",
                "message": "Oops, something went wrong",
                "stack": "RuntimeError: Oops\n  at main (script.sh:42)",
            },
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True

    def test_curl_rum(self, live_server):
        """curl example 4 – POST a RUM pageview event."""
        r = requests.post(
            f"{live_server}/v1/rum",
            json=[
                {
                    "type": "pageview",
                    "timestamp": "2024-01-01T00:00:00Z",
                    "sessionId": "sess-abc123",
                    "url": "https://example.com/home",
                    "title": "Home Page",
                }
            ],
        )
        assert r.status_code == 200
        assert r.json()["accepted"] == 1

    def test_curl_ai(self, live_server):
        """curl example 5 – POST an AI transparency event."""
        r = requests.post(
            f"{live_server}/v1/ai",
            json={
                "service": "curl-demo",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": "What is the capital of France?",
                "response": "Paris.",
                "tokens_in": 10,
                "tokens_out": 2,
                "duration_ms": 250,
            },
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True


@pytest.mark.integration
class TestPythonOtelExample:
    """Simulate examples/python/otel_example.py sending data via the OTLP HTTP API."""

    SERVICE = "my-python-app"
    TRACE_ID = "aabbccdd11223344aabbccdd11223344"

    def test_otel_traces(self, live_server):
        """Send a handle_request span and a db_query child span."""
        r = requests.post(
            f"{live_server}/v1/traces",
            json=_otlp_trace_payload(
                self.SERVICE,
                [
                    _span(
                        "handle_request",
                        self.TRACE_ID,
                        "cafebabe12345678",
                        attrs=[
                            {"key": "user.id", "value": {"stringValue": "user-123"}},
                            {"key": "http.method", "value": {"stringValue": "GET"}},
                            {"key": "http.url", "value": {"stringValue": "/api/users"}},
                        ],
                    ),
                    _span("db_query", self.TRACE_ID, "deadbeef87654321", "cafebabe12345678"),
                ],
            ),
        )
        assert r.status_code == 200
        assert r.json()["accepted"] == 2

    def test_otel_logs(self, live_server):
        """Send the three log messages produced by otel_example.py."""
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": self.SERVICE}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": _ts_ns(),
                                    "severityText": "INFO",
                                    "body": {"stringValue": "Handling request for user user-123"},
                                    "traceId": self.TRACE_ID,
                                    "spanId": "cafebabe12345678",
                                },
                                {
                                    "timeUnixNano": _ts_ns(),
                                    "severityText": "DEBUG",
                                    "body": {"stringValue": "Querying database"},
                                },
                                {
                                    "timeUnixNano": _ts_ns(),
                                    "severityText": "INFO",
                                    "body": {"stringValue": "Request completed"},
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        r = requests.post(f"{live_server}/v1/logs", json=payload)
        assert r.status_code == 200
        assert r.json()["accepted"] == 3


@pytest.mark.integration
class TestFlaskExample:
    """Simulate examples/python/flask_example.py routes posting data to SOBS."""

    SERVICE = "flask-demo"

    def test_flask_index_log_and_trace(self, live_server):
        """Simulate the / route: auto-instrumented span + INFO log."""
        r = requests.post(
            f"{live_server}/v1/traces",
            json=_otlp_trace_payload(
                self.SERVICE,
                [
                    _span(
                        "GET /",
                        "f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6",
                        "a1b2c3d4e5f60001",
                        attrs=[
                            {"key": "http.method", "value": {"stringValue": "GET"}},
                            {"key": "http.route", "value": {"stringValue": "/"}},
                            {"key": "http.status_code", "value": {"intValue": 200}},
                        ],
                    )
                ],
            ),
        )
        assert r.status_code == 200
        r = requests.post(
            f"{live_server}/v1/logs",
            json=_otlp_log_payload("Root endpoint called", self.SERVICE),
        )
        assert r.status_code == 200

    def test_flask_error_route(self, live_server):
        """Simulate the /error route: ZeroDivisionError sent to SOBS."""
        r = requests.post(
            f"{live_server}/v1/errors",
            json={
                "service": self.SERVICE,
                "type": "ZeroDivisionError",
                "message": "division by zero",
                "stack": "ZeroDivisionError('division by zero')",
            },
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True

    def test_flask_ai_demo_route(self, live_server):
        """Simulate the /ai-demo route: LLM event sent to SOBS."""
        r = requests.post(
            f"{live_server}/v1/ai",
            json={
                "service": self.SERVICE,
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": "Summarise the user's request in one sentence.",
                "response": "The user wants a summary of their request.",
                "tokens_in": 12,
                "tokens_out": 10,
                "duration_ms": 100,
            },
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True


@pytest.mark.integration
class TestNodeJsExample:
    """Simulate examples/nodejs/example.js routes posting data to SOBS."""

    SERVICE = "node-demo"

    def test_nodejs_root_trace(self, live_server):
        """Simulate the Express GET / route generating a trace span."""
        r = requests.post(
            f"{live_server}/v1/traces",
            json=_otlp_trace_payload(
                self.SERVICE,
                [
                    _span(
                        "GET /",
                        "01020304050607080910111213141516",
                        "1122334455667700",
                        attrs=[
                            {"key": "http.method", "value": {"stringValue": "GET"}},
                            {"key": "http.route", "value": {"stringValue": "/"}},
                            {"key": "http.status_code", "value": {"intValue": 200}},
                        ],
                    )
                ],
            ),
        )
        assert r.status_code == 200
        assert r.json()["accepted"] == 1

    def test_nodejs_error_route(self, live_server):
        """Simulate the Express GET /error route sending an error to SOBS."""
        r = requests.post(
            f"{live_server}/v1/errors",
            json={
                "service": self.SERVICE,
                "type": "Error",
                "message": "Something went wrong",
                "stack": "Error: Something went wrong\n    at /app/example.js:54:11",
            },
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True

    def test_nodejs_ai_demo_route(self, live_server):
        """Simulate the Express GET /ai-demo route sending an AI event to SOBS."""
        r = requests.post(
            f"{live_server}/v1/ai",
            json={
                "service": self.SERVICE,
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": 'Translate "hello world" to Spanish.',
                "response": '"hola mundo"',
                "tokens_in": 8,
                "tokens_out": 3,
                "duration_ms": 50,
            },
        )
        assert r.status_code == 200
        assert r.json()["ok"] is True


# ---------------------------------------------------------------------------
# Data-visibility tests (run after all example tests have posted data)
# ---------------------------------------------------------------------------


@pytest.mark.integration
class TestDataVisibleInUI:
    """Verify that telemetry posted by the example simulations is visible in the UI."""

    def test_dashboard_loads(self, live_server):
        r = requests.get(f"{live_server}/")
        assert r.status_code == 200
        assert "Summary" in r.text
        assert "SOBS" in r.text

    def test_logs_page_shows_curl_demo_data(self, live_server):
        """The logs page must display the log posted by the curl example."""
        r = requests.get(f"{live_server}/logs?q=Hello+from+curl")
        assert r.status_code == 200
        assert "Hello from curl!" in r.text

    def test_logs_page_shows_otel_example_data(self, live_server):
        """The logs page must display logs from the Python OTel example."""
        r = requests.get(f"{live_server}/logs?q=Handling+request+for+user")
        assert r.status_code == 200
        assert "Handling request for user" in r.text

    def test_traces_page_shows_example_data(self, live_server):
        """The traces page must display spans from at least one example service."""
        r = requests.get(f"{live_server}/traces")
        assert r.status_code == 200
        assert any(svc in r.text for svc in ["curl-demo", "my-python-app", "flask-demo", "node-demo"])

    def test_errors_page_shows_example_data(self, live_server):
        """The errors page must list errors posted by the examples."""
        r = requests.get(f"{live_server}/errors")
        assert r.status_code == 200
        assert any(svc in r.text for svc in ["curl-demo", "flask-demo", "node-demo"])

    def test_rum_page_shows_pageview(self, live_server):
        """The RUM page must display the pageview event from the curl example."""
        r = requests.get(f"{live_server}/rum")
        assert r.status_code == 200
        assert "https://example.com/home" in r.text

    def test_ai_page_shows_llm_events(self, live_server):
        """The AI page must list the LLM events posted by the examples."""
        r = requests.get(f"{live_server}/ai")
        assert r.status_code == 200
        assert "gpt-4o-mini" in r.text


# ---------------------------------------------------------------------------
# Screenshot tests (Playwright) – visual regression
# ---------------------------------------------------------------------------


@pytest.mark.integration
class TestScreenshots:
    """Capture full-page screenshots of every UI view for visual regression."""

    def _screenshot(self, page: Page, filename: str, url: str) -> None:
        page.goto(url)
        page.wait_for_load_state("networkidle")
        os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
        page.screenshot(path=os.path.join(SCREENSHOTS_DIR, filename), full_page=True)

    def test_screenshot_summary(self, page: Page, live_server):
        self._screenshot(page, "summary.png", f"{live_server}/")
        expect(page.get_by_role("heading", name="Summary")).to_be_visible()

    def test_screenshot_logs(self, page: Page, live_server):
        self._screenshot(page, "logs.png", f"{live_server}/logs")
        expect(page.get_by_role("heading", name="Logs")).to_be_visible()

    def test_screenshot_traces(self, page: Page, live_server):
        self._screenshot(page, "traces.png", f"{live_server}/traces")
        expect(page.get_by_role("heading", name="Traces")).to_be_visible()

    def test_screenshot_errors(self, page: Page, live_server):
        self._screenshot(page, "errors.png", f"{live_server}/errors")
        expect(page.get_by_role("heading", name="Errors")).to_be_visible()

    def test_screenshot_rum(self, page: Page, live_server):
        self._screenshot(page, "rum.png", f"{live_server}/rum")
        expect(page.get_by_role("heading", name="Real User Monitoring")).to_be_visible()

    def test_screenshot_ai(self, page: Page, live_server):
        self._screenshot(page, "ai.png", f"{live_server}/ai")
        expect(page.get_by_role("heading", name="AI Transparency")).to_be_visible()
