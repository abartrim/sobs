# SOBS Go integration tests

Go port of `tests/test_integration.py`. Black-box tests that build and launch
the Go server (`gosobs/`) and exercise it over HTTP and a real headless browser.

This is a **separate Go module** (`sobs-integration`) so the browser-automation
dependency (go-rod) stays out of the server module's `go.mod` / `vendor/`.

## Run

```sh
cd gosobs/integration
go test -v                 # whole suite
go test -v -run 'TestIntegration/CurlExamples'   # one group
```

`TestMain` builds `../` into a temp binary, launches it with a throwaway
`SOBS_DATA_DIR` on port 15317 (cwd = repo root so it finds `templates/` and
`static/`), waits for `/health`, then shares one headless browser across tests.

On first run go-rod downloads a Chromium build to `~/.cache/rod` (needs network
once). No system Chrome required.

## Layout

| File | Mirrors |
|------|---------|
| `harness_test.go` | session live-server fixture + `page` (console/dialog capture) |
| `helpers_test.go` | HTTP helpers, OTLP payload builders, page-action wrappers |
| `http_test.go` | `TestCurlExamples`, `TestPythonOtelExample`, `TestFlaskExample`, `TestNodeJsExample`, `TestDataVisibleInUI` |
| `screenshots_test.go` | `TestScreenshots` (PNGs written to `screenshots/`) |
| `uiqa_test.go` + `uiqa_helpers_test.go` | `TestUIQA` behavioral checks |
| `seed_test.go` | sample-traffic generator (replaces `scripts/load_example.py`) |

Tests run as ordered subtests under `TestIntegration` to mirror pytest's
sequential class execution against one shared server (examples post first, then
the screenshot/UIQA groups seed traffic).

## Playwright → go-rod mapping

- `page.evaluate(js)` → `page.MustEval(js, args...)` — JS blocks port verbatim.
- `page.wait_for_function(js)` → `page.MustWait(js, args...)`.
- `add_init_script` → `MustEvalOnNewDocument`.
- console-error / pageerror capture: instead of the CDP Runtime domain (which
  conflicts with `WaitLoad` and throws the flaky `-32000 Object reference chain
  is too long`), `console.error` and `window.onerror` are hooked via an init
  script and read back at teardown (`consoleHookScript`).
- `tr:has-text('X') sel` (Playwright-only) → `rowEl(rowText, sel)`.

## Environment knobs

`SOBS_SCREENSHOT_SEED_TOTAL` (240), `SOBS_SCREENSHOT_SEED_WORKERS` (24),
`SOBS_UIQA_SEED_TOTAL` (64), `SOBS_UIQA_SEED_WORKERS` (8).
