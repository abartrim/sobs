package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Last unit-testable pure helpers: HTML-unescaped JSON encode, HMAC asset signature, method guard
// miss, ask-gen LLM stats, chart custom-mapping inference.

func TestJSONDumpsNoEscBytes(t *testing.T) {
	// json.dumps with ensure_ascii but no HTML escaping: < > & stay literal.
	if got := string(jsonDumpsNoEscBytes("a<b>&c")); got != `"a<b>&c"` {
		t.Errorf("string: got %s", got)
	}
	if got := string(jsonDumpsNoEscBytes(map[string]any{"k": "<x>"})); got != `{"k":"<x>"}` {
		t.Errorf("map: got %s", got)
	}
}

func TestRumAssetSignature(t *testing.T) {
	secret, payload := "topsecret", "POST\n/v1/rum/assets\n123"
	got := rumAssetSignature(secret, payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	if want := hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("rumAssetSignature = %q, want %q", got, want)
	}
	if len(got) != 64 { // sha256 hex
		t.Errorf("sha256 hex should be 64 chars, got %d", len(got))
	}
}

func TestExactMethodGuardMiss(t *testing.T) {
	// A path not in the route-method table -> not guarded (returns false, no write).
	req := httptest.NewRequest("GET", "/definitely-not-a-registered-route", nil)
	w := httptest.NewRecorder()
	if exactMethodGuard(w, req, "/definitely-not-a-registered-route") {
		t.Error("unknown path should not be method-guarded")
	}
}

func TestQueryAskGenOnlyLLMStats(t *testing.T) {
	o := queryAskGenOnlyLLMStats(llmStats{prompt: 10, completion: 5, thinking: 2})
	for _, k := range []string{"totals", "sql_generation"} {
		v, ok := o.Get(k)
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		if _, ok := v.(*jsonenc.Object); !ok {
			t.Errorf("%s should be an object, got %T", k, v)
		}
	}
}

func TestInferCustomMappingFromOption(t *testing.T) {
	if got := inferCustomMappingFromOption("", nil); got != nil {
		t.Errorf("no placeholders -> %v, want nil", got)
	}
	// only reserved placeholders -> nothing inferred -> nil
	if got := inferCustomMappingFromOption(`{{rows}}`, nil); got != nil {
		t.Errorf("all-reserved -> %v, want nil", got)
	}
	// labels with columns -> {from:column, name:col0}
	m := inferCustomMappingFromOption(`{{labels}}`, []any{"svc", "cnt"})
	if m == nil {
		t.Fatal("labels should infer")
	}
	lv, _ := m.Get("labels")
	lo, ok := lv.(*jsonenc.Object)
	if !ok {
		t.Fatalf("labels not object: %T", lv)
	}
	if from, _ := lo.Get("from"); from != "column" {
		t.Errorf("labels.from = %v, want column", from)
	}
	if name, _ := lo.Get("name"); name != "svc" {
		t.Errorf("labels.name = %v, want svc (columns[0])", name)
	}
	// unknown placeholder -> default {from:rows}
	d := inferCustomMappingFromOption(`{{whatever}}`, nil)
	if d == nil {
		t.Fatal("default placeholder should infer")
	}
	wv, _ := d.Get("whatever")
	wo := wv.(*jsonenc.Object)
	if from, _ := wo.Get("from"); from != "rows" {
		t.Errorf("default from = %v, want rows", from)
	}
}
