package main

// coverage_pure_e_test.go — oracle-anchored unit tests for SLICE E pure helpers
// (AI / chart-JSON / LLM-stats helpers). Assertions are anchored to the FROZEN
// ORACLE (app.py), not to "whatever Go currently returns".
//
// Target functions and their disposition:
//   TESTED:
//     queryStageStats   (ai_llm.go:291)   app.py:3481-3488 (_query_llm_stage_stats)
//     queryRunLLMStats  (fix_query.go:314) app.py:3491-3504 (_summarize_query_llm_stats)
//
//   SKIPPED (already covered by prior slices — verified executed, not just named):
//     buildFallbackCustomOptionJSON  — TestBuildFallbackCustomOptionJSON  (ai_build_chat_helpers_test.go)
//     objGetOr                       — TestObjGetOr                       (ai_build_chat_helpers_test.go)
//     pyExpectingValueError          — TestPyExpectingValueError          (ai_build_chat_helpers_test.go)
//     extractChartOptionPlaceholders — TestExtractChartOptionPlaceholders (notif_chart_mcp_helpers_test.go)
//     inferCustomMappingFromOption   — TestInferCustomMappingFromOption   (remaining_pure_helpers_test.go)
//     mcpNormalizeMap                — TestMcpNormalizeMap                (notif_chart_mcp_helpers_test.go)
//   The original 0%-coverage target list was stale; these six are already tested
//   in current go-main, so retesting them adds ~0 coverage. Only the two LLM-stage
//   stats helpers remained genuinely untested.
//
// NOTE on the mcpNormalizeMap oracle (for the record, since it was on the original
// list): mcp.py:518-523 _normalize_map_value has an ast.literal_eval fallback that
// the Go port (mcp_tools.go:374, JSON-only) lacks — so a Python-repr string like
// "{'a': 1}" normalizes to {"a": 1} in Python but {} in Go. That divergence is
// pre-existing and out of scope here (the func is already tested elsewhere); noting
// it so it is not lost.

import (
	"reflect"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// objToMap flattens a *jsonenc.Object one level deep into a comparable map.
// Nested *jsonenc.Object values are flattened recursively.
func objEToMap(o *jsonenc.Object) map[string]any {
	if o == nil {
		return nil
	}
	m := map[string]any{}
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		if sub, ok := v.(*jsonenc.Object); ok {
			m[k] = objEToMap(sub)
		} else {
			m[k] = v
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// queryStageStats — ai_llm.go:291
// Oracle: app.py:3481-3488 _query_llm_stage_stats(stats) ->
//   {"prompt_tokens": int(payload.get("prompt_tokens") or 0),
//    "completion_tokens": int(payload.get("completion_tokens") or 0),
//    "thinking_tokens": int(payload.get("thinking_tokens") or 0),
//    "elapsed_ms": int(payload.get("elapsed_ms") or 0)}
// The Go llmStats type carries prompt/completion/thinking ints and no elapsed
// field, so elapsed_ms is always 0 — matching the oracle for the payloads Go's
// callers feed (where elapsed is absent/0). Key order must equal the oracle dict
// literal order.
// ---------------------------------------------------------------------------

func TestSliceE_queryStageStats(t *testing.T) {
	cases := []struct {
		desc string
		in   llmStats
		want map[string]any
	}{
		{
			"zero stats",
			llmStats{},
			map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "thinking_tokens": 0, "elapsed_ms": 0},
		},
		{
			"populated stats",
			llmStats{prompt: 12, completion: 34, thinking: 5},
			map[string]any{"prompt_tokens": 12, "completion_tokens": 34, "thinking_tokens": 5, "elapsed_ms": 0},
		},
		{
			"large values preserved",
			llmStats{prompt: 100000, completion: 250000, thinking: 9999},
			map[string]any{"prompt_tokens": 100000, "completion_tokens": 250000, "thinking_tokens": 9999, "elapsed_ms": 0},
		},
	}
	wantOrder := []string{"prompt_tokens", "completion_tokens", "thinking_tokens", "elapsed_ms"}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := queryStageStats(c.in)
			if gotMap := objEToMap(got); !reflect.DeepEqual(gotMap, c.want) {
				t.Errorf("queryStageStats(%+v) = %v, want %v", c.in, gotMap, c.want)
			}
			if gotKeys := got.Keys(); !reflect.DeepEqual(gotKeys, wantOrder) {
				t.Errorf("queryStageStats key order = %v, want %v", gotKeys, wantOrder)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// queryRunLLMStats — fix_query.go:314
// Oracle: app.py:3491-3504 _summarize_query_llm_stats(named_query_generation=…,
// chart_generation=…) -> {"totals": <sum of stages>, "named_query_generation":
// <stage>, "chart_generation": <stage>}. totals is the element-wise sum of the
// two stages; each stage is a _query_llm_stage_stats dict. Insertion order:
// totals first (seeded by the oracle), then each stage in kwarg order.
// ---------------------------------------------------------------------------

func TestSliceE_queryRunLLMStats(t *testing.T) {
	stage := func(p, c, th int) map[string]any {
		return map[string]any{"prompt_tokens": p, "completion_tokens": c, "thinking_tokens": th, "elapsed_ms": 0}
	}
	cases := []struct {
		desc        string
		named, char llmStats
		want        map[string]any
	}{
		{
			"both zero",
			llmStats{}, llmStats{},
			map[string]any{
				"totals":                 stage(0, 0, 0),
				"named_query_generation": stage(0, 0, 0),
				"chart_generation":       stage(0, 0, 0),
			},
		},
		{
			"populated stages summed into totals",
			llmStats{prompt: 10, completion: 20, thinking: 3},
			llmStats{prompt: 1, completion: 2, thinking: 4},
			map[string]any{
				"totals":                 stage(11, 22, 7),
				"named_query_generation": stage(10, 20, 3),
				"chart_generation":       stage(1, 2, 4),
			},
		},
		{
			"only named stage populated",
			llmStats{prompt: 7, completion: 8, thinking: 9},
			llmStats{},
			map[string]any{
				"totals":                 stage(7, 8, 9),
				"named_query_generation": stage(7, 8, 9),
				"chart_generation":       stage(0, 0, 0),
			},
		},
	}
	wantOrder := []string{"totals", "named_query_generation", "chart_generation"}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := queryRunLLMStats(c.named, c.char)
			if gotMap := objEToMap(got); !reflect.DeepEqual(gotMap, c.want) {
				t.Errorf("queryRunLLMStats(%+v, %+v) = %v, want %v", c.named, c.char, gotMap, c.want)
			}
			if gotKeys := got.Keys(); !reflect.DeepEqual(gotKeys, wantOrder) {
				t.Errorf("queryRunLLMStats top-level key order = %v, want %v", gotKeys, wantOrder)
			}
		})
	}
}
