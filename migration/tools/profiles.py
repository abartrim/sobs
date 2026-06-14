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
}


def route_profile(route: dict) -> str:
    """The profile a manifest route belongs to (default ``base``)."""
    return str(route.get("profile") or "base")


def profile_env(name: str) -> dict[str, str]:
    """The env overlay for a profile name (empty for unknown names / base)."""
    return dict(PROFILES.get(name, {}))
