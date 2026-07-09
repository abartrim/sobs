package render

// cov95_b2_engine_test.go — batch 2 (coverage/raise-toward-95): unit tests for
// internal/render/engine.go functions that are pure and template-driven but had low
// unit-only coverage (the golden corpus exercises many of these, but plenty of branches —
// especially error paths, macro imports, block-inheritance super(), {% call %}, {% with %},
// dotted-set on a namespace, and toList's various input shapes — are missed by unit tests
// run in isolation). Every test writes small templates to a t.TempDir() and drives them
// through the real Engine.Render/New/AddFunc entry points, so these are black-box tests of
// the template engine, not white-box probes of engine.go internals.
//
// Checked coverage_eval_gaps_test.go / coverage_divvalues_test.go / coverage_filters_gaps_test.go
// first: none of them touch engine.go at all (they exercise evalExpr/applyFilter directly).
// This file is new territory.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

func writeTemplateB2(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

// TestEngine_AddFunc_And_KWOrder covers AddFunc (engine.go:51, 0% unit-only) and KWOrder
// (engine.go:34, 0% unit-only): a registered func is callable from a template, and the
// keyword-argument insertion order it observed is exposed via KWOrder.
func TestEngine_AddFunc_And_KWOrder(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{{ myfunc(b=2, a=1) }}")
	eng := New(dir)
	var seenOrder []string
	eng.AddFunc("myfunc", func(pos []any, kw map[string]any) (any, error) {
		seenOrder = append(seenOrder, eng.KWOrder()...)
		return "ok", nil
	})
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "ok" {
		t.Errorf("Render output = %q, want %q", out, "ok")
	}
	if len(seenOrder) != 2 || seenOrder[0] != "b" || seenOrder[1] != "a" {
		t.Errorf("KWOrder() = %#v, want [b a] (insertion order)", seenOrder)
	}
}

// TestEngine_Load_ErrorPropagation covers the error branch of load/Render when a template
// file does not exist (engine.go:58-59, 116-118).
func TestEngine_Load_ErrorPropagation(t *testing.T) {
	dir := t.TempDir()
	eng := New(dir)
	if _, err := eng.Render("does-not-exist.html", nil); err == nil {
		t.Fatal("Render(missing template) = nil error, want an error")
	}
}

// TestEngine_Load_CRLFTrailingNewline covers load's \r\n-stripping branch
// (engine.go:64-66), distinct from the plain \n case.
func TestEngine_Load_CRLFTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "crlf.html", "hello\r\n")
	eng := New(dir)
	out, err := eng.Render("crlf.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "hello" {
		t.Errorf("Render CRLF template = %q, want %q (trailing CRLF stripped)", out, "hello")
	}
}

// TestEngine_Load_Cached exercises the cache-hit branch of load (engine.go:54-56): render
// the same template twice and confirm the parse is reused (no observable difference, but
// this hits the `ok` branch that a single render never reaches).
func TestEngine_Load_Cached(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "cached.html", "{{ x }}")
	eng := New(dir)
	ctx := map[string]any{"x": 1}
	out1, err := eng.Render("cached.html", ctx)
	if err != nil {
		t.Fatalf("Render #1: %v", err)
	}
	out2, err := eng.Render("cached.html", ctx)
	if err != nil {
		t.Fatalf("Render #2: %v", err)
	}
	if out1 != out2 || out1 != "1" {
		t.Errorf("Render outputs = %q, %q, want both %q", out1, out2, "1")
	}
}

// TestEngine_LookupDotted covers scope.lookupDotted (engine.go:94-108, 0% unit-only):
// map[string]any attribute chains, and the default nil-fallback when an intermediate value
// isn't a container.
func TestEngine_LookupDotted(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{{ a.b.c }}|{{ missing.b.c }}|{{ notanobj.x }}")
	eng := New(dir)
	ctx := map[string]any{
		"a":        map[string]any{"b": map[string]any{"c": "deep"}},
		"notanobj": "just-a-string",
	}
	out, err := eng.Render("t.html", ctx)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "deep||" {
		t.Errorf("Render = %q, want %q", out, "deep||")
	}
}

