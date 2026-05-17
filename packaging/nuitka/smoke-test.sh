#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SOBS_BUILD_MODE="${SOBS_BUILD_MODE:-standalone}"
SOBS_TEST_HOST="${SOBS_TEST_HOST:-127.0.0.1}"
SOBS_TEST_PORT="${SOBS_TEST_PORT:-8765}"
SOBS_TEST_TIMEOUT_SECONDS="${SOBS_TEST_TIMEOUT_SECONDS:-20}"
SOBS_TEST_EXECUTABLE="${SOBS_TEST_EXECUTABLE:-}"
SOBS_TEST_CONSOLE_TELEMETRY="${SOBS_TEST_CONSOLE_TELEMETRY:-false}"

if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: curl is required for smoke testing." >&2
  exit 1
fi

if [[ -z "${SOBS_TEST_EXECUTABLE}" ]]; then
  if [[ "${SOBS_BUILD_MODE}" == "standalone" ]]; then
    if [[ -x "${REPO_ROOT}/dist/sobs.dist/sobs" ]]; then
      SOBS_TEST_EXECUTABLE="${REPO_ROOT}/dist/sobs.dist/sobs"
    else
      SOBS_TEST_EXECUTABLE="${REPO_ROOT}/dist/app.dist/sobs"
    fi
  elif [[ "${SOBS_BUILD_MODE}" == "onefile" ]]; then
    SOBS_TEST_EXECUTABLE="${REPO_ROOT}/dist/sobs"
  else
    echo "ERROR: Unsupported SOBS_BUILD_MODE='${SOBS_BUILD_MODE}'." >&2
    exit 1
  fi
fi

if [[ ! -x "${SOBS_TEST_EXECUTABLE}" ]]; then
  echo "ERROR: Packaged executable not found or not executable: ${SOBS_TEST_EXECUTABLE}" >&2
  echo "Build first with ./packaging/nuitka/build-local.sh" >&2
  exit 1
fi

TMP_ROOT="$(mktemp -d)"
PID=""

cleanup() {
  if [[ -n "${PID}" ]] && kill -0 "${PID}" 2>/dev/null; then
    kill "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT

wait_for_health() {
  local health_url="$1"
  local log_file="$2"
  local start_ts now elapsed
  start_ts="$(date +%s)"

  while true; do
    if curl -fsS "${health_url}" >/dev/null 2>&1; then
      return 0
    fi

    if ! kill -0 "${PID}" 2>/dev/null; then
      echo "ERROR: Process exited before becoming healthy." >&2
      cat "${log_file}" >&2
      return 1
    fi

    now="$(date +%s)"
    elapsed="$((now - start_ts))"
    if (( elapsed >= SOBS_TEST_TIMEOUT_SECONDS )); then
      echo "ERROR: Timed out waiting for ${health_url}" >&2
      cat "${log_file}" >&2
      return 1
    fi
    sleep 1
  done
}

run_case() {
  local case_name="$1"
  local telemetry_enabled="$2"
  local otel_sdk_disabled="$3"
  local telemetry_exporter="$4"
  local telemetry_console_export="$5"
  local case_root="${TMP_ROOT}/${case_name}"
  local case_data="${case_root}/data"
  local case_log="${case_root}/sobs.log"
  local health_url="http://${SOBS_TEST_HOST}:${SOBS_TEST_PORT}/health"

  mkdir -p "${case_data}"
  echo "Running smoke case: ${case_name}"
  echo "  executable: ${SOBS_TEST_EXECUTABLE}"
  echo "  health: ${health_url}"
  echo "  telemetry: enabled=${telemetry_enabled} exporter=${telemetry_exporter:-none} OTEL_SDK_DISABLED=${otel_sdk_disabled}"

  SOBS_DATA_DIR="${case_data}" \
  HYPERCORN_BIND="${SOBS_TEST_HOST}:${SOBS_TEST_PORT}" \
  PORT="${SOBS_TEST_PORT}" \
  SOBS_TELEMETRY_ENABLED="${telemetry_enabled}" \
  OTEL_SDK_DISABLED="${otel_sdk_disabled}" \
  SOBS_TELEMETRY_EXPORTER="${telemetry_exporter}" \
  SOBS_TELEMETRY_CONSOLE_EXPORT="${telemetry_console_export}" \
  "${SOBS_TEST_EXECUTABLE}" >"${case_log}" 2>&1 &
  PID="$!"

  wait_for_health "${health_url}" "${case_log}"
  sleep 2

  if ! kill -0 "${PID}" 2>/dev/null; then
    echo "ERROR: Process died after startup in case '${case_name}'." >&2
    cat "${case_log}" >&2
    return 1
  fi

  kill "${PID}" 2>/dev/null || true
  wait "${PID}" 2>/dev/null || true
  PID=""
  echo "Smoke case passed: ${case_name}"
}

run_case "telemetry_disabled" "false" "false" "none" "false"
run_case "otel_sdk_disabled_override" "true" "true" "none" "false"

if [[ "${SOBS_TEST_CONSOLE_TELEMETRY}" == "true" ]]; then
  run_case "console_telemetry" "true" "false" "console" "true"
else
  echo "Skipping optional console telemetry case (set SOBS_TEST_CONSOLE_TELEMETRY=true to run it)."
fi

echo "All smoke-test cases passed."
