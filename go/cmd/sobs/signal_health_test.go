package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// getSignalHealthByService drives the per-service anomaly-rollup used by the overview page's
// service-health widget. The byte-parity corpus only reaches its all-normal / empty-result
// branches; the severity aggregation and sort never see a genuine rank split there.
// Oracle: app.py _get_signal_health_by_service.
func TestGetSignalHealthByService(t *testing.T) {
	sigCols := []string{"ServiceName", "SignalSource", "SignalName", "AttrFingerprint", "value", "SampleCount"}
	ruleCols := []string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
		"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource", "SecondarySignalName",
		"SecondaryComparator", "SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount", "SeasonalBucketsJson"}

	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "v_derived_signals_anomaly"):
			if len(params) != 1 || params[0] != 6 {
				t.Fatalf("unexpected hours param: %v", params)
			}
			return storetest.Result(sigCols,
				[]any{"svc-a", "cpu", "usage", "", 95.0, 10.0}, // matches rule, value>=critical -> outlier
				[]any{"svc-a", "mem", "usage", "", 10.0, 10.0}, // different source, no match -> normal
				[]any{"svc-b", "cpu", "usage", "", 50.0, 10.0}, // matches rule, below warning -> normal
				[]any{"svc-c", "cpu", "usage", "", 85.0, 10.0}, // matches rule, warning<=value<critical -> warning
				[]any{"svc-d", "net", "usage", "", 1.0, 10.0},  // no match -> normal
				[]any{"svc-d", "gpu", "usage", "", 2.0, 10.0},  // no match -> normal (2nd normal signal, same service)
				[]any{"svc-d", "io", "usage", "", 3.0, 10.0},   // no match -> normal (3rd normal signal, same service)
			), nil
		case strings.Contains(q, "sobs_anomaly_rules"):
			return storetest.Result(ruleCols,
				[]any{"rule-1", "CPU High", "", "cpu", "usage", "", "", "gt", 80.0, 90.0, "", "", "", 0.0, 0.0, 1.0, ""},
			), nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}}

	got := s.getSignalHealthByService(6)
	if len(got) != 4 {
		t.Fatalf("want 4 services (one entry each, not one per signal), got %d: %v", len(got), got)
	}
	// Sorted by severity descending, then service name ascending within a tier:
	// svc-a (outlier), svc-c (warning), svc-b (normal), svc-d (normal).
	svc0 := got[0].(map[string]any)
	if svc0["service"] != "svc-a" || svc0["worst_state"] != "outlier" || svc0["signal_count"] != 2 {
		t.Fatalf("rank0 wrong: %v", svc0)
	}
	svc1 := got[1].(map[string]any)
	if svc1["service"] != "svc-c" || svc1["worst_state"] != "warning" || svc1["signal_count"] != 1 {
		t.Fatalf("rank1 wrong: %v", svc1)
	}
	svc2 := got[2].(map[string]any)
	if svc2["service"] != "svc-b" || svc2["worst_state"] != "normal" || svc2["signal_count"] != 1 {
		t.Fatalf("rank2 wrong: %v", svc2)
	}
	// Regression: a service whose signals are ALL "normal" (rank 0) must still
	// collapse to a single entry — previously the seen-check piggybacked on
	// serviceWorst[svc]'s key ever being inserted, which never happened when
	// rank stayed 0, so the service was appended to `order` once per signal.
	svc3 := got[3].(map[string]any)
	if svc3["service"] != "svc-d" || svc3["worst_state"] != "normal" || svc3["signal_count"] != 3 {
		t.Fatalf("rank3 wrong (duplicate-service regression): %v", svc3)
	}

	// Query error -> empty slice, not nil.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.getSignalHealthByService(6); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}

	// No rows -> empty slice.
	sEmpty := &server{db: &storetest.FakeDB{}}
	if got := sEmpty.getSignalHealthByService(6); len(got) != 0 {
		t.Fatalf("no rows: want empty, got %v", got)
	}
}
