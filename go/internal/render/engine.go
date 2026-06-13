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
	dir      string
	cache    map[string]*parseResult
	funcs    map[string]Func
	macros   map[string]*macroDef // active macro registry (per Render)
	activeRC *renderCtx           // active render context (for macro-body rendering)
	kwOrder  []string             // keyword-arg order of the in-flight callFunc (for url_for)
}

// KWOrder returns the keyword-arg order of the function call currently being dispatched —
// used by url_for to emit leftover kwargs as query params in insertion order (Quart).
func (e *Engine) KWOrder() []string { return e.kwOrder }

func New(dir string) *Engine {
	e := &Engine{dir: dir, cache: map[string]*parseResult{}, funcs: map[string]Func{}}
	// Jinja builtin: namespace(**kw) returns a mutable attribute container. Modeled as a
	// map[string]any (Go maps are references, so `{% set ns.x = v %}` mutations persist).
	e.funcs["namespace"] = func(pos []any, kw map[string]any) (any, error) {
		ns := map[string]any{}
		for k, v := range kw {
			ns[k] = v
		}
		return ns, nil
	}
	return e
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

	// Collect the macro registry: every template's own macros plus those it imports
	// via {% from "x" import ... %}.
	macros := map[string]*macroDef{}
	for _, pr := range chain {
		for mn, md := range pr.macros {
			macros[mn] = md
		}
		for _, imp := range pr.imports {
			ipr, err := e.load(imp.template)
			if err != nil {
				return "", err
			}
			// Register the explicitly-imported names AND every sibling macro in the same
			// file: in Jinja, macros within a module can call one another, so an imported
			// macro's body may invoke a sibling that wasn't named in the import.
			for mn, md := range ipr.macros {
				if _, exists := macros[mn]; !exists {
					macros[mn] = md
				}
			}
			for _, mn := range imp.names {
				if md, ok := ipr.macros[mn]; ok {
					macros[mn] = md
				}
			}
		}
	}
	e.macros = macros

	root := chain[0]
	rc := &renderCtx{engine: e, blockChains: blockChains, superDepth: map[string]int{}}
	e.activeRC = rc
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
		// Dotted target ({% set ns.found = true %}): mutate the attribute of the
		// (map-backed) object — e.g. a namespace. Falls back to a plain var otherwise.
		if dot := strings.LastIndexByte(x.name, '.'); dot >= 0 {
			if m, ok := sc.lookupDotted(x.name[:dot]).(map[string]any); ok {
				m[x.name[dot+1:]] = val
				return nil
			}
		}
		sc.vars[x.name] = val
	case setBlockNode:
		var bb strings.Builder
		if err := rc.renderNodes(x.body, sc, &bb); err != nil {
			return err
		}
		sc.vars[x.name] = safeString{bb.String()}
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
	case *callNode:
		return rc.renderCall(x, sc, sb)
	case callNode:
		return rc.renderCall(&x, sc, sb)
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

// renderCall implements {% call %}: bind caller() to the block body (rendered in a child
// of the call-site scope with the caller params), then invoke the macro expression.
func (rc *renderCtx) renderCall(n *callNode, sc *scope, sb *strings.Builder) error {
	e := rc.engine
	prev, had := e.funcs["caller"]
	e.funcs["caller"] = func(pos []any, kw map[string]any) (any, error) {
		cs := newScope(sc)
		for i, p := range n.callerParams {
			if i < len(pos) {
				cs.vars[p] = pos[i]
			}
		}
		var bb strings.Builder
		if err := rc.renderNodes(n.body, cs, &bb); err != nil {
			return nil, err
		}
		return safeString{bb.String()}, nil
	}
	val, err := e.evalExpr(n.macroExpr, sc)
	if had {
		e.funcs["caller"] = prev
	} else {
		delete(e.funcs, "caller")
	}
	if err != nil {
		return err
	}
	sb.WriteString(renderOutput(val))
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
