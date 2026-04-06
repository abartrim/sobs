#!/usr/bin/env bash
set -euo pipefail

# Register GitHub validation fixture repos/releases into a SOBS instance.
# Designed to pair with scripts/setup_repo_connect_validation.sh.

ORG=""
PREFIX="sobs-validation"
WORKDIR=""
BASE_URL="http://127.0.0.1:44317"
API_KEY=""
PYTHON_BIN="python3"
DRY_RUN=0
ASSUME_YES=0
SOURCE_MAP_STORAGE_PREFIX="local://source-maps"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGISTER_SCRIPT="$SCRIPT_DIR/register_release_artifacts.py"

usage() {
  cat <<'USAGE'
Usage:
  register_validation_fixtures_to_sobs.sh --org <github-org-or-user> [options]

Required:
  --org <name>               GitHub org/user that owns fixture repos.

Options:
  --prefix <value>           Repo prefix (default: sobs-validation)
  --workdir <path>           Local repo workspace (default: ./sobs-validation-bootstrap)
  --base-url <url>           SOBS base URL (default: http://127.0.0.1:44317)
  --api-key <key>            SOBS API key (optional)
  --python-bin <path>        Python interpreter (default: python3)
  --source-map-storage-prefix <uri>
                             Storage URI prefix recorded in release artifacts metadata
                             (default: local://source-maps)
  --dry-run                  Print actions without performing writes
  --yes                      Skip confirmation prompt
  -h, --help                 Show help

Examples:
  ./scripts/register_validation_fixtures_to_sobs.sh --org my-test-org
  ./scripts/register_validation_fixtures_to_sobs.sh --org my-test-org --api-key "$SOBS_API_KEY"
USAGE
}

log() {
  printf '[INFO] %s\n' "$*"
}

todo() {
  printf '[TODO] %s\n' "$*"
}

die() {
  printf '[ERROR] %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
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
      --workdir)
        WORKDIR="${2:-}"
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
      --source-map-storage-prefix)
        SOURCE_MAP_STORAGE_PREFIX="${2:-}"
        shift 2
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
  if [[ -z "$WORKDIR" ]]; then
    WORKDIR="$PWD/sobs-validation-bootstrap"
  fi
}

print_startup_guidance() {
  local vuln_repo="${PREFIX}-vuln-fixture"
  local fixed_repo="${PREFIX}-fixed-fixture"
  local agent_repo="${PREFIX}-agent-playground"

  printf '\n'
  printf '==============================================================\n'
  printf 'SOBS Validation Fixture Auto-Registration\n'
  printf '==============================================================\n'
  printf 'This script will do the following:\n'
  printf '  1) Ensure local fixture repos are available (clone if needed)\n'
  printf '  2) Register apps + releases in SOBS via register_release_artifacts.py\n'
  printf '  3) Register dependency artifacts for CVE scanning tests\n'
  printf '\n'
  printf 'Fixture repos expected:\n'
  printf '  - https://github.com/%s/%s\n' "$ORG" "$vuln_repo"
  printf '  - https://github.com/%s/%s\n' "$ORG" "$fixed_repo"
  printf '  - https://github.com/%s/%s\n' "$ORG" "$agent_repo"
  printf '\n'
  printf 'SOBS target: %s\n' "$BASE_URL"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf 'Mode: DRY RUN (no SOBS writes)\n'
  else
    printf 'Mode: LIVE RUN (SOBS app/release/artifact records will be created/updated)\n'
  fi
  printf '==============================================================\n'
  printf '\n'
}

confirm_execution() {
  if [[ "$ASSUME_YES" -eq 1 ]] || [[ "$DRY_RUN" -eq 1 ]]; then
    return 0
  fi

  local response
  read -r -p "Continue with SOBS registration writes? [y/N]: " response
  case "${response:-}" in
    y|Y|yes|YES)
      return 0
      ;;
    *)
      die "Cancelled by user"
      ;;
  esac
}

check_environment() {
  require_cmd gh
  require_cmd "$PYTHON_BIN"
  [[ -f "$REGISTER_SCRIPT" ]] || die "Missing helper script: $REGISTER_SCRIPT"

  if ! gh auth status >/dev/null 2>&1; then
    todo "Run: gh auth login"
    die "gh CLI is not authenticated"
  fi

  mkdir -p "$WORKDIR"
}

