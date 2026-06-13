package render

import (
	"fmt"
	"strings"
)

// AST node types.
type node interface{}

type textNode struct{ text string }
type outputNode struct{ expr string } // {{ expr }}
type commentNode struct{}

type ifBranch struct {
	cond string // empty for the final {% else %}
	body []node
}
type ifNode struct{ branches []ifBranch }

type forNode struct {
	vars     []string // loop variables
	iter     string   // iterable expression
	body     []node
	elseBody []node // {% for %}...{% else %}...{% endfor %} (when iterable empty)
}

type withNode struct {
	assigns [][2]string // name, expr
	body    []node
}

type setNode struct {
	name string
	expr string
}

// setBlockNode is {% set name %}...{% endset %}: the rendered body is captured (as safe
// Markup) into the variable.
type setBlockNode struct {
	name string
	body []node
}

type blockNode struct {
	name string
	body []node
}

// callNode is {% call [(params)] macroexpr %}body{% endcall %}: invokes macroexpr with
// caller() bound to the body (rendered in the call-site scope + optional params).
type callNode struct {
	callerParams []string
	macroExpr    string
	body         []node
}

type includeNode struct{ name string }

type extendsNode struct{ name string }

// macro support
type macroParam struct {
	name string
	def  string // default-value expression, "" if required
}
type macroDef struct {
	name   string
	params []macroParam
	body   []node
}
type importSpec struct {
	template string
	names    []string // imported macro names
}

// parseResult is the parsed template: its node list plus, if it extends a parent, the
// parent name (the top-level nodes of an extending child are only its block overrides).
type parseResult struct {
	nodes   []node
	extends string
	blocks  map[string][]node    // block name -> body (collected from this template)
	macros  map[string]*macroDef // macros defined in this template
	imports []importSpec         // {% from "x" import a, b %}
}

func parse(src string) (*parseResult, error) {
	toks := lex(src)
	p := &parser{toks: toks, blocks: map[string][]node{}, macros: map[string]*macroDef{}}
	nodes, err := p.parseUntil(nil)
	if err != nil {
		return nil, err
	}
	return &parseResult{nodes: nodes, extends: p.extends, blocks: p.blocks, macros: p.macros, imports: p.imports}, nil
}

type parser struct {
	toks    []token
	pos     int
	extends string
	blocks  map[string][]node
	macros  map[string]*macroDef
	imports []importSpec
}

