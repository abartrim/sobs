package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// rumErrorStats runs five sequential hyperdx_sessions queries (trend, 24h type breakdown,
// 180-minute sparkline, top messages, top urls), each early-exiting the whole function on error.
// The empty-fixture corpus profile only exercises the "everything empty, all succeed" path, so
// the trend-direction math and each individual early-exit are corpus-unreachable.
// Oracle: app.py view_rum's "Error trend" block.

// rumStatsQueryKind classifies a rumErrorStats query by a distinguishing substring, in the same
// order the function issues them.
func rumStatsQueryKind(q string) string {
	switch {
	case strings.Contains(q, "AS recent"):
		return "trend"
	case strings.Contains(q, "GROUP BY EventName"):
		return "byType"
	case strings.Contains(q, "WITH FILL"):
		return "sparkline"
	case strings.Contains(q, "JSONExtractString(Body, 'message')"):
		return "messages"
	case strings.Contains(q, "LogAttributes['url']"):
		return "urls"
	}
	return "unknown"
}

// rumStatsDB answers each query kind from a success-result map; a kind present in failOn errors
// instead. Kinds absent from both return an empty (success) result.
func rumStatsDB(t *testing.T, results map[string]*store.Result, failOn map[string]bool) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		kind := rumStatsQueryKind(q)
		if kind == "unknown" {
			t.Fatalf("unrecognized query: %s", q)
		}
		if failOn[kind] {
			return nil, errors.New("boom")
		}
		if res, ok := results[kind]; ok {
			return res, nil
		}
		return &store.Result{}, nil
	}}
}

func TestRumErrorStats_FullSuccess(t *testing.T) {
	results := map[string]*store.Result{
		"trend":     storetest.Result([]string{"recent", "prior"}, []any{10.0, 4.0}), // 10 > 4*1.25 -> up
		"byType":    storetest.Result([]string{"EventName", "cnt"}, []any{"error", 7.0}, []any{"unhandledrejection", 3.0}),
		"sparkline": storetest.Result([]string{"mb", "cnt"}, []any{"2024-01-01 00:00:00", 2.0}),
		"messages":  storetest.Result([]string{"message", "cnt"}, []any{"Boom failed", 5.0}),
		"urls":      storetest.Result([]string{"url", "cnt"}, []any{"/checkout", 5.0}),
	}
	s := &server{db: rumStatsDB(t, results, nil)}
	got := s.rumErrorStats()

	check := func(key string, want any) {
		v, ok := got.Get(key)
		if !ok || v != want {
			t.Fatalf("%s: got %v (present=%v), want %v", key, v, ok, want)
		}
	}
	check("trend", "up")
	check("recent", 10)
	check("prior", 4)
	check("total", 10)

	byType, _ := got.Get("by_type")
	bt := byType.(*jsonenc.Object)
	if v, _ := bt.Get("error"); v != 7 {
		t.Fatalf("by_type.error: got %v", v)
	}
	if v, _ := bt.Get("unhandledrejection"); v != 3 {
		t.Fatalf("by_type.unhandledrejection: got %v", v)
	}

	if v, _ := got.Get("sparkline"); len(v.([]any)) != 1 {
		t.Fatalf("sparkline: want 1 point, got %v", v)
	}
	if v, _ := got.Get("top_messages"); len(v.([]any)) != 1 {
		t.Fatalf("top_messages: want 1, got %v", v)
	}
	if v, _ := got.Get("top_urls"); len(v.([]any)) != 1 {
		t.Fatalf("top_urls: want 1, got %v", v)
	}
}

func TestRumErrorStats_TrendBranches(t *testing.T) {
	cases := []struct {
		name          string
		recent, prior float64
		want          string
	}{
		{"zero_prior_zero_recent_stable", 0, 0, "stable"},
		{"zero_prior_nonzero_recent_up", 5, 0, "up"},
		{"big_jump_up", 10, 4, "up"},
		{"big_drop_down", 1, 10, "down"},
		{"within_band_stable", 5, 5, "stable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := map[string]*store.Result{
				"trend": storetest.Result([]string{"recent", "prior"}, []any{tc.recent, tc.prior}),
			}
			s := &server{db: rumStatsDB(t, results, nil)}
			got := s.rumErrorStats()
			if v, _ := got.Get("trend"); v != tc.want {
				t.Fatalf("trend: got %v, want %v", v, tc.want)
			}
		})
	}
}

func TestRumErrorStats_EarlyExitOnEachQuery(t *testing.T) {
	for _, kind := range []string{"trend", "byType", "sparkline", "messages", "urls"} {
		t.Run(kind, func(t *testing.T) {
			s := &server{db: rumStatsDB(t, nil, map[string]bool{kind: true})}
			got := s.rumErrorStats()
			// Every early exit returns the untouched defaults.
			if v, _ := got.Get("total"); v != 0 {
				t.Fatalf("%s: total should stay at default 0, got %v", kind, v)
			}
			if v, _ := got.Get("sparkline"); len(v.([]any)) != 0 {
				t.Fatalf("%s: sparkline should stay empty, got %v", kind, v)
			}
		})
	}
}
