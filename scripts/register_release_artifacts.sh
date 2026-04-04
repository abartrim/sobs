#!/usr/bin/env bash
set -euo pipefail

# Thin CI-friendly wrapper around scripts/register_release_artifacts.py.
# Keeps one implementation source while allowing bash-native invocation.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PY_HELPER="${SCRIPT_DIR}/register_release_artifacts.py"

PYTHON_BIN="${PYTHON_BIN:-python3}"
if ! command -v "${PYTHON_BIN}" >/dev/null 2>&1; then
  if command -v python >/dev/null 2>&1; then
    PYTHON_BIN="python"
  else
    echo "ERROR: python3 (or python) is required to run ${PY_HELPER}" >&2
    exit 127
  fi
fi

exec "${PYTHON_BIN}" "${PY_HELPER}" "$@"
