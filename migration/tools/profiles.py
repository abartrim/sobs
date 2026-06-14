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
    # createrepo: a no-env isolation profile for create_repo — its sobs_apps/ai-settings INSERTs
    # run in their own fixture copy so they don't ripple into the base registry/repository readers
    # (a profile pass = fresh fixture, and only this route runs in it).
    "createrepo": {},
    # mcpkey: a seeded mcp.api_keys descriptor so DELETE /api/mcp/keys/<id> can revoke it.
    "mcpkey": {},
    # aichat: a seeded gen_ai chat turn (otel_logs) so the chat-detail reader serializes it. The
    # otel_logs row is isolated to this profile so base telemetry readers stay empty.
    "aichat": {},
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
    "notif",
    "notifcheck",
    "notifgen",
    "agenttrigger",
    "githubtoken",
    "mcpkey",
    "aichat",
}


def route_profile(route: dict) -> str:
    """The profile a manifest route belongs to (default ``base``)."""
    return str(route.get("profile") or "base")


def profile_env(name: str) -> dict[str, str]:
    """The env overlay for a profile name (empty for unknown names / base)."""
    return dict(PROFILES.get(name, {}))
