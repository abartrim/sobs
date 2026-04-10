from typing import Any

import pytest

sync_playwright: Any | None

try:
    from playwright.sync_api import sync_playwright as _real_sync_playwright

    sync_playwright = _real_sync_playwright
except ModuleNotFoundError:  # pragma: no cover - depends on environment
    sync_playwright = None


@pytest.fixture(scope="function")
def page(request: pytest.FixtureRequest):
    """Local Playwright page fixture for screenshot/integration tests.

    This avoids requiring pytest-playwright plugin wiring while keeping
    test_integration.py unchanged.
    """
    if sync_playwright is None:
        pytest.skip("Playwright is not installed in this test environment")

    allow_marker = request.node.get_closest_marker("allow_console_errors")
    allow_all = bool(allow_marker and allow_marker.kwargs.get("all", False))
    allow_patterns: list[str] = []
    if allow_marker:
        allow_patterns.extend(str(v) for v in allow_marker.args if isinstance(v, str))
        kw_patterns = allow_marker.kwargs.get("patterns")
        if isinstance(kw_patterns, (list, tuple, set)):
            allow_patterns.extend(str(v) for v in kw_patterns)
        elif isinstance(kw_patterns, str):
            allow_patterns.append(kw_patterns)

    with sync_playwright() as p:
        browser = None
        try:
            browser = p.chromium.launch(headless=True)
        except Exception as exc:  # pragma: no cover - environment dependent
            pytest.skip(f"Playwright Chromium unavailable: {exc}")
        context = browser.new_context(viewport={"width": 1440, "height": 900})
        page_obj = context.new_page()
        console_errors: list[str] = []
        page_errors: list[str] = []

        def _is_allowed(entry: str) -> bool:
            if allow_all:
                return True
            return any(pattern in entry for pattern in allow_patterns)

        def _on_console(msg: Any) -> None:
            if getattr(msg, "type", "") != "error":
                return
            text = ""
            try:
                text = msg.text or ""
            except Exception:
                text = ""
            location = ""
            try:
                loc = msg.location or {}
                url = str(loc.get("url") or "")
                line = str(loc.get("lineNumber") or "")
                col = str(loc.get("columnNumber") or "")
                if url:
                    location = f" @ {url}:{line}:{col}"
            except Exception:
                location = ""
            console_errors.append(f"{text}{location}".strip())

        def _on_page_error(err: Exception) -> None:
            page_errors.append(str(err))

        page_obj.on("console", _on_console)
        page_obj.on("pageerror", _on_page_error)
        try:
            yield page_obj
        finally:
            unexpected_console = [entry for entry in console_errors if not _is_allowed(entry)]
            unexpected_page_errors = [entry for entry in page_errors if not _is_allowed(entry)]
            context.close()
            browser.close()
            if unexpected_console or unexpected_page_errors:
                details: list[str] = []
                if unexpected_console:
                    details.append("console errors:")
                    details.extend(f"- {entry}" for entry in unexpected_console)
                if unexpected_page_errors:
                    details.append("page errors:")
                    details.extend(f"- {entry}" for entry in unexpected_page_errors)
                if allow_patterns and not allow_all:
                    details.append(f"allowed patterns: {allow_patterns}")
                pytest.fail("Unexpected browser console/page errors detected\n" + "\n".join(details))
