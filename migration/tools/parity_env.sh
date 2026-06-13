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
export BUILD_VERSION="parity-build"
export SOBS_BASE_PATH=""

# Feature flags — pin to a known config so page branches are stable. Mirror these in
# the Go config loader's parity defaults.
export SOBS_ENABLE_FIRST_RUN_TOUR="0"
export SOBS_QUERY_PAGE_ENABLED="1"
export SOBS_KUBERNETES_ENABLED="1"

# chdb memory caps — pin so any size-derived output (rare) is stable.
export CHDB_MAX_SERVER_MB="768"
export CHDB_MAX_THREADS="1"

# Optional features OFF during parity (ported in Phase 6).
export SOURCE_MAP_ENABLE="0"
