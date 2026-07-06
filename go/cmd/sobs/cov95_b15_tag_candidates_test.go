package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_tag_candidates_test.go — batch 15 coverage for cmd/sobs/tag_candidates.go:
//   autoTagSlug (23)                 71.4%
//   buildAutoTagRuleCandidates (58)  95.8%

func TestAutoTagSlug(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		fallback string
		want     string
	}{
		{"simple lowercase word", "Checkout", "svc", "checkout"},
		{"non-alnum runs collapse to underscore", "My Service!!v2", "svc", "my_service_v2"},
		{"leading/trailing separators trimmed", "--hello--", "svc", "hello"},
		{"empty after normalization uses fallback", "!!!", "fallback-value", "fallback-value"},
		{"empty input uses fallback", "", "fallback-value", "fallback-value"},
		{"whitespace-only input uses fallback", "   ", "fallback-value", "fallback-value"},
		{"long value truncated to 64 chars", strings.Repeat("a", 100), "svc", strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := autoTagSlug(c.value, c.fallback); got != c.want {
				t.Errorf("autoTagSlug(%q, %q) = %q, want %q", c.value, c.fallback, got, c.want)
			}
		})
	}
}

// tagCandidatesFakeDB answers each of buildAutoTagRuleCandidates's five telemetry queries
// (log/trace/error/ai/rum) with canned rows, keyed by a distinguishing substring, plus an
// existing-tag-rules lookup (loadTagRulesCtx -> sobs_tag_rules). existingRules, when non-nil, is
// returned verbatim as the sobs_tag_rules result (columns matching loadTagRulesCtx's SELECT).
func tagCandidatesFakeDB(t *testing.T, existingRules *store.Result) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_tag_rules"):
			if existingRules == nil {
				return &store.Result{}, nil
			}
			return existingRules, nil
		case strings.Contains(q, "FROM otel_logs") && strings.Contains(q, "GROUP BY ServiceName"):
			return storetest.Result([]string{"ServiceName", "c"},
				[]any{"checkout-prod", 50.0},
				[]any{"other-service", 10.0},
			), nil
		case strings.Contains(q, "FROM otel_traces") && strings.Contains(q, "GROUP BY ServiceName"):
			return storetest.Result([]string{"ServiceName", "c"}, []any{"checkout-staging", 20.0}), nil
		case strings.Contains(q, "coalesce(LogAttributes['exception.type']"):
			return storetest.Result([]string{"ExceptionType", "c"}, []any{"ValueError", 15.0}), nil
		case strings.Contains(q, "gen_ai.provider.name"):
			return storetest.Result([]string{"Provider", "c"}, []any{"openai", 30.0}), nil
		case strings.Contains(q, "FROM hyperdx_sessions"):
			return storetest.Result([]string{"EventName", "c"}, []any{"page_view", 25.0}), nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
}

func TestBuildAutoTagRuleCandidates_AllRecordTypes(t *testing.T) {
	s := &server{db: tagCandidatesFakeDB(t, nil)}
	candidates, counters := s.buildAutoTagRuleCandidates(24, 5, "", nil)

	// The log query returns 2 rows (checkout-prod, other-service); trace/error/ai/rum return 1
	// row each, for 6 candidates total. "examined" counts every row scanned across all 5 queries.
	if len(candidates) != 6 {
		t.Fatalf("want 6 candidates (2 log + 1 each of trace/error/ai/rum), got %d: %#v", len(candidates), candidates)
	}
	if counters["examined"] != 6 {
		t.Errorf("examined = %d, want 6", counters["examined"])
	}
	if counters["existing"] != 0 || counters["invalid"] != 0 {
		t.Errorf("existing/invalid = %d/%d, want 0/0", counters["existing"], counters["invalid"])
	}

	types := map[string]bool{}
	for _, c := range candidates {
		m := c.(map[string]any)
		rts, _ := m["record_types"].([]any)
		if len(rts) == 1 {
			types[rts[0].(string)] = true
		}
	}
	for _, want := range []string{"log", "trace", "error", "ai", "rum"} {
		if !types[want] {
			t.Errorf("missing candidate of record_type %q, got types=%v", want, types)
		}
	}

	// The log candidate for "checkout-prod" should infer env=production (inferEnvFromService).
	found := false
	for _, c := range candidates {
		m := c.(map[string]any)
		if m["tag_key"] == "env" && m["tag_value"] == "production" {
			found = true
		}
	}
	if !found {
		t.Error("expected an env=production candidate inferred from service 'checkout-prod'")
	}
}

