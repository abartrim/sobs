#!/usr/bin/env bash
set -euo pipefail

# Profile local Sobs process resource usage into a CSV while you interact with the app.
# macOS-focused (uses ps + vm_stat).
#
# Usage:
#   ./scripts/profile_sobs_resources.sh
#   ./scripts/profile_sobs_resources.sh --duration 300 --interval 1 --wait 120
#   ./scripts/profile_sobs_resources.sh --pid 12345
#   ./scripts/profile_sobs_resources.sh --regex "python.*app.py"

DURATION_SEC=300
INTERVAL_SEC=1
WAIT_SEC=120
PID=""
PROCESS_REGEX="(^|[[:space:]])(([^[:space:]]*/)?app\.py)([[:space:]]|$)"
OUT_DIR="data/profiles"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --duration)
      DURATION_SEC="$2"
      shift 2
      ;;
    --interval)
      INTERVAL_SEC="$2"
      shift 2
      ;;
    --wait)
      WAIT_SEC="$2"
      shift 2
      ;;
    --pid)
      PID="$2"
      shift 2
      ;;
    --regex)
      PROCESS_REGEX="$2"
      shift 2
      ;;
    --out-dir)
      OUT_DIR="$2"
      shift 2
      ;;
    *)
      echo "Unknown arg: $1" >&2
      exit 1
      ;;
  esac
done

mkdir -p "$OUT_DIR"
TS="$(date +%Y%m%d-%H%M%S)"
CSV="$OUT_DIR/sobs-resource-profile-$TS.csv"

find_pid() {
  ps -axo pid=,command= | awk -v re="$PROCESS_REGEX" '
    {
      pid=$1
      $1=""
      sub(/^ +/, "", $0)
      cmd=$0
      if (cmd ~ /rum_replay_test_app\.py|tests\/test_app\.py/) {
        next
      }
      if (cmd ~ re) {
        print pid
        exit
      }
    }
  ' || true
}

if [[ -z "$PID" ]]; then
  echo "[info] waiting up to ${WAIT_SEC}s for process matching: $PROCESS_REGEX"
  deadline=$(( $(date +%s) + WAIT_SEC ))
  while [[ $(date +%s) -lt $deadline ]]; do
    PID="$(find_pid)"
    if [[ -n "$PID" ]]; then
      break
    fi
    sleep 1
  done
fi

if [[ -z "$PID" ]]; then
  echo "[error] no matching process found" >&2
  exit 1
fi

if ! ps -p "$PID" >/dev/null 2>&1; then
  echo "[error] PID $PID is not running" >&2
  exit 1
fi

# Capture command line for traceability.
CMDLINE="$(ps -p "$PID" -o command= | sed -e 's/^ *//')"

echo "[ok] profiling PID=$PID"
echo "[info] cmd: $CMDLINE"
echo "[info] writing: $CSV"

echo "timestamp,elapsed_sec,pid,cpu_pct,mem_pct,rss_mb,vsz_mb,threads,sys_used_mb,sys_free_mb" > "$CSV"

start_ts=$(date +%s)
end_ts=$(( start_ts + DURATION_SEC ))

while [[ $(date +%s) -lt $end_ts ]]; do
  if ! ps -p "$PID" >/dev/null 2>&1; then
    echo "[warn] process $PID exited early"
    break
  fi

  now_iso="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  now_s=$(date +%s)
  elapsed=$(( now_s - start_ts ))

  # rss/vsz are KB on macOS ps.
  read -r cpu mem rss_kb vsz_kb < <(ps -p "$PID" -o %cpu=,%mem=,rss=,vsz=)

  # macOS: count per-thread lines and subtract header.
  thcount=$(ps -M -p "$PID" | wc -l | awk '{print $1}')
  if [[ "$thcount" -gt 0 ]]; then
    thcount=$(( thcount - 1 ))
  fi

  rss_mb=$(awk -v v="$rss_kb" 'BEGIN { printf "%.2f", v/1024 }')
  vsz_mb=$(awk -v v="$vsz_kb" 'BEGIN { printf "%.2f", v/1024 }')

  # vm_stat is pages; convert to MB using page size from first line.
  vm_line="$(vm_stat | head -n 1)"
  page_size=$(echo "$vm_line" | awk -F'page size of ' '{print $2}' | awk '{print $1}')
  if [[ -z "$page_size" ]]; then
    page_size=4096
  fi
  free_pages=$(vm_stat | awk '/Pages free/ {gsub("\\.","",$3); print $3}')
  inactive_pages=$(vm_stat | awk '/Pages inactive/ {gsub("\\.","",$3); print $3}')
  speculative_pages=$(vm_stat | awk '/Pages speculative/ {gsub("\\.","",$3); print $3}')
  active_pages=$(vm_stat | awk '/Pages active/ {gsub("\\.","",$3); print $3}')
  wired_pages=$(vm_stat | awk '/Pages wired down/ {gsub("\\.","",$4); print $4}')
  compressed_pages=$(vm_stat | awk '/Pages occupied by compressor/ {gsub("\\.","",$5); print $5}')

  free_total_pages=$(( free_pages + inactive_pages + speculative_pages ))
  used_total_pages=$(( active_pages + wired_pages + compressed_pages ))
  sys_free_mb=$(awk -v p="$free_total_pages" -v sz="$page_size" 'BEGIN { printf "%.2f", (p*sz)/(1024*1024) }')
  sys_used_mb=$(awk -v p="$used_total_pages" -v sz="$page_size" 'BEGIN { printf "%.2f", (p*sz)/(1024*1024) }')

  echo "$now_iso,$elapsed,$PID,$cpu,$mem,$rss_mb,$vsz_mb,$thcount,$sys_used_mb,$sys_free_mb" >> "$CSV"

  sleep "$INTERVAL_SEC"
done

echo ""
echo "[summary]"
awk -F',' '
NR==2 { min_rss=$6; max_rss=$6; sum_rss=0; min_cpu=$4; max_cpu=$4; sum_cpu=0; n=0 }
NR>1 {
  if ($6 < min_rss) min_rss=$6
  if ($6 > max_rss) max_rss=$6
  if ($4 < min_cpu) min_cpu=$4
  if ($4 > max_cpu) max_cpu=$4
  sum_rss += $6
  sum_cpu += $4
  n += 1
}
END {
  if (n == 0) {
    print "no samples recorded"
    exit
  }
  printf "samples: %d\n", n
  printf "rss_mb: avg=%.2f min=%.2f max=%.2f\n", sum_rss/n, min_rss, max_rss
  printf "cpu_pct: avg=%.2f min=%.2f max=%.2f\n", sum_cpu/n, min_cpu, max_cpu
}
' "$CSV"

echo "[done] profile saved to $CSV"
