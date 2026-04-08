#!/usr/bin/env python3
"""Run repeatable complex SRE chart-builder prompts against /api/dashboards/spec/ai-build.

Usage:
  python scripts/run_complex_chart_prompts.py \
      --endpoint http://127.0.0.1:44317 \
      --thinking-level off

Optional:
  --output /tmp/complex_chart_report.json
  --timeout 180
"""

from __future__ import annotations

import argparse
import json
from dataclasses import dataclass

from ai_prompt_runner_common import post_json, summarize_chart_option


@dataclass
class PromptCase:
    label: str
    question: str
    preferred_chart_type: str
    chart_instruction: str


DEFAULT_CASES: list[PromptCase] = [
    PromptCase(
        label="error_trend_overlay",
        question="Error trends over the last 24 hours with signal periods overlaid",
        preferred_chart_type="line",
        chart_instruction="Overlay anomaly/signal periods clearly with readable legend and time axis.",
    ),
    PromptCase(
        label="error_rate_p50_p95",
        question="Hourly error rate by service for last 24h with p50 and p95 bands",
        preferred_chart_type="line",
        chart_instruction="Use multi-series with confidence bands where possible.",
    ),
    PromptCase(
        label="error_budget_burn_proxy",
        question="Error budget burn proxy over last 24 hours using failing traces / total traces per service",
        preferred_chart_type="line",
        chart_instruction="Percent axis, highlight sustained burn periods over threshold.",
    ),
    PromptCase(
        label="noisy_services_burst_v1",
        question="Top noisy services and their error burst windows in the last 24h",
        preferred_chart_type="bar",
        chart_instruction="Prioritize on-call readability and burst period annotations.",
    ),
    PromptCase(
        label="noisy_services_burst_v2",
        question="Show top services by count of error burst windows in the past 24 hours",
        preferred_chart_type="bar",
        chart_instruction=(
            "Return service categories on x-axis and burst-window counts on y-axis; " "no empty chart specs."
        ),
    ),
    PromptCase(
        label="noisy_services_burst_v3",
        question=("For the last 24h, rank services by number of windows where SignalType indicates error bursts"),
        preferred_chart_type="bar",
        chart_instruction=("Produce a simple operator-ready bar chart with top 10 services and burst window counts."),
    ),
    PromptCase(
        label="noisy_services_burst_v4",
        question=("Which services had the most sustained error windows in the last day? Show counts by service."),
        preferred_chart_type="bar",
        chart_instruction=("Use a horizontal bar chart if labels are long; include clear title and tooltip."),
    ),
    PromptCase(
        label="deploy_corr_errors_latency",
        question="Correlate deployment windows with spikes in errors and latency for the last 24h",
        preferred_chart_type="line",
        chart_instruction="Overlay deployment periods and show dual-axis if needed.",
    ),
]


def _call_case(endpoint: str, timeout: int, thinking_level: str, case: PromptCase) -> tuple[int, dict[str, object]]:
    payload = {
        "question": case.question,
        "preferred_chart_type": case.preferred_chart_type,
        "chart_instruction": case.chart_instruction,
        "thinking_level": thinking_level,
    }
    api_result = post_json(endpoint, "/api/dashboards/spec/ai-build", payload, timeout)
    status = int(api_result.status)
    body = api_result.body

    spec_raw = body.get("spec")
    spec_dict: dict[str, object] = spec_raw if isinstance(spec_raw, dict) else {}
    visual_raw = spec_dict.get("visual")
    visual: dict[str, object] = visual_raw if isinstance(visual_raw, dict) else {}
    option_text = str(visual.get("custom_option_json") or "") if isinstance(visual, dict) else ""
    mapping_text = str(visual.get("custom_mapping_json") or "") if isinstance(visual, dict) else ""

    columns_raw = body.get("columns")
    columns_count = len(columns_raw) if isinstance(columns_raw, list) else 0
    named_queries_raw = body.get("named_queries")
    named_queries_count = len(named_queries_raw) if isinstance(named_queries_raw, list) else 0

    result = {
        "status": status,
        "ok": bool(body.get("ok")),
        "error": str(body.get("error") or ""),
        "chart_error": str(body.get("chart_error") or ""),
        "sql_head": str(body.get("sql") or "")[:260],
        "columns_count": columns_count,
        "named_queries_count": named_queries_count,
        "mapping_len": len(mapping_text),
        "option_len": len(option_text),
        "option": summarize_chart_option(option_text),
    }
    return status, result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--endpoint", default="http://127.0.0.1:44317")
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--thinking-level", default="off")
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    report: list[dict[str, object]] = []
    failures = 0

    for idx, case in enumerate(DEFAULT_CASES, start=1):
        status, result = _call_case(args.endpoint, args.timeout, args.thinking_level, case)
        row = {
            "index": idx,
            "label": case.label,
            "question": case.question,
            "preferred_chart_type": case.preferred_chart_type,
            **result,
        }
        report.append(row)

        print(f"\n=== [{idx}] {case.label}: {case.question} ===")
        print(f"status={status} ok={row['ok']}")
        if row["error"]:
            print(f"error={str(row['error'])[:240]}")
        if row["chart_error"]:
            print(f"chart_error={str(row['chart_error'])[:240]}")
        print(f"sql_head={row['sql_head']}")
        option = row["option"] if isinstance(row["option"], dict) else {}
        print(
            "option="
            f"present={option.get('present')} empty_obj={option.get('empty_obj')} "
            f"keys={option.get('keys')} series_types={option.get('series_types')}"
        )

        if (not row["ok"]) or (status >= 400):
            failures += 1

    # Quality summary: successful HTTP/ok responses with non-empty option object.
    good_rows: list[dict[str, object]] = []
    for row in report:
        option_raw = row.get("option") if isinstance(row, dict) else {}
        option_summary: dict[str, object] = option_raw if isinstance(option_raw, dict) else {}
        status_raw = row.get("status", 0)
        status_value = int(status_raw) if isinstance(status_raw, (int, float, str)) else 0
        option_is_non_empty = bool(
            isinstance(option_summary, dict)
            and option_summary.get("present") is True
            and option_summary.get("empty_obj") is False
        )
        if bool(row.get("ok")) and status_value < 400 and option_is_non_empty:
            good_rows.append(row)

    print(
        f"\nChart quality summary: {len(good_rows)}/{len(report)} responses had ok=true, HTTP<400, and non-empty option"
    )
    if good_rows:
        print("Best-performing prompts:")
        for row in good_rows:
            option_raw = row.get("option") if isinstance(row, dict) else {}
            option_summary_row: dict[str, object] = option_raw if isinstance(option_raw, dict) else {}
            series_types = option_summary_row.get("series_types") if isinstance(option_summary_row, dict) else []
            print(f"- {row.get('label')}: option_len={row.get('option_len')} " f"series_types={series_types}")

    if args.output:
        with open(args.output, "w", encoding="utf-8") as f:
            json.dump(report, f, ensure_ascii=False, indent=2)
        print(f"\nWrote report to {args.output}")

    print(f"\nSummary: {len(report) - failures}/{len(report)} requests returned ok=true with HTTP < 400")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
