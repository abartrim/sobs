"""Onboarding, setup wizard, and RUM asset serving routes blueprint."""

from __future__ import annotations

import os
import time
import uuid
from typing import Any

from quart import Blueprint, jsonify, request, send_from_directory

import app as sobs_app
from app import (  # noqa: E402
    _CI_PUSH_API_KEY_DEFAULT_TTL_DAYS,
    _WIZARD_DEPLOYMENTS,
    _WIZARD_ENVS,
    _WIZARD_LANGUAGES,
    _app_slug,
    _assign_issue_to_copilot,
    _build_ci_metadata_issue_body,
    _build_github_repo_url,
    _build_otel_audit_issue_body,
    _build_setup_wizard_steps,
    _ci_push_api_key_status,
    _find_app_id_by_repo_url,
    _github_api_headers,
    _inspect_repo_for_onboarding,
    _load_ai_setting,
    _load_repo_scoped_github_token,
    _normalize_github_token_expiry_input,
    _now_iso,
    _parse_github_repo_owner_name,
    _persist_onboarding_work_item,
    _resolve_github_repo_fields,
    _rotate_ci_push_api_key,
    _rum_etag,
    _save_ai_setting,
    _save_repo_scoped_github_token,
    _set_ci_push_realtime_enabled,
    get_db,
    require_basic_auth,
)

onboarding_bp: Blueprint = Blueprint("onboarding", __name__)


async def _get_async_http_client(*args: Any, **kwargs: Any):
    return await sobs_app._get_async_http_client(*args, **kwargs)


async def _create_or_update_onboarding_issue(*args: Any, **kwargs: Any):
    return await sobs_app._create_or_update_onboarding_issue(*args, **kwargs)


def _insert_rows_json_each_row(*args: Any, **kwargs: Any):
    return sobs_app._insert_rows_json_each_row(*args, **kwargs)


@onboarding_bp.route("/static/rum.js")
async def rum_js():
    static_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), "static")
    etag = _rum_etag(os.path.join(static_dir, "rum.js"))
    response = await send_from_directory(static_dir, "rum.js", mimetype="application/javascript")
    response.headers["ETag"] = f'"{etag}"'
    response.headers["X-SourceMap"] = "rum.js.map"
    response.headers["SourceMap"] = "rum.js.map"
    return response


@onboarding_bp.route("/static/rum.js.map")
async def rum_js_map():
    static_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), "static")
    map_path = os.path.join(static_dir, "rum.js.map")
    if not os.path.isfile(map_path):
        return "", 404
    return await send_from_directory(static_dir, "rum.js.map", mimetype="application/json")


@onboarding_bp.route("/static/rum.min.js")
async def rum_min_js():
    static_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), "static")
    etag = _rum_etag(os.path.join(static_dir, "rum.min.js"))
    response = await send_from_directory(static_dir, "rum.min.js", mimetype="application/javascript")
    response.headers["ETag"] = f'"{etag}"'
    return response


@onboarding_bp.route("/static/rum.min.js.map")
async def rum_min_js_map():
    static_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), "static")
    return await send_from_directory(static_dir, "rum.min.js.map", mimetype="application/json")


@onboarding_bp.route("/static/rum.d.ts")
async def rum_d_ts():
    static_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), "static")
    return await send_from_directory(static_dir, "rum.d.ts", mimetype="text/plain; charset=utf-8")


