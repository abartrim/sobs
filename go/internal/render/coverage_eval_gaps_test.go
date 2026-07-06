package render

// coverage_eval_gaps_test.go — table-driven tests for eval.go branches that were never
// exercised: the golden corpus only ever feeds evalExpr the fixed set of expressions the
// real templates contain, so operators/tests/filters that no template happens to use in a
// given shape (floor division, ordering comparisons, `is`/`in` tests, error propagation
// from a failing sub-expression, several filter edge cases) were never reached. These are
// all pure functions of the expression string + context — no mocking needed, just more
// input variety than the frozen corpus provides.

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

func evalOK(t *testing.T, eng *Engine, ctx *scope, expr string) any {
	t.Helper()
	got, err := eng.evalExpr(expr, ctx)
	if err != nil {
		t.Fatalf("evalExpr(%q) unexpected error: %v", expr, err)
	}
	return got
}

func evalErr(t *testing.T, eng *Engine, ctx *scope, expr string) error {
	t.Helper()
	got, err := eng.evalExpr(expr, ctx)
	if err == nil {
		t.Fatalf("evalExpr(%q) = %#v, want an error", expr, got)
	}
	return err
}

// TestTernary_NoElseFalse pins Jinja's inline-conditional with no `else`: a false
// condition evaluates to undefined -> empty string (eval.go:45).
func TestTernary_NoElseFalse(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	if got := evalOK(t, eng, ctx, "'a' if false"); got != "" {
		t.Errorf(`evalExpr("'a' if false") = %#v, want ""`, got)
	}
}

// TestTernary_CondError propagates a failing condition expression (eval.go:39).
func TestTernary_CondError(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "'a' if no_such_func() else 'b'")
}

// TestOr_ErrorPropagation / TestAnd_ErrorPropagation / TestNot_ErrorPropagation cover the
// error-propagation branches of evalOr/evalAnd/evalNot (eval.go:59,78,93) — a failing
// operand must surface, not be silently swallowed.
func TestOr_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() or true")
}

func TestAnd_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() and true")
}

func TestNot_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "not no_such_func()")
}

// TestMembership_NotIn covers the `not in` operator (eval.go:111), the case-safeString
// branch of membership (eval.go:234, via a |safe-produced value), and error propagation
// from both sides (eval.go:222,226).
func TestMembership_NotIn(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	if got := evalOK(t, eng, ctx, "'q' not in 'xyz'"); got != true {
		t.Errorf(`"'q' not in 'xyz'" = %#v, want true`, got)
	}
	if got := evalOK(t, eng, ctx, "'x' not in 'xyz'"); got != false {
		t.Errorf(`"'x' not in 'xyz'" = %#v, want false`, got)
	}
}

func TestMembership_SafeStringSubstring(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["html"] = "xabcz"
	if got := evalOK(t, eng, ctx, "'abc' in (html|safe)"); got != true {
		t.Errorf(`"'abc' in (html|safe)" = %#v, want true`, got)
	}
}

func TestMembership_ListMembership(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	if got := evalOK(t, eng, ctx, "5 in [1, 2, 5]"); got != true {
		t.Errorf(`"5 in [1, 2, 5]" = %#v, want true`, got)
	}
	if got := evalOK(t, eng, ctx, "9 in [1, 2, 5]"); got != false {
		t.Errorf(`"9 in [1, 2, 5]" = %#v, want false`, got)
	}
}

func TestMembership_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["x"] = 1
	evalErr(t, eng, ctx, "no_such_func() in x")
	evalErr(t, eng, ctx, "x in no_such_func()")
}

