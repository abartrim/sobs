package main

import (
	"errors"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// loadObservedAiModels / loadSavedAiPricing / coerceAiPricingEntry / loadAiPricingWithSources
// build the /settings/ai pricing table: defaults, promoted to "inferred" for observed-but-
// unpriced models, then overridden by saved pricing (promoted to "confirmed" or "custom"). The
// empty-fixture corpus profile never has an observed span or a saved-pricing setting, so none of
// this merge logic is corpus-reachable. Oracle: app.py _load_observed_ai_models /
// _load_saved_ai_pricing / _coerce_ai_pricing_entry / _load_ai_pricing_with_sources.

func TestLoadObservedAiModels(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"model"},
			[]any{"GPT-4o"}, []any{"gpt-4o"}, []any{" claude-3-5-sonnet "}, // dedupe after normalize
		), nil
	}}}
	got := s.loadObservedAiModels(10)
	if len(got) != 2 || got[0] != "gpt-4o" || got[1] != "claude-3-5-sonnet" {
		t.Fatalf("unexpected models: %v", got)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadObservedAiModels(10); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}
}

func TestCoerceAiPricingEntry(t *testing.T) {
	valid := jsonenc.NewObject().Set("in", 1.5).Set("out", "2.5")
	if got := coerceAiPricingEntry(valid); got == nil {
		t.Fatal("valid entry should coerce")
	} else {
		if v, _ := got.Get("in"); v != 1.5 {
			t.Fatalf("in: got %v", v)
		}
		if v, _ := got.Get("out"); v != 2.5 {
			t.Fatalf("out: string-numeric should coerce, got %v", v)
		}
	}
	if coerceAiPricingEntry("not-an-object") != nil {
		t.Fatal("non-object should be nil")
	}
	if coerceAiPricingEntry(jsonenc.NewObject().Set("in", 1.0)) != nil {
		t.Fatal("missing out should be nil")
	}
	if coerceAiPricingEntry(jsonenc.NewObject().Set("in", "abc").Set("out", 1.0)) != nil {
		t.Fatal("non-numeric in should be nil")
	}
}

func TestLoadSavedAiPricing(t *testing.T) {
	s := &server{db: storetest.SettingsDB(nil)}
	if got := s.loadSavedAiPricing(); got.Len() != 0 {
		t.Fatalf("unset setting: want empty, got %v", got)
	}

	sBad := &server{db: storetest.SettingsDB(map[string]string{"ai.model_pricing": "{not json"})}
	if got := sBad.loadSavedAiPricing(); got.Len() != 0 {
		t.Fatalf("invalid json: want empty, got %v", got)
	}

	raw := `{"GPT-4o":{"in":1,"out":2},"bad-entry":{"in":1}}`
	sGood := &server{db: storetest.SettingsDB(map[string]string{"ai.model_pricing": raw})}
	got := sGood.loadSavedAiPricing()
	if got.Len() != 1 { // bad-entry (missing "out") is dropped
		t.Fatalf("want 1 entry, got %d: %v", got.Len(), got)
	}
	if _, ok := got.Get("gpt-4o"); !ok { // normalized key
		t.Fatalf("want normalized key 'gpt-4o', got %v", got.Keys())
	}
}

func TestLoadAiPricingWithSources(t *testing.T) {
	settings := map[string]string{
		"ai.model_pricing_confirmed": `["totally-new-model"]`,
		"ai.model_pricing":           `{"totally-new-model":{"in":1,"out":2},"another-custom-model":{"in":3,"out":4}}`,
	}
	fake := &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		if len(params) == 0 {
			return storetest.Result([]string{"model"}, []any{"totally-new-model"}), nil
		}
		return storetest.SettingsDB(settings).Execute("", params...)
	}}
	merged, sources := (&server{db: fake}).loadAiPricingWithSources()

	if v, _ := sources.Get("gpt-4o"); v != "default" {
		t.Fatalf("known default key should stay 'default', got %v", v)
	}
	if v, _ := sources.Get("totally-new-model"); v != "confirmed" {
		t.Fatalf("observed+saved+confirmed model should be 'confirmed', got %v", v)
	}
	m, _ := merged.Get("totally-new-model")
	mo := m.(*jsonenc.Object)
	if in, _ := mo.Get("in"); in != 1.0 {
		t.Fatalf("saved price should win over the inferred one: %v", in)
	}
	if v, _ := sources.Get("another-custom-model"); v != "custom" {
		t.Fatalf("saved-only (not observed, not default) model should be 'custom', got %v", v)
	}
}
