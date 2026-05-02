#!/usr/bin/env bash

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if [[ -d "$repo_root/.venv/bin" ]]; then
  export PATH="$repo_root/.venv/bin:$PATH"
fi

if ! command -v pytest >/dev/null 2>&1; then
  echo "check_unit_tests: pytest is required." >&2
  exit 1
fi

echo "check_unit_tests: running unit tests with coverage"
pytest tests --ignore=tests/test_integration.py -x --maxfail=1 -vv \
  --cov=app --cov=masking --cov=mcp \
  --cov-report=term-missing \
  --cov-report=xml:coverage.xml

if command -v diff-cover >/dev/null 2>&1 && git rev-parse --verify origin/main >/dev/null 2>&1; then
  echo "check_unit_tests: running diff-cover against origin/main"
  diff-cover coverage.xml --compare-branch=origin/main --fail-under=90
fi