package render

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// safeString marks a value as already-safe HTML (the |safe filter / Markup), so {{ }}
// output does not re-escape it.
type safeString struct{ s string }

// evalExpr evaluates a Jinja expression against the context. Layered by precedence:
// or < and < not < comparison(==,!=,in,not in) < filter(|) < atom. Supports literals,
// names, dotted attribute access, function/method calls with positional+keyword args,
// tuple/list literals, and the template filters the SOBS templates use.
func (e *Engine) evalExpr(expr string, ctx *scope) (any, error) {
	return e.evalOr(strings.TrimSpace(expr), ctx)
}

func (e *Engine) evalOr(s string, ctx *scope) (any, error) {
	parts := splitTopWord(s, "or")
	if len(parts) == 1 {
		return e.evalAnd(parts[0], ctx)
	}
	var last any
	for _, p := range parts {
		v, err := e.evalAnd(strings.TrimSpace(p), ctx)
		if err != nil {
			return nil, err
		}
		if !isFalsey(v) {
			return v, nil
		}
		last = v
	}
	return last, nil
}

func (e *Engine) evalAnd(s string, ctx *scope) (any, error) {
	parts := splitTopWord(s, "and")
	if len(parts) == 1 {
		return e.evalNot(parts[0], ctx)
	}
	var last any
	for _, p := range parts {
		v, err := e.evalNot(strings.TrimSpace(p), ctx)
		if err != nil {
			return nil, err
		}
		if isFalsey(v) {
			return v, nil
		}
		last = v
	}
	return last, nil
}

func (e *Engine) evalNot(s string, ctx *scope) (any, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "not "); ok {
		v, err := e.evalNot(strings.TrimSpace(rest), ctx)
		if err != nil {
			return nil, err
		}
		return isFalsey(v), nil
	}
	return e.evalCompare(s, ctx)
}

func (e *Engine) evalCompare(s string, ctx *scope) (any, error) {
	s = strings.TrimSpace(s)
	// 'not in' first (longer operator), then 'in', '==', '!='
	if l, r, ok := splitTopOp(s, " not in "); ok {
		return e.membership(l, r, ctx, true)
	}
	if l, r, ok := splitTopOp(s, " in "); ok {
		return e.membership(l, r, ctx, false)
	}
	if l, r, ok := splitTopOp(s, " == "); ok {
		return e.compareEq(l, r, ctx, true)
	}
	if l, r, ok := splitTopOp(s, " != "); ok {
		return e.compareEq(l, r, ctx, false)
	}
	return e.evalFiltered(s, ctx)
}

func (e *Engine) membership(l, r string, ctx *scope, negate bool) (any, error) {
	lv, err := e.evalFiltered(strings.TrimSpace(l), ctx)
	if err != nil {
		return nil, err
	}
	rv, err := e.evalFiltered(strings.TrimSpace(r), ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, item := range toList(rv) {
		if equalValues(lv, item) {
			found = true
			break
		}
	}
	return found != negate, nil
}

func (e *Engine) compareEq(l, r string, ctx *scope, want bool) (any, error) {
	lv, err := e.evalFiltered(strings.TrimSpace(l), ctx)
	if err != nil {
		return nil, err
	}
	rv, err := e.evalFiltered(strings.TrimSpace(r), ctx)
	if err != nil {
		return nil, err
	}
	return equalValues(lv, rv) == want, nil
}

// evalFiltered handles an atom followed by | filters.
func (e *Engine) evalFiltered(expr string, ctx *scope) (any, error) {
	expr = strings.TrimSpace(expr)
	parts := splitTop(expr, '|')
	val, err := e.evalAtom(strings.TrimSpace(parts[0]), ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range parts[1:] {
		val, err = e.applyFilter(strings.TrimSpace(f), val, ctx)
		if err != nil {
			return nil, err
		}
	}
	return val, nil
}

func (e *Engine) evalAtom(s string, ctx *scope) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// string literal
	if (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1], nil
	}
	// super() inside an overridden block -> the pre-rendered parent block content.
	if s == "super()" {
		if v := ctx.lookup("__super__"); v != nil {
			return v, nil
		}
		return safeString{""}, nil
	}
	// parenthesized: grouped expression or tuple literal
	if s[0] == '(' && s[len(s)-1] == ')' {
		inner := s[1 : len(s)-1]
		elems := splitTop(inner, ',')
		// a trailing comma or multiple elems => tuple; otherwise a grouped expression
		if len(elems) > 1 || (len(elems) == 1 && strings.HasSuffix(strings.TrimSpace(inner), ",")) {
			return e.evalSeq(elems, ctx)
		}
		return e.evalExpr(inner, ctx)
	}
	// list literal
	if s[0] == '[' && s[len(s)-1] == ']' {
		return e.evalSeq(splitTop(s[1:len(s)-1], ','), ctx)
	}
	// dict literal {'k': v, ...}
	if s[0] == '{' && s[len(s)-1] == '}' {
		out := map[string]any{}
		for _, pair := range splitTop(s[1:len(s)-1], ',') {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			c := splitTop(pair, ':')
			if len(c) != 2 {
				return nil, fmt.Errorf("bad dict entry %q", pair)
			}
			kv, err := e.evalExpr(strings.TrimSpace(c[0]), ctx)
			if err != nil {
				return nil, err
			}
			vv, err := e.evalExpr(strings.TrimSpace(c[1]), ctx)
			if err != nil {
				return nil, err
			}
			out[toString(kv)] = vv
		}
		return out, nil
	}
	// subscript base[index] (not a list literal: '[' is not at position 0)
	if s[len(s)-1] == ']' {
		if open := matchingSubscript(s); open > 0 {
			base, err := e.evalAtom(strings.TrimSpace(s[:open]), ctx)
			if err != nil {
				return nil, err
			}
			idx, err := e.evalExpr(strings.TrimSpace(s[open+1:len(s)-1]), ctx)
			if err != nil {
				return nil, err
			}
			return subscript(base, idx), nil
		}
	}
	// bool / none
	switch s {
	case "true", "True":
		return true, nil
	case "false", "False":
		return false, nil
	case "none", "None":
		return nil, nil
	}
	// int literal
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	// call: name(args) — name may be dotted (method call, e.g. config.get(...))
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		name := strings.TrimSpace(s[:i])
		argstr := s[i+1 : len(s)-1]
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			return e.callMethod(name[:dot], name[dot+1:], argstr, ctx)
		}
		return e.callFunc(name, argstr, ctx)
	}
	// attribute access a.b (request.endpoint, loop.index, …)
	if strings.Contains(s, ".") && !strings.ContainsAny(s, "('\"") {
		return ctx.lookupDotted(s), nil
	}
	// bare name
	return ctx.lookup(s), nil
}

