package render

// cov95_b2_eval_test.go — batch 2 (coverage/raise-toward-95): unit tests for
// internal/render/eval.go functions/branches left uncovered by unit-only runs (the golden
// corpus's fixed set of real-template expressions never happens to reach these): subscript
// indexing (base[idx] on maps/objects/lists, including matchingSubscript's list-literal
// disambiguation), slicing (sliceBound/sliceValue/normSlice, all 0% unit-only), invokeMacro
// (macro param binding: positional, keyword, default, missing), pyRepr/pyStr for lists and
// dicts, tojson/toJSONValue, topLevelAssign, toIntVal's string/float branches, and the
// numeric-arithmetic helpers' non-numeric fallback branches.
//
// Checked coverage_eval_gaps_test.go / coverage_divvalues_test.go / coverage_filters_gaps_test.go
// / eval_test.go first for overlap: none of them touch subscript/slicing/invokeMacro/
// tojson/pyRepr/topLevelAssign, so this file is new territory. A couple of tests below
// slightly extend addValues/mulValues/toIntVal/pyStr coverage that TestDivValues_* and
// TestEvalMulDiv_* did not reach (those focus on divValues and error propagation, not the
// string-coercion fallback paths).

import (
	"math"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---- subscript / matchingSubscript ----------------------------------------------------

// TestSubscript_Map covers subscript's map[string]any branch (eval.go:1024-1025).
func TestSubscript_Map(t *testing.T) {
	m := map[string]any{"a": 1, "b": 2}
	if got := subscript(m, "a"); got != 1 {
		t.Errorf(`subscript(map, "a") = %#v, want 1`, got)
	}
	if got := subscript(m, "missing"); got != nil {
		t.Errorf(`subscript(map, "missing") = %#v, want nil`, got)
	}
}

// TestSubscript_JsonencObject covers subscript's *jsonenc.Object branch (eval.go:1021-1023).
func TestSubscript_JsonencObject(t *testing.T) {
	obj := jsonenc.NewObject().Set("k", "v")
	if got := subscript(obj, "k"); got != "v" {
		t.Errorf(`subscript(obj, "k") = %#v, want "v"`, got)
	}
}

// TestSubscript_List covers subscript's []any branch, including the int-index fast path,
// the string-index fallback (idx isn't already an int), and out-of-range/negative bounds
// (eval.go:1026-1035).
func TestSubscript_List(t *testing.T) {
	list := []any{"x", "y", "z"}
	if got := subscript(list, 1); got != "y" {
		t.Errorf(`subscript(list, 1) = %#v, want "y"`, got)
	}
	// idx as a non-int that parses as an integer string.
	if got := subscript(list, "2"); got != "z" {
		t.Errorf(`subscript(list, "2") = %#v, want "z"`, got)
	}
	// out of range.
	if got := subscript(list, 10); got != nil {
		t.Errorf(`subscript(list, 10) = %#v, want nil`, got)
	}
	if got := subscript(list, -1); got != nil {
		t.Errorf(`subscript(list, -1) = %#v, want nil (no negative-index support)`, got)
	}
	// idx not parseable as an integer at all.
	if got := subscript(list, "not-a-number"); got != nil {
		t.Errorf(`subscript(list, "not-a-number") = %#v, want nil`, got)
	}
}

// TestSubscript_UnsupportedBaseType covers subscript's final fallthrough (eval.go:1037):
// any other base type returns nil rather than panicking.
func TestSubscript_UnsupportedBaseType(t *testing.T) {
	if got := subscript(42, "x"); got != nil {
		t.Errorf("subscript(42, x) = %#v, want nil", got)
	}
	if got := subscript(nil, "x"); got != nil {
		t.Errorf("subscript(nil, x) = %#v, want nil", got)
	}
}

// TestMatchingSubscript covers the '[' / ']' depth-matching walk directly (eval.go:1002-1016).
func TestMatchingSubscript(t *testing.T) {
	if got := matchingSubscript("items[0]"); got != 5 {
		t.Errorf(`matchingSubscript("items[0]") = %d, want 5`, got)
	}
	if got := matchingSubscript("m['a']['b']"); got != 6 {
		t.Errorf(`matchingSubscript("m['a']['b']") = %d, want 6`, got)
	}
	// A plain list literal starts at index 0, so callers treat that specially — but
	// matchingSubscript itself just reports where the matching '[' is.
	if got := matchingSubscript("[1, 2, 3]"); got != 0 {
		t.Errorf(`matchingSubscript("[1, 2, 3]") = %d, want 0`, got)
	}
}

// TestEvalAtom_Subscript_EndToEnd drives base[index] and slicing through the real
// evalExpr entry point (evalAtom's subscript branch, eval.go:669-688).
func TestEvalAtom_Subscript_EndToEnd(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["items"] = []any{"a", "b", "c", "d"}
	ctx.vars["m"] = map[string]any{"k": "v"}

	if got := evalOK(t, eng, ctx, "items[1]"); got != "b" {
		t.Errorf(`evalExpr("items[1]") = %#v, want "b"`, got)
	}
	if got := evalOK(t, eng, ctx, "m['k']"); got != "v" {
		t.Errorf(`evalExpr("m['k']") = %#v, want "v"`, got)
	}
	if got := evalOK(t, eng, ctx, "items[1:3]"); !equalAnySlices(got, []any{"b", "c"}) {
		t.Errorf(`evalExpr("items[1:3]") = %#v, want [b c]`, got)
	}
	if got := evalOK(t, eng, ctx, "items[:2]"); !equalAnySlices(got, []any{"a", "b"}) {
		t.Errorf(`evalExpr("items[:2]") = %#v, want [a b]`, got)
	}
	if got := evalOK(t, eng, ctx, "items[2:]"); !equalAnySlices(got, []any{"c", "d"}) {
		t.Errorf(`evalExpr("items[2:]") = %#v, want [c d]`, got)
	}
}

func TestEvalAtom_Subscript_BaseExprErrorPropagates(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func()[0]")
}

func TestEvalAtom_Subscript_IndexExprErrorPropagates(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["items"] = []any{"a"}
	evalErr(t, eng, ctx, "items[no_such_func()]")
}

func equalAnySlices(got any, want []any) bool {
	gl, ok := got.([]any)
	if !ok || len(gl) != len(want) {
		return false
	}
	for i := range want {
		if gl[i] != want[i] {
			return false
		}
	}
	return true
}

// ---- slicing: sliceBound / sliceValue / normSlice --------------------------------------

// TestSliceBound covers sliceBound directly (eval.go:1822-1831, 0% unit-only): the
// empty-expr no-bound case, a successful eval, and the swallowed-error case (a failing
// bound expression is treated as "no bound" rather than propagating — matches the omitted-
// bound behavior for the unreachable case of a bad slice expression).
func TestSliceBound(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["n"] = 3

	if v, ok := eng.sliceBound("", ctx); ok || v != 0 {
		t.Errorf(`sliceBound("", ctx) = (%d, %v), want (0, false)`, v, ok)
	}
	if v, ok := eng.sliceBound("n", ctx); !ok || v != 3 {
		t.Errorf(`sliceBound("n", ctx) = (%d, %v), want (3, true)`, v, ok)
	}
	if v, ok := eng.sliceBound("no_such_func()", ctx); ok || v != 0 {
		t.Errorf(`sliceBound("no_such_func()", ctx) = (%d, %v), want (0, false) (error -> no bound)`, v, ok)
	}
}

// TestSliceValue_String covers sliceValue's string branch, rune-based (not byte-based, so
// multi-byte UTF-8 slices correctly) (eval.go:1836-1839).
func TestSliceValue_String(t *testing.T) {
	if got := sliceValue("hello world", 0, true, 5, true); got != "hello" {
		t.Errorf(`sliceValue("hello world", 0, 5) = %#v, want "hello"`, got)
	}
	// multi-byte runes: café (é is 2 bytes in UTF-8, 1 rune).
	if got := sliceValue("café", 0, true, 4, true); got != "café" {
		t.Errorf(`sliceValue("café", 0, 4) = %#v, want "café" (rune-based slicing)`, got)
	}
}

// TestSliceValue_SafeString covers sliceValue's safeString branch (eval.go:1840-1843).
func TestSliceValue_SafeString(t *testing.T) {
	got := sliceValue(safeString{"<b>hi</b>"}, 0, true, 3, true)
	ss, ok := got.(safeString)
	if !ok || ss.s != "<b>" {
		t.Errorf(`sliceValue(safeString{"<b>hi</b>"}, 0, 3) = %#v, want safeString{"<b>"}`, got)
	}
}

// TestSliceValue_List covers sliceValue's []any branch (eval.go:1844-1846).
func TestSliceValue_List(t *testing.T) {
	list := []any{1, 2, 3, 4, 5}
	got := sliceValue(list, 1, true, 4, true)
	if !equalAnySlices(got, []any{2, 3, 4}) {
		t.Errorf("sliceValue(list, 1, 4) = %#v, want [2 3 4]", got)
	}
}

// TestSliceValue_UnsupportedType covers sliceValue's final fallback (eval.go:1848): any
// other base type returns "" rather than panicking.
func TestSliceValue_UnsupportedType(t *testing.T) {
	if got := sliceValue(42, 0, true, 1, true); got != "" {
		t.Errorf("sliceValue(42, ...) = %#v, want empty string", got)
	}
	if got := sliceValue(nil, 0, false, 0, false); got != "" {
		t.Errorf("sliceValue(nil, ...) = %#v, want empty string", got)
	}
}

// TestNormSlice covers normSlice's bound-normalization rules directly (eval.go:1851-1878,
// 0% unit-only): negative indices count from the end, out-of-range clamps, and end<start
// collapses to an empty (zero-length) slice rather than a negative range.
func TestNormSlice(t *testing.T) {
	cases := []struct {
		desc             string
		n, start, end    int
		hasStart, hasEnd bool
		wantS, wantE     int
	}{
		{"no bounds -> full range", 5, 0, 0, false, false, 0, 5},
		{"explicit positive bounds", 10, 2, 7, true, true, 2, 7},
		{"negative start counts from end", 10, -3, 10, true, true, 7, 10},
		{"negative end counts from end", 10, 0, -2, true, true, 0, 8},
		{"start clamps to n when too large", 5, 100, 5, true, false, 5, 5},
		{"end clamps to n when too large", 5, 0, 100, true, true, 0, 5},
		{"start clamps to 0 when very negative", 5, -100, 5, true, false, 0, 5},
		{"end<start collapses to empty at start", 10, 6, 2, true, true, 6, 6},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			s, e := normSlice(c.n, c.start, c.hasStart, c.end, c.hasEnd)
			if s != c.wantS || e != c.wantE {
				t.Errorf("normSlice(%d,%d,%v,%d,%v) = (%d,%d), want (%d,%d)",
					c.n, c.start, c.hasStart, c.end, c.hasEnd, s, e, c.wantS, c.wantE)
			}
		})
	}
}