ensure_repo_local() {
  local repo_name="$1"
  local repo_path="$2"

  if [[ -d "$repo_path/.git" ]]; then
    log "Using local repo: $repo_path"
    git -C "$repo_path" fetch --all --tags >/dev/null 2>&1 || true
    return 0
  fi

  log "Cloning repo: $ORG/$repo_name"
  gh repo clone "$ORG/$repo_name" "$repo_path" >/dev/null
}

register_release() {
  local app_name="$1"
  local app_slug="$2"
  local repo_url="$3"
  local release_version="$4"
  local environment="$5"
  local requirements_file="$6"
  local dependencies_file="$7"
  local dependencies_name="$8"
  local commit_sha="$9"
  local build_id="${10}"
  local release_metadata_json="${11}"
  local artifacts_file="${12}"

  local cmd
  cmd=(
    "$PYTHON_BIN" "$REGISTER_SCRIPT"
    --base-url "$BASE_URL"
    --app-name "$app_name"
    --app-slug "$app_slug"
    --repo-url "$repo_url"
    --default-environment "$environment"
    --release-version "$release_version"
    --commit-sha "$commit_sha"
    --build-id "$build_id"
    --environment "$environment"
    --release-metadata-json "$release_metadata_json"
  )

  if [[ -n "$API_KEY" ]]; then
    cmd+=(--api-key "$API_KEY")
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    cmd+=(--dry-run)
  fi

  if [[ -n "$requirements_file" ]]; then
    cmd+=(--requirements-file "$requirements_file")
  fi
  if [[ -n "$dependencies_file" ]]; then
    cmd+=(--dependencies-file "$dependencies_file")
  fi
  if [[ -n "$dependencies_name" ]]; then
    cmd+=(--dependencies-name "$dependencies_name")
  fi
  if [[ -n "$artifacts_file" ]]; then
    cmd+=(--artifacts-file "$artifacts_file")
  fi

  "${cmd[@]}"
}

build_commit_sha() {
  local repo_name="$1"
  local release_version="$2"
  printf '%s' "${repo_name}:${release_version}" | shasum | awk '{print $1}'
}

write_release_metadata_json() {
  local repo_name="$1"
  local release_version="$2"
  local build_id="$3"

  cat <<EOF
{"ci":{"provider":"github-actions","workflow":"validation-fixture","buildId":"$build_id"},"fixture":{"repo":"$repo_name","release":"$release_version","seededBy":"register_validation_fixtures_to_sobs.sh"}}
EOF
}

write_release_artifacts_manifest() {
  local repo_name="$1"
  local release_version="$2"
  local output_path="$3"

  local clean_release
  clean_release="${release_version#v}"
  local storage_ref
  storage_ref="${SOURCE_MAP_STORAGE_PREFIX%/}/${repo_name}/${clean_release}/app.min.js.map"

  cat >"$output_path" <<EOF
[
  {
    "artifactType": "js_sourcemap",
    "name": "app.min.js.map",
    "contentType": "application/json",
    "size": 0,
    "storageRef": "$storage_ref",
    "checksumSha256": "",
    "metadata": {
      "bundle": "app.min.js",
      "fixture": true,
      "repo": "$repo_name"
    }
  }
]
EOF
}

write_pkg_lock_dependencies() {
  local package_lock_path="$1"
  local output_path="$2"

  "$PYTHON_BIN" - "$package_lock_path" "$output_path" <<'PY'
import json
import sys

package_lock, out = sys.argv[1], sys.argv[2]
with open(package_lock, encoding="utf-8") as fh:
    data = json.load(fh)

deps = []
packages = data.get("packages", {})
for key, value in packages.items():
    if not key or not key.startswith("node_modules/"):
        continue
    name = key.replace("node_modules/", "", 1)
    version = str(value.get("version", "")).strip()
    if name:
        deps.append({"package": name, "version": version, "ecosystem": "npm"})

with open(out, "w", encoding="utf-8") as fh:
    json.dump(deps, fh)
PY
}

