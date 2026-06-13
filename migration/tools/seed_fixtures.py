#!/usr/bin/env python3
"""Build a deterministic chdb fixture dataset for parity capture & replay.

The golden corpus is only meaningful if both Python and Go read the SAME, FIXED data.
This script creates migration/fixtures/data/sobs.chdb (+ rum_assets) seeded with a
small, stable, ORDER-BY-deterministic dataset covering every table a golden route
queries. Python capture and Go replay both point SOBS_DATA_DIR at this directory
(parity_check.py copies it to a scratch dir per run so writes don't poison reruns).

Design rules:
  * Every inserted row uses fixed timestamps within the determinism window
    (around determinism.FIXED_EPOCH) so time-derived output is stable.
  * Enough rows per table to exercise pagination/empty/non-empty branches, but small.
  * Data is chosen so queries that lack a total ORDER BY still return a stable order
    (e.g. unique sortable keys). If a golden route's output order is unstable, add an
    ORDER BY assumption here, never a normalization rule.

This is intentionally a GUIDED SKELETON: it reuses app.py's own SCHEMA and insert
helpers so the fixture tables exactly match production DDL. Fill in the per-table
fixture rows as routes are brought online (you only need to seed what a captured route
reads — grow it with the ledger).

Run from repo root:  python migration/tools/seed_fixtures.py
"""

from __future__ import annotations

import os
import shutil
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
FIXTURE_DIR = REPO / "migration" / "fixtures" / "data"

sys.path.insert(0, str(REPO))
sys.path.insert(0, str(Path(__file__).resolve().parent))  # sibling tool modules


def _fresh_dir() -> None:
    if FIXTURE_DIR.exists():
        shutil.rmtree(FIXTURE_DIR)
    FIXTURE_DIR.mkdir(parents=True, exist_ok=True)
    (FIXTURE_DIR / "rum_assets").mkdir(parents=True, exist_ok=True)


def _boot_app_db():
    # Freeze first so any module-level timestamps in app.py are fixed, then import.
    os.environ["SOBS_PARITY"] = "1"
    os.environ["SOBS_DATA_DIR"] = str(FIXTURE_DIR)
    import determinism

    determinism.install()
    import app as app_module

    determinism.patch_module(app_module)
    # get_db() applies SCHEMA on first open — gives us the exact production DDL.
    db = app_module.get_db()
    return app_module, db


# ---- Fixture rows -----------------------------------------------------------------
# Add one function per table/area as routes come online. Each must be DETERMINISTIC.
# Use app's own insert helpers (e.g. _insert_rows_json_each_row) so format matches.


def seed_logs(app, db) -> None:
    rows = [
        {
            "Timestamp": "2024-01-02 03:00:00.000",
            "ServiceName": "checkout",
            "SeverityText": "ERROR",
            "Body": "payment declined",
            "TraceId": "00000000000000000000000000000001",
            "SpanId": "0000000000000001",
            # …match the real otel_logs columns; pull the exact column set from SCHEMA.
        },
        {
            "Timestamp": "2024-01-02 03:01:00.000",
            "ServiceName": "checkout",
            "SeverityText": "INFO",
            "Body": "order created",
            "TraceId": "00000000000000000000000000000002",
            "SpanId": "0000000000000002",
        },
    ]
    app._insert_rows_json_each_row(db, "otel_logs", rows)


def seed_settings(app, db) -> None:
    # Settings drive feature flags + many page branches. Seed a fixed, known config.
    # Use the app's own setter so encryption/format matches.
    pass  # TODO: app._set_app_setting(db, "key", "value") for each flag a golden needs


def seed_all(app, db) -> None:
    # Order matters only for FK-like joins; otherwise independent.
    seed_logs(app, db)
    seed_settings(app, db)
    # seed_traces(app, db); seed_metrics(app, db); seed_rum(app, db);
    # seed_reports/dashboards/tags/agents/cve/work_items/... — grow with the ledger.


def main() -> int:
    _fresh_dir()
    app, db = _boot_app_db()
    seed_all(app, db)
    # Force merges so ReplacingMergeTree FINAL reads are stable across engines.
    try:
        for t in ("otel_logs", "sobs_app_settings"):
            db.execute(f"OPTIMIZE TABLE {t} FINAL")
    except Exception as e:
        print(f"(non-fatal) OPTIMIZE skipped: {e}")
    print(f"Seeded fixture DB at {FIXTURE_DIR}")
    print("Grow seed_*() coverage as routes are captured — only seed what goldens read.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