@onboarding_bp.route("/api/setup-wizard/steps", methods=["GET"])
@require_basic_auth
async def api_setup_wizard_steps():
    """Return tailored OTEL setup steps for the given context.

    Query parameters
    ----------------
    env         ``dev`` or ``prod`` (default: ``dev``)
    language    One of ``python``, ``node``, ``go``, ``java``, ``dotnet``, ``ruby``, ``php``
                (default: ``python``)
    deployment  One of ``docker``, ``kubernetes``, ``baremetal``, ``cloud``
                (default: ``docker``)
    """
    env = request.args.get("env", "dev").strip().lower()
    language = request.args.get("language", "python").strip().lower()
    deployment = request.args.get("deployment", "docker").strip().lower()

    if env not in _WIZARD_ENVS:
        return jsonify({"ok": False, "error": f"Invalid env '{env}'. Must be one of: {sorted(_WIZARD_ENVS)}"}), 400
    if language not in _WIZARD_LANGUAGES:
        return (
            jsonify(
                {"ok": False, "error": f"Invalid language '{language}'. Must be one of: {sorted(_WIZARD_LANGUAGES)}"}
            ),
            400,
        )
    if deployment not in _WIZARD_DEPLOYMENTS:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": f"Invalid deployment '{deployment}'. Must be one of: {sorted(_WIZARD_DEPLOYMENTS)}",
                }
            ),
            400,
        )

    result = _build_setup_wizard_steps(env, language, deployment)
    return jsonify({"ok": True, **result})


@onboarding_bp.route("/api/onboarding/create-repo", methods=["POST"])
@require_basic_auth
async def api_onboarding_create_repo():
    """Create a repository entry for onboarding wizard and return JSON details."""
    db = get_db()
    body = await request.get_json(silent=True) or {}

    name = str(body.get("name", "") or "").strip()
    slug_raw = str(body.get("slug", "") or "").strip()
    repo_url_input = str(body.get("repo_url", "") or "").strip()
    repo_owner_input = str(body.get("repo_owner", "") or "").strip()
    repo_name_input = str(body.get("repo_name", "") or "").strip()
    repo_url, owner, repo = _resolve_github_repo_fields(repo_url_input, repo_owner_input, repo_name_input)
    default_environment = str(body.get("default_environment", "") or "").strip()
    github_token = str(body.get("github_token", "") or "").strip()
    github_token_expiry = _normalize_github_token_expiry_input(body.get("github_token_expires_at") or "")
    set_github_token = bool(body.get("set_github_token", False))
    set_repo_token = bool(body.get("set_repo_token", True))
    set_agent_repo = bool(body.get("set_agent_repo", True))

    if not name or not repo_url:
        return jsonify({"ok": False, "error": "App name and repository are required"}), 400

    slug = _app_slug(slug_raw or name)
    existing = db.execute(
        "SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
        [slug],
    ).fetchone()
    if existing:
        return jsonify({"ok": False, "error": "App slug already exists"}), 409

    version = int(time.time() * 1000)
    row = {
        "Id": uuid.uuid4().hex,
        "Name": name,
        "Slug": slug,
        "OwnerTeam": "",
        "RepoUrl": repo_url,
        "DefaultEnvironment": default_environment,
        "Enabled": 1,
        "MetadataJson": "{}",
        "IsDeleted": 0,
        "Version": version,
        "CreatedAt": _now_iso(),
        "UpdatedAt": _now_iso(),
    }
    _insert_rows_json_each_row(db, "sobs_apps", [row])

    if set_github_token and github_token:
        _save_ai_setting(db, "ai.github_token", github_token)
        _save_ai_setting(db, "ai.github_token_expires_at", github_token_expiry)
        _save_ai_setting(db, "ai.github_token_last_validated_at", "")
        _save_ai_setting(db, "ai.github_token_last_validation_status", "")
        _save_ai_setting(db, "ai.github_token_last_validation_message", "")

    if set_repo_token and github_token and owner and repo:
        _save_repo_scoped_github_token(db, owner, repo, github_token)

    if set_agent_repo and owner and repo:
        _save_ai_setting(db, "ai.github_repo", f"{owner}/{repo}")

    return jsonify(
        {
            "ok": True,
            "app_id": str(row["Id"]),
            "name": name,
            "slug": slug,
            "repo_url": repo_url,
            "owner": owner,
            "repo": repo,
        }
    )


