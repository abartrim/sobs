package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 5: cmd/sobs/handlers_incident.go undertested branches. Oracle: app.py's
// view_incident helper surface (_try_pretty_json_text, _first_scalar, _to_summary,
// _get_resolved_error_ids, _parse_time_window_args, _window_copy_counts,
// _list_trace_overlapping_raw_windows, _compute_health_chips, _fetch_trace_metric_context,
// _load_work_item_links_for_ref_ids, _map_to_dict/_compact_text/attrsToStringMap).

// --- tryPrettyJSONText ------------------------------------------------------------------

func TestTryPrettyJSONText_Cov95B5(t *testing.T) {
	t.Run("empty_string_not_json", func(t *testing.T) {
		ok, pretty := tryPrettyJSONText("")
		if ok || pretty != "" {
			t.Fatalf("got (%v,%q), want (false,\"\")", ok, pretty)
		}
	})
	t.Run("plain_text_not_json", func(t *testing.T) {
		ok, _ := tryPrettyJSONText("just a message")
		if ok {
			t.Fatal("expected not-json for plain text")
		}
	})
	t.Run("malformed_json_object", func(t *testing.T) {
		ok, _ := tryPrettyJSONText(`{"a": }`)
		if ok {
			t.Fatal("expected parse failure to yield ok=false")
		}
	})
	t.Run("valid_object_pretty_printed", func(t *testing.T) {
		ok, pretty := tryPrettyJSONText(`{"a":1,"b":"x"}`)
		if !ok {
			t.Fatal("expected ok=true for valid object")
		}
		if !strings.Contains(pretty, "\"a\": 1") {
			t.Fatalf("expected indented pretty JSON, got %q", pretty)
		}
	})
	t.Run("valid_array_pretty_printed", func(t *testing.T) {
		ok, pretty := tryPrettyJSONText(`[1,2,3]`)
		if !ok || !strings.Contains(pretty, "1") {
			t.Fatalf("got (%v,%q)", ok, pretty)
		}
	})
}

// --- firstScalarFromJSON / isJSONScalar / jsonScalarToStr -------------------------------

