"""
routes/apps.py – App registry, release and artifact metadata endpoints.

Blueprint: apps_bp
Prefix: none (routes keep their original /v1/* paths)

Routes moved from app.py (original lines 10296–10518):
  GET   /v1/apps
  POST  /v1/apps
  GET   /v1/apps/<app_id>
  PATCH /v1/apps/<app_id>
  GET   /v1/apps/<app_id>/releases
  POST  /v1/apps/<app_id>/releases
  GET   /v1/releases/<release_id>
  GET   /v1/releases/<release_id>/artifacts
  POST  /v1/releases/<release_id>/artifacts/meta

Helpers remain in app.py (shared with settings and ingest routes).
This blueprint imports them via a deferred module-level import that is safe
because app.py registers this blueprint at the very end of its module body,
after all helpers are defined.
"""

from __future__ import annotations

import time
import uuid

from quart import Blueprint, request

# These imports are resolved when app.py executes its final registration step.
# All symbols are defined in app.py before that point.
from app import (  # noqa: E402
    _app_slug,
    _find_app_by_id,
    _find_release_by_id,
    _insert_rows_json_each_row,
    _now_iso,
    _parse_bool,
    _safe_json_dumps,
    _safe_json_loads,
    _serialize_app_row,
    get_db,
    jsonify,
    require_api_key,
)

apps_bp = Blueprint("apps", __name__)


# ---------------------------------------------------------------------------
# Helpers used exclusively by this blueprint (moved from app.py Milestone 3)
# ---------------------------------------------------------------------------


def _serialize_release_row(row: dict) -> dict:
    return {
        "id": str(row.get("Id", "")),
        "appId": str(row.get("AppId", "")),
        "version": str(row.get("ReleaseVersion", "")),
        "commitSha": str(row.get("CommitSha", "")),
        "buildId": str(row.get("BuildId", "")),
        "environment": str(row.get("Environment", "")),
        "releasedAt": str(row.get("ReleasedAt", "")),
        "metadata": _safe_json_loads(row.get("MetadataJson", ""), {}),
    }


def _serialize_artifact_row(row: dict) -> dict:
    return {
        "id": str(row.get("Id", "")),
        "releaseId": str(row.get("ReleaseId", "")),
        "artifactType": str(row.get("ArtifactType", "")),
        "name": str(row.get("Name", "")),
        "contentType": str(row.get("ContentType", "")),
        "size": int(row.get("Size", 0) or 0),
        "storageRef": str(row.get("StorageRef", "")),
        "checksumSha256": str(row.get("ChecksumSha256", "")),
        "platform": str(row.get("Platform", "")),
        "architecture": str(row.get("Architecture", "")),
        "metadata": _safe_json_loads(row.get("MetadataJson", ""), {}),
        "uploadedAt": str(row.get("UploadedAt", "")),
    }


# ---------------------------------------------------------------------------
# App registry
# ---------------------------------------------------------------------------


@apps_bp.route("/v1/apps", methods=["GET"])
@require_api_key
async def list_apps():
    db = get_db()
    q = (request.args.get("q") or "").strip().lower()
    rows = [dict(r) for r in db.execute("SELECT * FROM sobs_apps FINAL WHERE IsDeleted=0 ORDER BY Name ASC").fetchall()]
    apps = [_serialize_app_row(row) for row in rows]
    if q:
        apps = [item for item in apps if q in item["name"].lower() or q in item["slug"].lower()]
    return jsonify(apps), 200


