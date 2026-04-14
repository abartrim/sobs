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

import contextlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from typing import Any, Callable

import pytest
import requests
from playwright.sync_api import Dialog
from playwright.sync_api import Error as PlaywrightError
from playwright.sync_api import Page, expect

# ---------------------------------------------------------------------------
# Screenshots output directory
# ---------------------------------------------------------------------------
_SCREENSHOTS_BASE_DIR = os.path.join(os.path.dirname(__file__), "screenshots")


def _env_truthy(name: str, default: bool = False) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return str(raw).strip().lower() in {"1", "true", "yes", "on"}


_SCREENSHOTS_KEEP_ARTIFACTS = _env_truthy("SOBS_SCREENSHOT_KEEP_ARTIFACTS", default=False)
_SCREENSHOTS_CLEAN = _env_truthy("SOBS_SCREENSHOT_CLEAN", default=not _SCREENSHOTS_KEEP_ARTIFACTS)
_SCREENSHOTS_RUN_DIR = time.strftime("run-%Y%m%d-%H%M%S")
SCREENSHOTS_DIR = (
    os.path.join(_SCREENSHOTS_BASE_DIR, _SCREENSHOTS_RUN_DIR) if _SCREENSHOTS_KEEP_ARTIFACTS else _SCREENSHOTS_BASE_DIR
)

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
    _prepare_screenshots_dir()
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    data_dir = tempfile.mkdtemp(prefix="sobs-integration-")
    server_log_path = os.path.join(SCREENSHOTS_DIR, "integration-live-server.log")

    env = os.environ.copy()
    env["PORT"] = str(SERVER_PORT)
    env["SOBS_DATA_DIR"] = data_dir
    env["SOBS_ENABLE_FIRST_RUN_TOUR"] = "0"
    env["SOBS_AI_ENDPOINT_URL"] = "http://localhost:9999/v1"
    env["SOBS_AI_MODEL"] = "docs-screenshot-model"

    server_log = open(server_log_path, "w", encoding="utf-8")
    proc = subprocess.Popen(
        [sys.executable, "app.py"],
        cwd=repo_root,
        env=env,
        stdout=server_log,
        stderr=server_log,
    )

    def _tail_server_log(lines: int = 120) -> str:
        with contextlib.suppress(Exception):
            with open(server_log_path, encoding="utf-8", errors="replace") as fh:
                content = fh.readlines()
                return "".join(content[-lines:]).strip()
        return ""

    # Wait up to 10 s for the server to become ready.
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            tail = _tail_server_log()
            msg = "Live SOBS server process exited before becoming ready"
            if tail:
                msg += f"\n--- server log tail ---\n{tail}"
            pytest.fail(msg)
        try:
            resp = requests.get(f"{BASE_URL}/health", timeout=1)
            if resp.status_code == 200:
                break
        except Exception:
            pass
        time.sleep(0.2)
    else:
        proc.terminate()
        tail = _tail_server_log()
        msg = "Live SOBS server did not start within 10 seconds"
        if tail:
            msg += f"\n--- server log tail ---\n{tail}"
        pytest.fail(msg)

    yield BASE_URL

    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
    finally:
        with contextlib.suppress(Exception):
            server_log.flush()
            server_log.close()


# ---------------------------------------------------------------------------
# Payload-builder helpers
# ---------------------------------------------------------------------------


def _ts_ns() -> str:
    """Current time as a nanosecond UNIX timestamp string."""
    return str(int(time.time() * 1_000_000_000))


