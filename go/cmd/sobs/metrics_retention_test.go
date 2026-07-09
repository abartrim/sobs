package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// applyRawMetricsRetention runs only at real-runtime startup (it early-returns under s.cfg.Parity),
// so the byte-parity corpus never reaches its ALTER loop. Exercise it directly through the store.DB
// seam with an injected fake. Oracle: app.py _ensure_raw_metrics_retention (the day/hour TTL ALTERs).

func TestApplyRawMetricsRetention_IssuesTTLAlters(t *testing.T) {
	var got []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		got = append(got, q)
		return &store.Result{}, nil
	}}} // cfg.Parity defaults false

	s.applyRawMetricsRetention()

	// One ALTER per raw table (baseline HOUR TTL) + one per pinned table (pinned DAY TTL).
	if want := len(rawMetricTables) + len(pinnedMetricTables); len(got) != want {
		t.Fatalf("statement count: got %d, want %d\n%s", len(got), want, strings.Join(got, "\n"))
	}
	for _, tbl := range rawMetricTables {
		want := fmt.Sprintf("ALTER TABLE %s MODIFY TTL TimeUnixMs + INTERVAL %s HOUR", tbl, rawMetricsBaselineTTLDefaultHours)
		if !containsStmt(got, want) {
			t.Fatalf("missing raw-table ALTER %q in:\n%s", want, strings.Join(got, "\n"))
		}
	}
	for _, tbl := range pinnedMetricTables {
		want := fmt.Sprintf("ALTER TABLE %s MODIFY TTL TimeUnixMs + INTERVAL %s DAY", tbl, rawMetricsPinnedTTLDefaultDays)
		if !containsStmt(got, want) {
			t.Fatalf("missing pinned-table ALTER %q in:\n%s", want, strings.Join(got, "\n"))
		}
	}
}

func TestApplyRawMetricsRetention_SkipsWhenGuarded(t *testing.T) {
	// Parity mode → early return, no ALTERs even with a live store.
	calls := 0
	sParity := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		calls++
		return &store.Result{}, nil
	}}}
	sParity.cfg.Parity = true
	sParity.applyRawMetricsRetention()
	if calls != 0 {
		t.Fatalf("parity mode issued %d statements, want 0", calls)
	}

	// nil store → no panic, no work.
	(&server{}).applyRawMetricsRetention()
}

func TestApplyRawMetricsRetention_ContinuesOnError(t *testing.T) {
	// Every ALTER fails; the loop must attempt all of them (error is logged, not fatal).
	attempts := 0
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		attempts++
		return nil, errors.New("ILLEGAL_TYPE")
	}}}
	s.applyRawMetricsRetention()
	if want := len(rawMetricTables) + len(pinnedMetricTables); attempts != want {
		t.Fatalf("attempts on error: got %d, want %d", attempts, want)
	}
}

func containsStmt(stmts []string, want string) bool {
	for _, s := range stmts {
		if s == want {
			return true
		}
	}
	return false
}
