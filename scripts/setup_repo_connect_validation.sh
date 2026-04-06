#!/usr/bin/env bash
set -euo pipefail

# Bootstrap private GitHub validation repos for SOBS enrichment/CVE/agent-flow testing.
#
# What this script automates:
# - Creates 3 private repos with gh CLI
# - Seeds vulnerable and fixed dependency fixtures
# - Creates release tags/branches for ref fallback testing
# - Seeds labels + sample issues for agent-flow testing
#
# What still requires user action:
# - GitHub authentication (`gh auth login`) if not already set
# - Adding the token/repo settings in SOBS UI
# - Optional org-level policy tweaks (if your org blocks actions)

SCRIPT_NAME="$(basename "$0")"
ORG=""
PREFIX="sobs-validation"
WORKDIR=""
ALLOW_GIT_DIR=0
SKIP_ISSUES=0
DRY_RUN=0
ASSUME_YES=0

usage() {
  cat <<'USAGE'
Usage:
  setup_repo_connect_validation.sh --org <github-org-or-user> [options]

Required:
  --org <name>               GitHub org/user that will own the test repos.

Options:
  --prefix <value>           Repo name prefix (default: sobs-validation).
  --workdir <path>           Directory for local repo scaffolding.
                             Default: ./sobs-validation-bootstrap (from current dir).
  --allow-git-dir            Allow running from inside an existing git repo.
  --skip-issues              Skip creating labels/issues in agent playground repo.
  --yes                      Skip confirmation prompt.
  --dry-run                  Print commands without executing.
  -h, --help                 Show this help.

Example:
  ./scripts/setup_repo_connect_validation.sh \
    --org my-test-org \
    --prefix sobs-lab \
    --workdir "$HOME/dev/sobs-validation-bootstrap"
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

run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf '[DRYRUN]'
    local arg
    for arg in "$@"; do
      printf ' %q' "$arg"
    done
    printf '\n'
  else
    "$@"
  fi
}