@onboarding_bp.route("/api/onboarding/import-repo", methods=["POST"])
@require_basic_auth
async def api_onboarding_import_repo():
    """Fetch repository metadata from GitHub for onboarding form auto-fill."""
    db = get_db()
    body = await request.get_json(silent=True) or {}

    repo_url_input = str(body.get("repo_url", "") or "").strip()
    repo_owner_input = str(body.get("repo_owner", "") or "").strip()
    repo_name_input = str(body.get("repo_name", "") or "").strip()
    repo_url, owner, repo = _resolve_github_repo_fields(repo_url_input, repo_owner_input, repo_name_input)
    token_override = str(body.get("github_token", "") or "").strip()

    if not owner or not repo:
        return jsonify({"ok": False, "error": "Enter a valid GitHub owner and repository name"}), 400

    github_token = token_override or _load_ai_setting(db, "ai.github_token", "").strip()
    if github_token:
        headers = _github_api_headers(github_token)
    else:
        headers = {
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        }

    client = await _get_async_http_client()
    try:
        resp = await client.get(
            f"https://api.github.com/repos/{owner}/{repo}",
            headers=headers,
            timeout=15,
        )
        payload = resp.json() if resp.content else {}
    except Exception as exc:
        return jsonify({"ok": False, "error": f"GitHub lookup failed: {exc}"}), 502

    if resp.status_code != 200:
        detail = ""
        if isinstance(payload, dict):
            detail = str(payload.get("message") or "").strip()
        return jsonify({"ok": False, "error": detail or f"GitHub lookup failed ({resp.status_code})"}), 400

    if not isinstance(payload, dict):
        return jsonify({"ok": False, "error": "Unexpected GitHub response payload"}), 502

    full_name = str(payload.get("full_name") or f"{owner}/{repo}").strip()
    imported_repo_url = str(payload.get("html_url") or f"https://github.com/{owner}/{repo}").strip()
    suggested_name = str(payload.get("name") or repo).strip() or repo

    return jsonify(
        {
            "ok": True,
            "owner": owner,
            "repo": repo,
            "full_name": full_name,
            "repo_url": imported_repo_url,
            "name": suggested_name,
            "slug": _app_slug(suggested_name),
            "default_branch": str(payload.get("default_branch") or ""),
            "visibility": str(payload.get("visibility") or "public"),
            "description": str(payload.get("description") or ""),
        }
    )


@onboarding_bp.route("/api/onboarding/list-repos", methods=["POST"])
@require_basic_auth
async def api_onboarding_list_repos():
    """List repositories for an owner/user to support onboarding autocomplete."""
    db = get_db()
    body = await request.get_json(silent=True) or {}

    owner = str(body.get("owner", "") or "").strip().strip("/")
    token_override = str(body.get("github_token", "") or "").strip()
    if not owner:
        return jsonify({"ok": False, "error": "Owner or username is required"}), 400

    github_token = token_override or _load_ai_setting(db, "ai.github_token", "").strip()
    token_used = bool(github_token)
    headers = (
        _github_api_headers(github_token)
        if github_token
        else {
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        }
    )

    endpoints: list[str] = []
    if token_used:
        endpoints.append(f"https://api.github.com/users/{owner}/repos?per_page=100&type=all&sort=full_name")
        endpoints.append(f"https://api.github.com/orgs/{owner}/repos?per_page=100&type=all&sort=full_name")
    else:
        endpoints.append(f"https://api.github.com/users/{owner}/repos?per_page=100&type=public&sort=full_name")
        endpoints.append(f"https://api.github.com/orgs/{owner}/repos?per_page=100&type=public&sort=full_name")

    client = await _get_async_http_client()
    payload: Any = None
    response_status = 0
    for url in endpoints:
        try:
            resp = await client.get(url, headers=headers, timeout=15)
        except Exception as exc:
            return jsonify({"ok": False, "error": f"GitHub lookup failed: {exc}"}), 502

        response_status = int(resp.status_code)
        payload = resp.json() if resp.content else None
        if response_status == 200:
            break

    if response_status != 200 or not isinstance(payload, list):
        detail = ""
        if isinstance(payload, dict):
            detail = str(payload.get("message") or "").strip()
        return jsonify({"ok": False, "error": detail or f"GitHub lookup failed ({response_status})"}), 400

    repos: list[dict[str, Any]] = []
    for item in payload:
        if not isinstance(item, dict):
            continue
        repo_name = str(item.get("name") or "").strip()
        if not repo_name:
            continue
        repo_owner = str((item.get("owner") or {}).get("login") or owner).strip()
        repos.append(
            {
                "name": repo_name,
                "full_name": str(item.get("full_name") or f"{repo_owner}/{repo_name}").strip(),
                "repo_url": str(item.get("html_url") or _build_github_repo_url(repo_owner, repo_name)).strip(),
                "private": bool(item.get("private", False)),
            }
        )

    repos.sort(key=lambda r: str(r.get("name", "")).lower())
    return jsonify(
        {
            "ok": True,
            "owner": owner,
            "repos": repos,
            "token_used": token_used,
            "visibility_note": ("Need PAT to see private repositories." if not token_used else ""),
        }
    )


