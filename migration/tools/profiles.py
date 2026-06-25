"""Capture/replay PROFILES — env overlays that flip a feature gate so a config-gated
route can be exercised on its *enabled* branch.

The parity corpus is captured against the empty fixture, where AI/query/kubernetes are all
OFF, so every config-gated route returns its disabled-guard payload. That is a faithful
branch, but it never exercises the route's real work. A profile is a named set of environment
variables that BOTH the Python oracle and the Go server honor identically, so each side
reaches the same gate state and the *enabled* branch can be captured + diffed.

A manifest route opts into a profile with a ``profile: <name>`` field (default ``base``).
``capture_routes.py --profile <name>`` boots the Python app with that profile's env and
captures only its routes; ``parity_check.py`` boots a SEPARATE Go server per profile (the
gate flags are read once at boot) and replays each profile's routes against its own server.

Why this is correct rather than a hack: each route is still diffed byte-for-byte against a
golden captured from the frozen Python oracle — only the *config* under which both run
changes. The base pages stay AOFF (their real behavior); the config-gated API routes, whose
real behavior REQUIRES the gate on, are tested with the gate on. The two world-states never
mix because a route belongs to exactly one profile.

The ``ai`` profile is a pure env overlay (no DB seeding, no mock upstream): the query-page
gate is ``_query_page_enabled()`` = ``ai.endpoint_url`` AND ``ai.model`` set, and the
query-page introspection routes only *check* that gate before running chdb schema queries —
they never call the endpoint. So any non-empty endpoint/model value flips the gate.
"""

from __future__ import annotations

import json
from pathlib import Path

# Absolute path to the canned upstream (GitHub/OSV) response directory, shared by the Python
# determinism httpx shim and the Go upstream.go fixtures transport.
_UPSTREAM_DIR = str(Path(__file__).resolve().parents[2] / "migration" / "fixtures" / "upstream")

# Absolute path to the committed Source-Map fixture dir. Both the Python oracle
# (_sourcemap_lookup_for_file -> sourcemap.loads) and the Go server (source_map.go) read the
# SAME `.map` file from here via SOBS_SOURCE_MAP_DIR. An absolute path resolves identically
# from each process's CWD (Python capture in-process; the Go server boots with cwd=REPO), which
# is what keeps the console-stack remap byte-identical on both sides.
_SOURCEMAP_DIR = str(Path(__file__).resolve().parents[2] / "migration" / "fixtures" / "sourcemaps")

# app-registry boot seed (SOBS_APP_REGISTRY_SEED_JSON): drives _seed_app_release_registry_from_env
# (app.py 8935) / seedAppRegistry (Go), which run at startup when this env holds an app/release/
# artifact graph. Every entity carries an explicit id/slug + already-normalized values so the seed
# is uuid-free and the readback is deterministic; releasedAt/uploadedAt are pinned (DateTime64(3),
# JSON-overridable) so they are byte-compared. The apps' createdAt/updatedAt come from _now_iso()
# at boot (NOT overridable) so they drift per-process -> masked in the two app-surfacing routes.
# Interleaved malformed entries (non-dict / empty-name app, non-dict / empty-version release,
# non-dict / empty-type artifact, non-list releases/artifacts) exercise every skip branch of the
# seeder in BOTH ports; each is skipped identically, so the byte output stays exactly the two real
# apps. Distinct release/artifact timestamps make the ReleasedAt/UploadedAt DESC orderings stable.
_APP_REGISTRY_SEED = json.dumps(
    {
        "apps": [
            "not-a-dict-skipped",  # non-dict app -> skipped (8964-8965)
            {"name": "", "slug": "empty-name-skipped"},  # empty name -> skipped (8967-8968)
            {
                "id": "a1000000000000000000000000000001",
                "name": "Alpha Service",
                "slug": "alpha-service",
                "ownerTeam": "payments",
                "repoUrl": "https://github.com/acme/alpha",
                "defaultEnvironment": "production",
                "enabled": True,
                "metadata": {},
                "releases": [
                    "bad-release-skipped",  # non-dict release -> skipped (8998-8999)
                    {"commitSha": "no-version-skipped"},  # empty version -> skipped (9001-9002)
                    {
                        "id": "a2000000000000000000000000000001",
                        "version": "2.1.0",
                        "commitSha": "c0ffee01",
                        "buildId": "build-1001",
                        "environment": "production",
                        "releasedAt": "2024-01-03 03:00:00.000000",
                        "metadata": {},
                        "artifacts": [
                            "bad-artifact-skipped",  # non-dict artifact -> skipped (9035-9036)
                            {"name": "no-type-skipped"},  # empty artifactType -> skipped (9039-9040)
                            {
                                "id": "a3000000000000000000000000000001",
                                "artifactType": "container-image",
                                "name": "alpha-image",
                                "contentType": "application/vnd.oci.image.manifest.v1+json",
                                "size": 10485760,
                                "storageRef": "oci://registry/acme/alpha:2.1.0",
                                "checksumSha256": "1111111111111111111111111111111111111111111111111111111111111111",
                                "platform": "linux",
                                "architecture": "amd64",
                                "metadata": {},
                                "uploadedAt": "2024-01-03 03:10:00.000000",
                            },
                            {
                                "id": "a3000000000000000000000000000002",
                                "artifactType": "sbom",
                                "name": "alpha-sbom",
                                "contentType": "application/spdx+json",
                                "size": 4096,
                                "storageRef": "oci://registry/acme/alpha-sbom:2.1.0",
                                "checksumSha256": "2222222222222222222222222222222222222222222222222222222222222222",
                                "platform": "",
                                "architecture": "",
                                "metadata": {},
                                "uploadedAt": "2024-01-03 03:05:00.000000",
                            },
                        ],
                    },
                    {
                        "id": "a2000000000000000000000000000002",
                        "version": "2.0.0",
                        "commitSha": "c0ffee00",
                        "buildId": "build-1000",
                        "environment": "production",
                        "releasedAt": "2024-01-02 03:00:00.000000",
                        "metadata": {},
                        "artifacts": "not-a-list-skipped",  # artifacts not a list -> skipped (9032-9033)
                    },
                ],
            },
            {
                "id": "a1000000000000000000000000000002",
                "name": "Beta Worker",
                "slug": "beta-worker",
                "ownerTeam": "data",
                "repoUrl": "https://github.com/acme/beta",
                "defaultEnvironment": "staging",
                "enabled": True,
                "metadata": {},
                "releases": "not-a-list-skipped",  # releases not a list -> skipped (8995-8996)
            },
        ]
    }
)

