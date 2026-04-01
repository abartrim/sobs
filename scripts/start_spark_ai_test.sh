#!/usr/bin/env bash
# start_spark_ai_test.sh — Start local SOBS against Spark-cluster AI services.
#
# This script:
# 1) Starts kubectl port-forwards for LLM, embeddings, and DLP services.
# 2) Reads the DLP shared secret from a Kubernetes Secret.
# 3) Exports SOBS AI/Guard/DLP env vars and runs SOBS (or a custom command).
#
# Usage:
#   ./scripts/start_spark_ai_test.sh
#   ./scripts/start_spark_ai_test.sh -- python app.py
#   ./scripts/start_spark_ai_test.sh -- .venv/bin/python app.py

set -euo pipefail

KUBECONFIG_PATH="${KUBECONFIG_PATH:-$HOME/.kube/microk8s.config}"
KUBE_NAMESPACE="${KUBE_NAMESPACE:-default}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-$KUBE_NAMESPACE}"

# Service/resource names.
LLM_RESOURCE="${LLM_RESOURCE:-svc/vllm-llm}"
EMBED_RESOURCE="${EMBED_RESOURCE:-svc/vllm-embeddings}"
DLP_RESOURCE="${DLP_RESOURCE:-svc/dlp}"

# Remote service ports in cluster.
LLM_REMOTE_PORT="${LLM_REMOTE_PORT:-8000}"
EMBED_REMOTE_PORT="${EMBED_REMOTE_PORT:-8000}"
DLP_REMOTE_PORT="${DLP_REMOTE_PORT:-8080}"

# Local ports for port-forward.
LLM_LOCAL_PORT="${LLM_LOCAL_PORT:-18000}"
EMBED_LOCAL_PORT="${EMBED_LOCAL_PORT:-18001}"
DLP_LOCAL_PORT="${DLP_LOCAL_PORT:-18002}"

# Model names served by the LLM vLLM instance.
SOBS_AI_MODEL="${SOBS_AI_MODEL:-gpt-oss-120b-base}"
SOBS_AI_GUARD_MODEL="${SOBS_AI_GUARD_MODEL:-gpt-oss-120b-guard-lora}"

# DLP secret source.
DLP_SECRET_NAME="${DLP_SECRET_NAME:-infra-secrets}"
DLP_SECRET_KEY="${DLP_SECRET_KEY:-dlp-shared-secret}"
REQUIRE_DLP_SECRET="${REQUIRE_DLP_SECRET:-1}"

# Optional explicit override for the token sent as Bearer to AI/Guard/DLP.
# If empty, script falls back to DLP secret.
SOBS_AI_API_KEY="${SOBS_AI_API_KEY:-}"

# Command to run after setup.
if [[ "${1:-}" == "--" ]]; then
  shift
