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


def _boot_app_db(data_dir: Path | None = None):
    # Pin the parity env (auth=none, fixed secret, etc.) before import.
    for line in (TOOLS / "parity_env.sh").read_text().splitlines():
        line = line.strip()
        if line.startswith("export ") and "=" in line:
            k, v = line[len("export ") :].split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))
    os.environ["SOBS_PARITY"] = "1"
    os.environ["SOBS_DATA_DIR"] = str(data_dir or FIXTURE_DIR)

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


# App-registry fixtures (the example seeder leaves sobs_apps/releases/artifacts empty). These
# drive the /v1 registry serialize + found paths. Deterministic ids/timestamps so goldens
# reproduce. DateTime64(3) columns are pre-normalized ("YYYY-MM-DD HH:MM:SS.ffffff").
APP_ID = "a0000000000000000000000000000a01"
REL_ID = "a0000000000000000000000000000b02"
ART_ID = "a0000000000000000000000000000c03"


def seed_apps(db) -> None:
    _insert(
        db,
        "sobs_apps",
        [
            {
                "Id": APP_ID,
                "Name": "Checkout Service",
                "Slug": "checkout-service",
                "OwnerTeam": "payments",
                "RepoUrl": "https://github.com/acme/checkout",
                "DefaultEnvironment": "production",
                "Enabled": 1,
                "MetadataJson": '{"tier":"gold"}',
                "IsDeleted": 0,
                "Version": 1704164645000,
                "CreatedAt": _TS,
                "UpdatedAt": _TS,
            }
        ],
    )
    _insert(
        db,
        "sobs_app_releases",
        [
            {
                "Id": REL_ID,
                "AppId": APP_ID,
                "ReleaseVersion": "1.2.0",
                "CommitSha": "abc123def456",
                "BuildId": "build-42",
                "Environment": "production",
                "ReleasedAt": _TS,
                "MetadataJson": '{"channel":"stable"}',
                "IsDeleted": 0,
                "Version": 1704164645000,
            }
        ],
    )
    _insert(
        db,
        "sobs_release_artifacts",
        [
            {
                "Id": ART_ID,
                "ReleaseId": REL_ID,
                "ArtifactType": "binary",
                "Name": "checkout-linux-amd64",
                "ContentType": "application/octet-stream",
                "Size": 1048576,
                "StorageRef": "s3://artifacts/checkout/1.2.0/linux-amd64",
                "ChecksumSha256": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0",
                "Platform": "linux",
                "Architecture": "amd64",
                "MetadataJson": "{}",
                "UploadedAt": _TS,
                "IsDeleted": 0,
                "Version": 1704164645000,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_apps FINAL")
    db.execute("OPTIMIZE TABLE sobs_app_releases FINAL")
    db.execute("OPTIMIZE TABLE sobs_release_artifacts FINAL")


def seed_rules(db) -> None:
    # One agent rule + one tag rule with FIXED ids so the /settings/agents|tags delete
    # success paths have a stable record to soft-delete. These ids never collide with the
    # frozen-uuid example seeds (00000000-...). The agents/tags readers (and the few other
    # routes that load these tables) render these rows — captured goldens reflect them.
    _insert(
        db,
        "sobs_agent_rules",
        [
            {
                "Id": "b0000000-0000-4000-8000-000000000a01",
                "Name": "Release Gate Watch",
                "Description": "Seeded agent rule for delete-path parity",
                "TriggerType": "manual",
                "TriggerRefId": "",
                "TriggerState": "any",
                "Actions": "analyze",
                "RateLimitMinutes": 60,
                "IsEnabled": 1,
                "IsDeleted": 0,
                "Version": 1704164645000,
            }
        ],
    )
    _insert(
        db,
        "sobs_tag_rules",
        [
            {
                "Id": "b0000000-0000-4000-8000-000000000b01",
                # MatchValue is a service that exists in NO fixture record, so this rule tags
                # nothing — it ripples only into the tag-rule LISTING pages, never the
                # record-listing pages that apply rules.
                "Name": "Inert Sample Tagger",
                "RecordTypes": "log",
                "MatchField": "service_name",
                "MatchOperator": "eq",
                "MatchValue": "no-such-service-zzz",
                "MatchAttrKey": "",
                "TagKey": "team",
                "TagValue": "payments",
                "ConditionsJson": (
                    '[{"match_field": "service_name", "match_operator": "eq", '
                    '"match_value": "no-such-service-zzz", "match_attr_key": ""}]'
                ),
                "IsDeleted": 0,
                "Version": 1704164645000,
            }
        ],
    )


def seed_extra(app, db) -> None:
    seed_reports(db)
    seed_rum_sessions(db)
    # seed_rules: enabled once the Go agents/tags readers render real rule rows (see
    # loadAgentRulesCtx/loadTagRulesCtx). Until then, seeding would diverge those readers.
    seed_rules(db)
    # NOTE: sobs_apps/releases/artifacts are intentionally NOT seeded as persistent baseline
    # rows. The /settings/data-management page reports system.parts byte sizes, which chdb
    # varies across boots; adding app tables there made the size masking non-robust. The v1
    # registry serialize/GET paths are instead exercised by create-then-read-back manifest
    # entries (in the mutator section, after every reader) so data-management & repositories
    # readers see an empty registry — their proven-stable state. seed_apps() is retained for
    # any future opt-in but is not called.


# ---- per-profile seeds -------------------------------------------------------------
# Rows seeded ONLY for a specific capture/replay profile (see profiles.py / parity_check),
# NOT into the base fixture — so a "found"/non-empty branch can be exercised without the
# seeded state rippling into every base reader (the trap that forced the notification cluster
# revert). Applied by `seed_fixtures.py --only-profile <name> [--data-dir <dir>]`: it boots the
# app on the EXISTING db (schema + example seeder are idempotent) and inserts just these rows.


def seed_agent_run(db) -> None:
    # One completed, not-yet-dismissed agent run so /api/agent/runs serializes a real row and
    # /api/agent/runs/<id>/dismiss can flip it. Version is 1ms BELOW the dismiss re-insert
    # (1704164645000) so the dismissed row wins the ReplacingMergeTree FINAL.
    _insert(
        db,
        "sobs_agent_runs",
        [
            {
                "Id": "a9000000000000000000000000000001",
                "RuleId": "b0000000000000000000000000000a01",
                "RuleName": "Parity Agent Rule",
                "TriggerContext": "error spike on checkout",
                "Status": "completed",
                "GuardDecision": "allow",
                "DlpResult": "clean",
                "Analysis": "Investigated the error spike.",
                "Suggestion": "Roll back the last deploy.",
                "GithubIssueUrl": "",
                "ErrorMessage": "",
                "CreatedAt": _TS,
                "CompletedAt": _TS,
                "IsDismissed": 0,
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_agent_runs FINAL")


PROFILE_SEEDS = {
    "agentrun": seed_agent_run,
}


def _optimize_all(db) -> None:
    # Force a full merge of EVERY table to one part. The example seeder leaves several tables
    # (anomaly_rules, chart_configs, ...) as multiple un-merged parts, so `sum(rows) FROM
    # system.parts` — the data-management page's unmasked total_rows — is merge-timing-dependent
    # (a fresh boot can see transient pre-merge active parts). OPTIMIZE FINAL collapses each
    # table to its deduplicated single part, making total_rows a stable, capture-reproducible
    # value that the Go replay reads identically.
    rows = db.execute(
        "SELECT DISTINCT table FROM system.parts WHERE active=1 AND database=currentDatabase()"
    ).fetchall()
    for row in rows:
        table = str(row[0] if not isinstance(row, dict) else row.get("table"))
        db.execute(f"OPTIMIZE TABLE {table} FINAL")


def main() -> int:
    import argparse

    ap = argparse.ArgumentParser()
    ap.add_argument("--only-profile", help="insert ONLY this profile's seed rows into an already-seeded db")
    ap.add_argument("--data-dir", help="target chdb data dir (default migration/fixtures/data)")
    args = ap.parse_args()

    if args.only_profile:
        seed = PROFILE_SEEDS.get(args.only_profile)
        if not seed:
            print(f"No profile seed for '{args.only_profile}' (no-op).")
            return 0
        data_dir = Path(args.data_dir) if args.data_dir else FIXTURE_DIR
        _app, db = _boot_app_db(data_dir)  # schema + example seeder are idempotent on an existing db
        seed(db)
        print(f"Seeded profile '{args.only_profile}' rows into {data_dir}")
        return 0

    _fresh_dir()
    app, db = _boot_app_db()
    seed_extra(app, db)
    print(f"Seeded fixture DB at {FIXTURE_DIR}")
    print("Baseline = app example seeder (dashboards/rules/metrics) + extra reports/RUM rows.")
    print("Next: re-capture affected goldens (capture_routes.py) then parity_check.py.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
