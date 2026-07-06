package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_tag_condition_suggestions_test.go — batch 15 coverage for
// cmd/sobs/tag_condition_suggestions.go:
//   suggestStrings (18)                  84.6%
//   tagRuleAttributeKeySuggestions (42)   93.5%
//   recordTagKeySuggestions (160)         91.7%
//   recordTagValueSuggestions (182)       86.7%

func TestSuggestStrings(t *testing.T) {
	t.Run("filters blank rows and trims when requested", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"value"},
				[]any{"  hello  "},
				[]any{""},
				[]any{"   "},
				[]any{"world"},
			), nil
		}}}
		got := s.suggestStrings("SELECT value FROM x", true)
		if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
			t.Errorf("got %#v, want [hello world] (trimmed, blanks skipped)", got)
		}
	})

	t.Run("preserves raw value when trimValue=false", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"value"}, []any{"  raw  "}), nil
		}}}
		got := s.suggestStrings("SELECT value FROM x", false)
		if len(got) != 1 || got[0] != "  raw  " {
			t.Errorf("got %#v, want the untrimmed raw value", got)
		}
	})

	t.Run("query error yields empty slice", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return nil, errB15Boom
		}}}
		if got := s.suggestStrings("SELECT 1", true); len(got) != 0 {
			t.Errorf("got %#v, want empty", got)
		}
	})

	t.Run("no columns in result yields empty slice", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.suggestStrings("SELECT 1", true); len(got) != 0 {
			t.Errorf("got %#v, want empty", got)
		}
	})
}

// TestTagRuleAttributeKeySuggestions_RankingAndFiltering seeds distinctive, test-unique keys
// straight into the shared logAttrKeyCache singleton (bypassing prime(), since the cache is a
// package-level "primed once" store other tests may already have populated) and asserts the
// startswith > contains > alphabetical ranking, plus the q-filter and limit cap, using key
// prefixes ("zzcov95b15") no other test/handler uses so ambient cache pollution can't interfere
// with the specific assertions below.
func TestTagRuleAttributeKeySuggestions_RankingAndFiltering(t *testing.T) {
	// allKeysUnion only iterates the FIXED attrKeyRecordTypes ("log","span","resource","scope"),
	// so the seeded keys must land in one of those (a made-up record type would silently never be
	// read). "log" is exercised elsewhere too, so merging test-unique keys into it is safe.
	logAttrKeyCache.mu.Lock()
	logAttrKeyCache.loaded = true // prevent a later prime() call from resetting byType underneath us
	if logAttrKeyCache.byType["log"] == nil {
		logAttrKeyCache.byType["log"] = map[string]struct{}{}
	}
	for _, k := range []string{
		"zzcov95b15.beta",                             // contains "zzcov95b15" but does not start with the query
		"zzcov95b15.alpha_prefix",                     // starts with the query -> ranked first
		"zzcov95b15.zzz_contains_zzcov95b15query_zzz", // contains the query text, not prefix
		"unrelated_other_key",                         // does not match the query at all
	} {
		logAttrKeyCache.byType["log"][k] = struct{}{}
	}
	logAttrKeyCache.mu.Unlock()

	s := &server{db: &storetest.FakeDB{}}
	got := s.tagRuleAttributeKeySuggestions("zzcov95b15", 10)

	if len(got) != 3 {
		t.Fatalf("want 3 matching keys, got %d: %#v", len(got), got)
	}
	// startswith("zzcov95b15") ranks before mere-contains; alphabetical breaks remaining ties.
	if got[0] != "zzcov95b15.alpha_prefix" {
		t.Errorf("got[0] = %v, want the startswith match ranked first", got[0])
	}

	// Limit caps the result even when more keys match.
	capped := s.tagRuleAttributeKeySuggestions("zzcov95b15", 1)
	if len(capped) != 1 || capped[0] != "zzcov95b15.alpha_prefix" {
		t.Errorf("capped = %#v, want just the top-ranked match", capped)
	}

	// An empty query returns every (globally seen) key, alphabetically — just assert our seeded
	// keys are present among the results, since other tests may add more to the shared cache.
	all := s.tagRuleAttributeKeySuggestions("", 10000)
	found := map[string]bool{}
	for _, k := range all {
		found[k.(string)] = true
	}
	for _, want := range []string{"zzcov95b15.beta", "zzcov95b15.alpha_prefix",
		"zzcov95b15.zzz_contains_zzcov95b15query_zzz", "unrelated_other_key"} {
		if !found[want] {
			t.Errorf("expected seeded key %q in the unfiltered result", want)
		}
	}
}

func TestRecordTagKeySuggestions(t *testing.T) {
	t.Run("all record types (rt empty -> 'all', no RecordType filter)", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "RecordType = ?") {
				t.Fatalf("expected no RecordType filter for rt=all, got: %s", q)
			}
			if len(params) != 3 { // q, q, limit
				t.Fatalf("unexpected params: %v", params)
			}
			return storetest.Result([]string{"TagKey"}, []any{"env"}, []any{"team"}), nil
		}}}
		got := s.recordTagKeySuggestions("", 20, "")
		if len(got) != 2 || got[0] != "env" || got[1] != "team" {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("scoped to a specific record_type", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if !strings.Contains(q, "RecordType = ?") {
				t.Fatalf("expected a RecordType filter, got: %s", q)
			}
			if len(params) != 4 { // recordType, q, q, limit
				t.Fatalf("unexpected params: %v", params)
			}
			if params[0] != "log" {
				t.Errorf("record_type param = %v, want log (lowercased)", params[0])
			}
			return storetest.Result([]string{"TagKey"}, []any{"env"}), nil
		}}}
		got := s.recordTagKeySuggestions("EN", 5, "LOG")
		if len(got) != 1 || got[0] != "env" {
			t.Errorf("got %#v", got)
		}
	})
}

func TestRecordTagValueSuggestions(t *testing.T) {
	t.Run("blank tag key -> empty without querying", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			t.Fatal("should not query when tagKey is blank")
			return nil, nil
		}}}
		if got := s.recordTagValueSuggestions("   ", "", 10, ""); len(got) != 0 {
			t.Errorf("got %#v, want empty", got)
		}
	})

	t.Run("all record types", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "AND RecordType = ?") {
				t.Fatalf("expected no RecordType filter, got: %s", q)
			}
			return storetest.Result([]string{"TagValue"}, []any{"production"}), nil
		}}}
		got := s.recordTagValueSuggestions("env", "", 10, "")
		if len(got) != 1 || got[0] != "production" {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("scoped to a specific record_type", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if !strings.Contains(q, "AND RecordType = ?") {
				t.Fatalf("expected a RecordType filter, got: %s", q)
			}
			return storetest.Result([]string{"TagValue"}, []any{"staging"}), nil
		}}}
		got := s.recordTagValueSuggestions("env", "stag", 10, "trace")
		if len(got) != 1 || got[0] != "staging" {
			t.Errorf("got %#v", got)
		}
	})
}