// TestCompareOrd covers every ordering operator (>=, <=, >, <) over both numeric operands
// and (via orderString, including its safeString branch) string operands, plus the
// non-comparable fallback to false (eval.go:147-174,182).
func TestCompareOrd_Numeric(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	cases := []struct {
		expr string
		want bool
	}{
		{"3 <= 4", true},
		{"4 <= 4", true},
		{"5 <= 4", false},
		{"4 >= 4", true},
		{"3 >= 4", false},
		{"5 > 4", true},
		{"4 > 4", false},
		{"3 < 4", true},
		{"4 < 4", false},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			if got := evalOK(t, eng, ctx, c.expr); got != c.want {
				t.Errorf("evalExpr(%q) = %#v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestCompareOrd_StringOrdering(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	if got := evalOK(t, eng, ctx, "'b' >= 'a'"); got != true {
		t.Errorf(`"'b' >= 'a'" = %#v, want true`, got)
	}
	if got := evalOK(t, eng, ctx, "'a' < 'b'"); got != true {
		t.Errorf(`"'a' < 'b'" = %#v, want true`, got)
	}
	// safeString on the left (produced by |safe) compared against a plain string literal —
	// exercises orderString's safeString case (eval.go:182).
	ctx.vars["html"] = "b"
	if got := evalOK(t, eng, ctx, "(html|safe) >= 'a'"); got != true {
		t.Errorf(`"(html|safe) >= 'a'" = %#v, want true`, got)
	}
}

func TestCompareOrd_NonComparableFallsToFalse(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	// nil is neither numeric nor string-like on either side, so compareOrd's final
	// fallback (eval.go:174) applies rather than a panic or type-assertion error.
	if got := evalOK(t, eng, ctx, "none <= 5"); got != false {
		t.Errorf(`"none <= 5" = %#v, want false`, got)
	}
}

func TestCompareOrd_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() <= 3")
	evalErr(t, eng, ctx, "3 <= no_such_func()")
}

// TestIsTest covers the Jinja `is`/`is not` test forms (eval.go:189-218): none, defined,
// string, mapping (both map[string]any and *jsonenc.Object), iterable/sequence, the
// unrecognized-test default (truthiness), and error propagation from the tested operand.
func TestIsTest(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["nilv"] = nil
	ctx.vars["strv"] = "hello"
	ctx.vars["intv"] = 5
	ctx.vars["mapv"] = map[string]any{"a": 1}
	obj := jsonenc.NewObject()
	obj.Set("a", 1)
	ctx.vars["objv"] = obj
	ctx.vars["listv"] = []any{1, 2}

	cases := []struct {
		expr string
		want bool
	}{
		{"nilv is none", true},
		{"intv is none", false},
		{"nilv is not none", false},
		{"intv is defined", true},
		{"nilv is defined", false},
		{"strv is string", true},
		{"intv is string", false},
		{"mapv is mapping", true},
		{"objv is mapping", true},
		{"strv is mapping", false},
		{"listv is iterable", true},
		{"strv is iterable", true},
		{"intv is iterable", false},
		{"listv is sequence", true},
		// unrecognized test name falls through to the default (truthiness) case.
		{"intv is odd", true},
		{"nilv is odd", false},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			if got := evalOK(t, eng, ctx, c.expr); got != c.want {
				t.Errorf("evalExpr(%q) = %#v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestIsTest_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() is none")
}

// TestCompareEq_ErrorPropagation covers eval.go:249,253.
func TestCompareEq_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() == 1")
	evalErr(t, eng, ctx, "1 == no_such_func()")
}

// TestEvalFiltered_ErrorPropagation covers the filter-application error branch
// (eval.go:271): an unsupported filter must surface as an error, not silently pass
// the value through.
func TestEvalFiltered_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	err := evalErr(t, eng, ctx, "5 | no_such_filter")
	if !strings.Contains(err.Error(), "unsupported filter") {
		t.Errorf("error = %q, want it to mention the unsupported filter", err.Error())
	}
}

// TestEvalAddSub_ErrorPropagation covers both the first-term and later-term error
// branches (eval.go:288,293).
func TestEvalAddSub_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() + 1") // first term fails
	evalErr(t, eng, ctx, "1 + no_such_func()") // later term fails
}

// TestEvalConcat covers the `~` string-concat operator, including pyStr() coercion of
// non-string operands and error propagation from a failing part (eval.go:316).
func TestEvalConcat(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["n"] = 7
	if got := evalOK(t, eng, ctx, "'g' ~ n ~ 'i'"); got != "g7i" {
		t.Errorf(`"'g' ~ n ~ 'i'" = %#v, want "g7i"`, got)
	}
}

func TestEvalConcat_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "'a' ~ no_such_func() ~ 'b'")
}

// TestFloorDiv covers Jinja's `//` operator end to end (eval.go:327-345): the golden
// corpus's known templates never use `//`, so none of this branch (including its error
// propagation and zero-guard) was previously exercised at all.
func TestFloorDiv(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	cases := []struct {
		expr string
		want int
	}{
		{"7 // 2", 3},
		{"6 // 3", 2},
		{"7 // 0", 0},   // zero guard
		{"-7 // 2", -4}, // Python floor division rounds toward -inf, not toward zero
		{"-6 // 3", -2}, // evenly divides -> no extra decrement
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			if got := evalOK(t, eng, ctx, c.expr); got != c.want {
				t.Errorf("evalExpr(%q) = %#v, want %d", c.expr, got, c.want)
			}
		})
	}
}

func TestFloorDiv_ErrorPropagation(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func() // 2")
	evalErr(t, eng, ctx, "2 // no_such_func()")
}

// TestModulo_ZeroGuard covers the `%` operator's zero-denominator guard (eval.go:378),
// separate from the `//` guard above.
func TestModulo_ZeroGuard(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	if got := evalOK(t, eng, ctx, "5 % 0"); got != 0 {
		t.Errorf("modulo by zero = %#v, want 0", got)
	}
}