// TestEngine_LookupDotted_JsonencObject covers lookupDotted's *jsonenc.Object branch.
func TestEngine_LookupDotted_JsonencObject(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{{ obj.name }}")
	eng := New(dir)
	obj := jsonenc.NewObject()
	obj.Set("name", "widget")
	out, err := eng.Render("t.html", map[string]any{"obj": obj})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "widget" {
		t.Errorf("Render = %q, want %q", out, "widget")
	}
}

// TestEngine_SetDottedNamespace covers the dotted-set branch (engine.go:209-214): mutating
// a namespace-shaped map[string]any via {% set ns.found = true %}.
func TestEngine_SetDottedNamespace(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html",
		"{% set ns = namespace(found=false) %}{% set ns.found = true %}{{ ns.found }}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "True" {
		t.Errorf("Render = %q, want %q", out, "True")
	}
}

// TestEngine_SetDotted_FallsBackWhenNotNamespace covers the case where the LHS before the
// last dot does NOT resolve to a map[string]any: falls back to setting `x.y` as a literal
// scope key rather than panicking (engine.go:209-215).
func TestEngine_SetDotted_FallsBackWhenNotNamespace(t *testing.T) {
	dir := t.TempDir()
	// "missing" was never defined, so lookupDotted("missing") is nil, not a map -> fallback.
	writeTemplateB2(t, dir, "t.html", "{% set missing.attr = 5 %}ok")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "ok" {
		t.Errorf("Render = %q, want %q", out, "ok")
	}
}

// TestEngine_SetBlock covers setBlockNode (engine.go:216-221): body is rendered and captured
// as safe Markup into the variable (no double-escaping on subsequent output).
func TestEngine_SetBlock(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% set greeting %}<b>hi</b>{% endset %}{{ greeting }}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "<b>hi</b>" {
		t.Errorf("Render = %q, want unescaped %q", out, "<b>hi</b>")
	}
}

// TestEngine_IfElifElse covers renderNode's ifNode branch beyond a bare if (engine.go:222-234):
// an if/elif/else chain.
func TestEngine_IfElifElse(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html",
		"{% if x == 1 %}one{% elif x == 2 %}two{% else %}other{% endif %}")
	eng := New(dir)
	for _, c := range []struct {
		x    int
		want string
	}{{1, "one"}, {2, "two"}, {3, "other"}} {
		out, err := eng.Render("t.html", map[string]any{"x": c.x})
		if err != nil {
			t.Fatalf("Render(x=%d): %v", c.x, err)
		}
		if out != c.want {
			t.Errorf("Render(x=%d) = %q, want %q", c.x, out, c.want)
		}
	}
}

func TestEngine_If_CondErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% if no_such_func() %}yes{% endif %}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring if-condition = nil error, want an error")
	}
}