run_retry() {
  local attempts="$1"
  shift
  local delay_sec="$1"
  shift
  local i
  for ((i = 1; i <= attempts; i += 1)); do
    if run "$@"; then
      return 0
    fi
    if [[ "$i" -lt "$attempts" ]]; then
      log "Retrying in ${delay_sec}s (${i}/${attempts})..."
      sleep "$delay_sec"
    fi
  done
  return 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

validate_identifier() {
  local label="$1"
  local value="$2"
  if [[ ! "$value" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    die "$label contains unsupported characters. Allowed: letters, digits, dot, underscore, hyphen."
  fi
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
      --allow-git-dir)
        ALLOW_GIT_DIR=1
        shift
        ;;
      --skip-issues)
        SKIP_ISSUES=1
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
  validate_identifier "--org" "$ORG"
  validate_identifier "--prefix" "$PREFIX"

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
  printf 'SOBS Validation Repo Bootstrap\n'
  printf '==============================================================\n'
  printf 'This script will do the following:\n'
  printf '  1) Create/Update private GitHub repos under %s:\n' "$ORG"
  printf '     - %s\n' "$vuln_repo"
  printf '     - %s\n' "$fixed_repo"
  printf '     - %s\n' "$agent_repo"
  printf '  2) Seed fixture files (vulnerable/fixed dependencies, templates).\n'
  printf '  3) Push commits, tags, and branch refs for backfill/ref-fallback tests.\n'
  printf '  4) Create labels + sample issues in the agent playground repo.\n'
  printf '\n'
  printf 'Work directory: %s\n' "$WORKDIR"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf 'Mode: DRY RUN (no changes will be made)\n'
  else
    printf 'Mode: LIVE RUN (GitHub repos/issues/tags may be created or updated)\n'
  fi
  printf '\n'
  printf 'You may still need to do these manually after this script:\n'
  printf '  - Configure SOBS GitHub token and repository settings\n'
  printf '  - Register releases/artifacts in SOBS (use companion auto-register script)\n'
  printf '==============================================================\n'
  printf '\n'
}

confirm_execution() {
  if [[ "$ASSUME_YES" -eq 1 ]] || [[ "$DRY_RUN" -eq 1 ]]; then
    return 0
  fi

  local response
  read -r -p "Continue with live GitHub changes? [y/N]: " response
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
  require_cmd git

  if [[ "$ALLOW_GIT_DIR" -ne 1 ]] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    die "Run this from a clean non-git directory, or pass --allow-git-dir if you really want to run here."
  fi

  if ! gh auth status >/dev/null 2>&1; then
    todo "Run: gh auth login"
    die "gh CLI is not authenticated"
  fi

  if [[ -d "$WORKDIR" ]]; then
    if [[ -n "$(ls -A "$WORKDIR" 2>/dev/null || true)" ]]; then
      todo "Using existing workdir: $WORKDIR"
      todo "If you want a clean run, remove it first: rm -rf '$WORKDIR'"
    fi
  fi

  run mkdir -p "$WORKDIR"
}

write_common_files() {
  local repo_path="$1"
  local repo_name="$2"

  cat >"$repo_path/.gitignore" <<'EOF'
__pycache__/
*.pyc
node_modules/
.DS_Store
EOF

  cat >"$repo_path/README.md" <<EOF
# ${repo_name}

Purpose-built test fixture repository for SOBS enrichment/CVE and agent-flow validation.

> This repository intentionally contains dependency versions used for security/test simulation.
> Do not deploy this code in production.
EOF
}

seed_vulnerable_repo() {
  local repo_path="$1"

  write_common_files "$repo_path" "${PREFIX}-vuln-fixture"

  cat >"$repo_path/requirements.txt" <<'EOF'
Flask==0.12.2
urllib3==1.24.1
requests==2.19.1
jinja2==2.10
EOF

  cat >"$repo_path/package-lock.json" <<'EOF'
{
  "name": "sobs-vuln-fixture",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "sobs-vuln-fixture",
      "version": "0.1.0",
      "dependencies": {
        "lodash": "4.17.11",
        "minimist": "0.0.8"
      }
    },
    "node_modules/lodash": {
      "version": "4.17.11"
    },
    "node_modules/minimist": {
      "version": "0.0.8"
    }
  }
}
EOF

  cat >"$repo_path/go.sum" <<'EOF'
github.com/dgrijalva/jwt-go v3.2.0+incompatible h1:7qlOGliEKZXTDg6OTjfoBKDXWrumCAMpl/TFQ4/5kLM=
github.com/dgrijalva/jwt-go v3.2.0+incompatible/go.mod h1:E3ru+11k8f5W4nQyJm3QlAj8Xz6Q7fM3cYVvL96yQqY=
EOF

  cat >"$repo_path/Gemfile.lock" <<'EOF'
GEM
  remote: https://rubygems.org/
  specs:
    rack (1.6.0)

PLATFORMS
  ruby

DEPENDENCIES
  rack

BUNDLED WITH
   2.1.4
EOF

  cat >"$repo_path/app.py" <<'EOF'
from flask import Flask

app = Flask(__name__)


@app.get("/")
def health():
    return {"status": "ok", "fixture": "vulnerable"}
EOF
}

seed_fixed_repo() {
  local repo_path="$1"

  write_common_files "$repo_path" "${PREFIX}-fixed-fixture"

  cat >"$repo_path/requirements.txt" <<'EOF'
Flask==3.0.3
urllib3==2.2.2
requests==2.32.3
jinja2==3.1.4
EOF

  cat >"$repo_path/package-lock.json" <<'EOF'
{
  "name": "sobs-fixed-fixture",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "sobs-fixed-fixture",
      "version": "1.0.0",
      "dependencies": {
        "lodash": "4.17.21",
        "minimist": "1.2.8"
      }
    },
    "node_modules/lodash": {
      "version": "4.17.21"
    },
    "node_modules/minimist": {
      "version": "1.2.8"
    }
  }
}
EOF

  cat >"$repo_path/app.py" <<'EOF'
from flask import Flask

