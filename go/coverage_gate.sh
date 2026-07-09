#!/bin/sh
# Enforces a minimum Go statement-coverage floor, the Go-native successor to the deleted
# migration/tools/coverage_gate.py (Python-era floor: migration/COVERAGE_FLOOR, 84.0 against
# app.py's oracle coverage — not a like-for-like number, since it measured a different program).
# This floor is ratchet-style, same spirit as the old one: committed in COVERAGE_FLOOR next to
# this script, meant to be raised as real coverage grows, never lowered to paper over a
# regression.
#
# Usage: coverage_gate.sh <GOCOVERDIR>
# GOCOVERDIR must already contain merged counter data from BOTH `go test -cover ./...` (unit
# tests) and the golden-corpus replay (goldenreplay, built+run with SOBS_GOCOVER=1) — see
# Dockerfile.ci, which points both at the same directory so this measures their union, not
# either alone.
#
# Generated protobuf code (internal/otlp/genpb/...) is excluded from both the numerator and
# denominator: it's vendored-shape code no one hand-writes or meaningfully unit-tests, and
# counting it would let real, hand-written coverage regress while the aggregate number holds
# steady (it did exactly this in the pre-cutover Go coverage snapshots — see
# migration/GO_COVERAGE.md's "All Go" vs "hand-written Go" split).
set -eu

covdir="${1:?usage: coverage_gate.sh <GOCOVERDIR>}"
floor_file="$(dirname "$0")/COVERAGE_FLOOR"
floor="$(cat "$floor_file")"

merged="$(mktemp)"
trap 'rm -f "$merged"' EXIT

go tool covdata textfmt -i="$covdir" -o="$merged"

actual="$(awk '
  NR == 1 { next }  # skip the "mode: atomic" header line
  {
    file = $1
    sub(/:.*/, "", file)
    numStmt = $2
    count = $3
    if (file ~ /internal\/otlp\/genpb\//) next
    total += numStmt
    if (count + 0 > 0) covered += numStmt
  }
  END {
    if (total == 0) { print "0.00"; exit 1 }
    printf "%.2f", (covered / total) * 100
  }
' "$merged")"

echo "Go coverage (hand-written, excl. generated OTLP protobuf): ${actual}% (floor: ${floor}%)"

if ! awk -v a="$actual" -v f="$floor" 'BEGIN { exit !(a >= f) }'; then
  echo "::error::Go coverage ${actual}% is below the required floor of ${floor}% (go/COVERAGE_FLOOR)"
  exit 1
fi

awk -v a="$actual" -v f="$floor" 'BEGIN {
  if (a - f > 5) print "notice: coverage is " a "%, comfortably above the " f "% floor in go/COVERAGE_FLOOR - consider raising it to lock in the gain"
}'