// TestEngine_ForLoop covers renderFor (engine.go:289-309, 0% unit-only): loop variable
// binding, the `loop` context dict (index/index0/first/last/length), and the {% else %}
// branch on an empty iterable.
func TestEngine_ForLoop(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html",
		"{% for x in items %}{{ loop.index }}:{{ loop.index0 }}:{{ x }}"+
			"{% if loop.first %}(first){% endif %}{% if loop.last %}(last){% endif %};"+
			"{% endfor %}")
	eng := New(dir)
	out, err := eng.Render("t.html", map[string]any{"items": []any{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "1:0:a(first);2:1:b;3:2:c(last);"
	if out != want {
		t.Errorf("Render = %q, want %q", out, want)
	}
}

func TestEngine_ForLoop_ElseOnEmpty(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% for x in items %}{{ x }}{% else %}empty{% endfor %}")
	eng := New(dir)
	out, err := eng.Render("t.html", map[string]any{"items": []any{}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "empty" {
		t.Errorf("Render on empty iterable = %q, want %q", out, "empty")
	}
}

// TestEngine_ForLoop_TupleUnpacking covers bindLoopVars' tuple-unpacking branch
// (engine.go:354-367, 0% unit-only): {% for k, v in pairs %}.
func TestEngine_ForLoop_TupleUnpacking(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% for k, v in pairs %}{{ k }}={{ v }};{% endfor %}")
	eng := New(dir)
	pairs := []any{[]any{"a", 1}, []any{"b", 2}}
	out, err := eng.Render("t.html", map[string]any{"pairs": pairs})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "a=1;b=2;" {
		t.Errorf("Render = %q, want %q", out, "a=1;b=2;")
	}
}

// TestEngine_ForLoop_TupleUnpacking_ShortTuple covers bindLoopVars when the tuple has fewer
// elements than vars (the `if i < len(tup)` guard, engine.go:361-364).
func TestEngine_ForLoop_TupleUnpacking_ShortTuple(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% for k, v in pairs %}{{ k }}={{ v|default('MISSING') }};{% endfor %}")
	eng := New(dir)
	pairs := []any{[]any{"a"}}
	out, err := eng.Render("t.html", map[string]any{"pairs": pairs})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "a=MISSING;" {
		t.Errorf("Render = %q, want %q", out, "a=MISSING;")
	}
}

// TestEngine_ForLoop_IterError covers renderFor's iterable-evaluation error branch
// (engine.go:290-293).
func TestEngine_ForLoop_IterError(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% for x in no_such_func() %}{{ x }}{% endfor %}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring for-iterable = nil error, want an error")
	}
}

// TestEngine_ForLoop_BodyError covers renderFor's body-render error branch (engine.go:304-306).
func TestEngine_ForLoop_BodyError(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% for x in items %}{{ no_such_func() }}{% endfor %}")
	eng := New(dir)
	if _, err := eng.Render("t.html", map[string]any{"items": []any{1}}); err == nil {
		t.Fatal("Render with erroring for-body = nil error, want an error")
	}
}

// TestEngine_With covers renderWith (engine.go:342-352, 0% unit-only): assigns are scoped to
// the with-block only, and evaluated in the OUTER scope (Jinja semantics: `{% with x = x + 1 %}`
// reads the pre-with x).
func TestEngine_With(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% with y = x + 1 %}{{ y }}{% endwith %}|{{ y|default('gone') }}")
	eng := New(dir)
	out, err := eng.Render("t.html", map[string]any{"x": 4})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "5|gone" {
		t.Errorf("Render = %q, want %q (y not visible outside with)", out, "5|gone")
	}
}

func TestEngine_With_AssignErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% with y = no_such_func() %}{{ y }}{% endwith %}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring with-assign = nil error, want an error")
	}
}

// TestEngine_Call covers renderCall (engine.go:313-340, 0% unit-only): {% call %} binds
// caller() to the block body and invokes a macro; the macro's `{{ caller() }}` splices the
// rendered body back in.
func TestEngine_Call(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		"{% macro wrap() %}<div>{{ caller() }}</div>{% endmacro %}")
	writeTemplateB2(t, dir, "t.html",
		"{% from \"macros.html\" import wrap %}"+
			"{% call wrap() %}inner{% endcall %}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "<div>inner</div>" {
		t.Errorf("Render = %q, want %q", out, "<div>inner</div>")
	}
}

// TestEngine_Call_WithCallerParams covers callNode.callerParams binding (engine.go:318-322):
// {% call(item) macro() %}{{ item }}{% endcall %}.
func TestEngine_Call_WithCallerParams(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		"{% macro listify() %}[{{ caller(1) }},{{ caller(2) }}]{% endmacro %}")
	writeTemplateB2(t, dir, "t.html",
		"{% from \"macros.html\" import listify %}"+
			"{% call(n) listify() %}{{ n }}{% endcall %}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "[1,2]" {
		t.Errorf("Render = %q, want %q", out, "[1,2]")
	}
}