@apps_bp.route("/v1/apps", methods=["POST"])
@require_api_key
async def create_app_registry_entry():
    db = get_db()
    payload = await request.get_json(force=True, silent=True) or {}
    name = str(payload.get("name", "")).strip()
    if not name:
        return jsonify({"error": "name is required"}), 400

    slug = _app_slug(str(payload.get("slug", "")).strip() or name)
    existing = db.execute(
        "SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
        [slug],
    ).fetchone()
    if existing:
        return jsonify({"error": "app slug already exists"}), 409

    version = int(time.time() * 1000)
    app_id = str(payload.get("id", "")).strip() or uuid.uuid4().hex
    row = {
        "Id": app_id,
        "Name": name,
        "Slug": slug,
        "OwnerTeam": str(payload.get("ownerTeam", "")).strip(),
        "RepoUrl": str(payload.get("repoUrl", "")).strip(),
        "DefaultEnvironment": str(payload.get("defaultEnvironment", "")).strip(),
        "Enabled": 1 if _parse_bool(payload.get("enabled", True), True) else 0,
        "MetadataJson": _safe_json_dumps(payload.get("metadata", {})),
        "IsDeleted": 0,
        "Version": version,
        "CreatedAt": _now_iso(),
        "UpdatedAt": _now_iso(),
    }
    _insert_rows_json_each_row(db, "sobs_apps", [row])
    return jsonify(_serialize_app_row(row)), 201


@apps_bp.route("/v1/apps/<app_id>", methods=["GET"])
@require_api_key
async def get_app_registry_entry(app_id: str):
    db = get_db()
    row = _find_app_by_id(db, app_id)
    if not row:
        return jsonify({"error": "not found"}), 404
    return jsonify(_serialize_app_row(row)), 200


@apps_bp.route("/v1/apps/<app_id>", methods=["PATCH"])
@require_api_key
async def update_app_registry_entry(app_id: str):
    db = get_db()
    current = _find_app_by_id(db, app_id)
    if not current:
        return jsonify({"error": "not found"}), 404

    payload = await request.get_json(force=True, silent=True) or {}
    name = str(payload.get("name", current.get("Name", ""))).strip()
    if not name:
        return jsonify({"error": "name is required"}), 400

    slug = _app_slug(str(payload.get("slug", current.get("Slug", ""))).strip() or name)
    conflict = db.execute(
        "SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 AND Id!=? LIMIT 1",
        [slug, app_id],
    ).fetchone()
    if conflict:
        return jsonify({"error": "app slug already exists"}), 409

    version = int(time.time() * 1000)
    row = {
        "Id": app_id,
        "Name": name,
        "Slug": slug,
        "OwnerTeam": str(payload.get("ownerTeam", current.get("OwnerTeam", ""))).strip(),
        "RepoUrl": str(payload.get("repoUrl", current.get("RepoUrl", ""))).strip(),
        "DefaultEnvironment": str(payload.get("defaultEnvironment", current.get("DefaultEnvironment", ""))).strip(),
        "Enabled": 1 if _parse_bool(payload.get("enabled", int(current.get("Enabled", 1))), True) else 0,
        "MetadataJson": _safe_json_dumps(
            payload.get("metadata", _safe_json_loads(current.get("MetadataJson", ""), {}))
        ),
        "IsDeleted": 0,
        "Version": version,
        "CreatedAt": str(current.get("CreatedAt", "")) or _now_iso(),
        "UpdatedAt": _now_iso(),
    }
    _insert_rows_json_each_row(db, "sobs_apps", [row])
    return jsonify(_serialize_app_row(row)), 200


# ---------------------------------------------------------------------------
# Releases
# ---------------------------------------------------------------------------


@apps_bp.route("/v1/apps/<app_id>/releases", methods=["GET"])
@require_api_key
async def list_app_releases(app_id: str):
    db = get_db()
    app_row = _find_app_by_id(db, app_id)
    if not app_row:
        return jsonify({"error": "app not found"}), 404
    rows = [
        _serialize_release_row(dict(r))
        for r in db.execute(
            "SELECT * FROM sobs_app_releases FINAL WHERE AppId=? AND IsDeleted=0 ORDER BY ReleasedAt DESC",
            [app_id],
        ).fetchall()
    ]
    return jsonify(rows), 200


