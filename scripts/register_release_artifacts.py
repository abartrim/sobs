#!/usr/bin/env python3
"""
Register app/release/artifact metadata with SOBS (CI helper).

This helper is idempotent-oriented:
- Reuses an existing app when slug/name already exists.
- Reuses an existing release when version/commit/environment/build matches.
- Skips artifact metadata rows that already exist by (artifactType, name, storageRef).

Usage examples:
  python scripts/register_release_artifacts.py \
    --base-url http://127.0.0.1:44317 \
    --api-key "$SOBS_API_KEY" \
    --app-name checkout-web \
    --release-version 1.2.3 \
    --commit-sha "$GITHUB_SHA" \
    --environment prod \
    --artifacts-file ./build/sobs-artifacts.json

Register dependencies (Python requirements.txt):
  python scripts/register_release_artifacts.py \
    --app-name checkout-web \
    --release-version 1.2.3 \
    --requirements-file requirements.txt

Register dependencies (generic JSON — works for any language):
  python scripts/register_release_artifacts.py \
    --app-name checkout-web \
    --release-version 1.2.3 \
    --dependencies-json '[{"package":"express","version":"4.18.2","ecosystem":"npm"}]'

You can also configure mostly through env vars:
  SOBS_BASE_URL
  SOBS_API_KEY
  SOBS_APP_NAME
  SOBS_APP_SLUG
  SOBS_OWNER_TEAM
  SOBS_APP_REPO_URL
  SOBS_DEFAULT_ENVIRONMENT
  SOBS_RELEASE_VERSION
  SOBS_RELEASE_COMMIT_SHA
  SOBS_RELEASE_BUILD_ID
  SOBS_RELEASE_ENVIRONMENT
  SOBS_RELEASED_AT
  SOBS_RELEASE_METADATA_JSON
  SOBS_RELEASE_ARTIFACTS_JSON
  SOBS_RELEASE_ARTIFACTS_JSON_FILE
  SOBS_RELEASE_DEPENDENCIES_JSON       -- JSON array [{package,version,ecosystem}, ...]
  SOBS_RELEASE_DEPENDENCIES_JSON_FILE  -- path to same JSON format
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _load_json_from_env_or_file(json_env: str, file_env: str, default: Any) -> Any:
    raw = _env(json_env)
    if not raw:
        file_path = _env(file_env)
        if file_path:
            with open(file_path, encoding="utf-8") as handle:
                raw = handle.read().strip()
    if not raw:
        return default
    parsed = json.loads(raw)
    return parsed


def _slugify(value: str) -> str:
    out = []
    prev_dash = False
    for ch in value.lower().strip():
        if ch.isalnum():
            out.append(ch)
            prev_dash = False
        else:
            if not prev_dash:
                out.append("-")
                prev_dash = True
    slug = "".join(out).strip("-")
    return slug or "app"


class SobsApi:
    def __init__(self, base_url: str, api_key: str, timeout: int) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def _request(self, method: str, path: str, payload: dict | list | None = None) -> Any:
        url = f"{self.base_url}{path}"
        body = None
        headers = {"Accept": "application/json"}
        if payload is not None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if self.api_key:
            headers["X-API-Key"] = self.api_key

        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # noqa: S310
                text = resp.read().decode("utf-8")
                return json.loads(text) if text else None
        except urllib.error.HTTPError as exc:
            err_body = ""
            try:
                err_body = exc.read().decode("utf-8")
            except Exception:
                err_body = ""
            raise RuntimeError(f"HTTP {exc.code} {method} {path}: {err_body}") from exc

    def get(self, path: str) -> Any:
        return self._request("GET", path)

    def post(self, path: str, payload: dict | list) -> Any:
        return self._request("POST", path, payload)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Register app/release/artifact metadata in SOBS")
    p.add_argument("--base-url", default=_env("SOBS_BASE_URL", "http://127.0.0.1:44317"))
    p.add_argument("--api-key", default=_env("SOBS_API_KEY"))

    p.add_argument("--app-name", default=_env("SOBS_APP_NAME"))
    p.add_argument("--app-slug", default=_env("SOBS_APP_SLUG"))
    p.add_argument("--owner-team", default=_env("SOBS_OWNER_TEAM"))
    p.add_argument("--repo-url", default=_env("SOBS_APP_REPO_URL"))
    p.add_argument("--default-environment", default=_env("SOBS_DEFAULT_ENVIRONMENT"))
    p.add_argument("--app-metadata-json", default=_env("SOBS_APP_METADATA_JSON"))

    p.add_argument("--release-version", default=_env("SOBS_RELEASE_VERSION"))
    p.add_argument("--commit-sha", default=_env("SOBS_RELEASE_COMMIT_SHA", _env("GITHUB_SHA")))
    p.add_argument("--build-id", default=_env("SOBS_RELEASE_BUILD_ID", _env("GITHUB_RUN_ID")))
    p.add_argument("--environment", default=_env("SOBS_RELEASE_ENVIRONMENT"))
    p.add_argument("--released-at", default=_env("SOBS_RELEASED_AT"))
    p.add_argument("--release-metadata-json", default=_env("SOBS_RELEASE_METADATA_JSON"))

    p.add_argument("--artifacts-file", default=_env("SOBS_RELEASE_ARTIFACTS_JSON_FILE"))
    p.add_argument("--artifacts-json", default=_env("SOBS_RELEASE_ARTIFACTS_JSON"))

    # Dependency/lockfile registration (stored as ArtifactType="dependencies-lockfile")
    p.add_argument(
        "--dependencies-json",
        default=_env("SOBS_RELEASE_DEPENDENCIES_JSON"),
        help="JSON array of {package,version,ecosystem} objects",
    )
    p.add_argument(
        "--dependencies-file",
        default=_env("SOBS_RELEASE_DEPENDENCIES_JSON_FILE"),
        help="Path to JSON file containing [{package,version,ecosystem}, ...] array",
    )
    p.add_argument(
        "--dependencies-name",
        default=_env("SOBS_RELEASE_DEPENDENCIES_NAME", "lockfile"),
        help="Label for this dependency set (e.g. 'requirements.txt', 'package-lock.json')",
    )
    p.add_argument(
        "--requirements-file",
        default=_env("SOBS_RELEASE_REQUIREMENTS_FILE"),
        help="Path to a pip requirements.txt (pip-freeze format); auto-converts to dependency list",
    )

    p.add_argument("--timeout", type=int, default=int(_env("SOBS_HTTP_TIMEOUT_SEC", "20") or "20"))
    p.add_argument("--dry-run", action="store_true")
    return p.parse_args()


def _parse_requirements_txt(path: str) -> list[dict[str, str]]:
    """Parse a pip freeze / requirements.txt file into [{package, version, ecosystem}]."""
    deps: list[dict[str, str]] = []
    with open(path, encoding="utf-8") as fh:
        for raw_line in fh:
            line = raw_line.strip()
            if not line or line.startswith("#") or line.startswith("-"):
                continue
            # Handle pkg==ver, pkg>=ver, pkg~=ver etc.
            for sep in ("==", ">=", "<=", "~=", "!=", ">", "<"):
                if sep in line:
                    parts = line.split(sep, 1)
                    package = parts[0].strip()
                    version = parts[1].strip().split(",")[0].strip()
                    if package:
                        deps.append({"package": package, "version": version, "ecosystem": "PyPI"})
                    break
    return deps


def _load_dependencies(args: argparse.Namespace) -> list[dict[str, str]]:
    """Return normalised [{package, version, ecosystem}] list or empty list."""
    # Python requirements.txt shorthand
    if args.requirements_file:
        deps = _parse_requirements_txt(args.requirements_file)
        if not args.dependencies_name or args.dependencies_name == "lockfile":
            args.dependencies_name = args.requirements_file
        return deps

    raw = str(args.dependencies_json or "").strip()
    if not raw and args.dependencies_file:
        with open(args.dependencies_file, encoding="utf-8") as fh:
            raw = fh.read().strip()
    if not raw:
        return []

    parsed = json.loads(raw)
    if not isinstance(parsed, list):
        raise ValueError("--dependencies-json must be a JSON array")

    normalized: list[dict[str, str]] = []
    for item in parsed:
        if not isinstance(item, dict):
            continue
        package = str(item.get("package", item.get("name", ""))).strip()
        version = str(item.get("version", "")).strip()
        ecosystem = str(item.get("ecosystem", "")).strip()
        if package:
            normalized.append({"package": package, "version": version, "ecosystem": ecosystem})
    return normalized


def _register_dependencies(
    api: SobsApi,
    release_id: str,
    deps: list[dict[str, str]],
    dep_name: str,
    dry_run: bool,
) -> int:
    """Register a dependency list as a 'dependencies-lockfile' artifact.

    Stored as a single artifact row with the dependency array in MetadataJson,
    keyed by (ArtifactType='dependencies-lockfile', Name=dep_name).
    Any previous row with the same key is superseded by the ReplacingMergeTree.
    """
    if not deps:
        return 0

    artifact = {
        "artifactType": "dependencies-lockfile",
        "name": dep_name,
        "contentType": "application/json",
        "size": 0,
        "storageRef": "",
        "checksumSha256": "",
        "platform": "",
        "architecture": "",
        "metadata": {"dependencies": deps},
        "uploadedAt": "",
    }

    if dry_run:
        print(f"[dry-run] would register {len(deps)} dependencies as '{dep_name}'")
        return len(deps)

    api.post(f"/v1/releases/{urllib.parse.quote(release_id)}/artifacts/meta", artifact)
    return len(deps)


def _parse_optional_json(text: str, default: Any) -> Any:
    raw = str(text or "").strip()
    if not raw:
        return default
    return json.loads(raw)


def _load_artifacts(args: argparse.Namespace) -> list[dict[str, Any]]:
    if args.artifacts_file:
        with open(args.artifacts_file, encoding="utf-8") as handle:
            parsed = json.loads(handle.read())
    elif args.artifacts_json:
        parsed = json.loads(args.artifacts_json)
    else:
        parsed = _load_json_from_env_or_file("SOBS_RELEASE_ARTIFACTS_JSON", "SOBS_RELEASE_ARTIFACTS_JSON_FILE", [])

    if parsed is None:
        return []
    if not isinstance(parsed, list):
        raise ValueError("artifacts payload must be a JSON array")

    normalized: list[dict[str, Any]] = []
    for item in parsed:
        if not isinstance(item, dict):
            continue
        artifact_type = str(item.get("artifactType", "")).strip()
        name = str(item.get("name", "")).strip()
        if not artifact_type or not name:
            continue
        normalized.append(
            {
                "artifactType": artifact_type,
                "name": name,
                "contentType": str(item.get("contentType", "")).strip(),
                "size": int(item.get("size", 0) or 0),
                "storageRef": str(item.get("storageRef", "")).strip(),
                "checksumSha256": str(item.get("checksumSha256", "")).strip(),
                "platform": str(item.get("platform", "")).strip(),
                "architecture": str(item.get("architecture", "")).strip(),
                "metadata": item.get("metadata", {}),
                "uploadedAt": str(item.get("uploadedAt", "")).strip(),
            }
        )
    return normalized


def _find_or_create_app(api: SobsApi, args: argparse.Namespace, dry_run: bool) -> dict[str, Any]:
    app_name = str(args.app_name or "").strip()
    if not app_name:
        raise ValueError("--app-name (or SOBS_APP_NAME) is required")

    app_slug = str(args.app_slug or "").strip() or _slugify(app_name)
    if not dry_run:
        apps = api.get("/v1/apps") or []
        for app in apps:
            if not isinstance(app, dict):
                continue
            if str(app.get("slug", "")) == app_slug or str(app.get("name", "")) == app_name:
                print(f"Using existing app: {app.get('id')} ({app.get('slug')})")
                return app

    payload = {
        "name": app_name,
        "slug": app_slug,
        "ownerTeam": str(args.owner_team or "").strip(),
        "repoUrl": str(args.repo_url or "").strip(),
        "defaultEnvironment": str(args.default_environment or "").strip(),
        "metadata": _parse_optional_json(args.app_metadata_json, {}),
    }

    if dry_run:
        fake = dict(payload)
        fake["id"] = "dry-run-app"
        print(f"[dry-run] would create app: {json.dumps(payload, ensure_ascii=False)}")
        return fake

    created = api.post("/v1/apps", payload)
    print(f"Created app: {created.get('id')} ({created.get('slug')})")
    return created


def _find_or_create_release(
    api: SobsApi,
    app_id: str,
    args: argparse.Namespace,
    dry_run: bool,
) -> dict[str, Any]:
    release_version = str(args.release_version or "").strip()
    if not release_version:
        raise ValueError("--release-version (or SOBS_RELEASE_VERSION) is required")

    commit_sha = str(args.commit_sha or "").strip()
    build_id = str(args.build_id or "").strip()
    environment = str(args.environment or "").strip()

    if not dry_run:
        releases = api.get(f"/v1/apps/{urllib.parse.quote(app_id)}/releases") or []
        for rel in releases:
            if not isinstance(rel, dict):
                continue
            if (
                str(rel.get("version", "")) == release_version
                and str(rel.get("commitSha", "")) == commit_sha
                and str(rel.get("environment", "")) == environment
                and str(rel.get("buildId", "")) == build_id
            ):
                print(f"Using existing release: {rel.get('id')} ({rel.get('version')})")
                return rel

    payload = {
        "version": release_version,
        "commitSha": commit_sha,
        "buildId": build_id,
        "environment": environment,
        "releasedAt": str(args.released_at or "").strip(),
        "metadata": _parse_optional_json(args.release_metadata_json, {}),
    }

    if dry_run:
        fake = dict(payload)
        fake["id"] = "dry-run-release"
        print(f"[dry-run] would create release: {json.dumps(payload, ensure_ascii=False)}")
        return fake

    created = api.post(f"/v1/apps/{urllib.parse.quote(app_id)}/releases", payload)
    print(f"Created release: {created.get('id')} ({created.get('version')})")
    return created


def _upsert_artifact_meta(
    api: SobsApi,
    release_id: str,
    artifacts: list[dict[str, Any]],
    dry_run: bool,
) -> tuple[int, int]:
    existing_keys = set()
    if not dry_run:
        existing = api.get(f"/v1/releases/{urllib.parse.quote(release_id)}/artifacts") or []
        existing_keys = {
            (
                str(item.get("artifactType", "")),
                str(item.get("name", "")),
                str(item.get("storageRef", "")),
            )
            for item in existing
            if isinstance(item, dict)
        }

    created = 0
    skipped = 0
    for artifact in artifacts:
        key = (
            str(artifact.get("artifactType", "")),
            str(artifact.get("name", "")),
            str(artifact.get("storageRef", "")),
        )
        if key in existing_keys:
            skipped += 1
            continue

        if dry_run:
            print(f"[dry-run] would register artifact: {json.dumps(artifact, ensure_ascii=False)}")
            created += 1
            continue

        api.post(f"/v1/releases/{urllib.parse.quote(release_id)}/artifacts/meta", artifact)
        created += 1

    return created, skipped


def main() -> int:
    args = parse_args()

    try:
        artifacts = _load_artifacts(args)
        deps = _load_dependencies(args)
        api = SobsApi(args.base_url, args.api_key, args.timeout)

        app = _find_or_create_app(api, args, args.dry_run)
        app_id = str(app.get("id", "")).strip()
        if not app_id:
            raise RuntimeError("app id is missing")

        release = _find_or_create_release(api, app_id, args, args.dry_run)
        release_id = str(release.get("id", "")).strip()
        if not release_id:
            raise RuntimeError("release id is missing")

        created, skipped = _upsert_artifact_meta(api, release_id, artifacts, args.dry_run)
        deps_registered = _register_dependencies(api, release_id, deps, args.dependencies_name, args.dry_run)

        print("Done.")
        print(f"  app_id={app_id}")
        print(f"  release_id={release_id}")
        print(f"  artifacts_registered={created}")
        print(f"  artifacts_skipped_existing={skipped}")
        if deps_registered:
            print(f"  dependencies_registered={deps_registered} (source: {args.dependencies_name})")
        return 0
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
