package render

// coverage_filters_gaps_test.go — fills in filter/method branches the golden corpus never
// reached because no real template happens to invoke them in a shape that hits these
// specific paths (selectattr/rejectattr, urlencode of an ordered object, the `default`
// filter's boolean flag, the `mask` filter, string/object method calls, ceil/floor
// rounding, and Python str.format() via a method call). All pure functions of
// expression+context, exercised through the same evalExpr/Render entry points the rest of
// the render package's tests already use.

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

func TestDefaultFilter_BooleanFlag(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["nilv"] = nil
	ctx.vars["emptyv"] = ""

	// No boolean flag: substitution only for undefined (nil), not merely falsy.
	if got := evalOK(t, eng, ctx, "emptyv|default('literal')"); got != "" {
		t.Errorf(`emptyv|default('literal') = %#v, want "" (empty string is defined)`, got)
	}
	if got := evalOK(t, eng, ctx, "nilv|default('literal')"); got != "literal" {
		t.Errorf(`nilv|default('literal') = %#v, want "literal"`, got)
	}
	// boolean=true: substitute on any falsy value, not just undefined.
	if got := evalOK(t, eng, ctx, "emptyv|default('empty', true)"); got != "empty" {
		t.Errorf(`emptyv|default('empty', true) = %#v, want "empty"`, got)
	}
}

func TestMinMaxFilter_EmptyList(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["empty"] = []any{}
	if got := evalOK(t, eng, ctx, "empty|min"); got != nil {
		t.Errorf("empty|min = %#v, want nil", got)
	}
	if got := evalOK(t, eng, ctx, "empty|max"); got != nil {
		t.Errorf("empty|max = %#v, want nil", got)
	}
}

func TestSelectRejectAttr(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	mk := func(name string, active bool) *jsonenc.Object {
		o := jsonenc.NewObject()
		o.Set("name", name)
		o.Set("active", active)
		return o
	}
	ctx.vars["items"] = []any{mk("a", true), mk("b", false), mk("c", true)}

	got := evalOK(t, eng, ctx, "(items|selectattr('active'))|length")
	if got != 2 {
		t.Errorf("selectattr('active')|length = %#v, want 2", got)
	}
	got = evalOK(t, eng, ctx, "(items|rejectattr('active'))|length")
	if got != 1 {
		t.Errorf("rejectattr('active')|length = %#v, want 1", got)
	}
	got = evalOK(t, eng, ctx, "(items|selectattr('name', 'equalto', 'a'))|length")
	if got != 1 {
		t.Errorf("selectattr('name','equalto','a')|length = %#v, want 1", got)
	}
	got = evalOK(t, eng, ctx, "(items|selectattr('name', 'ne', 'a'))|length")
	if got != 2 {
		t.Errorf("selectattr('name','ne','a')|length = %#v, want 2", got)
	}
}

func TestUrlencodeFilter_OrderedObject(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	o := jsonenc.NewObject()
	o.Set("q", "a b")
	o.Set("n", 5)
	ctx.vars["params"] = o
	got := evalOK(t, eng, ctx, "params|urlencode")
	if got != "q=a%20b&n=5" {
		t.Errorf("params|urlencode = %#v, want q=a%%20b&n=5", got)
	}
	// Plain-string fallback branch.
	got = evalOK(t, eng, ctx, "'a b'|urlencode")
	if got != "a%20b" {
		t.Errorf("'a b'|urlencode = %#v, want a%%20b", got)
	}
}

func TestMaskFilter(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["secret"] = "sensitive"
	// No mask func installed: identity.
	if got := evalOK(t, eng, ctx, "secret|mask"); got != "sensitive" {
		t.Errorf("secret|mask (no maskFunc) = %#v, want unchanged", got)
	}
	eng.SetMaskFunc(func(v any) any { return "***" })
	if got := evalOK(t, eng, ctx, "secret|mask"); got != "***" {
		t.Errorf("secret|mask (maskFunc set) = %#v, want ***", got)
	}
}

func TestStringMethodCalls(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["s"] = "Hello World"

	cases := []struct {
		expr string
		want any
	}{
		{"s.startswith('Hello')", true},
		{"s.startswith('bye')", false},
		{"s.endswith('World')", true},
		{"s.endswith('bye')", false},
		{"s.lower()", "hello world"},
		{"s.upper()", "HELLO WORLD"},
		{"s.replace('World', 'There')", "Hello There"},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			if got := evalOK(t, eng, ctx, c.expr); got != c.want {
				t.Errorf("evalExpr(%q) = %#v, want %#v", c.expr, got, c.want)
			}
		})
	}
	ctx.vars["padded"] = "  spaced  "
	if got := evalOK(t, eng, ctx, "padded.strip()"); got != "spaced" {
		t.Errorf("padded.strip() = %#v, want %q", got, "spaced")
	}
}