register_fixture_repo() {
  local repo_name="$1"
  local app_name="$2"
  local app_slug="$3"
  local repo_path="$4"
  shift 4
  local releases=("$@")

  local repo_url="https://github.com/$ORG/$repo_name"
  local requirements_file="$repo_path/requirements.txt"
  local package_lock="$repo_path/package-lock.json"
  local tmp_deps
  local tmp_artifacts
  tmp_deps="$(mktemp)"
  tmp_artifacts="$(mktemp)"

  if [[ -f "$package_lock" ]]; then
    write_pkg_lock_dependencies "$package_lock" "$tmp_deps"
  else
    printf '[]' >"$tmp_deps"
  fi

  for release in "${releases[@]}"; do
    log "Registering $app_name release: $release"
    local commit_sha
    local build_id
    local release_metadata_json
    commit_sha="$(build_commit_sha "$repo_name" "$release")"
    build_id="fixture-${repo_name}-${release//[^a-zA-Z0-9]/-}"
    release_metadata_json="$(write_release_metadata_json "$repo_name" "$release" "$build_id")"
    write_release_artifacts_manifest "$repo_name" "$release" "$tmp_artifacts"

    if [[ -f "$requirements_file" ]]; then
      register_release "$app_name" "$app_slug" "$repo_url" "$release" "prod" \
        "$requirements_file" "" "requirements.txt" "$commit_sha" "$build_id" "$release_metadata_json" "$tmp_artifacts"
    else
      register_release "$app_name" "$app_slug" "$repo_url" "$release" "prod" "" "" "" \
        "$commit_sha" "$build_id" "$release_metadata_json" "$tmp_artifacts"
    fi

    if [[ -s "$tmp_deps" ]] && [[ "$(cat "$tmp_deps")" != "[]" ]]; then
      register_release "$app_name" "$app_slug" "$repo_url" "$release" "prod" \
        "" "$tmp_deps" "package-lock.json" "$commit_sha" "$build_id" "$release_metadata_json" "$tmp_artifacts"
    fi
  done

  rm -f "$tmp_deps"
  rm -f "$tmp_artifacts"
}

register_agent_repo() {
  local repo_name="$1"
  local repo_path="$2"
  local repo_url="https://github.com/$ORG/$repo_name"

  # Register a baseline release so repo health/agent testing has a tracked app entry.
  log "Registering agent playground baseline release"
  local release="0.1.0"
  local commit_sha
  local build_id
  local release_metadata_json
  local tmp_artifacts
  commit_sha="$(build_commit_sha "$repo_name" "$release")"
  build_id="fixture-${repo_name}-${release//[^a-zA-Z0-9]/-}"
  release_metadata_json="$(write_release_metadata_json "$repo_name" "$release" "$build_id")"
  tmp_artifacts="$(mktemp)"
  write_release_artifacts_manifest "$repo_name" "$release" "$tmp_artifacts"
  register_release "${PREFIX}-agent-playground" "${PREFIX}-agent-playground" "$repo_url" "$release" "dev" "" "" "" \
    "$commit_sha" "$build_id" "$release_metadata_json" "$tmp_artifacts"
  rm -f "$tmp_artifacts"

  if [[ ! -d "$repo_path" ]]; then
    todo "Agent repo path missing locally: $repo_path"
  fi
}

print_summary() {
  printf '\n'
  log "Registration complete"
  printf '\n'
  todo "Open SOBS -> CVE page and run 'Scan now' to validate vulnerable vs fixed findings."
  todo "Open SOBS release details and verify commit/build/release metadata + js_sourcemap artifacts are present."
  todo "Open SOBS -> Settings -> GitHub Repositories and confirm releases are linked to each app."
  todo "Open SOBS -> Settings -> AI and ensure github_token has contents:read + issues:write scopes."
}

main() {
  parse_args "$@"
  print_startup_guidance
  confirm_execution
  check_environment

  local vuln_repo="${PREFIX}-vuln-fixture"
  local fixed_repo="${PREFIX}-fixed-fixture"
  local agent_repo="${PREFIX}-agent-playground"

  local vuln_path="$WORKDIR/$vuln_repo"
  local fixed_path="$WORKDIR/$fixed_repo"
  local agent_path="$WORKDIR/$agent_repo"

  ensure_repo_local "$vuln_repo" "$vuln_path"
  ensure_repo_local "$fixed_repo" "$fixed_path"
  ensure_repo_local "$agent_repo" "$agent_path"

  register_fixture_repo "$vuln_repo" "${PREFIX}-vuln-fixture" "${PREFIX}-vuln-fixture" "$vuln_path" "v0.1.0" "0.1.0" "0.2.0"
  register_fixture_repo "$fixed_repo" "${PREFIX}-fixed-fixture" "${PREFIX}-fixed-fixture" "$fixed_path" "v1.0.0" "1.0.0"
  register_agent_repo "$agent_repo" "$agent_path"

  print_summary
}

main "$@"