@onboarding_bp.route("/api/onboarding/inspect-repo", methods=["GET"])
@require_basic_auth
async def api_onboarding_inspect_repo():
    """Inspect a configured repository for Sobs onboarding readiness.

    Query parameters
    ----------------
    app_id   UUID of the app in ``sobs_apps`` (preferred)
    repo     ``owner/repo`` or full GitHub URL (fallback if app_id not provided)
    """
    db = get_db()
    app_id = request.args.get("app_id", "").strip()
    repo_param = request.args.get("repo", "").strip()

    repo_url = ""
    if app_id:
        row = db.execute(
            "SELECT RepoUrl FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
            [app_id],
        ).fetchone()
        if not row:
            return jsonify({"ok": False, "error": "App not found"}), 404
        repo_url = str(row[0] or "")
    elif repo_param:
        repo_url = repo_param
    else:
        return jsonify({"ok": False, "error": "app_id or repo parameter required"}), 400

    owner, repo = _parse_github_repo_owner_name(repo_url)
    if not owner or not repo:
        return jsonify({"ok": False, "error": f"Could not parse owner/repo from '{repo_url}'"}), 400

    # Resolve token: repo-scoped first, then global fallback
    github_token = _load_repo_scoped_github_token(db, owner, repo)
    if not github_token:
        github_token = _load_ai_setting(db, "ai.github_token", "").strip()
    if not github_token:
        return jsonify(
            {
                "ok": True,
                "owner": owner,
                "repo": repo,
                "has_github_actions": False,
                "sobs_ci_found": False,
                "sobs_otel_found": False,
                "copilot_available": False,
                "workflow_files": [],
                "error": "No GitHub token configured for this repository",
            }
        )

    result = await _inspect_repo_for_onboarding(github_token, owner, repo)
    return jsonify({"ok": True, "owner": owner, "repo": repo, **result})


