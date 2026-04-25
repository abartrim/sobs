# Contributing

## Branching Policy (Required)

- Do not commit directly to `main`.
- Do not push directly to `main`.
- All work must use this flow: Issue -> new branch -> pull request -> review -> merge.
- Use branch names like `issue-<number>-<short-description>`.
- Every PR should reference its issue and include test/validation notes.

## Local Setup

Use a virtual environment and install development dependencies:

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt -r requirements-integration.txt
pip install black isort flake8 mypy djlint
```

## Coverage

`requirements-integration.txt` includes `pytest-cov` and `diff-cover`.

Run unit tests with a terminal coverage summary and an XML report:

```bash
pytest tests --ignore=tests/test_integration.py \
    --cov=app --cov=masking --cov=mcp \
    --cov-report=term-missing \
    --cov-report=xml:coverage.xml
```

To check coverage only on lines changed by the current branch (useful before
opening a PR):

```bash
diff-cover coverage.xml --compare-branch=origin/main
```

`diff-cover` exits non-zero when any changed line is uncovered, so it can gate
a PR locally in the same way CI does.  Coverage configuration (omit patterns,
exclusion rules) lives in the `[tool.coverage.*]` sections of `pyproject.toml`.

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

And on staged Jinja templates in `templates/*.html`:

- `python3 scripts/run_djlint.py --reformat --lint`

The helper lints all matched templates, but only reformats/checks explicitly targeted or branch-changed templates that do not embed Jinja inside script-heavy blocks.

If formatters update files, the hook re-stages those files automatically.

## Manual Checks

Run these before opening or updating a PR:

```bash
isort *.py tests/ scripts
black *.py tests/ scripts
flake8 *.py tests/ scripts
mypy app.py tests scripts
python3 scripts/run_djlint.py --reformat --lint templates
python3 scripts/run_djlint.py --check --lint templates
pytest tests/
```

On a clean tree, `--check` applies only to templates changed on the current branch. To format a specific file directly, pass the file path instead of the `templates` directory.

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

## Releasing

Releases are driven by a Git tag pushed to GitHub. CI picks up any tag matching `v*`, runs the full test pipeline, then builds and publishes a multi-arch Docker image stamped with the version.

### Steps

1. **Ensure `main` is green** — all CI checks must pass before tagging.

2. **Create and push a version tag:**

   ```bash
   git tag v1.2.3
   git push origin v1.2.3
   ```

3. **Create a GitHub Release** from that tag (via the GitHub UI or CLI). The release description becomes the public changelog entry.

   ```bash
   gh release create v1.2.3 --title "v1.2.3" --notes "Release notes here"
   ```

4. **CI publishes the image automatically.** The `docker` job in `.github/workflows/ci.yml` detects `refs/tags/v*`, passes `SOBS_BUILD_VERSION=v1.2.3` as a Docker build arg, and pushes to GHCR with both the version tag and a new `latest`:

   - `ghcr.io/abartrim/sobs:v1.2.3`
   - `ghcr.io/abartrim/sobs:latest`

5. **Verify** the version appears in the sidebar footer of a freshly pulled container:

   ```bash
   docker pull ghcr.io/abartrim/sobs:v1.2.3
   docker run -p 44317:4317 ghcr.io/abartrim/sobs:v1.2.3
   # Open http://localhost:44317 — sidebar footer should show "v1.2.3"
   ```

### Version format

Use [Semantic Versioning](https://semver.org/): `vMAJOR.MINOR.PATCH` (e.g. `v1.2.3`). Pre-release suffixes like `v1.2.3-beta` are supported. Images built from `main` without a tag show `dev` in the sidebar.