// ---- invokeMacro ------------------------------------------------------------------------

// TestInvokeMacro_PositionalKeywordDefault covers invokeMacro's three binding branches
// (eval.go:971-997, 0% unit-only): positional args, keyword args, and param defaults
// (evaluated in the CALLER's scope), plus the missing-value-with-no-default fallback ("").
func TestInvokeMacro_PositionalKeywordDefault(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		`{% macro greet(name, greeting="Hi", suffix) %}{{ greeting }}, {{ name }}{{ suffix }}{% endmacro %}`)
	writeTemplateB2(t, dir, "t.html",
		`{% from "macros.html" import greet %}{{ greet("Ann") }}|{{ greet("Bo", greeting="Yo") }}`)
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "Hi, Ann|Yo, Bo" {
		t.Errorf(`Render = %q, want %q`, out, "Hi, Ann|Yo, Bo")
	}
}

// TestInvokeMacro_DefaultEvaluatedInCallerScope covers evalExpr(p.def, ctx) using the
// CALL-SITE scope (eval.go:980), a real Jinja quirk: a macro param default expression sees
// the caller's variables, not the macro's own (empty) scope.
func TestInvokeMacro_DefaultEvaluatedInCallerScope(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		`{% macro show(x=fallback) %}{{ x }}{% endmacro %}`)
	writeTemplateB2(t, dir, "t.html",
		`{% from "macros.html" import show %}{{ show() }}`)
	eng := New(dir)
	out, err := eng.Render("t.html", map[string]any{"fallback": "caller-value"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "caller-value" {
		t.Errorf(`Render = %q, want %q`, out, "caller-value")
	}
}

// TestInvokeMacro_DefaultErrorPropagates covers invokeMacro's default-eval error branch
// (eval.go:981-983).
func TestInvokeMacro_DefaultErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		`{% macro show(x=no_such_func()) %}{{ x }}{% endmacro %}`)
	writeTemplateB2(t, dir, "t.html",
		`{% from "macros.html" import show %}{{ show() }}`)
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring macro default = nil error, want an error")
	}
}

