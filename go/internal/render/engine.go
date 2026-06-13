package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Func is a template-callable global (url_for, get_flashed_messages, signal_label, …).
// It receives positional and keyword args already evaluated.
type Func func(pos []any, kw map[string]any) (any, error)

// Engine loads and renders Jinja templates from a directory.
type Engine struct {
	dir   string
	cache map[string]*parseResult
	funcs map[string]Func
}

func New(dir string) *Engine {
	return &Engine{dir: dir, cache: map[string]*parseResult{}, funcs: map[string]Func{}}
}

// AddFunc registers a template-callable global.
func (e *Engine) AddFunc(name string, f Func) { e.funcs[name] = f }

func (e *Engine) load(name string) (*parseResult, error) {
	if pr, ok := e.cache[name]; ok {
		return pr, nil
	}
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		return nil, err
	}
	// Jinja keep_trailing_newline defaults to False: strip a single trailing newline
	// (\n or \r\n) from each template before compiling.
	src := string(b)
	if strings.HasSuffix(src, "\r\n") {
		src = src[:len(src)-2]
	} else if strings.HasSuffix(src, "\n") {
		src = src[:len(src)-1]
	}
	pr, err := parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	e.cache[name] = pr
	return pr, nil
}

// scope is a chained variable environment.
type scope struct {
	vars   map[string]any
	parent *scope
}

func newScope(parent *scope) *scope { return &scope{vars: map[string]any{}, parent: parent} }

func (s *scope) lookup(name string) any {
	for c := s; c != nil; c = c.parent {
		if v, ok := c.vars[name]; ok {
			return v
		}
	}
	return nil
}

func (s *scope) lookupDotted(path string) any {
	parts := strings.Split(path, ".")
	cur := s.lookup(parts[0])
	for _, p := range parts[1:] {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// Render renders template `name` with the given context map, resolving inheritance.
func (e *Engine) Render(name string, ctx map[string]any) (string, error) {
	// Build the inheritance chain (root parent first .. leaf child last).
	var chain []*parseResult
	cur := name
	for cur != "" {
		pr, err := e.load(cur)
		if err != nil {
			return "", err
		}
		chain = append([]*parseResult{pr}, chain...) // prepend: root ends up first
		cur = pr.extends
	}

	// Collect block override chains: name -> bodies ordered root..leaf.
	blockChains := map[string][][]node{}
	for _, pr := range chain {
		for bn, body := range pr.blocks {
			blockChains[bn] = append(blockChains[bn], body)
		}
	}

	root := chain[0]
	rc := &renderCtx{engine: e, blockChains: blockChains, superDepth: map[string]int{}}
	top := newScope(nil)
	for k, v := range ctx {
		top.vars[k] = v
	}
	var sb strings.Builder
	if err := rc.renderNodes(root.nodes, top, &sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

type renderCtx struct {
	engine      *Engine
	blockChains map[string][][]node
	superDepth  map[string]int // per-block current index into its chain (for super())
}

func (rc *renderCtx) renderNodes(nodes []node, sc *scope, sb *strings.Builder) error {
	for _, n := range nodes {
		if err := rc.renderNode(n, sc, sb); err != nil {
			return err
		}
	}
	return nil
}

func (rc *renderCtx) renderNode(n node, sc *scope, sb *strings.Builder) error {
	switch x := n.(type) {
	case textNode:
		sb.WriteString(x.text)
	case commentNode:
		// nothing
	case outputNode:
		val, err := rc.engine.evalExpr(x.expr, sc)
		if err != nil {
			return err
		}
		sb.WriteString(renderOutput(val))
	case setNode:
		val, err := rc.engine.evalExpr(x.expr, sc)
		if err != nil {
			return err
		}
		sc.vars[x.name] = val
	case ifNode:
		for _, br := range x.branches {
			if br.cond == "" { // else
				return rc.renderNodes(br.body, sc, sb)
			}
			v, err := rc.engine.evalExpr(br.cond, sc)
			if err != nil {
				return err
			}
			if !isFalsey(v) {
				return rc.renderNodes(br.body, sc, sb)
			}
		}
	case *ifNode:
		return rc.renderNode(*x, sc, sb)
	case forNode:
		return rc.renderFor(&x, sc, sb)
	case *forNode:
		return rc.renderFor(x, sc, sb)
	case withNode:
		return rc.renderWith(&x, sc, sb)
	case *withNode:
		return rc.renderWith(x, sc, sb)
	case blockNode:
		return rc.renderBlock(x.name, x.body, sc, sb)
	case includeNode:
		pr, err := rc.engine.load(x.name)
		if err != nil {
			return err
		}
		return rc.renderNodes(pr.nodes, newScope(sc), sb)
	default:
		return fmt.Errorf("cannot render node %T", n)
	}
	return nil
}

func (rc *renderCtx) renderBlock(name string, defBody []node, sc *scope, sb *strings.Builder) error {
	chain := rc.blockChains[name]
	if len(chain) == 0 {
		return rc.renderNodes(defBody, newScope(sc), sb)
	}
	// most-derived is the last in the chain
	return rc.renderBlockAt(name, chain, len(chain)-1, sc, sb)
}

// renderBlockAt renders the block body at chain index idx, exposing super() as the body
// at idx-1 (or empty when idx==0).
func (rc *renderCtx) renderBlockAt(name string, chain [][]node, idx int, sc *scope, sb *strings.Builder) error {
	inner := newScope(sc)
	// Pre-render super() content (the next-less-derived definition).
	superStr := ""
	if idx > 0 {
		var ssb strings.Builder
		if err := rc.renderBlockAt(name, chain, idx-1, sc, &ssb); err != nil {
			return err
		}
		superStr = ssb.String()
	}
	inner.vars["__super__"] = safeString{superStr}
	return rc.renderNodes(chain[idx], inner, sb)
}

func (rc *renderCtx) renderFor(n *forNode, sc *scope, sb *strings.Builder) error {
	it, err := rc.engine.evalExpr(n.iter, sc)
	if err != nil {
		return err
	}
	items := toList(it)
	if len(items) == 0 {
		return rc.renderNodes(n.elseBody, newScope(sc), sb)
	}
	for i, item := range items {
		inner := newScope(sc)
		bindLoopVars(inner, n.vars, item)
		inner.vars["loop"] = map[string]any{
			"index": i + 1, "index0": i, "first": i == 0, "last": i == len(items)-1, "length": len(items),
		}
		if err := rc.renderNodes(n.body, inner, sb); err != nil {
			return err
		}
	}
	return nil
}

func (rc *renderCtx) renderWith(n *withNode, sc *scope, sb *strings.Builder) error {
	inner := newScope(sc)
	for _, a := range n.assigns {
		v, err := rc.engine.evalExpr(a[1], sc)
		if err != nil {
			return err
		}
		inner.vars[a[0]] = v
	}
	return rc.renderNodes(n.body, inner, sb)
}

func bindLoopVars(sc *scope, vars []string, item any) {
	if len(vars) == 1 {
		sc.vars[vars[0]] = item
		return
	}
	// tuple unpacking
	if tup, ok := item.([]any); ok {
		for i, v := range vars {
			if i < len(tup) {
				sc.vars[v] = tup[i]
			}
		}
	}
}

func toList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	default:
		return []any{x}
	}
}
