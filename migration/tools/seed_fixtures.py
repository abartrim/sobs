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


# otel_metrics_gauge/_sum carry a baseline TTL of TimeUnixMs + 48 HOUR (SOBS_RAW_METRICS_TTL_HOURS),
# and chDB evaluates that TTL against the REAL wall clock (NOT the frozen 2024 determinism epoch). A
# fixed 2024 timestamp is therefore immediately TTL-dropped (the part lands inactive), so k8s metric
# rows MUST be now()-anchored to survive. The function emits max(TimeUnix) AS last_seen ("created"),
# so that string drifts across capture/replay -> the routes MASK the timestamp (rumvitals/metricsrich
# pattern). A CONSTANT offset (all rows now() - INTERVAL 1 HOUR) keeps max(TimeUnix) identical for
# every group, so every "created" is the same masked token and all non-time values stay byte-stable.
_K8S_OFFSET_SQL = "now() - INTERVAL 1 HOUR"


def _k8s_enable(db) -> None:
    _insert(db, "sobs_app_settings", [{"Key": "kubernetes.enabled", "Value": "1", "UpdatedAt": _TS}])
    db.execute("OPTIMIZE TABLE sobs_app_settings FINAL")


def _k8s_insert(db, table: str, rows: list[tuple]) -> None:
    # Each row is (metric, value, attrs_dict). Insert via INSERT ... SELECT so TimeUnix is the
    # now()-anchored offset (within the 48h TTL window) and Attributes is built with map(...). One
    # statement per row keeps the map literal simple and the value list explicit.
    for metric, value, attrs in rows:
        pairs = ", ".join(f"'{k}', '{v}'" for k, v in attrs.items())
        db.execute(
            f"INSERT INTO {table} (TimeUnix, ServiceName, MetricName, Value, Attributes) "
            f"SELECT {_K8S_OFFSET_SQL}, 'k8s', '{metric}', {value}, map({pairs}) FROM numbers(1)"
        )
    db.execute(f"OPTIMIZE TABLE {table} FINAL")


def seed_k8srich(db) -> None:
    # OTEL-NATIVE k8s metrics (k8s.* resource attributes) so _detect_k8s_metric_format returns
    # "otel" (any otel_metrics_gauge row with Attributes['k8s.node.name'] != '') and the function
    # takes its ELSE (native-OTEL) branch for every section with POPULATED data. Covers the otel
    # nodes/pods/deployments/namespaces success bodies (already empty-executed by the bare `k8s`
    # profile) PLUS, when a route adds ?name=, the otel name-filter branches (31273-31274,
    # 31459-31460, 31594-31595) and the _append_or_equals non-empty branch (31102-31104).
    #
    # CONSTANT values + one now()-anchored offset so max(TimeUnix) AS last_seen ("created") is one
    # masked token (the TTL forces now()-relative rows; the function has no now() window itself).
    # Every aggregate is single-row-per-group so any()/anyIf()/maxIf() are deterministic, and each
    # entity's default-sort column (node name, pod/deployment namespace, namespace name) is UNIQUE so
    # ORDER BY needs no extra tiebreak.
    #
    # Shape exercises every summary arm:
    #   nodes:       node-a Ready (cpu 0.5, mem 2e9, version v1.29.1), node-b NotReady -> nodes_ready=1
    #   pods:        ns "alpha" web-1 Running restarts 0; ns "beta" api-1 Failed restarts 3 (on node-b)
    #                -> pods_running=1, pods_failed=1; api-1 has cpu/mem usage + restarts>0
    #   deployments: ns "alpha" web desired 3 ready 3 (healthy); ns "beta" api desired 2 ready 1
    #                (ready < desired -> deployments_unhealthy=1)
    #   namespaces:  alpha, beta (DISTINCT k8s.namespace.name) -> namespaces_total=2
    rows = [
        # --- nodes ---
        ("k8s.node.condition_ready", 1.0, {"k8s.node.name": "node-a", "k8s.kubelet.version": "v1.29.1"}),
        ("k8s.node.cpu.usage", 0.5, {"k8s.node.name": "node-a"}),
        ("k8s.node.memory.usage", 2000000000.0, {"k8s.node.name": "node-a"}),
        ("k8s.node.condition_ready", 0.0, {"k8s.node.name": "node-b", "k8s.kubelet.version": "v1.29.1"}),
        ("k8s.node.cpu.usage", 0.9, {"k8s.node.name": "node-b"}),
        ("k8s.node.memory.usage", 3000000000.0, {"k8s.node.name": "node-b"}),
        # --- pods: alpha/web-1 (Running, no restarts) ---
        (
            "k8s.pod.status_ready",
            1.0,
            {
                "k8s.namespace.name": "alpha",
                "k8s.pod.name": "web-1",
                "k8s.pod.phase": "Running",
                "k8s.node.name": "node-a",
            },
        ),
        (
            "k8s.pod.cpu.usage",
            0.25,
            {
                "k8s.namespace.name": "alpha",
                "k8s.pod.name": "web-1",
                "k8s.pod.phase": "Running",
                "k8s.node.name": "node-a",
            },
        ),
        (
            "k8s.pod.memory.usage",
            500000000.0,
            {
                "k8s.namespace.name": "alpha",
                "k8s.pod.name": "web-1",
                "k8s.pod.phase": "Running",
                "k8s.node.name": "node-a",
            },
        ),
        # --- pods: beta/api-1 (Failed, restarts 3) ---
        (
            "k8s.pod.status_ready",
            0.0,
            {
                "k8s.namespace.name": "beta",
                "k8s.pod.name": "api-1",
                "k8s.pod.phase": "Failed",
                "k8s.node.name": "node-b",
            },
        ),
        (
            "k8s.pod.cpu.usage",
            0.75,
            {
                "k8s.namespace.name": "beta",
                "k8s.pod.name": "api-1",
                "k8s.pod.phase": "Failed",
                "k8s.node.name": "node-b",
            },
        ),
        (
            "k8s.pod.memory.usage",
            800000000.0,
            {
                "k8s.namespace.name": "beta",
                "k8s.pod.name": "api-1",
                "k8s.pod.phase": "Failed",
                "k8s.node.name": "node-b",
            },
        ),
        (
            "k8s.container.restart_count",
            3.0,
            {
                "k8s.namespace.name": "beta",
                "k8s.pod.name": "api-1",
                "k8s.pod.phase": "Failed",
                "k8s.node.name": "node-b",
            },
        ),
        # --- deployments: alpha/web healthy (3/3) ---
        ("k8s.deployment.desired", 3.0, {"k8s.namespace.name": "alpha", "k8s.deployment.name": "web"}),
        ("k8s.deployment.ready", 3.0, {"k8s.namespace.name": "alpha", "k8s.deployment.name": "web"}),
        ("k8s.deployment.available", 3.0, {"k8s.namespace.name": "alpha", "k8s.deployment.name": "web"}),
        ("k8s.deployment.updated", 3.0, {"k8s.namespace.name": "alpha", "k8s.deployment.name": "web"}),
        # --- deployments: beta/api unhealthy (ready 1 < desired 2) ---
        ("k8s.deployment.desired", 2.0, {"k8s.namespace.name": "beta", "k8s.deployment.name": "api"}),
        ("k8s.deployment.ready", 1.0, {"k8s.namespace.name": "beta", "k8s.deployment.name": "api"}),
        ("k8s.deployment.available", 1.0, {"k8s.namespace.name": "beta", "k8s.deployment.name": "api"}),
        ("k8s.deployment.updated", 2.0, {"k8s.namespace.name": "beta", "k8s.deployment.name": "api"}),
    ]
    _k8s_insert(db, "otel_metrics_gauge", rows)
    _k8s_enable(db)


def seed_k8sprom(db) -> None:
    # PROMETHEUS (kube-state-metrics + cAdvisor) k8s metrics so _detect_k8s_metric_format returns
    # "prometheus": NO Attributes['k8s.node.name'] anywhere (otel probe misses) and MetricName LIKE
    # 'kube_%' / container_memory_working_set_bytes present. Drives the PROMETHEUS branch of every
    # section (the big uncovered set: nodes 31198-31266, pods 31331-31451, deployments 31524-31586,
    # namespaces 31643-31665), incl. the otel_metrics_sum restart-counter UNION (31376-31390) and
    # the prom name-filter branches when a route adds ?name=.
    #
    # CONSTANT values + one now()-anchored offset (TTL window -> stable masked "created"). Single row
    # per (metric, group) keeps any()/anyIf()/maxIf() deterministic; unique default-sort key per
    # entity keeps ORDER BY tie-free. Same logical shape as k8srich so every summary arm fires:
    #   nodes:       node-a Ready (mem alloc 2e9, kubelet v1.29.1), node-b NotReady -> nodes_ready=1
    #   pods:        ns "alpha" web-1 Running (mem 500MB, restart counter in otel_metrics_SUM=1,
    #                node node-a); ns "beta" api-1 Failed (restart counter=4, node node-b)
    #                -> pods_running=1, pods_failed=1
    #   deployments: ns "alpha" web 3/3 healthy; ns "beta" api desired 2 ready 1 unhealthy
    #   namespaces:  alpha (phase Active), beta (phase Active) -> namespaces_total=2
    gauge_rows = [
        # --- nodes ---
        ("kube_node_status_condition", 1.0, {"node": "node-a", "condition": "Ready", "status": "true"}),
        ("kube_node_status_allocatable", 2000000000.0, {"node": "node-a", "resource": "memory"}),
        ("kube_node_info", 1.0, {"node": "node-a", "kubelet_version": "v1.29.1"}),
        ("kube_node_status_condition", 0.0, {"node": "node-b", "condition": "Ready", "status": "true"}),
        ("kube_node_status_allocatable", 3000000000.0, {"node": "node-b", "resource": "memory"}),
        ("kube_node_info", 1.0, {"node": "node-b", "kubelet_version": "v1.29.1"}),
        # --- pods: alpha/web-1 Running ---
        ("kube_pod_status_phase", 1.0, {"namespace": "alpha", "pod": "web-1", "phase": "Running"}),
        ("kube_pod_status_ready", 1.0, {"namespace": "alpha", "pod": "web-1", "condition": "true"}),
        ("container_memory_working_set_bytes", 500000000.0, {"namespace": "alpha", "pod": "web-1", "container": "app"}),
        ("kube_pod_container_status_restarts_total", 0.0, {"namespace": "alpha", "pod": "web-1"}),
        ("kube_pod_info", 1.0, {"namespace": "alpha", "pod": "web-1", "node": "node-a"}),
        # --- pods: beta/api-1 Failed ---
        ("kube_pod_status_phase", 1.0, {"namespace": "beta", "pod": "api-1", "phase": "Failed"}),
        ("kube_pod_status_ready", 0.0, {"namespace": "beta", "pod": "api-1", "condition": "true"}),
        ("container_memory_working_set_bytes", 800000000.0, {"namespace": "beta", "pod": "api-1", "container": "api"}),
        ("kube_pod_container_status_restarts_total", 2.0, {"namespace": "beta", "pod": "api-1"}),
        ("kube_pod_info", 1.0, {"namespace": "beta", "pod": "api-1", "node": "node-b"}),
        # --- deployments: alpha/web healthy ---
        ("kube_deployment_spec_replicas", 3.0, {"namespace": "alpha", "deployment": "web"}),
        ("kube_deployment_status_replicas_ready", 3.0, {"namespace": "alpha", "deployment": "web"}),
        ("kube_deployment_status_replicas_available", 3.0, {"namespace": "alpha", "deployment": "web"}),
        ("kube_deployment_status_replicas_updated", 3.0, {"namespace": "alpha", "deployment": "web"}),
        # --- deployments: beta/api unhealthy ---
        ("kube_deployment_spec_replicas", 2.0, {"namespace": "beta", "deployment": "api"}),
        ("kube_deployment_status_replicas_ready", 1.0, {"namespace": "beta", "deployment": "api"}),
        ("kube_deployment_status_replicas_available", 1.0, {"namespace": "beta", "deployment": "api"}),
        ("kube_deployment_status_replicas_updated", 2.0, {"namespace": "beta", "deployment": "api"}),
        # --- namespaces ---
        ("kube_namespace_status_phase", 1.0, {"namespace": "alpha", "phase": "Active"}),
        ("kube_namespace_status_phase", 1.0, {"namespace": "beta", "phase": "Active"}),
    ]
    _k8s_insert(db, "otel_metrics_gauge", gauge_rows)

    # Restart counters in otel_metrics_sum (some exporters store _total there) -> exercises the
    # pod_sum_sql UNION branch (31376-31390) + max(restarts) merge. web-1 sum=1 (gauge 0 -> merged 1);
    # api-1 sum=4 (gauge 2 -> merged 4). Both > their gauge value so the sum-table value wins, proving
    # the UNION+max path runs.
    sum_rows = [
        ("kube_pod_container_status_restarts_total", 1.0, {"namespace": "alpha", "pod": "web-1"}),
        ("kube_pod_container_status_restarts_total", 4.0, {"namespace": "beta", "pod": "api-1"}),
    ]
    _k8s_insert(db, "otel_metrics_sum", sum_rows)
    _k8s_enable(db)


# Far older than now() - INTERVAL <any period>: chDB evaluates now() against the REAL wall clock
# (not the frozen 2024 epoch), so a fixed 2020 timestamp is retention-eligible for any prune window
# the route is exercised with — every seeded row is deleted on BOTH the Python capture and the Go
# replay, so the prune's clean success message is byte-identical regardless of when the harness runs.
_DM_PRUNE_OLD_TS = "2020-01-01 00:00:00.000000"


