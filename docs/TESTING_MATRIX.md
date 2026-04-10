# Testing Matrix

This document defines the recommended split between browser E2E tests and Python tests in this repository.

## Principles

1. Use Node Playwright as the single source of truth for browser UI behavior and visual capture.
2. Use Python tests for backend correctness, API contracts, data logic, and integration checks.
3. Avoid duplicating the same user journey in both Python and Node Playwright.
4. Keep one smoke path fast and deterministic for PRs; run broader suites on schedule/nightly.

## Current Test Layers

### Layer 1: UI Behavior + Visual Evidence (Node Playwright)

Primary scope:
- Toast and confirm modal behavior
- Non-destructive UI state changes and reverts
- Screenshot artifacts for review/debugging
- Modal/load-order regressions

Entry point:
- [scripts/playwright-toast-qa.mjs](scripts/playwright-toast-qa.mjs)

Command:
- `BASE_URL=http://127.0.0.1:<port> npm run qa:ui:playwright`

Artifacts:
- `tests/screenshots/playwright-toast-qa-<timestamp>/`

### Layer 2: Backend + Integration (Pytest)

Primary scope:
- Route/service correctness
- Data model and storage behavior
- Integration behavior that does not require browser rendering

Entry points:
- [tests/test_app.py](tests/test_app.py)
- [tests/test_integration.py](tests/test_integration.py)

Typical command:
- `pytest -q`

## Ownership and Boundaries

1. Node Playwright owns UI assertions:
- DOM behavior
- visual/modal states
- animation/transition timing correctness

2. Python tests own non-UI assertions:
- status codes and response payloads
- DB/business logic outcomes
- server-side edge cases

3. If a test needs screenshots, keep it in Node Playwright.
4. If a test can be validated without a browser, prefer Python.

## CI Test Tiers

### PR Required (Fast)

1. Python quick checks:
- `pytest -q`

2. UI smoke (headless):
- `BASE_URL=<ephemeral-app-url> npm run qa:ui:playwright`

Target:
- keep under ~10 minutes total

### Nightly / Scheduled (Broader)

1. Full Python suite with heavier integration data.
2. UI Playwright run with retained screenshot artifacts.
3. Optional cross-browser Playwright expansion if needed.

## Suggested Conventions

1. Tag tests by intent, not by language:
- smoke
- integration
- visual
- destructive

2. Keep UI scenarios deterministic:
- seed minimal data in test flow
- cleanup seeded records in same run

3. Keep non-destructive by default:
- use change-and-revert patterns for setting toggles

## Decision Rule for New Tests

1. Is browser rendering, modal state, animation, or screenshot needed?
- Yes: add to Node Playwright.

2. Is this API/data behavior without browser-specific UI assertions?
- Yes: add to Python pytest.

3. Does a similar scenario already exist in one layer?
- Extend existing layer instead of duplicating in both.

## Future Consolidation Option

If desired later, reduce dual-Playwright maintenance by keeping only Node Playwright for browser E2E and migrating any overlapping Python Playwright tests to either:
1. Node Playwright (for UI behavior), or
2. Python pytest (for backend-only behavior).