func TestFirstScalarFromJSON_Cov95B5(t *testing.T) {
	keyset := map[string]bool{"message": true}

	t.Run("depth_exceeded_returns_empty", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("message", jsonenc.NewObject())
		got := firstScalarFromJSON(obj, keyset, 6)
		if got != "" {
			t.Fatalf("expected empty at depth>5, got %q", got)
		}
	})

	t.Run("direct_match_string", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("message", "  hello  ")
		got := firstScalarFromJSON(obj, keyset, 0)
		if got != "hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("descend_into_nested_object", func(t *testing.T) {
		inner := jsonenc.NewObject().Set("message", "nested-msg")
		obj := jsonenc.NewObject().Set("other", inner)
		got := firstScalarFromJSON(obj, keyset, 0)
		if got != "nested-msg" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("descend_into_array", func(t *testing.T) {
		inner := jsonenc.NewObject().Set("message", "arr-msg")
		arr := []any{inner}
		got := firstScalarFromJSON(arr, keyset, 0)
		if got != "arr-msg" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("plain_map_direct_match", func(t *testing.T) {
		m := map[string]any{"message": "plain-map-msg"}
		got := firstScalarFromJSON(m, keyset, 0)
		if got != "plain-map-msg" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("plain_map_descend", func(t *testing.T) {
		m := map[string]any{"outer": map[string]any{"message": "deep-msg"}}
		got := firstScalarFromJSON(m, keyset, 0)
		if got != "deep-msg" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("non_scalar_non_container_returns_empty", func(t *testing.T) {
		got := firstScalarFromJSON(nil, keyset, 0)
		if got != "" {
			t.Fatalf("expected empty for nil, got %q", got)
		}
	})

	t.Run("descend_pass_recurses_into_every_value_including_unmatched_scalars", func(t *testing.T) {
		// The descend pass (unlike the direct-match pass) recurses into every child
		// unconditionally; recursing into a bare scalar hits the `default:` branch, which
		// treats "is this value itself a scalar" as a match regardless of the key name. So an
		// object whose only key ISN'T in keyset can still yield that key's own scalar value.
		obj := jsonenc.NewObject().Set("unrelated_key", "bar")
		got := firstScalarFromJSON(obj, keyset, 0)
		if got != "bar" {
			t.Fatalf("got %q, want %q", got, "bar")
		}
	})

	t.Run("no_match_when_only_containers_with_no_scalars", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("unrelated_key", jsonenc.NewObject())
		got := firstScalarFromJSON(obj, keyset, 0)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})
}

func TestIsJSONScalar_Cov95B5(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{
		{"str", true},
		{true, true},
		{float64(1.5), true},
		{map[string]any{}, false},
		{[]any{}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isJSONScalar(c.v); got != c.want {
			t.Errorf("isJSONScalar(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestJSONScalarToStr_Cov95B5(t *testing.T) {
	if got := jsonScalarToStr("hi"); got != "hi" {
		t.Errorf("string: got %q", got)
	}
	if got := jsonScalarToStr(true); got != "True" {
		t.Errorf("true: got %q", got)
	}
	if got := jsonScalarToStr(false); got != "False" {
		t.Errorf("false: got %q", got)
	}
	if got := jsonScalarToStr(float64(3.5)); got != "3.5" {
		t.Errorf("float: got %q", got)
	}
	// default branch: unrecognized type falls to fmt.Sprintf("%v", v)
	if got := jsonScalarToStr([]int{1, 2}); got != "[1 2]" {
		t.Errorf("default branch: got %q", got)
	}
}

// --- summaryFromParsed -------------------------------------------------------------------

func TestSummaryFromParsed_Cov95B5(t *testing.T) {
	t.Run("non_dict_non_array_returns_empty", func(t *testing.T) {
		if got := summaryFromParsed("just a string"); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("empty_array_uses_empty_object", func(t *testing.T) {
		if got := summaryFromParsed([]any{}); got != "" {
			t.Fatalf("expected empty summary for empty array, got %q", got)
		}
	})

	t.Run("array_first_elem_used", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("message", "arr-elem-msg")
		got := summaryFromParsed([]any{obj})
		if got != "arr-elem-msg" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("message_with_type_and_code_extras", func(t *testing.T) {
		obj := jsonenc.NewObject().
			Set("message", "connection failed").
			Set("type", "NetworkError").
			Set("code", "500")
		got := summaryFromParsed(obj)
		if !strings.Contains(got, "connection failed") || !strings.Contains(got, "NetworkError") || !strings.Contains(got, "code 500") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("message_already_contains_type_and_code_no_dup", func(t *testing.T) {
		obj := jsonenc.NewObject().
			Set("message", "NetworkError code 500 occurred").
			Set("type", "NetworkError").
			Set("code", "500")
		got := summaryFromParsed(obj)
		if strings.Contains(got, "[") {
			t.Fatalf("expected no extras bracket since already present, got %q", got)
		}
	})

	// NOTE on the remaining branches (handlers_incident.go:254-263, the typeText/codeText-only
	// tails): firstScalarFromJSON's descend pass (see TestFirstScalarFromJSON_Cov95B5/
	// descend_pass_recurses_into_every_value_including_unmatched_scalars above) recurses into
	// *every* child value unconditionally, and a bare scalar child always "matches" in the
	// default branch regardless of its key. So probing with textKeys against any object that
	// has at least one scalar-valued key anywhere (which type/code objects always do) makes
	// messageText non-empty, and control never reaches the typeText/codeText-only tail
	// branches for realistic (or even adversarial, scalar-only) JSON payloads — only a dict
	// containing solely non-scalar values (nested empty containers) could skip the messageText
	// branch, but then typeText/codeText would also be empty. These branches mirror app.py's
	// defensive dead code and are effectively unreachable; asserting the ACTUAL (messageText-path)
	// outcome below documents that behavior instead of a value this code cannot produce.
	t.Run("type_and_code_present_but_message_text_wins_via_descend_quirk", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("type", "TimeoutError").Set("code", "408")
		got := summaryFromParsed(obj)
		if got != "TimeoutError [code 408]" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("code_only_present_but_message_text_wins_via_descend_quirk", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("code", "408")
		got := summaryFromParsed(obj)
		if got != "408" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unrelated_scalar_key_still_yields_a_message_via_descend_quirk", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("unrelated", "x")
		got := summaryFromParsed(obj)
		if got != "x" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("truly_empty_object_returns_empty", func(t *testing.T) {
		got := summaryFromParsed(jsonenc.NewObject())
		if got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

// --- resolvedErrorIDs ----------------------------------------------------------------------

func TestResolvedErrorIDs_Cov95B5(t *testing.T) {
	t.Run("db_error_returns_empty_map", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.resolvedErrorIDs()
		if len(got) != 0 {
			t.Fatalf("expected empty map on error, got %v", got)
		}
	})

	t.Run("rows_populate_set", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result([]string{"ErrorId"}, []any{"e1"}, []any{"e2"}), nil
		}}}
		got := s.resolvedErrorIDs()
		if !got["e1"] || !got["e2"] || len(got) != 2 {
			t.Fatalf("got %v", got)
		}
	})
}

// --- parseTimeWindowArgsQuery / parseISOLocalNaive / tsStrToEpochMs -------------------------

func TestParseTimeWindowArgsQuery_Cov95B5(t *testing.T) {
	t.Run("no_args_returns_empty", func(t *testing.T) {
		from, to, errMsg := parseTimeWindowArgsQuery(map[string][]string{})
		if from != "" || to != "" || errMsg != "" {
			t.Fatalf("got (%q,%q,%q)", from, to, errMsg)
		}
	})

	t.Run("from_and_window_s_computes_to", func(t *testing.T) {
		q := map[string][]string{
			"from_ts":  {"2026-01-01T00:00:00Z"},
			"window_s": {"60"},
		}
		from, to, errMsg := parseTimeWindowArgsQuery(q)
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if from == "" || to == "" {
			t.Fatalf("expected both from/to populated, got (%q,%q)", from, to)
		}
	})

	t.Run("window_s_invalid_int", func(t *testing.T) {
		q := map[string][]string{
			"from_ts":  {"2026-01-01T00:00:00Z"},
			"window_s": {"not-a-number"},
		}
		_, _, errMsg := parseTimeWindowArgsQuery(q)
		if errMsg == "" {
			t.Fatal("expected error for invalid window_s")
		}
	})

	t.Run("window_s_clamped_to_min_1", func(t *testing.T) {
		q := map[string][]string{
			"from_ts":  {"2026-01-01T00:00:00Z"},
			"window_s": {"0"},
		}
		_, to, errMsg := parseTimeWindowArgsQuery(q)
		if errMsg != "" || to == "" {
			t.Fatalf("expected clamp to 1s window, got errMsg=%q to=%q", errMsg, to)
		}
	})

	t.Run("from_invalid_with_window_s", func(t *testing.T) {
		// normalizeCHTimestamp on garbage still returns something parseISOLocalNaive rejects.
		q := map[string][]string{
			"from_ts":  {"not-a-timestamp-at-all"},
			"window_s": {"60"},
		}
		_, _, errMsg := parseTimeWindowArgsQuery(q)
		if errMsg == "" {
			t.Fatal("expected invalid time value error")
		}
	})

	t.Run("both_from_and_to_but_to_before_from", func(t *testing.T) {
		q := map[string][]string{
			"from_ts": {"2026-01-02T00:00:00Z"},
			"to_ts":   {"2026-01-01T00:00:00Z"},
		}
		_, _, errMsg := parseTimeWindowArgsQuery(q)
		if !strings.Contains(errMsg, "to_ts must be later") {
			t.Fatalf("got %q", errMsg)
		}
	})

	t.Run("both_from_and_to_valid_ok", func(t *testing.T) {
		q := map[string][]string{
			"from_ts": {"2026-01-01T00:00:00Z"},
			"to_ts":   {"2026-01-02T00:00:00Z"},
		}
		from, to, errMsg := parseTimeWindowArgsQuery(q)
		if errMsg != "" || from == "" || to == "" {
			t.Fatalf("got (%q,%q,%q)", from, to, errMsg)
		}
	})

	t.Run("from_and_to_unparseable_naive", func(t *testing.T) {
		// normalizeCHTimestamp passes through unrecognized text unmodified per mutation_helpers;
		// feeding straight garbage through both from_ts/to_ts should trip the ok1/ok2 guard.
		q := map[string][]string{
			"from_ts": {"zzzzzz"},
			"to_ts":   {"zzzzzz"},
		}
		_, _, errMsg := parseTimeWindowArgsQuery(q)
		if errMsg == "" {
			t.Fatal("expected invalid time value error for unparseable naive times")
		}
	})
}

func TestParseISOLocalNaive_Cov95B5(t *testing.T) {
	t.Run("valid_space_separated", func(t *testing.T) {
		_, ok := parseISOLocalNaive("2026-01-01 00:00:00.000000")
		if !ok {
			t.Fatal("expected ok=true")
		}
	})
	t.Run("invalid_garbage", func(t *testing.T) {
		_, ok := parseISOLocalNaive("not-a-time")
		if ok {
			t.Fatal("expected ok=false")
		}
	})
}

func TestTsStrToEpochMs_Cov95B5(t *testing.T) {
	t.Run("empty_input", func(t *testing.T) {
		if got := tsStrToEpochMs(""); got != 0.0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("fractional_seconds_truncated_and_padded", func(t *testing.T) {
		got := tsStrToEpochMs("2026-01-01 00:00:00.5")
		if got <= 0 {
			t.Fatalf("expected positive epoch ms, got %v", got)
		}
	})
	t.Run("long_fraction_truncated_to_6_digits", func(t *testing.T) {
		got := tsStrToEpochMs("2026-01-01 00:00:00.123456789")
		if got <= 0 {
			t.Fatalf("expected positive epoch ms, got %v", got)
		}
	})
	t.Run("unparseable_returns_zero", func(t *testing.T) {
		if got := tsStrToEpochMs("garbage"); got != 0.0 {
			t.Fatalf("got %v", got)
		}
	})
}

// --- windowCopyCounts ------------------------------------------------------------------
// _window_copy_counts is reached from listTraceOverlappingRawWindows whenever the raw-window
// query returns rows (see handlers_incident.go:546) — not gated behind any background-only
// path — so it is directly unit-testable via a fake DB.

func TestWindowCopyCounts_Cov95B5(t *testing.T) {
	t.Run("empty_window_ids_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			t.Fatal("Execute should not be called for empty windowIDs")
			return nil, nil
		}}}
		got := s.windowCopyCounts(nil)
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %v", got)
		}
	})

	t.Run("db_error_returns_empty_map", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.windowCopyCounts([]string{"w1"})
		if len(got) != 0 {
			t.Fatalf("expected empty map on error, got %v", got)
		}
	})

	t.Run("rows_populate_counts_by_window_id", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if !strings.Contains(q, "sobs_raw_window_copy_state") {
				t.Fatalf("unexpected query: %q", q)
			}
			if len(p) != 2 || p[0] != "w1" || p[1] != "w2" {
				t.Fatalf("unexpected params: %v", p)
			}
			return storetest.Result([]string{"WindowId", "c"}, []any{"w1", float64(2)}, []any{"w2", float64(3)}), nil
		}}}
		got := s.windowCopyCounts([]string{"w1", "w2"})
		if got["w1"] != 2 || got["w2"] != 3 {
			t.Fatalf("got %v", got)
		}
	})
}

// --- listTraceOverlappingRawWindows ------------------------------------------------------

func TestListTraceOverlappingRawWindows_Cov95B5(t *testing.T) {
	t.Run("db_error_returns_empty_slice", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.listTraceOverlappingRawWindows([]string{"svc"}, "2026-01-01 00:00:00", "2026-01-01 01:00:00", 25)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})

	t.Run("no_rows_returns_empty_slice", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		got := s.listTraceOverlappingRawWindows(nil, "2026-01-01 00:00:00", "2026-01-01 01:00:00", 25)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})

	t.Run("limit_clamped_low_and_high", func(t *testing.T) {
		var capturedLimits []any
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_raw_windows") {
				capturedLimits = append(capturedLimits, p[len(p)-1])
				return &store.Result{}, nil
			}
			return &store.Result{}, nil
		}}}
		s.listTraceOverlappingRawWindows(nil, "a", "b", 0)
		s.listTraceOverlappingRawWindows(nil, "a", "b", 999)
		if len(capturedLimits) != 2 || capturedLimits[0] != 1 || capturedLimits[1] != 100 {
			t.Fatalf("expected clamp to [1,100], got %v", capturedLimits)
		}
	})

	t.Run("with_service_names_and_rows_populates_copy_complete", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_raw_windows"):
				if !strings.Contains(q, "ServiceName IN") {
					t.Fatalf("expected ServiceName IN clause, got %q", q)
				}
				return storetest.Result(
					[]string{"Id", "SignalType", "SignalRef", "ServiceName", "Namespace", "NodeName", "WindowStart", "WindowEnd"},
					[]any{"w1", "traces", "ref1", "svc-a", "ns1", "node1", "2026-01-01 00:00:00", "2026-01-01 00:05:00"},
				), nil
			case strings.Contains(q, "sobs_raw_window_copy_state"):
				return storetest.Result([]string{"WindowId", "c"}, []any{"w1", float64(3)}), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.listTraceOverlappingRawWindows([]string{"svc-a"}, "2026-01-01 00:00:00", "2026-01-01 00:10:00", 25)
		if len(got) != 1 {
			t.Fatalf("expected 1 window, got %v", got)
		}
		w := got[0].(map[string]any)
		if w["copied_count"] != 3 || w["expected_count"] != len(rawMetricTables) || w["copy_complete"] != true {
			t.Fatalf("got %v", w)
		}
	})
}

