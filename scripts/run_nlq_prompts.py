#!/usr/bin/env python3
"""Run repeatable NLQ (Query page) prompts against /api/query/ask.

Usage:
  python scripts/run_nlq_prompts.py \
      --endpoint http://127.0.0.1:44317 \
      --thinking-level off

Optional:
  --output /tmp/nlq_prompt_report.json
  --timeout 180
"""

from __future__ import annotations

import argparse
import json
from dataclasses import dataclass

from ai_prompt_runner_common import post_json, summarize_chart_option


@dataclass
class NlqCase:
    label: str
    question: str
    chart: bool
    preferred_chart_type: str
    chart_instruction: str


DEFAULT_CASES: list[NlqCase] = [
    NlqCase(
        label="nlq_error_trend_overlay",
        question="Error trends over the last 24 hours with signal periods overlaid",
        chart=True,
        preferred_chart_type="line",
        chart_instruction="Overlay anomaly/signal periods with clear legend and readable time axis.",
    ),
    NlqCase(
        label="nlq_error_rate_p95",
        question="Hourly error rate by service for last 24h with p50 and p95 bands",
        chart=True,
        preferred_chart_type="line",
        chart_instruction="Show per-service lines and percentile bands in a readable chart.",
    ),
    NlqCase(
        label="nlq_burn_proxy",
        question="Error budget burn proxy over last 24 hours using failing traces over total traces per service",
        chart=True,
        preferred_chart_type="line",
        chart_instruction="Percent axis, highlight sustained burn periods.",
    ),
    NlqCase(
        label="nlq_noisy_services_burst",
        question="Show top services by count of error burst windows in the past 24 hours",
        chart=True,
        preferred_chart_type="bar",
        chart_instruction="Simple operator-ready bar chart, top 10 services.",
    ),
    NlqCase(
        label="nlq_deploy_corr",
        question="Correlate deployment windows with spikes in errors and latency for the last 24h",
        chart=True,
        preferred_chart_type="line",
        chart_instruction="Overlay deployment windows and show both error and latency trends.",
    ),
]


def _call_case(endpoint: str, timeout: int, thinking_level: str, case: NlqCase) -> tuple[int, dict[str, object]]:
    payload = {
        "question": case.question,
        "execute": True,
        "chart": case.chart,
        "preferred_chart_type": case.preferred_chart_type,
        "chart_instruction": case.chart_instruction,
        "thinking_level": thinking_level,
    }
    api_result = post_json(endpoint, "/api/query/ask", payload, timeout)
    status = int(api_result.status)
    body = api_result.body

    chart_spec_text = str(body.get("chart_spec") or "")
    chart_summary = summarize_chart_option(chart_spec_text)

    rows_count = 0
    rows_val = body.get("rows")
    if isinstance(rows_val, list):
        rows_count = len(rows_val)

    columns_count = 0
    cols_val = body.get("columns")
    if isinstance(cols_val, list):
        columns_count = len(cols_val)

    retry_count_raw = body.get("retry_count", 0)
    retry_count = int(retry_count_raw) if isinstance(retry_count_raw, (int, float, str)) else 0

    result = {
        "status": status,
        "ok": bool(body.get("ok")),
        "error": str(body.get("error") or ""),
        "sql_head": str(body.get("sql") or "")[:260],
        "columns_count": columns_count,
        "rows_count": rows_count,
        "retry_count": retry_count,
        "chart_spec_len": len(chart_spec_text),
        "chart": chart_summary,
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
            "chart": case.chart,
            "preferred_chart_type": case.preferred_chart_type,
            **result,
        }
        report.append(row)

        print(f"\n=== [{idx}] {case.label}: {case.question} ===")
        print(f"status={status} ok={row['ok']} retry_count={row['retry_count']}")
        if row["error"]:
            print(f"error={str(row['error'])[:240]}")
        print(f"sql_head={row['sql_head']}")
        print(f"rows={row['rows_count']} columns={row['columns_count']}")

        chart_raw = row.get("chart") if isinstance(row, dict) else {}
        chart_summary_print: dict[str, object] = chart_raw if isinstance(chart_raw, dict) else {}
        print(
            "chart="
            f"present={chart_summary_print.get('present')} empty_obj={chart_summary_print.get('empty_obj')} "
            f"keys={chart_summary_print.get('keys')} series_types={chart_summary_print.get('series_types')}"
        )

        if (not row["ok"]) or (status >= 400):
            failures += 1

    good_rows: list[dict[str, object]] = []
    for row in report:
        chart_raw = row.get("chart") if isinstance(row, dict) else {}
        chart_summary_eval: dict[str, object] = chart_raw if isinstance(chart_raw, dict) else {}
        status_raw = row.get("status", 0)
        status_value = int(status_raw) if isinstance(status_raw, (int, float, str)) else 0
        chart_non_empty = bool(
            chart_summary_eval.get("present") is True and chart_summary_eval.get("empty_obj") is False
        )
        if bool(row.get("ok")) and status_value < 400 and chart_non_empty:
            good_rows.append(row)

    print(
        "\nNLQ quality summary: "
        f"{len(good_rows)}/{len(report)} responses had ok=true, "
        "HTTP<400, and non-empty chart_spec"
    )
    if good_rows:
        print("Best-performing NLQ prompts:")
        for row in good_rows:
            chart_raw = row.get("chart") if isinstance(row, dict) else {}
            chart_summary_best: dict[str, object] = chart_raw if isinstance(chart_raw, dict) else {}
            print(
                f"- {row.get('label')}: chart_spec_len={row.get('chart_spec_len')} "
                f"series_types={chart_summary_best.get('series_types')}"
            )

    if args.output:
        with open(args.output, "w", encoding="utf-8") as f:
            json.dump(report, f, ensure_ascii=False, indent=2)
        print(f"\nWrote report to {args.output}")

    print(f"\nSummary: {len(report) - failures}/{len(report)} requests returned ok=true with HTTP < 400")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
