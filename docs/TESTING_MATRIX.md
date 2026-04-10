# Testing Matrix

This document defines the testing split for SOBS after consolidating browser QA into Python Playwright integration tests.

## Principles

1. Use Python pytest + Playwright as the source of truth for UI behavior checks.
2. Keep backend/API correctness tests in Python unit/integration tests.
3. Avoid duplicate journeys across layers unless there is a clear reliability reason.
4. Keep PR-required checks deterministic and non-destructive.

## Current Test Layers

### Layer 1: Backend + API Correctness (Pytest)

Primary scope:
- Route/service correctness
- Data model/storage behavior
- API contract and edge-case handling

Entry points:
- [tests/test_app.py](tests/test_app.py)
- targeted backend tests under [tests/](tests/)

### Layer 2: Integration + Browser UI QA (Pytest + Playwright)

Primary scope:
- UI notify/confirm behavior
- Modal layering and interaction behavior
- Non-destructive UI change-and-revert flows
- Screenshot artifacts from integration runs

Entry point:
- [tests/test_integration.py](tests/test_integration.py)

Markers:
- `integration`
- `uiqa`

## Ownership and Boundaries

1. Python Playwright (`tests/test_integration.py`) owns browser-visible UI behavior assertions.
2. Backend tests own status code, payload, business logic, and storage assertions.
3. Prefer backend tests when browser rendering is not required.

## CI Test Tiers

### PR Required

1. Lint/type checks.
2. Unit tests.
3. Integration tests (includes Playwright-backed UIQA checks).

Current CI command:
- `pytest tests/test_integration.py -v`

### Optional Local Fast Target

Run only browser UI behavior checks:
- `pytest tests/test_integration.py -m uiqa -v`

## Suggested Conventions

1. Tag tests by intent:
- `integration`
- `uiqa`

2. Keep UI scenarios deterministic and non-destructive:
- seed only the minimum required data
- cleanup seeded entities inside the same test flow

3. Keep data-visibility assertions explicit:
- tests that validate producer flows should not be auto-masked by global seed fixtures

## Decision Rule for New Tests

1. If browser rendering/state/interaction is required, add to Playwright-backed pytest integration tests.
2. If browser rendering is not required, prefer backend/unit pytest tests.
3. Extend an existing layer before introducing a new harness.
