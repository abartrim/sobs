#!/usr/bin/env bash

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if [[ -d "$repo_root/.venv/bin" ]]; then
  export PATH="$repo_root/.venv/bin:$PATH"
fi

mode="check"
template_targets=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --fix)
      mode="fix"
      shift
      ;;
    --check)
      mode="check"
      shift
      ;;
    --help|-h)
      echo "Usage: bash scripts/check_templates.sh [--check|--fix] [targets...]"
      exit 0
      ;;
    --)
      shift
      while [[ $# -gt 0 ]]; do
        template_targets+=("$1")
        shift
      done
      ;;
    *)
      template_targets+=("$1")
      shift
      ;;
  esac
done

if ! command -v python3 >/dev/null 2>&1; then
  echo "check_templates: python3 is required." >&2
  exit 1
fi

if [[ ${#template_targets[@]} -eq 0 ]]; then
  template_targets=("templates")
fi

if [[ "$mode" == "fix" ]]; then
  echo "check_templates: formatting and linting templates with djlint"
  python3 scripts/run_djlint.py --reformat --lint "${template_targets[@]}"
else
  echo "check_templates: checking templates with djlint"
  python3 scripts/run_djlint.py --check --lint "${template_targets[@]}"
fi