# name -> {ENV_VAR: value}. "base" is the empty overlay (current corpus behavior, unchanged).
PROFILES: dict[str, dict[str, str]] = {
    "base": {},
    # validateerr: no env/seed — POST an invalid SQL filter so the validate-filter ERROR branch
    # (publicDashboardQueryError on the chdb probe exception) is byte-tested. A pure syntax error
    # ("1 1") errors at parse time, so it's column/table-agnostic (stable on logs AND ai). Locks in
    # differential-fuzz finding F1 as a permanent regression test.
    "validateerr": {},
    # formerr: no env/seed — POST invalid form bodies to mutation endpoints so their early
    # validation/error branches (flash + redirect, before any DB write) are byte-tested. These
    # branches are mostly already ported but uncovered; the profile confirms them and surfaces any
    # divergence (cf. fuzz finding F1). Deterministic: errors return before touching chdb.
    "formerr": {},
    # logsview: seed otel_logs (5 rows, distinct fixed timestamps) + record tags so view_logs renders
    # its POPULATED branches — the rows/record-id/snapshot block, the level/service/event/trace
    # filters, the raw-SQL-where path (incl. has_tag() translation), the regex(q) clauses, and the
    # batch-tag join. Deterministic: fixed seed timestamps + frozen now() make the stats-age block
    # stable; distinct timestamps make ORDER BY Timestamp DESC stable.
    "logsview": {},
    # logsrich: separate otel_logs seed (2 rows, distinct fixed timestamps) for view_logs' raw-SQL
    # query-execution ERROR branch (app.py 11402-11404). A route passes sql=<col that passes
    # _validate_user_sql_where but does not exist> -> chdb UNKNOWN_IDENTIFIER on the COUNT query ->
    # except -> error_msg = "SQL error: " + _public_dashboard_query_error(exc). Sanitized message is
    # byte-identical on both sides (same libchdb + same SQL); no now()/uuid content. Kept separate
    # from logsview so its 12 goldens don't shift.
    "logsrich": {},
    # tagmatch (M38): seed ONLY tag rules whose conditions span every operator (eq/contains/regex/
    # unknown) and field (service_name/severity/body/event_type/attribute/unknown) plus a composite
    # AND rule and a trace-only record-type-gated rule. The matcher (_match_single_condition /
    # _match_tag_rule) is INGEST-time, not render-time, so the route pair POSTs an OTLP log batch to
    # /v1/logs (running the matcher -> sobs_record_tags) then GETs /logs to render the applied tags.
    # Deterministic: every rule writes a DISTINCT tag_key (no last-wins ambiguity), all fields are
    # fixed constants, the POST timeUnixNano is the frozen epoch, and capture drains the async write
    # queue before the GET (mirroring Go's parity commit-before-ack). Isolated so the ingested rows +
    # rules never ripple into base/otlpingest/logsview readers. No env overlay — just the seed.
    "tagmatch": {},
    # errorsview: seed otel_logs error events (3 groups, counts 3/2/1, one TraceId each) +
    # one sobs_error_resolutions row so view_errors renders its POPULATED branches — the
    # non-grouped narrow+hydrate path, the grouped aggregate path, and the resolved=0/1/all
    # variants. Deterministic: distinct counts (tie-free grouped ORDER BY), one TraceId per group
    # (single-element groupUniqArray), distinct fixed timestamps (stable argMax + ORDER BY).
    "errorsview": {},
    # errorsummary: seed 12 otel_logs error rows whose exception.message / Body carry distinct
    # structured (JSON) and plain payloads so _build_error_item -> _extract_structured_error_summary
    # hits every branch (message+type+code extras, nested/list descent, type/code-only summaries,
    # substring-skip extras, the json.dumps(ensure_ascii=False) fallback incl. unicode, invalid-JSON
    # continue, non-{/[ prefix continue, raw_body-JSON success, plain fallback). No env overlay.
    # Deterministic: no time-window filter is applied (no from_ts/to_ts), distinct fixed timestamps
    # give a total ORDER BY Timestamp DESC and a unique hydrate dedup key per row; values constant.
    "errorsummary": {},
    # aiview: seed otel_traces AI spans (gen_ai.* SpanAttributes) so view_ai renders its POPULATED
    # branches — the ai_items build, the flat + trace view modes, the totals aggregation, and the
    # service/model/operation/span_name/row_type/sql filters. Deterministic: distinct timestamps,
    # distinct token/model values, empty message JSON (parsers return empty), no ties.
    "aiview": {},
    # airich: an isolated AI span whose token attrs are NON-NUMERIC ("abc") / INFINITE ("inf"), so
    # view_ai's _safe_attr_int defensive branches run — the ValueError path (app.py 18757-18758) and
    # the NaN/inf guard (18760), both yielding 0. No env overlay — just the (isolated) seed; base
    # aiview goldens stay untouched. (_safe_duration_ms's matching branches are unreachable: Duration
    # is a UInt64 column, so r["Duration"] is always a parseable, finite integer.)
    "airich": {},
    # aiturns: an isolated trace (aiturnstrace1) of 5 AI spans across 2 gen_ai.turn_ids, each carrying
    # a distinct gen_ai.input/output.messages JSON shape, captured via GET /ai?view=trace. Exercises
    # the full GenAI message-rendering cluster (_genai_message_content_to_text / _reasoning_to_text /
    # _extract_messages_text / _genai_tool_calls_to_text) AND _build_ai_trace_turn_cards' group/sort/
    # enumerate over >1 turn. No env overlay -- view_ai is a read-only telemetry view (no AI gate); the
    # isolated seed keeps the base aiview goldens untouched. Fixed 2023 ts -> no now()-window, no masks.
    "aiturns": {},
    # aitoolturns: residual _build_ai_trace_turn_cards arms (8545/8547/8549/8583/8585/8587/
    # 8589-8598/8602-8632) + _summarize_ai_tool_action (8486-8503). One trace (aitoolstrace1) with
    # 12 spans across 6 turns covering: deferred model/provider/chat_id fields; per-turn summary
    # attrs; guard.result; turn.blocked; turn.error; turn.cancelled; tool.proposed / tool.executed
    # with all five sobs.ai.tool.action shapes (sql_where / target_page / type-only / non-JSON /
    # JSON-non-dict / empty). No env overlay; isolated trace id; fixed 2023-08-01 timestamps.
    "aitoolturns": {},
    # airichsql: the SAME airich seed, but a SEPARATE profile so the raw-SQL exec-error route is
    # captured in its OWN process. view_ai's totals-error append mutates the (aliased) cached
    # _ai_filter_metadata "errors" list in the oracle, which would otherwise leak the "totals=..."
    # alert into a same-process plain-airich golden (Go does not alias the cache, so it would not
    # replicate the leak -> RED). Process isolation keeps both goldens correct on both sides.
    "airichsql": {},
    # tracesrich: an isolated single-span trace + 51 BYTE-IDENTICAL error logs, so view_traces'
    # trace-detail errors_truncated branch (app.py 15479-15480) runs: the trace-error query fetches
    # LIMIT 51, gets >50, flips errors_truncated and slices to 50. Identical rows make the
    # (loop.index-keyed) accordion render permutation-invariant despite the query's missing ORDER BY.
    "tracesrich": {},
    # tracemetrics: an isolated now()-anchored single-span trace carrying k8s identity attrs
    # (namespace/pod/node/deployment) PLUS matching now()-anchored otel_metrics_gauge rows, so
    # view_traces' trace-detail metric-context fetch (_fetch_trace_metric_context, app.py 15580)
    # hits its tier-1 "pod + namespace" success path. No env overlay -- just the (isolated) seed.
    "tracemetrics": {},
    # tracewindows: clones the tracemetrics now()-anchored single-span trace and ADDS two overlapping
    # sobs_raw_windows rows (one fully containing the trace, one interior) so view_traces' trace-detail
    # render runs _build_trace_window_overlay_segments (app.py 14787, called 15594) and emits two
    # overlay segments with deterministic RELATIVE left/width % + titles. No env overlay -- isolated seed.
    "tracewindows": {},
    # reportsimport: no seed (reuses the base fixture's 2 example reports for conflict tests). An
    # isolated profile because the success routes MUTATE sobs_reports — keeping them out of base so
    # they don't perturb other base routes that read reports. Routes run in manifest order and
    # accumulate mutations within the profile; the JSON response is pure counts (no uuid/timestamp),
    # so it's deterministic given a fixed order. Covers api_import_reports' envelope/item validation
    # + skip/replace/rename/insert branches.
    "reportsimport": {},
    # rulecreate: no seed (isolated copy of base). Exercises create_metrics_rule's threshold-ordering
    # / secondary-comparator / composite validation branches + the SUCCESS insert path (flash +
    # redirect, like the formerr routes but reaching the write). Isolated because success mutates
    # sobs_anomaly_rules; routes ordered so the (uuid-consuming) success inserts run last.
    "rulecreate": {},
    # rumingest: no seed, isolated (ingest_rum writes hyperdx_sessions + otel_logs). Exercises the
    # error-event indexing branch (page/artifact/replay attrs) and the list-payload + non-dict-event
    # branches. Response is {accepted: count} only (no uuid/timestamp) -> deterministic.
    "rumingest": {},
    # metricscreate: same seed as metricsauto (constant log_volume series) but ISOLATED so the
    # auto_metrics_rules action=create path (inserts sobs_anomaly_rules) doesn't perturb the
    # metricsauto view/preview routes. The threshold candidates are timestamp-independent (constant
    # series) so the created-count flash is deterministic; seasonal mode is skipped (wall-clock
    # hour-of-day buckets drift).
    "metricscreate": {},
    # notifrule: seed_notif (channels+rules) but ISOLATED so create_notification_rule's success
    # insert doesn't perturb the notif profile's own routes. Provides a valid channel id for the
    # channel-validation + insert path; response is flash+redirect (deterministic).
    "notifrule": {},
    # aisettings: no seed, isolated (save_ai_settings writes sobs_app_settings). Covers the
    # model_pricing / model_pricing_confirmed JSON validation (valid + invalid) and the
    # github-token-changed branch. Response is flash+redirect or 400 JSON (deterministic); the
    # Fernet-encrypted writes are nondeterministic but never appear in the response.
    "aisettings": {},
    # rumtoken: env overlay enabling origin-bound RUM client auth, so issue_rum_client_token reaches
    # its token-issuance path (otherwise the base route returns the disabled payload). iat/exp use the
    # frozen clock so expiresAt is deterministic; the token embeds a random jti uuid so its value is
    # masked. Both sides read the same SOBS_RUM_CLIENT_* env.
    "rumtoken": {
        "SOBS_RUM_CLIENT_AUTH_MODE": "origin",
        "SOBS_RUM_CLIENT_SIGNING_KEY": "parity-rum-signing-key",
    },
    # rummodebad: RUM client auth mode set to an unrecognized value, so issue_rum_client_token hits
    # the `mode not in ("origin", "origin-session")` arm -> 500 {"error":"Invalid SOBS_RUM_CLIENT_AUTH_MODE"}.
    # The invalid-mode check precedes the signing-key check, so no key is needed. Env-only overlay.
    "rummodebad": {
        "SOBS_RUM_CLIENT_AUTH_MODE": "bogus",
    },
    # rumnokey: a valid mode ("origin") but NO signing key (base/parity_env never sets one), so
    # issue_rum_client_token reaches the `if not RUM_CLIENT_SIGNING_KEY` arm -> 503
    # {"error":"RUM client signing key is not configured"}. Env-only overlay.
    "rumnokey": {
        "SOBS_RUM_CLIENT_AUTH_MODE": "origin",
    },
    # cveview: seed sobs_cve_findings (1 per severity, distinct Published) so the summary
    # cve-overview counts and view_enrichment_cve findings loop/filters render populated.
    # Read-only (the pages only query) -> shareable, but kept isolated/seeded for clarity.
    "cveview": {},
    # summaryrich: seed now()-anchored otel_logs (5 ERROR + 5 INFO in one minute bucket for the base
    # "web" service) + logs-source anomaly rules so the summary dashboard renders POPULATED:
    # recent_errors (_build_error_item full body), recent_logs, and signal_health (the annotate /
    # threshold / seasonal rule-evaluation chain). All counts/states/ratios are constant; only the
    # recent-errors/recent-logs timestamps drift (masked in the route). No env overlay. Isolated:
    # the now()-anchored rows + extra rules must not perturb other base summary readers.
    "summaryrich": {},
    # aiingest: no seed, isolated (ingest_ai writes otel_traces). A full JSON payload exercises the
    # input/output/system-instructions/prompt/response/error_type span-attr branches. Response is
    # {"ok": true} only (no uuid/count) -> deterministic; provide timestamp to avoid _now_iso drift.
    "aiingest": {},
    # dashauto: no seed, isolated (auto_metrics_rules_dashboard create inserts a dashboard + charts).
    # Candidates come from the base example anomaly rules (no extra seed). create's redirect Location
    # embeds the new dashboard_id (frozen uuid -> deterministic as the first uuid consumer); preview
    # + no-candidates branches don't mutate. Routes ordered so the create runs last.
    "dashauto": {},
    "ai": {
        # Python derives _query_page_enabled() from these (via _AI_ENV_OVERRIDES); Go reads
        # them through aiEnvOverrides AND gates the query page on SOBS_QUERY_PAGE_ENABLED.
        # The endpoint is never dialed by the gate-check routes, so the URL need not listen.
        "SOBS_AI_ENDPOINT_URL": "http://127.0.0.1:8788/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
    },
    "github": {
        # External GitHub/OSV routes: both sides read canned responses from this dir (no
        # network). determinism.install() activates the httpx shim when this is set.
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # queryrun: the query-page gate ON (same as `ai`) but its OWN fixture, so query_run's
    # telemetry emit (otel_logs/traces inserts) doesn't ripple into the ai-profile schema route
    # (whose attr-key context reads otel_logs).
    "queryrun": {
        "SOBS_AI_ENDPOINT_URL": "http://127.0.0.1:8788/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
    },
    # SEEDED-state profiles (no env overlay): rows inserted ONLY into their own fixture so a
    # found/mutate branch runs without rippling into base readers. `notif` decouples the
    # notification toggle/delete bundle (seed channels+rules → test toggle/delete, while base
    # check_notifications/auto-generate stay on their empty path).
    "agentrun": {},
    # notif also points at the upstream fixtures so the channel /test webhook POST is served
    # from a canned response (the toggle/delete routes make no HTTP calls, so it's a no-op there).
    "notif": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # notifydispatch (Milestone 25): cover the channel-DISPATCH cluster via the per-channel
    # POST /api/notifications/channels/<id>/test routes. Points at the upstream fixtures so the
    # webhook/slack/push outbound POSTs are served from canned 2xx responses (=> dispatch returns
    # "ok" => {"ok": true}). SOBS_VAPID_PRIVATE_KEY is a FIXED, structurally-valid P-256 PKCS8 DER
    # key (base64url) read identically by app.py _get_vapid_private_key_b64 and the Go
    # loadVapidPrivateKey, so the browser_push VAPID/JWT path runs without raising on both sides.
    "notifydispatch": {
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
        "SOBS_VAPID_PRIVATE_KEY": (
            "MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgESIzRFVmd4gRIjNEVWZ3iBEiM0RVZneI"
            "ESIzRFVmd4ihRANCAARB9kj5MzwMoMBeoRI3FUvOmnLUPtEVJ4BQ8uky1TkZfJ_J6vv4CHvqtkuPYZy"
            "Q7nV0dFTHvJYnBhfXvlda4orD"
        ),
    },
    # githubtoken = the github mock + a seeded ai.github_token, so the onboarding inspect/issue
    # routes reach (and exercise) their token-gated GitHub branch.
    "githubtoken": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # onboardrepos = the github mock + a seeded ai.github_token so the onboarding READ endpoints
    # take their token-USED branch: list-repos dials the ?type=all users endpoint (canned repo list
    # incl. private repos -> token_used=true, empty visibility_note) and inspect-repo runs the full
    # repo-inspection GitHub flow (workflows listing + ci.yml + copilot graphql, all canned).
    "onboardrepos": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # repohealth = the github mock + a seeded ai.github_token + three apps/releases so
    # GET /api/enrichment/github/repo-health takes its populated per-repo scan branch: each repo's
    # token-gated /issues?state=open call is served from a canned fixture (rich list, empty list,
    # and a missing fixture -> 404 skip), exercising the version-scope/issue-PR-security counting.
    "repohealth": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # createrepo: a no-env isolation profile for create_repo — its sobs_apps/ai-settings INSERTs
    # run in their own fixture copy so they don't ripple into the base registry/repository readers
    # (a profile pass = fresh fixture, and only this route runs in it).
    "createrepo": {},
    # mcpkey: a seeded mcp.api_keys descriptor so DELETE /api/mcp/keys/<id> can revoke it.
    "mcpkey": {},
    # mcpauth: a seeded mcp key (scrypt hash of "mcp-parity-token") so authenticated tools/list +
    # tools/call run. Isolated so the seeded key doesn't ripple into base mcp tests.
    "mcpauth": {},
    # aichat: a seeded gen_ai chat turn (otel_logs) so the chat-detail reader serializes it. The
    # otel_logs row is isolated to this profile so base telemetry readers stay empty.
    "aichat": {},
    # aiexport2: a seeded gen_ai otel_traces span whose input/output message attrs are NON-JSON
    # strings, so export_ai_training's two json.loads calls raise and take the prompt/response
    # fallback (app.py 19185-19187 / 19193-19195). Read-only export -> deterministic; isolated so
    # the malformed span doesn't ripple into other AI readers.
    "aiexport2": {},
    # tracedetail: a small multi-span trace (+ a few non-error logs) seeded into otel_traces /
    # otel_logs so /traces?trace_id=… builds the populated trace_detail waterfall (span tree,
    # active/gap timeline, log counts). No env overlay — just the (isolated) seed. The spans sit at
    # 2023-06-01 (outside the frozen 48h anomaly window) and no metrics/raw-windows are seeded, so
    # the anomaly / metric-context / window-overlay blocks take their deterministic empty paths.
    "tracedetail": {},
    # tracedetailerr: the SAME tracedetail trace plus ONE ERROR otel_logs row carrying the
    # trace's id, so view_traces' trace-detail errors loop (app.py 15481-15488) executes and
    # renders an error item. No env overlay — just the (isolated) seed; the base tracedetail
    # goldens are untouched.
    "tracedetailerr": {},
    # incidentmatch: a hyperdx_sessions row whose session key equals ?rum_session=incident-sess-001
    # PLUS a sobs_github_work_items row whose AnomalyRuleId is that same id (with a non-empty
    # IssueUrl), so view_incident's rum_session MATCH branch (primary_rum set -> service/event_ts)
    # AND the existing_work_item resolution (app.py 15904-15905/15918-15920/16113-16114) both run.
    # No env overlay — just the (isolated) seed; a unique ServiceName + fixed 2023 timestamps keep
    # every related/window/metric/anomaly block on its deterministic empty path.
    "incidentmatch": {},
    # dashview: a seeded dashboard (fixed id) + two charts so GET /dashboards/<id>
    # (view_custom_dashboard) renders its view branch against real data. No env overlay — just the
    # isolated seed. The base example seeder also creates a dashboard, but with a determinism-derived
    # id; this profile pins a known id for the manifest path. Isolated so the rows never ripple into
    # base dashboard readers.
    "dashview": {},
    # chartedit: a seeded dashboard (d0…d001) + one chart (c0…c001), both fixed-id, so the chart
    # MUTATION form routes (edit_chart / clone_chart) reach their real branches against an existing
    # chart inside an existing dashboard. No env overlay — just the isolated seed. Both handlers
    # redirect to the EXISTING dashboard_id, so even clone's server-generated chart uuid never
    # reaches the response — both success bodies are byte-stable. Isolated (mutating POSTs) so the
    # rows never ripple into base dashboard readers.
    "chartedit": {},
    # feedback: a no-env isolation profile — ai_helper_feedback's telemetry INSERT (otel_logs +
    # otel_traces) runs in its own fixture copy so it doesn't ripple into base telemetry readers.
    "feedback": {},
    # execute: ai_helper_execute decodes a signed action token + emits tool.executed telemetry;
    # isolation so that insert doesn't ripple into base telemetry readers.
    "execute": {},
    # notifcheck: seeded notification rules so check_notifications evaluates real (empty-condition,
    # non-firing) rules; isolated from the notif toggle/delete tests. Also exercises auto-generate
    # *preview* with enabled channels (channel pre-selection + covered-set build).
    "notifcheck": {},
    # notifeval (Milestone 27): seeded notification rules carrying REAL conditions + matching tag
    # data, so check_notifications runs the rule-EVALUATION cluster (_normalize_notification_condition,
    # _evaluate_tag_condition, _check_notification_rule) end to end — covering the fire / not-fired /
    # disabled / cooldown arms. The ONE firing rule dispatches to a webhook on hooks.example.com, so
    # SOBS_UPSTREAM_FIXTURES points at the canned-2xx sink (=> dispatch returns "ok"). AI stays
    # UNCONFIGURED so the automatic agent-trigger branch is a no-op (agent_runs == []), exactly like
    # the oracle on this fixture.
    "notifeval": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # notifgen: seeded channels+rules so auto-generate *create* runs its insert branch (derives a
    # notification rule per uncovered anomaly rule); isolated so the new rows don't ripple.
    "notifgen": {},
    # dmprune: retention-eligible (old) rows seeded into every data-management-managed table so
    # POST /api/data-management/prune with a custom period runs its real ALTER … DELETE window +
    # OPTIMIZE … FINAL pass against POPULATED tables (the empty base fixture deletes nothing). No
    # env overlay — just the (isolated) seed; the DELETE must never ripple into base readers.
    "dmprune": {},
    # dmbackup: data_management.backup_enabled=1 so backup/run + restore reach their enabled branch
    # (no S3 configured -> deterministic "S3 bucket is not configured" / "backup_name is required").
    "dmbackup": {},
    # dmsecret: SOBS_SETTINGS_ENCRYPTION_KEY set so the data-management secret-save path runs its
    # at-rest-encryption branch. Its POST stores s3_secret_access_key + backup_encryption_password
    # (Fernet-encrypted at rest), then a follow-up GET renders dm_secret_present=true with the
    # secret values masked to "". Both responses are deterministic — the random Fernet IV lives
    # only at rest and never surfaces, so this is byte-comparable while exercising the C6 fix.
    # Pure env overlay (no seed): the POST creates the rows the GET reads, in manifest order.
    "dmsecret": {"SOBS_SETTINGS_ENCRYPTION_KEY": "sobs-parity-dm-secret-key"},
    # k8s: Go boot flag on; Python reads the seeded kubernetes.enabled=1. No k8s metrics -> empty status.
    "k8s": {"SOBS_KUBERNETES_ENABLED": "1"},
    # k8srich: enabled + OTEL-native k8s.* gauge metrics seeded so _fetch_k8s_from_otel's "otel"
    # branch runs over POPULATED data (nodes/pods/deployments/namespaces lists + summary), and
    # ?name=/?namespace= routes hit the otel name/value filter branches. No now() window in the
    # function -> the max(TimeUnix) "created" string is constant, so no time mask is needed.
    "k8srich": {"SOBS_KUBERNETES_ENABLED": "1"},
    # k8sprom: enabled + PROMETHEUS (kube_*) gauge+sum metrics seeded so _fetch_k8s_from_otel's
    # "prometheus" branch runs (the big uncovered set), including the otel_metrics_sum restart-counter
    # UNION. Same no-mask reasoning as k8srich.
    "k8sprom": {"SOBS_KUBERNETES_ENABLED": "1"},
    # repoapp: a seeded registered app + release + github token so /settings/repositories/<id>/...
    # actions run their real branch; the github mock serves the token-validate /rate_limit call.
    "repoapp": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # appreg: env-only (NOT in SEEDED_PROFILES) — the app self-seeds the registry at boot from
    # SOBS_APP_REGISTRY_SEED_JSON, so _seed_app_release_registry_from_env (app.py 8935) runs in BOTH
    # the oracle (capture_routes.boot sets the env before `import app`) and the Go server (parity_check
    # passes profile_env to the boot). The /v1/apps + /v1/releases read routes then byte-verify the
    # seeded graph. Isolated populated state — other profiles keep the intentionally-empty registry.
    "appreg": {"SOBS_APP_REGISTRY_SEED_JSON": _APP_REGISTRY_SEED},
    # cveosv: a seeded telemetry.sdk library so the cve scan reaches its OSV branch; the OSV mock
    # serves the canned /v1/query vuln response. No github token (backfill stays the 0/0/cap no-op).
    "cveosv": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # cvescan (M41): a seeded TIER-3 scope library (one otel_traces row carrying ScopeName/
    # ScopeVersion/ServiceName) so _collect_library_inventory reaches its instrumentation-scope
    # branch (the io.opentelemetry.* -> "Maven" ecosystem path), distinct from cveosv's tier-2
    # telemetry.sdk.* and depsrich/lockfiles' tier-1 release-artifact paths. The cve scan then
    # queries OSV (canned /v1/query) for that one library and WRITES the parsed finding into
    # sobs_cve_findings. A FOLLOW-UP findings-read route runs in the SAME profile/process so the
    # OSV finding-parse FIELDS (cve_ids/severity/published/...) are byte-verified against the row
    # the scan itself wrote (not merely against directly-seeded rows like cveview). No github token
    # so the backfill stays the 0/0/cap no-op. scanned_at (POST) and last_scan (GET) are _now_iso()
    # wall-clock values -> masked; every other field is byte-compared.
    "cvescan": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # otlpingest: a no-env isolation profile so the OTLP /v1/{logs,traces,metrics} ingest INSERTs
    # (which would otherwise ripple into every otel reader) land in their own fixture copy.
    "otlpingest": {},
    # tagauto: 30 recent prod-service otel_logs rows so auto_tag_rules' in-window branch generates
    # one candidate. No env overlay — just the (isolated) seed. Timestamp-independent output.
    "tagauto": {},
    # tagautorich: rich now()-anchored telemetry (distinct services across logs/traces/sessions +
    # exception/provider attrs + a dup tag rule) so auto_tag_rules' PREVIEW exercises EVERY
    # _build_auto_tag_rule_candidates arm — log env + log non-env, trace env + trace non-env, error
    # append + error empty-type skip, ai append + ai empty-provider skip, rum append, and the
    # skipped_existing dedup arm — in one capture. No env overlay — just the (isolated) seed. The
    # candidate sort key is (point_count desc, name desc) and carries no timestamps, so the preview
    # body is byte-stable; the rendered services dropdown (_list_tag_candidate_services, an outer
    # SELECT DISTINCT ... ORDER BY ServiceName) is deterministically alphabetical. Isolated so the
    # extra services + dup rule never ripple into base/tagauto readers.
    "tagautorich": {},
    # dashboardautorich: seed sobs_anomaly_rules ONLY (no telemetry) so auto_metrics_rules_dashboard's
    # PREVIEW exercises _build_auto_dashboard_chart_candidates' remaining arms — empty source/signal
    # skip, service_filter mismatch skip, and the AttrFingerprint WHERE arm. The candidate builder
    # reads only sobs_anomaly_rules; seeding ZERO telemetry keeps _list_derived_signal_dimensions'
    # services dropdown equal to the base set (genuinely racy once >1 service is present), which is
    # the byte-stability safeguard. Preview only (the create path is deferred — uuid in redirect,
    # R12). No env overlay — just the (isolated) seed.
    "dashboardautorich": {},
    # metricsauto: a constant recent log_volume series so auto_metrics_rules' threshold scan
    # generates fixed candidates (exact quantiles of a constant). No env overlay — just the seed.
    "metricsauto": {},
    # seasonalauto (M32): same constant log_volume=5 series as metricsauto but confined to ONE
    # wall-clock hour (toStartOfHour(now()) + 0..30 MINUTE), so auto_metrics_rules' SEASONAL scan
    # (mode=seasonal) emits candidates whose seasonal_bucket_count is ALWAYS 1 (every minute bucket
    # shares one toHour()) regardless of the real hour. Exact quantiles of the constant series give
    # fixed thresholds; the action=preview render omits the wall-clock bucket KEY (only the count is
    # shown), so the page is byte-reproducible. Isolated so the single-hour layout never ripples into
    # base/metricsauto readers. No env overlay — just the seed.
    "seasonalauto": {},
    # metricsrich: a web log_volume series that SPIKES on its latest bucket (29x5 + 1x40) so
    # v_derived_signals_anomaly's latest bucket is anomaly_state='outlier' (anomaly_score 5.3852),
    # PLUS a seasonal anomaly rule matching (logs, log_volume, web) whose all-24-hour buckets fire on
    # value 40. view_metrics then renders the OUTLIER anomaly-state badge, the populated RULE badge,
    # and runs _evaluate_seasonal_rule's bucket-match path (it passes time_key="last_time"). All
    # derived quantities are timestamp-independent (fixed window over fixed-offset buckets); only the
    # now()-anchored last_time minute bucket drifts and is masked. Isolated so the spike never ripples
    # into base/metricsauto readers. No env overlay.
    "metricsrich": {},
    # rumvitals: now()-relative web-vital + error rows in hyperdx_sessions so view_rum's Web-vitals
    # (anomaly summary + 1m sparkline + hotspot) and Error-trend (direction + by_type + sparkline)
    # blocks all populate. Constant values -> every derived quantity is timestamp-independent; only
    # the now()-derived bucket/last_seen timestamps drift, masked in the route. No env overlay.
    "rumvitals": {},
    # webtraffic: hyperdx_sessions rows carrying client.ip (from the geoip parity corpus) so
    # /api/web-traffic/geo runs the local geoip2fast lookup (_get_geo_db + _geo_lookup_batch) and
    # returns deterministic country totals. No env overlay; no now() (no time-window args).
    "webtraffic": {},
    # tagsuggest: seeded otel_logs/otel_traces/hyperdx_sessions + sobs_record_tags + sobs_log_attr_keys
    # so /api/settings/tags/condition-suggestions returns non-empty ranked suggestions for every
    # scope/target/field branch. No env overlay — just the (isolated) seed. All seed rows use the
    # fixed determinism-window timestamp and the builders' ORDER BY ... , <value> tiebreaks make the
    # ranked output fully deterministic.
    "tagsuggest": {},
    # onboard: a seeded app+token so onboarding create-issues runs its realtime path (rotate CI key)
    # and its github-issue path (open-issue search 404s -> empty -> create via the canned POST).
    "onboard": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # onbupdate (M37): clone of onboard but the seeded app points at a DISTINCT repo
    # (acme/onboard-existing) so onboarding create-issues takes the UPDATE branch of
    # _create_or_update_onboarding_issue. The canned GitHub fixtures drive it: the open-issues GET
    # returns BOTH onboarding issues already open (titles matching the CI + OTEL issue text, #77/#78);
    # each issue-detail GET reports the issue still in new state (open, comments=0, created_at ==
    # updated_at) so _github_issue_is_new_state -> True; the matching PATCH /issues/<n> returns the
    # updated issue -> _update_github_issue_record yields status="updated". Distinct repo so its
    # open-issues fixture never ripples into onboard (acme/widget must keep 404-ing to CREATE).
    "onbupdate": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # cvebackfill: a seeded app+release+github token so the cve scan's github backfill attempts a
    # release (the repo has no fetchable lockfile -> every contents GET 404s -> attempted=1,
    # inserted=0). The github mock dir must be set so the fetches 404 rather than erroring.
    "cvebackfill": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # depsrich: like cvebackfill but the seeded release carries a CommitSha, so the cve scan's github
    # backfill walks the FULL dependency-parse chain. Canned fixtures drive it: the actions/runs +
    # artifacts traversal (no matching snapshot -> empty -> contents fallback), then a base64
    # package-lock.json that _parse_package_lock_dependencies parses into 3 npm deps -> one
    # dependencies-lockfile artifact inserted -> 3 libs -> OSV (canned) scan. attempted=1, inserted=1.
    "depsrich": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # lockfiles (M30): like depsrich but FOUR apps/releases so the cve backfill reaches all four
    # lockfile parsers (requirements / package-lock LEGACY-dependencies / go.sum / Gemfile.lock).
    # Canned per-repo contents fixtures stop the contents loop on a distinct parser per repo;
    # missing-fixture->404 advances earlier candidates. attempted=4, inserted=4,
    # libraries_found=13, vulns_found=13 (canned OSV -> 1 vuln per lib).
    "lockfiles": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # cveactions: like depsrich but the canned actions/artifacts fixture DOES contain the
    # sobs-release-dependency-snapshots artifact, so _github_actions_dependency_rows downloads
    # the zip (bytes_b64 fixture), parses pip-freeze-linux-x86_64.txt -> requests+flask ->
    # one dependencies-lockfile row inserted -> 2 libs -> OSV (canned) -> 2 vulns_found.
    # Uses a distinct CommitSha (aabbccdd1122) from depsrich so fixture keys are disjoint.
    "cveactions": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # refine: query/refine-chart — query gate on + the LLM endpoint pointed at the canned
    # /chat/completions mock (distinct path so its URL key is unique to this route).
    "refine": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/refine/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # refinerepair: query/refine-chart REPAIR path — the canned mock (distinct endpoint so its URL
    # key is unique) returns "not valid json" for BOTH the refine call and the repair call.
    # _parse_chart_spec_json fails -> _repair_chart_spec_json_with_llm is called -> repair mock also
    # returns "not valid json" -> parse fails again -> returns a compound error message. Exercises
    # app.py _vanna_refine_chart_spec lines 30072-30080. URL key: ec583354cd26fab1bd57381ba51fe9be.
    "refinerepair": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/refinerepair/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # agenttrigger: trigger_agent_run runs the full agent flow (guard + analyze LLM call) for a
    # seeded analyze-only rule. Guard + analyze endpoints on DISTINCT mock paths (two canned
    # responses); the runs it inserts are isolated so the agent-runs list test stays stable.
    "agenttrigger": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # agentflow (M29): trigger_agent_run runs the FULL agent flow (guard -> analyze LLM -> github
    # issue CREATE) for a seeded analyze+github_issue rule. Same agent + guard mock paths and the
    # same acme/widget github repo+token as issuesraise, so the canned guard/analyze/create fixtures
    # are reused; the open-issues GET 404s -> no dedup candidate -> a fresh issue (#42) is created.
    # Only run_id (uuid4) is volatile in the response and is masked.
    "agentflow": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # agentctx (M40): clone of agentflow's env (same agent + guard mock paths + acme/widget repo+token
    # + canned guard/analyze/create fixtures). The ONLY difference is the seed adds 12 ERROR otel_logs
    # rows and the POST body passes a rich extra_context, so _build_agent_context_summary's dark
    # branches (additional_context line, service+err_type event-frequency/noise block, "Trigger
    # details" remaining-keys block) execute during capture. The context summary feeds only the
    # mock-ignored LLM prompt + GitHub issue body, so the byte-compared response is identical to
    # agentflow's; run_id (uuid4) is the only volatile field and is masked.
    "agentctx": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # agentcopilot (M31): clone of agentflow but the seeded rule's Actions includes
    # github_issue_copilot, so after the create-new-issue arm the flow probes Copilot support
    # (GraphQL api.github.com/graphql -> suggestedActors has copilot-swe-agent) and POSTs the
    # assignee (/repos/acme/widget/issues/42/assignees -> assignees include copilot-swe-agent[bot])
    # via _assign_issue_to_copilot, which returns ("requested", "Copilot assignment requested").
    # Same agent/guard mock paths + acme/widget repo+token as agentflow; reuses its guard/analyze/
    # create/graphql fixtures plus one NEW assignees fixture. No prior work items => both Copilot
    # rate limiters are 0 < default 1, so the assignment deterministically proceeds. The response's
    # copilot_assignment_status/reason are byte-compared; requested_at is DB-only. run_id is masked.
    "agentcopilot": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # notifagent: check_notifications' AUTOMATIC agent branch. AI configured (same agent + guard mock
    # paths as agenttrigger) + a seeded tag rule, recent auto tags, and an analyze-only agent rule
    # whose tag-rule trigger fires from those tags. POST /api/notifications/check then runs the agent
    # flow (guard -> analyze -> completed run) and returns it under agent_runs. No notification rules
    # seeded, so the rule loop stays empty and the (masked) run_id uuid sequence matches.
    "notifagent": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # notifagentmiss: same AI gate (so the agent-trigger branch runs) but the seeded agent rules ALL
    # hit a `continue` arm (disabled / anomaly_rule+ref-no-event / tag_rule-no-ref-state-mismatch),
    # so NO _run_agent_rule_instance fires -> agent_runs stays []. No upstream mock is dialed (no run
    # makes an LLM call), but the env is the same shape as notifagent for symmetry.
    "notifagentmiss": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # anomalycheck: AI configured (same mock endpoints as notifagent) so evaluateAgentRuleTriggers
    # runs. The seed inserts a log-volume spike for "anomaly-prod" so v_derived_signals_anomaly
    # produces an outlier row; an anomaly rule + agent rule make the event flow to the rate-limit
    # check; a seeded sobs_agent_runs row (1 s before FIXED_EPOCH) makes elapsed_minutes=0.02 ->
    # skipped_rate_limited (no uuid/now() in response body -- fully deterministic, no mask needed).
    "anomalycheck": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # issuesraise: raise_issue_from_user_observation runs the agent flow with a github_issue +
    # dlp_check action. AI endpoints = the agent mock (guard + analyze canned); a seeded global
    # github repo+token + the canned POST /issues let the flow create a fresh issue (search 404s
    # -> new_issue). SOBS_AI_DLP_ENDPOINT_URL points at a canned DLP responder (a CLEAN verdict)
    # so the dlp_check sub-branch runs on both sides and screens the issue text before creation
    # without altering the route bytes (the issue is still created; dlp_result lives in the run row).
    "issuesraise": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_DLP_ENDPOINT_URL": "http://sobs-ai.mock/dlp/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # issuereuse: same agent mock + github mock as issuesraise, but the fixture is seeded with a
    # prior work item AND the github mock returns a matching OPEN issue, so the agent flow's dedup
    # subsystem reuses the existing issue (dedup-key fallback -> "same") instead of creating a new
    # one. A second seeded work item sits at the active-Copilot limit so the reuse-path Copilot
    # assignment is deterministically blocked (no assign HTTP call). Uses a DISTINCT repo
    # (acme/reuse-demo) so its open-issues fixture never ripples into issuesraise (acme/widget, whose
    # open-issues lookup must keep 404-ing so it still creates a fresh issue).
    "issuereuse": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # issuereuse2: like issuereuse but covers three additional branches of _choose_github_issue_outcome:
    #   (1) local work item W0 has an EMPTY IssueUrl -> continue (line 5415)
    #   (2) open-issues fixture returns a SECOND issue (#100) that has no local candidate -> appended
    #       via the open-issues-only loop (line 5442)
    #   (3) open-issues fixture for #41 includes "copilot-swe-agent[bot]" assignee -> assignment_status
    #       overridden to "active" (line 5484); with assign_copilot=true and active status the reuse-path
    #       copilot block fires (lines 5529-5530, "issue is already being worked by Copilot").
    # Uses a DISTINCT repo (acme/reuse2) so its open-issues fixture (fcf0aee5…) never ripples elsewhere.
    "issuereuse2": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # issuecreateerr: like issuesraise but the GitHub POST /issues fixture returns 422 (Validation
    # Failed), so _create_github_issue_record's HTTPStatusError handler fires (lines 6421-6423),
    # returning {"error": "GitHub issue creation failed: Validation Failed"}. The outer
    # raise_issue_from_user_observation handler then returns 502 (create_failed path).
    # Uses a DISTINCT repo (acme/issue-err) whose POST fixture (7fd2a848…) returns 422 and whose
    # open-issues lookup has no fixture -> 404 -> [] so no candidates exist.
    "issuecreateerr": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/agent/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/agent-guard/v1",
        "SOBS_AI_DLP_ENDPOINT_URL": "http://sobs-ai.mock/dlp/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # onbupdateerr: like onbupdate but the GitHub PATCH fixture for issue #5 in acme/onboard-err
    # returns 422, so _update_github_issue_record's HTTPStatusError handler fires (lines 33033-33038),
    # returning {"error": "GitHub issue update failed: Validation Failed"}. The outer
    # api_onboarding_create_issues handler surfaces it as ci_issue: {error: "..."} with ok: true.
    # Uses a DISTINCT repo (acme/onboard-err) so the failing PATCH fixture never ripples into onbupdate.
    "onbupdateerr": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # workitems: the github mock + a seeded ai.github_token + 3 stale work items so the SYNCHRONOUS
    # backfill awaited inside GET /api/work-items (not the background page caller) refreshes each row
    # from the canned issue-GET / PR-search fixtures and returns the updated items. Only the upstream
    # fixtures dir is needed (no AI endpoints — the chain calls GitHub only).
    "workitems": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # aibuild: dashboards/spec/ai-build — the vanna pipeline (generate SQL -> execute -> named
    # queries -> chart option -> chart repair). No guard check. BODY-KEYED (strict prompt parity):
    # the FOUR distinct LLM calls each resolve a body-keyed fixture (all return "SELECT 1 AS x", so
    # generate yields valid SQL, named-queries parses to none, chart-spec fails -> repair, repair
    # fails -> safe template fallback). The URL-keyed fallback was removed, so any drift in ANY of the
    # four prompt builders (NL->SQL, named-queries, chart-spec, chart-repair) -> body-key miss -> 404
    # -> RED. Keys: 19f939a2(SQL) 2108ee30(named) e733443a(chart) 9283972d(repair).
    "aibuild": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aibuild/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aibuildspec: dashboards/spec/ai-build CHART-SPEC SUCCESS path. SQL call returns "SELECT 1 AS x"
    # (URL-keyed fallback 20fd0c0d), named-queries also returns "SELECT 1 AS x" (URL-keyed) which is
    # not valid JSON so named_queries=[]. Chart spec call uses a BODY-KEYED fixture returning valid
    # ECharts JSON with {{labels}} and {{values}} placeholders -> _vanna_generate_chart_spec success
    # path (app.py 29867-29870) + _infer_custom_mapping_from_option (29900-29927). Body-keyed chart
    # fixture computed after Docker bodydump capture. URL-keyed fallback: 20fd0c0d5719059a4d312b1fa9335abd.
    "aibuildspec": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aibuildspec/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aihelper: /api/ai/helper (non-streaming) — guard + main /chat/completions on DISTINCT mock
    # paths. The main endpoint returns a CANNED SSE stream (the mock's `content` field) that both
    # _stream_llm_endpoint (Python) and streamLLMEndpoint (Go) parse identically; the guard returns
    # a plain JSON "safe" reply. A plain (no-tool, no-memory) answer keeps the turn deterministic.
    "aihelper": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aihelper/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aihelper-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # ciauth: a seeded registered app + release carrying a MANAGED per-app CI-push key (the scrypt
    # hash of "ci-parity-token", stored exactly as a Settings->Repositories key rotation would). The
    # static SOBS_API_KEY stays unset, so this profile exercises the managed-key path of
    # require_api_key: the right X-API-Key is accepted (200), and a missing/wrong key is rejected
    # (401 Unauthorized). No env overlay — just the (isolated) seed.
    "ciauth": {},
    # aihelpermem: like `aihelper`, but the canned answer carries an <assistant_meta> block with a
    # memory_candidate, so the route exercises the memory-consolidation WRITE path (load related →
    # consolidate → soft-delete drops → _upsert_ai_memory). With a fresh fixture the chat has no
    # prior memories, so consolidation finds nothing related; the consolidation LLM call collides
    # with the main endpoint's SSE fixture (not valid JSON), so both sides take the keep_new
    # fallback and persist the candidate verbatim. saved_memory_ids carries a fresh memory uuid
    # (frozen-counter Python vs random Go) → masked in the manifest; everything else is byte-equal.
    "aihelpermem": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aihelpermem/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aihelpermem-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aihelpermemcons: like `aihelpermem`, but seeds a prior sobs_ai_memories row for the same
    # chat so _semantic_memory_matches returns a non-empty `related` list (covering the
    # _consolidate_memory_candidates loop body, lines 3851-3854). A body-keyed fixture for the
    # consolidation POST (key f3bbd675…) returns valid JSON {"action": "merge", "memory": "...",
    # "drop_ids": ["mem-prior-cons-01"]}, driving the successful-parse path (lines 3883-3900).
    # The outer turn behaviour is identical to aihelpermem (same SSE fixture URL-keyed at
    # 7cb42069…); saved_memory_ids is masked as always.
    "aihelpermemcons": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aihelpermemcons/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aihelpermemcons-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aitools: like `aihelper`, but the canned MAIN /chat/completions SSE emits a `tool_calls`
    # delta (function propose_ui_action) carrying a logs.filter.apply_sql action_id that EXISTS in
    # the /logs page action manifest. This drives the previously-dark tool branch of the (non-
    # streaming) ai_helper turn loop: _stream_llm_endpoint accumulates the tool-call delta,
    # _normalize_generic_ui_action_tool_call validates it against the manifest + runs the
    # apply_sql_filter sql_where extraction + _build_client_action sanitization, the loop mints a
    # signed action_token (deterministic under SOBS_SECRET_KEY + frozen clock), appends it to
    # tool_proposals, and — because logs.filter.apply_sql is on-page (requires_confirmation False) —
    # the URL-keyed mock returns the SAME tool-call SSE every round, so the loop runs to its
    # max_tool_rounds cap (4 identical proposals) before producing the final answer. No memory
    # candidates (empty memory_candidates) so saved_memory_ids stays [] — nothing to mask; the
    # whole turn (answer/summary/model_stats/guard_stats + the 4 normalized tool_proposals with
    # their tokens) is byte-compared identically on both sides.
    "aitools": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitools/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitools-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aitoolsrich: like `aitools` but with a model name containing "tool" so _model_supports_tools
    # returns True -> tools is non-empty -> _stream_llm_endpoint (app.py 4793-4795) executes
    # `payload["tools"] = tools; payload["tool_choice"] = "auto"` (previously dark lines). The SSE
    # fixture (6da220bd…) prepends three malformed lines before the real events to cover:
    #   "event: ping" -> non-"data:" line -> line 4815 continue
    #   "data:" -> empty data after strip -> line 4818 continue
    #   "data: {invalid}" -> json.JSONDecodeError -> lines 4823-4824 continue
    # The real events (content delta + tool_calls + finish_reason) are identical to aitools, so the
    # tool_proposals and turn_summary are the same. The done event carries
    # "model":"sobs-parity-tool-model" (instead of "sobs-parity-model") — byte-compared on both sides.
    # Guard fixture 3e967d2a… (POST aitoolsrich-guard/v1/chat/completions): same "safe" SAFE reply.
    # URL-keyed (mock ignores body) — no body-key needed since the response is the same regardless.
    "aitoolsrich": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsrich/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsrich-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-tool-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # dmttl: data-management save with apply_ttl=1. No env / no seed — the OTel tables already
    # exist in the base fixture, so the day-based ALTER … MODIFY TTL runs for real; isolated so
    # those schema mutations (and TTL materialization) never ripple into base otel readers.
    "dmttl": {},
    # regex{logs,traces,rum,errors,metrics}: validate-regex sample-probe profiles. Each seeds ONE
    # in-window row (now()-1h, real wall-clock — like tagauto) whose sample column holds a FIXED
    # token, so a matching pattern returns that exact, timestamp-independent string. No env overlay;
    # isolated so the seeded telemetry never ripples into base readers. regexmetrics reuses the
    # metricsauto constant-series seed so v_derived_signals_anomaly yields a stable 'log_volume'.
    "regexlogs": {},
    "regextraces": {},
    "regexrum": {},
    "regexerrors": {},
    "regexmetrics": {},
    # enrichlibs: seed otel_traces (one telemetry.sdk row + two ScopeName/ScopeVersion rows) plus a
    # single matching sobs_cve_findings row so api_enrichment_libraries renders its POPULATED branch
    # — three libraries spanning all three statuses (vulnerable / clean / unknown_ecosystem). The
    # handler re-sorts by (cve_count desc, source order, package, version, service), so the output
    # order is fully determined by the distinct fixed keys. scanned_at stays "" (cve_last_scan unset).
    # No now()/uuid in the response -> no mask. No env overlay — just the (isolated) seed.
    "enrichlibs": {},
    # rumasset: an on-disk RUM asset (DATA_DIR/rum_assets/<id>.meta.json + the stored blob) so
    # rum_asset_download serves its FOUND/download branch (the existing routes only cover 400/404).
    # The id and content are FIXED; the response is the asset bytes with a stored Content-Type. The
    # only volatile headers (Last-Modified / Werkzeug FS-ETag) are dropped by normalize.py, so the
    # comparison is deterministic. No env overlay — the asset lives on the filesystem, not chdb.
    "rumasset": {},
    # rumassetupload: SOBS_RUM_ASSET_SIGNING_KEY set to a fixed parity key so _verify_rum_asset_signature
    # (app.py 7706-7745) reaches its signing/validation branches and ingest_rum_asset (9699-9756) hits
    # its success path. A pre-computed HMAC-SHA256 signature (body=b"sobs parity rum asset body\n",
    # timestamp=FIXED_EPOCH=1704164645, method=POST, path=/v1/rum/assets, content-type=text/plain,
    # type=asset, name=parity-asset.txt) drives the success route. Additional cases in the same profile
    # exercise the missing-headers, invalid-timestamp, expired-timestamp, and wrong-signature 401 arms.
    # The uploaded asset_id (uuid4.hex) and url differ between Python (frozen counter) and Go (random),
    # so both fields are masked in the manifest. No seed needed — the upload itself creates the file.
    # Isolated so the written rum_asset files never ripple into base/rumasset readers.
    "rumassetupload": {
        "SOBS_RUM_ASSET_SIGNING_KEY": "sobs-parity-rum-asset-signing-key",
    },
    # ask: query/ask — guard + main endpoints on DISTINCT mock paths (two canned responses).
    # BODY-KEYED (strict prompt parity): the two ask fixtures are keyed by the sha256 of the EXACT
    # LLM request body (upstream_fixture_key_body), NOT the URL — and the URL-keyed fallbacks were
    # removed. So the canned guard/SQL responses resolve ONLY when Python and Go send byte-identical
    # request bodies. If either runtime's prompt drifts (system prompt, schema context, observed
    # attr-key line, max_tokens, …), its body-key changes, no fixture matches, the mock 404s, and the
    # route diffs RED. Fixture files: 57da0797…(guard "SAFE") + 84464a88…(SQL "SELECT 1 AS x"). To
    # regenerate after an intentional prompt change: SOBS_MOCK_BODYDUMP=<f> + capture, read the keys.
    "ask": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/ask/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/ask-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguard: query/ask GUARD-BLOCK path with a gpt-oss-safeguard guard model. The guard model name
    # routes _check_guard_model through _is_gpt_oss_safeguard_model -> _build_oss_safeguard_prompt +
    # _parse_oss_safeguard_reply. The canned guard reply is a JSON verdict ({"violation": 1,
    # "policy_category": "S7", ...}) so the parser yields UNSAFE + S7. S7 (Privacy) is NOT a noisy
    # category, so none of the benign-override arms apply -> _check_guard_model returns
    # "blocked (S7: Privacy)" and api_query_ask returns 403 with that reason in `error`. The main
    # ai.endpoint_url is set only to satisfy _query_page_enabled; the block path never dials it. The
    # whole non-streaming LLM chain (_call_llm_endpoint POST -> response parse -> verdict) runs.
    # BODY-KEYED guard fixture 1a484efd… (see the `ask` note): the gpt-oss-safeguard prompt builder is
    # byte-verified — drift in _build_oss_safeguard_prompt changes the body-key -> 404 -> RED.
    "aiguard": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguard/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguard-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardllama: same query/ask GUARD-BLOCK path, but a llama-guard model name routes the OTHER
    # way -> _build_llama_guard_prompt + _parse_guard_reply (the two-line "unsafe\nSx" parser). The
    # canned reply "unsafe\nS9" yields UNSAFE + S9 (Indiscriminate Weapons, NOT noisy) ->
    # "blocked (S9: Indiscriminate Weapons)" -> 403. Covers the llama parser arm + prompt builder.
    # BODY-KEYED guard fixture 3237d47b… (see the `ask` note): the llama-guard prompt builder is
    # byte-verified — drift in _build_llama_guard_prompt changes the body-key -> 404 -> RED.
    "aiguardllama": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardllama/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardllama-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "llama-guard-3-8b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # vannarepair (M39): query/ask SQL REPAIR-TO-FAILURE path. DISTINCT mock endpoints (so the canned
    # INVALID-SQL response does not clobber the `ask` profile's valid-SQL fixture). The guard model is
    # the default `sobs-guard-model` -> _check_guard_model parses the canned "SAFE" reply -> ALLOW, so
    # the flow proceeds into _vanna_generate_sql. The body-ignored URL-keyed mock returns the SAME
    # canned response for BOTH generate AND repair calls, so:
    #   generate -> "SELECT nonexistent_column_xyz FROM otel_logs" (passes validate_sql: read-only +
    #     otel_logs is allowlisted, but the column does not exist) -> EXPLAIN fails (chdb
    #     UNKNOWN_IDENTIFIER) -> _auto_repair_incomplete_cte_sql no-ops (not a CTE) -> _vanna_repair_sql
    #     returns the SAME invalid SQL (mock ignores the repair prompt) -> the bounded for-loop runs
    #     all max_attempts=3 times, each EXPLAIN/run failing identically, incrementing retry_count to 3,
    #     then gives up -> final_error = the run_query error. This exercises _vanna_repair_sql (call +
    #     SQL extraction + stats) and the give-up arm of _vanna_validate_and_execute_with_repair.
    # Env-only (no DB seed): the base schema already creates otel_logs, which is all the EXPLAIN needs.
    "vannarepair": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/vannarepair/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/vannarepair-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # askrepair: query/ask AUTO-REPAIR-SUCCESS path. The guard returns SAFE -> flow runs
    # _vanna_generate_sql, which returns a TRUNCATED CTE SQL (the LLM output was cut off):
    #   WITH t AS (SELECT count() AS cnt FROM otel_logs WHERE ServiceName IN ('svc-1','svc-
    # EXPLAIN fails (unbalanced parens / truncated literal). _auto_repair_incomplete_cte_sql fires:
    #   _repair_truncated_in_clause_literals keeps 'svc-1' (even quote count) and drops 'svc-
    #   (odd), appends ")" to balance the single open paren, then appends "\nSELECT * FROM t".
    # The repaired SQL passes EXPLAIN and executes successfully (cnt=0 on the empty fixture).
    # retry_count=1 (one auto-repair iteration) is byte-compared in the response. DISTINCT mock
    # endpoint (askrepair/v1) so the truncated-SQL fixture does not clobber the `ask` profile's
    # valid-SQL fixture. URL-keyed (body-ignored) fixtures suffice because the URL is distinct per
    # profile; the body-key approach is not needed here. No DB seed required.
    "askrepair": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/askrepair/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/askrepair-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # rumauthmap: a PURE env overlay (no DB seed) that enables BOTH RUM client-auth verification
    # AND JS source-map console-stack remap on POST /v1/rum (ingest_rum). The signing key flips
    # _verify_rum_client_auth on (mode "origin" + fixed HMAC key, same key as `rumtoken`); the
    # source-map dir/enable flip _sourcemap_lookup_for_file / _maybe_demangle_js_stack on. Manifest
    # tokens are pre-signed offline with this key; the success token's exp=4102444800 (year 2100) so
    # the `exp <= now()` check passes wall-clock-independently. Both runtimes read the IDENTICAL key
    # + the same committed `.map`, so the accept/reject decision (the {"accepted": N} response) is
    # byte-identical.
    "rumauthmap": {
        "SOBS_RUM_CLIENT_AUTH_MODE": "origin",
        "SOBS_RUM_CLIENT_SIGNING_KEY": "parity-rum-signing-key",
        "SOBS_SOURCE_MAP_ENABLE": "1",
        "SOBS_SOURCE_MAP_DIR": _SOURCEMAP_DIR,
    },
    # ---- require_basic_auth decorator branches (R34) ----------------------------------------
    # The auth gate (app.py require_basic_auth / _auth_mode) is exercised only on its "none" arm by
    # the base corpus (no auth env set). These PURE env overlays (no DB seed) flip _auth_mode() to
    # each of its other configured states so the gate's branches become byte-comparable. They target
    # the SAME deterministic decorated route the base corpus already proves GREEN
    # (GET /api/web-traffic/browsers -> {"ok": true, "browsers": []} on the empty seed) for the
    # PASS cases, and any decorated route for the FAIL cases (the gate returns BEFORE the route runs,
    # so the 401/403/500 body + WWW-Authenticate header are static). Header comparison is byte-exact
    # in normalize.py (WWW-Authenticate is NOT in any drop-list), so the realm challenge is diffed.
    #
    # authbasic: both basic creds set (-> mode "basic"), and CSRF_ORIGIN_CHECK ON so the write-path
    # origin gate is also exercised. A valid `Basic base64("sobs:secret")` passes to the handler;
    # a wrong password / a missing Authorization header falls through to 401 Basic-realm; a write
    # (POST) with no same-origin Origin trips the CSRF 403 (which fires before the credential check,
    # so it needs no creds). The GET pass-route is unaffected by the CSRF gate (GET is not a write).
    "authbasic": {
        "SOBS_BASIC_AUTH_USERNAME": "sobs",
        "SOBS_BASIC_AUTH_PASSWORD": "secret",
        "SOBS_CSRF_ORIGIN_CHECK": "1",
    },
    # authexternal: EXTERNAL_AUTH_URL set (-> mode "external"). _check_external_auth POSTs
    # {EXTERNAL_AUTH_URL}/internal/auth/validate and passes iff that returns HTTP 200. The URL host
    # is sobs-ai.mock (on the determinism shim allowlist) and SOBS_UPSTREAM_FIXTURES points at the
    # canned dir, so BOTH the Python oracle (httpx MockTransport) and the Go server
    # (checkExternalAuth -> upstreamRequest, R34 fix) read the SAME URL-keyed fixture. The /extauth
    # fixture returns 200 -> a Bearer request passes to the handler; a request with no Bearer never
    # dials the service (the startswith guard fails) and falls to 401 Bearer-realm.
    "authexternal": {
        "SOBS_EXTERNAL_AUTH_URL": "http://sobs-ai.mock/extauth",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # authextfail: same external mode, but the auth-validate fixture returns 403 (not 200), so
    # _check_external_auth returns False even for a well-formed Bearer token -> the request falls to
    # 401 Bearer-realm. This covers the external REJECT arm (the validate-call-but-not-200 branch),
    # distinct from authexternal's no-Bearer-at-all reject.
    "authextfail": {
        "SOBS_EXTERNAL_AUTH_URL": "http://sobs-ai.mock/extauthbad",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # authinvalid: BOTH basic creds AND an external URL configured -> _auth_mode() returns "invalid"
    # (configuration is exclusive). Any decorated route then short-circuits to 500
    # {"error": "Server auth misconfiguration"} before the route runs.
    "authinvalid": {
        "SOBS_BASIC_AUTH_USERNAME": "sobs",
        "SOBS_BASIC_AUTH_PASSWORD": "secret",
        "SOBS_EXTERNAL_AUTH_URL": "http://sobs-ai.mock/extauth",
    },
    # llmguarderr: query/ask GUARD-UNAVAILABLE path via HTTP 500 from the guard endpoint.
    # _call_llm_endpoint (app.py 4742-4770) raises HTTPStatusError when the mock returns HTTP 500;
    # the except branch catches it -> error_text="HTTP 500: Internal Server Error" -> returns ("", stats).
    # _check_guard_model (5182-5192): reply="" -> tries parser(guard_stats.error) -> verdict="" ->
    # returns (False, "guard_unavailable", guard_stats). api_query_ask returns 403
    # {"ok": false, "error": "Request blocked by safety guard: guard_unavailable"}.
    # URL-keyed fixture a161c58d… (POST http://sobs-ai.mock/llmguarderr-guard/v1/chat/completions):
    # {"status": 500, "content": "Internal Server Error"} -> raise_for_status() -> HTTPStatusError.
    # Guard model is the default sobs-guard-model (llama path) so no body-key needed; DISTINCT guard
    # endpoint ensures no collision with other profiles' fixtures.
    "llmguarderr": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/llmguarderr/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/llmguarderr-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # llmguardempty: query/ask GUARD-UNAVAILABLE path via empty-content retry exhaustion.
    # _call_llm_endpoint (app.py 4671-4741): the mock returns HTTP 200 with content=null on BOTH
    # the initial call and the retry call (same URL-keyed fixture), so both reply_text.strip() checks
    # fail -> builds retry messages -> fires again -> still empty -> builds error_stats with
    # retry_max_tokens/initial_max_tokens/error keys -> returns ("", retry_stats_out).
    # _check_guard_model: reply="" -> tries parser(guard_stats.error) -> verdict="" ->
    # returns (False, "guard_unavailable", guard_stats). Same 403 route response as llmguarderr.
    # URL-keyed fixture 38925adb… (POST http://sobs-ai.mock/llmguardempty-guard/v1/chat/completions):
    # {"status": 200, "json": {"choices": [{"message": {"content": null}, "finish_reason": "stop"}],
    # "usage": {"prompt_tokens": 5, "completion_tokens": 1}}} — same fixture for initial + retry call.
    "llmguardempty": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/llmguardempty/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/llmguardempty-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # ---- _normalize_generic_ui_action_tool_call coverage (app.py 4399-4529) -------------------
    # aitoolsempty: LLM proposes propose_ui_action with action_id="" -> line 4407 returns None ->
    # tool_proposals stays empty. Guard returns SAFE (sobs-guard-model, llama path). Distinct mock
    # URLs ensure no fixture collision. SSE fixture a59e76e4… carries the empty action_id tool call.
    # No memory candidates -> saved_memory_ids=[]. The whole turn is byte-compared (no masks).
    "aitoolsempty": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsempty/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsempty-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aitoolsunsupported: LLM proposes logs.nonexistent.action which is NOT in the /logs page
    # manifest -> line 4427-4439 ("unsupported action") -> None -> tool_proposals=[]. Lines 4427-
    # 4439 covered. SSE fixture c6ff1bd6… carries the unknown action_id. No masks needed.
    "aitoolsunsupported": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsunsupported/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsunsupported-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aitoolsalt: LLM proposes logs.filter.apply_sql with alt-key {"sql": "SeverityText = 'ERROR'"}
    # (instead of the canonical sql_where key) -> lines 4471-4494 (alt-key loop) extract the value.
    # SSE fixture 1c2abc8d… carries that alt-key payload. Guard a7f8… is SAFE.
    "aitoolsalt": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsalt/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsalt-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aitoolscrosspage: LLM proposes ai.view.flat from page=/logs but that action lives on /ai ->
    # line 4422-4424 (cross-page manifest lookup) finds it. SSE fixture a9f5f3f1… carries target_page
    # =/ai. The action is confirmed (requires_confirmation=True for cross-page), so the turn ends with
    # a single proposal and no looping. Guard fixture 23ebd651… is SAFE.
    "aitoolscrosspage": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitoolscrosspage/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitoolscrosspage-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiformfilters: LLM proposes ai.filter.apply from page=/ai with valid filters {view, service} ->
    # lines 4445-4469 (apply_form_filters allowlist). Both keys are in the allowlist -> proposal built
    # normally. SSE fixture fa2f5c68… carries those filter args. Guard 3b3bfab5… is SAFE.
    "aiformfilters": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiformfilters/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiformfilters-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiformfiltersblock: LLM proposes ai.filter.apply with {"unknown_filter": "val"} -> line 4460
    # (unknown key not in allowlist) -> the filter is stripped -> args become empty -> proposal not
    # built. SSE fixture 10865084… carries that disallowed key. Guard 609cd1c6… is SAFE.
    "aiformfiltersblock": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiformfiltersblock/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiformfiltersblock-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aitoolsdefault: LLM proposes ai.view.flat from page=/ai with empty args {} -> lines 4502-4504
    # (template default args merged in: the manifest's data-ai-action-args are used as defaults).
    # SSE fixture a291412b… carries empty args. Guard d0302589… is SAFE.
    "aitoolsdefault": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsdefault/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aitoolsdefault-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # ---- _parse_oss_safeguard_reply coverage (app.py 5042-5092) --------------------------------
    # aiguardossempty: gpt-oss-safeguard guard returns content="" -> _parse_oss_safeguard_reply("")
    # -> line 5048 (empty strip) -> ("", "") -> reply="" -> line 5192 guard_unavailable.
    # Guard fixture cc892dac… returns empty content. QUERY_PAGE_ENABLED so the guard call happens.
    "aiguardossempty": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossempty/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossempty-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardossbool: gpt-oss-safeguard guard returns {"violation": true, "policy_category": "S3"}
    # -> line 5069-5070 (bool violation -> UNSAFE) + line 5082-5083 (policy_category as category).
    # S3 (Sex-Related Crimes) has a label and is NOT noisy -> line 5226 "blocked (S3: Sex-Related
    # Crimes)". Guard fixture 4e2da042….
    "aiguardossbool": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossbool/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossbool-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardossstr: gpt-oss-safeguard guard returns {"violation": "unsafe", "policy_category": "S5"}
    # -> line 5073-5076 (str violation "unsafe" -> UNSAFE). S5 (Defamation) has a label, NOT noisy
    # -> line 5226 "blocked (S5: Defamation)". Guard fixture 83cfc132….
    "aiguardossstr": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossstr/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossstr-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardossstrafe: gpt-oss-safeguard guard returns {"violation": "safe"} -> line 5077-5078
    # (str violation "safe" -> SAFE) -> allowed=True -> main SQL-gen endpoint called. Guard fixture
    # c5a49d4a…; main fixture 39749feb… (SELECT count() AS cnt FROM otel_logs). execute=false so
    # no chdb run; the SQL is returned verbatim. trace_id/turn_id time-based -> masked.
    "aiguardossstrafe": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossstrafe/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardossstrafe-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardruleids: gpt-oss-safeguard guard returns {"violation": 1, "policy_category": null,
    # "rule_ids": ["S11"]} -> line 5069 (int/float truthy -> UNSAFE) + line 5065 (parsed_obj not None)
    # + line 5084-5087 (rule_ids[0]="S11" used as category). S11 (Suicide & Self-Harm) has a label,
    # not noisy -> line 5226 "blocked (S11: Suicide & Self-Harm)". Guard fixture 6aa70e86….
    "aiguardruleids": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardruleids/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardruleids-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardembedjson: gpt-oss-safeguard guard returns plain text with embedded JSON
    # "Guard analysis: {\"violation\": 1, \"policy_category\": \"S3\"}" -> line 5052-5060 (regex
    # extracts the embedded JSON object) -> parsed normally -> UNSAFE + S3 -> "blocked (S3: ...)".
    # Guard fixture dd76c0b9….
    "aiguardembedjson": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardembedjson/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardembedjson-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # ---- _check_guard_model coverage (app.py 5136-5233) ---------------------------------------
    # aiheuristic: question contains "jailbreak" (in _AI_GUARD_BLOCK_KEYWORDS) -> _heuristic_guard_
    # check returns False -> line 5143 returns (False, "Blocked by heuristic safety check", {}).
    # Guard LLM is never called. Guard URL set but never dialed; no guard fixture needed. The
    # returned 403 {"error": "... Blocked by heuristic safety check"} is byte-compared.
    "aiheuristic": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiheuristic/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiheuristic-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # ainoguard: no SOBS_AI_GUARD_ENDPOINT_URL and no SOBS_AI_GUARD_MODEL -> guard_url="" and
    # guard_model="" -> line 5149-5150 returns (False, "guard_not_configured", {}). The 403 error
    # "Request blocked by safety guard: guard_not_configured" is byte-compared. No guard fixture.
    # Distinct main endpoint so the URL-keyed fixture does not collide with other profiles.
    "ainoguard": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/ainoguard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardbenigno: gpt-oss-safeguard guard returns S1 (Violent Crimes, NOISY) -> line 5208-5213:
    # question "show me error logs" has "error" + "logs" (2 benign_obs keywords, no HIGH_RISK) ->
    # benign_observability=True -> override -> allowed=True. Main SQL-gen called. Guard 94604b66…;
    # main c653417a…. trace_id/turn_id time-based -> masked. execute=false.
    "aiguardbenigno": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardbenigno/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardbenigno-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardbenignav: gpt-oss-safeguard guard returns S2 (Non-Violent Crimes, NOISY) -> line 5214-
    # 5219: question "navigate to the metrics view" has "navigate" (navigation_intent) + "view"
    # (navigation_surface) -> benign_navigation=True; "metrics" gives only 1 obs hit (<2) so
    # benign_observability=False -> benign_navigation branch fires -> allowed=True. Guard 4af0ba72…;
    # main 8d2a25e9…. trace_id/turn_id time-based -> masked. execute=false.
    "aiguardbenignav": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardbenignav/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardbenignav-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardbenigns8: gpt-oss-safeguard guard returns S8 (Intellectual Property, NOISY) -> line
    # 5220-5225: question "show me llm token usage" has "show" (usage_intent) + "token" (usage_
    # analytics) -> benign_ai_usage=True -> S8-specific override -> allowed=True. Guard 073ba76e…;
    # main c35481d7…. trace_id/turn_id time-based -> masked. execute=false.
    "aiguardbenigns8": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardbenigns8/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardbenigns8-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardinvalidcat: gpt-oss-safeguard guard returns {"violation": 1, "policy_category": "S15"}
    # -> UNSAFE + S15 (not in _AI_GUARD_CATEGORIES -> label="") -> line 5228-5230 (category but no
    # label, OSS model) -> "blocked (policy_category=S15)". Guard 53673bbb….
    "aiguardinvalidcat": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardinvalidcat/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardinvalidcat-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardllama20: llama-guard-20b (non-OSS llama model) returns "unsafe\nS20" -> _parse_guard_
    # reply (llama parser) -> UNSAFE + S20 (not in _AI_GUARD_CATEGORIES -> label="") -> line 5231
    # (category but no label, NON-OSS model) -> "blocked (S20)". Guard 769453d9….
    "aiguardllama20": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardllama20/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardllama20-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "llama-guard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardnocat: gpt-oss-safeguard guard returns {"violation": true} (no policy_category, no
    # rule_ids) -> bool UNSAFE + category="" -> line 5232 "blocked". Guard f554ccc2….
    "aiguardnocat": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardnocat/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardnocat-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
    # aiguardplaintext: gpt-oss-safeguard guard returns plain text with no JSON -> _parse_oss_
    # safeguard_reply fails to extract any verdict (no "{" in text after strip) -> ("", "") ->
    # verdict="" -> line 5233 "guard_invalid_reply: <text[:120]>". Guard 440f1fb3….
    "aiguardplaintext": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aiguardplaintext/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/aiguardplaintext-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "gpt-oss-safeguard-20b",
        "SOBS_QUERY_PAGE_ENABLED": "1",
        "SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR,
    },
}

# Profiles whose fixture needs extra rows inserted before capture/replay (via
# `seed_fixtures.py --only-profile <name>`). Isolated to the profile — never seeded into base.
SEEDED_PROFILES = {
    "agentrun",
    "dmprune",
    "notif",
    "notifydispatch",
    "notifcheck",
    "notifeval",
    "notifgen",
    "agenttrigger",
    "agentflow",
    "agentctx",
    "agentcopilot",
    "issuereuse",
    "issuereuse2",
    "issuecreateerr",
    "onbupdateerr",
    "workitems",
    "notifagent",
    "notifagentmiss",
    "anomalycheck",
    "dmbackup",
    "k8s",
    "k8srich",
    "k8sprom",
    "repoapp",
    "cveosv",
    "cvescan",
    "tagauto",
    "tagautorich",
    "dashboardautorich",
    "metricsauto",
    "seasonalauto",
    "metricsrich",
    "rumvitals",
    "tagsuggest",
    "cvebackfill",
    "depsrich",
    "cveactions",
    "lockfiles",
    "onboard",
    "onbupdate",
    "issuesraise",
    "githubtoken",
    "onboardrepos",
    "repohealth",
    "mcpkey",
    "mcpauth",
    "aichat",
    "aiexport2",
    "ciauth",
    "tracedetail",
    "tracedetailerr",
    "incidentmatch",
    "logsview",
    "logsrich",
    "tagmatch",
    "errorsview",
    "errorsummary",
    "aiview",
    "airich",
    "aiturns",
    "aitoolturns",
    "airichsql",
    "tracesrich",
    "tracemetrics",
    "tracewindows",
    "dashview",
    "chartedit",
    "regexlogs",
    "regextraces",
    "regexrum",
    "regexerrors",
    "regexmetrics",
    "metricscreate",
    "notifrule",
    "cveview",
    "summaryrich",
    "enrichlibs",
    "rumasset",
    "webtraffic",
    "aihelpermemcons",
}


def route_profile(route: dict) -> str:
    """The profile a manifest route belongs to (default ``base``)."""
    return str(route.get("profile") or "base")


def profile_env(name: str) -> dict[str, str]:
    """The env overlay for a profile name (empty for unknown names / base)."""
    return dict(PROFILES.get(name, {}))


# ---------------------------------------------------------------------------
# Pre-capture hooks (optional, per-profile)
# ---------------------------------------------------------------------------
# A pre-capture hook is an async callable (app_module) -> None that runs AFTER
# boot but BEFORE any HTTP routes are captured. Its execution is included in the
# coverage measurement, so it can cover app.py functions that are only reachable
# from background tasks (never from an HTTP route). The HTTP goldens for the
# profile's routes are captured immediately after, unchanged.
#
# PRE_CAPTURE_FNS maps profile name -> async callable(app_module) -> None.
# capture_routes.py calls profile_pre_capture_fn(profile, app_module, loop) when
# a matching entry exists.


async def _repohealth_sync_pre_capture(app_module) -> None:  # noqa: E501
    # Exercise _sync_github_repo_health_once (app.py 17255-17292): the persistence
    # wrapper that _github_repo_health_loop calls but no HTTP route exposes. Two
    # calls are needed to cover both branches of the change-dedup guard:
    #
    #   Call 1 (no previous raw in sobs_app_settings): covers 17257-17259 (entry),
    #     17262 (compact_values), 17270 (_get_app_setting returns ""), 17271 (if False),
    #     17286 (write last_sync), 17287 (compact dict), 17291 (write last_summary),
    #     17292 (return). Total: 10 statement lines.
    #   Call 2 (previous raw == current values, just written): covers 17272 (try:),
    #     17273 (_safe_json_loads), 17274 (previous_values dict), 17283 (compare),
    #     17284 (early return). Total: 5 more statement lines.
    #
    # Remaining uncovered (error/exception paths not exercisable from here without
    # DB failure or deliberately invalid sobs_app_settings JSON): 17260, 17281, 17282.
    #
    # The pre-capture runs inside the coverage measurement (via coverage run -p) so
    # these lines register as covered. The subsequent HTTP GET still returns the same
    # _collect_github_repo_health_summary result (URL-keyed fixtures, not counter-keyed),
    # so the golden is byte-identical to what the Go server replays.
    db = app_module.get_db()
    await app_module._sync_github_repo_health_once(db)  # first call: write path
    await app_module._sync_github_repo_health_once(db)  # second call: dedup path


PRE_CAPTURE_FNS: dict = {
    "repohealth": _repohealth_sync_pre_capture,
}


def profile_pre_capture_fn(name: str):
    """Return the async pre-capture callable for a profile, or None."""
    return PRE_CAPTURE_FNS.get(name)