// TestInvokeMacro_MissingRequiredParam covers invokeMacro's final fallback ("" for a param
// with neither a positional/keyword value nor a default, eval.go:985-987).
func TestInvokeMacro_MissingRequiredParam(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		`{% macro show(x) %}[{{ x }}]{% endmacro %}`)
	writeTemplateB2(t, dir, "t.html",
		`{% from "macros.html" import show %}{{ show() }}`)
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "[]" {
		t.Errorf(`Render = %q, want %q (missing required param -> "")`, out, "[]")
	}
}

// TestInvokeMacro_BodyErrorPropagates covers invokeMacro's render-error branch (eval.go:994-996).
func TestInvokeMacro_BodyErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		`{% macro bad() %}{{ no_such_func() }}{% endmacro %}`)
	writeTemplateB2(t, dir, "t.html",
		`{% from "macros.html" import bad %}{{ bad() }}`)
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring macro body = nil error, want an error")
	}
}

// ---- pyRepr / pyStr for containers -------------------------------------------------------

// TestPyRepr covers pyRepr directly (eval.go:461-466, 0% unit-only): strings get quotes,
// everything else falls through to pyStr.
func TestPyRepr(t *testing.T) {
	if got := pyRepr("hi"); got != "'hi'" {
		t.Errorf(`pyRepr("hi") = %q, want "'hi'"`, got)
	}
	if got := pyRepr(5); got != "5" {
		t.Errorf(`pyRepr(5) = %q, want "5"`, got)
	}
	if got := pyRepr(true); got != "True" {
		t.Errorf(`pyRepr(true) = %q, want "True"`, got)
	}
	if got := pyRepr(nil); got != "None" {
		t.Errorf(`pyRepr(nil) = %q, want "None"`, got)
	}
}

