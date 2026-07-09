package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// cov95_b15_work_items_test.go — batch 15 coverage for cmd/sobs/work_items.go:
//   workItemsTimeWindow (130)  63.3%

func wiReq(rawQuery string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/work-items?"+rawQuery, nil)
}

func TestWorkItemsTimeWindow(t *testing.T) {
	const isoErr = "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"

	t.Run("no params at all -> empty window, no error", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq(""))
		if from != "" || to != "" || errMsg != "" {
			t.Errorf("got (%q, %q, %q), want all empty", from, to, errMsg)
		}
	})

	t.Run("from_ts and to_ts both set and valid", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-01T00:00:00Z&to_ts=2026-01-02T00:00:00Z"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if from != "2026-01-01 00:00:00.000000" || to != "2026-01-02 00:00:00.000000" {
			t.Errorf("from=%q to=%q", from, to)
		}
	})

	t.Run("to_ts before from_ts -> ordering error", func(t *testing.T) {
		_, _, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-02T00:00:00Z&to_ts=2026-01-01T00:00:00Z"))
		if errMsg != "Invalid time window: to_ts must be later than from_ts" {
			t.Errorf("errMsg = %q", errMsg)
		}
	})

	t.Run("to_ts equal to from_ts -> ordering error (strict After)", func(t *testing.T) {
		_, _, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-01T00:00:00Z&to_ts=2026-01-01T00:00:00Z"))
		if errMsg != "Invalid time window: to_ts must be later than from_ts" {
			t.Errorf("errMsg = %q", errMsg)
		}
	})

	t.Run("from_ts + window_s derives to_ts", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-01T00:00:00Z&window_s=3600"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if from != "2026-01-01 00:00:00.000000" {
			t.Errorf("from = %q", from)
		}
		if to != "2026-01-01 01:00:00.000000" {
			t.Errorf("to = %q, want +1h derived from window_s", to)
		}
	})

	t.Run("window_s below 1 is clamped to 1 second", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-01T00:00:00Z&window_s=0"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if to != "2026-01-01 00:00:01.000000" {
			t.Errorf("to = %q, want from+1s (window_s clamped to 1)", to)
		}
		_ = from
	})

	t.Run("negative window_s is clamped to 1 second", func(t *testing.T) {
		_, to, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-01T00:00:00Z&window_s=-100"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if to != "2026-01-01 00:00:01.000000" {
			t.Errorf("to = %q, want from+1s", to)
		}
	})

	t.Run("non-numeric window_s -> ISO error", func(t *testing.T) {
		_, _, errMsg := workItemsTimeWindow(wiReq("from_ts=2026-01-01T00:00:00Z&window_s=notanumber"))
		if errMsg != isoErr {
			t.Errorf("errMsg = %q, want %q", errMsg, isoErr)
		}
	})

	t.Run("to_ts present makes window_s irrelevant (not derived)", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq(
			"from_ts=2026-01-01T00:00:00Z&to_ts=2026-01-03T00:00:00Z&window_s=60"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if to != "2026-01-03 00:00:00.000000" {
			t.Errorf("to = %q, want the explicit to_ts (window_s ignored)", to)
		}
		_ = from
	})

	t.Run("only to_ts set, no from_ts -> passthrough, no window math", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq("to_ts=2026-01-02T00:00:00Z"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if from != "" {
			t.Errorf("from = %q, want empty (never set)", from)
		}
		if to != "2026-01-02 00:00:00.000000" {
			t.Errorf("to = %q", to)
		}
	})

	t.Run("whitespace-only params treated as absent", func(t *testing.T) {
		from, to, errMsg := workItemsTimeWindow(wiReq("from_ts=+&to_ts=+"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if from != "" || to != "" {
			t.Errorf("from=%q to=%q, want both empty (whitespace params ignored)", from, to)
		}
	})
}
