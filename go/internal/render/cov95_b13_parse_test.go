package render

// cov95_b13_parse_test.go — batch 13 (coverage/raise-toward-95): direct unit tests for the Jinja
// template parser (parse.go). These call parse(src) directly with crafted template strings and
// assert on the resulting AST shape, rather than going through the full Engine.Render pipeline —
// parse.go's functions are pure syntax-tree builders, so this is a much more direct way to hit
// their branches (especially error paths and rarer tag forms) than driving full template
// rendering end to end.

import (
	"strings"
	"testing"
)

// ---- parse / parseUntil (top-level entry + stop-keyword loop) --------------------------------

func TestParse_PlainTextAndOutputAndComment(t *testing.T) {
	res, err := parse("hello {{ x }} world {# a comment #} tail")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(res.nodes) != 5 {
		t.Fatalf("want 5 nodes (text,output,text,comment,text), got %d: %#v", len(res.nodes), res.nodes)
	}
	if _, ok := res.nodes[0].(textNode); !ok {
		t.Errorf("node0 = %#v, want textNode", res.nodes[0])
	}
	out, ok := res.nodes[1].(outputNode)
	if !ok || strings.TrimSpace(out.expr) != "x" {
		t.Errorf("node1 = %#v, want outputNode{expr:x}", res.nodes[1])
	}
	if _, ok := res.nodes[3].(commentNode); !ok {
		t.Errorf("node3 = %#v, want commentNode", res.nodes[3])
	}
}

func TestParse_UnsupportedTagReturnsError(t *testing.T) {
	_, err := parse("{% bogus %}")
	if err == nil {
		t.Fatal("want error for unsupported tag")
	}
	if !strings.Contains(err.Error(), "unsupported tag") {
		t.Errorf("err = %v", err)
	}
}

// ---- parseTag: extends / include / set (inline + block) --------------------------------------