// parseUntil parses nodes until it hits one of the stop keywords (e.g. "endif","else").
// Returns the collected nodes; leaves pos pointing AT the stop tag.
func (p *parser) parseUntil(stops []string) ([]node, error) {
	var nodes []node
	for p.pos < len(p.toks) {
		t := p.toks[p.pos]
		switch t.kind {
		case tokText:
			nodes = append(nodes, textNode{t.text})
			p.pos++
		case tokComment:
			nodes = append(nodes, commentNode{})
			p.pos++
		case tokOutput:
			nodes = append(nodes, outputNode{t.text})
			p.pos++
		case tokTag:
			kw := firstWord(t.text)
			if contains(stops, kw) {
				return nodes, nil
			}
			n, err := p.parseTag(t.text, kw)
			if err != nil {
				return nil, err
			}
			if n != nil {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, nil
}

func (p *parser) parseTag(text, kw string) (node, error) {
	switch kw {
	case "extends":
		p.extends = unquote(strings.TrimSpace(strings.TrimPrefix(text, "extends")))
		p.pos++
		return nil, nil
	case "include":
		p.pos++
		return includeNode{name: unquote(strings.TrimSpace(strings.TrimPrefix(text, "include")))}, nil
	case "set":
		rest := strings.TrimSpace(strings.TrimPrefix(text, "set"))
		if eq := topLevelAssign(rest); eq >= 0 {
			p.pos++
			return setNode{name: strings.TrimSpace(rest[:eq]), expr: strings.TrimSpace(rest[eq+1:])}, nil
		}
		// block form: {% set name %}...{% endset %} -> capture rendered body
		p.pos++
		body, err := p.parseUntil([]string{"endset"})
		if err != nil {
			return nil, err
		}
		p.pos++ // endset
		return setBlockNode{name: rest, body: body}, nil
	case "if":
		return p.parseIf(text)
	case "for":
		return p.parseFor(text)
	case "with":
		return p.parseWith(text)
	case "block":
		return p.parseBlock(text)
	case "from":
		return p.parseFrom(text)
	case "import":
		// {% import "x" as y %} — not used by current templates; record nothing.
		p.pos++
		return nil, nil
	case "macro":
		return p.parseMacro(text)
	case "call":
		return p.parseCall(text)
	default:
		// {% call(params) macro() %} — no space after `call`, so kw is "call(...)".
		if strings.HasPrefix(text, "call(") {
			return p.parseCall(text)
		}
		return nil, fmt.Errorf("unsupported tag: %q", text)
	}
}

// parseFrom handles {% from "tpl" import a, b as c %} (the `as` alias is rare; we keep
// the original name).
func (p *parser) parseFrom(text string) (node, error) {
	p.pos++
	rest := strings.TrimSpace(strings.TrimPrefix(text, "from"))
	idx := strings.Index(rest, " import ")
	if idx < 0 {
		return nil, fmt.Errorf("bad from: %q", text)
	}
	tpl := unquote(strings.TrimSpace(rest[:idx]))
	var names []string
	for _, n := range strings.Split(rest[idx+len(" import "):], ",") {
		n = strings.TrimSpace(n)
		if sp := strings.Index(n, " as "); sp >= 0 {
			n = strings.TrimSpace(n[:sp])
		}
		if n != "" {
			names = append(names, n)
		}
	}
	p.imports = append(p.imports, importSpec{template: tpl, names: names})
	return nil, nil
}

func (p *parser) parseMacro(text string) (node, error) {
	inner := strings.TrimSpace(strings.TrimPrefix(text, "macro"))
	lp := strings.IndexByte(inner, '(')
	name := strings.TrimSpace(inner[:lp])
	rp := strings.LastIndexByte(inner, ')')
	params := parseParams(inner[lp+1 : rp])
	p.pos++
	body, err := p.parseUntil([]string{"endmacro"})
	if err != nil {
		return nil, err
	}
	p.pos++ // endmacro
	p.macros[name] = &macroDef{name: name, params: params, body: body}
	return nil, nil
}

// parseParams parses a macro parameter list: name, name='default', name="d".
func parseParams(s string) []macroParam {
	var out []macroParam
	for _, part := range splitTop(s, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if eq := topLevelAssign(part); eq >= 0 {
			out = append(out, macroParam{name: strings.TrimSpace(part[:eq]), def: strings.TrimSpace(part[eq+1:])})
		} else {
			out = append(out, macroParam{name: part})
		}
	}
	return out
}

func (p *parser) parseIf(text string) (node, error) {
	n := &ifNode{}
	cond := strings.TrimSpace(strings.TrimPrefix(text, "if"))
	p.pos++ // consume {% if %}
	for {
		body, err := p.parseUntil([]string{"elif", "else", "endif"})
		if err != nil {
			return nil, err
		}
		n.branches = append(n.branches, ifBranch{cond: cond, body: body})
		stop := p.toks[p.pos].text
		kw := firstWord(stop)
		switch kw {
		case "elif":
			cond = strings.TrimSpace(strings.TrimPrefix(stop, "elif"))
			p.pos++
		case "else":
			p.pos++
			body, err := p.parseUntil([]string{"endif"})
			if err != nil {
				return nil, err
			}
			n.branches = append(n.branches, ifBranch{cond: "", body: body})
			p.pos++ // endif
			return n, nil
		case "endif":
			p.pos++
			return n, nil
		}
	}
}

// parseCall handles {% call [(p1,p2)] macroname(args) %}body{% endcall %} — invokes the
// macro with `caller()` bound to the rendered body (optionally taking the params).
func (p *parser) parseCall(text string) (node, error) {
	inner := strings.TrimSpace(strings.TrimPrefix(text, "call"))
	var callerParams []string
	if strings.HasPrefix(inner, "(") {
		if end := strings.IndexByte(inner, ')'); end >= 0 {
			callerParams = splitVars(inner[1:end])
			inner = strings.TrimSpace(inner[end+1:])
		}
	}
	n := &callNode{callerParams: callerParams, macroExpr: inner}
	p.pos++
	body, err := p.parseUntil([]string{"endcall"})
	if err != nil {
		return nil, err
	}
	n.body = body
	p.pos++ // endcall
	return n, nil
}

func (p *parser) parseFor(text string) (node, error) {
	inner := strings.TrimSpace(strings.TrimPrefix(text, "for"))
	parts := strings.SplitN(inner, " in ", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("bad for: %q", text)
	}
	vars := splitVars(parts[0])
	n := &forNode{vars: vars, iter: strings.TrimSpace(parts[1])}
	p.pos++
	body, err := p.parseUntil([]string{"else", "endfor"})
	if err != nil {
		return nil, err
	}
	n.body = body
	if firstWord(p.toks[p.pos].text) == "else" {
		p.pos++
		eb, err := p.parseUntil([]string{"endfor"})
		if err != nil {
			return nil, err
		}
		n.elseBody = eb
	}
	p.pos++ // endfor
	return n, nil
}

func (p *parser) parseWith(text string) (node, error) {
	inner := strings.TrimSpace(strings.TrimPrefix(text, "with"))
	n := &withNode{}
	if inner != "" {
		// support a single assignment (the only form base.html uses)
		if eq := strings.Index(inner, "="); eq >= 0 {
			n.assigns = append(n.assigns, [2]string{strings.TrimSpace(inner[:eq]), strings.TrimSpace(inner[eq+1:])})
		}
	}
	p.pos++
	body, err := p.parseUntil([]string{"endwith"})
	if err != nil {
		return nil, err
	}
	n.body = body
	p.pos++ // endwith
	return n, nil
}

func (p *parser) parseBlock(text string) (node, error) {
	name := strings.TrimSpace(strings.TrimPrefix(text, "block"))
	p.pos++
	body, err := p.parseUntil([]string{"endblock"})
	if err != nil {
		return nil, err
	}
	p.pos++ // endblock (may be {% endblock name %})
	p.blocks[name] = body
	return blockNode{name: name, body: body}, nil
}

// helpers
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func splitVars(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
