#!/usr/bin/env bash

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if [[ -d "$repo_root/.venv/bin" ]]; then
  export PATH="$repo_root/.venv/bin:$PATH"
fi

mode="check"
python_files=()

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
      echo "Usage: bash scripts/check_python.sh [--check|--fix] [files...]"
      exit 0
      ;;
    --)
      shift
      while [[ $# -gt 0 ]]; do
        python_files+=("$1")
        shift
      done
      ;;
    *)
      python_files+=("$1")
      shift
      ;;
  esac
done

required_tools=(isort black flake8 mypy)
for tool in "${required_tools[@]}"; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "check_python: missing required tool '$tool'." >&2
    exit 1
  fi
done

if [[ ${#python_files[@]} -eq 0 ]]; then
  while IFS= read -r -d '' file; do
    python_files+=("$file")
  done < <(git ls-files -z '*.py')
fi

if [[ ${#python_files[@]} -eq 0 ]]; then
  exit 0
fi

if [[ "$mode" == "fix" ]]; then
  echo "check_python: formatting Python files with isort and black"
  isort "${python_files[@]}"
  black "${python_files[@]}"
else
  echo "check_python: verifying Python formatting with isort and black"
  isort --check-only "${python_files[@]}"
  black --check "${python_files[@]}"
fi

echo "check_python: linting Python files with flake8"
flake8 "${python_files[@]}"

echo "check_python: type-checking Python files with mypy"
mypy "${python_files[@]}"