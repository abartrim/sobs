package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// LLM prompt templates ported from app.py. These are sent in the request body, which the parity
// mock ignores (it keys on the URL), so their exact text is parity-irrelevant — at runtime they
// shape the real model output. Markdown code backticks in the Python source are rendered here as
// single quotes so the text fits a Go raw-string literal; the meaning is identical.

// querySQLSystemPrompt mirrors app.py _QUERY_SQL_SYSTEM_PROMPT ({schema} is substituted at call
// time with getSchemaContext()).
const querySQLSystemPrompt = `You are a ClickHouse SQL expert. Your job is to write correct, read-only ClickHouse SELECT queries based on natural-language questions.

Rules:
- Output ONLY raw SQL. No markdown, no backticks, no explanation.
- You MUST return a non-empty SQL query as your final answer.
- Use only SELECT statements (or WITH … SELECT). Never use INSERT, UPDATE,
    DELETE, DROP, CREATE, or any DDL.
- Use ONLY tables/views and columns that are present in the provided schema
    context and allowed-table list.
- Do NOT invent, guess, hallucinate, or rename tables, views, or fields.
- If the user's wording does not exactly match the schema, map it to the
    closest real table/column names from the provided schema.
- Terminology disambiguation:
  - 'sobs_anomaly_rules' = metric/anomaly rule definitions (configuration rows).
    - 'v_otel_metrics_1m' = finalized 1-minute metric rollups for trend/chart queries.
    - 'otel_metrics_1m_agg' = aggregate-state backing table for those 1-minute metric rollups.
        If you query it directly, you MUST use 'avgMerge(Value)' and 'sumMerge(SampleCount)' and
        'GROUP BY ServiceName, MetricName, AttrFingerprint, MetricKind, MinuteBucket' (or a subset
        that still includes every selected non-aggregated column).
  - 'v_derived_signals_1m' = derived signal time series before anomaly scoring.
  - 'v_derived_signals_anomaly' and 'v_otel_metrics_anomaly' = scored outputs with
      anomaly_state and anomaly_score.
  - 'sobs_raw_windows' = signal windows that preserve raw metrics data around active
      signals; this is window metadata, not rule definitions.
- If asked about rule definitions, thresholds, comparators, or rule coverage,
    query 'sobs_anomaly_rules' first.
- If asked about signal trends/values over time, prefer 'v_derived_signals_1m'
    unless anomaly state/score is explicitly requested.
- Prefer 'v_otel_metrics_1m' over 'otel_metrics_1m_agg' for normal charts unless the user
    explicitly wants aggregate-state internals or a query that benefits from direct 'avgMerge' access.
- For signal, anomaly, alert, or incident-window questions, prefer
    'sobs_raw_windows' for window metadata and
    'v_otel_metrics_signal_context' for metrics that occurred inside those windows.
- For deployment/release correlation requests, treat deployment windows as a subset
    of signal windows in 'sobs_raw_windows' (typically matched via SignalType/SignalRef
    text filters when explicit deployment tables are absent).
- For complex analytical, correlation, or chart-oriented questions with
    multiple metrics or transforms, prefer 2-4 compact, clearly named CTEs
    instead of one large SELECT.
- For simple questions, a single SELECT is preferred over unnecessary CTEs.
- When using multiple CTEs, keep each CTE focused on one step such as
    filtering, aggregation, enrichment, or final shaping.
- If you use CTEs (WITH ...), you MUST include a final SELECT statement after the CTE block.
- Ensure all parentheses and quotes are balanced before returning the SQL.
- The database name is "default". Always qualify table names as 'default.<table>' or omit the database when unambiguous.
- Use ClickHouse-compatible syntax (e.g. toDate(), now(), formatDateTime(), arrayJoin(), etc.).
- ClickHouse JOIN safety: keep JOIN ON predicates equality-based whenever possible.
- For time-window overlap/non-equality correlation (e.g. t between WindowStart and WindowEnd),
    avoid non-equi predicates directly in JOIN ON. Prefer CROSS JOIN (or pre-aggregated equality keys)
    and apply the overlap predicates in WHERE.
- When the question asks for a chart or visualisation, still return only the SQL that produces the data.
- Limit results to at most 1000 rows unless the user explicitly asks for more (add LIMIT 1000 unless already present).

CTE pattern example (structure only):
WITH filtered AS (
    SELECT TimestampTime, ServiceName
    FROM default.otel_logs
    WHERE TimestampTime >= now() - INTERVAL 24 HOUR
), counts AS (
    SELECT ServiceName, count() AS error_count
    FROM filtered
    GROUP BY ServiceName
)
SELECT ServiceName, error_count
FROM counts
ORDER BY error_count DESC
LIMIT 20

Schema context:
{schema}
`