app = Flask(__name__)


@app.get("/")
def health():
    return {"status": "ok", "fixture": "fixed"}
EOF
}

seed_agent_repo() {
  local repo_path="$1"

  write_common_files "$repo_path" "${PREFIX}-agent-playground"

  run mkdir -p "$repo_path/.github/ISSUE_TEMPLATE"

  cat >"$repo_path/.github/ISSUE_TEMPLATE/bug_report.md" <<'EOF'
---
name: Bug report
about: Track an issue generated by SOBS agent flow
---

## Summary

## Expected behavior

## Actual behavior

## Notes
EOF

  cat >"$repo_path/.github/pull_request_template.md" <<'EOF'
## Why

## What changed

## How tested

## Risk
EOF

  cat >"$repo_path/README.md" <<'EOF'
# Agent Playground

This repository is used to test SOBS agent issue/PR workflows.
EOF
}

init_or_pull_repo() {
  local repo_path="$1"
  local repo_name="$2"
  local org_repo="$3"
  local seed_fn="$4"

  # If local repo exists, pull latest
  if [[ -d "$repo_path/.git" ]]; then
    log "Local repo exists, pulling latest: $repo_path"
    run git -C "$repo_path" fetch origin main || true
    run git -C "$repo_path" checkout main || true
    run git -C "$repo_path" reset --hard origin/main 2>/dev/null || true
    return 0
  fi

  # If GitHub repo exists, clone it
  if gh repo view "$org_repo" >/dev/null 2>&1; then
    log "Cloning existing GitHub repo: $org_repo"
    run mkdir -p "$repo_path"
    run git clone "https://github.com/$org_repo.git" "$repo_path"
    return 0
  fi

  # Otherwise, create fresh locally and seed
  log "Creating new local repo: $repo_path"
  run mkdir -p "$repo_path"
  run git -C "$repo_path" init -b main
  "$seed_fn" "$repo_path"
  run git -C "$repo_path" add .
  run git -C "$repo_path" commit -m "chore: seed ${repo_name} fixture"
}

update_repo_content() {
  local repo_path="$1"
  local seed_fn="$2"

  # Seed/update fixture content
  "$seed_fn" "$repo_path"
  
  # Commit if there are changes
  if ! git -C "$repo_path" diff-index --quiet HEAD --; then
    run git -C "$repo_path" add .
    run git -C "$repo_path" commit -m "chore: update fixture content" || true
  fi
}

push_repo_to_github() {
  local repo_name="$1"
  local repo_path="$2"
  local org_repo="$3"

  # Create on GitHub if doesn't exist
  if ! gh repo view "$org_repo" >/dev/null 2>&1; then
    run gh repo create "$org_repo" --private
  fi

  # Ensure remote is set
  if ! git -C "$repo_path" remote get-url origin >/dev/null 2>&1; then
    run git -C "$repo_path" remote add origin "https://github.com/$org_repo.git"
  fi

  # Push with retry
  run_retry 3 2 git -C "$repo_path" push -u origin main || true
  run_retry 3 2 git -C "$repo_path" push origin --tags || true
}

create_release_refs() {
  local repo_path="$1"

  # Tag format coverage for ref fallback testing: v-prefixed + plain.
  run git -C "$repo_path" tag -f v0.1.0 || true
  run git -C "$repo_path" tag -f 0.1.0 || true

  # Branch ref fallback coverage: create/update 0.2.0 branch
  run git -C "$repo_path" checkout -B 0.2.0
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf '[DRYRUN] append branch fixture marker to %q\n' "$repo_path/README.md"
  else
    printf '\n# branch fixture\n' >> "$repo_path/README.md"
  fi
  run git -C "$repo_path" add README.md
  run git -C "$repo_path" commit -m "test: branch-based ref fixture" || true
  run git -C "$repo_path" checkout main

  # Push tags and branch (force branch to overwrite remote if exists)
  run git -C "$repo_path" push origin --tags || true
  run git -C "$repo_path" push --force origin 0.2.0 || true
}

