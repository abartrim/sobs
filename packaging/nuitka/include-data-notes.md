# Included runtime data for Nuitka standalone build

The build script includes only the runtime files needed by the packaged app:

- `templates/` – required by Quart/Jinja route rendering.
- `static/` – required for UI assets and `/static/*` routes (including RUM bundles and chart type catalog JSON).

Runtime-generated data is intentionally **not** bundled:

- local `data/` databases and backups
- `.git/`
- virtualenvs
- tests and local artifacts

Use `SOBS_DATA_DIR` at runtime (for example in `smoke-test.sh`) so packaged runs write state to an explicit local directory.
