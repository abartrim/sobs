#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SOBS_BUILD_MODE="${SOBS_BUILD_MODE:-standalone}"
SOBS_BUILD_CLEAN="${SOBS_BUILD_CLEAN:-false}"
SOBS_BUILD_OUTPUT_DIR="${SOBS_BUILD_OUTPUT_DIR:-dist}"
SOBS_BUILD_EXTRA_ARGS="${SOBS_BUILD_EXTRA_ARGS:-}"
SOBS_TELEMETRY_ENABLED="${SOBS_TELEMETRY_ENABLED:-false}"

if [[ ! -f "${REPO_ROOT}/app.py" ]]; then
  echo "ERROR: app.py was not found. Run this script from the Sobs repository." >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required." >&2
  exit 1
fi

if ! python3 -m nuitka --version >/dev/null 2>&1; then
  echo "ERROR: Nuitka is not installed for python3." >&2
  echo "Install with: python3 -m pip install nuitka" >&2
  exit 1
fi

CHDB_PACKAGE_DIR="$(python3 - <<'PY'
import importlib.util
import pathlib
import sys

spec = importlib.util.find_spec("chdb")
if spec is None or spec.origin is None:
    print("ERROR: chdb is not installed for the current interpreter.", file=sys.stderr)
    raise SystemExit(1)
print(pathlib.Path(spec.origin).resolve().parent)
PY
)"
PYTHON_MM="$(python3 - <<'PY'
import sys

print(f"{sys.version_info.major}.{sys.version_info.minor}")
PY
)"
CHDB_RUNTIME_LIB="${CHDB_PACKAGE_DIR}/libpybind11nonlimitedapi_chdb_${PYTHON_MM}.so"

if [[ ! -f "${CHDB_RUNTIME_LIB}" ]]; then
  echo "ERROR: Expected chDB runtime library not found: ${CHDB_RUNTIME_LIB}" >&2
  exit 1
fi

case "${SOBS_BUILD_MODE}" in
  standalone) mode_flag="--standalone" ;;
  onefile) mode_flag="--onefile" ;;
  *)
    echo "ERROR: Unsupported SOBS_BUILD_MODE='${SOBS_BUILD_MODE}'. Use standalone or onefile." >&2
    exit 1
    ;;
esac

if [[ "${SOBS_BUILD_OUTPUT_DIR}" = /* ]]; then
  OUTPUT_DIR="${SOBS_BUILD_OUTPUT_DIR}"
else
  OUTPUT_DIR="${REPO_ROOT}/${SOBS_BUILD_OUTPUT_DIR}"
fi

mkdir -p "${OUTPUT_DIR}"

if [[ "${SOBS_BUILD_CLEAN}" == "true" ]]; then
  echo "Cleaning scoped Nuitka outputs from ${OUTPUT_DIR}"
  rm -rf \
    "${OUTPUT_DIR}/sobs.build" \
    "${OUTPUT_DIR}/sobs.dist" \
    "${OUTPUT_DIR}/sobs.onefile-build" \
    "${OUTPUT_DIR}/sobs" \
    "${OUTPUT_DIR}/app.build" \
    "${OUTPUT_DIR}/app.dist" \
    "${OUTPUT_DIR}/app.onefile-build" \
    "${OUTPUT_DIR}/app"
fi

extra_args=()
if [[ -n "${SOBS_BUILD_EXTRA_ARGS}" ]]; then
  read -r -a extra_args <<< "${SOBS_BUILD_EXTRA_ARGS}"
fi

echo "Building Sobs with Nuitka"
echo "  repo root: ${REPO_ROOT}"
echo "  mode: ${SOBS_BUILD_MODE}"
echo "  output dir: ${OUTPUT_DIR}"
echo "  telemetry default (runtime env): SOBS_TELEMETRY_ENABLED=${SOBS_TELEMETRY_ENABLED}"

cmd=(
  python3 -m nuitka
  "${mode_flag}"
  --follow-imports
  --assume-yes-for-downloads
  --output-dir="${OUTPUT_DIR}"
  --output-filename=sobs
  --include-package=telemetry
  --include-data-files="${CHDB_RUNTIME_LIB}=chdb/$(basename "${CHDB_RUNTIME_LIB}")"
  --include-data-dir="${REPO_ROOT}/templates=templates"
  --include-data-dir="${REPO_ROOT}/static=static"
  "${extra_args[@]}"
  "${REPO_ROOT}/app.py"
)

printf 'Running:'
printf ' %q' "${cmd[@]}"
printf '\n'
"${cmd[@]}"

if [[ "${SOBS_BUILD_MODE}" == "standalone" ]]; then
  if [[ -x "${OUTPUT_DIR}/sobs.dist/sobs" ]]; then
    executable_path="${OUTPUT_DIR}/sobs.dist/sobs"
  else
    executable_path="${OUTPUT_DIR}/app.dist/sobs"
  fi
else
  executable_path="${OUTPUT_DIR}/sobs"
fi

if [[ ! -x "${executable_path}" ]]; then
  echo "ERROR: Build completed but executable not found at ${executable_path}" >&2
  exit 1
fi

echo "Build complete."
echo "Executable: ${executable_path}"
