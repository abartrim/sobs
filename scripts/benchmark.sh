#!/usr/bin/env bash
# benchmark.sh — Run load scenarios against a running SOBS instance.
#
# Usage:
#   ./scripts/benchmark.sh [BASE_URL]
#
# Examples:
#   ./scripts/benchmark.sh                         # default: http://127.0.0.1:4317
#   ./scripts/benchmark.sh http://127.0.0.1:44318  # custom port
#
# The script runs three scenarios and prints a summary table:
#   1. Light   —  420 requests, 14 workers
#   2. Default —  420 requests, 28 workers
#   3. Heavy   — 1260 requests, 56 workers

set -euo pipefail

BASE="${1:-http://127.0.0.1:4317}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXAMPLE="$SCRIPT_DIR/load_example.py"
PYTHON="${PYTHON:-python}"

# Verify the target is up before starting
if ! curl -sf "$BASE/health" > /dev/null; then
  echo "ERROR: SOBS is not reachable at $BASE/health — start the app first."
  exit 1
fi

echo "=== SOBS benchmark against $BASE ==="
echo

run_scenario() {
  local label="$1"
  local total="$2"
  local workers="$3"

  echo "--- $label (total=$total workers=$workers) ---"
  "$PYTHON" "$EXAMPLE" --base "$BASE" --total "$total" --workers "$workers"
  echo
}

run_scenario "light"   420  14
run_scenario "default" 420  28
run_scenario "heavy"   1260 56

echo "=== done ==="
