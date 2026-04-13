#!/usr/bin/env python3

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path

UNSAFE_REFORMAT_MARKERS = (
    "<script",
    "{% block scripts %}",
    "_regex_filter_script.js",
    "_rum_asset_viewer_script.js",
    "| tojson",
    "window.",
    "document.",
    "fetch(",
    "navigator.clipboard",
    "JSON.stringify(",
)


def _discover_template_files(targets: list[str]) -> list[str]:
    paths: list[str] = []
    for target in targets:
        target_path = Path(target)
        if target_path.is_dir():
            paths.extend(str(path) for path in sorted(target_path.rglob("*.html")))
        elif target_path.is_file() and target_path.suffix == ".html":
            paths.append(str(target_path))
    return paths


def _git_output(args: list[str]) -> str:
    result = subprocess.run(["git", *args], check=True, capture_output=True, text=True)
    return result.stdout.strip()


def _discover_changed_template_files(targets: list[str]) -> list[str]:
    diff_range: str | None = None
    base_ref = os.environ.get("SOBS_DJLINT_BASE_REF") or os.environ.get("GITHUB_BASE_REF")

    if base_ref:
        remote_ref = base_ref if "/" in base_ref else f"origin/{base_ref}"
        try:
            merge_base = _git_output(["merge-base", "HEAD", remote_ref])
            diff_range = f"{merge_base}...HEAD"
        except subprocess.CalledProcessError:
            diff_range = None

    if diff_range is None:
        try:
            parent = _git_output(["rev-parse", "HEAD^"])
            diff_range = f"{parent}...HEAD"
        except subprocess.CalledProcessError:
            return []

    try:
        changed = _git_output(["diff", "--name-only", "--diff-filter=ACMR", diff_range, "--", *targets])
    except subprocess.CalledProcessError:
        return []

    return [line for line in changed.splitlines() if line.endswith(".html")]


def _is_reformat_safe(path: str) -> bool:
    content = Path(path).read_text(encoding="utf-8")
    return not any(marker in content for marker in UNSAFE_REFORMAT_MARKERS)


def _run_djlint(mode: str, files: list[str]) -> int:
    if not files:
        return 0
    cmd = ["djlint", f"--{mode}", "--configuration", "pyproject.toml", *files]
    return subprocess.run(cmd, check=False).returncode


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Run djlint with a conservative rollout for script-heavy Jinja templates."
    )
    parser.add_argument("targets", nargs="*", default=["templates"])
    parser.add_argument("--lint", action="store_true")
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--reformat", action="store_true")
    args = parser.parse_args()

    if not (args.lint or args.check or args.reformat):
        parser.error("At least one of --lint, --check, or --reformat is required.")

    files = _discover_template_files(args.targets)
    if not files:
        print("djlint rollout: no template files matched.")
        return 0

    explicit_files = [target for target in args.targets if Path(target).is_file() and Path(target).suffix == ".html"]
    candidate_files = explicit_files or _discover_changed_template_files(args.targets)
    safe_files = [path for path in candidate_files if _is_reformat_safe(path)]
    unsafe_files = [path for path in candidate_files if path not in safe_files]

    if candidate_files and unsafe_files and (args.check or args.reformat):
        print(
            "djlint rollout: skipping --check/--reformat for "
            f"{len(unsafe_files)} script-heavy template(s); lint still runs on all templates.",
            file=sys.stderr,
        )

    if not candidate_files and (args.check or args.reformat):
        print(
            "djlint rollout: no changed or explicitly targeted templates eligible "
            "for --check/--reformat; lint still runs on all templates.",
            file=sys.stderr,
        )

    exit_code = 0
    if args.reformat:
        exit_code = max(exit_code, _run_djlint("reformat", safe_files))
    if args.check:
        exit_code = max(exit_code, _run_djlint("check", safe_files))
    if args.lint:
        exit_code = max(exit_code, _run_djlint("lint", files))
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