def seed_dm_prune(db) -> None:
    # Retention-eligible rows in every _DM_TTL_TABLES / _DM_METRIC_TABLES table so POST
    # /api/data-management/prune (custom period) runs its real ALTER … DELETE WHERE … window +
    # OPTIMIZE … FINAL pass against POPULATED tables — the gap the prune port closes (the empty
    # base fixture deletes nothing, making the DELETE branch parity-invisible). The metric tables'
    # TimeUnixMs (DateTime) derives from the inserted TimeUnix via its column DEFAULT, so the
    # _get_dm_column_type probe sees "datetime" and the plain (non-ms) DELETE primary applies.
    for table in ("otel_logs", "hyperdx_sessions"):
        _insert(
            db, table, [{"Timestamp": _DM_PRUNE_OLD_TS, "ServiceName": "dmprune-old", "Body": "old"} for _ in range(5)]
        )
    _insert(
        db,
        "otel_traces",
        [{"Timestamp": _DM_PRUNE_OLD_TS, "ServiceName": "dmprune-old", "SpanName": "old"} for _ in range(5)],
    )
    for table in ("otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"):
        _insert(
            db,
            table,
            [{"TimeUnix": _DM_PRUNE_OLD_TS, "ServiceName": "dmprune-old", "MetricName": "old"} for _ in range(5)],
        )
    for table in (
        "otel_logs",
        "otel_traces",
        "hyperdx_sessions",
        "otel_metrics_gauge",
        "otel_metrics_sum",
        "otel_metrics_histogram",
    ):
        db.execute(f"OPTIMIZE TABLE {table} FINAL")


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


def seed_depsrich(db) -> None:
    # Like seed_repo_app (an enabled app + a release + a github token) but the release carries a
    # non-empty CommitSha so the cve scan's github backfill walks the FULL dependency-parse chain:
    #   _fetch_release_deps_from_github -> _github_actions_dependency_rows (runs/artifacts traversal,
    #   reached because CommitSha is set) -> contents-API lockfile fetch+parse.
    # The canned upstream fixtures (migration/fixtures/upstream/) drive these:
    #   - actions/runs?head_sha=<sha> -> one successful run (run id 555)
    #   - actions/runs/555/artifacts  -> a list WITHOUT the snapshot artifact name, so the actions
    #     path finds no usable snapshot, returns no rows, and the contents fallback runs (the binary
    #     zip-archive download branch is not representable in the utf-8 `content` fixture harness).
    #   - contents/requirements.txt   -> 404 (no fixture) so the loop advances to the next candidate.
    #   - contents/package-lock.json  -> a base64 package-lock that _parse_package_lock_dependencies
    #     parses into 3 npm deps (left-pad, nested-dep, dup-pkg) -> one dependencies-lockfile artifact
    #     is INSERTED -> _collect_library_inventory tier-1 yields those 3 libs -> OSV (canned) scan.
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
                "CommitSha": "deadbeefcafe",
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