@onboarding_bp.route("/api/onboarding/create-issues", methods=["POST"])
@require_basic_auth
async def api_onboarding_create_issues():
    """Create onboarding GitHub issues (CI metadata and/or OTEL audit).

    JSON body
    ---------
    app_id          UUID of the app in ``sobs_apps``
    repo            ``owner/repo`` fallback if app_id not provided
    create_ci       bool — create CI metadata setup issue
    create_otel     bool — create OTEL & RUM audit issue
    assign_copilot  bool — attempt to assign both issues to Copilot
    has_github_actions  bool — passed from inspection result (affects issue body)
    enable_realtime_support bool — include manual realtime CI setup guidance and key state
    """
    db = get_db()
    body = await request.get_json(silent=True) or {}

    app_id = str(body.get("app_id", "") or "").strip()
    repo_param = str(body.get("repo", "") or "").strip()
    create_ci = bool(body.get("create_ci", True))
    create_otel = bool(body.get("create_otel", True))
    assign_copilot = bool(body.get("assign_copilot", False))
    has_github_actions = bool(body.get("has_github_actions", True))
    enable_realtime_support = bool(body.get("enable_realtime_support", False))

    if not create_ci and not create_otel and not enable_realtime_support:
        return jsonify({"ok": False, "error": "Select at least one issue type or enable realtime support"}), 400

    repo_url = ""
    if app_id:
        row = db.execute(
            "SELECT RepoUrl FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
            [app_id],
        ).fetchone()
        if not row:
            return jsonify({"ok": False, "error": "App not found"}), 404
        repo_url = str(row[0] or "")
    elif repo_param:
        repo_url = repo_param
    else:
        return jsonify({"ok": False, "error": "app_id or repo parameter required"}), 400

    owner, repo = _parse_github_repo_owner_name(repo_url)
    if not owner or not repo:
        return jsonify({"ok": False, "error": f"Could not parse owner/repo from '{repo_url}'"}), 400

    github_token = _load_repo_scoped_github_token(db, owner, repo)
    if not github_token:
        github_token = _load_ai_setting(db, "ai.github_token", "").strip()
    if not github_token:
        return jsonify({"ok": False, "error": "No GitHub token configured for this repository"}), 400

    github_repo = f"{owner}/{repo}"
    results: dict[str, Any] = {"ok": True, "ci_issue": None, "otel_issue": None, "realtime": None}

    if enable_realtime_support:
        realtime_app_id = str(app_id or "").strip()
        if not realtime_app_id and repo_url:
            realtime_app_id = _find_app_id_by_repo_url(db, repo_url)

        if not realtime_app_id:
            return jsonify({"ok": False, "error": "Realtime support requires a saved repository app."}), 400

        key_plain = ""
        key_status = _ci_push_api_key_status(db, realtime_app_id)
        if (not key_status.get("configured")) or str((key_status.get("expiry") or {}).get("state") or "") == "expired":
            key_plain, _ = _rotate_ci_push_api_key(db, realtime_app_id, _CI_PUSH_API_KEY_DEFAULT_TTL_DAYS)
            key_status = _ci_push_api_key_status(db, realtime_app_id)
        _set_ci_push_realtime_enabled(db, realtime_app_id, True)
        app_id_for_example = realtime_app_id or "<APP_ID>"
        results["realtime"] = {
            "app_id": realtime_app_id,
            "enabled": True,
            "configured": bool(key_status.get("configured")),
            "expires_at": str(key_status.get("expires_at") or ""),
            "expiry_state": str((key_status.get("expiry") or {}).get("state") or "unknown"),
            "expiry_message": str((key_status.get("expiry") or {}).get("message") or ""),
            "api_key": key_plain,
            "api_key_show_once": bool(key_plain),
            "instructions": {
                "required_secrets": ["SOBS_URL", "SOBS_INGEST_API_KEY", "SOBS_APP_ID"],
                "curl_example": (
                    f"curl -sS -X POST '$SOBS_URL/v1/apps/{app_id_for_example}/releases' "
                    "-H 'X-API-Key: $SOBS_INGEST_API_KEY' "
                    "-H 'Content-Type: application/json' "
                    '-d \'{"version":"$VERSION","commitSha":"$COMMIT_SHA","buildId":"$BUILD_ID"}\''
                ),
                "webhook_note": "Optional: add a GitHub webhook for push/workflow events to reduce polling latency.",
            },
        }

    if create_ci:
        ci_body = _build_ci_metadata_issue_body(owner, repo, has_github_actions)
        ci_result = await _create_or_update_onboarding_issue(
            github_token,
            github_repo,
            f"[Sobs] Set up CI metadata scripts for {repo}",
            ci_body,
            labels=["sobs-onboarding", "ci-metadata"],
        )
        if "error" in ci_result:
            results["ci_issue"] = {"error": ci_result["error"]}
        else:
            issue_url = str(ci_result.get("issue_url", ""))
            issue_number = int(ci_result.get("issue_number", 0) or 0)
            issue_status = str(ci_result.get("status") or "")
            issue_note = str(ci_result.get("note") or "")
            copilot_assignment_status = "not_requested"
            copilot_assignment_reason = ""
            copilot_assignment_requested_at = 0
            if assign_copilot and issue_number:
                (
                    copilot_assignment_status,
                    copilot_assignment_reason,
                    copilot_assignment_requested_at,
                ) = await _assign_issue_to_copilot(github_token, github_repo, issue_number)
            results["ci_issue"] = {
                "url": issue_url,
                "number": issue_number,
                "status": issue_status,
                "note": issue_note,
                "copilot_status": copilot_assignment_status,
                "copilot_assignment_status": copilot_assignment_status,
                "copilot_assignment_reason": copilot_assignment_reason,
                "copilot_assignment_requested_at": copilot_assignment_requested_at,
            }
            if issue_status in ("created", "updated"):
                _persist_onboarding_work_item(
                    db,
                    github_repo=github_repo,
                    issue_url=issue_url,
                    issue_number=issue_number,
                    issue_title=str(ci_result.get("issue_title") or f"[Sobs] Set up CI metadata scripts for {repo}"),
                    issue_state=str(ci_result.get("issue_state") or "open"),
                    dedup_decision=issue_status,
                    note=issue_note,
                    copilot_assignment_status=copilot_assignment_status,
                    copilot_assignment_reason=copilot_assignment_reason,
                    copilot_assignment_requested_at=copilot_assignment_requested_at,
                    issue_type="ci",
                )

    if create_otel:
        otel_body = _build_otel_audit_issue_body(owner, repo)
        otel_result = await _create_or_update_onboarding_issue(
            github_token,
            github_repo,
            f"[Sobs] OTEL & RUM telemetry audit for {repo}",
            otel_body,
            labels=["sobs-onboarding", "observability"],
        )
        if "error" in otel_result:
            results["otel_issue"] = {"error": otel_result["error"]}
        else:
            issue_url = str(otel_result.get("issue_url", ""))
            issue_number = int(otel_result.get("issue_number", 0) or 0)
            issue_status = str(otel_result.get("status") or "")
            issue_note = str(otel_result.get("note") or "")
            copilot_assignment_status = "not_requested"
            copilot_assignment_reason = ""
            copilot_assignment_requested_at = 0
            if assign_copilot and issue_number:
                (
                    copilot_assignment_status,
                    copilot_assignment_reason,
                    copilot_assignment_requested_at,
                ) = await _assign_issue_to_copilot(github_token, github_repo, issue_number)
            results["otel_issue"] = {
                "url": issue_url,
                "number": issue_number,
                "status": issue_status,
                "note": issue_note,
                "copilot_status": copilot_assignment_status,
                "copilot_assignment_status": copilot_assignment_status,
                "copilot_assignment_reason": copilot_assignment_reason,
                "copilot_assignment_requested_at": copilot_assignment_requested_at,
            }
            if issue_status in ("created", "updated"):
                _persist_onboarding_work_item(
                    db,
                    github_repo=github_repo,
                    issue_url=issue_url,
                    issue_number=issue_number,
                    issue_title=str(otel_result.get("issue_title") or f"[Sobs] OTEL & RUM telemetry audit for {repo}"),
                    issue_state=str(otel_result.get("issue_state") or "open"),
                    dedup_decision=issue_status,
                    note=issue_note,
                    copilot_assignment_status=copilot_assignment_status,
                    copilot_assignment_reason=copilot_assignment_reason,
                    copilot_assignment_requested_at=copilot_assignment_requested_at,
                    issue_type="observability",
                )

    return jsonify(results)
