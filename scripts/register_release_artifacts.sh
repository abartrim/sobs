#!/usr/bin/env bash
set -euo pipefail

# Thin CI-friendly wrapper around scripts/register_release_artifacts.py.
# Keeps one implementation source while allowing bash-native invocation.
#
# All arguments are forwarded verbatim to the Python script.
# New flags (as of this update):
#   --requirements-file PATH  Python requirements.txt / pip-freeze format
#   --dependencies-json JSON  Any-language dep list: '[{"package":"x","version":"1","ecosystem":"PyPI"}]'
#   --dependencies-file PATH  Same as above but from a file
#   --dependencies-name NAME  Label for the source (defaults to "lockfile")
#
# Environment variable equivalents:
#   SOBS_RELEASE_DEPENDENCIES_JSON / SOBS_RELEASE_DEPENDENCIES_JSON_FILE
#   SOBS_RELEASE_REQUIREMENTS_FILE / SOBS_RELEASE_DEPENDENCIES_NAME

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