func TestBuildAutoTagRuleCandidates_RecordTypeFilter(t *testing.T) {
	s := &server{db: tagCandidatesFakeDB(t, nil)}
	// Only "log" selected: none of the other four queries should even run (tagCandidatesFakeDB
	// would t.Fatalf on an unexpected query, so this also proves the other branches are skipped).
	candidates, counters := s.buildAutoTagRuleCandidates(24, 5, "", []string{"log"})
	if len(candidates) != 2 {
		t.Fatalf("want 2 log candidates (checkout-prod + other-service), got %d: %#v", len(candidates), candidates)
	}
	if counters["examined"] != 2 {
		t.Errorf("examined = %d, want 2", counters["examined"])
	}

	// An unrecognized record type falls back to "all" (the empty-selection default): 6 candidates,
	// same as TestBuildAutoTagRuleCandidates_AllRecordTypes.
	allCandidates, _ := s.buildAutoTagRuleCandidates(24, 5, "", []string{"bogus_type"})
	if len(allCandidates) != 6 {
		t.Errorf("unrecognized record type should default to all 5 record types (6 candidates), got %d", len(allCandidates))
	}
}

func TestBuildAutoTagRuleCandidates_SkipsExistingAndInvalid(t *testing.T) {
	// An existing rule matching the log/env=production candidate this fixture would otherwise
	// propose (service_name/contains/checkout-prod/env/production) must be skipped as "existing".
	// loadTagRulesCtx's columns: Id, Name, RecordTypes, MatchField, MatchOperator, MatchValue,
	// MatchAttrKey, TagKey, TagValue, ConditionsJson (empty ConditionsJson + non-blank MatchField
	// triggers the backward-compat single-condition fallback buildAutoTagRuleCandidates reads).
	existingRules := storetest.Result(
		[]string{"Id", "Name", "RecordTypes", "MatchField", "MatchOperator", "MatchValue",
			"MatchAttrKey", "TagKey", "TagValue", "ConditionsJson"},
		[]any{"rule-1", "Prod Env", "log", "service_name", "contains", "checkout-prod", "", "env", "production", ""},
	)
	s := &server{db: tagCandidatesFakeDB(t, existingRules)}
	candidates, counters := s.buildAutoTagRuleCandidates(24, 5, "", []string{"log"})

	if counters["existing"] != 1 {
		t.Errorf("existing = %d, want 1 (the pre-existing checkout-prod/env rule)", counters["existing"])
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 remaining log candidate (other-service), got %d: %#v", len(candidates), candidates)
	}
	m := candidates[0].(map[string]any)
	if m["tag_value"] != "other-service" {
		t.Errorf("remaining candidate = %#v, want the other-service one", m)
	}
}

func TestBuildAutoTagRuleCandidates_ServiceFilterScopesQueries(t *testing.T) {
	var seenWhere []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_tag_rules") {
			return &store.Result{}, nil
		}
		seenWhere = append(seenWhere, q)
		return &store.Result{}, nil
	}}}
	s.buildAutoTagRuleCandidates(6, 1, "checkout", []string{"log"})
	if len(seenWhere) != 1 || !strings.Contains(seenWhere[0], "ServiceName = 'checkout'") {
		t.Errorf("expected the log query scoped by ServiceName = 'checkout', got %v", seenWhere)
	}
}

func TestBuildAutoTagRuleCandidates_ErrorAndAIBlankSkip(t *testing.T) {
	// Blank ExceptionType / Provider values must be counted invalid and skipped, not turned into
	// a candidate with an empty match_value.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_tag_rules"):
			return &store.Result{}, nil
		case strings.Contains(q, "exception.type"):
			return storetest.Result([]string{"ExceptionType", "c"}, []any{"", 5.0}), nil
		case strings.Contains(q, "gen_ai.provider.name"):
			return storetest.Result([]string{"Provider", "c"}, []any{"", 5.0}), nil
		}
		return &store.Result{}, nil
	}}}
	candidates, counters := s.buildAutoTagRuleCandidates(24, 1, "", []string{"error", "ai"})
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates (blank values skipped), got %#v", candidates)
	}
	if counters["invalid"] != 2 {
		t.Errorf("invalid = %d, want 2", counters["invalid"])
	}
}
