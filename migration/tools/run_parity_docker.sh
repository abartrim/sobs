#!/usr/bin/env bash
# Run the golden-corpus parity check inside the sobs-parity Docker image (Python 3.14 oracle +
# chdb 4.1.9 + Go 1.23), mirroring the CI "Go Parity" job locally. The repo is bind-mounted so the
# live worktree code is what gets tested; the pinned native libchdb (chdb-core v26.5.0) for the Go
# side is supplied from <main-repo>/.libchdb via CHDB_LIB_PATH.
#
# Usage:
#   migration/tools/run_parity_docker.sh                     # full capture+replay (run_parity_ci.py)
#   migration/tools/run_parity_docker.sh get__traces         # capture+check a single route id
#
# Prereqs (one-time):
#   docker build -f Dockerfile.parity -t sobs-parity:latest .
#   mkdir -p .libchdb && curl -fsSL <chdb-core v26.5.0 libchdb for your arch> | tar -xz -C .libchdb
set -euo pipefail

# Resolve the main repo root (this script lives in <root>/migration/tools, but may run from a
# git worktree under <root>/.claude/worktrees/<name>). MOUNT_ROOT is what we bind-mount so both
# the worktree code and the shared .libchdb cache are visible inside the container.
WORKTREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAIN_ROOT="$(git -C "$WORKTREE_ROOT" rev-parse --path-format=absolute --git-common-dir 2>/dev/null | sed 's#/\.git$##')"
MAIN_ROOT="${MAIN_ROOT:-$WORKTREE_ROOT}"

ROUTE="${1:-}"
LIBCHDB="${MAIN_ROOT}/.libchdb/libchdb.so"
if [[ ! -f "$LIBCHDB" ]]; then
  echo "error: missing $LIBCHDB — download the pinned libchdb first (see header)." >&2
  exit 1
fi

# Inside the container the main repo mounts at /repo; the worktree is at the same relative path.
REL="$(python3 -c "import os,sys;print(os.path.relpath(sys.argv[1],sys.argv[2]))" "$WORKTREE_ROOT" "$MAIN_ROOT")"
CONTAINER_WORKDIR="/repo/${REL}"
[[ "$REL" == "." ]] && CONTAINER_WORKDIR="/repo"

if [[ -n "$ROUTE" ]]; then
  RUN_CMDS="python migration/tools/seed_fixtures.py \
    && python migration/tools/capture_routes.py --only ${ROUTE} --profile base \
    && python migration/tools/parity_check.py --only ${ROUTE}"
else
  RUN_CMDS="python migration/tools/run_parity_ci.py"
fi

# Make the in-container Go build hermetic: the local Dockerfile.parity (unlike Dockerfile.parity-ci)
# does not bake `go mod download`, so a cold build fetches every module from proxy.golang.org — slow
# and flaky over the colima VM's egress (TLS handshake timeouts). Mount the host's populated module
# cache read-only and set GOPROXY=off/GOFLAGS=-mod=mod so the build reads modules straight from the
# cache with no network. GOMODCACHE holds platform-independent SOURCE, so a macOS cache builds fine
# inside the Linux container. Skipped automatically if the host has no Go / empty cache.
GOMODCACHE_HOST="$(go env GOMODCACHE 2>/dev/null || true)"
GO_MOUNT=()
if [[ -n "${GOMODCACHE_HOST}" && -d "${GOMODCACHE_HOST}" ]]; then
  GO_MOUNT=(-v "${GOMODCACHE_HOST}:/root/go/pkg/mod:ro" -e GOFLAGS=-mod=mod -e GOPROXY=off -e GOSUMDB=off)
fi

exec docker run --rm \
  -v "${MAIN_ROOT}:/repo" \
  -w "${CONTAINER_WORKDIR}" \
  -e CHDB_LIB_PATH=/repo/.libchdb/libchdb.so \
  "${GO_MOUNT[@]}" \
  sobs-parity:latest \
  bash -c "set -euo pipefail; ${RUN_CMDS}"