// TestPyStr_ListAndDict covers pyStr's []any and *jsonenc.Object branches (eval.go:442-454),
// previously only reached at 18.8% (i.e. essentially just the default case).
func TestPyStr_ListAndDict(t *testing.T) {
	if got := pyStr([]any{"a", 1, true, nil}); got != `['a', 1, True, None]` {
		t.Errorf(`pyStr([a,1,true,nil]) = %q, want %q`, got, `['a', 1, True, None]`)
	}
	obj := jsonenc.NewObject().Set("k", "v").Set("n", 2)
	if got := pyStr(obj); got != `{'k': 'v', 'n': 2}` {
		t.Errorf(`pyStr(obj) = %q, want %q`, got, `{'k': 'v', 'n': 2}`)
	}
	// nil / bool / plain string coverage (cheap extra branches).
	if got := pyStr(nil); got != "None" {
		t.Errorf(`pyStr(nil) = %q, want "None"`, got)
	}
	if got := pyStr(false); got != "False" {
		t.Errorf(`pyStr(false) = %q, want "False"`, got)
	}
	if got := pyStr("plain"); got != "plain" {
		t.Errorf(`pyStr("plain") = %q, want "plain"`, got)
	}
}

// TestToString_ListAndDict covers toString's []any / *jsonenc.Object delegation to pyStr
// (eval.go:1285-1288), reached only through {{ list }} / {{ dict }} output — the corpus's
// known templates apparently never output a bare list/dict directly.
func TestToString_ListAndDict(t *testing.T) {
	if got := toString([]any{"ERROR"}); got != `['ERROR']` {
		t.Errorf(`toString([ERROR]) = %q, want %q`, got, `['ERROR']`)
	}
	obj := jsonenc.NewObject().Set("a", 1)
	if got := toString(obj); got != `{'a': 1}` {
		t.Errorf(`toString(obj) = %q, want %q`, got, `{'a': 1}`)
	}
}

