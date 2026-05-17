# Nuitka packaging POC for local Sobs development

This directory contains a small, reversible proof-of-concept for packaging Sobs as a standalone executable.

## Current scope

- Preferred target: standalone folder build (`dist/app.dist/sobs` in current Linux run).
- Stretch target: onefile build (`dist/sobs`) with documented status.
- Packaging entry point: `python -m sobs` (delegates to `app` runtime).

## Prerequisites

- Python 3.12+ available as `python3`.
- Runtime deps installed:
  - `pip install -r requirements.txt`
- Nuitka installed:
  - `python3 -m pip install nuitka`

Quick source-mode start using the packaging entry point:

```bash
python -m sobs
```

## Build

From repo root:

```bash
./packaging/nuitka/build-local.sh
```

Environment variables:

```bash
SOBS_BUILD_MODE=standalone        # standalone|onefile
SOBS_BUILD_CLEAN=false            # true removes scoped sobs build outputs first
SOBS_BUILD_OUTPUT_DIR=dist        # relative to repo root or absolute path
SOBS_BUILD_EXTRA_ARGS=""          # extra raw Nuitka args (space-separated)
SOBS_TELEMETRY_ENABLED=false      # documented runtime default for packaged mode
```

Examples:

```bash
# Default standalone build
./packaging/nuitka/build-local.sh

# Onefile attempt
SOBS_BUILD_MODE=onefile ./packaging/nuitka/build-local.sh

# Standalone clean rebuild with extra Nuitka options
SOBS_BUILD_CLEAN=true \
SOBS_BUILD_EXTRA_ARGS="--show-progress --show-modules" \
./packaging/nuitka/build-local.sh
```

## Smoke test

```bash
./packaging/nuitka/smoke-test.sh
```

What it validates:

1. Packaged executable exists.
2. Starts Sobs on a test host/port.
3. `/health` returns success.
4. Process stays alive briefly.
5. Process shuts down cleanly.
6. Telemetry compatibility cases:
   - `SOBS_TELEMETRY_ENABLED=false`
   - `SOBS_TELEMETRY_ENABLED=true` with `OTEL_SDK_DISABLED=true`
   - optional console telemetry startup case when `SOBS_TEST_CONSOLE_TELEMETRY=true`

Smoke-test environment variables:

```bash
SOBS_BUILD_MODE=standalone
SOBS_TEST_EXECUTABLE=             # optional explicit binary path
SOBS_TEST_HOST=127.0.0.1
SOBS_TEST_PORT=8765
SOBS_TEST_TIMEOUT_SECONDS=20
SOBS_TEST_CONSOLE_TELEMETRY=false
```

## Manual validation checklist

- [ ] Source mode starts (`python -m sobs`).
- [ ] Standalone packaged app starts.
- [ ] `/health` responds.
- [ ] Basic UI loads (`/`).
- [ ] Telemetry-disabled startup works (default/no-op path).
- [ ] `OTEL_SDK_DISABLED=true` startup works.
- [ ] Optional console telemetry startup works when dependencies are present.
- [ ] App exits cleanly.
- [ ] No local DB/secret/dev artifacts are bundled.

## Runtime data and native dependency notes

- Included runtime data is documented in `include-data-notes.md`.
- Native dependency status (notably `chdb`) is validated by smoke-test startup and should be re-checked per platform.

## POC results (initial run)

| Platform | Python version | Nuitka version | Build mode | Build command | Build duration | Output size | Startup time | Idle RSS | Telemetry mode | Known missing imports/files | Known native library issues | Smoke-test result |
|---|---|---|---|---|---:|---:|---:|---:|---|---|---|---|
| Linux x86_64 (CI sandbox) | 3.12.3 | 2.8.6 | standalone | `./packaging/nuitka/build-local.sh` | ~19m33s (clean build) | ~485 MB (`dist/app.dist`) | ~3s to `/health` | ~920-940 MiB RSS observed in startup logs | disabled by default; OTEL override case validated | None observed in initial script wiring | chDB reports transient memory warnings under sandbox limits but app remains healthy | passed |
| Linux x86_64 (CI sandbox) | 3.12.3 | 2.8.6 | onefile | `SOBS_BUILD_MODE=onefile ./packaging/nuitka/build-local.sh` | not run | n/a | n/a | n/a | n/a | not run | not run | not run |

## Notes / limitations

- macOS ARM64 is the first required local-dev target and still needs local validation.
- Onefile mode is intentionally a follow-up after standalone mode is stable.
- This POC does not add release signing/notarization or CI artifact publishing.
