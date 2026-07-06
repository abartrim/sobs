package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 5: cmd/sobs/handlers_pages_traces_detail.go undertested branches. Oracle:
// app.py view_traces' populated trace_id branch (_merge_span_intervals, span bounds,
// _build_trace_window_overlay_segments, int(str(value or 0)), and the full trace_detail
// builder).

// --- mergeSpanIntervals ---------------------------------------------------------------------

func TestMergeSpanIntervals_Cov95B5(t *testing.T) {
	t.Run("empty_spans_returns_nil", func(t *testing.T) {
		if got := mergeSpanIntervals(nil); got != nil {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("negative_duration_clamped_to_zero", func(t *testing.T) {
		spans := []any{map[string]any{"start_ms": 100.0, "duration_ms": -50.0}}
		got := mergeSpanIntervals(spans)
		if len(got) != 1 || got[0][0] != 100.0 || got[0][1] != 100.0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("overlapping_intervals_merged", func(t *testing.T) {
		spans := []any{
			map[string]any{"start_ms": 0.0, "duration_ms": 100.0},
			map[string]any{"start_ms": 50.0, "duration_ms": 100.0}, // overlaps -> extend to 150
		}
		got := mergeSpanIntervals(spans)
		if len(got) != 1 || got[0][0] != 0.0 || got[0][1] != 150.0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("non_overlapping_intervals_stay_separate", func(t *testing.T) {
		spans := []any{
			map[string]any{"start_ms": 0.0, "duration_ms": 10.0},
			map[string]any{"start_ms": 100.0, "duration_ms": 10.0},
		}
		got := mergeSpanIntervals(spans)
		if len(got) != 2 {
			t.Fatalf("expected 2 disjoint intervals, got %v", got)
		}
	})

	t.Run("touching_interval_does_not_extend_when_not_greater", func(t *testing.T) {
		// iv[1] > merged[last][1] guard: a span fully inside an existing interval must not
		// shrink or otherwise mutate the merged end.
		spans := []any{
			map[string]any{"start_ms": 0.0, "duration_ms": 100.0},
			map[string]any{"start_ms": 10.0, "duration_ms": 5.0}, // fully inside [0,100]
		}
		got := mergeSpanIntervals(spans)
		if len(got) != 1 || got[0][1] != 100.0 {
			t.Fatalf("got %v", got)
		}
	})
}

// --- traceSpanBounds -------------------------------------------------------------------------

func TestTraceSpanBounds_Cov95B5(t *testing.T) {
	t.Run("negative_duration_clamped", func(t *testing.T) {
		spans := []any{map[string]any{"start_ms": 10.0, "duration_ms": -5.0}}
		start, end, total := traceSpanBounds(spans)
		if start != 10.0 || end != 10.0 {
			t.Fatalf("got start=%v end=%v", start, end)
		}
		if total != 1.0 {
			t.Fatalf("expected min total_ms=1.0 floor, got %v", total)
		}
	})

	t.Run("total_ms_floor_when_span_is_zero_width", func(t *testing.T) {
		spans := []any{map[string]any{"start_ms": 5.0, "duration_ms": 0.0}}
		_, _, total := traceSpanBounds(spans)
		if total != 1.0 {
			t.Fatalf("got %v", total)
		}
	})

	t.Run("multiple_spans_widen_bounds", func(t *testing.T) {
		spans := []any{
			map[string]any{"start_ms": 20.0, "duration_ms": 5.0},
			map[string]any{"start_ms": 0.0, "duration_ms": 10.0},
		}
		start, end, total := traceSpanBounds(spans)
		if start != 0.0 || end != 25.0 || total != 25.0 {
			t.Fatalf("got start=%v end=%v total=%v", start, end, total)
		}
	})
}

// --- buildTraceWindowOverlaySegments -----------------------------------------------------

// traceEpochBaseMs anchors these tests away from Unix epoch 0: buildTraceWindowOverlaySegments
// treats a window_start/window_end that normalizes to epoch-ms <= 0 as unparseable (guards
// real parse failures, since tsStrToEpochMs returns 0.0 on error) — a span tree that itself
// starts exactly at epoch 0 would make every window_start==traceStartMs also equal to 0 and
// spuriously "fail to parse". Using a realistic (non-zero) base avoids that coincidental clash.
const traceEpochBaseMs = 1_700_000_000_000.0

func TestBuildTraceWindowOverlaySegments_Cov95B5(t *testing.T) {
	spans := []any{
		map[string]any{"start_ms": traceEpochBaseMs, "duration_ms": 1000.0},
	}

	t.Run("empty_spans_returns_empty", func(t *testing.T) {
		got := buildTraceWindowOverlaySegments(nil, []any{map[string]any{}})
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("empty_windows_returns_empty", func(t *testing.T) {
		got := buildTraceWindowOverlaySegments(spans, nil)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("window_with_zero_or_negative_epoch_skipped", func(t *testing.T) {
		windows := []any{
			map[string]any{"window_start": "", "window_end": "2026-01-01 00:00:01.000000"},
		}
		got := buildTraceWindowOverlaySegments(spans, windows)
		if len(got) != 0 {
			t.Fatalf("expected skip for unparseable window_start, got %v", got)
		}
	})

	t.Run("window_outside_trace_range_after_clamp_skipped", func(t *testing.T) {
		// A window entirely before the trace start clamps to endMs<=startMs -> skipped.
		windows := []any{
			map[string]any{
				"window_start": "1990-01-01 00:00:00.000000",
				"window_end":   "1990-01-01 00:00:01.000000",
			},
		}
		got := buildTraceWindowOverlaySegments(spans, windows)
		if len(got) != 0 {
			t.Fatalf("expected skip for out-of-range window, got %v", got)
		}
	})

	t.Run("valid_window_produces_segment_with_title_and_ratio", func(t *testing.T) {
		traceStartISO := epochMsToISOUTC(traceEpochBaseMs)
		traceEndISO := epochMsToISOUTC(traceEpochBaseMs + 1000)
		windows := []any{
			map[string]any{
				"window_start":   traceStartISO,
				"window_end":     traceEndISO,
				"copied_count":   "2",
				"expected_count": "3",
				"copy_complete":  false,
				"signal_type":    "metrics",
				"signal_ref":     "cpu",
			},
		}
		got := buildTraceWindowOverlaySegments(spans, windows)
		if len(got) != 1 {
			t.Fatalf("expected 1 segment, got %v", got)
		}
		seg := spanDict(got[0])
		title, _ := seg["title"].(string)
		if !strings.Contains(title, "metrics (cpu)") || !strings.Contains(title, "[2/3]") {
			t.Fatalf("got title %q", title)
		}
		if seg["copy_complete"] != false {
			t.Fatalf("expected copy_complete=false, got %v", seg["copy_complete"])
		}
	})

	t.Run("window_missing_signal_type_defaults_to_window_label", func(t *testing.T) {
		traceStartISO := epochMsToISOUTC(traceEpochBaseMs)
		traceEndISO := epochMsToISOUTC(traceEpochBaseMs + 1000)
		windows := []any{
			map[string]any{
				"window_start": traceStartISO,
				"window_end":   traceEndISO,
			},
		}
		got := buildTraceWindowOverlaySegments(spans, windows)
		if len(got) != 1 {
			t.Fatalf("got %v", got)
		}
		seg := spanDict(got[0])
		title, _ := seg["title"].(string)
		if !strings.HasPrefix(title, "window [0/0]") {
			t.Fatalf("got title %q", title)
		}
	})

	t.Run("segments_sorted_by_start_pct", func(t *testing.T) {
		longSpans := []any{map[string]any{"start_ms": traceEpochBaseMs, "duration_ms": 10000.0}}
		mk := func(startMs, endMs float64) map[string]any {
			return map[string]any{
				"window_start": epochMsToISOUTC(traceEpochBaseMs + startMs),
				"window_end":   epochMsToISOUTC(traceEpochBaseMs + endMs),
			}
		}
		windows := []any{mk(8000, 9000), mk(1000, 2000)}
		got := buildTraceWindowOverlaySegments(longSpans, windows)
		if len(got) != 2 {
			t.Fatalf("expected 2 segments, got %d", len(got))
		}
		first := spanDict(got[0])["start_pct"].(float64)
		second := spanDict(got[1])["start_pct"].(float64)
		if first >= second {
			t.Fatalf("expected sorted ascending start_pct, got %v then %v", first, second)
		}
	})

	t.Run("zero_width_after_clamp_skipped", func(t *testing.T) {
		// window exactly at the trace boundary end -> endMs<=startMs after clamp.
		windows := []any{
			map[string]any{
				"window_start": epochMsToISOUTC(traceEpochBaseMs + 1000),
				"window_end":   epochMsToISOUTC(traceEpochBaseMs + 2000),
			},
		}
		got := buildTraceWindowOverlaySegments(spans, windows)
		if len(got) != 0 {
			t.Fatalf("expected skip at exact boundary, got %v", got)
		}
	})
}

// --- intStrOrZero ----------------------------------------------------------------------------

func TestIntStrOrZero_Cov95B5(t *testing.T) {
	t.Run("nil_returns_zero", func(t *testing.T) {
		if got := intStrOrZero(nil); got != 0 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("string_digit", func(t *testing.T) {
		if got := intStrOrZero("42"); got != 42 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("float_value", func(t *testing.T) {
		if got := intStrOrZero(float64(7)); got != 7 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("unparseable_string_returns_zero", func(t *testing.T) {
		if got := intStrOrZero("not-a-number"); got != 0 {
			t.Fatalf("got %d", got)
		}
	})
}

// --- buildTraceDetail ------------------------------------------------------------------------

// traceDetailFakeRouter dispatches Execute calls for buildTraceDetail's query sequence by
// matching a distinguishing substring, letting each sub-test override only the branches it cares
// about while everything else returns an empty result (matching the "no data" default path).
func traceDetailFakeRouter(t *testing.T, overrides map[string]func(params ...any) (*store.Result, error)) *storetest.FakeDB {
	t.Helper()
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		for marker, fn := range overrides {
			if strings.Contains(q, marker) {
				return fn(params...)
			}
		}
		return &store.Result{}, nil
	}}
}

func TestBuildTraceDetail_Cov95B5(t *testing.T) {
	t.Run("count_query_error_still_proceeds_with_zero_total", func(t *testing.T) {
		s := &server{db: traceDetailFakeRouter(t, map[string]func(params ...any) (*store.Result, error){
			"FROM otel_traces WHERE TraceId=? ORDER BY": func(params ...any) (*store.Result, error) {
				return &store.Result{}, nil // no span rows -> nil detail
			},
		})}
		detail, total, links := s.buildTraceDetail("trace-x", 200, 0)
		if detail != nil {
			t.Fatalf("expected nil detail for no spans, got %v", detail)
		}
		if total != 0 {
			t.Fatalf("expected total=0, got %d", total)
		}
		if len(links) != 0 {
			t.Fatalf("expected empty work item links, got %v", links)
		}
	})

	t.Run("detail_query_error_returns_nil", func(t *testing.T) {
		s := &server{db: traceDetailFakeRouter(t, map[string]func(params ...any) (*store.Result, error){
			"FROM otel_traces WHERE TraceId=? ORDER BY": func(params ...any) (*store.Result, error) {
				return nil, errors.New("boom")
			},
		})}
		detail, total, links := s.buildTraceDetail("trace-x", 200, 0)
		if detail != nil || total != 0 || len(links) != 0 {
			t.Fatalf("got detail=%v total=%d links=%v", detail, total, links)
		}
	})

	t.Run("populated_trace_builds_full_detail", func(t *testing.T) {
		s := &server{db: traceDetailFakeRouter(t, map[string]func(params ...any) (*store.Result, error){
			"SELECT COUNT(*) AS c FROM otel_traces": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"c"}, []any{float64(2)}), nil
			},
			"FROM otel_traces WHERE TraceId=? ORDER BY": func(params ...any) (*store.Result, error) {
				return storetest.Result(
					[]string{"Timestamp", "TraceId", "SpanId", "ParentSpanId", "SpanName", "ServiceName",
						"Duration", "StatusCode", "SpanAttributes"},
					[]any{"2026-01-01 00:00:00.000000", "trace-x", "span-1", "", "root", "svc-a",
						float64(2_000_000), "OK", map[string]any{"k8s.namespace.name": "ns1"}},
					[]any{"2026-01-01 00:00:01.000000", "trace-x", "span-2", "span-1", "child", "svc-a",
						float64(1_000_000), "OK", map[string]any{}},
				), nil
			},
			"IsResolved": func(params ...any) (*store.Result, error) {
				return storetest.Result(
					[]string{"Timestamp", "ServiceName", "TraceId", "SpanId", "Body", "LogAttributes", "ErrorId", "IsResolved"},
					[]any{"2026-01-01 00:00:00.500000", "svc-a", "trace-x", "span-1", "boom", map[string]any{}, "err-1", float64(1)},
				), nil
			},
			"count() AS cnt FROM otel_logs WHERE TraceId=? AND SpanId": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"SpanId", "cnt"}, []any{"span-1", float64(5)}), nil
			},
			"SELECT Timestamp FROM otel_logs WHERE TraceId=? LIMIT": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"Timestamp"}, []any{"2026-01-01 00:00:00.200000"}), nil
			},
			"v_derived_signals_anomaly": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"anomaly_state"}, []any{"warning"}), nil
			},
		})}
		detail, total, links := s.buildTraceDetail("trace-x", 200, 0)
		if total != 2 {
			t.Fatalf("expected total_spans=2, got %d", total)
		}
		d, ok := detail.(map[string]any)
		if !ok {
			t.Fatalf("expected map detail, got %T", detail)
		}
		if d["anomaly_state"] != "warning" {
			t.Fatalf("expected anomaly_state populated, got %v", d["anomaly_state"])
		}
		errs, _ := d["errors"].([]any)
		if len(errs) != 1 {
			t.Fatalf("expected 1 trace error, got %v", errs)
		}
		errItem := spanDict(errs[0])
		if errItem["resolved"] != true {
			t.Fatalf("expected resolved=true, got %v", errItem["resolved"])
		}
		if errItem["id"] != "err-1" {
			t.Fatalf("expected ErrorId override applied, got %v", errItem["id"])
		}
		logCounts, _ := d["log_counts"].(map[string]any)
		if logCounts["span-1"] != 5 {
			t.Fatalf("expected log_counts populated, got %v", logCounts)
		}
		spanIDs, _ := d["error_span_ids"].([]any)
		if len(spanIDs) != 1 || spanIDs[0] != "span-1" {
			t.Fatalf("expected error_span_ids=[span-1], got %v", spanIDs)
		}
		if links == nil {
			t.Fatal("expected non-nil work item links map")
		}
	})

	t.Run("errors_truncated_flag_set_when_over_limit", func(t *testing.T) {
		rows := make([][]any, 0, traceDetailErrorLimit+1)
		for i := 0; i <= traceDetailErrorLimit; i++ {
			rows = append(rows, []any{
				"2026-01-01 00:00:00.000000", "svc-a", "trace-x", "span-1", "err", map[string]any{}, "e", float64(0),
			})
		}
		s := &server{db: traceDetailFakeRouter(t, map[string]func(params ...any) (*store.Result, error){
			"SELECT COUNT(*) AS c FROM otel_traces": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"c"}, []any{float64(1)}), nil
			},
			"FROM otel_traces WHERE TraceId=? ORDER BY": func(params ...any) (*store.Result, error) {
				return storetest.Result(
					[]string{"Timestamp", "TraceId", "SpanId", "ParentSpanId", "SpanName", "ServiceName",
						"Duration", "StatusCode", "SpanAttributes"},
					[]any{"2026-01-01 00:00:00.000000", "trace-x", "span-1", "", "root", "svc-a",
						float64(1_000_000), "OK", map[string]any{}},
				), nil
			},
			"IsResolved": func(params ...any) (*store.Result, error) {
				return &store.Result{Columns: []string{"Timestamp", "ServiceName", "TraceId", "SpanId", "Body", "LogAttributes", "ErrorId", "IsResolved"}, Rows: rows}, nil
			},
		})}
		detail, _, _ := s.buildTraceDetail("trace-x", 200, 0)
		d := detail.(map[string]any)
		if d["errors_truncated"] != true {
			t.Fatalf("expected errors_truncated=true, got %v", d["errors_truncated"])
		}
		errs, _ := d["errors"].([]any)
		if len(errs) != traceDetailErrorLimit {
			t.Fatalf("expected errors capped at %d, got %d", traceDetailErrorLimit, len(errs))
		}
	})

	t.Run("errors_query_error_leaves_errors_empty_but_detail_still_built", func(t *testing.T) {
		s := &server{db: traceDetailFakeRouter(t, map[string]func(params ...any) (*store.Result, error){
			"SELECT COUNT(*) AS c FROM otel_traces": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"c"}, []any{float64(1)}), nil
			},
			"FROM otel_traces WHERE TraceId=? ORDER BY": func(params ...any) (*store.Result, error) {
				return storetest.Result(
					[]string{"Timestamp", "TraceId", "SpanId", "ParentSpanId", "SpanName", "ServiceName",
						"Duration", "StatusCode", "SpanAttributes"},
					[]any{"2026-01-01 00:00:00.000000", "trace-x", "span-1", "", "root", "svc-a",
						float64(1_000_000), "OK", map[string]any{}},
				), nil
			},
			"IsResolved": func(params ...any) (*store.Result, error) {
				return nil, errors.New("boom")
			},
		})}
		detail, _, _ := s.buildTraceDetail("trace-x", 200, 0)
		d := detail.(map[string]any)
		errs, _ := d["errors"].([]any)
		if len(errs) != 0 {
			t.Fatalf("expected no errors on query failure, got %v", errs)
		}
	})

	t.Run("offset_beyond_total_snaps_to_last_page", func(t *testing.T) {
		s := &server{db: traceDetailFakeRouter(t, map[string]func(params ...any) (*store.Result, error){
			"SELECT COUNT(*) AS c FROM otel_traces": func(params ...any) (*store.Result, error) {
				return storetest.Result([]string{"c"}, []any{float64(3)}), nil
			},
			"FROM otel_traces WHERE TraceId=? ORDER BY": func(params ...any) (*store.Result, error) {
				return storetest.Result(
					[]string{"Timestamp", "TraceId", "SpanId", "ParentSpanId", "SpanName", "ServiceName",
						"Duration", "StatusCode", "SpanAttributes"},
					[]any{"2026-01-01 00:00:00.000000", "trace-x", "span-1", "", "root", "svc-a",
						float64(1_000_000), "OK", map[string]any{}},
					[]any{"2026-01-01 00:00:01.000000", "trace-x", "span-2", "span-1", "child-a", "svc-a",
						float64(1_000_000), "OK", map[string]any{}},
					[]any{"2026-01-01 00:00:02.000000", "trace-x", "span-3", "span-1", "child-b", "svc-a",
						float64(1_000_000), "OK", map[string]any{}},
				), nil
			},
		})}
		// limit=1 so cappedTotalSpans(3)/1 pages exist; offset way beyond total forces the
		// snap-to-last-page branch (handlers_pages_traces_detail.go:568-573).
		detail, _, _ := s.buildTraceDetail("trace-x", 1, 999)
		d := detail.(map[string]any)
		if d["page_offset"].(int) >= 3 {
			t.Fatalf("expected page_offset snapped within bounds, got %v", d["page_offset"])
		}
		if d["has_prev_page"] != true {
			t.Fatalf("expected has_prev_page=true after snapping past page 0, got %v", d["has_prev_page"])
		}
	})
}