// ---- tojson / toJSONValue ----------------------------------------------------------------

// TestTojson_HTMLEscaping covers tojson end to end (eval.go:1310-1320, 0% unit-only): plain
// JSON encoding plus the HTML-unsafe-character escaping Jinja's htmlsafe_json applies.
func TestTojson_HTMLEscaping(t *testing.T) {
	got := tojson("<script>alert('x')&y</script>")
	want := `"\u003cscript\u003ealert(\u0027x\u0027)\u0026y\u003c/script\u003e"`
	if got != want {
		t.Errorf("tojson(html-ish string) = %q, want %q", got, want)
	}
}

// TestTojson_ViaFilter_EndToEnd drives |tojson through evalExpr (applyFilter's tojson case),
// confirming the result is marked safe (not re-escaped by renderOutput).
func TestTojson_ViaFilter_EndToEnd(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	ctx.vars["v"] = map[string]any{"a": 1}
	got := evalOK(t, eng, ctx, "v|tojson")
	ss, ok := got.(safeString)
	if !ok {
		t.Fatalf("v|tojson = %#v (%T), want safeString", got, got)
	}
	if ss.s != `{"a": 1}` {
		t.Errorf("v|tojson = %q, want %q", ss.s, `{"a": 1}`)
	}
}

// TestToJSONValue covers toJSONValue's branches directly (eval.go:1324-1343, 0% unit-only):
// safeString unwraps to its plain string, map[string]any becomes an ordered Object, []any
// converts element-wise (recursively), and anything else passes through unchanged.
func TestToJSONValue(t *testing.T) {
	if got := toJSONValue(safeString{"raw"}); got != "raw" {
		t.Errorf(`toJSONValue(safeString{"raw"}) = %#v, want "raw"`, got)
	}
	if got := toJSONValue(42); got != 42 {
		t.Errorf(`toJSONValue(42) = %#v, want 42`, got)
	}
	obj, ok := toJSONValue(map[string]any{"a": 1}).(*jsonenc.Object)
	if !ok {
		t.Fatalf("toJSONValue(map) = %#v, want *jsonenc.Object", toJSONValue(map[string]any{"a": 1}))
	}
	if v, _ := obj.Get("a"); v != 1 {
		t.Errorf(`toJSONValue(map)["a"] = %#v, want 1`, v)
	}
	// []any recursion: nested map inside a list becomes a nested Object.
	out, ok := toJSONValue([]any{map[string]any{"x": safeString{"y"}}}).([]any)
	if !ok || len(out) != 1 {
		t.Fatalf("toJSONValue([map]) = %#v, want a 1-elem []any", out)
	}
	inner, ok := out[0].(*jsonenc.Object)
	if !ok {
		t.Fatalf("toJSONValue([map])[0] = %#v, want *jsonenc.Object", out[0])
	}
	if v, _ := inner.Get("x"); v != "y" {
		t.Errorf(`nested toJSONValue unwrap = %#v, want "y"`, v)
	}
}

// ---- topLevelAssign -----------------------------------------------------------------------

