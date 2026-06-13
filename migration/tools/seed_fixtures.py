#!/usr/bin/env python3
"""Build the deterministic chdb fixture dataset for parity capture & replay.

The golden corpus is only meaningful if both Python and Go read the SAME, FIXED data.
This script is the CANONICAL, reproducible builder for migration/fixtures/data/sobs.chdb:

  1. Boot app.py in parity mode (determinism frozen) and call get_db(), which applies the
     production SCHEMA and runs the app's OWN example seeder (anomaly rules, an example
     dashboard + charts, example metrics, app-release registry). That seeded content is
     the realistic baseline — it is exactly what a fresh production install ships with.
  2. Insert a few extra DETERMINISTIC rows for tables the example seeder leaves empty but
     that captured routes read (saved reports, RUM web-traffic sessions). Grow this as
     more routes come online — only seed what a captured route reads.
  3. OPTIMIZE ... FINAL the ReplacingMergeTree tables so FINAL reads are stable.

Determinism: app import happens FIRST, then determinism.install() (freezing before the
pandas/numpy C-extension import hangs). get_db()'s example seeder therefore runs with
frozen uuid4/time, so the baseline is byte-reproducible. The app's _seed_*_if_missing
helpers key off natural columns (Name/Title), so re-booting never duplicates rows.

Run from repo root:  .venv/bin/python migration/tools/seed_fixtures.py
"""

from __future__ import annotations

import os
import shutil
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
TOOLS = Path(__file__).resolve().parent
FIXTURE_DIR = REPO / "migration" / "fixtures" / "data"

sys.path.insert(0, str(REPO))
sys.path.insert(0, str(TOOLS))


def _fresh_dir() -> None:
    if FIXTURE_DIR.exists():
        shutil.rmtree(FIXTURE_DIR)
    FIXTURE_DIR.mkdir(parents=True, exist_ok=True)
    (FIXTURE_DIR / "rum_assets").mkdir(parents=True, exist_ok=True)


def _boot_app_db():
    # Pin the parity env (auth=none, fixed secret, etc.) before import.
    for line in (TOOLS / "parity_env.sh").read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))
    os.environ["SOBS_PARITY"] = "1"
    os.environ["SOBS_DATA_DIR"] = str(FIXTURE_DIR)

    import determinism

    import app as app_module  # import FIRST

    determinism.install()  # ...then freeze entropy
    db = app_module.get_db()  # applies SCHEMA + runs the example seeder deterministically
    return app_module, db


# ---- Extra fixture rows ------------------------------------------------------------
# Direct JSONEachRow inserts (what _insert_rows_json_each_row does, minus the
# _WRITABLE_TABLES guard) with pre-normalized DateTime64 strings inside the determinism
# window. Every value is fixed so the on-disk bytes — and thus the goldens — reproduce.

_TS = "2024-01-02 03:00:00.000000"  # within the determinism window


def _insert(db, table: str, rows: list[dict]) -> None:
    import json

    payload = "\n".join(json.dumps(r, ensure_ascii=False) for r in rows)
    db.execute(f"INSERT INTO {table} FORMAT JSONEachRow\n" + payload)


def seed_reports(db) -> None:
    # Two saved reports with nested FiltersJson — exercises /api/reports' filter parsing
    # and the PageType, Name ORDER BY (logs < traces).
    _insert(
        db,
        "sobs_reports",
        [
            {
                "Id": "11111111-0000-4000-8000-0000000000a1",
                "Name": "Checkout errors",
                "Description": "ERROR-level checkout logs",
                "PageType": "logs",
                "FiltersJson": '{"service":"checkout","severity":["ERROR"],"limit":100}',
                "IsDeleted": 0,
                "Version": 1704164645000,
            },
            {
                "Id": "22222222-0000-4000-8000-0000000000b2",
                "Name": "Slow traces",
                "Description": "Spans over 250ms",
                "PageType": "traces",
                "FiltersJson": '{"min_duration_ms":250}',
                "IsDeleted": 0,
                "Version": 1704164645000,
            },
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_reports FINAL")


def seed_rum_sessions(db) -> None:
    # RUM browser events feeding the /api/web-traffic/* aggregations. Two identical Chrome
    # rows + one Safari row => deterministic, tie-free COUNT(*) ordering.
    def attrs(browser, bver, osn, osv, tz, lang, device):
        return {
            "browser.context.browserName": browser,
            "browser.context.browserVersion": bver,
            "browser.context.osName": osn,
            "browser.context.osVersion": osv,
            "browser.context.timezone": tz,
            "browser.context.language": lang,
            "browser.context.deviceClass": device,
        }

    rows = [
        {
            "Timestamp": _TS,
            "ServiceName": "web",
            "Body": "pageview",
            "LogAttributes": attrs("Chrome", "120", "macOS", "14.2", "America/New_York", "en-US", "Desktop"),
        },
        {
            "Timestamp": _TS,
            "ServiceName": "web",
            "Body": "pageview",
            "LogAttributes": attrs("Chrome", "120", "macOS", "14.2", "America/New_York", "en-US", "Desktop"),
        },
        {
            "Timestamp": _TS,
            "ServiceName": "web",
            "Body": "pageview",
            "LogAttributes": attrs("Safari", "17.1", "iOS", "17.2", "Europe/London", "en-GB", "Mobile"),
        },
    ]
    _insert(db, "hyperdx_sessions", rows)


def seed_extra(app, db) -> None:
    seed_reports(db)
    seed_rum_sessions(db)


def main() -> int:
    _fresh_dir()
    app, db = _boot_app_db()
    seed_extra(app, db)
    print(f"Seeded fixture DB at {FIXTURE_DIR}")
    print("Baseline = app example seeder (dashboards/rules/metrics) + extra reports/RUM rows.")
    print("Next: re-capture affected goldens (capture_routes.py) then parity_check.py.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
