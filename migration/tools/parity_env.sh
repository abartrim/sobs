# Pinned environment for parity capture & replay. Sourced by capture_golden.py and
# parity_check.py for BOTH the Python and Go processes. Every variable here is something
# the HTTP output can depend on; pinning it removes a source of divergence.
#
# Rule: if a golden changes when you change one of these, it belongs here (pinned).
# Adding a new SOBS_* env that affects output? Pin it here so Python and Go agree.

export SOBS_PARITY=1
export SOBS_SECRET_KEY="parity-fixed-secret-key"
export SOBS_SETTINGS_ENCRYPTION_SECRET="parity-fixed-encryption-secret"
export SOBS_SESSION_COOKIE_NAME="sobs_session"
export SOBS_SESSION_COOKIE_SAMESITE="Lax"
# app.py reads SOBS_BUILD_VERSION (default "" -> sobs_version renders "dev"). Leave unset
# so the goldens render "dev"; the Go side defaults to "dev" too.
export SOBS_BASE_PATH=""

# Feature flags. NOTE: query_enabled and kubernetes_enabled are NOT env-controlled —
# the app computes them from DB settings (_query_page_enabled needs ai.endpoint_url +
# ai.model; _kubernetes_enabled needs the kubernetes.enabled setting). With the empty
# fixture DB both are False, which is what the goldens capture. The Go side must compute
# them from the same settings once the DB layer lands; until then they default False.
export SOBS_ENABLE_FIRST_RUN_TOUR="0"

# Disable the summary-stats TTL cache during parity. app.py caches the dashboard headline stats
# (Logs / Open Errors / AI / Services) in a module global for SOBS_SUMMARY_STATS_CACHE_TTL_SEC
# (default 60s); under the frozen parity clock that cache NEVER expires, so the value is whatever
# the FIRST summary() call in the capture process computed — making the golden depend on capture
# order/state. The Go port has no such cache (it computes fresh per request), so the two diverge.
# TTL=0 makes `expires_at > now` always false -> Python also computes fresh every request, so both
# sides are symmetric and the now()-anchored summary routes become deterministic.
export SOBS_SUMMARY_STATS_CACHE_TTL_SEC="0"

# chdb memory caps — pin so any size-derived output (rare) is stable.
export CHDB_MAX_SERVER_MB="768"
export CHDB_MAX_THREADS="1"

# Optional features OFF during parity (ported in Phase 6).
export SOURCE_MAP_ENABLE="0"