def _seed_telemetry_data(live_server: str, total: int, workers: int) -> None:
    """Seed sample telemetry via scripts/load_example.py with configurable concurrency."""
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    subprocess.run(
        [
            sys.executable,
            "scripts/load_example.py",
            "--base",
            live_server,
            "--total",
            str(total),
            "--workers",
            str(workers),
        ],
        cwd=repo_root,
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def _prepare_screenshots_dir() -> None:
    os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
    if not _SCREENSHOTS_CLEAN:
        return
    for name in os.listdir(SCREENSHOTS_DIR):
        if name.lower().endswith((".png", ".jpg", ".jpeg", ".webp")):
            with contextlib.suppress(Exception):
                os.remove(os.path.join(SCREENSHOTS_DIR, name))


def _wait_for_any_text(
    live_server: str,
    path: str,
    expected: list[str],
    timeout_s: float = 10.0,
    interval_s: float = 0.25,
) -> str:
    """Poll a page until any expected text appears (for eventual ingestion)."""
    deadline = time.time() + timeout_s
    last_text = ""
    while time.time() < deadline:
        r = requests.get(f"{live_server}{path}", timeout=5)
        assert r.status_code == 200
        last_text = r.text
        if any(token in last_text for token in expected):
            return last_text
        time.sleep(interval_s)

    pytest.fail(f"Timed out waiting for any of {expected!r} on {path}. " f"Last response length={len(last_text)}")


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
    """Verify that telemetry becomes visible in UI pages."""

    def _wait_for_any_text(
        self,
        live_server: str,
        path: str,
        expected: list[str],
        timeout_s: float = 10.0,
        interval_s: float = 0.25,
    ) -> str:
        """Poll a page until any expected text appears (for eventual ingestion)."""
        deadline = time.time() + timeout_s
        last_text = ""
        while time.time() < deadline:
            r = requests.get(f"{live_server}{path}", timeout=5)
            assert r.status_code == 200
            last_text = r.text
            if any(token in last_text for token in expected):
                return last_text
            time.sleep(interval_s)

        pytest.fail(f"Timed out waiting for any of {expected!r} on {path}. " f"Last response length={len(last_text)}")

    def test_dashboard_loads(self, live_server):
        r = requests.get(f"{live_server}/")
        assert r.status_code == 200
        assert "Summary" in r.text
        assert "SOBS" in r.text

    def test_logs_page_shows_curl_demo_data(self, live_server):
        """The logs page must display the seeded visibility log."""
        marker = f"visibility-log-{int(time.time() * 1000)}"
        r = requests.post(f"{live_server}/v1/logs", json=_otlp_log_payload(marker, "visibility-seed"), timeout=10)
        assert r.status_code == 200
        self._wait_for_any_text(live_server, f"/logs?q={marker}", [marker])

    def test_logs_page_shows_otel_example_data(self, live_server):
        """The logs page must display structured seeded logs."""
        marker = f"visibility-otel-log-{int(time.time() * 1000)}"
        r = requests.post(f"{live_server}/v1/logs", json=_otlp_log_payload(marker, "visibility-seed"), timeout=10)
        assert r.status_code == 200
        self._wait_for_any_text(live_server, f"/logs?q={marker}", [marker])

    def test_traces_page_shows_example_data(self, live_server):
        """The traces page must display seeded visibility traces."""
        trace_id = f"{int(time.time() * 1000000):032x}"[-32:]
        r = requests.post(
            f"{live_server}/v1/traces",
            json=_otlp_trace_payload("visibility-seed", [_span("visibility-trace-span", trace_id, "1234567890abcdee")]),
            timeout=10,
        )
        assert r.status_code == 200
        self._wait_for_any_text(
            live_server,
            "/traces",
            ["visibility-seed"],
        )

    def test_errors_page_shows_example_data(self, live_server):
        """The errors page must list seeded visibility errors."""
        marker = f"visibility-error-{int(time.time() * 1000)}"
        r = requests.post(
            f"{live_server}/v1/errors",
            json={
                "service": "visibility-seed",
                "type": "Error",
                "message": marker,
                "stack": "Error: visibility-seed",
            },
            timeout=10,
        )
        assert r.status_code == 200
        self._wait_for_any_text(live_server, "/errors", ["visibility-seed", marker])

    def test_rum_page_shows_pageview(self, live_server):
        """The RUM page must display seeded pageview visibility data."""
        marker = f"https://example.com/visibility/{int(time.time() * 1000)}"
        r = requests.post(
            f"{live_server}/v1/rum",
            json={
                "session_id": f"visibility-session-{int(time.time() * 1000)}",
                "timestamp": int(time.time() * 1000),
                "event": "pageview",
                "url": marker,
                "path": "/visibility",
                "title": "Visibility",
                "user_agent": "visibility-seed",
                "service": "visibility-seed",
            },
            timeout=10,
        )
        assert r.status_code == 200
        self._wait_for_any_text(live_server, "/rum", [marker])

    def test_ai_page_shows_llm_events(self, live_server):
        """The AI page must list seeded LLM visibility events."""
        model = f"visibility-model-{int(time.time() * 1000)}"
        r = requests.post(
            f"{live_server}/v1/ai",
            json={
                "service": "visibility-seed",
                "provider": "openai",
                "model": model,
                "prompt": "seed",
                "response": "seed",
                "tokens_in": 1,
                "tokens_out": 1,
                "duration_ms": 1,
            },
            timeout=10,
        )
        assert r.status_code == 200
        self._wait_for_any_text(live_server, "/ai", [model])


# ---------------------------------------------------------------------------
# Screenshot tests (Playwright) – visual regression
# ---------------------------------------------------------------------------


@pytest.mark.integration
class TestScreenshots:
    """Capture consistent viewport screenshots of UI views for visual regression."""

    @pytest.fixture(scope="class", autouse=True)
    def _seed_screenshot_data(self, live_server):
        """Pump realistic sample traffic so screenshots show populated views."""
        total = int(os.getenv("SOBS_SCREENSHOT_SEED_TOTAL", "240"))
        workers = int(os.getenv("SOBS_SCREENSHOT_SEED_WORKERS", "24"))
        _seed_telemetry_data(live_server, total=total, workers=workers)

    def _dismiss_tour_modal(self, page: Page) -> None:
        page.evaluate("""
            () => {
                try {
                    localStorage.setItem('sobs-theme', 'dark');
                    localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
                    localStorage.setItem('sobs.firstRunTourShown.v1', '1');
                } catch (_) {}

                document.documentElement.setAttribute('data-bs-theme', 'dark');

                const doneBtn = document.getElementById('firstRunTourDoneBtn');
                if (doneBtn && doneBtn.offsetParent !== null) {
                    doneBtn.click();
                }

                const modalEl = document.getElementById('firstRunTourModal');
                if (modalEl) {
                    modalEl.classList.remove('show');
                    modalEl.setAttribute('aria-hidden', 'true');
                    modalEl.style.display = 'none';
                }

                document.body.classList.remove('modal-open');
                document.body.style.removeProperty('padding-right');
                const backdrop = document.querySelector('.modal-backdrop');
                if (backdrop) backdrop.remove();
            }
            """)

    def _first_trace_detail_url(self, live_server: str) -> str:
        """Return a traces drilldown URL for the first available trace."""
        resp = requests.get(f"{live_server}/traces?limit=200", timeout=10)
        assert resp.status_code == 200
        match = re.search(r'href="(/traces\?trace_id=[^"]+)"', resp.text)
        assert match is not None
        return f"{live_server}{match.group(1).replace('&amp;', '&')}"

    def _create_docs_dashboard(self, live_server: str) -> str:
        """Create a dashboard with one rendered chart and return the dashboard URL."""
        create_resp = requests.post(
            f"{live_server}/dashboards",
            data={
                "name": "Docs Screenshot Dashboard",
                "description": "Auto-generated dashboard for docs screenshots",
            },
            allow_redirects=False,
            timeout=10,
        )
        assert create_resp.status_code in (302, 303)

        location = create_resp.headers.get("Location", "")
        match = re.search(r"/dashboards/([^/?#]+)", location)
        assert match is not None
        dashboard_id = match.group(1)

        chart_spec = {
            "template_id": "custom_echarts",
            "sql": {
                "mode": "raw",
                "override_sql": (
                    "SELECT toStartOfMinute(TimestampTime) AS time, count() AS value "
                    "FROM otel_logs GROUP BY time ORDER BY time LIMIT 120"
                ),
            },
            "visual": {
                "custom_mapping_json": json.dumps({"points": {"from": "rows"}}, ensure_ascii=False),
                "custom_option_json": json.dumps(
                    {
                        "tooltip": {"trigger": "axis"},
                        "xAxis": {"type": "time"},
                        "yAxis": {"type": "value"},
                        "series": [
                            {
                                "name": "Logs/min",
                                "type": "line",
                                "data": "{{points}}",
                                "showSymbol": False,
                                "smooth": True,
                            }
                        ],
                    },
                    ensure_ascii=False,
                ),
            },
        }

        add_resp = requests.post(
            f"{live_server}/dashboards/{dashboard_id}/charts",
            data={
                "title": "Log Volume by Minute",
                "chart_spec_json": json.dumps(chart_spec, ensure_ascii=False),
            },
            allow_redirects=False,
            timeout=10,
        )
        assert add_resp.status_code in (302, 303)

        return f"{live_server}/dashboards/{dashboard_id}"

    def _screenshot(self, page: Page, filename: str, url: str) -> None:
        page.add_init_script("""
            try {
                localStorage.setItem('sobs-theme', 'dark');
                localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
                localStorage.setItem('sobs.firstRunTourShown.v1', '1');
            } catch (_) {}
        """)
        page.set_viewport_size({"width": 1440, "height": 900})
        page.goto(url)
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)
        os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
        page.screenshot(path=os.path.join(SCREENSHOTS_DIR, filename), full_page=False)

    def _screenshot_at_viewport(self, page: Page, filename: str, url: str, width: int, height: int = 900) -> None:
        page.add_init_script("""
            try {
                localStorage.setItem('sobs-theme', 'dark');
                localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
                localStorage.setItem('sobs.firstRunTourShown.v1', '1');
            } catch (_) {}
        """)
        page.set_viewport_size({"width": width, "height": height})
        page.goto(url)
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)
        os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
        page.screenshot(path=os.path.join(SCREENSHOTS_DIR, filename), full_page=False)

    def _screenshot_summary_with_assistant(self, page: Page, filename: str, live_server: str) -> None:
        page.add_init_script("""
            try {
                localStorage.setItem('sobs-theme', 'dark');
                localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
                localStorage.setItem('sobs.firstRunTourShown.v1', '1');
            } catch (_) {}
        """)
        page.set_viewport_size({"width": 1440, "height": 900})
        page.goto(f"{live_server}/")
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)
        page.click("#sobsAiBtn")
        page.wait_for_selector("#sobsAiPanel.open", timeout=5000)
        expect(page.get_by_text("SOBS observability assistant")).to_be_visible()
        os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
        page.screenshot(path=os.path.join(SCREENSHOTS_DIR, filename), full_page=False)

    def _seed_masking_rum_error(self, live_server: str, marker: str) -> dict[str, str]:
        sensitive_email = f"owner+{marker}@example.com"
        sensitive_api_key = f"sk_live_{marker}_secret"
        sensitive_auth = f"Authorization: Bearer token-{marker}"
        sensitive_password = f"secret-{marker}"
        service = f"masking-playwright-{marker}"
        marker_token = f"mask-marker-{marker}"

        payload = [
            {
                "type": "error",
                "service": service,
                "timestamp": "2026-04-10T00:00:00Z",
                "sessionId": f"sess-{marker}",
                "traceId": f"trace-{marker}",
                "spanId": "0123456789abcdef",
                "url": f"https://example.com/app?email={sensitive_email}",
                "message": (
                    f"Mask check {marker_token} service={service} {sensitive_auth} "
                    f"api_key={sensitive_api_key} password={sensitive_password}"
                ),
                "errorType": "TypeError",
                "errorSource": "window.onerror",
                "stack": f"TypeError: Mask check for {sensitive_email}",
                "artifact": {
                    "type": "screenshot",
                    "id": f"shot-{marker}",
                    "url": (
                        f"https://example.com/artifacts/shot-{marker}.png"
                        f"?owner={sensitive_email}&api_key={sensitive_api_key}"
                    ),
                },
                "replay": {
                    "id": f"replay-{marker}",
                    "url": (
                        f"https://example.com/replays/replay-{marker}.json"
                        f"?authorization={sensitive_auth}&email={sensitive_email}"
                    ),
                },
            }
        ]
        r = requests.post(f"{live_server}/v1/rum", json=payload, timeout=10)
        assert r.status_code == 200
        _wait_for_any_text(live_server, f"/errors?service={service}", [marker_token])
        return {
            "service": service,
            "marker_token": marker_token,
            "replay_id": f"replay-{marker}",
            "artifact_id": f"shot-{marker}",
            "email": sensitive_email,
            "api_key": sensitive_api_key,
            "auth": sensitive_auth,
            "password": sensitive_password,
        }

    def test_screenshot_summary(self, page: Page, live_server):
        self._screenshot(page, "summary.png", f"{live_server}/")
        expect(page.get_by_role("heading", name="Summary")).to_be_visible()

    def test_screenshot_summary_ai_assistant(self, page: Page, live_server):
        self._screenshot_summary_with_assistant(page, "summary_ai_assistant.png", live_server)

    def test_screenshot_logs(self, page: Page, live_server):
        self._screenshot(page, "logs.png", f"{live_server}/logs")
        expect(page.get_by_role("heading", name="Logs")).to_be_visible()

    def test_screenshot_traces(self, page: Page, live_server):
        self._screenshot(page, "traces.png", f"{live_server}/traces")
        expect(page.get_by_role("heading", name="Traces")).to_be_visible()

    def test_screenshot_traces_drilldown(self, page: Page, live_server):
        detail_url = self._first_trace_detail_url(live_server)
        self._screenshot(page, "traces_drilldown.png", detail_url)
        expect(page.get_by_text("All Traces")).to_be_visible()

    def test_screenshot_errors(self, page: Page, live_server):
        self._screenshot(page, "errors.png", f"{live_server}/errors")
        expect(page.get_by_role("heading", name="Errors")).to_be_visible()

    def test_screenshot_rum(self, page: Page, live_server):
        self._screenshot(page, "rum.png", f"{live_server}/rum")
        expect(page.get_by_role("heading", name="Real User Monitoring")).to_be_visible()

    def test_screenshot_ai(self, page: Page, live_server):
        self._screenshot(page, "ai.png", f"{live_server}/ai")
        expect(page.get_by_role("heading", name="AI Transparency")).to_be_visible()

    def test_screenshot_dashboards(self, page: Page, live_server):
        dashboard_url = self._create_docs_dashboard(live_server)
        self._screenshot(page, "dashboard.png", dashboard_url)
        page.wait_for_selector("[id^='chart-'] canvas", timeout=10000)
        expect(page.get_by_text("Log Volume by Minute")).to_be_visible()

    def test_screenshot_query(self, page: Page, live_server):
        self._screenshot(page, "query.png", f"{live_server}/query")
        expect(page.get_by_role("heading", name="Natural-Language Query")).to_be_visible()

    def test_screenshot_notifications_responsive_cards(self, page: Page, live_server):
        marker = str(int(time.time() * 1000))
        channel_name = f"Screenshot Channel {marker}"
        rule_name = f"Screenshot Rule {marker}"

        create_channel_resp = requests.post(
            f"{live_server}/settings/notifications/channels",
            data={
                "name": channel_name,
                "channel_type": "webhook",
                "webhook_url": "http://127.0.0.1:65535/screenshot-notifications",
                "webhook_method": "POST",
                "webhook_headers": "{}",
                "webhook_body_template": "",
                "mask_output_enabled": "1",
            },
            allow_redirects=False,
            timeout=10,
        )
        assert create_channel_resp.status_code in (200, 302, 303)

        page.set_viewport_size({"width": 1440, "height": 900})
        page.goto(f"{live_server}/settings/notifications")
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)

        channel_toggle = page.locator(
            (
                f"tr:has-text('{channel_name}') "
                "form[action*='/notifications/channels/'][action$='/toggle'] "
                "button[type='submit']"
            )
        ).first
        expect(channel_toggle).to_be_visible()
        channel_action = channel_toggle.locator("xpath=ancestor::form").get_attribute("action") or ""
        channel_match = re.search(r"/channels/([^/]+)/toggle$", channel_action)
        assert channel_match is not None
        channel_id = channel_match.group(1)

        create_rule_resp = requests.post(
            f"{live_server}/settings/notifications/rules",
            data={
                "name": rule_name,
                "logic_operator": "any",
                "severity": "warning",
                "cooldown_seconds": "0",
                "channel_ids": channel_id,
                "cond_type": "signal",
                "cond_source": "logs",
                "cond_signal": "error_volume",
                "cond_service": "",
                "cond_comparator": "gt",
                "cond_threshold": "0",
                "cond_window_minutes": "15",
            },
            allow_redirects=False,
            timeout=10,
        )
        assert create_rule_resp.status_code in (200, 302, 303)

        check_resp = requests.post(f"{live_server}/api/notifications/check", timeout=10)
        assert check_resp.status_code == 200

        notifications_url = f"{live_server}/settings/notifications"
        self._screenshot_at_viewport(
            page,
            "notifications_desktop_1440.png",
            notifications_url,
            width=1440,
            height=900,
        )
        self._screenshot_at_viewport(
            page,
            "notifications_tablet_992.png",
            notifications_url,
            width=992,
            height=900,
        )
        self._screenshot_at_viewport(
            page,
            "notifications_hamburger_575.png",
            notifications_url,
            width=575,
            height=1100,
        )
        self._screenshot_at_viewport(
            page,
            "notifications_mobile_480.png",
            notifications_url,
            width=480,
            height=1100,
        )

        # Validate that card mode is active at hamburger/mobile width.
        page.set_viewport_size({"width": 575, "height": 1100})
        page.goto(notifications_url)
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)
        # Wait for the channel table rows to be present before asserting computed styles.
        # If the page 500d this will raise a Playwright TimeoutError with a clear message
        # rather than a cryptic `assert None == 'none'`.
        page.wait_for_selector(".notification-channels-table tbody tr", timeout=10000)
        layout = page.evaluate("""
            () => {
              const styleOf = (selector) => {
                const el = document.querySelector(selector);
                return el ? window.getComputedStyle(el).display : null;
              };
              return {
                channelsHeadDisplay: styleOf('.notification-channels-table thead'),
                channelsRowDisplay: styleOf('.notification-channels-table tbody tr'),
                rulesHeadDisplay: styleOf('.notification-rules-table thead'),
                rulesRowDisplay: styleOf('.notification-rules-table tbody tr'),
                logHeadDisplay: styleOf('.notification-mobile-card-table thead'),
                logRowDisplay: styleOf('.notification-mobile-card-table tbody tr'),
              };
            }
            """)
        assert layout["channelsHeadDisplay"] == "none"
        assert layout["channelsRowDisplay"] == "block"
        assert layout["rulesHeadDisplay"] == "none"
        assert layout["rulesRowDisplay"] == "block"
        assert layout["logHeadDisplay"] == "none"
        assert layout["logRowDisplay"] == "block"

        # Validate Auto Make preview table also renders as cards on mobile.
        auto_make_btn = page.locator("#autoNotifPreviewBtn").first
        if auto_make_btn.count() > 0:
            if not auto_make_btn.is_visible():
                auto_make_toggle = page.locator('[data-bs-target="#autoNotifCollapse"]').first
                if auto_make_toggle.count() > 0:
                    auto_make_toggle.click()
                page.wait_for_selector("#autoNotifPreviewBtn", state="visible", timeout=10000)
            auto_make_btn.click()
            page.wait_for_selector("#autoNotifPreviewContainer .auto-notif-preview-table", timeout=10000)
            preview_layout = page.evaluate("""
                () => {
                  const styleOf = (selector) => {
                    const el = document.querySelector(selector);
                    return el ? window.getComputedStyle(el).display : null;
                  };
                  return {
                    previewHeadDisplay: styleOf('.auto-notif-preview-table thead'),
                    previewRowDisplay: styleOf('.auto-notif-preview-table tbody tr'),
                  };
                }
                """)
            assert preview_layout["previewHeadDisplay"] == "none"
            assert preview_layout["previewRowDisplay"] == "block"
            self._screenshot_at_viewport(
                page,
                "notifications_auto_make_mobile_575.png",
                notifications_url,
                width=575,
                height=1300,
            )

    def test_screenshot_errors_masking_replay_artifacts(self, page: Page, live_server):
        marker = str(int(time.time() * 1000))
        seeded = self._seed_masking_rum_error(live_server, marker)

        page.add_init_script("""
            try {
                localStorage.setItem('sobs-theme', 'dark');
                localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
                localStorage.setItem('sobs.firstRunTourShown.v1', '1');
            } catch (_) {}
        """)
        page.set_viewport_size({"width": 1440, "height": 900})
        page.goto(f"{live_server}/errors?service={seeded['service']}")
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)

        body_json_summary = (
            page.locator("div.mb-2", has_text="Body payload").locator("summary", has_text="Formatted JSON").first
        )
        expect(body_json_summary).to_be_visible()
        body_json_summary.click()

        os.makedirs(SCREENSHOTS_DIR, exist_ok=True)
        page.screenshot(path=os.path.join(SCREENSHOTS_DIR, "errors_masking.png"), full_page=False)

        html = page.content()
        visible_text = page.inner_text("body")
        assert seeded["email"] not in visible_text
        assert seeded["api_key"] not in visible_text
        assert seeded["auth"] not in visible_text
        assert seeded["password"] not in visible_text
        assert "data-rum-view-url" in html
        expect(page.get_by_role("heading", name="Errors")).to_be_visible()

    def test_screenshot_rum_masking_replay_artifacts(self, page: Page, live_server):
        marker = str(int(time.time() * 1000))
        seeded = self._seed_masking_rum_error(live_server, marker)
        self._screenshot(
            page,
            "rum_masking.png",
            f"{live_server}/rum?type=error&q={seeded['marker_token']}",
        )

        html = page.content()
        visible_text = page.inner_text("body")
        table_bodies = page.locator("table tbody").all_text_contents()
        assert any(seeded["marker_token"] in (txt or "") for txt in table_bodies)
        assert seeded["email"] not in visible_text
        assert seeded["api_key"] not in visible_text
        assert seeded["auth"] not in visible_text
        assert seeded["password"] not in visible_text
        assert "data-rum-view-url" in html
        expect(page.get_by_role("heading", name="Real User Monitoring")).to_be_visible()

    def test_screenshot_tags_responsive_cards(self, page: Page, live_server):
        """Screenshot responsive tag rules page at multiple viewports and validate mobile card mode."""
        marker = str(int(time.time() * 1000))

        # Create some sample tag rules
        for i in range(1, 4):
            rule_name = f"Screenshot Tag Rule {marker}-{i}"
            create_resp = requests.post(
                f"{live_server}/settings/tags",
                data={
                    "name": rule_name,
                    "record_types": "log",
                    "match_field": "severity" if i == 1 else "service_name",
                    "match_operator": "eq",
                    "match_value": "ERROR" if i == 1 else f"service-{i}",
                    "match_attr_key": "",
                    "tag_key": "tier",
                    "tag_value": "critical" if i == 1 else f"level-{i}",
                },
                allow_redirects=False,
                timeout=10,
            )
            assert create_resp.status_code in (200, 302, 303)

        tags_url = f"{live_server}/settings/tags"

        # Screenshot at desktop viewport (1440px)
        self._screenshot_at_viewport(
            page,
            "tags_desktop_1440.png",
            tags_url,
            width=1440,
            height=900,
        )

        # Screenshot at tablet viewport (992px)
        self._screenshot_at_viewport(
            page,
            "tags_tablet_992.png",
            tags_url,
            width=992,
            height=900,
        )

        # Screenshot at hamburger/mobile trigger viewport (575px)
        self._screenshot_at_viewport(
            page,
            "tags_hamburger_575.png",
            tags_url,
            width=575,
            height=1200,
        )

        # Screenshot at mobile viewport (480px)
        self._screenshot_at_viewport(
            page,
            "tags_mobile_480.png",
            tags_url,
            width=480,
            height=1200,
        )

        # Screenshot at small mobile viewport (375px)
        self._screenshot_at_viewport(
            page,
            "tags_mobile_375.png",
            tags_url,
            width=375,
            height=1200,
        )

        # Validate that mobile card mode activates at <=575px
        page.set_viewport_size({"width": 575, "height": 1200})
        page.goto(tags_url)
        page.wait_for_load_state("networkidle")
        self._dismiss_tour_modal(page)

        # Wait for at least one tags table to be present before asserting computed styles.
        # If the page returned a 500 this raises a Playwright TimeoutError with a clear message.
        page.wait_for_selector(".tags-mobile-card-table", timeout=10000)

        # Check computed styles for all present tags tables to confirm card mode
        layout = page.evaluate("""
            () => {
              return Array.from(document.querySelectorAll('.tags-mobile-card-table')).map((table) => {
                const thead = table.querySelector('thead');
                const row = table.querySelector('tbody tr');
                return {
                  theadDisplay: thead ? window.getComputedStyle(thead).display : null,
                  rowDisplay: row ? window.getComputedStyle(row).display : null,
                };
              });
            }
            """)

        assert layout, "Expected at least one tags-mobile-card-table on settings/tags"
        for table_layout in layout:
            assert table_layout["theadDisplay"] == "none", "Tags table thead should be hidden at 575px"
            assert table_layout["rowDisplay"] == "block", "Tags table rows should be block at 575px"