// TestTopLevelAssign covers topLevelAssign directly (eval.go:1482-1506, 0% unit-only): a
// bare kwarg `=`, and the various NOT-a-kwarg-assign operators it must skip (==, !=, <=, >=)
// plus depth-tracking through parens/brackets.
func TestTopLevelAssign(t *testing.T) {
	cases := []struct {
		expr string
		want int
	}{
		{"x=1", 1},
		{"x = 1", 2},
		{"x==1", -1},
		{"x!=1", -1},
		{"x<=1", -1},
		{"x>=1", -1},
		{"noassign", -1},
		{"f(a=1)", -1}, // the '=' is inside parens (depth>0), so the top-level scan finds none
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			if got := topLevelAssign(c.expr); got != c.want {
				t.Errorf("topLevelAssign(%q) = %d, want %d", c.expr, got, c.want)
			}
		})
	}
}

// TestCallFunc_KeywordArgs_EndToEnd drives callFunc's keyword-arg parsing (eval.go:932-966)
// through a registered Func, covering topLevelAssign's use inside callFunc for real (not
// just via url_for elsewhere in the suite).
func TestCallFunc_KeywordArgs_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", `{{ f(1, 2, x=3, y=4) }}`)
	eng := New(dir)
	var gotPos []any
	var gotKW map[string]any
	eng.AddFunc("f", func(pos []any, kw map[string]any) (any, error) {
		gotPos, gotKW = pos, kw
		return "done", nil
	})
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "done" {
		t.Errorf("Render = %q, want %q", out, "done")
	}
	if len(gotPos) != 2 || gotPos[0] != 1 || gotPos[1] != 2 {
		t.Errorf("positional args = %#v, want [1 2]", gotPos)
	}
	if gotKW["x"] != 3 || gotKW["y"] != 4 {
		t.Errorf("keyword args = %#v, want {x:3 y:4}", gotKW)
	}
}

func TestCallFunc_UnknownFunctionErrors(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	evalErr(t, eng, ctx, "no_such_func_at_all()")
}

func TestCallFunc_PositionalArgErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", `{{ f(no_such_func()) }}`)
	eng := New(dir)
	eng.AddFunc("f", func(pos []any, kw map[string]any) (any, error) { return "x", nil })
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring positional call arg = nil error, want an error")
	}
}

func TestCallFunc_KeywordArgErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", `{{ f(x=no_such_func()) }}`)
	eng := New(dir)
	eng.AddFunc("f", func(pos []any, kw map[string]any) (any, error) { return "x", nil })
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring keyword call arg = nil error, want an error")
	}
}

// ---- toIntVal / arithmetic helper fallback branches --------------------------------------

// TestToIntVal covers all of toIntVal's type branches (eval.go:474-485, 33.3% unit-only):
// int passthrough, float64 truncation, string parsing (with whitespace trimming), a
// non-numeric string (silently 0, matching strconv.Atoi's error-swallowed default), and the
// final fallback for any other type.
func TestToIntVal(t *testing.T) {
	if got := toIntVal(5); got != 5 {
		t.Errorf("toIntVal(5) = %d, want 5", got)
	}
	if got := toIntVal(5.9); got != 5 {
		t.Errorf("toIntVal(5.9) = %d, want 5 (truncated)", got)
	}
	if got := toIntVal("  42  "); got != 42 {
		t.Errorf(`toIntVal("  42  ") = %d, want 42`, got)
	}
	if got := toIntVal("not-a-number"); got != 0 {
		t.Errorf(`toIntVal("not-a-number") = %d, want 0`, got)
	}
	if got := toIntVal(nil); got != 0 {
		t.Errorf("toIntVal(nil) = %d, want 0", got)
	}
	if got := toIntVal(true); got != 0 {
		t.Errorf("toIntVal(true) = %d, want 0 (bool not handled -> default)", got)
	}
}

// TestAddValues_ListConcatAndStringFallback covers addValues' list-concat branch and its
// final string-coercion fallback (eval.go:487-504); the int/int and float paths are already
// well covered elsewhere.
func TestAddValues_ListConcatAndStringFallback(t *testing.T) {
	got := addValues([]any{1, 2}, []any{3, 4})
	if !equalAnySlices(got, []any{1, 2, 3, 4}) {
		t.Errorf("addValues(list,list) = %#v, want [1 2 3 4]", got)
	}
	// Mixed non-numeric, non-list types fall back to string concatenation.
	if got := addValues("foo", 5); got != "foo5" {
		t.Errorf(`addValues("foo", 5) = %#v, want "foo5"`, got)
	}
	if got := addValues(true, "x"); got != "Truex" {
		t.Errorf(`addValues(true, "x") = %#v, want "Truex"`, got)
	}
}