seed_agent_labels_and_issues() {
  local repo_name="$1"

  [[ "$SKIP_ISSUES" -eq 1 ]] && return 0

  local labels=(
    "agent-fix:Issues discovered by SOBS agent flow"
    "security:Security/CVE test issue"
    "triage:Needs triage"
  )

  for pair in "${labels[@]}"; do
    local name="${pair%%:*}"
    local desc="${pair#*:}"
    if ! gh label view "$name" --repo "$ORG/$repo_name" >/dev/null 2>&1; then
      run gh label create "$name" --repo "$ORG/$repo_name" --description "$desc" || true
    fi
  done

  if ! gh issue list --repo "$ORG/$repo_name" --state all --limit 100 --search "in:title \"Fixture: vulnerable dependency should be upgraded\"" --json title --jq 'length' | grep -Eq '^[1-9][0-9]*$'; then
    run gh issue create --repo "$ORG/$repo_name" --title "Fixture: vulnerable dependency should be upgraded" --body "Generated test issue for SOBS agent fix workflow." --label security --label triage
  fi
  if ! gh issue list --repo "$ORG/$repo_name" --state all --limit 100 --search "in:title \"Fixture: create PR from suggested patch\"" --json title --jq 'length' | grep -Eq '^[1-9][0-9]*$'; then
    run gh issue create --repo "$ORG/$repo_name" --title "Fixture: create PR from suggested patch" --body "Use this issue to validate agent PR authoring flow." --label agent-fix
  fi
}

ensure_repo_health_issue() {
  local repo_full="$1"
  local title="$2"
  local body="$3"
  local labels_csv="$4"

  if gh issue list --repo "$repo_full" --state all --limit 200 --search "in:title \"$title\"" --json title --jq 'length' | grep -Eq '^[1-9][0-9]*$'; then
      # Create labels if they don't exist
      if [[ -n "$labels_csv" ]]; then
        IFS=',' read -r -a label_parts <<< "$labels_csv"
        local label
        for label in "${label_parts[@]}"; do
          label="${label// /}"  # trim whitespace
          if ! gh label view "$label" --repo "$repo_full" >/dev/null 2>&1; then
            run gh label create "$label" --repo "$repo_full" || true
          fi
        done
      fi

    return 0
  fi

  local cmd=(gh issue create --repo "$repo_full" --title "$title" --body "$body")
  if [[ -n "$labels_csv" ]]; then
    IFS=',' read -r -a label_parts <<< "$labels_csv"
    local label
    for label in "${label_parts[@]}"; do
        label="${label// /}"  # trim whitespace
      cmd+=(--label "$label")
    done
  fi
  run "${cmd[@]}"
}

seed_version_scoped_repo_health_fixtures() {
  local vuln_repo="$1"
  local fixed_repo="$2"
  local agent_repo="$3"
  local vuln_path="$4"

  [[ "$SKIP_ISSUES" -eq 1 ]] && return 0

  local vuln_full="$ORG/$vuln_repo"
  local fixed_full="$ORG/$fixed_repo"
  local agent_full="$ORG/$agent_repo"

  ensure_repo_health_issue \
    "$vuln_full" \
    "Repo health fixture: v0.1.0 security review" \
    "Version-scoped fixture for SOBS repo health. Affects releases: v0.1.0 and 0.1.0. Security context: CVE triage." \
    "security,triage"

  ensure_repo_health_issue \
    "$vuln_full" \
    "Repo health fixture: 0.2.0 regression follow-up" \
    "Version-scoped fixture for SOBS repo health. Target release 0.2.0." \
    "triage"

  ensure_repo_health_issue \
    "$fixed_full" \
    "Repo health fixture: v1.0.0 release hardening" \
    "Version-scoped fixture for SOBS repo health. Target release v1.0.0 / 1.0.0." \
    "security"

  ensure_repo_health_issue \
    "$agent_full" \
    "Repo health fixture: 0.1.0 agent workflow check" \
    "Version-scoped fixture for SOBS repo health and agent tests. Target release 0.1.0." \
    "agent-fix"

  # Seed one deterministic open PR mentioning version tokens for PR-count validation.
  local pr_branch="fixture/repo-health-v0-1-0"
  local pr_title="Repo health fixture PR: v0.1.0 docs update"
  if ! gh pr list --repo "$vuln_full" --state open --head "$pr_branch" --json number --jq 'length' | grep -Eq '^[1-9][0-9]*$'; then
    run git -C "$vuln_path" checkout -B "$pr_branch"
    if [[ "$DRY_RUN" -eq 1 ]]; then
      printf '[DRYRUN] append repo-health PR fixture marker to %q\n' "$vuln_path/README.md"
    else
      printf '\nRepo health PR fixture for v0.1.0 and 0.1.0\n' >> "$vuln_path/README.md"
    fi
    run git -C "$vuln_path" add README.md
    run git -C "$vuln_path" commit -m "test: repo health PR fixture v0.1.0" || true
    run git -C "$vuln_path" push -u origin "$pr_branch" --force-with-lease
    run gh pr create --repo "$vuln_full" --base main --head "$pr_branch" --title "$pr_title" --body "Version-scoped PR fixture for SOBS repo health (v0.1.0, 0.1.0). Security follow-up."
    run git -C "$vuln_path" checkout main
  fi
}

