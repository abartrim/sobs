#!/usr/bin/env bash
# start_ollama_ai_test_go.sh — Go variant of start_ollama_ai_test.sh.
#
# Builds the Go SOBS server and runs it (in place of `python app.py`) with the same local
# workflow: a local Ollama for AI features + the RUM/OTEL demo app for triggering data.
# The Go server uses the native libchdb embedded DB (NO Python/chdb needed) and
# self-initializes its chdb schema on first run, so it sidesteps the Python-3.14/chdb-wheel
# bind that breaks `python app.py` locally.
#
# Usage:
#   ./scripts/start_ollama_ai_test_go.sh                      # build + run (Ollama optional)
#   SOBS_GO_SKIP_BUILD=1 ./scripts/start_ollama_ai_test_go.sh # reuse the last build
#   START_EXAMPLE_APP=0  ./scripts/start_ollama_ai_test_go.sh # server only, no demo app
#   SOBS_AI_OPTIONAL=0   ./scripts/start_ollama_ai_test_go.sh # require Ollama (like the Python script)
#
# Go-specific env (everything else is passed through to start_ollama_ai_test.sh):
#   CHDB_LIB_PATH   path to libchdb.so (auto-detected: /usr/local/lib, repo .libchdb, /usr/lib)
#   SOBS_DATA_DIR   chdb data dir (default .local/sobs-go-data; schema self-inits on first run)
#   SOBS_GO_BIN     output binary path (default .local/sobs-go)
#   SOBS_PORT       server port (default 44317 — matches the demo app endpoints + health check)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO"

# --- libchdb: the embedded ClickHouse the Go server dlopens at runtime ---
if [[ -z "${CHDB_LIB_PATH:-}" ]]; then
  for cand in /usr/local/lib/libchdb.so "$REPO/.libchdb/libchdb.so" /usr/lib/libchdb.so; do
    [[ -f "$cand" ]] && { CHDB_LIB_PATH="$cand"; break; }
  done
fi
if [[ -z "${CHDB_LIB_PATH:-}" || ! -f "${CHDB_LIB_PATH:-}" ]]; then
  echo "[error] libchdb.so not found. Set CHDB_LIB_PATH=/path/to/libchdb.so" >&2
  echo "        (the repo ships a Linux libchdb under .libchdb/ for Docker; on macOS install the" >&2
  echo "         native libchdb to /usr/local/lib/libchdb.so — see go/CHDB_PIN.md)" >&2
  exit 1
fi
export CHDB_LIB_PATH
echo "[info] CHDB_LIB_PATH=$CHDB_LIB_PATH"

# --- local chdb data dir (Go applies the 43-statement schema on first run) ---
export SOBS_DATA_DIR="${SOBS_DATA_DIR:-$REPO/.local/sobs-go-data}"
mkdir -p "$SOBS_DATA_DIR"
echo "[info] SOBS_DATA_DIR=$SOBS_DATA_DIR"

# --- build the Go binary ---
GO_BIN="${SOBS_GO_BIN:-$REPO/.local/sobs-go}"
mkdir -p "$(dirname "$GO_BIN")"
if [[ "${SOBS_GO_SKIP_BUILD:-0}" == "1" && -x "$GO_BIN" ]]; then
  echo "[info] reusing existing Go binary: $GO_BIN (SOBS_GO_SKIP_BUILD=1)"
else
  echo "[info] building Go SOBS server -> $GO_BIN (CGO_ENABLED=1)"
  ( cd go && CGO_ENABLED=1 go build -trimpath -o "$GO_BIN" ./cmd/sobs )
fi

# --- hand off to the shared launcher (Ollama check + AI env + RUM/OTEL demo app), running the
#     Go binary instead of `python app.py`. Ollama is OPTIONAL by default here so you can start the
#     server and trigger data without a model server (AI routes degrade; everything else works). ---
export SOBS_AI_OPTIONAL="${SOBS_AI_OPTIONAL:-1}"
echo "[info] handing off to start_ollama_ai_test.sh (Go binary; SOBS_AI_OPTIONAL=$SOBS_AI_OPTIONAL)"
exec "$SCRIPT_DIR/start_ollama_ai_test.sh" -- "$GO_BIN"