fi
RUN_CMD=("$@")
if [[ ${#RUN_CMD[@]} -eq 0 ]]; then
  RUN_CMD=(python app.py)
fi

PF_PIDS=()
PF_LOG_DIR="${PF_LOG_DIR:-/tmp/sobs-port-forward}"
mkdir -p "$PF_LOG_DIR"

cleanup() {
  for pid in "${PF_PIDS[@]:-}"; do
    if kill -0 "$pid" >/dev/null 2>&1; then
      kill "$pid" >/dev/null 2>&1 || true
      wait "$pid" >/dev/null 2>&1 || true
    fi
  done
}
trap cleanup EXIT INT TERM

start_port_forward() {
  local name="$1"
  local resource="$2"
  local local_port="$3"
  local remote_port="$4"
  local log_file="$PF_LOG_DIR/${name}.log"

  echo "[start] $name: $resource $local_port:$remote_port"
  kubectl --kubeconfig "$KUBECONFIG_PATH" -n "$KUBE_NAMESPACE" \
    port-forward "$resource" "$local_port:$remote_port" >"$log_file" 2>&1 &
  local pf_pid=$!
  PF_PIDS+=("$pf_pid")

  # Wait briefly to ensure the forward is live.
  local i
  for i in $(seq 1 30); do
    if nc -z 127.0.0.1 "$local_port" >/dev/null 2>&1; then
      echo "[ok] $name available on 127.0.0.1:$local_port"
      return 0
    fi
    if ! kill -0 "$pf_pid" >/dev/null 2>&1; then
      echo "[error] port-forward for $name exited early. Log: $log_file" >&2
      tail -n 40 "$log_file" >&2 || true
      return 1
    fi
    sleep 0.2
  done

  echo "[error] timed out waiting for $name on 127.0.0.1:$local_port" >&2
  tail -n 40 "$log_file" >&2 || true
  return 1
}

load_dlp_secret() {
  local encoded
  encoded="$({
    kubectl --kubeconfig "$KUBECONFIG_PATH" -n "$INFRA_NAMESPACE" \
      get secret "$DLP_SECRET_NAME" -o "jsonpath={.data.${DLP_SECRET_KEY}}"
  } 2>/dev/null || true)"

  if [[ -z "$encoded" ]]; then
    if [[ "$REQUIRE_DLP_SECRET" == "1" ]]; then
      echo "[error] missing secret ${INFRA_NAMESPACE}/${DLP_SECRET_NAME} key ${DLP_SECRET_KEY}" >&2
      return 1
    fi
    return 0
  fi

  local decoded
  decoded="$(printf '%s' "$encoded" | base64 --decode 2>/dev/null || printf '%s' "$encoded" | base64 -D 2>/dev/null || true)"
  if [[ -z "$decoded" && "$REQUIRE_DLP_SECRET" == "1" ]]; then
    echo "[error] unable to decode ${INFRA_NAMESPACE}/${DLP_SECRET_NAME} key ${DLP_SECRET_KEY}" >&2
    return 1
  fi

  if [[ -z "$SOBS_AI_API_KEY" ]]; then
    SOBS_AI_API_KEY="$decoded"
  fi
  return 0
}

start_port_forward "llm" "$LLM_RESOURCE" "$LLM_LOCAL_PORT" "$LLM_REMOTE_PORT"
start_port_forward "embeddings" "$EMBED_RESOURCE" "$EMBED_LOCAL_PORT" "$EMBED_REMOTE_PORT"
start_port_forward "dlp" "$DLP_RESOURCE" "$DLP_LOCAL_PORT" "$DLP_REMOTE_PORT"
load_dlp_secret

export SOBS_AI_ENDPOINT_URL="http://127.0.0.1:${LLM_LOCAL_PORT}/v1"
export SOBS_AI_GUARD_ENDPOINT_URL="http://127.0.0.1:${LLM_LOCAL_PORT}/v1"
export SOBS_AI_DLP_ENDPOINT_URL="http://127.0.0.1:${DLP_LOCAL_PORT}/v1"
export SOBS_AI_MODEL
export SOBS_AI_GUARD_MODEL

if [[ -n "$SOBS_AI_API_KEY" ]]; then
  export SOBS_AI_API_KEY
fi

echo
echo "Configured local endpoints:"
echo "  SOBS_AI_ENDPOINT_URL=$SOBS_AI_ENDPOINT_URL"
echo "  SOBS_AI_GUARD_ENDPOINT_URL=$SOBS_AI_GUARD_ENDPOINT_URL"
echo "  SOBS_AI_DLP_ENDPOINT_URL=$SOBS_AI_DLP_ENDPOINT_URL"
echo "  SOBS_AI_MODEL=$SOBS_AI_MODEL"
echo "  SOBS_AI_GUARD_MODEL=$SOBS_AI_GUARD_MODEL"
if [[ -n "${SOBS_AI_API_KEY:-}" ]]; then
  echo "  SOBS_AI_API_KEY=<set from secret/override>"
else
  echo "  SOBS_AI_API_KEY=<empty>"
fi
echo ""
echo "Embeddings proxy available at: http://127.0.0.1:${EMBED_LOCAL_PORT}/v1"
echo "(SOBS does not currently use embeddings endpoint directly.)"
echo
echo "Running: ${RUN_CMD[*]}"

"${RUN_CMD[@]}"
