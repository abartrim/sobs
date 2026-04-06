#!/usr/bin/env python3
"""Validate anomaly-triggered agent GitHub issue flows via running SOBS APIs.

Flow:
1) Auto-create anomaly rules from current telemetry.
2) Create two agent rules bound to anomaly events:
   - issue-only (github_issue)
   - issue+copilot (github_issue + github_issue_copilot)
3) Trigger /api/notifications/check to fire automatic agent runs.
4) Verify GitHub issue outcomes and formal Copilot assignment behavior.
5) Verify created issues and assignment state are visible in SOBS work-items API and UI.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request


def _post_form(base_url: str, path: str, data: dict[str, str | list[str]], timeout: int) -> tuple[int, str]:
    encoded_pairs: list[tuple[str, str]] = []
    for key, value in data.items():
        if isinstance(value, list):
            for item in value:
                encoded_pairs.append((key, str(item)))
        else:
            encoded_pairs.append((key, str(value)))

    payload = urllib.parse.urlencode(encoded_pairs).encode("utf-8")
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}{path}",
        data=payload,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
        return resp.getcode(), resp.read().decode("utf-8", errors="replace")


def _post_json(base_url: str, path: str, payload: dict, api_key: str, timeout: int) -> dict:
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}{path}",
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            **({"X-API-Key": api_key} if api_key else {}),
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
        body = resp.read().decode("utf-8", errors="replace")
        return json.loads(body) if body else {}


def _get_json(base_url: str, path: str, api_key: str, timeout: int) -> dict:
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}{path}",
        headers={
            "Accept": "application/json",
            **({"X-API-Key": api_key} if api_key else {}),
        },
        method="GET",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
        body = resp.read().decode("utf-8", errors="replace")
        return json.loads(body) if body else {}


def _get_text(base_url: str, path: str, timeout: int) -> str:
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}{path}",
        headers={"Accept": "text/html"},
        method="GET",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
        return resp.read().decode("utf-8", errors="replace")


def _verify_work_items_visibility(
    base_url: str,
    api_key: str,
    timeout: int,
    issue_only_url: str,
    issue_copilot_url: str,
) -> dict[str, object]:
    """Verify created issues appear in both /api/work-items and /work-items UI."""
    api_items: list[dict] = []
    html_text = ""
    matched_api_urls: set[str] = set()
    issue_only_assignment_status = ""
    issue_copilot_assignment_status = ""

    # Retry to absorb asynchronous persistence timing.
    for _ in range(12):
        payload = _get_json(base_url, "/api/work-items?limit=200", api_key, timeout)
        if bool(payload.get("ok")):
            api_items = list(payload.get("items") or [])
            for item in api_items:
                url = str(item.get("issue_url", "")).strip()
                if url in {issue_only_url, issue_copilot_url}:
                    matched_api_urls.add(url)
                    if url == issue_only_url:
                        issue_only_assignment_status = str(item.get("copilot_assignment_status", "")).strip()
                    if url == issue_copilot_url:
                        issue_copilot_assignment_status = str(item.get("copilot_assignment_status", "")).strip()

        try:
            html_text = _get_text(base_url, "/work-items", timeout)
        except urllib.error.URLError:
            html_text = ""

        if (
            issue_only_url in matched_api_urls
            and issue_copilot_url in matched_api_urls
            and issue_only_url in html_text
            and issue_copilot_url in html_text
        ):
            break
        time.sleep(0.8)

    if issue_only_url not in matched_api_urls or issue_copilot_url not in matched_api_urls:
        raise RuntimeError("created issues were not found in /api/work-items")
    if issue_only_url not in html_text or issue_copilot_url not in html_text:
        raise RuntimeError("created issues were not rendered on /work-items page")

    return {
        "api_items_seen": len(api_items),
        "api_matched_urls": sorted(matched_api_urls),
        "issue_only_assignment_status": issue_only_assignment_status,
        "issue_copilot_assignment_status": issue_copilot_assignment_status,
        "ui_contains_issue_only": issue_only_url in html_text,
        "ui_contains_issue_copilot": issue_copilot_url in html_text,
    }


def _parse_issue_url(url: str) -> tuple[str, str, int]:
    m = re.search(r"github\.com/([^/]+)/([^/]+)/issues/(\d+)", url)
    if not m:
        raise RuntimeError(f"unexpected issue URL: {url}")
    return m.group(1), m.group(2), int(m.group(3))


def _gh_issue_has_copilot_assignee(owner: str, repo: str, number: int) -> bool:
    cmd = [
        "gh",
        "api",
        f"repos/{owner}/{repo}/issues/{number}",
        "--jq",
        ".assignees[].login",
    ]
    out = subprocess.check_output(cmd, text=True)
    return "copilot-swe-agent" in out.lower()


def _extract_input_value(html: str, field_name: str) -> str:
    m = re.search(rf'name="{re.escape(field_name)}"[^>]*value="([^"]*)"', html)
    return m.group(1).strip() if m else ""


def _preflight_settings(base_url: str, timeout: int) -> dict[str, str]:
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/settings/ai",
        headers={"Accept": "text/html"},
        method="GET",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
        html = resp.read().decode("utf-8", errors="replace")

    github_repo = _extract_input_value(html, "github_repo")
    endpoint_url = _extract_input_value(html, "endpoint_url")
    model = _extract_input_value(html, "model")
    max_issues = _extract_input_value(html, "agent_max_issues_per_hour")
    max_assignments = _extract_input_value(html, "agent_max_assignments_per_hour")
    max_active_assignments = _extract_input_value(html, "agent_max_active_assignments")
    guard_model = _extract_input_value(html, "guard_model")

    if not endpoint_url or not model:
        raise RuntimeError("ai.endpoint_url and ai.model must be configured for agent rule execution")

    try:
        max_issues_n = int(max_issues or "0")
    except ValueError:
        max_issues_n = 0
    if max_issues_n < 2:
        raise RuntimeError(
            "ai.agent_max_issues_per_hour is less than 2. "
            "Set it to at least 2 for validating both issue-only and issue+copilot flows in one run."
        )
    try:
        max_assignments_n = int(max_assignments or "0")
    except ValueError:
        max_assignments_n = 0
    if max_assignments_n < 1:
        raise RuntimeError("ai.agent_max_assignments_per_hour must be at least 1 for Copilot assignment validation.")
    try:
        max_active_assignments_n = int(max_active_assignments or "0")
    except ValueError:
        max_active_assignments_n = 0
    if max_active_assignments_n < 1:
        raise RuntimeError("ai.agent_max_active_assignments must be at least 1 for Copilot assignment validation.")

    return {
        "github_repo_default": github_repo,
        "endpoint_url": endpoint_url,
        "model": model,
        "agent_max_issues_per_hour": str(max_issues_n),
        "agent_max_assignments_per_hour": str(max_assignments_n),
        "agent_max_active_assignments": str(max_active_assignments_n),
        "guard_model": guard_model,
    }


def _create_agent_rule(
    base_url: str,
    timeout: int,
    *,
    name: str,
    trigger_ref_id: str,
    actions: list[str],
) -> None:
    code, _body = _post_form(
        base_url,
        "/settings/agents",
        {
            "name": name,
            "description": "Validation: anomaly-triggered issue flow",
            "trigger_type": "anomaly_rule",
            "trigger_ref_id": trigger_ref_id,
            "trigger_state": "any",
            "actions": actions,
            "rate_limit_minutes": "1",
        },
        timeout,
    )
    if code not in (200, 302):
        raise RuntimeError(f"failed creating agent rule {name}: status={code}")


def run(args: argparse.Namespace) -> dict:
    preflight = _preflight_settings(args.base_url, args.timeout)

    run_suffix = str(int(time.time()))
    metric_rule_name = f"validation-metric-trigger-{run_suffix}"
    issue_only_rule = f"validation-issue-only-{run_suffix}"
    issue_copilot_rule = f"validation-issue-followup-{run_suffix}"

    # Create deterministic anomaly rule targeting validation telemetry.
    _post_form(
        args.base_url,
        "/metrics/rules",
        {
            "name": metric_rule_name,
            "rule_type": "threshold",
            "source": "traces",
            "signal": "trace_volume",
            "service": f"{args.prefix}-vuln-fixture",
            "attr_fp": "",
            "comparator": "gt",
            "warning_threshold": "0.0001",
            "critical_threshold": "0.0002",
            "min_sample_count": "1",
        },
        args.timeout,
    )

    rules_html_req = urllib.request.Request(
        f"{args.base_url.rstrip('/')}/metrics/rules",
        headers={"Accept": "text/html"},
        method="GET",
    )
    with urllib.request.urlopen(rules_html_req, timeout=args.timeout) as resp:  # noqa: S310
        rules_html = resp.read().decode("utf-8", errors="replace")

    id_match = re.search(
        rf'data-rule-id="([^"]+)"[^>]*data-rule-name="{re.escape(metric_rule_name)}"',
        rules_html,
    )
    if not id_match:
        raise RuntimeError(f"failed to find created metric rule id for {metric_rule_name}")
    metric_rule_id = id_match.group(1)

    # Create two anomaly-triggered agent rules.
    _create_agent_rule(
        args.base_url,
        args.timeout,
        name=issue_only_rule,
        trigger_ref_id="",
        actions=["github_issue"],
    )
    _create_agent_rule(
        args.base_url,
        args.timeout,
        name=issue_copilot_rule,
        trigger_ref_id="",
        actions=["github_issue", "github_issue_copilot"],
    )

    agents_html_req = urllib.request.Request(
        f"{args.base_url.rstrip('/')}/settings/agents",
        headers={"Accept": "text/html"},
        method="GET",
    )
    with urllib.request.urlopen(agents_html_req, timeout=args.timeout) as resp:  # noqa: S310
        agents_html = resp.read().decode("utf-8", errors="replace")

    issue_only_id_match = re.search(
        rf'data-rule-id="([^"]+)"[^>]*data-rule-name="{re.escape(issue_only_rule)}"',
        agents_html,
    )
    issue_followup_id_match = re.search(
        rf'data-rule-id="([^"]+)"[^>]*data-rule-name="{re.escape(issue_copilot_rule)}"',
        agents_html,
    )
    if not issue_only_id_match or not issue_followup_id_match:
        raise RuntimeError("failed to resolve created agent rule IDs from settings page")
    issue_only_rule_id = issue_only_id_match.group(1)
    issue_copilot_rule_id = issue_followup_id_match.group(1)

    # Trigger automatic rule evaluation from anomaly/tag events.
    matched: dict[str, dict] = {}
    check_payload: dict = {}
    for _ in range(8):
        check_payload = _post_json(args.base_url, "/api/notifications/check", {}, args.api_key, args.timeout)
        if not bool(check_payload.get("ok")):
            raise RuntimeError(f"notifications/check failed: {check_payload}")
        agent_runs = check_payload.get("agent_runs") or []
        for item in agent_runs:
            rid = str(item.get("rule_id", ""))
            if rid in {issue_only_rule_id, issue_copilot_rule_id}:
                matched[rid] = dict(item)
        if issue_only_rule_id in matched and issue_copilot_rule_id in matched:
            break
        time.sleep(0.8)

    if issue_only_rule_id not in matched or issue_copilot_rule_id not in matched:
        raise RuntimeError("new agent rules did not fire from notifications/check")

    only_result = dict(matched[issue_only_rule_id].get("result") or {})
    follow_result = dict(matched[issue_copilot_rule_id].get("result") or {})
    if str(only_result.get("status", "")) != "completed":
        raise RuntimeError(f"issue-only rule did not complete: {matched[issue_only_rule_id]}")
    if str(follow_result.get("status", "")) != "completed":
        raise RuntimeError(f"issue+copilot rule did not complete: {matched[issue_copilot_rule_id]}")

    issue_only_url = str(only_result.get("github_issue_url", "")).strip()
    issue_copilot_url = str(follow_result.get("github_issue_url", "")).strip()
    if not issue_only_url:
        raise RuntimeError("issue-only rule did not create a GitHub issue")
    if not issue_copilot_url:
        raise RuntimeError("issue+copilot rule did not create a GitHub issue")

    io_owner, io_repo, io_num = _parse_issue_url(issue_only_url)
    ic_owner, ic_repo, ic_num = _parse_issue_url(issue_copilot_url)

    io_has_copilot = _gh_issue_has_copilot_assignee(io_owner, io_repo, io_num)
    ic_has_copilot = _gh_issue_has_copilot_assignee(ic_owner, ic_repo, ic_num)

    if io_has_copilot:
        raise RuntimeError("issue-only flow unexpectedly assigned the issue to Copilot")
    if not ic_has_copilot:
        raise RuntimeError("issue+copilot flow did not assign the issue to Copilot")

    work_items_visibility = _verify_work_items_visibility(
        args.base_url,
        args.api_key,
        args.timeout,
        issue_only_url,
        issue_copilot_url,
    )
    if work_items_visibility["issue_copilot_assignment_status"] not in {"requested", "active"}:
        raise RuntimeError("issue+copilot flow did not persist a Copilot assignment status in SOBS work items")

    return {
        "ok": True,
        "preflight": preflight,
        "metric_rule_name": metric_rule_name,
        "metric_rule_id": metric_rule_id,
        "issue_only_rule": issue_only_rule,
        "issue_copilot_rule": issue_copilot_rule,
        "issue_only_rule_id": issue_only_rule_id,
        "issue_copilot_rule_id": issue_copilot_rule_id,
        "issue_only_url": issue_only_url,
        "issue_copilot_url": issue_copilot_url,
        "issue_only_has_copilot_assignee": io_has_copilot,
        "issue_copilot_has_copilot_assignee": ic_has_copilot,
        "work_items_visibility": work_items_visibility,
        "notifications_check": {
            "evaluated": check_payload.get("evaluated", 0),
            "fired": check_payload.get("fired", 0),
            "agent_runs": len(check_payload.get("agent_runs") or []),
        },
    }


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Validate anomaly-triggered issue-only and issue+copilot flows")
    p.add_argument("--base-url", default="http://127.0.0.1:44317")
    p.add_argument("--api-key", default="")
    p.add_argument("--org", required=True)
    p.add_argument("--prefix", default="sobs-validation")
    p.add_argument("--timeout", type=int, default=20)
    p.add_argument("--dry-run", action="store_true")
    return p.parse_args()


def main() -> int:
    args = parse_args()
    if args.dry_run:
        print(
            json.dumps(
                {
                    "ok": True,
                    "dry_run": True,
                    "base_url": args.base_url,
                    "org": args.org,
                    "prefix": args.prefix,
                },
                ensure_ascii=False,
            )
        )
        return 0

    out = run(args)
    print(json.dumps(out, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
        raise
