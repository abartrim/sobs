#!/usr/bin/env bash
# start_ollama_ai_test.sh — Start local SOBS against a local Ollama server.
#
# This script:
# 1) Validates local Ollama availability.
# 2) Exports SOBS AI/Guard env vars using Ollama's OpenAI-compatible /v1 endpoint.
# 3) Runs SOBS (or a custom command).
#
# Kubernetes is not used here. No kubectl setup is required.
#
# Usage:
#   ./scripts/start_ollama_ai_test.sh
#   ./scripts/start_ollama_ai_test.sh -- python app.py
#   OLLAMA_BASE_URL=http://127.0.0.1:11434 ./scripts/start_ollama_ai_test.sh -- .venv/bin/python app.py
#
# Optional demo app controls:
#   START_EXAMPLE_APP=1 (default) launches a local Flask demo app for RUM/replay testing.
#   EXAMPLE_APP_PORT=5005 sets the demo app port.

set -euo pipefail

OLLAMA_BASE_URL="${OLLAMA_BASE_URL:-http://127.0.0.1:11434}"
OLLAMA_TAGS_URL="${OLLAMA_BASE_URL%/}/api/tags"

# Default to practical local models; override as needed.
SOBS_AI_MODEL="${SOBS_AI_MODEL:-llama3.1:8b}"
SOBS_AI_GUARD_MODEL="${SOBS_AI_GUARD_MODEL:-llama-guard3:1b}"

# Optional auto-pull of models before launch.
OLLAMA_PULL_MODELS="${OLLAMA_PULL_MODELS:-0}"

# Demo app (browser RUM/replay test surface).
START_EXAMPLE_APP="${START_EXAMPLE_APP:-1}"
EXAMPLE_APP_PORT="${EXAMPLE_APP_PORT:-5005}"
EXAMPLE_APP_SOBS_BASE_URL="${EXAMPLE_APP_SOBS_BASE_URL:-http://127.0.0.1:44317}"
EXAMPLE_APP_SCRIPT="${EXAMPLE_APP_SCRIPT:-examples/python/rum_replay_test_app.py}"
EXAMPLE_APP_PYTHON="${EXAMPLE_APP_PYTHON:-python}"
EXAMPLE_APP_LOG="${EXAMPLE_APP_LOG:-/tmp/sobs-rum-replay-demo.log}"
EXAMPLE_APP_PID=""

if [[ "${1:-}" == "--" ]]; then
  shift