func TestStringFormatMethod(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["a"] = 1
	ctx.vars["b"] = 2
	if got := evalOK(t, eng, ctx, "'{}-{}'.format(a, b)"); got != "1-2" {
		t.Errorf(`'{}-{}'.format(a, b) = %#v, want "1-2"`, got)
	}
	ctx.vars["n"] = 1234567
	if got := evalOK(t, eng, ctx, "'{:,}'.format(n)"); got != "1,234,567" {
		t.Errorf(`'{:,}'.format(n) = %#v, want "1,234,567"`, got)
	}
	ctx.vars["f"] = 3.14159
	if got := evalOK(t, eng, ctx, "'{:.2f}'.format(f)"); got != "3.14" {
		t.Errorf(`'{:.2f}'.format(f) = %#v, want "3.14"`, got)
	}
}

func TestOrderedObjectMethods(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	o := jsonenc.NewObject()
	o.Set("a", 1)
	o.Set("b", 2)
	ctx.vars["o"] = o

	if got := evalOK(t, eng, ctx, "o.keys()|list|length"); got != 2 {
		t.Errorf("o.keys()|list|length = %#v, want 2", got)
	}
	if got := evalOK(t, eng, ctx, "o.values()|length"); got != 2 {
		t.Errorf("o.values()|length = %#v, want 2", got)
	}
	if got := evalOK(t, eng, ctx, "o.items()|length"); got != 2 {
		t.Errorf("o.items()|length = %#v, want 2", got)
	}
	if got := evalOK(t, eng, ctx, "o.get('a')"); got != 1 {
		t.Errorf("o.get('a') = %#v, want 1", got)
	}
	if got := evalOK(t, eng, ctx, "o.get('missing', 'fallback')"); got != "fallback" {
		t.Errorf("o.get('missing','fallback') = %#v, want fallback", got)
	}
}

func TestPlainMapMethods(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["m"] = map[string]any{"x": 1, "y": 2}
	if got := evalOK(t, eng, ctx, "m.keys()|length"); got != 2 {
		t.Errorf("m.keys()|length = %#v, want 2", got)
	}
	if got := evalOK(t, eng, ctx, "m.get('x')"); got != 1 {
		t.Errorf("m.get('x') = %#v, want 1", got)
	}
	if got := evalOK(t, eng, ctx, "m.get('z', 9)"); got != 9 {
		t.Errorf("m.get('z',9) = %#v, want 9", got)
	}
}

func TestCallMethod_UnsupportedError(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["n"] = 5
	evalErr(t, eng, ctx, "n.no_such_method()")
}

func TestJinjaRound_CeilFloorNegativePrecision(t *testing.T) {
	if got := jinjaRound(2.1, 0, "ceil"); got != 3 {
		t.Errorf(`jinjaRound(2.1, 0, "ceil") = %v, want 3`, got)
	}
	if got := jinjaRound(2.9, 0, "floor"); got != 2 {
		t.Errorf(`jinjaRound(2.9, 0, "floor") = %v, want 2`, got)
	}
	if got := jinjaRound(1234.0, -2, "common"); got != 1200 {
		t.Errorf(`jinjaRound(1234.0, -2, "common") = %v, want 1200`, got)
	}
}

func TestJinjaTruncate_WordBreak(t *testing.T) {
	// killwords=false (soft break at the last space before the cut point).
	got := jinjaTruncate("The quick brown fox jumps over", 15, false, "...")
	if got != "The quick..." {
		t.Errorf(`jinjaTruncate(..., 15, false, "...") = %q, want "The quick..."`, got)
	}
	// No space before the cut point: falls back to the hard cut (cut = length - len(end)).
	got = jinjaTruncate("Supercalifragilisticexpialidocious", 10, false, "...")
	if got != "Superca..." {
		t.Errorf(`jinjaTruncate long single word = %q, want "Superca..."`, got)
	}
}

func TestIntFloatFilter_Defaults(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["bad"] = "not-a-number"
	if got := evalOK(t, eng, ctx, "bad|int(9)"); got != 9 {
		t.Errorf("bad|int(9) = %#v, want 9", got)
	}
	if got := evalOK(t, eng, ctx, "bad|float(1.5)"); got != 1.5 {
		t.Errorf("bad|float(1.5) = %#v, want 1.5", got)
	}
}