// TestSubValues_NonNumericFallback covers subValues' final "return 0" fallback (eval.go:
// 508-520) when neither operand is numeric — the int/int and float paths are covered by
// TestEvalMulDiv_StillWorksForValidOperands in eval_test.go.
func TestSubValues_NonNumericFallback(t *testing.T) {
	if got := subValues("a", "b"); got != 0 {
		t.Errorf(`subValues("a","b") = %#v, want 0`, got)
	}
	if got := subValues(nil, nil); got != 0 {
		t.Errorf("subValues(nil,nil) = %#v, want 0", got)
	}
	// int - float mix: takes the numFloat path, not the int/int fast path.
	got := subValues(5, 2.5)
	if f, ok := got.(float64); !ok || f != 2.5 {
		t.Errorf("subValues(5, 2.5) = %#v, want 2.5", got)
	}
}

// TestMulValues_NonNumericFallsBackToIntCoercion covers mulValues' final fallback
// (eval.go:525-537): non-numeric operands go through toIntVal on both sides.
func TestMulValues_NonNumericFallsBackToIntCoercion(t *testing.T) {
	if got := mulValues("3", "4"); got != 12 {
		t.Errorf(`mulValues("3","4") = %#v, want 12 (toIntVal coercion)`, got)
	}
	if got := mulValues(nil, 5); got != 0 {
		t.Errorf("mulValues(nil, 5) = %#v, want 0", got)
	}
	// int * float mix takes the numFloat path.
	got := mulValues(4, 2.5)
	if f, ok := got.(float64); !ok || f != 10.0 {
		t.Errorf("mulValues(4, 2.5) = %#v, want 10.0", got)
	}
}

// ---- unescapeStringLiteral ----------------------------------------------------------------

// TestUnescapeStringLiteral covers the escape/no-escape fast path and every recognized
// escape plus the "keep the backslash" default for an unrecognized one (eval.go:741-775,
// 47.6%/9.5% unit-only depending on the batch snapshot).
func TestUnescapeStringLiteral(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},         // fast path: no backslash at all
		{`a\\b`, `a\b`},            // \\ -> \
		{`a\'b`, `a'b`},            // \' -> '
		{`a\"b`, `a"b`},            // \" -> "
		{"a\\nb", "a\nb"},          // \n -> newline
		{"a\\tb", "a\tb"},          // \t -> tab
		{"a\\rb", "a\rb"},          // \r -> CR
		{`a\&b`, `a\&b`},           // unrecognized escape: backslash kept
		{`trailing\`, `trailing\`}, // trailing lone backslash (i+1 >= len(s))
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := unescapeStringLiteral(c.in); got != c.want {
				t.Errorf("unescapeStringLiteral(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestEvalAtom_StringLiteral_EndToEnd drives a quoted literal with escapes through evalAtom
// (eval.go:618-621).
func TestEvalAtom_StringLiteral_EndToEnd(t *testing.T) {
	eng := New(t.TempDir())
	ctx := newScope(nil)
	if got := evalOK(t, eng, ctx, `'line1\nline2'`); got != "line1\nline2" {
		t.Errorf(`evalExpr('line1\nline2') = %#v, want "line1\nline2"`, got)
	}
}

// ---- misc small gap-fillers ---------------------------------------------------------------

