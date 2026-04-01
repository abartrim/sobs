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

set -euo pipefail

OLLAMA_BASE_URL="${OLLAMA_BASE_URL:-http://127.0.0.1:11434}"
OLLAMA_TAGS_URL="${OLLAMA_BASE_URL%/}/api/tags"

# Default to practical local models; override as needed.
SOBS_AI_MODEL="${SOBS_AI_MODEL:-llama3.1:8b}"
SOBS_AI_GUARD_MODEL="${SOBS_AI_GUARD_MODEL:-llama-guard3:1b}"

# Optional auto-pull of models before launch.
OLLAMA_PULL_MODELS="${OLLAMA_PULL_MODELS:-0}"

if [[ "${1:-}" == "--" ]]; then
  shift
fi
RUN_CMD=("$@")
if [[ ${#RUN_CMD[@]} -eq 0 ]]; then
  RUN_CMD=(python app.py)
fi

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
echo
printf 'Running: %s\n' "${RUN_CMD[*]}"

"${RUN_CMD[@]}"