def seed_tagautorich(db) -> None:
    # Rich now()-anchored telemetry so auto_tag_rules' candidate generator
    # (_build_auto_tag_rule_candidates, app.py 12062-12308) exercises EVERY per-record-type
    # candidate arm in a single preview, instead of the single "log env=production" candidate the
    # plain `tagauto` seed produces. Like seed_tagauto, every row sits at a FIXED now()-relative
    # offset (NOT the frozen 2024 epoch) so it lands inside the route's 24h scan window at both
    # capture and replay; the base fixture has nothing in-window. The candidate OUTPUT
    # (service/operator/tag/count/name) is timestamp-independent, and the final
    # sort key is (point_count desc, name desc) — fully deterministic — so capture and replay,
    # seeded at different wall-clock times, produce byte-identical candidate lists.
    #
    # Each service uses >= the route's default min_count (30) so its GROUP BY ... HAVING c >= 30
    # bucket survives. Distinct, deterministic counts give every candidate a distinct point_count so
    # the sort is total (no name-tie ambiguity beyond the deterministic name tiebreak).
    #
    # Branch coverage (app.py line -> seed shape):
    #   log env       12150-12164  : already covered by plain tagauto, included here too (api-prod logs)
    #   log non-env   12166-12175  : 'checkout' logs (no env token) -> "log service=checkout"
    #   trace env     12187-12191  : 'api-prod' traces (ScopeName != 'sobs-ai') -> "trace env=production"
    #   trace non-env 12201-12202  : 'gateway' traces -> "trace service=gateway"
    #   error append  12225-12230  : ERROR logs w/ exception.type=ValueError -> "error type=valueerror"
    #   error skip    12226-12228  : ERROR logs w/ NO exception.type (Provider='') -> skipped_invalid
    #   ai append     12253-12258  : sobs-ai traces w/ gen_ai.provider.name=openai -> "ai provider=openai"
    #   ai skip       12254-12256  : sobs-ai traces w/ NO provider -> skipped_invalid
    #   rum append    12280-12282  : hyperdx_sessions EventName='click' -> "rum event=click"
    #   skipped_existing 12119-12121: a sobs_tag_rules row matching the 'dupsvc' log candidate exactly
    #                                 -> that candidate is skipped instead of appended
    # The remaining uncovered lines are route-form-driven (12073 empty-selected, 12139 service_filter)
    # and covered by dedicated routes; 12108-12109 and 12297-12298 are defensive/dead under chDB
    # (every call site pre-validates and point_count is always a valid int) and are deferred.

    # --- log branch -----------------------------------------------------------------------------
    # 'checkout' (no env token) -> log non-env candidate; count 31 (distinct from others).
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'checkout', 'request handled' FROM numbers(31)"
    )
    # 'dupsvc' (no env token) -> would be "log service=dupsvc", but a matching tag rule (seeded below)
    # already exists -> skipped_existing arm.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'dupsvc', 'request handled' FROM numbers(32)"
    )

    # --- error branch (otel_logs, severity/exception driven) -----------------------------------
    # 'errsvc' ERROR rows WITH exception.type=ValueError -> error append candidate. These rows ALSO
    # feed the log branch (no severity filter there) -> an extra "log service=errsvc" candidate
    # (deterministic). count 33.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body, LogAttributes) "
        "SELECT now() - INTERVAL 1 HOUR, 'errsvc', 'ERROR', 17, 'boom', "
        "map('exception.type', 'ValueError') FROM numbers(33)"
    )
    # 'errnotype' ERROR rows with NO exception.type -> the error GROUP BY yields ExceptionType=''
    # -> skipped_invalid (12226-12228). These also feed the log branch -> "log service=errnotype".
    # count 34.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'errnotype', 'ERROR', 17, 'boom no type' FROM numbers(34)"
    )

    # --- trace branch (otel_traces, ScopeName != 'sobs-ai') ------------------------------------
    # 'api-prod' (env token) -> trace env candidate. count 30.
    db.execute(
        "INSERT INTO otel_traces (Timestamp, ServiceName, SpanName, ScopeName) "
        "SELECT now() - INTERVAL 1 HOUR, 'api-prod', 'GET /api', 'io.opentelemetry.http' FROM numbers(30)"
    )
    # 'gateway' (no env token) -> trace non-env candidate. count 35.
    db.execute(
        "INSERT INTO otel_traces (Timestamp, ServiceName, SpanName, ScopeName) "
        "SELECT now() - INTERVAL 1 HOUR, 'gateway', 'route', 'io.opentelemetry.http' FROM numbers(35)"
    )

    # --- ai branch (otel_traces, ScopeName = 'sobs-ai') ----------------------------------------
    # sobs-ai traces WITH gen_ai.provider.name=openai -> ai append candidate. count 36. Use a single
    # service 'sobs-ai-svc' so these don't add an extra trace-branch candidate (the trace query
    # excludes ScopeName='sobs-ai').
    db.execute(
        "INSERT INTO otel_traces (Timestamp, ServiceName, SpanName, ScopeName, SpanAttributes) "
        "SELECT now() - INTERVAL 1 HOUR, 'sobs-ai-svc', 'chat', 'sobs-ai', "
        "map('gen_ai.provider.name', 'openai') FROM numbers(36)"
    )
    # sobs-ai traces with NO provider -> Provider='' -> skipped_invalid (12254-12256). count 37.
    db.execute(
        "INSERT INTO otel_traces (Timestamp, ServiceName, SpanName, ScopeName) "
        "SELECT now() - INTERVAL 1 HOUR, 'sobs-ai-svc', 'chat-noprov', 'sobs-ai' FROM numbers(37)"
    )

    # --- rum branch (hyperdx_sessions) ---------------------------------------------------------
    # EventName='click' -> rum append candidate. count 38.
    db.execute(
        "INSERT INTO hyperdx_sessions (Timestamp, ServiceName, EventName, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'rumsvc', 'click', 'tap' FROM numbers(38)"
    )

    db.execute("OPTIMIZE TABLE otel_logs FINAL")
    db.execute("OPTIMIZE TABLE otel_traces FINAL")
    db.execute("OPTIMIZE TABLE hyperdx_sessions FINAL")

    # An EXISTING tag rule that exactly matches the 'dupsvc' log candidate
    # (record_types=["log"], match_field="service_name", match_operator="eq", match_value="dupsvc",
    # match_attr_key="", tag_key="service", tag_value="dupsvc"). _build_auto_tag_rule_candidates
    # builds existing_keys from these rows, so the generated dupsvc candidate is dropped via the
    # skipped_existing arm (app.py 12119-12121). RecordTypes is the single token "log" so the
    # joined+sorted record-types component of the existing key equals the candidate's single
    # record_type "log".
    _insert(
        db,
        "sobs_tag_rules",
        [
            {
                "Id": "ta11ta11-0000-4000-8000-00000000d0pa",
                "Name": "tagautorich dupsvc existing",
                "RecordTypes": "log",
                "MatchField": "service_name",
                "MatchOperator": "eq",
                "MatchValue": "dupsvc",
                "MatchAttrKey": "",
                "TagKey": "service",
                "TagValue": "dupsvc",
                "ConditionsJson": "",
                "IsDeleted": 0,
                "Version": 1,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_tag_rules FINAL")


def seed_dashboardautorich(db) -> None:
    # Seed sobs_anomaly_rules ONLY (no telemetry) so auto_metrics_rules_dashboard's PREVIEW exercises
    # the remaining uncovered arms of _build_auto_dashboard_chart_candidates (app.py 12311-12385):
    #   12322-12323 : a rule with empty SignalSource/SignalName -> `continue` (skipped)
    #   12326-12327 : a rule whose ServiceName != the request service_filter -> `continue` (skipped)
    #   12337-12338 : a rule with a non-empty AttrFingerprint -> the AttrFingerprint WHERE clause arm
    # The candidate builder reads ONLY sobs_anomaly_rules; it never queries otel_logs/otel_traces/
    # hyperdx_sessions, so seeding zero telemetry keeps _list_derived_signal_dimensions' services
    # dropdown equal to the base-fixture set (genuinely racy once >1 service is present — see
    # seed_metricsauto). The preview render embeds that dropdown, so NOT perturbing it is what keeps
    # this route byte-stable. The candidate list itself sorts by (service, source, signal, title) and
    # carries no timestamps/uuids, so it is deterministic.
    #
    # The route is invoked with action=preview + service_filter=web (see the dashboardautorich
    # routes). With that filter:
    #   - 'da-empty'  (source/signal blank)     -> 12322-12323 continue
    #   - 'da-other'  (service='other-svc')     -> 12326-12327 continue (service != 'web')
    #   - 'da-attrfp' (service='web', attr_fp set) -> appended via the attr_fp WHERE arm (12337-12338)
    #   - 'da-plain'  (service='web', no attr_fp)  -> appended (baseline arm), keeps a second candidate
    _rules = [
        # (Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint)
        ("da-empty", "a dar empty", "threshold", "", "", "web", ""),
        ("da-other", "b dar other", "threshold", "logs", "log_volume", "other-svc", ""),
        ("da-attrfp", "c dar attrfp", "threshold", "logs", "log_volume", "web", "deadbeefdeadbeef"),
        ("da-plain", "d dar plain", "threshold", "logs", "log_volume", "web", ""),
    ]
    rows = []
    for rid, name, rtype, src, sig, svc, fp in _rules:
        rows.append(
            {
                "Id": rid,
                "Name": name,
                "RuleType": rtype,
                "SignalSource": src,
                "SignalName": sig,
                "ServiceName": svc,
                "AttrFingerprint": fp,
                "Comparator": "gt",
                "WarningThreshold": 3.0,
                "CriticalThreshold": 8.0,
                "SecondarySignalSource": "",
                "SecondarySignalName": "",
                "SecondaryComparator": "gt",
                "SecondaryWarningThreshold": 0.0,
                "SecondaryCriticalThreshold": 0.0,
                "MinSampleCount": 1,
                "SeasonalBucketsJson": "",
                "IsDeleted": 0,
                "Version": 1,
            }
        )
    _insert(db, "sobs_anomaly_rules", rows)
    db.execute("OPTIMIZE TABLE sobs_anomaly_rules FINAL")


def seed_rumvitals(db) -> None:
    # Seed hyperdx_sessions with now()-relative web-vital + error rows so view_rum's "Web vitals"
    # and "Error trend" blocks (app.py 17472-17633) all populate. Every row is laid out at a FIXED
    # offset from now() with CONSTANT values, so every derived quantity is timestamp-independent
    # (deterministic regardless of the exact wall-clock capture/replay time). Only the now()-derived
    # bucket/last_seen TIMESTAMPS drift between capture and replay; those are masked in the route.
    #
    # All rows share one ServiceName ('web') and one LogAttributes['sessionId'] so they collapse into
    # a SINGLE deterministic session group in the default sessions view (no row-order ambiguity), and
    # one LogAttributes['url'] so the hotspot / top_urls group keys are singular. Offsets keep a wide
    # margin (>=5min) from every window boundary (now()-30/60min for the trend split, now()-60min for
    # the derived signals) so capture/replay second-level drift never flips a row across a boundary.
    #
    # Web vitals: 4 LCP web-vital rows at now()-10/15/20/25 MINUTE (all inside both the 24h hotspot
    # window AND the 60min derived-signal window). Body value=1200 (constant) -> quantileExact(0.75)
    # = 1200.0 exactly in every minute bucket -> v_derived_signals_1m is a constant LCP=1200 series,
    # so the 60-row rolling baseline has stddev=0 -> anomaly_state='normal' for every bucket (covers
    # 17488-92 summary + 17505-06 sparkline). rating='good' -> poor_count=0/poor_rate=0.0; hotspot
    # p75 = quantileExact(0.75)(value) = 1200.0 (covers 17529-32 + 17542-44; HAVING total>=3 fires
    # at total=4). Each row in its own minute bucket -> 4 distinct sparkline points.
    for off in (10, 15, 20, 25):
        db.execute(
            "INSERT INTO hyperdx_sessions (Timestamp, ServiceName, EventName, Body, LogAttributes) "
            "SELECT now() - INTERVAL ? MINUTE, 'web', 'web-vital', "
            '\'{"name":"LCP","value":1200,"rating":"good"}\', '
            "map('url', 'https://app.example.com/checkout', 'sessionId', 'sobs-rumvitals-session') "
            "FROM numbers(1)",
            [off],
        )
    # Error trend: errors at now()-5 MINUTE (recent, inside now()-30min) and now()-45 MINUTE (prior,
    # inside now()-60min AND outside now()-30min). recent=5 ('error' x4 + 'unhandledrejection' x1),
    # prior=1 -> recent(5) > prior(1)*1.25 -> trend 'up' (covers 17576-79 + 17581). 24h totals by
    # type: error=5, unhandledrejection=1, total=6 (covers 17593-95). The 180min sparkline gets two
    # populated buckets (now()-5, now()-45) plus WITH FILL zeros (covers 17632-33). Constant Body
    # 'message' + LogAttributes['url'] make top_messages / top_urls singular and deterministic.
    _RUM_ERR_BODY = '\'{"message":"TypeError: cannot read x"}\''
    _RUM_ERR_ATTRS = "map('url', 'https://app.example.com/checkout', 'sessionId', 'sobs-rumvitals-session')"
    # recent window (now()-5min): 4 'error' + 1 'unhandledrejection'
    db.execute(
        "INSERT INTO hyperdx_sessions (Timestamp, ServiceName, EventName, Body, LogAttributes) "
        f"SELECT now() - INTERVAL 5 MINUTE, 'web', 'error', {_RUM_ERR_BODY}, {_RUM_ERR_ATTRS} "
        "FROM numbers(4)"
    )
    db.execute(
        "INSERT INTO hyperdx_sessions (Timestamp, ServiceName, EventName, Body, LogAttributes) "
        f"SELECT now() - INTERVAL 5 MINUTE, 'web', 'unhandledrejection', {_RUM_ERR_BODY}, {_RUM_ERR_ATTRS} "
        "FROM numbers(1)"
    )
    # prior window (now()-45min): 1 'error'
    db.execute(
        "INSERT INTO hyperdx_sessions (Timestamp, ServiceName, EventName, Body, LogAttributes) "
        f"SELECT now() - INTERVAL 45 MINUTE, 'web', 'error', {_RUM_ERR_BODY}, {_RUM_ERR_ATTRS} "
        "FROM numbers(1)"
    )
    db.execute("OPTIMIZE TABLE hyperdx_sessions FINAL")


def seed_summaryrich(db) -> None:
    # Populate the summary dashboard (GET /, def summary) so its data-driven panels and helper
    # chains run their POPULATED branches instead of the all-empty base render:
    #   * recent_errors  -> _build_error_item full body (app.py 10652-10694, 10855-10856)
    #   * recent_logs    -> the recent-logs append (10905)
    #   * signal_health  -> _get_signal_health_by_service POPULATED path (12892-12925) +
    #                       _annotate_rows_with_rules / _build_series_rule_lookups /
    #                       _combine_rule_states + the threshold/seasonal rule evaluators
    #                       (12939-13017, 13033-13088) via seeded logs-source anomaly rules.
    #
    # Everything is laid out at FIXED offsets from now() with CONSTANT values so every derived
    # quantity (counts, ratios, rule states, signal_count) is timestamp-independent; only the
    # now()-derived recent-errors/recent-logs TIMESTAMPS drift between capture and replay and are
    # masked in the route.
    #
    # SINGLE service "web" on PURPOSE (mirrors seed_metricsauto): stats.services is a DISTINCT-over
    # -UNION whose row order is racy once >1 service exists, and the base fixture already carries
    # exactly the "web" service, so reusing it keeps the Services panel a singular, ordered badge.
    #
    # ALL 10 log rows land in ONE minute bucket (toStartOfMinute(now() - INTERVAL 5 MINUTE)) at
    # distinct WHOLE-SECOND offsets 0..9. The single bucket means v_derived_signals_1m yields one
    # row per series, so argMax(value,time) in signal_health is just that bucket's value:
    #     log_volume = 10, error_volume = 5, error_ratio = 0.5  (all exact, integer/half).
    # Distinct second-level timestamps give recent_errors (ORDER BY Timestamp DESC LIMIT 5) and
    # recent_logs (LIMIT 10) a TOTAL order, so neither LIMIT slice is row-order ambiguous. The
    # now()-5min anchor keeps every row well inside both the 48h recent-errors window and the 24h
    # signal-health window with a wide margin against capture/replay drift.
    #
    # 5 of the 10 rows are SeverityText='ERROR' carrying exception.* LogAttributes so they feed
    # ERROR_SOURCES_SQL (the unresolved-errors source) AND drive _build_error_item's attribute
    # extraction (exception.type/message/stacktrace etc.). No resolution is seeded, so all 5 stay
    # unresolved and appear in recent_errors. Distinct Body per row keeps messages stable.
    _BASE = "toStartOfMinute(now() - INTERVAL 5 MINUTE)"
    for sec in range(5):  # 5 ERROR rows -> recent_errors + error_volume series
        db.execute(
            "INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body, LogAttributes) "
            f"SELECT {_BASE} + INTERVAL ? SECOND, 'web', 'ERROR', 17, ?, "
            "map('exception.type', 'ValueError', 'exception.message', ?, "
            "'exception.stacktrace', 'Traceback (most recent call last):\\n  File app.py') "
            "FROM numbers(1)",
            [sec, f"boom #{sec}: invalid input", f"invalid input {sec}"],
        )
    for sec in range(5, 10):  # 5 INFO rows -> recent_logs filler (distinct timestamps/bodies)
        db.execute(
            "INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body) "
            f"SELECT {_BASE} + INTERVAL ? SECOND, 'web', 'INFO', 9, ? "
            "FROM numbers(1)",
            [sec, f"request handled {sec}"],
        )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")

    # Logs-source anomaly rules so signal_health's _annotate_rows_with_rules has matching rules.
    # ServiceName 'web' + empty AttrFingerprint -> _rule_matches_series matches every series of that
    # service (rule_attr_fp == '' short-circuits the fp check). Constant values -> deterministic
    # rule states. The set deliberately exercises every threshold/seasonal evaluator branch:
    #   gt-critical  (log_volume 10 >= crit 8)            -> outlier
    #   gt-warning   (error_volume 5 in [warn 3, crit 8)) -> warning
    #   lt-critical  (error_ratio 0.5 <= crit 0.6)        -> outlier
    #   lt-warning   (error_ratio 0.5 in (crit 0.3, warn 0.55]) -> warning
    #   normal       (log_volume 10 < warn 50)            -> state stays normal (early return)
    #   min-skip     (SampleCount 10 < min_sample_count 999) -> early return
    #   seasonal     (RuleType=seasonal; signal_health passes no time_key so the bucket-match path
    #                 is skipped and it falls back to the global thresholds)
    # Fixed Ids + fixed Version keep the rows byte-stable. summary renders only worst_state/service/
    # signal_count, so rule_id/rule_reason never reach the page.
    # Each tuple: Id, Name, RuleType, SignalName, Comparator, Warn, Crit, MinSampleCount,
    # SeasonalBucketsJson, ServiceName, AttrFingerprint. The last two default to the matching
    # ("web", "") for the firing rules and carry a deliberate mismatch on the two _rule_matches_series
    # negative-branch probes.
    _rules = [
        ("sumr-rule-1", "z sumrich log_volume crit", "threshold", "log_volume", "gt", 3.0, 8.0, 1, "", "web", ""),
        ("sumr-rule-2", "z sumrich error_volume warn", "threshold", "error_volume", "gt", 3.0, 8.0, 1, "", "web", ""),
        ("sumr-rule-3", "z sumrich error_ratio crit", "threshold", "error_ratio", "lt", 0.9, 0.6, 1, "", "web", ""),
        ("sumr-rule-4", "z sumrich error_ratio warn", "threshold", "error_ratio", "lt", 0.55, 0.3, 1, "", "web", ""),
        ("sumr-rule-5", "z sumrich log_volume normal", "threshold", "log_volume", "gt", 50.0, 99.0, 1, "", "web", ""),
        ("sumr-rule-6", "z sumrich log_volume minskip", "threshold", "log_volume", "gt", 1.0, 2.0, 999, "", "web", ""),
        (
            "sumr-rule-7",
            "z sumrich log_volume seasonal",
            "seasonal",
            "log_volume",
            "gt",
            3.0,
            8.0,
            1,
            '{"strategy":"hour_of_day","buckets":{"0":{"warning":3,"critical":8}}}',
            "web",
            "",
        ),
        # seasonal rule whose value (log_volume=10) does NOT cross the thresholds -> the inner
        # _evaluate_threshold_condition returns None -> the seasonal "if not evaluation: return None"
        # branch (app.py 13082-13083) runs.
        (
            "sumr-rule-8",
            "z sumrich log_volume seasonal normal",
            "seasonal",
            "log_volume",
            "gt",
            50.0,
            99.0,
            1,
            "",
            "web",
            "",
        ),
        # seasonal rule with MALFORMED SeasonalBucketsJson -> the json.JSONDecodeError except path
        # (app.py 13070-13071) runs before falling back to the global thresholds.
        (
            "sumr-rule-9",
            "z sumrich log_volume seasonal badjson",
            "seasonal",
            "log_volume",
            "gt",
            3.0,
            8.0,
            1,
            "{not json",
            "web",
            "",
        ),
        # _rule_matches_series negative branches (same source+signal so the first two checks pass):
        #   wrong ServiceName -> the rule_service mismatch return (app.py 12944-12945)
        ("sumr-rule-10", "z sumrich svc mismatch", "threshold", "log_volume", "gt", 3.0, 8.0, 1, "", "other-svc", ""),
        #   matching ServiceName but non-empty mismatching AttrFingerprint -> the rule_attr_fp
        #   mismatch return (app.py 12947-12948)
        (
            "sumr-rule-11",
            "z sumrich fp mismatch",
            "threshold",
            "log_volume",
            "gt",
            3.0,
            8.0,
            1,
            "",
            "web",
            "deadbeefdeadbeef",
        ),
    ]
    rows = []
    for rid, name, rtype, signal, cmp_, warn, crit, minc, seasonal, svc, fp in _rules:
        rows.append(
            {
                "Id": rid,
                "Name": name,
                "RuleType": rtype,
                "SignalSource": "logs",
                "SignalName": signal,
                "ServiceName": svc,
                "AttrFingerprint": fp,
                "Comparator": cmp_,
                "WarningThreshold": warn,
                "CriticalThreshold": crit,
                "SecondarySignalSource": "",
                "SecondarySignalName": "",
                "SecondaryComparator": "gt",
                "SecondaryWarningThreshold": 0.0,
                "SecondaryCriticalThreshold": 0.0,
                "MinSampleCount": minc,
                "SeasonalBucketsJson": seasonal,
                "IsDeleted": 0,
                "Version": 1,
            }
        )
    _insert(db, "sobs_anomaly_rules", rows)
    db.execute("OPTIMIZE TABLE sobs_anomaly_rules FINAL")


def seed_metricsrich(db) -> None:
    # Populate the metrics index (GET /metrics, def view_metrics) so its POPULATED render branches
    # that the constant metricsauto seed never reaches all run byte-stably:
    #   * the OUTLIER anomaly_state badge (metrics.html 127 `bg-danger`) — needs a derived signal
    #     whose latest bucket deviates >3 stddev from its rolling baseline.
    #   * the populated RULE badge (metrics.html 132-135) + rule_state badge classes — needs an
    #     anomaly rule that MATCHES a rendered series and FIRES.
    #   * _evaluate_seasonal_rule's per-bucket-match path (app.py 13049-13067) — view_metrics is the
    #     caller that passes time_key="last_time", so a seasonal rule whose buckets cover the
    #     last_time hour runs the bucket lookup (is_seasonal=True) instead of the global fallback.
    #   * the attr_fp filter branch (app.py 13479-13480) — reached by ?attr_fp=<fp> in a route.
    #   * the pagination block (metrics.html 159-184) — reached by ?sort_by=signal&limit=2 routes.
    #
    # SINGLE service "web" on PURPOSE (mirrors seed_metricsauto/seed_summaryrich): the metrics
    # Service dropdown is _list_derived_signal_dimensions' DISTINCT-over-UNION whose trailing ORDER
    # BY binds only to the last branch -> genuinely racy once >1 derived-signal service exists. The
    # base fixture already carries exactly the lone "web" service, so reusing it keeps the dropdown a
    # singular, ordered badge. This profile is isolated, so the spike rows never ripple into base
    # readers.
    #
    # log_volume series for "web": 29 CONSTANT minute buckets at 5 logs each (now()-2 … now()-30)
    # then a FINAL spike bucket at now()-1 with 40 logs. The window (ROWS 59 PRECEDING) at the latest
    # bucket is fixed (30 buckets: 29x5 + 1x40), so mean/stddev/anomaly_score are timestamp-
    # independent: argMax(value,time)=40.0, anomaly_score=5.3852, anomaly_state='outlier',
    # SampleCount=40, point_count=30 — all exact/byte-stable (only the now()-anchored last_time
    # minute bucket drifts, masked in the routes, exactly like seed_metricsauto). error_volume /
    # error_ratio stay a constant-0 series (anomaly_state 'normal'). Three signals total -> the
    # default no-filter view has 3 rows (paginatable with limit=2, sort_by=signal for a total order).
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL (intDiv(number, 5) + 2) MINUTE, 'web', 'req' "
        "FROM numbers(145)"
    )
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL 1 MINUTE, 'web', 'spike' "
        "FROM numbers(40)"
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")

    # A SEASONAL anomaly rule matching (logs, log_volume, web). ServiceName 'web' + empty
    # AttrFingerprint -> _rule_matches_series matches the web log_volume series. The seasonal buckets
    # span ALL 24 hour-of-day keys with IDENTICAL thresholds (warning 10, critical 30), so regardless
    # of which hour the now()-anchored last_time lands in, _evaluate_seasonal_rule finds a bucket and
    # takes its (constant) thresholds -> is_seasonal=True and the rule fires deterministically:
    # value 40 >= critical 30 -> rule_state='outlier' (covers the bucket-match path 13049-13067 AND
    # the bg-danger rule badge). Fixed Id/Version keep the row byte-stable; view_metrics renders only
    # rule_name/rule_state/rule_reason (all constant here).
    import json as _json

    _all_hour_buckets = _json.dumps(
        {
            "strategy": "hour_of_day",
            "buckets": {str(h): {"warning": 10, "critical": 30} for h in range(24)},
        }
    )
    # A SECOND seasonal rule using strategy=day_of_week (all 7 weekday buckets identical) so
    # _evaluate_seasonal_rule's day_of_week bucket-key branch (app.py 13060) executes too. Its
    # thresholds are deliberately HIGH (warn 100 / crit 200) so value 40 crosses neither -> the inner
    # _evaluate_threshold_condition returns None -> this rule does NOT fire, leaving the hour_of_day
    # 'outlier' rule (rule-1) as the rendered best_match (so the byte-compared rule badge is stable).
    # All 7 buckets identical -> whichever weekday last_time lands on yields the same thresholds ->
    # deterministic regardless of capture/replay wall-clock day.
    _all_weekday_buckets = _json.dumps(
        {
            "strategy": "day_of_week",
            "buckets": {str(d): {"warning": 100, "critical": 200} for d in range(1, 8)},
        }
    )
    _insert(
        db,
        "sobs_anomaly_rules",
        [
            {
                "Id": "metr-rule-1",
                "Name": "z metricsrich log_volume seasonal",
                "RuleType": "seasonal",
                "SignalSource": "logs",
                "SignalName": "log_volume",
                "ServiceName": "web",
                "AttrFingerprint": "",
                "Comparator": "gt",
                "WarningThreshold": 10.0,
                "CriticalThreshold": 30.0,
                "SecondarySignalSource": "",
                "SecondarySignalName": "",
                "SecondaryComparator": "gt",
                "SecondaryWarningThreshold": 0.0,
                "SecondaryCriticalThreshold": 0.0,
                "MinSampleCount": 1,
                "SeasonalBucketsJson": _all_hour_buckets,
                "IsDeleted": 0,
                "Version": 1,
            },
            {
                "Id": "metr-rule-2",
                "Name": "a metricsrich log_volume seasonal dow",
                "RuleType": "seasonal",
                "SignalSource": "logs",
                "SignalName": "log_volume",
                "ServiceName": "web",
                "AttrFingerprint": "",
                "Comparator": "gt",
                "WarningThreshold": 100.0,
                "CriticalThreshold": 200.0,
                "SecondarySignalSource": "",
                "SecondarySignalName": "",
                "SecondaryComparator": "gt",
                "SecondaryWarningThreshold": 0.0,
                "SecondaryCriticalThreshold": 0.0,
                "MinSampleCount": 1,
                "SeasonalBucketsJson": _all_weekday_buckets,
                "IsDeleted": 0,
                "Version": 1,
            },
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_anomaly_rules FINAL")


def seed_tagsuggest(db) -> None:
    # Seed every table the tag-rule condition-suggestion builders read so each scope/target/field
    # branch of /api/settings/tags/condition-suggestions returns a non-empty, ranked result. The
    # base fixture has NO otel_logs/otel_traces/sobs_record_tags/sobs_log_attr_keys rows and its
    # hyperdx_sessions rows carry EventName='' (so they neither feed event_type nor v_derived
    # rum_vitals), so this profile's rows are the only data the builders see. Every row uses the
    # fixed determinism-window timestamp (_TS); the builders apply no time filter, and their
    # ORDER BY count() DESC, <grouped value> gives a total order (the tiebreak column is the
    # grouped value, hence distinct per group), so capture and replay are byte-identical.
    _insert(
        db,
        "otel_logs",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "checkout-api",
                "SeverityText": "ERROR",
                "Body": "payment declined",
                "EventName": "exception",
                "LogAttributes": {"http.method": "POST", "http.route": "/checkout"},
            },
            {
                "Timestamp": _TS,
                "ServiceName": "checkout-api",
                "SeverityText": "ERROR",
                "Body": "payment declined",
                "EventName": "exception",
                "LogAttributes": {"http.method": "POST", "http.route": "/checkout"},
            },
            {
                "Timestamp": _TS,
                "ServiceName": "checkout-api",
                "SeverityText": "INFO",
                "Body": "request handled",
                "EventName": "request",
                "LogAttributes": {"http.method": "GET", "http.route": "/health"},
            },
            {
                "Timestamp": _TS,
                "ServiceName": "payments-api",
                "SeverityText": "WARN",
                "Body": "retry scheduled",
                "EventName": "request",
                "LogAttributes": {"http.method": "PUT", "http.route": "/pay"},
            },
        ],
    )
    _insert(
        db,
        "otel_traces",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "checkout-api",
                "SpanName": "POST /checkout",
                "SpanAttributes": {"http.method": "POST", "rpc.service": "CheckoutService"},
            },
            {
                "Timestamp": _TS,
                "ServiceName": "frontend-web",
                "SpanName": "GET /home",
                "SpanAttributes": {"http.method": "GET"},
            },
            {
                "Timestamp": _TS,
                "ServiceName": "frontend-web",
                "SpanName": "GET /home",
                "SpanAttributes": {"http.method": "GET"},
            },
        ],
    )
    # A hyperdx_sessions row with a real EventName so the event_type union has a session source if
    # ever filtered for it (kept distinct from base 'web'/EventName='' rows).
    _insert(
        db,
        "hyperdx_sessions",
        [
            {"Timestamp": _TS, "ServiceName": "frontend-web", "EventName": "page_view", "Body": ""},
        ],
    )
    # sobs_record_tags is ReplacingMergeTree ORDER BY (RecordType, RecordId, TagKey): each row needs
    # a distinct RecordId so FINAL keeps all of them (two values under one TagKey must not collapse).
    _insert(
        db,
        "sobs_record_tags",
        [
            {
                "RecordType": "log",
                "RecordId": "rec-tagsuggest-1",
                "TagKey": "env",
                "TagValue": "production",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "RecordType": "log",
                "RecordId": "rec-tagsuggest-2",
                "TagKey": "env",
                "TagValue": "staging",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "RecordType": "log",
                "RecordId": "rec-tagsuggest-3",
                "TagKey": "team",
                "TagValue": "checkout",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "RecordType": "trace",
                "RecordId": "rec-tagsuggest-4",
                "TagKey": "env",
                "TagValue": "production",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
        ],
    )
    # sobs_log_attr_keys feeds _tag_rule_attribute_key_suggestions (the cache primes from this
    # table). Keys span the four record types the union covers (log/span/resource/scope).
    _insert(
        db,
        "sobs_log_attr_keys",
        [
            {"RecordType": "log", "AttrKey": "http.method", "IsDeleted": 0, "Version": 1704164644000},
            {"RecordType": "log", "AttrKey": "http.route", "IsDeleted": 0, "Version": 1704164644001},
            {"RecordType": "log", "AttrKey": "db.system", "IsDeleted": 0, "Version": 1704164644002},
            {"RecordType": "span", "AttrKey": "http.method", "IsDeleted": 0, "Version": 1704164644003},
            {"RecordType": "span", "AttrKey": "rpc.service", "IsDeleted": 0, "Version": 1704164644004},
            {"RecordType": "resource", "AttrKey": "service.version", "IsDeleted": 0, "Version": 1704164644005},
            {"RecordType": "scope", "AttrKey": "otel.scope.name", "IsDeleted": 0, "Version": 1704164644006},
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")
    db.execute("OPTIMIZE TABLE otel_traces FINAL")
    db.execute("OPTIMIZE TABLE hyperdx_sessions FINAL")
    db.execute("OPTIMIZE TABLE sobs_record_tags FINAL")
    db.execute("OPTIMIZE TABLE sobs_log_attr_keys FINAL")


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


def seed_notif_agent(db) -> None:
    # Exercise the AUTOMATIC agent-rule trigger branch of check_notifications (the path that the
    # base/notifcheck profiles never reach because AI is off there). Three isolated seeds:
    #   1. A tag rule whose (tag_key, tag_value) = ("env", "production").
    #   2. Recent auto-applied record tags matching it, so _collect_tag_rule_agent_events emits a
    #      "warning" event keyed by the tag rule's id. The tags are versioned at the real wall-clock
    #      now (NOT the frozen 2024 epoch) so they land inside the 5-minute lookback window for BOTH
    #      the Python capture and the Go replay (each re-seeds immediately before it runs); the count
    #      itself only feeds the (un-captured) trigger context, so the seeded value is irrelevant to
    #      the golden.
    #   3. An enabled, analyze-only agent rule with trigger_type=tag_rule pointing at that tag rule.
    # With AI configured (profile env -> agent + guard mock endpoints) the branch runs the full agent
    # flow (guard -> analyze LLM -> completed run) and returns it under agent_runs. No notification
    # rules are seeded, so the rule-evaluation loop fires nothing and consumes no uuid before the
    # agent run_id — keeping the (masked) uuid sequence identical to the manual agenttrigger path.
    _insert(
        db,
        "sobs_tag_rules",
        [
            {
                "Id": "ab000000000000000000000000000001",
                "Name": "Prod Env Tag",
                "RecordTypes": "",
                "MatchField": "",
                "MatchOperator": "",
                "MatchValue": "",
                "MatchAttrKey": "",
                "TagKey": "env",
                "TagValue": "production",
                "ConditionsJson": "",
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    # Recent auto tags (real now() so they're inside the 5-min Version window); count is uncaptured.
    db.execute(
        "INSERT INTO sobs_record_tags (RecordType, RecordId, TagKey, TagValue, IsAuto, IsDeleted, Version) "
        "SELECT 'log', concat('rec-', toString(number)), 'env', 'production', 1, 0, toUnixTimestamp64Milli(now64(3)) "
        "FROM numbers(7)"
    )
    _insert(
        db,
        "sobs_agent_rules",
        [
            {
                "Id": "e2000000000000000000000000000001",
                "Name": "Parity Tag Agent Rule",
                "Description": "Analyze-only agent rule auto-triggered by a tag-rule event.",
                "TriggerType": "tag_rule",
                "TriggerRefId": "ab000000000000000000000000000001",
                "TriggerState": "any",
                "Actions": "analyze",
                "RateLimitMinutes": 60,
                "IsEnabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_tag_rules FINAL")
    db.execute("OPTIMIZE TABLE sobs_record_tags FINAL")
    db.execute("OPTIMIZE TABLE sobs_agent_rules FINAL")


def seed_notif_agent_miss(db) -> None:
    # Cover the agent-trigger branch's NON-firing `continue` arms of check_notifications — the ones
    # the (firing) notifagent profile skips because its rule matches an event and runs the flow.
    # Same tag infra as notifagent (a tag rule + recent auto tags so _collect_tag_rule_agent_events
    # emits a "warning" event keyed by the tag rule id), but the seeded AGENT rules every hit a
    # `continue` and NEVER reach _run_agent_rule_instance — so no uuid/now() is consumed and
    # agent_runs stays [] (byte-stable, identical surface to the empty-agent path):
    #   - "A Disabled Watch" (is_enabled=0)            -> `if not is_enabled: continue`         26374
    #   - "B Anomaly Ref"   anomaly_rule + ref id      -> anomaly_events.get(ref)=None          26382-26383
    #                                                     -> `if not event: continue`           26398
    #     (NO anomaly events exist: v_derived_signals_anomaly is now()-24h-windowed with no
    #      in-window fixture rows, so _collect_anomaly_agent_events returns {}.)
    #   - "C Tag NoRef Crit" tag_rule, NO ref id,      -> event = all_tag_events[0]             26392-26393
    #     trigger_state="critical"                       -> state "warning" != "critical"
    #                                                     -> `if not state matches: continue`   26402
    # Names are A/B/C-prefixed so the ORDER BY Name load order is fixed and documented.
    # IMPORTANT: the firing "Parity Tag Agent Rule" (e2…0001) is NOT seeded here, so nothing runs.
    _insert(
        db,
        "sobs_tag_rules",
        [
            {
                "Id": "ab000000000000000000000000000001",
                "Name": "Prod Env Tag",
                "RecordTypes": "",
                "MatchField": "",
                "MatchOperator": "",
                "MatchValue": "",
                "MatchAttrKey": "",
                "TagKey": "env",
                "TagValue": "production",
                "ConditionsJson": "",
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    # Recent auto tags (real now() so they're inside the 5-min Version window). The count only feeds
    # the (un-captured) trigger context — and here no run is even reached — so the value is moot.
    db.execute(
        "INSERT INTO sobs_record_tags (RecordType, RecordId, TagKey, TagValue, IsAuto, IsDeleted, Version) "
        "SELECT 'log', concat('rec-', toString(number)), 'env', 'production', 1, 0, toUnixTimestamp64Milli(now64(3)) "
        "FROM numbers(7)"
    )
    _insert(
        db,
        "sobs_agent_rules",
        [
            {
                "Id": "f1000000000000000000000000000001",
                "Name": "A Disabled Watch",
                "Description": "Disabled agent rule (is_enabled continue arm).",
                "TriggerType": "tag_rule",
                "TriggerRefId": "ab000000000000000000000000000001",
                "TriggerState": "any",
                "Actions": "analyze",
                "RateLimitMinutes": 60,
                "IsEnabled": 0,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Id": "f1000000000000000000000000000002",
                "Name": "B Anomaly Ref",
                "Description": "anomaly_rule trigger with ref id but no event (continue).",
                "TriggerType": "anomaly_rule",
                "TriggerRefId": "no-such-anomaly-rule",
                "TriggerState": "any",
                "Actions": "analyze",
                "RateLimitMinutes": 60,
                "IsEnabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Id": "f1000000000000000000000000000003",
                "Name": "C Tag NoRef Crit",
                "Description": "tag_rule trigger, no ref -> all_tag_events[0]; state mismatch (continue).",
                "TriggerType": "tag_rule",
                "TriggerRefId": "",
                "TriggerState": "critical",
                "Actions": "analyze",
                "RateLimitMinutes": 60,
                "IsEnabled": 1,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_tag_rules FINAL")
    db.execute("OPTIMIZE TABLE sobs_record_tags FINAL")
    db.execute("OPTIMIZE TABLE sobs_agent_rules FINAL")


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


def seed_onboard_repos(db) -> None:
    # A configured global GitHub token so the onboarding READ endpoints take their token-USED
    # branch. list_repos then dials https://api.github.com/users/<owner>/repos?per_page=100&
    # type=all&sort=full_name (canned in migration/fixtures/upstream) -> token_used=true and an
    # empty visibility_note; inspect_repo runs the full repo-inspection GitHub flow off the same
    # global token (the repo-scoped key is absent). Token value is FIXED -> deterministic.
    # Also seed ONE registered app whose RepoUrl is the canned-fixture repo (testowner/demo-svc),
    # so inspect_repo can be exercised through its app_id -> sobs_apps RepoUrl resolution branch
    # (distinct from the `repo=` query branch the githubtoken profile already covers). Id is FIXED.
    _insert(
        db,
        "sobs_apps",
        [
            {
                "Id": "f1000000000000000000000000000002",
                "Name": "Demo Service",
                "Slug": "demo-service",
                "OwnerTeam": "platform",
                "RepoUrl": "https://github.com/testowner/demo-svc",
                "DefaultEnvironment": "prod",
                "Enabled": 1,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
                "CreatedAt": _TS,
                "UpdatedAt": _TS,
            }
        ],
    )
    _insert(
        db,
        "sobs_ai_settings",
        [{"Key": "ai.github_token", "Value": "ghp_parityfixturetoken", "IsDeleted": 0, "Version": 1704164645000}],
    )
    db.execute("OPTIMIZE TABLE sobs_apps FINAL")
    db.execute("OPTIMIZE TABLE sobs_ai_settings FINAL")


def seed_repohealth(db) -> None:
    # Exercises the populated _collect_github_repo_health_summary scan path (app.py 17944-18083),
    # reached by GET /api/enrichment/github/repo-health. Seeds a global GitHub token plus three
    # enabled apps, each with a parseable GitHub RepoUrl AND at least one release version, so each
    # becomes a repo_target and the token-gated per-repo /issues?state=open scan runs:
    #   * parityorg/health-svc (v3.1.0): canned /issues list with version-matching issues, PRs,
    #     security items (keyword + label), a non-matching item, and a non-dict item -> covers the
    #     issue/PR split, security detection, version-token filter, and the populated repos entry.
    #   * parityorg/quiet-svc (v9.9.9): canned EMPTY /issues list -> scanned, all-zero repos entry.
    #   * parityorg/dark-svc (v5.0.0): NO upstream fixture -> the mock returns 404, so the per-repo
    #     branch increments scanned_repos then `continue`s (resp.status_code != 200) and is NOT
    #     appended -> covers the non-200 skip. Names sort AAA/BBB/CCC so iteration order is fixed;
    #     last_synced_at is the frozen-clock _now_iso() (deterministic on both sides). Ids/token FIXED.
    _insert(
        db,
        "sobs_apps",
        [
            {
                "Id": "f1000000000000000000000000000031",
                "Name": "AAA Health Service",
                "Slug": "aaa-health-service",
                "OwnerTeam": "platform",
                "RepoUrl": "https://github.com/parityorg/health-svc",
                "DefaultEnvironment": "prod",
                "Enabled": 1,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
                "CreatedAt": _TS,
                "UpdatedAt": _TS,
            },
            {
                "Id": "f1000000000000000000000000000032",
                "Name": "BBB Quiet Service",
                "Slug": "bbb-quiet-service",
                "OwnerTeam": "platform",
                "RepoUrl": "https://github.com/parityorg/quiet-svc",
                "DefaultEnvironment": "prod",
                "Enabled": 1,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
                "CreatedAt": _TS,
                "UpdatedAt": _TS,
            },
            {
                "Id": "f1000000000000000000000000000033",
                "Name": "CCC Dark Service",
                "Slug": "ccc-dark-service",
                "OwnerTeam": "platform",
                "RepoUrl": "https://github.com/parityorg/dark-svc",
                "DefaultEnvironment": "prod",
                "Enabled": 1,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
                "CreatedAt": _TS,
                "UpdatedAt": _TS,
            },
        ],
    )
    _insert(
        db,
        "sobs_app_releases",
        [
            {
                "Id": "f1000000000000000000000000000041",
                "AppId": "f1000000000000000000000000000031",
                "ReleaseVersion": "3.1.0",
                "CommitSha": "",
                "BuildId": "",
                "Environment": "prod",
                "ReleasedAt": _TS,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
            },
            {
                "Id": "f1000000000000000000000000000042",
                "AppId": "f1000000000000000000000000000032",
                "ReleaseVersion": "9.9.9",
                "CommitSha": "",
                "BuildId": "",
                "Environment": "prod",
                "ReleasedAt": _TS,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
            },
            {
                "Id": "f1000000000000000000000000000043",
                "AppId": "f1000000000000000000000000000033",
                "ReleaseVersion": "5.0.0",
                "CommitSha": "",
                "BuildId": "",
                "Environment": "prod",
                "ReleasedAt": _TS,
                "MetadataJson": "{}",
                "IsDeleted": 0,
                "Version": 1704164645000,
            },
        ],
    )
    _insert(
        db,
        "sobs_ai_settings",
        [{"Key": "ai.github_token", "Value": "ghp_parityfixturetoken", "IsDeleted": 0, "Version": 1704164645000}],
    )
    db.execute("OPTIMIZE TABLE sobs_apps FINAL")
    db.execute("OPTIMIZE TABLE sobs_app_releases FINAL")
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
            {
                "Key": _app._ci_push_setting_key(CI_AUTH_APP_ID, "hash"),
                "Value": key_hash,
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Key": _app._ci_push_setting_key(CI_AUTH_APP_ID, "expires_at"),
                "Value": "2030-01-01T23:59:59+00:00",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
            {
                "Key": _app._ci_push_setting_key(CI_AUTH_APP_ID, "realtime_enabled"),
                "Value": "true",
                "IsDeleted": 0,
                "Version": 1704164644000,
            },
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


def seed_aiexport2(db) -> None:
    # export_ai_training's two json.loads EXCEPT branches (app.py 19185-19187 / 19193-19195): a
    # gen_ai span whose input/output message attrs are NON-JSON strings. json.loads(raw) then
    # raises JSONDecodeError, so the handler falls back to the extracted prompt/response text and
    # appends {"role":"user","content":<raw>} / {"role":"assistant","content":<raw>}. The raw value
    # is itself the fallback text: _extract_messages_text("...") returns the string unchanged when
    # it isn't JSON, so prompt/response are truthy and the `if prompt:` / `if response:` arms run.
    # All record fields come from the seeded row (fixed Timestamp/Duration/TraceId) -> deterministic.
    _insert(
        db,
        "otel_traces",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "sobs-ai-export2",
                "TraceId": "trace-aiexport2-001",
                "Duration": 2500000000,
                "SpanAttributes": {
                    "gen_ai.provider.name": "openai",
                    "gen_ai.request.model": "gpt-4o",
                    # Non-JSON strings: json.loads raises -> except branch -> prompt/response fallback.
                    "gen_ai.input.messages": "plain user question",
                    "gen_ai.output.messages": "plain assistant answer",
                    "gen_ai.usage.input_tokens": "11",
                    "gen_ai.usage.output_tokens": "22",
                },
            }
        ],
    )
    db.execute("OPTIMIZE TABLE otel_traces FINAL")


# Trace-detail waterfall fixture: a small multi-span trace (two roots — a request tree plus an
# async sibling that starts after the request finishes, creating a coverage gap) so view_traces'
# trace_id branch exercises every helper (span tree, interval merge, active/coverage, timeline
# active+gap segments) and the per-span offset/width math. Spans land at 2023-06-01 — far outside
# the frozen now()-48h anomaly window — so the per-service anomaly lookup is deterministically
# empty; no metrics / raw windows are seeded, so the metric-context + window-overlay blocks take
# their empty paths. A few non-error logs drive log_counts; one log lands in the gap so the
# "potential instrumentation gap" flag fires. Status codes / http statuses / durations are chosen
# to span the template's success / unset / error / outlier branches.
_TRACE_DETAIL_TRACE_ID = "aaaa1111bbbb2222cccc3333dddd4444"


def seed_trace_detail(db) -> None:
    tid = _TRACE_DETAIL_TRACE_ID

    def span(ts_frac, span_id, parent, service, name, dur_ms, status, attrs):
        return {
            "Timestamp": f"2023-06-01 12:00:00.{ts_frac}",
            "TraceId": tid,
            "SpanId": span_id,
            "ParentSpanId": parent,
            "SpanName": name,
            "ServiceName": service,
            "Duration": int(dur_ms * 1_000_000),  # ns
            "StatusCode": status,
            "SpanAttributes": attrs,
        }

    _insert(
        db,
        "otel_traces",
        [
            # root request span [0, 80ms]; carries http + k8s attrs (drives the metric-context
            # dimension collection, which still resolves to "no match" with no metrics seeded).
            span(
                "000000",
                "aaaa000000000001",
                "",
                "checkout",
                "GET /checkout",
                80,
                "STATUS_CODE_OK",
                {
                    "http.method": "GET",
                    "http.url": "/checkout",
                    "http.status_code": "200",
                    "k8s.namespace.name": "shop",
                    "k8s.pod.name": "checkout-7d",
                    "k8s.node.name": "node-a",
                    "k8s.deployment.name": "checkout",
                },
            ),
            # child of root [10, 40ms]; UNSET status -> secondary badge branch.
            span("010000", "aaaa000000000002", "aaaa000000000001", "checkout", "validate-cart", 30, "UNSET", {}),
            # child of root [50, 170ms]; ERROR status + http 500 -> error styling branch.
            span(
                "050000",
                "aaaa000000000003",
                "aaaa000000000001",
                "checkout-db",
                "SELECT items",
                120,
                "STATUS_CODE_ERROR",
                {"http.status_code": "500"},
            ),
            # grandchild (child of validate-cart) [15, 20ms]; tiny span -> 0.5% min width branch.
            span("015000", "aaaa000000000004", "aaaa000000000002", "checkout", "cache-get", 5, "STATUS_CODE_OK", {}),
            # second root [400, 1500ms]; >=1s, not error -> outlier badge branch; starts after the
            # request tree ends (170ms) -> coverage gap [170, 400ms].
            span(
                "400000",
                "aaaa000000000005",
                "",
                "checkout",
                "async-flush",
                1100,
                "STATUS_CODE_OK",
                {"http.status_code": "404"},
            ),
        ],
    )

    # Non-error logs: two on validate-cart, one on the db span (-> log_counts), and one in the
    # coverage gap with no span id (-> activity that flips the gap to "potential"). Default
    # SeverityNumber/SeverityText/EventName keep these out of ERROR_SOURCES_SQL.
    def logrow(ts_frac, span_id, service, body):
        return {
            "Timestamp": f"2023-06-01 12:00:00.{ts_frac}",
            "TraceId": tid,
            "SpanId": span_id,
            "ServiceName": service,
            "Body": body,
        }

    _insert(
        db,
        "otel_logs",
        [
            logrow("010000", "aaaa000000000002", "checkout", "cart validated"),
            logrow("012000", "aaaa000000000002", "checkout", "cart items=3"),
            logrow("055000", "aaaa000000000003", "checkout-db", "query ok"),
            logrow("250000", "", "checkout", "async scheduled"),
        ],
    )
    db.execute("OPTIMIZE TABLE otel_traces FINAL")
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_tracedetailerr(db) -> None:
    # Same multi-span trace as seed_trace_detail, PLUS one ERROR otel_logs row carrying the trace's
    # id so view_traces' trace-detail "errors" loop (app.py 15481-15488) executes and renders an
    # error item. seed_trace_detail seeds NON-error logs only, so that loop body is otherwise never
    # reached (err_rows is empty). Isolated profile: the base tracedetail goldens are untouched.
    #
    # The error row attaches to the existing error span (aaaa000000000003, the STATUS_CODE_ERROR
    # "SELECT items" span) so it lands inside the trace's span tree (error_span_ids -> that span's
    # error badge stays deterministic). ERROR_SOURCES_SQL matches it via EventName='exception' /
    # SeverityNumber>=17 / SeverityText='ERROR' / exception.type set. Exactly ONE error row: the
    # trace-error query has NO ORDER BY (FROM (ERROR_SOURCES_SQL) WHERE TraceId=? LIMIT N), so a
    # single row removes any chDB row-order nondeterminism while still covering the loop body.
    # Fixed 2023 timestamp + plain-text (non-JSON) message keep the render byte-reproducible
    # (the *_is_json branches stay False).
    seed_trace_detail(db)
    _insert(
        db,
        "otel_logs",
        [
            {
                "Timestamp": "2023-06-01 12:00:00.060000",
                "TraceId": _TRACE_DETAIL_TRACE_ID,
                "SpanId": "aaaa000000000003",
                "ServiceName": "checkout-db",
                "SeverityText": "ERROR",
                "SeverityNumber": 17,
                "EventName": "exception",
                "Body": "connection reset by peer",
                "LogAttributes": {
                    "exception.type": "ConnectionError",
                    "exception.message": "connection reset by peer",
                },
            }
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_incidentmatch(db) -> None:
    # Cover view_incident's two remaining deterministically-reachable MATCH branches (app.py
    # view_incident at 15785):
    #   1. rum_session MATCH (app.py 15904-15905 + 15918-15920): a hyperdx_sessions row whose
    #      session key (_RUM_SESSION_KEY_SQL) equals the requested ?rum_session id, so
    #      `if rum_row: primary_rum = _build_rum_event_item(rum_row)` runs and the
    #      `elif primary_rum:` branch sets service/event_ts. The base seed_rum_sessions rows carry
    #      NO sessionId (anon md5 key) so a clean ?rum_session=X never matches them — this row sets
    #      LogAttributes['sessionId'] explicitly so the key resolves to 'incident-sess-001'.
    #   2. existing_work_item (app.py 16113-16114): a sobs_github_work_items row whose AnomalyRuleId
    #      equals that same rum_session id with a non-empty IssueUrl, so _load_work_item_links_for_ref_ids
    #      returns a link for ref_id 'incident-sess-001' and the `existing_work_item = wi; break` runs.
    #
    # Determinism notes (ALL fixed 2023 timestamps, NO now()):
    #   • Unique ServiceName ('incident-web-001') so the derived `service` matches no base rows: the
    #     related-errors / related-logs / related-spans queries (filtered by ServiceName in the fixed
    #     2023 window) all return empty.
    #   • The RUM row carries NO LogAttributes['service.name'/'service'], so the related-RUM summary
    #     (filtered by those keys) does not even match this row -> related_rum_count=0. Plain-text
    #     (non-JSON) Body keeps _build_rum_event_item's data.* branches empty and byte-stable.
    #   • from_ts/to_ts are derived from the RUM event's FIXED timestamp (deterministic). The
    #     window/metrics block runs but finds no seeded raw windows / metrics for the unique service
    #     in the 2023 window -> empty metrics_context. The anomaly query uses now()-48h, which the
    #     fixed-2023 service never falls in -> anomaly_state=None.
    _insert(
        db,
        "hyperdx_sessions",
        [
            {
                "Timestamp": "2023-06-01 12:00:00.000000",
                "ServiceName": "incident-web-001",
                "EventName": "error",
                "Body": "TypeError: undefined is not a function",
                "TraceId": "9999000000000000000000000000aaaa",
                "SpanId": "9999000000000001",
                "LogAttributes": {
                    "sessionId": "incident-sess-001",
                    "url": "https://shop.example.com/checkout",
                },
            }
        ],
    )
    db.execute("OPTIMIZE TABLE hyperdx_sessions FINAL")
    _insert(
        db,
        "sobs_github_work_items",
        [
            {
                "Id": "d1000000000000000000000000000001",
                "CreatedAt": "2023-06-01 12:05:00.000000",
                "CompletedAt": "2023-06-01 12:05:00.000000",
                "AgentRunId": "ar000000000000000000000000000d01",
                "AgentRuleId": "",
                "AgentRuleName": "User Raised Issue (incident)",
                "AgentAction": "github_issue",
                "ServiceName": "incident-web-001",
                # _load_work_item_links_for_ref_ids keys on AnomalyRuleId == ref_id (here the
                # rum_session id), and requires IssueUrl != '' to surface the link.
                "AnomalyRuleId": "incident-sess-001",
                "AnomalyState": "critical",
                "SignalSource": "rum",
                "SignalName": "TypeError",
                "SignalValue": 1.0,
                "GithubRepo": "acme/reuse-demo",
                "DedupKey": "incident-web-001||rum|typeerror|critical",
                "DedupDecision": "new_issue",
                "DedupConfidence": 1.0,
                "IssueNumber": 77,
                "IssueUrl": "https://github.com/acme/reuse-demo/issues/77",
                "CanonicalIssueNumber": 77,
                "CanonicalIssueUrl": "https://github.com/acme/reuse-demo/issues/77",
                "RelatedIssueUrls": "[]",
                "OccurrenceCount": 1,
                "IssueState": "open",
                "IssueTitle": "RUM TypeError on checkout",
                "AnalysisSummary": "",
                "SuggestionSummary": "",
                "CopilotAssignmentRequestedAt": 0,
                "CopilotAssignmentStatus": "not_requested",
                "CopilotAssignmentReason": "",
                "PrLinked": 0,
                "PrNumber": 0,
                "PrUrl": "",
                "IsDeleted": 0,
                "Version": 1685620500000,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_github_work_items FINAL")


def seed_logsview(db) -> None:
    # Populate otel_logs so view_logs (GET /logs) renders its POPULATED branches. Six rows with
    # DISTINCT fixed timestamps (deterministic ORDER BY Timestamp DESC). Severity and service counts
    # are kept DISTINCT (ERROR=3, WARN=2, INFO=1; api=4, worker=2) so the stats queries' ORDER BY cnt
    # DESC has no ties — a tie would let ClickHouse pick either order and break byte-parity. The fixed
    # 2023 timestamps + the frozen now() keep the stats-snapshot age block deterministic.
    def logrow(frac, service, severity, sevnum, event, trace, span, body):
        return {
            "Timestamp": f"2023-06-01 12:00:00.{frac}",
            "ServiceName": service,
            "SeverityText": severity,
            "SeverityNumber": sevnum,
            "EventName": event,
            "TraceId": trace,
            "SpanId": span,
            "Body": body,
            "LogAttributes": {},
        }

    _insert(
        db,
        "otel_logs",
        [
            logrow(
                "100000000",
                "api",
                "ERROR",
                17,
                "http.request",
                "trace-aaa",
                "span-0001",
                "database connection timeout after 5000ms",
            ),
            logrow(
                "200000000",
                "api",
                "ERROR",
                17,
                "http.request",
                "trace-aaa",
                "span-0002",
                "upstream returned status 503",
            ),
            logrow(
                "300000000", "api", "ERROR", 17, "http.request", "trace-bbb", "span-0003", "request failed validation"
            ),
            logrow(
                "400000000",
                "api",
                "WARN",
                13,
                "http.request",
                "trace-bbb",
                "span-0004",
                "slow query detected on orders",
            ),
            logrow("500000000", "worker", "WARN", 13, "job.run", "trace-ccc", "span-0005", "queue backlog growing"),
            logrow("600000000", "worker", "INFO", 9, "job.run", "trace-ccc", "span-0006", "batch job completed"),
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")
    # Two tags on the first (ERROR/api/trace-aaa) record so the batch-tag join (view_logs) and a
    # has_tag('env','prod') SQL filter both resolve. RecordId is computed chdb-side with the SAME
    # lower(hex(MD5(concat(...)))) the app's _record_id_for_log produces, so it matches what both the
    # Python capture and the Go replay compute at read time.
    for tk, tv in (("env", "prod"), ("owner", "team-a")):
        db.execute(
            "INSERT INTO sobs_record_tags "
            "(RecordType, RecordId, TagKey, TagValue, IsAuto, IsDeleted, Version) "
            "SELECT 'log', "
            "lower(hex(MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)))), "
            f"'{tk}', '{tv}', 0, 0, 1704164644000 "
            "FROM otel_logs WHERE SpanId='span-0001'"
        )
    db.execute("OPTIMIZE TABLE sobs_record_tags FINAL")


def seed_logsrich(db) -> None:
    # Richer view_logs (GET /logs) fixture, kept SEPARATE from logsview so the 12 logsview goldens
    # don't shift. Two otel_logs rows with DISTINCT fixed timestamps so any ORDER BY Timestamp DESC
    # is tie-free and deterministic. The point of this profile is to drive view_logs' query-execution
    # ERROR branch (app.py 11402-11404): a route passes a raw `sql` WHERE fragment that PASSES
    # _validate_user_sql_where (no write/DDL keyword) but references a column that does not exist, so
    # chdb raises UNKNOWN_IDENTIFIER on the COUNT query inside the try -> the `except` sets
    # error_msg = "SQL error: " + _public_dashboard_query_error(exc). That sanitized message is the
    # SAME on both sides (identical libchdb + identical SQL), so it is byte-deterministic with no
    # now()/uuid/elapsed content. The seeded rows make the seed non-empty (the error fires before
    # the rows query, but the rows existing keeps the profile reusable for future populated branches).
    def logrow(frac, service, severity, sevnum, event, trace, span, body):
        return {
            "Timestamp": f"2023-07-01 09:00:00.{frac}",
            "ServiceName": service,
            "SeverityText": severity,
            "SeverityNumber": sevnum,
            "EventName": event,
            "TraceId": trace,
            "SpanId": span,
            "Body": body,
            "LogAttributes": {},
        }

    _insert(
        db,
        "otel_logs",
        [
            logrow("100000000", "api", "ERROR", 17, "http.request", "rtrace-aaa", "rspan-0001", "boom one"),
            logrow("200000000", "worker", "INFO", 9, "job.run", "rtrace-bbb", "rspan-0002", "ok two"),
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_errorsview(db) -> None:
    # Error events for view_errors (ERROR_SOURCES_SQL = otel_logs rows with EventName='exception'
    # / SeverityNumber>=17 / SeverityText in ERROR,CRITICAL,FATAL / exception.type set). Three
    # groups keyed by (lower service, lower exception.type, lower exception.message) with DISTINCT
    # row counts 3/2/1 so the grouped ORDER BY Count DESC is tie-free, and ONE TraceId per group so
    # groupUniqArray(TraceId) is single-element (deterministic). Distinct fixed timestamps make
    # argMax(*, Timestamp) and ORDER BY Timestamp DESC stable. Plain-text message/Body (not JSON)
    # so the *_is_json branches stay False and the render is deterministic.
    def errrow(frac, service, etype, emsg, trace, span):
        return {
            "Timestamp": f"2023-06-01 14:00:00.{frac}",
            "ServiceName": service,
            "SeverityText": "ERROR",
            "SeverityNumber": 17,
            "EventName": "exception",
            "TraceId": trace,
            "SpanId": span,
            "Body": emsg,
            "LogAttributes": {"exception.type": etype, "exception.message": emsg},
        }

    _insert(
        db,
        "otel_logs",
        [
            # Group A: api / ValueError -> count 3
            errrow("100000000", "api", "ValueError", "bad input on field amount", "errtraceaaaa", "errspan-a1"),
            errrow("200000000", "api", "ValueError", "bad input on field amount", "errtraceaaaa", "errspan-a2"),
            errrow("300000000", "api", "ValueError", "bad input on field amount", "errtraceaaaa", "errspan-a3"),
            # Group B: api / KeyError -> count 2
            errrow("400000000", "api", "KeyError", "missing key user_id", "errtracebbbb", "errspan-b1"),
            errrow("500000000", "api", "KeyError", "missing key user_id", "errtracebbbb", "errspan-b2"),
            # Group C: worker / TimeoutError -> count 1
            errrow("600000000", "worker", "TimeoutError", "operation timed out", "errtracecccc", "errspan-c1"),
        ],
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")
    # Resolve group C's single event so resolved=1 returns it and resolved=0 returns A+B. ErrorId is
    # computed chdb-side with the SAME expression app._error_id_sql_expr() uses, so the resolved
    # subquery (error_id_sql IN sobs_error_resolutions) matches on both the Python and Go sides.
    err_id_sql = (
        "lower(hex(MD5(concat("
        "toString(Timestamp), '|', ServiceName, '|', "
        "if(mapContains(LogAttributes, 'exception.type'), LogAttributes['exception.type'], 'Error'), '|', "
        "if(mapContains(LogAttributes, 'exception.message'), LogAttributes['exception.message'], Body), '|', "
        "TraceId, '|', SpanId"
        "))))"
    )
    db.execute(
        "INSERT INTO sobs_error_resolutions (ErrorId, ResolvedAt) "
        f"SELECT {err_id_sql}, '2024-01-02 03:00:00.000' "
        "FROM otel_logs WHERE SpanId='errspan-c1'"
    )
    db.execute("OPTIMIZE TABLE sobs_error_resolutions FINAL")


def seed_aiview(db) -> None:
    # otel_traces AI spans for view_ai (_AI_SPAN_CONDITION = any of gen_ai.provider.name /
    # gen_ai.system / gen_ai.operation.name set). Distinct timestamps (stable ORDER BY Timestamp
    # DESC), distinct models/services (tie-free filter metadata), empty message JSON (the genai
    # message parsers return empty → deterministic, dodging message/turn-card formatting). Two
    # spans share a TraceId (trace-view grouping); a third is a model-less system span carrying an
    # error.type (drives the errors total + row_type=system filter).
    def aispan(frac, span_id, service, name, dur_ms, trace, attrs):
        return {
            "Timestamp": f"2023-06-01 16:00:00.{frac}",
            "TraceId": trace,
            "SpanId": span_id,
            "ParentSpanId": "",
            "SpanName": name,
            "ServiceName": service,
            "Duration": int(dur_ms * 1_000_000),  # ns
            "StatusCode": "STATUS_CODE_OK",
            "SpanAttributes": attrs,
        }

    _insert(
        db,
        "otel_traces",
        [
            aispan(
                "100000000",
                "aispan-0001",
                "ai-svc",
                "ai.chat",
                2000,
                "aitrace1",
                {
                    "gen_ai.provider.name": "anthropic",
                    "gen_ai.request.model": "claude-opus",
                    "gen_ai.operation.name": "chat",
                    "gen_ai.usage.input_tokens": "100",
                    "gen_ai.usage.output_tokens": "50",
                },
            ),
            aispan(
                "200000000",
                "aispan-0002",
                "ai-svc",
                "ai.chat",
                1500,
                "aitrace1",
                {
                    "gen_ai.provider.name": "openai",
                    "gen_ai.request.model": "gpt-4",
                    "gen_ai.operation.name": "chat",
                    "gen_ai.usage.input_tokens": "200",
                    "gen_ai.usage.output_tokens": "80",
                },
            ),
            aispan(
                "300000000",
                "aispan-0003",
                "worker",
                "ai.embed",
                500,
                "aitrace2",
                {
                    "gen_ai.provider.name": "anthropic",
                    "gen_ai.operation.name": "embed",
                    "error.type": "RateLimitError",
                    "exception.message": "rate limited",
                },
            ),
        ],
    )
    db.execute("OPTIMIZE TABLE otel_traces FINAL")


_TRACESRICH_TRACE_ID = "bbbb1111cccc2222dddd3333eeee4444"


def seed_tracesrich(db) -> None:
    # view_traces trace-detail "errors_truncated" branch (app.py 15479-15480). The trace-error query
    # fetches LIMIT _TRACE_ERROR_LIMIT+1 (=51) and, if it gets >50, flips errors_truncated=True and
    # slices to the first 50. Seed a tiny SINGLE-span trace + EXACTLY 51 BYTE-IDENTICAL error logs on
    # it: the inner query has NO ORDER BY (FROM (ERROR_SOURCES_SQL) WHERE TraceId=? LIMIT 51) so chdb
    # may return the 51 rows in any order, but because every error row is identical the rendered
    # accordion (loop.index-keyed, 50 identical items after truncation) is permutation-invariant and
    # byte-reproducible. Fixed 2023 timestamps + plain-text (non-JSON) message keep the *_is_json
    # branches False. Isolated profile (unique trace id) — base tracedetail/tracedetailerr untouched.
    tid = _TRACESRICH_TRACE_ID
    _insert(
        db,
        "otel_traces",
        [
            {
                "Timestamp": "2023-07-01 09:00:00.000000",
                "TraceId": tid,
                "SpanId": "bbbb000000000001",
                "ParentSpanId": "",
                "SpanName": "GET /orders",
                "ServiceName": "orders",
                "Duration": int(40 * 1_000_000),  # ns
                "StatusCode": "STATUS_CODE_ERROR",
                "SpanAttributes": {"http.status_code": "500"},
            }
        ],
    )

    # 51 byte-identical ERROR logs, all attached to the single span on this trace. ERROR_SOURCES_SQL
    # matches via EventName='exception' / SeverityNumber>=17 / SeverityText='ERROR' / exception.type.
    err_logs = [
        {
            "Timestamp": "2023-07-01 09:00:00.010000",
            "TraceId": tid,
            "SpanId": "bbbb000000000001",
            "ServiceName": "orders",
            "SeverityText": "ERROR",
            "SeverityNumber": 17,
            "EventName": "exception",
            "Body": "order processing failed",
            "LogAttributes": {
                "exception.type": "OrderError",
                "exception.message": "order processing failed",
            },
        }
        for _ in range(51)
    ]
    _insert(db, "otel_logs", err_logs)
    db.execute("OPTIMIZE TABLE otel_traces FINAL")
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_airich(db) -> None:
    # view_ai _safe_attr_int defensive branches (app.py 18757-18758 ValueError; 18760 NaN/inf guard).
    # ONE AI span (gen_ai.* attrs so _AI_SPAN_CONDITION matches) carrying a NON-NUMERIC input_tokens
    # ("abc" -> float() raises -> 18757-18758 -> 0) and an INFINITE output_tokens ("inf" -> float()
    # succeeds -> NaN/inf guard 18760 -> 0). Both render as 0, so the token cells are deterministic.
    # Distinct fixed 2023 timestamp + a unique service/model keep ORDER BY / filter metadata tie-free.
    # Isolated profile — base aiview goldens are untouched. (_safe_duration_ms's matching branches are
    # unreachable: Duration is a UInt64 column so r["Duration"] is always a parseable non-NaN int.)
    _insert(
        db,
        "otel_traces",
        [
            {
                "Timestamp": "2023-07-01 11:00:00.000000",
                "TraceId": "airichtrace1",
                "SpanId": "airichspan01",
                "ParentSpanId": "",
                "SpanName": "ai.chat",
                "ServiceName": "airich-svc",
                "Duration": int(1200 * 1_000_000),  # ns
                "StatusCode": "STATUS_CODE_OK",
                "SpanAttributes": {
                    "gen_ai.provider.name": "anthropic",
                    "gen_ai.request.model": "claude-airich",
                    "gen_ai.operation.name": "chat",
                    # non-numeric -> _safe_attr_int ValueError branch
                    "gen_ai.usage.input_tokens": "abc",
                    # parses to +inf -> _safe_attr_int NaN/inf guard branch
                    "gen_ai.usage.output_tokens": "inf",
                },
            }
        ],
    )
    db.execute("OPTIMIZE TABLE otel_traces FINAL")


def seed_dashview(db) -> None:
    # One dashboard + two charts with FIXED ids so GET /dashboards/<id> (view_custom_dashboard)
    # renders its view branch against real data. The base example seeder also creates an example
    # dashboard, but its id is determinism-derived; this profile pins a known id for the manifest
    # path. Each chart's OptionsJson is built EXACTLY as _seed_chart_if_missing does
    # (json.dumps({"chart_spec": _build_raw_chart_spec(chart_type, query)}, ensure_ascii=False)), so
    # _get_charts' normalize-from-stored-spec path is exercised identically on both sides. Two
    # distinct template types (builder + raw) cover the common rebuild path. Version is fixed so the
    # ReplacingMergeTree FINAL read is deterministic.
    import json as _json

    import app as _app

    dash_id = "d0000000-0000-4000-8000-00000000d001"
    _insert(
        db,
        "sobs_dashboards",
        [
            {
                "Id": dash_id,
                "Name": "Parity View Dashboard",
                "Description": "Seeded dashboard for the dashboard-view parity route.",
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    charts = [
        (
            "d0000000-0000-4000-8000-00000000c001",
            "Trace volume overlay",
            "derived_signal_overlay",
            "SELECT time, value FROM v_derived_signals_anomaly WHERE service = 'web' ORDER BY time",
            0,
        ),
        (
            "d0000000-0000-4000-8000-00000000c002",
            "Latency percentiles",
            "time_series_percentiles",
            "SELECT time, value, p95, p99 FROM otel_traces ORDER BY time",
            1,
        ),
    ]
    _insert(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": cid,
                "DashboardId": dash_id,
                "Title": title,
                "ChartType": ctype,
                "Query": query,
                "OptionsJson": _json.dumps(
                    {"chart_spec": _app._build_raw_chart_spec(ctype, query)},
                    ensure_ascii=False,
                ),
                "Position": pos,
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
            for cid, title, ctype, query, pos in charts
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_dashboards FINAL")
    db.execute("OPTIMIZE TABLE sobs_chart_configs FINAL")


def seed_chartedit(db) -> None:
    # ONE dashboard (d0…d001) + ONE chart (c0…c001) with FIXED ids so the chart-MUTATION form
    # routes (edit_chart / clone_chart) reach their real branches: the source chart EXISTS inside
    # an existing dashboard, so the lookup `next(c for c in charts if c["id"]==chart_id)` matches
    # and the parse+insert path runs. ISOLATED from the dashview profile (which uses the same dash
    # id but different env scope and is read-only) so the mutating POSTs here never ripple into the
    # base dashboard readers. Both handlers redirect to view_custom_dashboard(dashboard_id=<this
    # existing id>) — the redirect body carries only the EXISTING dashboard id, so even clone's
    # server-generated chart uuid never reaches the response: both success bodies are byte-stable.
    # The chart's OptionsJson is built EXACTLY as _seed_chart_if_missing does so _get_charts'
    # normalize-from-stored-spec path is identical on both sides. Version is fixed for a stable
    # ReplacingMergeTree FINAL read.
    import json as _json

    import app as _app

    dash_id = "d0000000-0000-4000-8000-00000000d001"
    _insert(
        db,
        "sobs_dashboards",
        [
            {
                "Id": dash_id,
                "Name": "Parity Chart-Edit Dashboard",
                "Description": "Seeded dashboard for the chart edit/clone mutation parity routes.",
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    chart_id = "c0000000-0000-4000-8000-00000000c001"
    chart_type = "anomaly_overlay"
    query = "SELECT 1 AS time, 2 AS value"
    _insert(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dash_id,
                "Title": "Editable chart",
                "ChartType": chart_type,
                "Query": query,
                "OptionsJson": _json.dumps(
                    {"chart_spec": _app._build_raw_chart_spec(chart_type, query)},
                    ensure_ascii=False,
                ),
                "Position": 0,
                "IsDeleted": 0,
                "Version": 1704164644000,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_dashboards FINAL")
    db.execute("OPTIMIZE TABLE sobs_chart_configs FINAL")


# validate-regex sample-probe seeds. Each inserts ONE row at now()-1h (real wall-clock, like
# seed_tagauto — NOT the frozen 2024 epoch) so it lands inside the route's now()-24h candidate
# window at both capture and replay time, while the base fixture's frozen-epoch rows stay out of
# window. The sample column carries a FIXED token, so the validate-regex success branch returns
# that exact string regardless of the (varying) seed/read wall-clock — byte-identical goldens.
# Isolated per profile so the seeded telemetry never ripples into base readers.


def seed_regex_logs(db) -> None:
    # /api/logs/validate-regex reads otel_logs.Body. One in-window row -> the sole candidate, so a
    # pattern matching its Body deterministically returns that Body.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'regex-probe-svc', 'sobs-regex-probe-logs-hit' FROM numbers(1)"
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_regex_traces(db) -> None:
    # /api/traces/validate-regex reads otel_traces.SpanName.
    db.execute(
        "INSERT INTO otel_traces (Timestamp, ServiceName, SpanName) "
        "SELECT now() - INTERVAL 1 HOUR, 'regex-probe-svc', 'sobs-regex-probe-traces-hit' FROM numbers(1)"
    )
    db.execute("OPTIMIZE TABLE otel_traces FINAL")


def seed_regex_rum(db) -> None:
    # /api/rum/validate-regex reads hyperdx_sessions.Body.
    db.execute(
        "INSERT INTO hyperdx_sessions (Timestamp, ServiceName, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'regex-probe-svc', 'sobs-regex-probe-rum-hit' FROM numbers(1)"
    )
    db.execute("OPTIMIZE TABLE hyperdx_sessions FINAL")


def seed_regex_errors(db) -> None:
    # /api/errors/validate-regex reads Body from ERROR_SOURCES_SQL (otel_logs ∪ hyperdx_sessions
    # filtered to error rows). SeverityText='ERROR' makes the row qualify as an error source.
    db.execute(
        "INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, Body) "
        "SELECT now() - INTERVAL 1 HOUR, 'regex-probe-svc', 'ERROR', 'sobs-regex-probe-errors-hit' FROM numbers(1)"
    )
    db.execute("OPTIMIZE TABLE otel_logs FINAL")


def seed_cveview(db) -> None:
    # CVE findings, one per severity with DISTINCT Published (tie-free ORDER BY Published DESC) so the
    # summary cve-overview counts and view_enrichment_cve findings loop/filters render populated. No
    # inventory or dispositions seeded -> _effective_cve_disposition resolves "open" for all, which is
    # kept under the default show_all=off. Fixed Published/ScannedAt -> deterministic (no wall-clock).
    _insert(
        db,
        "sobs_cve_findings",
        [
            {
                "Package": "requests",
                "Ecosystem": "PyPI",
                "Version": "2.0.0",
                "ServiceName": "api",
                "OsvId": "GHSA-aaaa-0001",
                "CveIds": "CVE-2024-0001",
                "Summary": "Critical RCE in requests",
                "Severity": "CRITICAL",
                "Published": "2024-01-04 00:00:00",
                "ScannedAt": _TS,
            },
            {
                "Package": "flask",
                "Ecosystem": "PyPI",
                "Version": "1.0.0",
                "ServiceName": "api",
                "OsvId": "GHSA-aaaa-0002",
                "CveIds": "CVE-2024-0002",
                "Summary": "High severity flask issue",
                "Severity": "HIGH",
                "Published": "2024-01-03 00:00:00",
                "ScannedAt": _TS,
            },
            {
                "Package": "lodash",
                "Ecosystem": "npm",
                "Version": "4.0.0",
                "ServiceName": "web",
                "OsvId": "GHSA-aaaa-0003",
                "CveIds": "CVE-2024-0003",
                "Summary": "Medium prototype pollution",
                "Severity": "MEDIUM",
                "Published": "2024-01-02 00:00:00",
                "ScannedAt": _TS,
            },
            {
                "Package": "axios",
                "Ecosystem": "npm",
                "Version": "0.1.0",
                "ServiceName": "web",
                "OsvId": "GHSA-aaaa-0004",
                "CveIds": "CVE-2024-0004",
                "Summary": "Low severity axios advisory",
                "Severity": "LOW",
                "Published": "2024-01-01 00:00:00",
                "ScannedAt": _TS,
            },
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_cve_findings FINAL")
    # Non-integer backfill-stat settings so view_enrichment_cve hits the int() except (TypeError,
    # ValueError) -> 0 fallbacks (app.py 18112-18121). The except still yields 0 — byte-identical to
    # the unset case — so the existing cveview goldens are unchanged; only the code path differs. Go's
    # appSettingIntOrZero("x") -> strconv.Atoi error -> 0 matches, keeping parity GREEN.
    import app as _app

    _app._set_app_setting(db, "enrichment.cve_last_scan_github_backfill_attempted", "x")
    _app._set_app_setting(db, "enrichment.cve_last_scan_github_backfill_inserted", "x")
    _app._set_app_setting(db, "enrichment.cve_last_scan_github_backfill_cap", "x")


def seed_enrichlibs(db) -> None:
    # Populated library inventory for api_enrichment_libraries (GET /api/enrichment/libraries).
    # Three otel_traces rows give _collect_library_inventory three distinct libraries, one per
    # status, and one matching sobs_cve_findings row makes the SDK library "vulnerable". Every
    # value is a FIXED string (no now()/uuid), and the handler RE-SORTS the merged list by
    # (cve_count desc, source order, package lower, version lower, service lower) — so the output
    # order is fully determined by these distinct keys and is byte-reproducible regardless of the
    # dict/insert iteration order on either side. scanned_at stays "" (cve_last_scan unset).
    #
    #   tier 2 (otel_sdk):    package="opentelemetry"   ecosystem=PyPI  cve_count=1 -> "vulnerable"
    #   tier 3 (otel_scope):  package="io.opentelemetry.http"  ecosystem=Maven (io.* prefix), cve_count=0 -> "clean"
    #   tier 3 (otel_scope):  package="custom-tracer"  ecosystem="" (no prefix match) -> "unknown_ecosystem"
    _insert(
        db,
        "otel_traces",
        [
            {
                "Timestamp": _TS,
                "ServiceName": "billing-api",
                "SpanName": "GET /charge",
                "ScopeName": "",
                "ScopeVersion": "",
                "ResourceAttributes": {
                    "telemetry.sdk.name": "opentelemetry",
                    "telemetry.sdk.version": "1.20.0",
                    "telemetry.sdk.language": "python",
                },
            },
            {
                "Timestamp": _TS,
                "ServiceName": "billing-api",
                "SpanName": "http.client",
                "ScopeName": "io.opentelemetry.http",
                "ScopeVersion": "2.5.0",
                "ResourceAttributes": {},
            },
            {
                "Timestamp": _TS,
                "ServiceName": "billing-api",
                "SpanName": "internal.work",
                "ScopeName": "custom-tracer",
                "ScopeVersion": "0.9.1",
                "ResourceAttributes": {},
            },
        ],
    )
    # One CVE finding keyed on (Package, Ecosystem, Version) of the SDK library so its
    # countDistinct(OsvId) join -> cve_count=1 -> status "vulnerable". Fixed scalars only.
    _insert(
        db,
        "sobs_cve_findings",
        [
            {
                "Package": "opentelemetry",
                "Ecosystem": "PyPI",
                "Version": "1.20.0",
                "ServiceName": "billing-api",
                "OsvId": "GHSA-libs-0001",
                "CveIds": "CVE-2024-9001",
                "Summary": "SDK advisory for opentelemetry 1.20.0",
                "Severity": "HIGH",
                "Published": "2024-01-02 00:00:00",
                "ScannedAt": _TS,
            }
        ],
    )
    db.execute("OPTIMIZE TABLE sobs_cve_findings FINAL")


# RUM asset download fixture (GET /v1/rum/assets/<asset_id>). The asset is a pair of files on
# disk under DATA_DIR/rum_assets: <id>.meta.json (the metadata Quart/Go re-read) + the stored
# blob. Both servers serve the SAME bytes from the SAME data dir (parity_check copies the fixture,
# including rum_assets/, into the Go server's _run dir). The id and content are FIXED, so the body
# is byte-identical; mtime-derived caching headers (Last-Modified / Werkzeug FS-ETag) are dropped
# by normalize.py, so the on-disk mtime does not affect parity.
RUM_ASSET_ID = "0123456789abcdef0123456789abcdef"  # 32-char lowercase hex (passes the id regex)
RUM_ASSET_STORAGE = RUM_ASSET_ID + ".txt"
RUM_ASSET_BODY = b"sobs parity rum asset body\n"
RUM_ASSET_CONTENT_TYPE = "text/plain"


def seed_rumasset(db) -> None:
    # db is unused (the asset lives on the filesystem, not chdb). Resolve the active data dir the
    # same way _boot_app_db set it, so both the default fixture build and the --data-dir _run copy
    # land their files in the correct rum_assets/ directory.
    import json

    data_dir = Path(os.environ.get("SOBS_DATA_DIR", str(FIXTURE_DIR)))
    asset_dir = data_dir / "rum_assets"
    asset_dir.mkdir(parents=True, exist_ok=True)
    (asset_dir / RUM_ASSET_STORAGE).write_bytes(RUM_ASSET_BODY)
    # Metadata mirrors ingest_rum_asset's json.dump (the download route only reads storage_name +
    # content_type; the rest is informational). Fixed scalars -> deterministic, but the download
    # response never echoes the metadata, so even non-read fields are harmless.
    meta = {
        "id": RUM_ASSET_ID,
        "type": "asset",
        "original_name": "parity-asset.txt",
        "storage_name": RUM_ASSET_STORAGE,
        "content_type": RUM_ASSET_CONTENT_TYPE,
        "size": len(RUM_ASSET_BODY),
        "uploaded_at": "2024-01-02T03:00:00+00:00",
    }
    (asset_dir / f"{RUM_ASSET_ID}.meta.json").write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


PROFILE_SEEDS = {
    "agentrun": seed_agent_run,
    "notif": seed_notif,
    "notifcheck": seed_notif,  # same rows; isolated so check doesn't see toggle/delete mutations
    "notifgen": seed_notif,  # channels+rules; auto-generate create inserts new rules (isolated)
    "agenttrigger": seed_agent_rule,  # analyze-only rule; trigger_agent_run runs the agent flow
    "dmprune": seed_dm_prune,  # retention-eligible rows -> prune's DELETE window runs on real data
    "notifagent": seed_notif_agent,  # tag rule + auto tags + agent rule -> check_notifications auto-triggers the flow
    "notifagentmiss": seed_notif_agent_miss,  # tag infra + continue-only agent rules -> agent_runs stays []
    "dmbackup": seed_dm_backup,
    "k8s": seed_k8s,  # backup_enabled=1; backup/run + restore reach their enabled branch
    "k8srich": seed_k8srich,  # OTEL-native k8s.* gauge rows -> _fetch_k8s_from_otel otel-branch populated
    "k8sprom": seed_k8sprom,  # prometheus (kube_*) gauge+sum rows -> _fetch_k8s_from_otel prometheus-branch
    "repoapp": seed_repo_app,  # registered app + release + github token; repositories-sub actions
    "cveosv": seed_cve_osv,  # telemetry.sdk row -> non-empty inventory -> OSV scan finds a vuln
    "tagauto": seed_tagauto,  # 30 recent prod-service logs -> auto_tag_rules in-window candidate
    "tagautorich": seed_tagautorich,  # rich telemetry -> every _build_auto_tag_rule_candidates arm
    "dashboardautorich": seed_dashboardautorich,  # anomaly rules -> _build_auto_dashboard_chart_candidates arms
    "cveview": seed_cveview,  # CVE findings (1/severity) -> summary cve-overview + view_enrichment_cve
    "summaryrich": seed_summaryrich,  # now()-anchored logs + logs-source anomaly rules -> populated summary panels
    "metricsauto": seed_metricsauto,  # constant log_volume series -> auto_metrics_rules candidates
    "metricsrich": seed_metricsrich,  # web log_volume SPIKE + seasonal rule -> view_metrics outlier/rule render
    "rumvitals": seed_rumvitals,  # now()-relative web-vital + error rows -> view_rum vitals + error-trend
    "tagsuggest": seed_tagsuggest,  # otel/tags/attr-key rows -> condition-suggestions non-empty branches
    "cvebackfill": seed_repo_app,  # app+release+github token -> cve github backfill attempts a release
    "depsrich": seed_depsrich,  # app+release(+commit)+token -> cve backfill walks the full deps-parse chain
    "onboard": seed_repo_app,  # app+token -> onboarding create-issues realtime + github-issue paths
    "issuesraise": seed_issues_raise,  # global github repo+token -> issues/raise agent flow creates an issue
    "issuereuse": seed_issues_reuse,  # prior work item + matching open issue -> issues/raise reuses it (dedup)
    "githubtoken": seed_github_token,
    "onboardrepos": seed_onboard_repos,  # global github token -> onboarding list/inspect token-USED branch
    "repohealth": seed_repohealth,  # apps+releases+github token -> populated repo-health /issues scan
    "mcpkey": seed_mcp_key,
    "mcpauth": seed_mcp_auth,  # api key whose hash auths tools/list + tools/call
    "aichat": seed_aichat,
    "aiexport2": seed_aiexport2,  # gen_ai span w/ non-JSON messages -> export_ai_training json.loads except
    "ciauth": seed_ci_key,  # registered app + managed per-app CI-push key; managed-key require_api_key path
    "tracedetail": seed_trace_detail,  # multi-span trace + logs -> populated trace_detail waterfall
    "tracedetailerr": seed_tracedetailerr,  # tracedetail trace + 1 ERROR log -> trace_detail errors loop
    "incidentmatch": seed_incidentmatch,  # rum_session hyperdx row + matching work-item link -> view_incident MATCH
    "logsview": seed_logsview,  # 5 otel_logs rows + record tags -> populated view_logs branches
    "logsrich": seed_logsrich,  # 2 otel_logs rows -> view_logs raw-SQL query-execution error branch (11402-11404)
    "errorsview": seed_errorsview,  # error events + a resolution -> populated view_errors branches
    "aiview": seed_aiview,  # otel_traces AI spans (gen_ai.*) -> populated view_ai branches
    "tracesrich": seed_tracesrich,  # single-span trace + 51 identical ERROR logs -> errors_truncated (15479-15480)
    "airich": seed_airich,  # AI span w/ non-numeric + inf token attrs -> _safe_attr_int branches (18757-18760)
    "airichsql": seed_airich,  # same AI span; ISOLATED process so the sql-error totals cache mutation can't leak
    "dashview": seed_dashview,  # dashboard + charts -> GET /dashboards/<id> view branch
    "chartedit": seed_chartedit,  # dashboard + 1 chart -> edit_chart/clone_chart mutation branches
    "regexlogs": seed_regex_logs,  # validate-regex sample probe: otel_logs.Body
    "regextraces": seed_regex_traces,  # validate-regex sample probe: otel_traces.SpanName
    "regexrum": seed_regex_rum,  # validate-regex sample probe: hyperdx_sessions.Body
    "regexerrors": seed_regex_errors,  # validate-regex sample probe: ERROR_SOURCES_SQL Body
    "regexmetrics": seed_metricsauto,  # constant log_volume series -> v_derived_signals_anomaly probe
    "metricscreate": seed_metricsauto,  # same constant series; isolated for auto_metrics_rules create
    "notifrule": seed_notif,  # channels+rules; isolated for create_notification_rule success insert
    "enrichlibs": seed_enrichlibs,  # otel_traces sdk/scope rows + 1 CVE -> populated library inventory
    "rumasset": seed_rumasset,  # on-disk rum asset (meta.json + blob) -> rum_asset_download FOUND branch
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