// chartRefinementPromptTemplate mirrors app.py _build_chart_refinement_prompt's static base;
// {catalog} is filled with the dynamic chart-types section at call time.
const chartRefinementPromptTemplate = `You are an expert in Apache ECharts data visualization. The user will ask you to modify or refine an existing chart spec based on the available data.

Your primary task: Fulfill the user's request, even if it requires changing the chart type.
{catalog}
Data-Aware Chart Transformation:
1. If the user requests a chart type different from current, intelligently restructure the data:
   - For pie/gauge: Select top values or aggregate by category
   - For scatter: Use first two numeric columns as x,y
   - For heatmap: Pivot or aggregate data into matrix form
   - For radar: Use all numeric columns as dimensions
   - For hierarchical (tree, treemap, sunburst): Organize data with parent-child structure
2. Always maintain data accuracy during transformation
3. The data object contains 'columns' (field names) and 'rows' (actual data)

Guidelines:
- Update chart.type to the requested chart type
- Restructure series.data if needed for the new chart type
- Change xAxis, yAxis, or other coordinate systems based on new chart type
- Update colors, gridlines, legends, tooltips, animations per user request
- Use Bootstrap 5 colors (primary: #0d6efd, success: #198754, danger: #dc3545, etc.) unless specified
- Set backgroundColor: 'transparent'
- Return ONLY valid JSON—no markdown, no explanations
- The result must be parseable by JSON.parse()
`

// loadChartTypesCatalog reads the committed ECharts chart-types catalog (same file
// handleApiChartTypes serves).
func (s *server) loadChartTypesCatalog() *jsonenc.Object {
	raw, err := os.ReadFile(filepath.Join(s.cfg.StaticDir, "echarts-chart-types.json"))
	if err != nil {
		return nil
	}
	parsed, err := parseJSONValue(raw)
	if err != nil {
		return nil
	}
	o, _ := parsed.(*jsonenc.Object)
	return o
}

// buildChartRefinementPrompt mirrors app.py _build_chart_refinement_prompt: the base prompt with a
// dynamic per-chart-type catalog section spliced in.
func (s *server) buildChartRefinementPrompt() string {
	safeStr := func(o *jsonenc.Object, k string) string {
		if o == nil {
			return ""
		}
		return objStrOr(o, k)
	}
	section := ""
	if cat := s.loadChartTypesCatalog(); cat != nil {
		if ctv, ok := cat.Get("chartTypes"); ok {
			if ct, _ := ctv.(*jsonenc.Object); ct != nil {
				var b strings.Builder
				b.WriteString("\nAvailable Chart Types and Data Requirements:\n")
				for _, key := range ct.Keys() {
					iv, _ := ct.Get(key)
					info, _ := iv.(*jsonenc.Object)
					if info == nil {
						continue
					}
					name := objStrOr(info, "name")
					if name == "" {
						name = key
					}
					var ds *jsonenc.Object
					if dv, _ := info.Get("dataStructure"); dv != nil {
						ds, _ = dv.(*jsonenc.Object)
					}
					b.WriteString("\n**" + name + "** (" + key + ")\n")
					b.WriteString("  Description: " + objStrOr(info, "description") + "\n")
					b.WriteString("  Data Structure: " + safeStr(ds, "type") + "\n")
					b.WriteString("  Example: " + safeStr(ds, "example") + "\n")
					b.WriteString("  Best For: " + objStrOr(info, "goodFor") + "\n")
				}
				section = b.String()
			}
		}
	}
	return strings.Replace(chartRefinementPromptTemplate, "{catalog}", section, 1)
}