// TestFormatPyFloat_SpecialValues covers formatPyFloat's Inf/-Inf/NaN branches (eval.go:
// 1296-1306) — the finite-value path is well covered by jsonenc.PyFloatRepr's own tests.
func TestFormatPyFloat_SpecialValues(t *testing.T) {
	if got := formatPyFloat(mathInf(1)); got != "inf" {
		t.Errorf("formatPyFloat(+Inf) = %q, want inf", got)
	}
	if got := formatPyFloat(mathInf(-1)); got != "-inf" {
		t.Errorf("formatPyFloat(-Inf) = %q, want -inf", got)
	}
	if got := formatPyFloat(mathNaN()); got != "nan" {
		t.Errorf("formatPyFloat(NaN) = %q, want nan", got)
	}
}

// TestIsFalsey_MapAndObject covers isFalsey's map[string]any and *jsonenc.Object branches
// (eval.go:1361-1364), previously only reached at 45.5%.
func TestIsFalsey_MapAndObject(t *testing.T) {
	if !isFalsey(map[string]any{}) {
		t.Error("isFalsey(empty map) = false, want true")
	}
	if isFalsey(map[string]any{"a": 1}) {
		t.Error("isFalsey(non-empty map) = true, want false")
	}
	if !isFalsey(jsonenc.NewObject()) {
		t.Error("isFalsey(empty *Object) = false, want true")
	}
	if isFalsey(jsonenc.NewObject().Set("a", 1)) {
		t.Error("isFalsey(non-empty *Object) = true, want false")
	}
	// default branch: any other type (e.g. a func value) is never falsey.
	if isFalsey(func() {}) {
		t.Error("isFalsey(func) = true, want false (default branch)")
	}
}

// TestLengthOf_MapAndDefault covers lengthOf's map[string]any branch and the default-0
// fallback for unsupported types (eval.go:1370-1384), beyond the *jsonenc.Object case
// TestLengthOf_JsonencObject in eval_test.go already covers.
func TestLengthOf_MapAndDefault(t *testing.T) {
	if got := lengthOf(map[string]any{"a": 1, "b": 2}); got != 2 {
		t.Errorf("lengthOf(map) = %d, want 2", got)
	}
	if got := lengthOf(42); got != 0 {
		t.Errorf("lengthOf(42) = %d, want 0 (unsupported type)", got)
	}
	if got := lengthOf(nil); got != 0 {
		t.Errorf("lengthOf(nil) = %d, want 0", got)
	}
}

// TestAtomKind covers atomKind's bool/int/default branches directly (eval.go:919-930).
func TestAtomKind(t *testing.T) {
	if got := atomKind(nil); got != 0 {
		t.Errorf("atomKind(nil) = %d, want 0", got)
	}
	if got := atomKind(true); got != 1 {
		t.Errorf("atomKind(true) = %d, want 1", got)
	}
	if got := atomKind(5); got != 2 {
		t.Errorf("atomKind(5) = %d, want 2", got)
	}
	if got := atomKind("s"); got != 3 {
		t.Errorf(`atomKind("s") = %d, want 3`, got)
	}
}

// TestJinjaLess_StringFallback covers jinjaLess' case-insensitive string-comparison branch
// (eval.go:1736-1743) — the numeric branch is already covered by TestMinMaxFilter_EmptyList
// and the |min/|max filter tests, but never the non-numeric fallback.
func TestJinjaLess_StringFallback(t *testing.T) {
	if !jinjaLess("Apple", "banana") {
		t.Error(`jinjaLess("Apple","banana") = false, want true (case-insensitive)`)
	}
	if jinjaLess("Banana", "apple") {
		t.Error(`jinjaLess("Banana","apple") = true, want false`)
	}
}

// TestAttrValue_UnsupportedTypeReturnsNil covers attrValue's default fallback (eval.go:
// 1746-1759) when an intermediate value in the dotted path isn't a container.
func TestAttrValue_UnsupportedTypeReturnsNil(t *testing.T) {
	if got := attrValue("a plain string", "attr"); got != nil {
		t.Errorf(`attrValue("a plain string","attr") = %#v, want nil`, got)
	}
	if got := attrValue(nil, "attr"); got != nil {
		t.Errorf("attrValue(nil,\"attr\") = %#v, want nil", got)
	}
}

func mathInf(sign int) float64 { return math.Inf(sign) }
func mathNaN() float64         { return math.NaN() }
