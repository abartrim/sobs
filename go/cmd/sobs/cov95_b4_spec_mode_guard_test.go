package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// This file covers specModeGuard (cmd/sobs/handlers_mutations2.go), which was untested by the
// sibling cov95_b4_handlers_mutations2_test.go file: the guard's reject branch (with and without
// its optional "extra" callback) and its pass-through branch.

func TestSpecModeGuardRejectsMissingMode(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/whatever", strings.NewReader(`{"spec":{}}`))
	rejected := specModeGuard(w, r, nil)
	if !rejected {
		t.Fatal("want true (rejected) when sql.mode is absent")
	}
	if w.Code != 400 {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "sql.mode must be 'builder' or 'raw'") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestSpecModeGuardRejectsUnknownModeWithExtra(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/whatever", strings.NewReader(
		`{"spec":{"sql":{"mode":"bogus"}}}`))
	var extraCalled bool
	rejected := specModeGuard(w, r, func(o *jsonenc.Object) {
		extraCalled = true
		o.Set("extra_field", "value")
	})
	if !rejected {
		t.Fatal("want true (rejected) for an unknown mode")
	}
	if !extraCalled {
		t.Fatal("expected the extra callback to run on the reject path")
	}
	if !strings.Contains(w.Body.String(), `"extra_field":"value"`) {
		t.Fatalf("expected extra field merged into the error body: %s", w.Body.String())
	}
}

func TestSpecModeGuardPassesBuilderAndRawModes(t *testing.T) {
	for _, mode := range []string{"builder", "raw"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/whatever", strings.NewReader(
			`{"spec":{"sql":{"mode":"`+mode+`"}}}`))
		rejected := specModeGuard(w, r, nil)
		if rejected {
			t.Fatalf("mode %q: want false (accepted), got rejected with body %s", mode, w.Body.String())
		}
		if w.Code != 0 && w.Body.Len() != 0 {
			t.Fatalf("mode %q: guard must not write a response on the accept path, got %d %s",
				mode, w.Code, w.Body.String())
		}
	}
}