print_summary() {
  local vuln_repo="$1"
  local fixed_repo="$2"
  local agent_repo="$3"

  printf '\n'
  log "Bootstrap complete"
  printf '\n'
  printf 'Created/updated repos:\n'
  printf '  - https://github.com/%s/%s\n' "$ORG" "$vuln_repo"
  printf '  - https://github.com/%s/%s\n' "$ORG" "$fixed_repo"
  printf '  - https://github.com/%s/%s\n' "$ORG" "$agent_repo"
  printf '\n'

  todo "In SOBS: Settings -> GitHub Repositories -> add these repo URLs (or import later when Repo Connect ships)."
  todo "In SOBS: Settings -> AI -> set github_token with contents:read and issues:write scopes as needed."
  todo "If validating source-map remapping: set SOBS_SOURCE_MAP_ENABLE=true and SOBS_SOURCE_MAP_DIR to your fixture map directory."
  todo "Register release versions in SOBS for stronger version-scoped repo health testing."
  todo "Generate correlated OTEL/RUM data: python3 scripts/generate_validation_telemetry.py --org $ORG --prefix $PREFIX"
  todo "Run CVE scan and validate findings/dispositions against vuln vs fixed fixture repos."
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

  # Setup vuln fixture
  init_or_pull_repo "$vuln_path" "$vuln_repo" "$ORG/$vuln_repo" seed_vulnerable_repo
  update_repo_content "$vuln_path" seed_vulnerable_repo
  push_repo_to_github "$vuln_repo" "$vuln_path" "$ORG/$vuln_repo"
  create_release_refs "$vuln_path"

  # Setup fixed fixture
  init_or_pull_repo "$fixed_path" "$fixed_repo" "$ORG/$fixed_repo" seed_fixed_repo
  update_repo_content "$fixed_path" seed_fixed_repo
  push_repo_to_github "$fixed_repo" "$fixed_path" "$ORG/$fixed_repo"
  run git -C "$fixed_path" tag -f v1.0.0 || true
  run git -C "$fixed_path" push origin --tags || true

  # Setup agent playground
  init_or_pull_repo "$agent_path" "$agent_repo" "$ORG/$agent_repo" seed_agent_repo
  update_repo_content "$agent_path" seed_agent_repo
  push_repo_to_github "$agent_repo" "$agent_path" "$ORG/$agent_repo"
  seed_agent_labels_and_issues "$agent_repo"
  
  # Seed version-scoped issues for repo-health validation
  seed_version_scoped_repo_health_fixtures "$vuln_repo" "$fixed_repo" "$agent_repo" "$vuln_path"

  print_summary "$vuln_repo" "$fixed_repo" "$agent_repo"
}

main "$@"
