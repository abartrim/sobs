#!/usr/bin/env bash
set -euo pipefail

# End-to-end validation runner for SOBS enrichment + telemetry fixtures.
# This script assumes fixture repos already exist in GitHub.

ORG=""
PREFIX="sobs-validation"
BASE_URL="http://127.0.0.1:44317"
API_KEY=""
PYTHON_BIN="python3"
SOURCE_MAP_DIR="${SOBS_SOURCE_MAP_DIR:-}"
SKIP_REPO_SETUP=0
SKIP_REGISTRATION=0
SKIP_TELEMETRY=0
SKIP_AGENT_ISSUE_CHECK=0
SKIP_REPO_HEALTH_CHECK=0
DRY_RUN=0
ASSUME_YES=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_SCRIPT="$SCRIPT_DIR/setup_repo_connect_validation.sh"
REGISTER_SCRIPT="$SCRIPT_DIR/register_validation_fixtures_to_sobs.sh"
TELEMETRY_SCRIPT="$SCRIPT_DIR/generate_validation_telemetry.py"
AGENT_FLOW_SCRIPT="$SCRIPT_DIR/validate_agent_rule_issue_flows.py"

usage() {
  cat <<'USAGE'
Usage:
  run_validation_e2e.sh --org <github-org-or-user> [options]

Required:
  --org <name>               GitHub org/user for fixture repos.

Options:
  --prefix <value>           Repo prefix (default: sobs-validation)
  --base-url <url>           SOBS base URL (default: http://127.0.0.1:44317)
  --api-key <key>            SOBS API key (optional)
  --python-bin <path>        Python interpreter (default: python3)
  --source-map-dir <path>    Optional local map directory for remap fixture files
                             (defaults to SOBS_SOURCE_MAP_DIR env var when set)
  --skip-repo-setup          Skip setup_repo_connect_validation.sh
  --skip-registration        Skip register_validation_fixtures_to_sobs.sh
  --skip-telemetry           Skip generate_validation_telemetry.py
  --skip-agent-issue-check   Skip anomaly-triggered agent issue flow validation step
  --skip-repo-health-check   Skip /api/enrichment/github/repo-health validation step
  --dry-run                  Print intended execution only
  --yes                      Skip confirmation prompt
  -h, --help                 Show help
USAGE
}

log() {
  printf '[INFO] %s\n' "$*"
}

die() {
  printf '[ERROR] %s\n' "$*" >&2
  exit 1
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --org)
        ORG="${2:-}"
        shift 2
        ;;
      --prefix)
        PREFIX="${2:-}"
        shift 2
        ;;
      --base-url)
        BASE_URL="${2:-}"
        shift 2
        ;;
      --api-key)
        API_KEY="${2:-}"
        shift 2
        ;;
      --python-bin)
        PYTHON_BIN="${2:-}"
        shift 2
        ;;
      --source-map-dir)
        SOURCE_MAP_DIR="${2:-}"
        shift 2
        ;;
      --skip-repo-setup)
        SKIP_REPO_SETUP=1
        shift
        ;;
      --skip-registration)
        SKIP_REGISTRATION=1
        shift
        ;;
      --skip-telemetry)
        SKIP_TELEMETRY=1
        shift
        ;;
      --skip-agent-issue-check)
        SKIP_AGENT_ISSUE_CHECK=1
        shift
        ;;
      --skip-repo-health-check)
        SKIP_REPO_HEALTH_CHECK=1
        shift
        ;;
      --dry-run)
        DRY_RUN=1
        shift
        ;;
      --yes)
        ASSUME_YES=1
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "Unknown argument: $1"
        ;;
    esac
  done

  [[ -n "$ORG" ]] || die "--org is required"
}

confirm_execution() {
  if [[ "$ASSUME_YES" -eq 1 ]] || [[ "$DRY_RUN" -eq 1 ]]; then
    return 0
  fi

  printf '\n'
  printf 'This run may create/update GitHub repos, write SOBS app/release/artifact rows, and emit telemetry events.\n'
  if [[ -n "$SOURCE_MAP_DIR" ]]; then
    printf 'Source-map fixture directory: %s\n' "$SOURCE_MAP_DIR"
  fi
  read -r -p "Continue? [y/N]: " response
  case "${response:-}" in
    y|Y|yes|YES)
      return 0
      ;;
    *)
      die "Cancelled by user"
      ;;
  esac
}

run_cmd() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf '[DRYRUN]'
    for arg in "$@"; do
      printf ' %q' "$arg"
    done
    printf '\n'
  else
    "$@"
  fi
}

post_cve_scan() {
  local cmd=(curl -sS -X POST "$BASE_URL/api/enrichment/cve/scan" -H "Content-Type: application/json" -d '{}')
  if [[ -n "$API_KEY" ]]; then
    cmd+=( -H "X-API-Key: $API_KEY" )
  fi
  run_cmd "${cmd[@]}"
}

