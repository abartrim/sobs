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

from pathlib import Path

# Absolute path to the canned upstream (GitHub/OSV) response directory, shared by the Python
# determinism httpx shim and the Go upstream.go fixtures transport.
_UPSTREAM_DIR = str(Path(__file__).resolve().parents[2] / "migration" / "fixtures" / "upstream")

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
    # errorsview: seed otel_logs error events (3 groups, counts 3/2/1, one TraceId each) +
    # one sobs_error_resolutions row so view_errors renders its POPULATED branches — the
    # non-grouped narrow+hydrate path, the grouped aggregate path, and the resolved=0/1/all
    # variants. Deterministic: distinct counts (tie-free grouped ORDER BY), one TraceId per group
    # (single-element groupUniqArray), distinct fixed timestamps (stable argMax + ORDER BY).
    "errorsview": {},
    # aiview: seed otel_traces AI spans (gen_ai.* SpanAttributes) so view_ai renders its POPULATED
    # branches — the ai_items build, the flat + trace view modes, the totals aggregation, and the
    # service/model/operation/span_name/row_type/sql filters. Deterministic: distinct timestamps,
    # distinct token/model values, empty message JSON (parsers return empty), no ties.
    "aiview": {},
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
    # cveview: seed sobs_cve_findings (1 per severity, distinct Published) so the summary
    # cve-overview counts and view_enrichment_cve findings loop/filters render populated.
    # Read-only (the pages only query) -> shareable, but kept isolated/seeded for clarity.
    "cveview": {},
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
    # githubtoken = the github mock + a seeded ai.github_token, so the onboarding inspect/issue
    # routes reach (and exercise) their token-gated GitHub branch.
    "githubtoken": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # onboardrepos = the github mock + a seeded ai.github_token so the onboarding READ endpoints
    # take their token-USED branch: list-repos dials the ?type=all users endpoint (canned repo list
    # incl. private repos -> token_used=true, empty visibility_note) and inspect-repo runs the full
    # repo-inspection GitHub flow (workflows listing + ci.yml + copilot graphql, all canned).
    "onboardrepos": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
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
    # repoapp: a seeded registered app + release + github token so /settings/repositories/<id>/...
    # actions run their real branch; the github mock serves the token-validate /rate_limit call.
    "repoapp": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # cveosv: a seeded telemetry.sdk library so the cve scan reaches its OSV branch; the OSV mock
    # serves the canned /v1/query vuln response. No github token (backfill stays the 0/0/cap no-op).
    "cveosv": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # otlpingest: a no-env isolation profile so the OTLP /v1/{logs,traces,metrics} ingest INSERTs
    # (which would otherwise ripple into every otel reader) land in their own fixture copy.
    "otlpingest": {},
    # tagauto: 30 recent prod-service otel_logs rows so auto_tag_rules' in-window branch generates
    # one candidate. No env overlay — just the (isolated) seed. Timestamp-independent output.
    "tagauto": {},
    # metricsauto: a constant recent log_volume series so auto_metrics_rules' threshold scan
    # generates fixed candidates (exact quantiles of a constant). No env overlay — just the seed.
    "metricsauto": {},
    # rumvitals: now()-relative web-vital + error rows in hyperdx_sessions so view_rum's Web-vitals
    # (anomaly summary + 1m sparkline + hotspot) and Error-trend (direction + by_type + sparkline)
    # blocks all populate. Constant values -> every derived quantity is timestamp-independent; only
    # the now()-derived bucket/last_seen timestamps drift, masked in the route. No env overlay.
    "rumvitals": {},
    # tagsuggest: seeded otel_logs/otel_traces/hyperdx_sessions + sobs_record_tags + sobs_log_attr_keys
    # so /api/settings/tags/condition-suggestions returns non-empty ranked suggestions for every
    # scope/target/field branch. No env overlay — just the (isolated) seed. All seed rows use the
    # fixed determinism-window timestamp and the builders' ORDER BY ... , <value> tiebreaks make the
    # ranked output fully deterministic.
    "tagsuggest": {},
    # onboard: a seeded app+token so onboarding create-issues runs its realtime path (rotate CI key)
    # and its github-issue path (open-issue search 404s -> empty -> create via the canned POST).
    "onboard": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # cvebackfill: a seeded app+release+github token so the cve scan's github backfill attempts a
    # release (the repo has no fetchable lockfile -> every contents GET 404s -> attempted=1,
    # inserted=0). The github mock dir must be set so the fetches 404 rather than erroring.
    "cvebackfill": {"SOBS_UPSTREAM_FIXTURES": _UPSTREAM_DIR},
    # refine: query/refine-chart — query gate on + the LLM endpoint pointed at the canned
    # /chat/completions mock (distinct path so its URL key is unique to this route).
    "refine": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/refine/v1",
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
    # aibuild: dashboards/spec/ai-build — the vanna pipeline (generate SQL -> execute -> named
    # queries -> chart option). No guard check; one canned /chat/completions ("SELECT 1 AS x") is
    # reused for every stage (URL-keyed), so the named-query/chart stages fail to parse identically
    # on both sides and fall back deterministically.
    "aibuild": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/aibuild/v1",
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
    # ask: query/ask — guard + main endpoints on DISTINCT mock paths (two canned responses).
    "ask": {
        "SOBS_AI_ENDPOINT_URL": "http://sobs-ai.mock/ask/v1",
        "SOBS_AI_GUARD_ENDPOINT_URL": "http://sobs-ai.mock/ask-guard/v1",
        "SOBS_AI_MODEL": "sobs-parity-model",
        "SOBS_AI_GUARD_MODEL": "sobs-guard-model",
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
    "notifcheck",
    "notifgen",
    "agenttrigger",
    "issuereuse",
    "notifagent",
    "notifagentmiss",
    "dmbackup",
    "k8s",
    "repoapp",
    "cveosv",
    "tagauto",
    "metricsauto",
    "rumvitals",
    "tagsuggest",
    "cvebackfill",
    "onboard",
    "issuesraise",
    "githubtoken",
    "onboardrepos",
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
    "errorsview",
    "aiview",
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
    "enrichlibs",
    "rumasset",
}


def route_profile(route: dict) -> str:
    """The profile a manifest route belongs to (default ``base``)."""
    return str(route.get("profile") or "base")


def profile_env(name: str) -> dict[str, str]:
    """The env overlay for a profile name (empty for unknown names / base)."""
    return dict(PROFILES.get(name, {}))
