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


def seed_agent_rule(db) -> None:
    # One enabled "analyze"-only agent rule so POST /api/agent/runs (trigger_agent_run) can run the
    # full agent flow: guard -> LLM root-cause analysis -> record a completed run. Actions is
    # "analyze" alone, so the github-issue/DLP/dedup branch is skipped entirely.
    _insert(
        db,
        "sobs_agent_rules",
        [
            {
                "Id": "e1000000000000000000000000000001",
                "Name": "Parity Analyze Rule",
                "Description": "Analyze-only agent rule for parity.",
                "TriggerType": "manual",
                "TriggerRefId": "",
                "TriggerState": "any",
                "Actions": "analyze",
                "RateLimitMinutes": 60,
                "IsEnabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_agent_rules FINAL")


def seed_k8s(db) -> None:
    # Enable the Kubernetes health view (DB setting that Python's _kubernetes_enabled reads). The Go
    # side reads the SOBS_KUBERNETES_ENABLED boot flag from the profile env. With no k8s metrics in
    # otel_metrics, the status fetch returns the structured empty result.
    _insert(
        db,
        "sobs_app_settings",
        [{"Key": "kubernetes.enabled", "Value": "1", "UpdatedAt": _TS}],
    )
    db.execute("OPTIMIZE TABLE sobs_app_settings FINAL")


def seed_dm_backup(db) -> None:
    # Enable the data-management backup feature so backup/run + restore reach their real work.
    # No S3 bucket is configured, so both short-circuit to a deterministic message (the actual
    # BACKUP ALL TO S3 needs a real bucket).
    _insert(
        db,
        "sobs_app_settings",
        [{"Key": "data_management.backup_enabled", "Value": "1", "UpdatedAt": _TS}],
    )
    db.execute("OPTIMIZE TABLE sobs_app_settings FINAL")


def seed_repo_app(db) -> None:
    # One registered app + a release + a github token, so the /settings/repositories/<id>/...
    # actions (realtime/rotate/revoke/save/releases/delete) and github-token/validate run their
    # real branch. Version is 1ms below fixedVersionMillis so re-inserts (save/delete) win FINAL.
    _insert(
        db,
        "sobs_apps",
        [
            {
                "Id": "f1000000000000000000000000000001",
                "Name": "Widget Service",
                "Slug": "widget-service",
                "OwnerTeam": "platform",
                "RepoUrl": "https://github.com/acme/widget",
                "DefaultEnvironment": "prod",
                "Enabled": 1,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164644000,
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
                "Id": "f2000000000000000000000000000001",
                "AppId": "f1000000000000000000000000000001",
                "ReleaseVersion": "1.0.0",
                "CommitSha": "",
                "BuildId": "",
                "Environment": "prod",
                "ReleasedAt": _TS,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    _insert(
        db,
        "sobs_ai_settings",
        [{"Key": "ai.github_token", "Value": "ghp_parity_token", "IsDeleted": 0, "Version": 1704164644000}],
    )
    db.execute("OPTIMIZE TABLE sobs_apps FINAL")
    db.execute("OPTIMIZE TABLE sobs_app_releases FINAL")
    db.execute("OPTIMIZE TABLE sobs_ai_settings FINAL")


def seed_cve_osv(db) -> None:
    # One otel_logs row carrying telemetry.sdk.* resource attributes so _collect_library_inventory
    # (tier 2) yields a single library; the cve scan then queries OSV (canned) for it and records a
    # finding. No github token, so the github backfill stays the 0/0/cap no-op.
    _insert(
        db,
        "otel_logs",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "api",
                "Body": "startup",
                "ResourceAttributes": {
                    "telemetry.sdk.name": "opentelemetry",
                    "telemetry.sdk.version": "1.20.0",
                    "telemetry.sdk.language": "python",
                },
            }
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_tagauto(db) -> None:
    # 30 recent otel_logs rows for a single prod-named service so auto_tag_rules' in-window branch
    # finds EXACTLY one candidate ("log env=production", point_count=30). Seeded at now()-1h (real
    # wall-clock, NOT the frozen 2024 epoch) so the rows land inside the 24h scan window; the base
    # fixture has nothing in-window, so this is the only candidate. The candidate output
    # (service/operator/tag/count) is timestamp-independent, so capture and replay — seeded at
    # different real times — produce byte-identical goldens. SeverityNumber/EventName stay at
    # their column defaults (0 / '') so the error-branch scan excludes these rows.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'checkout-prod', 'request handled' "
        "FROM numbers(30)"
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_metricsauto(db) -> None:
    # 150 recent otel_logs rows for the EXISTING base service "web", laid out as 5 logs in each of
    # 30 distinct minute buckets (minutes now()-1 … now()-30, real wall-clock). The 1m
    # derived-signals view turns this into a CONSTANT log_volume=5 series across 30 buckets, so
    # every quantile equals 5.0 exactly (quantile of a constant is that constant — deterministic
    # even though chDB's reservoir sampler is otherwise non-deterministic). error_volume /
    # error_ratio are likewise constant 0. auto_metrics_rules' threshold scan therefore yields three
    # fixed candidates whose thresholds (5.0/5.5, 0.0/0.1) are timestamp-independent.
    #
    # We reuse "web" (already the lone base derived-signal service) rather than a fresh name on
    # PURPOSE: _list_derived_signal_dimensions' service dropdown is a DISTINCT-over-UNION whose
    # trailing ORDER BY binds only to the last branch, so its row order is genuinely racy in chDB
    # (verified: alternates run-to-run) once two services are present. Keeping the set at a single
    # service makes that list trivially deterministic. The candidate GROUP BY order is stable.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL (intDiv(number, 5) + 1) MINUTE, 'web', 'req' "
        "FROM numbers(150)"
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_issues_raise(db) -> None:
    # A global github repo + token so raise_issue_from_user_observation's agent flow resolves a
    # github target and creates an issue (via the canned POST). The AI endpoints come from the
    # profile env (agent mock paths), like agenttrigger.
    _insert(
        db,
        "sobs_ai_settings",
        [
            {"Key": "ai.github_repo", "Value": "acme/widget", "IsDeleted": 0, "Version": 1704164644000},
            {"Key": "ai.github_token", "Value": "ghp_parity_token", "IsDeleted": 0, "Version": 1704164644000},
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_ai_settings FINAL")


def seed_issues_reuse(db) -> None:
    # A global github repo + token (a DISTINCT repo from issuesraise so the seeded open-issue
    # fixture never ripples there) plus two prior work items, so raise_issue_from_user_observation's
    # agent flow exercises the dedup/reuse path:
    #   * W1 is a prior issue (#41) whose DedupKey equals the key the flow recomputes from the
    #     request (repo=acme/reuse-demo, source=errors, err_type=TimeoutError, state=critical, no
    #     service). The github mock returns #41 as an OPEN issue, so it becomes a dedup candidate and
    #     the local fallback classifies the proposed incident "same" -> reuse #41 (dedup_decision
    #     "reused_existing", occurrence_count 2). The AI mock returns the analyze root-cause text
    #     (no JSON), so the LLM dedupe classifier falls back deterministically.
    #   * W2 sits at CopilotAssignmentStatus="active" (a DIFFERENT issue #99 not returned as open,
    #     so it is not itself a candidate) so _count_active_copilot_assignments >= the default limit
    #     of 1, deterministically blocking the reuse-path Copilot assignment WITHOUT an assign HTTP
    #     call. Both copilot rate-limiters run (hourly count 0; active count >= 1).
    # RequestedAt=0 on both keeps the (clock-derived) hourly counter at 0.
    _insert(
        db,
        "sobs_ai_settings",
        [
            {"Key": "ai.github_repo", "Value": "acme/reuse-demo", "IsDeleted": 0, "Version": 1704164644000},
            {"Key": "ai.github_token", "Value": "ghp_parity_token", "IsDeleted": 0, "Version": 1704164644000},
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_ai_settings FINAL")
    _insert(
        db,
        "sobs_github_work_items",
        [
            {
                "Id": "c1000000000000000000000000000001",
                # Distinct (CreatedAt, AgentRunId) per row: the table sorts on (CreatedAt, AgentRunId),
                # so two rows sharing that key would collapse under ReplacingMergeTree FINAL.
                "CreatedAt": "2024-01-02 03:05:01.000000",
                "CompletedAt": "2024-01-02 03:05:01.000000",
                "AgentRunId": "ar000000000000000000000000000001",
                "AgentRuleId": "",
                "AgentRuleName": "User Raised Issue (errors)",
                "AgentAction": "github_issue",
                "ServiceName": "",
                "AnomalyRuleId": "",
                "AnomalyState": "critical",
                "SignalSource": "errors",
                "SignalName": "TimeoutError",
                "SignalValue": 1.0,
                "GithubRepo": "acme/reuse-demo",
                "DedupKey": "acme reuse demo||errors|timeouterror|critical",
                "DedupDecision": "new_issue",
                "DedupConfidence": 1.0,
                "IssueNumber": 41,
                "IssueUrl": "https://github.com/acme/reuse-demo/issues/41",
                "CanonicalIssueNumber": 41,
                "CanonicalIssueUrl": "https://github.com/acme/reuse-demo/issues/41",
                "RelatedIssueUrls": "[]",
                "OccurrenceCount": 1,
                "IssueState": "open",
                "IssueTitle": "Checkout latency: TimeoutError spike",
                "AnalysisSummary": "Prior root-cause analysis.",
                "SuggestionSummary": "Prior suggested fix.",
                "CopilotAssignmentRequestedAt": 0,
                "CopilotAssignmentStatus": "not_requested",
                "CopilotAssignmentReason": "",
                "PrLinked": 0,
                "PrNumber": 0,
                "PrUrl": "",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Id": "c1000000000000000000000000000002",
                "CreatedAt": "2024-01-02 03:05:02.000000",
                "CompletedAt": "2024-01-02 03:05:02.000000",
                "AgentRunId": "ar000000000000000000000000000002",
                "AgentRuleId": "",
                "AgentRuleName": "User Raised Issue (errors)",
                "AgentAction": "github_issue_copilot",
                "ServiceName": "",
                "AnomalyRuleId": "",
                "AnomalyState": "warning",
                "SignalSource": "errors",
                "SignalName": "OtherError",
                "SignalValue": 1.0,
                "GithubRepo": "acme/reuse-demo",
                "DedupKey": "acme reuse demo||errors|othererror|warning",
                "DedupDecision": "new_issue",
                "DedupConfidence": 1.0,
                "IssueNumber": 99,
                "IssueUrl": "https://github.com/acme/reuse-demo/issues/99",
                "CanonicalIssueNumber": 99,
                "CanonicalIssueUrl": "https://github.com/acme/reuse-demo/issues/99",
                "RelatedIssueUrls": "[]",
                "OccurrenceCount": 1,
                "IssueState": "open",
                "IssueTitle": "Unrelated active Copilot work item",
                "AnalysisSummary": "",
                "SuggestionSummary": "",
                "CopilotAssignmentRequestedAt": 0,
                "CopilotAssignmentStatus": "active",
                "CopilotAssignmentReason": "Copilot is assigned on the issue",
                "PrLinked": 0,
                "PrNumber": 0,
                "PrUrl": "",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_github_work_items FINAL")


def seed_notif(db) -> None:
    # Two channels + two rules, each on its OWN id so toggle/delete don't collide on the
    # ReplacingMergeTree version (both actions re-insert at Version 1704164645000). Seed Version
    # is 1ms lower so the action's re-insert wins FINAL. ConfigJson is plaintext (decrypt is
    # identity on un-prefixed values).
    _insert(
        db,
        "sobs_notification_channels",
        [
            {
                "Id": "c1000000000000000000000000000001",
                "Name": "Ops Webhook",
                "ChannelType": "webhook",
                "ConfigJson": '{"webhook_url": "https://hooks.example.com/ops"}',
                "Enabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Id": "c1000000000000000000000000000002",
                "Name": "Alerts Webhook",
                "ChannelType": "webhook",
                "ConfigJson": '{"webhook_url": "https://hooks.example.com/alerts"}',
                "Enabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                # The /test target: a generic webhook (config key "url") whose POST is served by
                # the canned upstream fixture, so dispatch returns "ok".
                "Id": "c1000000000000000000000000000003",
                "Name": "Test Webhook",
                "ChannelType": "webhook",
                "ConfigJson": '{"url": "https://hooks.example.com/ops"}',
                "Enabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
        ],
    )
    _insert(
        db,
        "sobs_notification_rules",
        [
            {
                "Id": "d1000000000000000000000000000001",
                "Name": "High error rate",
                "Enabled": 1,
                "LogicOperator": "any",
                "ConditionsJson": "[]",
                "ChannelIds": "[]",
                "Severity": "critical",
                "CooldownSeconds": 300,
                "LastFiredAt": "1970-01-01 00:00:00.000",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Id": "d1000000000000000000000000000002",
                "Name": "Latency spike",
                "Enabled": 1,
                "LogicOperator": "all",
                "ConditionsJson": "[]",
                "ChannelIds": "[]",
                "Severity": "warning",
                "CooldownSeconds": 600,
                "LastFiredAt": "1970-01-01 00:00:00.000",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_notification_channels FINAL")
    db.execute("OPTIMIZE TABLE sobs_notification_rules FINAL")


def seed_github_token(db) -> None:
    # A configured global GitHub token so onboarding inspect/issue routes reach their GitHub
    # branch (the repo-scoped key is absent, so this global one is used).
    _insert(
        db,
        "sobs_ai_settings",
        [{"Key": "ai.github_token", "Value": "ghp_parityfixturetoken", "IsDeleted": 0, "Version": 1704164645000}],
    )
    db.execute("OPTIMIZE TABLE sobs_ai_settings FINAL")


def seed_mcp_key(db) -> None:
    # One MCP API key descriptor (mcp.api_keys is a JSON list in sobs_app_settings) so the
    # DELETE /api/mcp/keys/<id> route can revoke it. Persisted via the app's own setter so the
    # on-disk encoding matches exactly.
    import json as _json

    import app as _app

    descriptor = {
        "id": "mk-parity-0001",
        "name": "Parity Key",
        "prefix": "sk-parity",
        "created_at": "2024-01-02T03:00:00+00:00",
        "last_used_at": "",
    }
    _app._set_app_setting(db, "mcp.api_keys", _json.dumps([descriptor], ensure_ascii=False))


def seed_mcp_auth(db) -> None:
    # An MCP API key descriptor whose key_hash is the scrypt fingerprint of "mcp-parity-token"
    # (computed by mcp._hash_key so it matches the Go hand-rolled scrypt). Sending that token in
    # X-MCP-API-Key authenticates tools/list + tools/call.
    import json as _json

    import app as _app
    import mcp as _mcp

    descriptor = {
        "id": "mk-auth-0001",
        "name": "Parity Auth Key",
        "prefix": "sk-parity",
        "key_hash": _mcp._hash_key("mcp-parity-token"),
        "created_at": "2024-01-02T03:00:00+00:00",
        "last_used_at": "",
    }
    _app._set_app_setting(db, "mcp.api_keys", _json.dumps([descriptor], ensure_ascii=False))


CI_AUTH_APP_ID = "c1c1000000000000000000000000ab01"
CI_AUTH_REL_ID = "c1c1000000000000000000000000cd02"


def seed_ci_key(db) -> None:
    # A registered app + release carrying a MANAGED per-app CI-push key, so the managed-key path of
    # require_api_key can be exercised with the static SOBS_API_KEY unset. The stored hash is the
    # app's OWN _hash_api_key("ci-parity-token") (keyed scrypt over a SOBS_SECRET_KEY-derived blake2b
    # salt) — i.e. byte-identical to what a Settings->Repositories key rotation writes, and to what
    # the Go server recomputes when validating the header. The hash key is NOT a sensitive setting
    # (so it is stored/read as plaintext on both sides); expires_at is far-future under the frozen
    # 2024 parity clock, so the key is unexpired. The CI-push settings never appear in the /v1 app or
    # release JSON, so only the auth DECISION (200 vs 401) depends on them — the response bytes are
    # the ordinary registry serialization, captured from the frozen Python oracle.
    import app as _app

    key_hash = _app._hash_api_key("ci-parity-token")
    _insert(
        db,
        "sobs_apps",
        [
            {
                "Id": CI_AUTH_APP_ID,
                "Name": "CI Managed Service",
                "Slug": "ci-managed-service",
                "OwnerTeam": "platform",
                "RepoUrl": "https://github.com/acme/ci-managed",
                "DefaultEnvironment": "prod",
                "Enabled": 1,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164644000,
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
                "Id": CI_AUTH_REL_ID,
                "AppId": CI_AUTH_APP_ID,
                "ReleaseVersion": "2.0.0",
                "CommitSha": "",
                "BuildId": "",
                "Environment": "prod",
                "ReleasedAt": _TS,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    _insert(
        db,
        "sobs_ai_settings",
        [
            {"Key": _app._ci_push_setting_key(CI_AUTH_APP_ID, "hash"), "Value": key_hash, "IsDeleted": 0, "Version": 1704164644000},
            {"Key": _app._ci_push_setting_key(CI_AUTH_APP_ID, "expires_at"), "Value": "2030-01-01T23:59:59+00:00", "IsDeleted": 0, "Version": 1704164644000},
            {"Key": _app._ci_push_setting_key(CI_AUTH_APP_ID, "realtime_enabled"), "Value": "true", "IsDeleted": 0, "Version": 1704164644000},
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_apps FINAL")
    db.execute("OPTIMIZE TABLE sobs_app_releases FINAL")
    db.execute("OPTIMIZE TABLE sobs_ai_settings FINAL")


def seed_aichat(db) -> None:
    # One AI-helper chat turn (otel_logs turn.complete) so /api/ai/helper/chats/<id> reconstructs
    # a user+assistant exchange. LogAttributes is a Map(String,String); output.messages is the
    # JSON the assistant content is parsed from.
    _insert(
        db,
        "otel_logs",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "sobs-ai-helper",
                "EventName": "turn.complete",
                "Body": "turn.complete",
                "LogAttributes": {
                    "gen_ai.chat_id": "chat-parity-001",
                    "gen_ai.turn_id": "turn-parity-001",
                    "gen_ai.input.question": "What is the error rate?",
                    "gen_ai.turn.summary.request": "What is the error rate?",
                    "gen_ai.output.messages": '[{"role": "assistant", "content": "The error rate is 2%."}]',
                },
            }
        ],
    )
    # A gen_ai span (otel_traces) matching _AI_SPAN_CONDITION so /api/ai/export emits a JSONL row.
    _insert(
        db,
        "otel_traces",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "sobs-ai-helper",
                "TraceId": "trace-parity-001",
                "Duration": 1500000000,
                "SpanAttributes": {
                    "gen_ai.provider.name": "openai",
                    "gen_ai.request.model": "gpt-4",
                    "gen_ai.input.messages": '[{"role": "user", "content": "Hello"}]',
                    "gen_ai.output.messages": '[{"role": "assistant", "content": "Hi there"}]',
                    "gen_ai.usage.input_tokens": "42",
                    "gen_ai.usage.output_tokens": "100",
                },
            }
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")
    db.execute("OPTIMIZE TABLE otel_traces FINAL")


PROFILE_SEEDS = {
    "agentrun": seed_agent_run,
    "notif": seed_notif,
    "notifcheck": seed_notif,  # same rows; isolated so check doesn't see toggle/delete mutations
    "notifgen": seed_notif,  # channels+rules; auto-generate create inserts new rules (isolated)
    "agenttrigger": seed_agent_rule,  # analyze-only rule; trigger_agent_run runs the agent flow
    "dmbackup": seed_dm_backup,
    "k8s": seed_k8s,  # backup_enabled=1; backup/run + restore reach their enabled branch
    "repoapp": seed_repo_app,  # registered app + release + github token; repositories-sub actions
    "cveosv": seed_cve_osv,  # telemetry.sdk row -> non-empty inventory -> OSV scan finds a vuln
    "tagauto": seed_tagauto,  # 30 recent prod-service logs -> auto_tag_rules in-window candidate
    "metricsauto": seed_metricsauto,  # constant log_volume series -> auto_metrics_rules candidates
    "cvebackfill": seed_repo_app,  # app+release+github token -> cve github backfill attempts a release
    "onboard": seed_repo_app,  # app+token -> onboarding create-issues realtime + github-issue paths
    "issuesraise": seed_issues_raise,  # global github repo+token -> issues/raise agent flow creates an issue
    "issuereuse": seed_issues_reuse,  # prior work item + matching open issue -> issues/raise reuses it (dedup)
    "githubtoken": seed_github_token,
    "mcpkey": seed_mcp_key,
    "mcpauth": seed_mcp_auth,  # api key whose hash auths tools/list + tools/call
    "aichat": seed_aichat,
    "ciauth": seed_ci_key,  # registered app + managed per-app CI-push key; managed-key require_api_key path
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