func TestParseTag_Extends(t *testing.T) {
	res, err := parse(`{% extends "base.html" %}{% block body %}hi{% endblock %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if res.extends != "base.html" {
		t.Errorf("extends = %q, want base.html", res.extends)
	}
	if _, ok := res.blocks["body"]; !ok {
		t.Errorf("blocks missing 'body': %#v", res.blocks)
	}
}

func TestParseTag_Include(t *testing.T) {
	res, err := parse(`{% include 'partial.html' %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(res.nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(res.nodes))
	}
	inc, ok := res.nodes[0].(includeNode)
	if !ok || inc.name != "partial.html" {
		t.Errorf("node = %#v, want includeNode{partial.html}", res.nodes[0])
	}
}

func TestParseTag_SetInline(t *testing.T) {
	res, err := parse(`{% set x = 1 + 2 %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	sn, ok := res.nodes[0].(setNode)
	if !ok || sn.name != "x" || sn.expr != "1 + 2" {
		t.Errorf("node = %#v, want setNode{x, 1 + 2}", res.nodes[0])
	}
}

func TestParseTag_SetBlockForm(t *testing.T) {
	res, err := parse(`{% set greeting %}hello {{ name }}{% endset %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	sb, ok := res.nodes[0].(setBlockNode)
	if !ok || sb.name != "greeting" {
		t.Errorf("node = %#v, want setBlockNode{greeting}", res.nodes[0])
	}
	if len(sb.body) != 2 { // "hello " textNode + outputNode
		t.Errorf("body = %#v, want 2 nodes", sb.body)
	}
}

func TestParseTag_ImportNoOp(t *testing.T) {
	res, err := parse(`{% import "macros.html" as m %}tail`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	// import is recorded as nothing (nil node); only the trailing text node should remain.
	if len(res.nodes) != 1 {
		t.Fatalf("want 1 node (the tail text), got %d: %#v", len(res.nodes), res.nodes)
	}
	if _, ok := res.nodes[0].(textNode); !ok {
		t.Errorf("node0 = %#v, want textNode", res.nodes[0])
	}
}

// ---- parseFrom ----------------------------------------------------------------------------------

func TestParseFrom_MultipleNamesWithAlias(t *testing.T) {
	res, err := parse(`{% from "macros.html" import foo, bar as baz %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(res.imports) != 1 {
		t.Fatalf("want 1 import spec, got %d", len(res.imports))
	}
	imp := res.imports[0]
	if imp.template != "macros.html" {
		t.Errorf("template = %q", imp.template)
	}
	if len(imp.names) != 2 || imp.names[0] != "foo" || imp.names[1] != "bar" {
		t.Errorf("names = %#v, want [foo bar] (alias dropped)", imp.names)
	}
}

func TestParseFrom_MissingImportKeywordErrors(t *testing.T) {
	_, err := parse(`{% from "macros.html" %}`)
	if err == nil {
		t.Fatal("want error for missing ' import '")
	}
	if !strings.Contains(err.Error(), "bad from") {
		t.Errorf("err = %v", err)
	}
}

// ---- parseMacro / parseParams -------------------------------------------------------------------

func TestParseMacro_WithDefaultsAndRequired(t *testing.T) {
	res, err := parse(`{% macro greet(name, greeting='Hi') %}{{ greeting }}, {{ name }}{% endmacro %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	m, ok := res.macros["greet"]
	if !ok {
		t.Fatalf("macro 'greet' not recorded: %#v", res.macros)
	}
	if len(m.params) != 2 {
		t.Fatalf("want 2 params, got %d: %#v", len(m.params), m.params)
	}
	if m.params[0].name != "name" || m.params[0].def != "" {
		t.Errorf("param0 = %#v, want required 'name'", m.params[0])
	}
	if m.params[1].name != "greeting" || m.params[1].def != "'Hi'" {
		t.Errorf("param1 = %#v, want default 'Hi'", m.params[1])
	}
}

func TestParseParams_EmptyAndBlankEntriesSkipped(t *testing.T) {
	params := parseParams(" , a , , b=2 , ")
	if len(params) != 2 {
		t.Fatalf("want 2 params (blanks skipped), got %d: %#v", len(params), params)
	}
	if params[0].name != "a" {
		t.Errorf("params[0] = %#v", params[0])
	}
	if params[1].name != "b" || params[1].def != "2" {
		t.Errorf("params[1] = %#v", params[1])
	}
}

func TestParseParams_NoArgs(t *testing.T) {
	params := parseParams("")
	if len(params) != 0 {
		t.Errorf("want 0 params for empty string, got %#v", params)
	}
}

// ---- parseIf: elif chains, else, and bare if/endif ------------------------------------------

func TestParseIf_SimpleIfEndif(t *testing.T) {
	res, err := parse(`{% if x %}A{% endif %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	n, ok := res.nodes[0].(*ifNode)
	if !ok {
		t.Fatalf("node = %#v, want *ifNode", res.nodes[0])
	}
	if len(n.branches) != 1 || n.branches[0].cond != "x" {
		t.Errorf("branches = %#v", n.branches)
	}
}

func TestParseIf_ElifElseChain(t *testing.T) {
	res, err := parse(`{% if a %}A{% elif b %}B{% elif c %}C{% else %}D{% endif %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	n := res.nodes[0].(*ifNode)
	if len(n.branches) != 4 {
		t.Fatalf("want 4 branches (if/elif/elif/else), got %d: %#v", len(n.branches), n.branches)
	}
	conds := []string{n.branches[0].cond, n.branches[1].cond, n.branches[2].cond, n.branches[3].cond}
	want := []string{"a", "b", "c", ""}
	for i := range want {
		if conds[i] != want[i] {
			t.Errorf("branch[%d].cond = %q, want %q", i, conds[i], want[i])
		}
	}
}

// ---- parseCall: {% call macro() %} and {% call(params) macro() %} -----------------------------

func TestParseCall_NoParams(t *testing.T) {
	res, err := parse(`{% call render_box() %}content{% endcall %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	cn, ok := res.nodes[0].(*callNode)
	if !ok {
		t.Fatalf("node = %#v, want *callNode", res.nodes[0])
	}
	if cn.macroExpr != "render_box()" {
		t.Errorf("macroExpr = %q", cn.macroExpr)
	}
	if len(cn.callerParams) != 0 {
		t.Errorf("callerParams = %#v, want empty", cn.callerParams)
	}
}

func TestParseCall_WithCallerParams(t *testing.T) {
	res, err := parse(`{% call(item, idx) render_row(item) %}row{% endcall %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	cn := res.nodes[0].(*callNode)
	if len(cn.callerParams) != 2 || cn.callerParams[0] != "item" || cn.callerParams[1] != "idx" {
		t.Errorf("callerParams = %#v, want [item idx]", cn.callerParams)
	}
	if cn.macroExpr != "render_row(item)" {
		t.Errorf("macroExpr = %q", cn.macroExpr)
	}
}

func TestParseTag_CallNoSpaceBeforeParen(t *testing.T) {
	// {% call(x) foo() %} tokenizes so that firstWord returns "call(x)" (no space after `call`);
	// parseTag's default branch special-cases this via strings.HasPrefix(text, "call(").
	res, err := parse(`{% call(x) foo(x) %}body{% endcall %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if _, ok := res.nodes[0].(*callNode); !ok {
		t.Errorf("node = %#v, want *callNode via call( prefix", res.nodes[0])
	}
}

// ---- parseFor: vars, iter, else clause -------------------------------------------------------

func TestParseFor_SimpleLoop(t *testing.T) {
	res, err := parse(`{% for item in items %}{{ item }}{% endfor %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	fn, ok := res.nodes[0].(*forNode)
	if !ok {
		t.Fatalf("node = %#v, want *forNode", res.nodes[0])
	}
	if len(fn.vars) != 1 || fn.vars[0] != "item" {
		t.Errorf("vars = %#v", fn.vars)
	}
	if fn.iter != "items" {
		t.Errorf("iter = %q", fn.iter)
	}
	if fn.elseBody != nil {
		t.Errorf("elseBody = %#v, want nil (no else clause)", fn.elseBody)
	}
}

func TestParseFor_MultipleVarsAndElse(t *testing.T) {
	res, err := parse(`{% for k, v in items %}{{ k }}{% else %}empty{% endfor %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	fn := res.nodes[0].(*forNode)
	if len(fn.vars) != 2 || fn.vars[0] != "k" || fn.vars[1] != "v" {
		t.Errorf("vars = %#v, want [k v]", fn.vars)
	}
	if fn.elseBody == nil || len(fn.elseBody) != 1 {
		t.Errorf("elseBody = %#v, want 1 text node", fn.elseBody)
	}
}

func TestParseFor_MissingInErrors(t *testing.T) {
	_, err := parse(`{% for item items %}{% endfor %}`)
	if err == nil {
		t.Fatal("want error for malformed for (no ' in ')")
	}
	if !strings.Contains(err.Error(), "bad for") {
		t.Errorf("err = %v", err)
	}
}

// ---- parseWith --------------------------------------------------------------------------------

func TestParseWith_SingleAssignment(t *testing.T) {
	res, err := parse(`{% with total = items|length %}{{ total }}{% endwith %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	wn, ok := res.nodes[0].(*withNode)
	if !ok {
		t.Fatalf("node = %#v, want *withNode", res.nodes[0])
	}
	if len(wn.assigns) != 1 || wn.assigns[0][0] != "total" || wn.assigns[0][1] != "items|length" {
		t.Errorf("assigns = %#v", wn.assigns)
	}
}

func TestParseWith_NoAssignment(t *testing.T) {
	res, err := parse(`{% with %}body{% endwith %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	wn := res.nodes[0].(*withNode)
	if len(wn.assigns) != 0 {
		t.Errorf("assigns = %#v, want empty (no '=' present)", wn.assigns)
	}
}

// ---- parseBlock ---------------------------------------------------------------------------------

func TestParseBlock_NamedEndblock(t *testing.T) {
	res, err := parse(`{% block content %}hi{% endblock content %}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	bn, ok := res.nodes[0].(blockNode)
	if !ok || bn.name != "content" {
		t.Errorf("node = %#v, want blockNode{content}", res.nodes[0])
	}
	if _, ok := res.blocks["content"]; !ok {
		t.Errorf("blocks map missing 'content': %#v", res.blocks)
	}
}

// ---- unquote ------------------------------------------------------------------------------------

func TestUnquote(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"hello"`, "hello"},
		{`'hello'`, "hello"},
		{"  'trimmed'  ", "trimmed"},
		{"noquotes", "noquotes"},
		{`"`, `"`},                       // single char, len<2 -> unchanged
		{``, ``},                         // empty -> unchanged
		{`"mismatched'`, `"mismatched'`}, // differing quote chars -> unchanged
	}
	for _, c := range cases {
		if got := unquote(c.in); got != c.want {
			t.Errorf("unquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- firstWord / contains (small helpers exercised incidentally above, direct-tested here) -----

func TestFirstWord(t *testing.T) {
	if got := firstWord("  if x  "); got != "if" {
		t.Errorf("firstWord = %q, want if", got)
	}
	if got := firstWord("solo"); got != "solo" {
		t.Errorf("firstWord = %q, want solo", got)
	}
}

func TestContainsHelper(t *testing.T) {
	if !contains([]string{"a", "b"}, "b") {
		t.Error("want true for present element")
	}
	if contains([]string{"a", "b"}, "c") {
		t.Error("want false for absent element")
	}
}
