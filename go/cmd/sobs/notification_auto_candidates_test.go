package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// notificationAutoCandidates derives candidate notification rules from active anomaly rules,
// skipping (source, signal) pairs already covered by an existing rule's conditions. The corpus
// never seeds a mix of covered/uncovered rules with varying threshold shapes, so the dedup and
// severity-selection branches are unexercised. Oracle: app.py _get_notification_auto_candidates.
func TestNotificationAutoCandidates(t *testing.T) {
	metricCols := []string{"Id", "Name", "SignalSource", "SignalName", "ServiceName", "Comparator",
		"WarningThreshold", "CriticalThreshold"}
	ruleCols := []string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"}
	chanCols := []string{"Id", "Name"}

	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_anomaly_rules"):
			if strings.Contains(q, "AND Id = ?") {
				t.Fatalf("unexpected scoped query in all-rules test: %s", q)
			}
			return storetest.Result(metricCols,
				[]any{"r1", "High CPU", "cpu", "usage", "svc-a", "gt", 70.0, 90.0}, // covered -> skipped
				[]any{"r2", "Mem Warn", "mem", "usage", "svc-b", "gt", 50.0, 0.0},  // warn only -> warning/50
				[]any{"r3", "Disk", "disk", "free", "svc-c", "lt", 0.0, 0.0},       // neither -> default warning/0
				[]any{"r4", "Net", "net", "errors", "svc-d", "gt", 0.0, 99.0},      // crit -> critical/99
			), nil
		case strings.Contains(q, "sobs_notification_rules"):
			return storetest.Result(ruleCols,
				[]any{"nr1", "CPU rule", float64(1), "any", `[{"type":"signal","source":"cpu","signal":"usage"}]`, "", "warning", 300.0},
			), nil
		case strings.Contains(q, "sobs_notification_channels"):
			return storetest.Result(chanCols,
				[]any{"ch1", "Slack"},
				[]any{"ch2", "Email"},
			), nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
	s := &server{db: fake}

	examined, skipped, candidates := s.notificationAutoCandidates("")
	if examined != 4 || skipped != 1 {
		t.Fatalf("want examined=4 skipped=1, got examined=%d skipped=%d", examined, skipped)
	}
	if len(candidates) != 3 {
		t.Fatalf("want 3 candidates, got %d: %v", len(candidates), candidates)
	}
	bySource := map[string]*jsonenc.Object{}
	for _, c := range candidates {
		o := c.(*jsonenc.Object)
		bySource[objGetStr(o, "source")] = o
	}
	if o := bySource["mem"]; o == nil || objGetStr(o, "severity") != "warning" || objGetFloat(o, "threshold") != 50.0 {
		t.Fatalf("mem candidate wrong: %v", o)
	}
	if o := bySource["disk"]; o == nil || objGetStr(o, "severity") != "warning" || objGetFloat(o, "threshold") != 0.0 {
		t.Fatalf("disk candidate wrong: %v", o)
	}
	if o := bySource["net"]; o == nil || objGetStr(o, "severity") != "critical" || objGetFloat(o, "threshold") != 99.0 {
		t.Fatalf("net candidate wrong: %v", o)
	}
	if o := bySource["mem"]; o != nil {
		ids, _ := func() ([]any, bool) { v, ok := o.Get("channel_ids"); l, k := v.([]any); return l, ok && k }()
		if len(ids) != 2 {
			t.Fatalf("want 2 channel ids on every candidate, got %v", ids)
		}
	}

	// Scoped by metricRuleID: query gains "AND Id = ? LIMIT 1" with that id as the bound param.
	fakeScoped := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_anomaly_rules"):
			if !strings.Contains(q, "AND Id = ?") || len(params) != 1 || params[0] != "r2" {
				t.Fatalf("expected scoped query for r2, got q=%q params=%v", q, params)
			}
			return storetest.Result(metricCols, []any{"r2", "Mem Warn", "mem", "usage", "svc-b", "gt", 50.0, 0.0}), nil
		}
		return &store.Result{}, nil
	}}
	examined2, skipped2, candidates2 := (&server{db: fakeScoped}).notificationAutoCandidates("r2")
	if examined2 != 1 || skipped2 != 0 || len(candidates2) != 1 {
		t.Fatalf("scoped: want 1/0/1, got %d/%d/%d", examined2, skipped2, len(candidates2))
	}

	// Query error on the metric-rules query -> zeroes and an empty (non-nil) slice.
	fakeErr := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}
	examined3, skipped3, candidates3 := (&server{db: fakeErr}).notificationAutoCandidates("")
	if examined3 != 0 || skipped3 != 0 || len(candidates3) != 0 {
		t.Fatalf("query error: want 0/0/empty, got %d/%d/%v", examined3, skipped3, candidates3)
	}
}
