# Contributing

## Local Setup

Use a virtual environment and install development dependencies:

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt -r requirements-integration.txt
pip install black isort flake8 mypy
```

## Pre-Commit Hook

This repository ships a version-controlled Git hook at `.githooks/pre-commit`.

Enable it once per clone:

```bash
git config core.hooksPath .githooks
chmod +x .githooks/pre-commit
```

On each commit, the hook runs these checks on staged Python files:

- `isort`
- `black`
- `flake8`
- `mypy`

If formatters update files, the hook re-stages those Python files automatically.

## Manual Checks

Run these before opening or updating a PR:

```bash
isort *.py tests/ scripts
black *.py tests/ scripts
flake8 *.py tests/ scripts
mypy app.py tests scripts
pytest tests/
```

## Regenerating Docs Screenshots

Use the integration screenshot suite to refresh UI screenshots used in docs/help:

```bash
python -m pytest tests/test_integration.py -q -k "TestScreenshots"
```

Output files are written to `tests/screenshots/`.

The screenshot harness disables the first-run tour (`SOBS_ENABLE_FIRST_RUN_TOUR=0`) and also force-dismisses any visible tour modal before capture so docs screenshots are not obscured.

To sync images used by in-app help pages:

```bash
cp tests/screenshots/dashboard.png static/help/dashboard.png
cp tests/screenshots/ai.png static/help/ai.png
cp tests/screenshots/logs.png static/help/logs.png
cp tests/screenshots/traces.png static/help/traces.png
cp tests/screenshots/traces_drilldown.png static/help/traces_drilldown.png
cp tests/screenshots/query.png static/help/query.png
cp tests/screenshots/summary.png static/help/summary.png
cp tests/screenshots/summary_ai_assistant.png static/help/summary_ai_assistant.png
```