fi
RUN_CMD=("$@")
if [[ ${#RUN_CMD[@]} -eq 0 ]]; then
  RUN_CMD=(python app.py)
fi

cleanup() {
  if [[ -n "$EXAMPLE_APP_PID" ]] && kill -0 "$EXAMPLE_APP_PID" >/dev/null 2>&1; then
    kill "$EXAMPLE_APP_PID" >/dev/null 2>&1 || true
    wait "$EXAMPLE_APP_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

start_example_app() {
  if [[ "$START_EXAMPLE_APP" != "1" ]]; then
    return 0
  fi

  if [[ ! -f "$EXAMPLE_APP_SCRIPT" ]]; then
    echo "[warn] demo app script not found: $EXAMPLE_APP_SCRIPT (continuing without demo app)"
    return 0
  fi

  echo "[info] starting RUM replay demo app on http://127.0.0.1:${EXAMPLE_APP_PORT}"
  SOBS_BASE_URL="$EXAMPLE_APP_SOBS_BASE_URL" EXAMPLE_APP_PORT="$EXAMPLE_APP_PORT" \
    "$EXAMPLE_APP_PYTHON" "$EXAMPLE_APP_SCRIPT" >"$EXAMPLE_APP_LOG" 2>&1 &
  EXAMPLE_APP_PID=$!

  local i
  for i in $(seq 1 40); do
    if nc -z 127.0.0.1 "$EXAMPLE_APP_PORT" >/dev/null 2>&1; then
      echo "[ok] demo app available at http://127.0.0.1:${EXAMPLE_APP_PORT}"
      echo "[info] demo app log: $EXAMPLE_APP_LOG"
      return 0
    fi
    if ! kill -0 "$EXAMPLE_APP_PID" >/dev/null 2>&1; then
      echo "[warn] demo app exited early; continuing without it. Log: $EXAMPLE_APP_LOG"
      tail -n 40 "$EXAMPLE_APP_LOG" || true
      EXAMPLE_APP_PID=""
      return 0
    fi
    sleep 0.2
  done

  echo "[warn] demo app startup timed out; continuing without it. Log: $EXAMPLE_APP_LOG"
  EXAMPLE_APP_PID=""
  return 0
}

if ! curl -fsS "$OLLAMA_TAGS_URL" >/dev/null 2>&1; then
  echo "[error] cannot reach Ollama at $OLLAMA_BASE_URL" >&2
  echo "Start Ollama first (example: 'ollama serve') or set OLLAMA_BASE_URL." >&2
  exit 1
fi

if [[ "$OLLAMA_PULL_MODELS" == "1" ]]; then
  if ! command -v ollama >/dev/null 2>&1; then
    echo "[error] OLLAMA_PULL_MODELS=1 requires 'ollama' CLI in PATH" >&2
    exit 1
  fi
  echo "[info] pulling model: $SOBS_AI_MODEL"
  ollama pull "$SOBS_AI_MODEL"
  if [[ "$SOBS_AI_GUARD_MODEL" != "$SOBS_AI_MODEL" ]]; then
    echo "[info] pulling guard model: $SOBS_AI_GUARD_MODEL"
    ollama pull "$SOBS_AI_GUARD_MODEL"
  fi
fi

export SOBS_AI_ENDPOINT_URL="${SOBS_AI_ENDPOINT_URL:-${OLLAMA_BASE_URL%/}/v1}"
export SOBS_AI_GUARD_ENDPOINT_URL="${SOBS_AI_GUARD_ENDPOINT_URL:-${OLLAMA_BASE_URL%/}/v1}"
export SOBS_AI_MODEL
export SOBS_AI_GUARD_MODEL

# DLP is optional for local Ollama workflow. Only export if provided externally.
if [[ -n "${SOBS_AI_DLP_ENDPOINT_URL:-}" ]]; then
  export SOBS_AI_DLP_ENDPOINT_URL
fi
if [[ -n "${SOBS_AI_API_KEY:-}" ]]; then
  export SOBS_AI_API_KEY
fi

start_example_app

echo
printf 'Configured AI settings for local Ollama:\n'
printf '  kubernetes_integration=disabled (local only)\n'
printf '  SOBS_AI_ENDPOINT_URL=%s\n' "$SOBS_AI_ENDPOINT_URL"
printf '  SOBS_AI_GUARD_ENDPOINT_URL=%s\n' "$SOBS_AI_GUARD_ENDPOINT_URL"
printf '  SOBS_AI_MODEL=%s\n' "$SOBS_AI_MODEL"
printf '  SOBS_AI_GUARD_MODEL=%s\n' "$SOBS_AI_GUARD_MODEL"
if [[ -n "${SOBS_AI_DLP_ENDPOINT_URL:-}" ]]; then
  printf '  SOBS_AI_DLP_ENDPOINT_URL=%s\n' "$SOBS_AI_DLP_ENDPOINT_URL"
else
  printf '  SOBS_AI_DLP_ENDPOINT_URL=<empty>\n'
fi
if [[ -n "${SOBS_AI_API_KEY:-}" ]]; then
  printf '  SOBS_AI_API_KEY=<set>\n'
else
  printf '  SOBS_AI_API_KEY=<empty>\n'
fi
if [[ "$START_EXAMPLE_APP" == "1" ]]; then
  printf '  demo_app_url=http://127.0.0.1:%s\n' "$EXAMPLE_APP_PORT"
  printf '  demo_app_script=%s\n' "$EXAMPLE_APP_SCRIPT"
else
  printf '  demo_app=<disabled>\n'
fi
echo
printf 'Running: %s\n' "${RUN_CMD[*]}"

"${RUN_CMD[@]}"