// TestEngine_Call_MacroExprErrorPropagates covers renderCall's macro-eval error branch
// (engine.go:329, 335-337), and confirms `caller` is still cleaned up (delete branch,
// engine.go:333) rather than leaking into a later render.
func TestEngine_Call_MacroExprErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% call no_such_func() %}inner{% endcall %}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring call-macro-expr = nil error, want an error")
	}
	// caller must not be defined outside any {% call %}.
	writeTemplateB2(t, dir, "t2.html", "{{ caller is defined }}")
	out, err := eng.Render("t2.html", nil)
	if err != nil {
		t.Fatalf("Render t2: %v", err)
	}
	if out != "False" {
		t.Errorf("caller is defined outside call = %q, want False (no leak)", out)
	}
}

// TestEngine_Call_RestoresPriorCallerFunc covers the "had" branch of renderCall (engine.go:
// 330-331): a NESTED {% call %} must restore the outer caller() after it completes, not
// delete it.
func TestEngine_Call_RestoresPriorCallerFunc(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		"{% macro outer() %}O[{{ caller() }}]{% endmacro %}"+
			"{% macro inner() %}I[{{ caller() }}]{% endmacro %}")
	writeTemplateB2(t, dir, "t.html",
		"{% from \"macros.html\" import outer, inner %}"+
			"{% call outer() %}{% call inner() %}deep{% endcall %}{% endcall %}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "O[I[deep]]" {
		t.Errorf("Render = %q, want %q", out, "O[I[deep]]")
	}
}

// TestEngine_Include covers the includeNode branch of renderNode (engine.go:251-256).
func TestEngine_Include(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "partial.html", "partial:{{ x }}")
	writeTemplateB2(t, dir, "t.html", "before,{% include \"partial.html\" %},after")
	eng := New(dir)
	out, err := eng.Render("t.html", map[string]any{"x": 42})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "before,partial:42,after" {
		t.Errorf("Render = %q, want %q", out, "before,partial:42,after")
	}
}

func TestEngine_Include_MissingTemplateErrors(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% include \"missing.html\" %}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with missing include = nil error, want an error")
	}
}

// TestEngine_BlockInheritance_NoOverride covers renderBlock's zero-chain fallback
// (engine.go:264-266, 0% unit-only): rendering a base template directly uses the block's own
// default body since blockChains has no entries for it.
func TestEngine_BlockInheritance_NoOverride(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "base.html", "[{% block content %}default{% endblock %}]")
	eng := New(dir)
	out, err := eng.Render("base.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "[default]" {
		t.Errorf("Render = %q, want %q", out, "[default]")
	}
}

// TestEngine_BlockInheritance_Override covers renderBlockAt (engine.go:274-287, 0%
// unit-only): a child template overriding a parent block.
func TestEngine_BlockInheritance_Override(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "base.html", "[{% block content %}default{% endblock %}]")
	writeTemplateB2(t, dir, "child.html",
		"{% extends \"base.html\" %}{% block content %}child{% endblock %}")
	eng := New(dir)
	out, err := eng.Render("child.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "[child]" {
		t.Errorf("Render = %q, want %q", out, "[child]")
	}
}

// TestEngine_BlockInheritance_SuperThreeLevels covers renderBlockAt's super() pre-render
// (engine.go:276-285): a three-level chain (grandparent -> parent -> child) exercises the
// idx>0 recursion more than once.
func TestEngine_BlockInheritance_SuperThreeLevels(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "grandparent.html", "{% block content %}G{% endblock %}")
	writeTemplateB2(t, dir, "parent.html",
		"{% extends \"grandparent.html\" %}{% block content %}P-{{ super() }}{% endblock %}")
	writeTemplateB2(t, dir, "child.html",
		"{% extends \"parent.html\" %}{% block content %}C-{{ super() }}{% endblock %}")
	eng := New(dir)
	out, err := eng.Render("child.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "C-P-G" {
		t.Errorf("Render = %q, want %q", out, "C-P-G")
	}
}

// TestEngine_MacroImport_SiblingVisibility covers the sibling-macro registration branch of
// Render (engine.go:144-150): a macro imported by name can call an un-imported sibling macro
// defined in the same file.
func TestEngine_MacroImport_SiblingVisibility(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "macros.html",
		"{% macro helper() %}H{% endmacro %}"+
			"{% macro main() %}M-{{ helper() }}{% endmacro %}")
	writeTemplateB2(t, dir, "t.html",
		"{% from \"macros.html\" import main %}{{ main() }}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "M-H" {
		t.Errorf("Render = %q, want %q (sibling macro visible)", out, "M-H")
	}
}