// evalSeq evaluates a comma-separated list of expressions into a []any (tuple/list).
func (e *Engine) evalSeq(elems []string, ctx *scope) (any, error) {
	var out []any
	for _, el := range elems {
		el = strings.TrimSpace(el)
		if el == "" {
			continue
		}
		v, err := e.evalExpr(el, ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// callMethod handles the small set of object methods templates use, currently
// `dict.get(key, default)` on a map (e.g. config.get('ENABLE_FIRST_RUN_TOUR', True)).
func (e *Engine) callMethod(objExpr, method, argstr string, ctx *scope) (any, error) {
	obj, err := e.evalExpr(objExpr, ctx)
	if err != nil {
		return nil, err
	}
	args, err := e.evalSeq(splitTop(argstr, ','), ctx)
	if err != nil {
		return nil, err
	}
	argList, _ := args.([]any)
	m, ok := obj.(map[string]any)
	if method == "get" && ok {
		var key string
		if len(argList) > 0 {
			key, _ = argList[0].(string)
		}
		if v, present := m[key]; present {
			return v, nil
		}
		if len(argList) > 1 {
			return argList[1], nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported method %q on %T", method, obj)
}

func equalValues(a, b any) bool {
	return toString(a) == toString(b) && atomKind(a) == atomKind(b)
}

// atomKind buckets values so "1"==1 doesn't accidentally match across types in ==.
func atomKind(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case int:
		return 2
	default:
		return 3 // string-ish
	}
}

func (e *Engine) callFunc(name, argstr string, ctx *scope) (any, error) {
	var pos []any
	kw := map[string]any{}
	for _, a := range splitTop(argstr, ',') {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if eq := topLevelAssign(a); eq >= 0 {
			key := strings.TrimSpace(a[:eq])
			v, err := e.evalExpr(strings.TrimSpace(a[eq+1:]), ctx)
			if err != nil {
				return nil, err
			}
			kw[key] = v
		} else {
			v, err := e.evalExpr(a, ctx)
			if err != nil {
				return nil, err
			}
			pos = append(pos, v)
		}
	}
	if m, ok := e.macros[name]; ok {
		return e.invokeMacro(m, pos, kw, ctx)
	}
	fn, ok := e.funcs[name]
	if !ok {
		return nil, fmt.Errorf("unknown function %q", name)
	}
	return fn(pos, kw)
}

// invokeMacro renders a macro body with its parameters bound (positional, then keyword,
// then defaults). Jinja imported macros don't see the caller's locals, so the body runs
// in a fresh scope containing only the parameters. The result is safe (Markup).
func (e *Engine) invokeMacro(m *macroDef, pos []any, kw map[string]any, ctx *scope) (any, error) {
	sc := newScope(nil)
	for i, p := range m.params {
		switch {
		case i < len(pos):
			sc.vars[p.name] = pos[i]
		case kw[p.name] != nil:
			sc.vars[p.name] = kw[p.name]
		case p.def != "":
			v, err := e.evalExpr(p.def, ctx)
			if err != nil {
				return nil, err
			}
			sc.vars[p.name] = v
		default:
			sc.vars[p.name] = ""
		}
	}
	// allow keyword args that match params even when the param's default would apply
	for k, v := range kw {
		sc.vars[k] = v
	}
	var sb strings.Builder
	if err := e.activeRC.renderNodes(m.body, sc, &sb); err != nil {
		return nil, err
	}
	return safeString{sb.String()}, nil
}

// matchingSubscript returns the index of the '[' that matches the trailing ']' of s, or
// -1. Used to detect base[index] subscripts (index 0 means it's a list literal instead).
func matchingSubscript(s string) int {
	depth := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ']':
			depth++
		case '[':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// subscript indexes a map by string key or a list by integer index.
func subscript(base, idx any) any {
	switch b := base.(type) {
	case map[string]any:
		return b[toString(idx)]
	case []any:
		n, ok := idx.(int)
		if !ok {
			if ni, err := strconv.Atoi(toString(idx)); err == nil {
				n, ok = ni, true
			}
		}
		if ok && n >= 0 && n < len(b) {
			return b[n]
		}
	}
	return nil
}

// applyFilter implements the template filters used by the SOBS templates.
func (e *Engine) applyFilter(f string, val any, ctx *scope) (any, error) {
	name := f
	var argstr string
	if i := strings.IndexByte(f, '('); i >= 0 && strings.HasSuffix(f, ")") {
		name = strings.TrimSpace(f[:i])
		argstr = f[i+1 : len(f)-1]
	}
	switch name {
	case "safe":
		return safeString{toString(val)}, nil
	case "e", "escape":
		return safeString{escapeHTML(toString(val))}, nil
	case "tojson":
		// Jinja tojson: compact JSON + HTML-safe escaping, marked safe.
		return safeString{tojson(val)}, nil
	case "default":
		if isFalsey(val) && argstr != "" {
			d, err := e.evalExpr(argstr, ctx)
			if err != nil {
				return nil, err
			}
			return d, nil
		}
		return val, nil
	case "length", "count":
		return lengthOf(val), nil
	default:
		return nil, fmt.Errorf("unsupported filter %q", name)
	}
}

// renderOutput converts a {{ }} value to its output string, applying autoescape unless
// the value is already safe.
func renderOutput(val any) string {
	if ss, ok := val.(safeString); ok {
		return ss.s
	}
	return escapeHTML(toString(val))
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case safeString:
		return x.s
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// tojson reproduces Jinja's tojson: json.dumps(compact) then HTML-escape <>&' to \uXXXX.
func tojson(v any) string {
	raw := string(jsonenc.Encode(toJSONValue(v), jsonenc.Compact))
	r := strings.NewReplacer(
		"<", `<`,
		">", `>`,
		"&", `&`,
		"'", `'`,
	)
	return r.Replace(raw)
}

// toJSONValue coerces interpreter values into types jsonenc understands.
func toJSONValue(v any) any {
	switch x := v.(type) {
	case safeString:
		return x.s
	default:
		return x
	}
}

func isFalsey(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case safeString:
		return x.s == ""
	case int:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

func lengthOf(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		return len(x)
	case map[string]any:
		return len(x)
	default:
		return 0
	}
}

// splitTop splits s on sep at the top level (not inside quotes or parentheses).
func splitTop(s string, sep byte) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// splitTopWord splits s on the keyword `word` when it appears as a standalone token at
// the top level (surrounded by spaces, not inside quotes/parens). Returns the segments.
func splitTopWord(s, word string) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	target := " " + word + " "
	for i := 0; i+len(target) <= len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			continue
		case c == '\'' || c == '"':
			quote = c
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
			continue
		case c == ')' || c == ']' || c == '}':
			depth--
			continue
		}
		if depth == 0 && s[i:i+len(target)] == target {
			out = append(out, s[start:i])
			start = i + len(target)
			i += len(target) - 1
		}
	}
	out = append(out, s[start:])
	return out
}

// splitTopOp finds the first top-level occurrence of op (e.g. " == ", " in ") and returns
// (left, right, true). Quotes and parens are skipped.
func splitTopOp(s, op string) (string, string, bool) {
	depth := 0
	var quote byte
	for i := 0; i+len(op) <= len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			continue
		case c == '\'' || c == '"':
			quote = c
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
			continue
		case c == ')' || c == ']' || c == '}':
			depth--
			continue
		}
		if depth == 0 && s[i:i+len(op)] == op {
			return s[:i], s[i+len(op):], true
		}
	}
	return "", "", false
}

// topLevelAssign returns the index of a top-level '=' that is a kwarg assignment (not
// '==', '<=', etc.), or -1.
func topLevelAssign(s string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == '=' && depth == 0:
			if (i == 0 || s[i-1] != '=' && s[i-1] != '!' && s[i-1] != '<' && s[i-1] != '>') &&
				(i+1 >= len(s) || s[i+1] != '=') {
				return i
			}
		}
	}
	return -1
}