@apps_bp.route("/v1/apps/<app_id>/releases", methods=["POST"])
@require_api_key
async def create_app_release(app_id: str):
    db = get_db()
    app_row = _find_app_by_id(db, app_id)
    if not app_row:
        return jsonify({"error": "app not found"}), 404

    payload = await request.get_json(force=True, silent=True) or {}
    release_version = str(payload.get("version", "")).strip()
    if not release_version:
        return jsonify({"error": "version is required"}), 400

    version = int(time.time() * 1000)
    row = {
        "Id": str(payload.get("id", "")).strip() or uuid.uuid4().hex,
        "AppId": app_id,
        "ReleaseVersion": release_version,
        "CommitSha": str(payload.get("commitSha", "")).strip(),
        "BuildId": str(payload.get("buildId", "")).strip(),
        "Environment": str(payload.get("environment", "")).strip(),
        "ReleasedAt": str(payload.get("releasedAt", "")).strip() or _now_iso(),
        "MetadataJson": _safe_json_dumps(payload.get("metadata", {})),
        "IsDeleted": 0,
        "Version": version,
    }
    _insert_rows_json_each_row(db, "sobs_app_releases", [row])
    return jsonify(_serialize_release_row(row)), 201


@apps_bp.route("/v1/releases/<release_id>", methods=["GET"])
@require_api_key
async def get_release(release_id: str):
    db = get_db()
    row = _find_release_by_id(db, release_id)
    if not row:
        return jsonify({"error": "not found"}), 404

    release = _serialize_release_row(row)
    artifacts = [
        _serialize_artifact_row(dict(r))
        for r in db.execute(
            "SELECT * FROM sobs_release_artifacts FINAL WHERE ReleaseId=? AND IsDeleted=0 ORDER BY UploadedAt DESC",
            [release_id],
        ).fetchall()
    ]
    return jsonify({"release": release, "artifacts": artifacts}), 200


@apps_bp.route("/v1/releases/<release_id>/artifacts", methods=["GET"])
@require_api_key
async def list_release_artifacts(release_id: str):
    db = get_db()
    row = _find_release_by_id(db, release_id)
    if not row:
        return jsonify({"error": "release not found"}), 404
    artifacts = [
        _serialize_artifact_row(dict(r))
        for r in db.execute(
            "SELECT * FROM sobs_release_artifacts FINAL WHERE ReleaseId=? AND IsDeleted=0 ORDER BY UploadedAt DESC",
            [release_id],
        ).fetchall()
    ]
    return jsonify(artifacts), 200


# ---------------------------------------------------------------------------
# Artifacts
# ---------------------------------------------------------------------------


@apps_bp.route("/v1/releases/<release_id>/artifacts/meta", methods=["POST"])
@require_api_key
async def create_release_artifact_meta(release_id: str):
    db = get_db()
    release = _find_release_by_id(db, release_id)
    if not release:
        return jsonify({"error": "release not found"}), 404

    payload = await request.get_json(force=True, silent=True) or {}
    artifact_type = str(payload.get("artifactType", "")).strip()
    name = str(payload.get("name", "")).strip()
    if not artifact_type or not name:
        return jsonify({"error": "artifactType and name are required"}), 400

    version = int(time.time() * 1000)
    row = {
        "Id": str(payload.get("id", "")).strip() or uuid.uuid4().hex,
        "ReleaseId": release_id,
        "ArtifactType": artifact_type,
        "Name": name,
        "ContentType": str(payload.get("contentType", "")).strip(),
        "Size": int(payload.get("size", 0) or 0),
        "StorageRef": str(payload.get("storageRef", "")).strip(),
        "ChecksumSha256": str(payload.get("checksumSha256", "")).strip(),
        "Platform": str(payload.get("platform", "")).strip(),
        "Architecture": str(payload.get("architecture", "")).strip(),
        "MetadataJson": _safe_json_dumps(payload.get("metadata", {})),
        "UploadedAt": str(payload.get("uploadedAt", "")).strip() or _now_iso(),
        "IsDeleted": 0,
        "Version": version,
    }
    _insert_rows_json_each_row(db, "sobs_release_artifacts", [row])
    return jsonify(_serialize_artifact_row(row)), 201
