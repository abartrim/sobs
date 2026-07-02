package render

// eval_test.go — regression tests for the three PR #352 review findings
// (github.com/abartrim/sobs/pull/352#issuecomment-4866909562), each a genuine
// divergence from the frozen Python oracle's Jinja semantics, in the same bug
// class as the previously-fixed int-only `/` and membership-vs-substring `in`
// issues:
//
//  1. equalValues/atomKind treated int and float64 as different "kinds", so
//     equalValues(1.0, 1) was false. Python's `1.0 == 1` is True.
//  2. evalMulDiv's `*`, `/`, `%` branches discarded operand-evaluation errors
//     (`lv, _ := e.evalMulDiv(...)`), silently turning a real template error
//     into 0 instead of surfacing it like `//`, `+`, and `-` do.
//  3. lengthOf didn't handle *jsonenc.Object (the app's ordered-map type), so
//     {{ dict|length }} silently reported 0 instead of the key count.

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// TestEqualValues_NumericCrossType pins Python numeric equality: 1.0 == 1 is
// True regardless of int/float representation.
func TestEqualValues_NumericCrossType(t *testing.T) {
	cases := []struct {
		desc string
		a, b any
		want bool
	}{
		{"1.0 == 1", 1.0, 1, true},
		{"1 == 1.0", 1, 1.0, true},
		{"2.5 == 2 (false)", 2.5, 2, false},
		{"0.0 == 0", 0.0, 0, true},
		{"-3.0 == -3", -3.0, -3, true},
		{"3.0 == 3.0 (both float)", 3.0, 3.0, true},
		{"3 == 3 (both int)", 3, 3, true},
		// Non-numeric kinds must be unaffected by the numeric fast path.
		{"\"1\" == 1 (string vs int, still false)", "1", 1, false},
		{"nil == 0 (still false)", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := equalValues(c.a, c.b); got != c.want {
				t.Errorf("equalValues(%#v, %#v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestEqualValues_BoolNotConflatedWithNumeric documents that this fix only
// merges the int/float64 split — it does NOT extend Python's `True == 1`
// special-case to the Go port (bool keeps its own atomKind bucket, distinct
// from int/float64), since numFloat does not recognize bool.
func TestEqualValues_BoolNotConflatedWithNumeric(t *testing.T) {
	if got := equalValues(true, 1); got != false {
		t.Errorf("equalValues(true, 1) = %v, want false (bool kind unchanged by this fix)", got)
	}
	if got := equalValues(false, 0); got != false {
		t.Errorf("equalValues(false, 0) = %v, want false (bool kind unchanged by this fix)", got)
	}
	// bool-to-bool equality still works via the original atomKind+toString path.
	if got := equalValues(true, true); got != true {
		t.Errorf("equalValues(true, true) = %v, want true", got)
	}
}

// TestEvalMulDiv_PropagatesOperandErrors verifies that *, /, % surface a real
// evaluation error from an operand (e.g. an unknown function call) instead of
// silently discarding it and returning 0 — matching how // (floor div) and the
// +/- binary-op evaluator already propagate errors.
func TestEvalMulDiv_PropagatesOperandErrors(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)

	cases := []struct {
		op   string
		expr string
	}{
		{"*", "no_such_func() * 2"},
		{"*", "2 * no_such_func()"},
		{"/", "no_such_func() / 2"},
		{"/", "2 / no_such_func()"},
		{"%", "no_such_func() % 2"},
		{"%", "2 % no_such_func()"},
	}
	for _, c := range cases {
		t.Run(c.op+": "+c.expr, func(t *testing.T) {
			got, err := eng.evalMulDiv(c.expr, ctx)
			if err == nil {
				t.Fatalf("evalMulDiv(%q) = (%#v, nil), want a propagated error (got silently swallowed to 0)", c.expr, got)
			}
			if !strings.Contains(err.Error(), "no_such_func") {
				t.Errorf("evalMulDiv(%q) error = %q, want it to reference the failing operand", c.expr, err.Error())
			}
		})
	}
}

// TestEvalMulDiv_StillWorksForValidOperands is a sanity check that the error-
// propagation fix didn't break the ordinary arithmetic paths.
func TestEvalMulDiv_StillWorksForValidOperands(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)

	got, err := eng.evalMulDiv("3 * 4", ctx)
	if err != nil {
		t.Fatalf("evalMulDiv(3 * 4) unexpected error: %v", err)
	}
	if got != 12 {
		t.Errorf("evalMulDiv(3 * 4) = %#v, want 12", got)
	}

	got, err = eng.evalMulDiv("7 % 2", ctx)
	if err != nil {
		t.Fatalf("evalMulDiv(7 %% 2) unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("evalMulDiv(7 %% 2) = %#v, want 1", got)
	}

	got, err = eng.evalMulDiv("4 / 2", ctx)
	if err != nil {
		t.Fatalf("evalMulDiv(4 / 2) unexpected error: %v", err)
	}
	if got != 2.0 {
		t.Errorf("evalMulDiv(4 / 2) = %#v, want 2.0 (Python true division)", got)
	}
}

// TestLengthOf_JsonencObject pins {{ dict|length }} against the app's primary
// ordered-map type: it must return the key count, not silently fall through
// to the zero default.
func TestLengthOf_JsonencObject(t *testing.T) {
	empty := jsonenc.NewObject()
	if got := lengthOf(empty); got != 0 {
		t.Errorf("lengthOf(empty *jsonenc.Object) = %d, want 0", got)
	}

	obj := jsonenc.NewObject()
	obj.Set("signal", "cpu.util")
	obj.Set("tag", "prod")
	obj.Set("count", 3)
	if got := lengthOf(obj); got != 3 {
		t.Errorf("lengthOf(3-key *jsonenc.Object) = %d, want 3", got)
	}

	// Overwriting an existing key must not double-count it.
	obj.Set("count", 4)
	if got := lengthOf(obj); got != 3 {
		t.Errorf("lengthOf(*jsonenc.Object) after key overwrite = %d, want 3 (unchanged key count)", got)
	}
}

// TestLengthOf_EndToEnd renders {{ dict|length }} through the full engine to
// confirm the fix reaches the template-facing `length` filter/`|length`
// pipeline, not just the lengthOf helper in isolation.
func TestLengthOf_EndToEnd(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	obj := jsonenc.NewObject()
	obj.Set("a", 1)
	obj.Set("b", 2)
	ctx.vars["d"] = obj

	got, err := eng.evalExpr("d|length", ctx)
	if err != nil {
		t.Fatalf("evalExpr(d|length) unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("evalExpr(d|length) = %#v, want 2", got)
	}
}