// --- computeHealthChips ------------------------------------------------------------------

func TestComputeHealthChips_Cov95B5(t *testing.T) {
	mk := func(metric string, avg, max float64) any {
		return map[string]any{"metric": metric, "avg": avg, "max": max}
	}

	t.Run("cpu_crit_warn_ok_levels", func(t *testing.T) {
		series := []any{mk("cpu_utilization", 90.0, 90.0)}
		chips := computeHealthChips(series)
		if len(chips) != 1 {
			t.Fatalf("expected 1 chip, got %d", len(chips))
		}
		c := chips[0].(map[string]any)
		if c["level"] != "crit" || c["label"] != "CPU" {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("cpu_warn_level", func(t *testing.T) {
		chips := computeHealthChips([]any{mk("system.cpu.usage", 70.0, 70.0)})
		c := chips[0].(map[string]any)
		if c["level"] != "warn" {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("memory_failures_crit", func(t *testing.T) {
		chips := computeHealthChips([]any{mk("mem_failures_total", 1.0, 2000.0)})
		c := chips[0].(map[string]any)
		if c["label"] != "Mem Faults" || c["level"] != "crit" {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("memory_failures_warn", func(t *testing.T) {
		chips := computeHealthChips([]any{mk("memory_failures", 1.0, 5.0)})
		c := chips[0].(map[string]any)
		if c["level"] != "warn" {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("memory_failures_ok", func(t *testing.T) {
		chips := computeHealthChips([]any{mk("memory_failures", 1.0, 0.0)})
		c := chips[0].(map[string]any)
		if c["level"] != "ok" {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("memory_usage_gb_format", func(t *testing.T) {
		gbBytes := 2.0 * 1024 * 1024 * 1024
		chips := computeHealthChips([]any{mk("memory_usage_bytes", gbBytes, gbBytes)})
		c := chips[0].(map[string]any)
		if c["label"] != "Memory" || !strings.HasSuffix(c["value"].(string), "GB") {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("memory_usage_mb_format_when_small", func(t *testing.T) {
		small := 5.0 * 1024 * 1024
		chips := computeHealthChips([]any{mk("memory_usage_bytes", small, small)})
		c := chips[0].(map[string]any)
		if !strings.HasSuffix(c["value"].(string), "MB") {
			t.Fatalf("got %v", c)
		}
	})

	t.Run("pod_phase_levels", func(t *testing.T) {
		okChip := computeHealthChips([]any{mk("kube_pod_status_phase", 0.95, 0.95)})[0].(map[string]any)
		if okChip["level"] != "ok" {
			t.Fatalf("got %v", okChip)
		}
		warnChip := computeHealthChips([]any{mk("pod_phase", 0.6, 0.6)})[0].(map[string]any)
		if warnChip["level"] != "warn" {
			t.Fatalf("got %v", warnChip)
		}
		critChip := computeHealthChips([]any{mk("pod_phase", 0.1, 0.1)})[0].(map[string]any)
		if critChip["level"] != "crit" {
			t.Fatalf("got %v", critChip)
		}
	})

	t.Run("tasks_state_levels", func(t *testing.T) {
		okChip := computeHealthChips([]any{mk("tasks_state", 0.0, 0.0)})[0].(map[string]any)
		if okChip["level"] != "ok" {
			t.Fatalf("got %v", okChip)
		}
		critChip := computeHealthChips([]any{mk("tasks_state", 5.0, 5.0)})[0].(map[string]any)
		if critChip["level"] != "crit" {
			t.Fatalf("got %v", critChip)
		}
	})

	t.Run("unmatched_metric_produces_no_chip", func(t *testing.T) {
		chips := computeHealthChips([]any{mk("something_unrelated", 1.0, 1.0)})
		if len(chips) != 0 {
			t.Fatalf("expected no chips, got %v", chips)
		}
	})

	t.Run("stops_at_six_chips", func(t *testing.T) {
		series := make([]any, 0, 10)
		for i := 0; i < 10; i++ {
			series = append(series, mk("cpu_utilization", 10.0, 10.0))
		}
		chips := computeHealthChips(series)
		if len(chips) != 6 {
			t.Fatalf("expected 6 chips (cap), got %d", len(chips))
		}
	})
}

// --- fetchTraceMetricContext -------------------------------------------------------------

func TestFetchTraceMetricContext_Cov95B5(t *testing.T) {
	t.Run("no_match_returns_none_source_mode", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil // every stats query returns zero rows -> continue to next attempt
		}}}
		got := s.fetchTraceMetricContext(nil, "", "", nil, 12, nil, nil, nil, nil)
		if got["source_mode"] != "none" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("service_exact_match_populates_series_and_chips", func(t *testing.T) {
		callCount := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "min(DedupRank)"):
				callCount++
				return storetest.Result([]string{"c", "min_rank", "max_rank"}, []any{float64(10), float64(0), float64(0)}), nil
			case strings.Contains(q, "GROUP BY ServiceName, MetricName") && strings.Contains(q, "ORDER BY points DESC"):
				return storetest.Result(
					[]string{"ServiceName", "MetricName", "points", "avg_value", "min_value", "max_value"},
					[]any{"svc-a", "cpu_utilization", float64(5), float64(50.0), float64(10.0), float64(90.0)},
				), nil
			case strings.Contains(q, "BucketIdx"):
				return storetest.Result([]string{"MetricName", "BucketIdx", "AvgVal"}, []any{"cpu_utilization", float64(0), float64(42.0)}), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.fetchTraceMetricContext(
			[]string{"svc-a"}, "2026-01-01 00:00:00.000000", "2026-01-01 00:10:00.000000",
			nil, 12, nil, nil, nil, nil)
		if got["source_mode"] != "raw" {
			t.Fatalf("expected raw source_mode, got %v", got["source_mode"])
		}
		if got["match_mode"] != "service_exact" {
			t.Fatalf("expected service_exact match, got %v", got["match_mode"])
		}
		if got["health_chips"] == nil {
			t.Fatal("expected health_chips populated")
		}
		if got["header_chip"] == nil {
			t.Fatal("expected header_chip populated for CPU metric")
		}
		if got["timeseries"] == nil {
			t.Fatal("expected timeseries populated")
		}
		if callCount == 0 {
			t.Fatal("expected at least one stats query")
		}
	})

	t.Run("pinned_source_mode_when_ranks_are_one", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "min(DedupRank)"):
				return storetest.Result([]string{"c", "min_rank", "max_rank"}, []any{float64(5), float64(1), float64(1)}), nil
			case strings.Contains(q, "ORDER BY points DESC"):
				return storetest.Result(
					[]string{"ServiceName", "MetricName", "points", "avg_value", "min_value", "max_value"},
					[]any{"svc-b", "mem_usage", float64(5), float64(1.0), float64(1.0), float64(1.0)},
				), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.fetchTraceMetricContext([]string{"svc-b"}, "2026-01-01 00:00:00.000000", "2026-01-01 00:10:00.000000", nil, 12, nil, nil, nil, nil)
		if got["source_mode"] != "pinned" {
			t.Fatalf("expected pinned, got %v", got["source_mode"])
		}
	})

	t.Run("mixed_source_mode", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "min(DedupRank)"):
				return storetest.Result([]string{"c", "min_rank", "max_rank"}, []any{float64(5), float64(0), float64(1)}), nil
			case strings.Contains(q, "ORDER BY points DESC"):
				return storetest.Result(
					[]string{"ServiceName", "MetricName", "points", "avg_value", "min_value", "max_value"},
					[]any{"svc-c", "disk_io", float64(5), float64(1.0), float64(1.0), float64(1.0)},
				), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.fetchTraceMetricContext([]string{"svc-c"}, "2026-01-01 00:00:00.000000", "2026-01-01 00:10:00.000000", nil, 12, nil, nil, nil, nil)
		if got["source_mode"] != "mixed" {
			t.Fatalf("expected mixed, got %v", got["source_mode"])
		}
	})

	t.Run("pod_and_namespace_match_dimensions", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "min(DedupRank)"):
				return storetest.Result([]string{"c", "min_rank", "max_rank"}, []any{float64(1), float64(0), float64(0)}), nil
			case strings.Contains(q, "ORDER BY points DESC"):
				return storetest.Result(
					[]string{"ServiceName", "MetricName", "points", "avg_value", "min_value", "max_value"},
					[]any{"svc-d", "node.cpu", float64(1), float64(1.0), float64(1.0), float64(1.0)},
				), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.fetchTraceMetricContext(
			nil, "2026-01-01 00:00:00.000000", "2026-01-01 00:10:00.000000", nil, 12,
			[]string{"ns1"}, []string{"pod1"}, nil, nil)
		if got["match_mode"] != "pod_exact" {
			t.Fatalf("expected pod_exact, got %v", got["match_mode"])
		}
		dims, _ := got["match_dimensions"].([]any)
		if len(dims) != 2 {
			t.Fatalf("expected 2 dims, got %v", dims)
		}
	})

	t.Run("second_stats_query_error_skips_attempt", func(t *testing.T) {
		// rowsRes query errors -> `continue` inside query() closure, falling through
		// remaining attempts to source_mode none.
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "min(DedupRank)"):
				return storetest.Result([]string{"c", "min_rank", "max_rank"}, []any{float64(5), float64(0), float64(0)}), nil
			case strings.Contains(q, "ORDER BY points DESC"):
				return nil, errors.New("boom")
			}
			return &store.Result{}, nil
		}}}
		got := s.fetchTraceMetricContext([]string{"svc-e"}, "2026-01-01 00:00:00.000000", "2026-01-01 00:10:00.000000", nil, 12, nil, nil, nil, nil)
		if got["source_mode"] != "none" {
			t.Fatalf("expected none after all attempts erred, got %v", got["source_mode"])
		}
	})

	t.Run("family_and_deployment_and_node_attempts_reachable", func(t *testing.T) {
		// Names with a hyphen produce a family; deployment/node values also populate their
		// own ranked attempts (verifying those branches build without panicking).
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		got := s.fetchTraceMetricContext(
			[]string{"svc-a-1"}, "x", "y", nil, 12,
			[]string{"ns1"}, nil, []string{"node1"}, []string{"deploy1"})
		if got["source_mode"] != "none" {
			t.Fatalf("got %v", got)
		}
	})
}

// --- attrsToStringMap ----------------------------------------------------------------------

func TestAttrsToStringMap_Cov95B5(t *testing.T) {
	t.Run("nil_value_returns_empty", func(t *testing.T) {
		got := attrsToStringMap(nil)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("map_string_any", func(t *testing.T) {
		got := attrsToStringMap(map[string]any{"a": "b", "n": float64(1)})
		if got["a"] != "b" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("jsonenc_object", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("k", "v")
		got := attrsToStringMap(obj)
		if got["k"] != "v" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("empty_string_returns_empty", func(t *testing.T) {
		got := attrsToStringMap("   ")
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("json_string_parsed", func(t *testing.T) {
		got := attrsToStringMap(`{"x":"y"}`)
		if got["x"] != "y" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("malformed_json_string_returns_empty", func(t *testing.T) {
		got := attrsToStringMap(`{not json`)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("unrecognized_type_returns_empty", func(t *testing.T) {
		got := attrsToStringMap(42)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}

// --- compactText ---------------------------------------------------------------------------

func TestCompactText_Cov95B5(t *testing.T) {
	t.Run("short_text_unchanged", func(t *testing.T) {
		got := compactText("hello   world", 220)
		if got != "hello world" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("long_text_truncated_with_ellipsis", func(t *testing.T) {
		got := compactText(strings.Repeat("a", 300), 10)
		if !strings.HasSuffix(got, "...") {
			t.Fatalf("got %q", got)
		}
		// cut = limit-1 runes kept, then "..." appended: 9 + 3 = 12 runes total.
		if len([]rune(got)) != 12 {
			t.Fatalf("expected length 12, got %d (%q)", len([]rune(got)), got)
		}
	})
	t.Run("negative_limit_clamped", func(t *testing.T) {
		got := compactText("hello", -5)
		if got != "..." {
			t.Fatalf("got %q", got)
		}
	})
}

// --- loadWorkItemLinksForRefIDs --------------------------------------------------------------

func TestLoadWorkItemLinksForRefIDs_Cov95B5(t *testing.T) {
	t.Run("empty_ref_ids_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			t.Fatal("Execute should not be called for empty refIDs")
			return nil, nil
		}}}
		got := s.loadWorkItemLinksForRefIDs(nil)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("dedupes_empty_and_duplicate_ref_ids", func(t *testing.T) {
		var capturedParams []any
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			capturedParams = p
			return &store.Result{}, nil
		}}}
		s.loadWorkItemLinksForRefIDs([]string{"r1", "", "r1", "r2"})
		if len(capturedParams) != 2 {
			t.Fatalf("expected 2 deduped params, got %v", capturedParams)
		}
	})

	t.Run("db_error_returns_empty_map", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.loadWorkItemLinksForRefIDs([]string{"r1"})
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("issue_url_fallback_to_canonical", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"AnomalyRuleId", "IssueUrl", "CanonicalIssueUrl", "IssueNumber", "IssueState"},
				[]any{"r1", "", "https://canonical", float64(7), "open"},
			), nil
		}}}
		got := s.loadWorkItemLinksForRefIDs([]string{"r1"})
		item, ok := got["r1"].(map[string]any)
		if !ok {
			t.Fatalf("expected r1 entry, got %v", got)
		}
		if item["issue_url"] != "https://canonical" || item["issue_number"] != 7 || item["issue_state"] != "open" {
			t.Fatalf("got %v", item)
		}
	})

	t.Run("first_row_wins_when_duplicate_ref_returned", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"AnomalyRuleId", "IssueUrl", "CanonicalIssueUrl", "IssueNumber", "IssueState"},
				[]any{"r1", "https://first", "", float64(1), "open"},
				[]any{"r1", "https://second", "", float64(2), "closed"},
			), nil
		}}}
		got := s.loadWorkItemLinksForRefIDs([]string{"r1"})
		item := got["r1"].(map[string]any)
		if item["issue_url"] != "https://first" {
			t.Fatalf("expected first row to win, got %v", item)
		}
	})

	t.Run("row_ref_not_in_seen_set_ignored", func(t *testing.T) {
		// A row whose AnomalyRuleId wasn't in the (deduped) input refIDs must be skipped
		// (defensive: mirrors the `if seen[ref]` guard).
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"AnomalyRuleId", "IssueUrl", "CanonicalIssueUrl", "IssueNumber", "IssueState"},
				[]any{"unexpected-ref", "https://x", "", float64(1), "open"},
			), nil
		}}}
		got := s.loadWorkItemLinksForRefIDs([]string{"r1"})
		if len(got) != 0 {
			t.Fatalf("expected no entries for unmatched ref, got %v", got)
		}
	})
}