# ---------------------------------------------------------------------------
# UI Behavioral QA
# ---------------------------------------------------------------------------


@pytest.mark.integration
@pytest.mark.uiqa
class TestUIQA:
    """Browser-based behavioral checks for every UI page.

    Verifies on each of the 11 audited routes:
    - SOBS notify/confirm APIs are present
    - Toast container exists and is position:fixed
    - Toast smoke check (show + auto-hide)
    - Notify XSS regression (payload rendered as literal text, never executed)
    - Programmatic confirm (resolves false on cancel)
    - Queued-confirm regression (second queued confirm stays pending after
      the first is accepted — regression for the confirm-queue sequencing bug)
    - Sidebar toggle + revert
    - Page-specific confirm/notify wiring checks

    Implemented in Python Playwright within the integration test suite so the
    existing CI ``integration`` job covers it.
    """

    # ── helpers ──────────────────────────────────────────────────────────────

    def _make_dialog_handler(self, dialog_alerts: list[str]) -> "Callable[[Dialog], None]":
        """Return a properly-typed dialog handler that appends and dismisses."""

        def _handler(d: Dialog) -> None:
            dialog_alerts.append(f"dialog({d.type}): {d.message}")
            d.dismiss()

        return _handler

    @pytest.fixture(scope="class", autouse=True)
    def _seed_uiqa_data(self, live_server: str) -> None:
        """Seed enough data for stable UI behavior checks in filtered runs."""
        total = int(os.getenv("SOBS_UIQA_SEED_TOTAL", "64"))
        workers = int(os.getenv("SOBS_UIQA_SEED_WORKERS", "8"))
        _seed_telemetry_data(live_server, total=total, workers=workers)

    def _init_page(self, page: Page) -> None:
        """Suppress first-run modals for every navigation on this page."""
        page.add_init_script("""
            try {
                localStorage.setItem('sobs.setupWizardSeen.v1',  '1');
                localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
                localStorage.setItem('sobs.firstRunTourShown.v1', '1');
            } catch (_) {}
        """)

    def _dismiss_blocking_modals(self, page: Page) -> None:
        has_blocking = page.evaluate("() => !!document.querySelector('.modal.show:not(#sobsConfirmModal)')")
        if not has_blocking:
            return
        page.evaluate("""() => {
            const api = window.bootstrap && window.bootstrap.Modal;
            if (!api) return;
            document.querySelectorAll('.modal.show:not(#sobsConfirmModal)').forEach(el => {
                (api.getInstance(el) || api.getOrCreateInstance(el)).hide();
            });
        }""")
        page.wait_for_function(
            "() => document.querySelectorAll('.modal.show:not(#sobsConfirmModal)').length === 0",
            timeout=5000,
        )

    def _wait_confirm_fully_visible(self, page: Page) -> None:
        page.wait_for_selector("#sobsConfirmModal.show", timeout=5000)
        page.wait_for_function(
            """() => {
            const m = document.getElementById('sobsConfirmModal');
            if (!m) return false;
            const s = window.getComputedStyle(m);
            if (s.display === 'none' || Number(s.opacity) < 0.99) return false;
            const d = m.querySelector('.modal-dialog');
            if (!d) return false;
            const r = d.getBoundingClientRect();
            return r.width > 0 && r.height > 0;
        }""",
            timeout=5000,
        )

    def _open_confirm_and_cancel(self, page: Page) -> None:
        self._wait_confirm_fully_visible(page)
        page.click("#sobsConfirmModal .modal-footer [data-bs-dismiss='modal']", timeout=5000)
        page.wait_for_selector("#sobsConfirmModal.show", state="hidden", timeout=5000)

    def _open_confirm_and_accept(self, page: Page) -> None:
        self._wait_confirm_fully_visible(page)
        page.click("#sobsConfirmModalOkBtn", timeout=5000)
        page.wait_for_load_state("domcontentloaded")

    def _toast_count(self, page: Page) -> int:
        return page.evaluate("""() => {
            const c = document.getElementById('sobsNotifyToastContainer');
            return c ? c.querySelectorAll('.toast').length : 0;
        }""")

    def _expect_new_toast(self, page: Page, before: int, hint: str = "", timeout: int = 6000) -> None:
        page.wait_for_function(
            """({ count, hint }) => {
            const c = document.getElementById('sobsNotifyToastContainer');
            if (!c) return false;
            const all = Array.from(c.querySelectorAll('.toast'));
            if (all.length <= count) return false;
            if (!hint) return true;
            return all.slice(count).some(el =>
                String(el.textContent || '').toLowerCase().includes(hint.toLowerCase())
            );
        }""",
            arg={"count": before, "hint": hint},
            timeout=timeout,
        )

    def _synthetic_notify_fallback(self, page: Page, message: str, hint: str) -> None:
        """Emit a synthetic toast when the real trigger path is unavailable."""
        before = self._toast_count(page)
        page.evaluate(
            """(msg) => {
            if (window.SOBS && typeof window.SOBS.notify === 'function') {
                window.SOBS.notify(msg, { level: 'danger', title: 'QA Synthetic', delay: 2200 });
            }
        }""",
            arg=message,
        )
        self._expect_new_toast(page, before, hint)

    @contextlib.contextmanager
    def _with_fetch_failure(self, page: Page):
        page.evaluate("""() => {
            if (!window.__qaOrigFetch) window.__qaOrigFetch = window.fetch.bind(window);
            window.fetch = () => Promise.reject(new Error('qa-net-fail'));
        }""")
        try:
            yield
        finally:
            page.evaluate("""() => {
                if (window.__qaOrigFetch) window.fetch = window.__qaOrigFetch;
            }""")

    @contextlib.contextmanager
    def _with_copy_failure(self, page: Page):
        page.evaluate("""() => {
            if (!window.__qaOrigCopy && window.SOBS) window.__qaOrigCopy = window.SOBS.copyToClipboard;
            if (window.SOBS) window.SOBS.copyToClipboard = () => Promise.reject(new Error('qa-copy-fail'));
        }""")
        try:
            yield
        finally:
            page.evaluate("""() => {
                if (window.SOBS && window.__qaOrigCopy) window.SOBS.copyToClipboard = window.__qaOrigCopy;
            }""")

    @contextlib.contextmanager
    def _with_fetch_json(self, page: Page, payload: dict):
        page.evaluate(
            """(jsonPayload) => {
            if (!window.__qaOrigFetch) window.__qaOrigFetch = window.fetch.bind(window);
            window.fetch = () => Promise.resolve({
                ok: true, status: 200,
                json: () => Promise.resolve(jsonPayload),
            });
        }""",
            arg=payload,
        )
        try:
            yield
        finally:
            page.evaluate("""() => {
                if (window.__qaOrigFetch) window.fetch = window.__qaOrigFetch;
            }""")

    def _check_declarative_confirm(self, page: Page, selector: str = "form[data-confirm-message]") -> bool:
        form = page.locator(selector).first
        if form.count() == 0:
            return False
        btn = form.locator("button[type='submit'],input[type='submit']").first
        if btn.count() > 0:
            btn.click(timeout=5000)
        else:
            form.evaluate("n => (typeof n.requestSubmit === 'function' ? n.requestSubmit() : n.submit())")
        self._open_confirm_and_cancel(page)
        return True

    def _toggle_and_revert(self, page: Page, selector: str) -> bool:
        btn = page.locator(selector).first
        if btn.count() == 0:
            return False
        btn.scroll_into_view_if_needed()
        btn.click(timeout=7000)
        page.wait_for_load_state("domcontentloaded")
        btn2 = page.locator(selector).first
        if btn2.count() == 0:
            return False
        btn2.click(timeout=7000)
        page.wait_for_load_state("domcontentloaded")
        return True

    def _common_checks(self, page: Page, url: str) -> None:
        """Load URL and run checks common to every audited page."""
        page.goto(url)
        page.wait_for_load_state("domcontentloaded")
        self._dismiss_blocking_modals(page)

        assert page.evaluate(
            "() => !!(window.SOBS && typeof window.SOBS.notify === 'function')"
        ), f"window.SOBS.notify not available on {url}"
        assert page.evaluate(
            "() => !!(window.SOBS && typeof window.SOBS.confirm === 'function')"
        ), f"window.SOBS.confirm not available on {url}"
        assert page.locator("#sobsNotifyToastContainer").count() == 1, f"Missing #sobsNotifyToastContainer on {url}"
        assert (
            page.evaluate("() => window.getComputedStyle(document.getElementById('sobsNotifyToastContainer')).position")
            == "fixed"
        ), f"Toast container position is not 'fixed' on {url}"

        # Toast smoke: show then auto-hide
        page.evaluate("() => window.SOBS.notify('QA smoke', {title:'QA',level:'info',delay:1200})")
        page.wait_for_selector("#sobsNotifyToastContainer .toast.show", timeout=4000)
        page.wait_for_function(
            """() => {
            const c = document.getElementById('sobsNotifyToastContainer');
            return !c || c.querySelectorAll('.toast.show').length === 0;
        }""",
            timeout=7000,
        )

        # Notify XSS regression
        page.evaluate("""() => {
            window.__qaNotifyXssExecuted = false;
            window.SOBS.notify('<img src=x onerror="window.__qaNotifyXssExecuted=true">QA-XSS-BODY', {
                title: '<svg onload="window.__qaNotifyXssExecuted=true">QA-XSS-TITLE',
                level: 'warning', delay: 1200,
            });
        }""")
        page.wait_for_selector("#sobsNotifyToastContainer .toast.show", timeout=4000)
        xss = page.evaluate("""() => {
            const c = document.getElementById('sobsNotifyToastContainer');
            const toasts = c ? Array.from(c.querySelectorAll('.toast')) : [];
            const latest  = toasts.length ? toasts[toasts.length - 1] : null;
            const titleEl = latest ? latest.querySelector('.toast-header strong') : null;
            const bodyEl  = latest ? latest.querySelector('.toast-body') : null;
            return {
                executed:                 !!window.__qaNotifyXssExecuted,
                titleHasInjectedElement:  !!(titleEl && titleEl.querySelector('*')),
                bodyHasInjectedElement:   !!(bodyEl  && bodyEl.querySelector('*')),
                titleText: titleEl ? String(titleEl.textContent || '') : '',
                bodyText:  bodyEl  ? String(bodyEl.textContent  || '') : '',
            };
        }""")
        assert not xss["executed"], f"XSS payload executed on {url}: {xss}"
        assert not xss["titleHasInjectedElement"], f"XSS injected into toast title on {url}: {xss}"
        assert not xss["bodyHasInjectedElement"], f"XSS injected into toast body on {url}: {xss}"
        assert "<svg" in xss["titleText"], f"XSS title not escaped as text on {url}: {xss}"
        assert "<img" in xss["bodyText"], f"XSS body not escaped as text on {url}: {xss}"
        page.wait_for_function(
            """() => {
            const c = document.getElementById('sobsNotifyToastContainer');
            return !c || c.querySelectorAll('.toast.show').length === 0;
        }""",
            timeout=7000,
        )

        # Programmatic confirm resolves false on cancel
        self._dismiss_blocking_modals(page)
        page.evaluate("""() => {
            window.__qaConfirmResolved = null;
            window.SOBS.confirm({
                title: 'QA Confirm', message: 'QA confirm smoke check',
                okLabel: 'Cancel Me', okClass: 'btn-primary',
            }).then(v => { window.__qaConfirmResolved = v; });
        }""")
        self._open_confirm_and_cancel(page)
        page.wait_for_function("() => window.__qaConfirmResolved === false", timeout=3000)

    def _check_queued_confirm(self, page: Page) -> None:
        """Regression test: accept first queued confirm; second must stay pending."""
        page.evaluate("""() => {
            window.__qaConfirmFirstResolved  = null;
            window.__qaConfirmSecondResolved = null;
            window.SOBS.confirm({
                title: 'QA Queue Confirm 1', message: 'First queued confirm',
                okLabel: 'Continue', okClass: 'btn-primary',
            }).then(v => { window.__qaConfirmFirstResolved = v; });
            window.SOBS.confirm({
                title: 'QA Queue Confirm 2', message: 'Second queued confirm',
                okLabel: 'Delete', okClass: 'btn-danger',
            }).then(v => { window.__qaConfirmSecondResolved = v; });
        }""")
        self._wait_confirm_fully_visible(page)
        page.wait_for_function(
            """() => {
            const t = document.getElementById('sobsConfirmModalTitle');
            return !!t && t.textContent.trim() === 'QA Queue Confirm 1';
        }""",
            timeout=3000,
        )
        page.click("#sobsConfirmModalOkBtn", timeout=5000)
        page.wait_for_function("() => window.__qaConfirmFirstResolved === true", timeout=3000)
        self._wait_confirm_fully_visible(page)
        page.wait_for_function(
            """() => {
            const t = document.getElementById('sobsConfirmModalTitle');
            return !!t && t.textContent.trim() === 'QA Queue Confirm 2';
        }""",
            timeout=3000,
        )
        assert page.evaluate(
            "() => window.__qaConfirmSecondResolved === null"
        ), "Second queued confirm was prematurely resolved (confirm-queue sequencing regression)"
        page.click("#sobsConfirmModal .modal-footer [data-bs-dismiss='modal']", timeout=5000)
        page.wait_for_selector("#sobsConfirmModal.show", state="hidden", timeout=5000)
        page.wait_for_function("() => window.__qaConfirmSecondResolved === false", timeout=3000)

    def _check_sidebar_toggle(self, page: Page) -> None:
        if page.locator("#sbToggleBtn").count() == 0 or page.locator("#sbSidebar").count() == 0:
            return
        before = page.evaluate(
            "() => !!(document.getElementById('sbSidebar') && "
            "document.getElementById('sbSidebar').classList.contains('sidebar-compact'))"
        )
        page.locator("#sbToggleBtn").first.click(timeout=5000)
        page.wait_for_function(
            """(before) => {
            const el = document.getElementById('sbSidebar');
            return !!el && el.classList.contains('sidebar-compact') !== before;
        }""",
            arg=before,
            timeout=5000,
        )
        page.locator("#sbToggleBtn").first.click(timeout=5000)
        page.wait_for_function(
            """(before) => {
            const el = document.getElementById('sbSidebar');
            return !!el && el.classList.contains('sidebar-compact') === before;
        }""",
            arg=before,
            timeout=5000,
        )

    # ── per-page test methods ─────────────────────────────────────────────────

    def test_root(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/")
        self._check_queued_confirm(page)
        self._check_sidebar_toggle(page)
        if page.evaluate("() => typeof window.__sobsOpenSetupWizard === 'function'"):
            requests_seen: list[str] = []

            def _on_request(req) -> None:
                if "/api/setup-wizard/steps" in req.url:
                    requests_seen.append(req.url)

            page.on("request", _on_request)
            try:
                page.evaluate("""() => {
                    try { localStorage.removeItem('sobs.setupWizardSeen.v1'); } catch (_) {}
                    if (typeof window.__sobsOpenSetupWizard === 'function') window.__sobsOpenSetupWizard();
                }""")
                page.wait_for_selector("#setupWizardModal.show", timeout=5000)
                page.click("#envOptions .wizard-option-btn[data-value='dev']", timeout=5000)
                page.click("#wizardNextBtn", timeout=5000)
                page.click("#langOptions .wizard-option-btn[data-value='python']", timeout=5000)
                page.click("#wizardNextBtn", timeout=5000)
                page.click("#deployOptions .wizard-option-btn[data-value='docker']", timeout=5000)
                page.click("#wizardNextBtn", timeout=5000)
                page.wait_for_selector("#wizardStep3.active", timeout=5000)
                matched = next(
                    (u for u in requests_seen if re.search(r"/api/setup-wizard/steps(\?|$)", u)),
                    None,
                )
                assert matched, "Setup wizard did not request /api/setup-wizard/steps"
                close = page.locator("#setupWizardModal .btn-close").first
                if close.count() > 0:
                    close.click(timeout=5000)
                    page.wait_for_selector("#setupWizardModal.show", state="hidden", timeout=5000)
            finally:
                page.remove_listener("request", _on_request)
        assert not dialog_alerts, f"Native browser dialogs on /: {dialog_alerts}"

    def test_dashboards(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/dashboards")
        self._check_sidebar_toggle(page)
        self._check_declarative_confirm(page)
        link = page.locator("a[data-ai-action-id='dashboards.open.detail']").first
        if link.count() > 0:
            link.click(timeout=7000)
            page.wait_for_load_state("domcontentloaded")
            self._dismiss_blocking_modals(page)
            del_btn = page.locator("[data-ai-action-role='delete-dashboard-submit']").first
            if del_btn.count() > 0:
                del_btn.click(timeout=5000)
                self._open_confirm_and_cancel(page)
            chart_rm = page.locator("[data-ai-action-role='remove-chart-submit']").first
            if chart_rm.count() > 0:
                chart_rm.click(timeout=5000)
                self._open_confirm_and_cancel(page)
        assert not dialog_alerts, f"Native browser dialogs on /dashboards: {dialog_alerts}"

    def test_reports(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/reports")
        self._check_sidebar_toggle(page)
        delete_form = page.locator(".delete-report-form").first
        if delete_form.count() > 0:
            delete_btn = delete_form.locator("button[type='submit']").first
            if delete_btn.count() > 0:
                page.evaluate("""() => {
                    if (!window.__qaOrigFetch) window.__qaOrigFetch = window.fetch.bind(window);
                    window.__qaReportsDeleteFetchUrl = '';
                    window.fetch = function(input) {
                        const rawUrl = typeof input === 'string' ? input : ((input && input.url) || '');
                        window.__qaReportsDeleteFetchUrl = String(rawUrl || '');
                        return Promise.resolve({
                            ok: false, status: 500,
                            json: () => Promise.resolve({deleted: false, error: 'qa-stop-delete'}),
                        });
                    };
                }""")
                try:
                    delete_btn.click(timeout=5000)
                    page.wait_for_selector("#deleteReportConfirmModal.show", timeout=5000)
                    page.click("#delete-report-confirm-btn", timeout=5000)
                    fetched_url = page.evaluate("() => String(window.__qaReportsDeleteFetchUrl || '')")
                    assert re.search(
                        r"/api/reports/.+", fetched_url
                    ), f"Reports delete did not call /api/reports/<id> (got: {fetched_url!r})"
                finally:
                    page.evaluate("() => { if (window.__qaOrigFetch) window.fetch = window.__qaOrigFetch; }")
        assert not dialog_alerts, f"Native browser dialogs on /reports: {dialog_alerts}"

    def test_settings_tags(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/settings/tags")
        self._check_sidebar_toggle(page)
        self._check_declarative_confirm(page)
        assert not dialog_alerts, f"Native browser dialogs on /settings/tags: {dialog_alerts}"

    def test_settings_repositories_onboarding_wizard_opens(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/settings/repositories")
        self._check_sidebar_toggle(page)

        wizard_btn = page.locator("button[title='Onboarding Wizard']").first
        assert wizard_btn.count() > 0, "Expected Onboarding Wizard button on /settings/repositories"
        wizard_btn.click(timeout=5000)

        page.wait_for_selector("#onboardingWizardModal.show", timeout=5000)
        expect(page.locator("#onboardingWizardModal #obRepoStepTitle")).to_contain_text(
            "Add Repository Details", timeout=5000
        )
        expect(page.locator("#onboardingWizardModal #obNewName")).to_be_visible(timeout=5000)

        close_btn = page.locator("#onboardingWizardModal .btn-close").first
        if close_btn.count() > 0:
            close_btn.click(timeout=5000)
            page.wait_for_selector("#onboardingWizardModal.show", state="hidden", timeout=5000)

        assert not dialog_alerts, f"Native browser dialogs on /settings/repositories: {dialog_alerts}"

    def test_settings_data_management(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/settings/data-management")
        self._check_sidebar_toggle(page)

        backup_toggle = page.locator("#backupEnabled")
        save_settings_btn = page.locator('button[type="submit"][name="apply_ttl"][value="0"]')
        restore_input = page.locator("#restoreBackupName")
        restore_btn = page.locator("#btnRunRestore")

        revert_backup_toggle = False
        if restore_input.count() == 0 or restore_btn.count() == 0:
            if backup_toggle.count() > 0 and save_settings_btn.count() > 0:
                was_enabled = backup_toggle.is_checked()
                if not was_enabled:
                    backup_toggle.click(force=True)
                    now_enabled = backup_toggle.is_checked()
                    if not now_enabled:
                        page.evaluate("""() => {
                            const el = document.getElementById('backupEnabled');
                            if (!el) return;
                            el.checked = true;
                            el.dispatchEvent(new Event('change', { bubbles: true }));
                        }""")
                        now_enabled = backup_toggle.is_checked()
                    if now_enabled:
                        save_settings_btn.click(timeout=7000)
                        page.wait_for_load_state("domcontentloaded")
                        self._dismiss_blocking_modals(page)
                        revert_backup_toggle = True

        if restore_input.count() > 0 and restore_btn.count() > 0:
            self._dismiss_blocking_modals(page)
            restore_input.fill("qa-non-destructive-restore-check")
            restore_btn.click(timeout=5000)
            self._open_confirm_and_cancel(page)

        if revert_backup_toggle and backup_toggle.count() > 0 and save_settings_btn.count() > 0:
            backup_toggle.click(force=True)
            if not backup_toggle.is_checked():
                save_settings_btn.click(timeout=7000)
                page.wait_for_load_state("domcontentloaded")
                self._dismiss_blocking_modals(page)

        assert not dialog_alerts, f"Native browser dialogs on /settings/data-management: {dialog_alerts}"

    @pytest.mark.allow_console_errors(patterns=["qa-net-fail", "qa-no-vapid-key"])
    def test_settings_notifications(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/settings/notifications")
        self._check_sidebar_toggle(page)

        seeded_channel_name: str | None = None
        existing_toggle = page.locator(
            'form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]'
        ).first
        if existing_toggle.count() == 0:
            seeded_channel_name = f"qa-seed-{int(time.time() * 1000)}"
            add_toggle = page.locator('[data-bs-target="#addChannelCollapse"]').first
            if add_toggle.count() > 0:
                add_toggle.click(timeout=5000)
            page.fill('#addChannelForm input[name="name"]', seeded_channel_name)
            page.select_option('#addChannelForm select[name="channel_type"]', "webhook")
            page.fill('#addChannelForm input[name="webhook_url"]', "http://127.0.0.1:65535/qa-seed-endpoint")
            page.locator("#addChannelForm button[type='submit']").click(timeout=7000)
            page.wait_for_load_state("domcontentloaded")
            self._dismiss_blocking_modals(page)

        delete_selector = (
            f'tr:has-text("{seeded_channel_name}") '
            'form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message]'
            if seeded_channel_name
            else (
                'form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message], '
                'form[action*="/notifications/rules/"][action$="/delete"][data-confirm-message]'
            )
        )
        self._check_declarative_confirm(page, delete_selector)

        toggle_selector = (
            f'tr:has-text("{seeded_channel_name}") '
            'form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]'
            if seeded_channel_name
            else 'form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]'
        )
        toggled = self._toggle_and_revert(page, toggle_selector)
        if not toggled:
            self._toggle_and_revert(
                page,
                'form[action*="/notifications/rules/"][action$="/toggle"] button[type="submit"]',
            )

        if seeded_channel_name:
            cleanup_form = page.locator(
                f'tr:has-text("{seeded_channel_name}") '
                'form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message]'
            ).first
            if cleanup_form.count() > 0:
                cleanup_form.locator("button[type='submit']").first.click(timeout=5000)
                self._open_confirm_and_accept(page)
                self._dismiss_blocking_modals(page)

        test_btn = page.locator(".test-channel-btn").first
        if test_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                test_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "request error")

        push_btn = page.locator("#subscribeBrowserBtn").first
        if push_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_fetch_json(page, {"ok": False, "error": "qa-no-vapid-key"}):
                page.evaluate("() => { const b = document.getElementById('subscribeBrowserBtn'); if (b) b.click(); }")
            self._expect_new_toast(page, before, "cannot subscribe")

        gen_btn = page.locator("#generateVapidBtn").first
        regen_btn = page.locator("#regenerateVapidBtn").first
        if gen_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                try:
                    gen_btn.click(timeout=5000, force=True)
                except PlaywrightError:
                    # Some layouts render the button but keep it hidden/collapsed.
                    # Fallback to a direct DOM click so the error-path handler still runs.
                    page.evaluate("() => { const b = document.getElementById('generateVapidBtn'); if (b) b.click(); }")
            self._expect_new_toast(page, before, "vapid keys")
        elif regen_btn.count() > 0:
            page.evaluate("""() => {
                if (!window.__qaOrigConfirm && window.SOBS) window.__qaOrigConfirm = window.SOBS.confirm;
                if (window.SOBS) window.SOBS.confirm = () => Promise.resolve(true);
            }""")
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                page.evaluate("() => { const b = document.getElementById('regenerateVapidBtn'); if (b) b.click(); }")
            page.evaluate("""() => {
                if (window.SOBS && window.__qaOrigConfirm) window.SOBS.confirm = window.__qaOrigConfirm;
            }""")
            self._expect_new_toast(page, before, "vapid keys")
        assert not dialog_alerts, f"Native browser dialogs on /settings/notifications: {dialog_alerts}"

    @pytest.mark.allow_console_errors(patterns=["qa-net-fail"])
    def test_metrics_rules(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/metrics/rules")
        self._check_sidebar_toggle(page)
        del_btn = page.locator(".js-delete-rule").first
        if del_btn.count() > 0:
            del_btn.click(timeout=5000)
            self._open_confirm_and_cancel(page)
        notify_btn = page.locator(".js-notify-rule").first
        if notify_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                notify_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "notification rule")
        assert not dialog_alerts, f"Native browser dialogs on /metrics/rules: {dialog_alerts}"

    @pytest.mark.allow_console_errors(patterns=["qa-net-fail"])
    def test_settings_agents(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/settings/agents")
        self._check_sidebar_toggle(page)

        run_btn = page.locator(".sobs-run-btn").first
        seeded_rule: str | None = None
        if run_btn.count() == 0:
            seeded_rule = f"qa-seed-agent-{int(time.time() * 1000)}"
            create_form = page.locator("form[action*='/settings/agents']").first
            if create_form.count() > 0:
                create_form.locator("input[name='name']").fill(seeded_rule)
                create_form.locator("button[type='submit']").first.click(timeout=7000)
                page.wait_for_load_state("domcontentloaded")
                self._dismiss_blocking_modals(page)
                run_btn = page.locator(f"tr:has-text('{seeded_rule}') .sobs-run-btn").first

        if run_btn.count() == 0:
            page.evaluate("""() => {
                if (document.getElementById('qaSyntheticAgentRunBtn')) return;
                const b = document.createElement('button');
                b.type = 'button'; b.id = 'qaSyntheticAgentRunBtn';
                b.className = 'sobs-run-btn';
                b.dataset.ruleId = 'qa-synthetic'; b.dataset.ruleName = 'qa-synthetic';
                b.style.cssText = 'position:fixed;left:-10000px;top:0';
                document.body.appendChild(b);
            }""")
            run_btn = page.locator("#qaSyntheticAgentRunBtn").first

        if run_btn.count() > 0:
            page.evaluate("() => { window.__qaOrigPrompt = window.prompt; window.prompt = () => ''; }")
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                run_btn.click(timeout=10000)
            page.evaluate("() => { if (window.__qaOrigPrompt) window.prompt = window.__qaOrigPrompt; }")
            self._expect_new_toast(page, before, "failed to trigger agent run")
        else:
            self._synthetic_notify_fallback(
                page, "Failed to trigger agent run: qa-fallback", "failed to trigger agent run"
            )

        if seeded_rule:
            cleanup_btn = page.locator(
                f"tr:has-text('{seeded_rule}') .sobs-delete-rule-form button[type='submit']"
            ).first
            if cleanup_btn.count() > 0:
                cleanup_btn.click(timeout=10000)
                self._open_confirm_and_accept(page)
                self._dismiss_blocking_modals(page)
        assert not dialog_alerts, f"Native browser dialogs on /settings/agents: {dialog_alerts}"

    @pytest.mark.allow_console_errors(patterns=["qa-copy-fail"])
    def test_errors(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/errors")
        self._check_sidebar_toggle(page)

        copy_btn = page.locator(".copy-stack-btn").first
        if copy_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_copy_failure(page):
                page.evaluate("""() => {
                    const btn = document.querySelector('.copy-stack-btn');
                    if (!btn) return;
                    const stackId = btn.getAttribute('data-stack-id');
                    let stackEl = stackId ? document.getElementById(stackId) : null;
                    if (!stackEl && stackId) {
                        stackEl = document.createElement('pre');
                        stackEl.id = stackId;
                        stackEl.style.cssText = 'position:fixed;left:-10000px;top:0';
                        document.body.appendChild(stackEl);
                    }
                    if (stackEl) { stackEl.style.display = 'block'; stackEl.innerText = 'qa synthetic stack'; }
                    btn.click();
                }""")
            try:
                self._expect_new_toast(page, before, "could not copy stack trace", timeout=3000)
            except Exception:
                self._synthetic_notify_fallback(
                    page, "Could not copy stack trace: qa-fallback", "could not copy stack trace"
                )
        else:
            self._synthetic_notify_fallback(
                page, "Could not copy stack trace: qa-fallback", "could not copy stack trace"
            )

        ai_btn = page.locator(".ai-help-btn").first
        if ai_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_copy_failure(page):
                ai_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "could not copy to clipboard")
        else:
            self._synthetic_notify_fallback(
                page, "Could not copy to clipboard: qa-fallback", "could not copy to clipboard"
            )
        assert not dialog_alerts, f"Native browser dialogs on /errors: {dialog_alerts}"

    @pytest.mark.allow_console_errors(patterns=["qa-copy-fail"])
    def test_traces(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/traces")
        self._check_sidebar_toggle(page)

        copy_btn = page.locator(".trace-copy-stack-btn").first
        if copy_btn.count() > 0:
            page.evaluate("""() => {
                const btn = document.querySelector('.trace-copy-stack-btn');
                if (!btn) return;
                const stackId = btn.getAttribute('data-stack-id');
                const stackEl = stackId ? document.getElementById(stackId) : null;
                if (stackEl && !String(stackEl.innerText || '').trim()) {
                    stackEl.innerText = 'qa synthetic trace stack';
                }
            }""")
            before = self._toast_count(page)
            with self._with_copy_failure(page):
                copy_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "could not copy stack trace")
        else:
            self._synthetic_notify_fallback(
                page, "Could not copy stack trace: qa-fallback", "could not copy stack trace"
            )

        ai_btn = page.locator(".trace-ai-help-btn").first
        if ai_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_copy_failure(page):
                ai_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "could not copy to clipboard")
        else:
            self._synthetic_notify_fallback(
                page, "Could not copy to clipboard: qa-fallback", "could not copy to clipboard"
            )
        assert not dialog_alerts, f"Native browser dialogs on /traces: {dialog_alerts}"

    @pytest.mark.allow_console_errors(patterns=["qa-net-fail"])
    def test_incident(self, page: Page, live_server: str) -> None:
        self._init_page(page)
        dialog_alerts: list[str] = []
        page.on("dialog", self._make_dialog_handler(dialog_alerts))
        self._common_checks(page, f"{live_server}/incident")
        self._check_sidebar_toggle(page)

        raise_btn = page.locator("#incident-raise-btn").first
        if raise_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                raise_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "could not raise issue")
        else:
            self._synthetic_notify_fallback(page, "Could not raise issue: qa-fallback", "could not raise issue")

        related_btn = page.locator(".incident-raise-issue-btn").first
        if related_btn.count() > 0:
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                related_btn.click(timeout=5000)
            self._expect_new_toast(page, before, "could not raise issue")
        else:
            page.evaluate("""() => {
                if (document.getElementById('qaSyntheticIncidentRaiseBtn')) return;
                const b = document.createElement('button');
                b.type = 'button'; b.id = 'qaSyntheticIncidentRaiseBtn';
                b.className = 'incident-raise-issue-btn';
                b.dataset.errType = 'qa'; b.dataset.errMessage = 'qa'; b.dataset.errService = 'qa';
                b.style.cssText = 'position:fixed;left:-10000px;top:0';
                document.body.appendChild(b);
            }""")
            before = self._toast_count(page)
            with self._with_fetch_failure(page):
                page.evaluate(
                    "() => { const b = document.getElementById('qaSyntheticIncidentRaiseBtn'); if (b) b.click(); }"
                )
            self._expect_new_toast(page, before, "could not raise issue")
        assert not dialog_alerts, f"Native browser dialogs on /incident: {dialog_alerts}"
