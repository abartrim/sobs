package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// buildAgentContextSummary assembles the plain-text snapshot fed to the guard/analysis LLM calls.
// The corpus's agent-flow profiles only exercise the bare "unknown rule, no extra" shape; the
// additional-context, frequency-noise, recent-error/anomaly listing, and trigger-details branches
// are corpus-unreachable. Oracle: app.py _build_agent_context_summary.

func emptyAncillaryDB() *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return &store.Result{}, nil // no rows for any of the three ancillary queries
	}}
}

func TestBuildAgentContextSummary_Minimal(t *testing.T) {
	s := &server{db: emptyAncillaryDB()}
	got := s.buildAgentContextSummary(jsonenc.NewObject())
	want := "=== SOBS Observability Context ===\nTriggered by: unknown rule ()"
	if got != want {
		t.Fatalf("minimal summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildAgentContextSummary_FullFeatured(t *testing.T) {
	tctx := jsonenc.NewObject().
		Set("rule_name", "High CPU").
		Set("trigger_state", "firing").
		Set("service", "svc-x").
		Set("extra", `{"additional_context":"  please look  ","err_type":"TimeoutError","foo":"bar","mask_output":true}`)

	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "countIf"):
			if len(params) != 2 || params[0] != "svc-x" || params[1] != "TimeoutError" {
				t.Fatalf("unexpected frequency params: %v", params)
			}
			return storetest.Result([]string{"c_1h", "c_24h"}, []any{0.0, 1.0}), nil // LOW
		case strings.Contains(q, "ExceptionType"):
			return storetest.Result([]string{"ServiceName", "ExceptionType", "c"}, []any{"svc-y", "BoomError", 3.0}), nil
		case strings.Contains(q, "v_derived_signals_anomaly"):
			return storetest.Result([]string{"ServiceName", "Signal", "anomaly_state"}, []any{"svc-z", "latency", "warning"}), nil
		}
		return &store.Result{}, nil
	}}
	got := (&server{db: fake}).buildAgentContextSummary(tctx)

	for _, want := range []string{
		"Triggered by: High CPU (firing)",
		"User-provided context: please look",
		"Event frequency (svc-x / TimeoutError):",
		"Last 1h:  0 occurrence(s)",
		"Last 24h: 1 occurrence(s)",
		"Noise indicator: LOW recurrence — may be an isolated event",
		"Recent errors (last 1h, all services):",
		"svc-y | BoomError x3",
		"Active anomalies:",
		"svc-z | latency → warning",
		"Trigger details: {'err_type': 'TimeoutError', 'foo': 'bar'}",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q; got:\n%s", want, got)
		}
	}
	// additional_context and mask_output must not leak into Trigger details.
	if strings.Contains(got, "'additional_context'") || strings.Contains(got, "'mask_output'") {
		t.Fatalf("Trigger details should exclude rendered-elsewhere keys; got:\n%s", got)
	}
}

func TestBuildAgentContextSummary_NoiseBranches(t *testing.T) {
	cases := []struct {
		name            string
		c1h, c24h       float64
		wantNoisePhrase string
	}{
		{"high_by_1h", 10, 1, "HIGH recurrence"},
		{"high_by_24h", 0, 50, "HIGH recurrence"},
		{"moderate", 5, 10, "MODERATE recurrence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tctx := jsonenc.NewObject().Set("service", "svc-x").Set("extra", `{"err_type":"E"}`)
			fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
				if strings.Contains(q, "countIf") {
					return storetest.Result([]string{"c_1h", "c_24h"}, []any{tc.c1h, tc.c24h}), nil
				}
				return &store.Result{}, nil
			}}
			got := (&server{db: fake}).buildAgentContextSummary(tctx)
			if !strings.Contains(got, tc.wantNoisePhrase) {
				t.Fatalf("%s: want phrase %q in:\n%s", tc.name, tc.wantNoisePhrase, got)
			}
		})
	}
}

func TestBuildAgentContextSummary_RawExtraStringPassthrough(t *testing.T) {
	// extra is set but does not parse to a JSON object -> the raw string is rendered verbatim.
	tctx := jsonenc.NewObject().Set("extra", "not json {{{")
	got := (&server{db: emptyAncillaryDB()}).buildAgentContextSummary(tctx)
	if !strings.Contains(got, "\nAdditional context: not json {{{") {
		t.Fatalf("want raw extra passthrough, got:\n%s", got)
	}
}

func TestBuildAgentContextSummary_ServiceOnlyNoErrType_SkipsFrequency(t *testing.T) {
	// service without err_type (or vice versa) must not fire the frequency block.
	tctx := jsonenc.NewObject().Set("service", "svc-x")
	got := (&server{db: emptyAncillaryDB()}).buildAgentContextSummary(tctx)
	if strings.Contains(got, "Event frequency") {
		t.Fatalf("frequency block should be skipped without err_type, got:\n%s", got)
	}
}