// TestEngine_MacroImport_MissingTemplateErrors covers Render's import-load error branch
// (engine.go:140-143).
func TestEngine_MacroImport_MissingTemplateErrors(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% from \"missing.html\" import x %}{{ x() }}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render importing from a missing template = nil error, want an error")
	}
}

// TestEngine_Render_OutputExprError covers renderNode's outputNode error branch
// (engine.go:197-199).
func TestEngine_Render_OutputExprError(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{{ no_such_func() }}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring output expr = nil error, want an error")
	}
}

// TestEngine_Render_SetExprError covers renderNode's setNode error branch (engine.go:203-205).
func TestEngine_Render_SetExprError(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "{% set x = no_such_func() %}{{ x }}")
	eng := New(dir)
	if _, err := eng.Render("t.html", nil); err == nil {
		t.Fatal("Render with erroring set expr = nil error, want an error")
	}
}

// TestEngine_CommentNode covers the commentNode no-op branch of renderNode (engine.go:194-195).
func TestEngine_CommentNode(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html", "a{# a comment #}b")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "ab" {
		t.Errorf("Render = %q, want %q (comment produces no output)", out, "ab")
	}
}

// TestToList covers toList's branches directly (engine.go:369-392, 16.7% unit-only): []any
// passthrough, map[string]any -> keys, nil -> nil, and the scalar-wraps-in-slice default.
func TestToList(t *testing.T) {
	if got := toList([]any{1, 2, 3}); len(got) != 3 {
		t.Errorf("toList([]any{1,2,3}) = %#v, want len 3", got)
	}
	if got := toList(nil); got != nil {
		t.Errorf("toList(nil) = %#v, want nil", got)
	}
	if got := toList(42); len(got) != 1 || got[0] != 42 {
		t.Errorf("toList(42) = %#v, want [42] (scalar wrapped)", got)
	}
	m := map[string]any{"a": 1, "b": 2}
	got := toList(m)
	if len(got) != 2 {
		t.Errorf("toList(map) = %#v, want len 2", got)
	}
}

// TestToList_JsonencObjectKeyOrder covers toList's *jsonenc.Object branch (iterating a
// mapping yields its keys, in insertion order) — previously entirely uncovered by unit tests.
func TestToList_JsonencObjectKeyOrder(t *testing.T) {
	obj := jsonenc.NewObject()
	obj.Set("z", 1)
	obj.Set("a", 2)
	obj.Set("m", 3)
	got := toList(obj)
	want := []any{"z", "a", "m"}
	if len(got) != len(want) {
		t.Fatalf("toList(obj) = %#v, want len %d", got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("toList(obj)[%d] = %#v, want %#v (insertion order)", i, got[i], w)
		}
	}
}

// TestEngine_Namespace_MultipleKeys covers New's namespace() builtin (engine.go:40-46) with
// more than one keyword arg.
func TestEngine_Namespace_MultipleKeys(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html",
		"{% set ns = namespace(a=1, b=2) %}{{ ns.a }}-{{ ns.b }}")
	eng := New(dir)
	out, err := eng.Render("t.html", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "1-2" {
		t.Errorf("Render = %q, want %q", out, "1-2")
	}
}

// TestEngine_Lookup_ParentScope covers scope.lookup's parent-chain walk (engine.go:85-92)
// beyond a single-level scope: a variable defined in an outer {% for %} scope must be
// visible from a nested {% if %} scope.
func TestEngine_Lookup_ParentScope(t *testing.T) {
	dir := t.TempDir()
	writeTemplateB2(t, dir, "t.html",
		"{% for x in items %}{% if x > 1 %}{{ x }}{% endif %}{% endfor %}")
	eng := New(dir)
	out, err := eng.Render("t.html", map[string]any{"items": []any{1, 2, 3}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "23" {
		t.Errorf("Render = %q, want %q", out, "23")
	}
}