validate_repo_health() {
  local tmp_json
  tmp_json="$(mktemp)"

  local curl_cmd=(curl -sS "$BASE_URL/api/enrichment/github/repo-health")
  if ! run_cmd "${curl_cmd[@]}" >"$tmp_json"; then
    rm -f "$tmp_json"
    die "repo-health request failed"
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    rm -f "$tmp_json"
    return 0
  fi

  local py=$'import json,sys\n'
  py+=$'d=json.load(open(sys.argv[1], encoding="utf-8"))\n'
  py+=$'ok=bool(d.get("ok"))\n'
  py+=$'err=str(d.get("error") or "")\n'
  py+=$'repos=d.get("repos") if isinstance(d.get("repos"), list) else []\n'
  py+=$'prs=int(d.get("open_prs", 0) or 0)\n'
  py+=$'issues=int(d.get("open_issues", 0) or 0)\n'
  py+=$'print(f"[INFO] Repo health summary: ok={ok} repos={len(repos)} issues={issues} prs={prs} error={err}")\n'
  py+=$'sys.exit(0 if (ok and len(repos)>0 and (issues+prs)>0) else 2)\n'

  if ! "$PYTHON_BIN" -c "$py" "$tmp_json"; then
    rm -f "$tmp_json"
    die "repo-health validation failed (ensure ai.github_token is configured and version-scoped fixture issues/PRs exist)"
  fi
  rm -f "$tmp_json"
}

main() {
  parse_args "$@"
  confirm_execution

  if [[ "$SKIP_REPO_SETUP" -eq 0 ]]; then
    log "Step 1/5: setup fixture repos"
    local setup_cmd=("$SETUP_SCRIPT" --org "$ORG" --prefix "$PREFIX" --yes)
    if [[ "$DRY_RUN" -eq 1 ]]; then
      setup_cmd+=(--dry-run)
    fi
    run_cmd "${setup_cmd[@]}"
  fi

  if [[ "$SKIP_REGISTRATION" -eq 0 ]]; then
    log "Step 2/5: register fixture releases/artifacts in SOBS"
    local register_cmd=(
      "$REGISTER_SCRIPT"
      --org "$ORG"
      --prefix "$PREFIX"
      --base-url "$BASE_URL"
      --python-bin "$PYTHON_BIN"
      --yes
    )
    if [[ -n "$API_KEY" ]]; then
      register_cmd+=(--api-key "$API_KEY")
    fi
    if [[ -n "$SOURCE_MAP_DIR" ]]; then
      register_cmd+=(--source-map-storage-prefix "file://$SOURCE_MAP_DIR")
    fi
    if [[ "$DRY_RUN" -eq 1 ]]; then
      register_cmd+=(--dry-run)
    fi
    run_cmd "${register_cmd[@]}"
  fi

  if [[ "$SKIP_TELEMETRY" -eq 0 ]]; then
    log "Step 3/6: emit correlated OTEL + RUM telemetry"
    local telemetry_cmd=(
      "$PYTHON_BIN"
      "$TELEMETRY_SCRIPT"
      --org "$ORG"
      --prefix "$PREFIX"
      --base-url "$BASE_URL"
    )
    if [[ -n "$API_KEY" ]]; then
      telemetry_cmd+=(--api-key "$API_KEY")
    fi
    if [[ -n "$SOURCE_MAP_DIR" ]]; then
      telemetry_cmd+=(--source-map-dir "$SOURCE_MAP_DIR")
    fi
    run_cmd "${telemetry_cmd[@]}"
  fi

  if [[ "$SKIP_AGENT_ISSUE_CHECK" -eq 0 ]]; then
    log "Step 4/6: validate anomaly-triggered GitHub issue flows + SOBS work-items visibility"
    local agent_cmd=(
      "$PYTHON_BIN"
      "$AGENT_FLOW_SCRIPT"
      --org "$ORG"
      --prefix "$PREFIX"
      --base-url "$BASE_URL"
    )
    if [[ -n "$API_KEY" ]]; then
      agent_cmd+=(--api-key "$API_KEY")
    fi
    if [[ "$DRY_RUN" -eq 1 ]]; then
      agent_cmd+=(--dry-run)
    fi
    run_cmd "${agent_cmd[@]}"
  fi

  log "Step 5/6: trigger CVE scan"
  post_cve_scan

  if [[ "$SKIP_REPO_HEALTH_CHECK" -eq 0 ]]; then
    log "Step 6/6: validate GitHub Repo Health endpoint"
    validate_repo_health
  fi

  printf '\n'
  log "Validation run complete"
  printf '[TODO] Open %s/enrichment/cve and verify vulnerable vs fixed service findings.\n' "$BASE_URL"
  printf '[TODO] Open %s/web-traffic and verify RUM/service/version correlations.\n' "$BASE_URL"
  printf '[TODO] Open %s/errors and confirm JS stacks include [mapped] entries when source maps are enabled.\n' "$BASE_URL"
  printf '[TODO] Open %s/enrichment/cve and verify GitHub Repo Health panel rows/counts match API validation.\n' "$BASE_URL"
  printf '[TODO] Open %s/settings/repositories and verify tracked repos + release versions.\n' "$BASE_URL"
  printf '[TODO] Open %s/work-items and verify the two newly-created agent issues are listed and link to GitHub.\n' "$BASE_URL"
}


main "$@"
