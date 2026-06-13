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

type blockNode struct {
	name string
	body []node
}

type includeNode struct{ name string }

type extendsNode struct{ name string }

// parseResult is the parsed template: its node list plus, if it extends a parent, the
// parent name (the top-level nodes of an extending child are only its block overrides).
type parseResult struct {
	nodes   []node
	extends string
	blocks  map[string][]node // block name -> body (collected from this template)
}

func parse(src string) (*parseResult, error) {
	toks := lex(src)
	p := &parser{toks: toks, blocks: map[string][]node{}}
	nodes, err := p.parseUntil(nil)
	if err != nil {
		return nil, err
	}
	return &parseResult{nodes: nodes, extends: p.extends, blocks: p.blocks}, nil
}

type parser struct {
	toks    []token
	pos     int
	extends string
	blocks  map[string][]node
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
		p.pos++
		rest := strings.TrimSpace(strings.TrimPrefix(text, "set"))
		if eq := strings.Index(rest, "="); eq >= 0 {
			return setNode{name: strings.TrimSpace(rest[:eq]), expr: strings.TrimSpace(rest[eq+1:])}, nil
		}
		return nil, fmt.Errorf("bad set: %q", text)
	case "if":
		return p.parseIf(text)
	case "for":
		return p.parseFor(text)
	case "with":
		return p.parseWith(text)
	case "block":
		return p.parseBlock(text)
	default:
		return nil, fmt.Errorf("unsupported tag: %q", text)
	}
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
