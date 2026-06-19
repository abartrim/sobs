package render

import (
	"fmt"
	"math"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

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
	return e.evalTernary(strings.TrimSpace(expr), ctx)
}

// evalTernary handles Jinja's inline conditional `A if COND [else B]` (lowest precedence).
// With no else and a false condition, the result is undefined -> empty string.
func (e *Engine) evalTernary(s string, ctx *scope) (any, error) {
	l, r, ok := splitTopOp(s, " if ")
	if !ok {
		return e.evalOr(s, ctx)
	}
	cond, falseExpr := r, ""
	if c, f, ok2 := splitTopOp(r, " else "); ok2 {
		cond, falseExpr = c, f
	}
	cv, err := e.evalOr(strings.TrimSpace(cond), ctx)
	if err != nil {
		return nil, err
	}
	if !isFalsey(cv) {
		return e.evalOr(strings.TrimSpace(l), ctx)
	}
	if strings.TrimSpace(falseExpr) == "" {
		return "", nil
	}
	return e.evalTernary(strings.TrimSpace(falseExpr), ctx)
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
	// Jinja `is`/`is not` tests (checked before in/==): x is [not] none|defined|string|…
	if l, r, ok := splitTopOp(s, " is not "); ok {
		return e.isTest(l, r, ctx, true)
	}
	if l, r, ok := splitTopOp(s, " is "); ok {
		return e.isTest(l, r, ctx, false)
	}
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
	// Ordering comparisons (numeric). Longer operators first so ` > ` does not match inside ` >= `.
	for _, op := range []string{" >= ", " <= ", " > ", " < "} {
		if l, r, ok := splitTopOp(s, op); ok {
			return e.compareOrd(l, r, ctx, strings.TrimSpace(op))
		}
	}
	return e.evalAddSub(s, ctx)
}

// compareOrd evaluates a numeric ordering comparison (>, <, >=, <=); each side runs through
// the full filter pipeline first so `x|length > 1` parses as `(x|length) > 1`.
func (e *Engine) compareOrd(l, r string, ctx *scope, op string) (any, error) {
	lv, err := e.evalAddSub(strings.TrimSpace(l), ctx)
	if err != nil {
		return nil, err
	}
	rv, err := e.evalAddSub(strings.TrimSpace(r), ctx)
	if err != nil {
		return nil, err
	}
	lf, lok := numFloat(lv)
	rf, rok := numFloat(rv)
	if lok && rok {
		switch op {
		case ">=":
			return lf >= rf, nil
		case "<=":
			return lf <= rf, nil
		case ">":
			return lf > rf, nil
		default: // "<"
			return lf < rf, nil
		}
	}
	// Non-numeric: Python compares two strings lexicographically (str >= str), used e.g. as
	// span.http_status|string >= '500'. Mixed number/string would TypeError in Python; treat
	// as false rather than raise.
	ls, lsok := orderString(lv)
	rs, rsok := orderString(rv)
	if lsok && rsok {
		switch op {
		case ">=":
			return ls >= rs, nil
		case "<=":
			return ls <= rs, nil
		case ">":
			return ls > rs, nil
		default: // "<"
			return ls < rs, nil
		}
	}
	return false, nil
}

// orderString extracts a comparable string from a string or safeString value.
func orderString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case safeString:
		return x.s, true
	}
	return "", false
}

// isTest implements Jinja `x is TEST` / `x is not TEST` for the tests templates use.
func (e *Engine) isTest(l, test string, ctx *scope, negate bool) (any, error) {
	lv, err := e.evalAddSub(strings.TrimSpace(l), ctx)
	if err != nil {
		return nil, err
	}
	var result bool
	switch strings.TrimSpace(test) {
	case "none", "None", "undefined":
		result = lv == nil
	case "defined":
		result = lv != nil
	case "string":
		_, result = lv.(string)
	case "mapping":
		// Dict-like: both a plain map and the order-preserving Object count (handlers pass
		// *jsonenc.Object for object-shaped data).
		if _, ok := lv.(map[string]any); ok {
			result = true
		} else if _, ok := lv.(*jsonenc.Object); ok {
			result = true
		}
	case "iterable", "sequence":
		_, isList := lv.([]any)
		_, isStr := lv.(string)
		result = isList || isStr
	default:
		result = !isFalsey(lv)
	}
	return result != negate, nil
}

func (e *Engine) membership(l, r string, ctx *scope, negate bool) (any, error) {
	lv, err := e.evalAddSub(strings.TrimSpace(l), ctx)
	if err != nil {
		return nil, err
	}
	rv, err := e.evalAddSub(strings.TrimSpace(r), ctx)
	if err != nil {
		return nil, err
	}
	found := false
	switch rs := rv.(type) {
	case string:
		// Python `x in str` is substring containment, not element membership.
		found = strings.Contains(rs, toString(lv))
	case safeString:
		found = strings.Contains(rs.s, toString(lv))
	default:
		for _, item := range toList(rv) {
			if equalValues(lv, item) {
				found = true
				break
			}
		}
	}
	return found != negate, nil
}

func (e *Engine) compareEq(l, r string, ctx *scope, want bool) (any, error) {
	lv, err := e.evalAddSub(strings.TrimSpace(l), ctx)
	if err != nil {
		return nil, err
	}
	rv, err := e.evalAddSub(strings.TrimSpace(r), ctx)
	if err != nil {
		return nil, err
	}
	return equalValues(lv, rv) == want, nil
}

// evalFiltered applies | filters to an atom. Filters bind tighter than arithmetic (Jinja:
// `a + b | f` is `a + (b|f)`), so this is the level just above evalAtom — its operand is a
// single atom, not an arithmetic expression.
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

// evalAddSub handles top-level binary `+` and `-` (numeric add/sub, list concat, string
// concat). Per Jinja precedence the operand of +/- is a concat-expression: +/- bind looser
// than ~, which binds looser than * // %, which bind looser than | filters.
func (e *Engine) evalAddSub(s string, ctx *scope) (any, error) {
	s = strings.TrimSpace(s)
	terms, ops := splitAddSub(s)
	if len(ops) == 0 {
		return e.evalConcat(s, ctx)
	}
	acc, err := e.evalConcat(strings.TrimSpace(terms[0]), ctx)
	if err != nil {
		return nil, err
	}
	for i, op := range ops {
		rv, err := e.evalConcat(strings.TrimSpace(terms[i+1]), ctx)
		if err != nil {
			return nil, err
		}
		if op == '-' {
			acc = subValues(acc, rv)
		} else {
			acc = addValues(acc, rv)
		}
	}
	return acc, nil
}

// evalConcat handles Jinja's `~` string-concatenation operator. Each operand is stringified
// with Python str() (so 'g' ~ id ~ 'i' ~ loop.index -> "g7i1"). Operands are * // %
// expressions (tighter than ~).
func (e *Engine) evalConcat(s string, ctx *scope) (any, error) {
	parts := splitTop(s, '~')
	if len(parts) == 1 {
		return e.evalMulDiv(strings.TrimSpace(s), ctx)
	}
	var b strings.Builder
	for _, p := range parts {
		v, err := e.evalMulDiv(strings.TrimSpace(p), ctx)
		if err != nil {
			return nil, err
		}
		b.WriteString(pyStr(v))
	}
	return b.String(), nil
}

// evalMulDiv handles // (floor div), * and % (the integer arithmetic templates use).
func (e *Engine) evalMulDiv(s string, ctx *scope) (any, error) {
	s = strings.TrimSpace(s)
	if l, r, ok := splitTopOp(s, "//"); ok {
		lv, err := e.evalMulDiv(l, ctx)
		if err != nil {
			return nil, err
		}
		rv, err := e.evalMulDiv(r, ctx)
		if err != nil {
			return nil, err
		}
		a, b := toIntVal(lv), toIntVal(rv)
		if b == 0 {
			return 0, nil
		}
		q := a / b
		if a%b != 0 && (a < 0) != (b < 0) {
			q-- // Python floor division rounds toward negative infinity
		}
		return q, nil
	}
	if l, r, ok := splitTopOp(s, " / "); ok {
		lv, _ := e.evalMulDiv(l, ctx)
		rv, _ := e.evalMulDiv(r, ctx)
		return divValues(lv, rv), nil
	}
	if l, r, ok := splitTopOp(s, " * "); ok {
		lv, _ := e.evalMulDiv(l, ctx)
		rv, _ := e.evalMulDiv(r, ctx)
		return mulValues(lv, rv), nil
	}
	if l, r, ok := splitTopOp(s, " % "); ok {
		lv, _ := e.evalMulDiv(l, ctx)
		rv, _ := e.evalMulDiv(r, ctx)
		b := toIntVal(rv)
		if b == 0 {
			return 0, nil
		}
		return toIntVal(lv) % b, nil
	}
	return e.evalFiltered(s, ctx)
}

// matchOpenParen returns the index of the '(' matching the trailing ')' of s, or -1.
func matchOpenParen(s string) int {
	depth := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// matchCloseParen returns the index of the ')' matching the '(' at index 0, or -1.
func matchCloseParen(s string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// pyStr mirrors Python str(v) for the values templates urlencode (str/list/int/dict/...).
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *jsonenc.Object:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			vv, _ := x.Get(k)
			parts = append(parts, pyRepr(k)+": "+pyRepr(vv))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return toString(v)
	}
}

// pyRepr mirrors Python repr() for the cases inside a list/dict str() (strings get quotes).
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}
	return pyStr(v)
}

// urlQueryEscape matches urllib.parse.quote_via for query values (space -> %20, and the
// reserved chars Werkzeug escapes). Go's url.QueryEscape uses '+' for space; replace it.
func urlQueryEscape(s string) string {
	return strings.ReplaceAll(neturl.QueryEscape(s), "+", "%20")
}

func toIntVal(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func addValues(a, b any) any {
	if al, ok := a.([]any); ok {
		if bl, ok := b.([]any); ok {
			return append(append([]any{}, al...), bl...)
		}
	}
	if ai, ok := a.(int); ok {
		if bi, ok := b.(int); ok {
			return ai + bi
		}
	}
	if af, ok := numFloat(a); ok {
		if bf, ok := numFloat(b); ok {
			return af + bf
		}
	}
	return toString(a) + toString(b)
}

// subValues implements numeric `-` (int stays int, otherwise float). Templates only
// subtract numbers (e.g. offset - limit).
func subValues(a, b any) any {
	if ai, ok := a.(int); ok {
		if bi, ok := b.(int); ok {
			return ai - bi
		}
	}
	if af, ok := numFloat(a); ok {
		if bf, ok := numFloat(b); ok {
			return af - bf
		}
	}
	return 0
}

// mulValues mirrors Python `*` numeric semantics: int*int stays int, but a float operand
// yields a float (e.g. total_ms * 0.25). Mirrors addValues/subValues; the non-numeric fallback
// keeps the prior integer-coercion behavior.
func mulValues(a, b any) any {
	if ai, ok := a.(int); ok {
		if bi, ok := b.(int); ok {
			return ai * bi
		}
	}
	if af, ok := numFloat(a); ok {
		if bf, ok := numFloat(b); ok {
			return af * bf
		}
	}
	return toIntVal(a) * toIntVal(b)
}

// divValues mirrors Python 3 true division `/`: the result is ALWAYS a float (even for
// int/int, e.g. 4/2 -> 2.0), so it matches Jinja's `/` operator. Division by zero raises
// in Python; no template divides by zero (only by nonzero constants), so we guard it and
// return 0.0 to avoid a panic rather than diverge on an unreachable path.
func divValues(a, b any) any {
	bf, bok := numFloat(b)
	af, aok := numFloat(a)
	if !aok || !bok || bf == 0 {
		return 0.0
	}
	return af / bf
}

// splitAddSub splits s on top-level binary `+`/`-` (skipping quotes, brackets, and unary
// signs). Returns the terms and the operator bytes between them.
func splitAddSub(s string) ([]string, []byte) {
	var terms []string
	var ops []byte
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			continue
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			depth--
			continue
		}
		if depth != 0 || (c != '+' && c != '-') {
			continue
		}
		// Binary only if the previous non-space char ends a value (not the term start and
		// not another operator) — otherwise this +/- is a unary sign.
		j := i - 1
		for j >= start && s[j] == ' ' {
			j--
		}
		if j < start {
			continue
		}
		switch s[j] {
		case '+', '-', '*', '/', '%', '~', '(', '[', '{', ',', '<', '>', '=', '!', '|':
			continue
		}
		terms = append(terms, s[start:i])
		ops = append(ops, c)
		start = i + 1
	}
	terms = append(terms, s[start:])
	return terms, ops
}

func numFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func (e *Engine) evalAtom(s string, ctx *scope) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// string literal (process Jinja/Python escapes: \\ -> \, \n, \t, \' ...)
	if (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return unescapeStringLiteral(s[1 : len(s)-1]), nil
	}
	// super() inside an overridden block -> the pre-rendered parent block content.
	if s == "super()" {
		if v := ctx.lookup("__super__"); v != nil {
			return v, nil
		}
		return safeString{""}, nil
	}
	// parenthesized: grouped expression or tuple literal — ONLY when the opening paren
	// matches the closing one at the end (else it's e.g. (expr).method(args)).
	if s[0] == '(' && matchCloseParen(s) == len(s)-1 {
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
	// subscript base[index] / slice base[a:b] (not a list literal: '[' is not at position 0)
	if s[len(s)-1] == ']' {
		if open := matchingSubscript(s); open > 0 {
			base, err := e.evalAtom(strings.TrimSpace(s[:open]), ctx)
			if err != nil {
				return nil, err
			}
			inner := strings.TrimSpace(s[open+1 : len(s)-1])
			// Python slice: base[start:end] (start/end optional). ':' at top level signals it.
			if ci := strings.IndexByte(inner, ':'); ci >= 0 {
				start, end := strings.TrimSpace(inner[:ci]), strings.TrimSpace(inner[ci+1:])
				si, hasS := e.sliceBound(start, ctx)
				ei, hasE := e.sliceBound(end, ctx)
				return sliceValue(base, si, hasS, ei, hasE), nil
			}
			idx, err := e.evalExpr(inner, ctx)
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
	// float literal (e.g. 0.0, 1.5) — checked before dotted-attribute access
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	// call: name(args) — name may be dotted (method call) or a parenthesized object
	// (e.g. (m or {}).get(...)). Find the '(' matching the TRAILING ')'.
	if strings.HasSuffix(s, ")") {
		if open := matchOpenParen(s); open > 0 {
			name := strings.TrimSpace(s[:open])
			argstr := s[open+1 : len(s)-1]
			if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
				return e.callMethod(name[:dot], name[dot+1:], argstr, ctx)
			}
			return e.callFunc(name, argstr, ctx)
		}
	}
	// attribute access a.b (request.endpoint, loop.index, …)
	if strings.Contains(s, ".") && !strings.ContainsAny(s, "('\"") {
		return ctx.lookupDotted(s), nil
	}
	// bare name. Inside an active {% call %}, `caller` is bound as a global func (not a scope
	// var), so `caller is defined` and a bare `{{ caller }}` reference must see it. Resolve it
	// from e.funcs when scope lookup misses; outside a {% call %} no such func exists, so
	// `caller is defined` stays False (matching Jinja).
	if v := ctx.lookup(s); v != nil {
		return v, nil
	}
	if s == "caller" {
		if fn, ok := e.funcs["caller"]; ok {
			return fn, nil
		}
	}
	return nil, nil
}

// evalSeq evaluates a comma-separated list of expressions into a []any (tuple/list).
// unescapeStringLiteral processes the backslash escapes Python/Jinja string literals use.
// Unrecognized escapes keep the backslash (Python semantics, e.g. `\&` stays `\&`).
func unescapeStringLiteral(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
				i++
			case '\'':
				b.WriteByte('\'')
				i++
			case '"':
				b.WriteByte('"')
				i++
			case 'n':
				b.WriteByte('\n')
				i++
			case 't':
				b.WriteByte('\t')
				i++
			case 'r':
				b.WriteByte('\r')
				i++
			default:
				b.WriteByte('\\') // keep backslash; next char emitted normally
			}
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

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
	// String methods used by templates.
	if str, isStr := obj.(string); isStr {
		switch method {
		case "startswith":
			if len(argList) > 0 {
				return strings.HasPrefix(str, toString(argList[0])), nil
			}
			return false, nil
		case "endswith":
			if len(argList) > 0 {
				return strings.HasSuffix(str, toString(argList[0])), nil
			}
			return false, nil
		case "lower":
			return strings.ToLower(str), nil
		case "upper":
			return strings.ToUpper(str), nil
		case "strip":
			return strings.TrimSpace(str), nil
		case "replace":
			if len(argList) >= 2 {
				return strings.ReplaceAll(str, toString(argList[0]), toString(argList[1])), nil
			}
			return str, nil
		case "title":
			return strings.Title(strings.ToLower(str)), nil //nolint:staticcheck
		case "format":
			return pyStrFormat(str, argList), nil
		}
	}
	// Ordered map (*jsonenc.Object): keys/values/items preserve insertion order.
	if om, ok := obj.(*jsonenc.Object); ok {
		switch method {
		case "keys":
			out := make([]any, 0, om.Len())
			for _, k := range om.Keys() {
				out = append(out, k)
			}
			return out, nil
		case "values":
			out := []any{}
			for _, k := range om.Keys() {
				v, _ := om.Get(k)
				out = append(out, v)
			}
			return out, nil
		case "items":
			out := []any{}
			for _, k := range om.Keys() {
				v, _ := om.Get(k)
				out = append(out, []any{k, v})
			}
			return out, nil
		case "get":
			key := ""
			if len(argList) > 0 {
				key = toString(argList[0])
			}
			if v, present := om.Get(key); present {
				return v, nil
			}
			if len(argList) > 1 {
				return argList[1], nil
			}
			return nil, nil
		}
	}
	if m, ok := obj.(map[string]any); ok && (method == "values" || method == "keys") {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic; the templates only call these on empty maps so far
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			if method == "keys" {
				out = append(out, k)
			} else {
				out = append(out, m[k])
			}
		}
		return out, nil
	}
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
	var kwKeys []string // keyword-arg order (Quart's url_for preserves it for query params)
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
			kwKeys = append(kwKeys, key)
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
	e.kwOrder = kwKeys // url_for reads this to emit query params in insertion order
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
	case *jsonenc.Object:
		v, _ := b.Get(toString(idx))
		return v
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
	case "string":
		// Jinja |string == soft_str == Python str(); used e.g. as span.http_status|string >= '500'.
		return pyStr(val), nil
	case "default", "d":
		// Jinja default(d, boolean=False): substitute only when the value is undefined
		// (nil), unless the boolean flag is set, in which case substitute on any falsy value.
		args := splitTop(argstr, ',')
		useBool := false
		if len(args) >= 2 {
			if bv, err := e.evalExpr(strings.TrimSpace(args[1]), ctx); err == nil {
				useBool = !isFalsey(bv)
			}
		}
		if (val == nil || (useBool && isFalsey(val))) && strings.TrimSpace(argstr) != "" {
			d, err := e.evalExpr(strings.TrimSpace(args[0]), ctx)
			if err != nil {
				return nil, err
			}
			return d, nil
		}
		return val, nil
	case "length", "count":
		return lengthOf(val), nil
	case "truncate":
		length, killwords, end := 255, false, "..."
		if argstr != "" {
			a := splitTop(argstr, ',')
			if d, err := e.evalExpr(strings.TrimSpace(a[0]), ctx); err == nil {
				length = toIntVal(d)
			}
			if len(a) >= 2 {
				if d, err := e.evalExpr(strings.TrimSpace(a[1]), ctx); err == nil {
					killwords = !isFalsey(d)
				}
			}
			if len(a) >= 3 {
				if d, err := e.evalExpr(strings.TrimSpace(a[2]), ctx); err == nil {
					end = toString(d)
				}
			}
		}
		return jinjaTruncate(toString(val), length, killwords, end), nil
	case "format":
		// Jinja `| format`: Python %-formatting — soft_str(value) % args.
		args, _ := e.evalSeq(splitTop(argstr, ','), ctx)
		al, _ := args.([]any)
		return pyPercentFormat(toString(val), al), nil
	case "round":
		precision, method := 0, "common"
		if argstr != "" {
			a := splitTop(argstr, ',')
			if d, err := e.evalExpr(strings.TrimSpace(a[0]), ctx); err == nil {
				precision = toIntVal(d)
			}
			if len(a) >= 2 {
				if d, err := e.evalExpr(strings.TrimSpace(a[1]), ctx); err == nil {
					method = strings.Trim(toString(d), "'\"")
				}
			}
		}
		f, _ := numFloat(val)
		return jinjaRound(f, precision, method), nil
	case "int":
		def := 0
		if argstr != "" {
			if d, err := e.evalExpr(strings.TrimSpace(splitTop(argstr, ',')[0]), ctx); err == nil {
				def = toIntVal(d)
			}
		}
		return jinjaInt(val, def), nil
	case "float":
		def := 0.0
		if argstr != "" {
			if d, err := e.evalExpr(strings.TrimSpace(splitTop(argstr, ',')[0]), ctx); err == nil {
				if f, ok := numFloat(d); ok {
					def = f
				}
			}
		}
		return jinjaFloat(val, def), nil
	case "min", "max":
		items := toList(val)
		if len(items) == 0 {
			return nil, nil
		}
		best := items[0]
		for _, it := range items[1:] {
			if (name == "max" && jinjaLess(best, it)) || (name == "min" && jinjaLess(it, best)) {
				best = it
			}
		}
		return best, nil
	case "lower":
		return strings.ToLower(toString(val)), nil
	case "upper":
		return strings.ToUpper(toString(val)), nil
	case "trim":
		return strings.TrimSpace(toString(val)), nil
	case "capitalize":
		s := strings.ToLower(toString(val))
		if s == "" {
			return s, nil
		}
		return strings.ToUpper(s[:1]) + s[1:], nil
	case "title":
		return jinjaTitle(toString(val)), nil
	case "replace":
		args, _ := e.evalSeq(splitTop(argstr, ','), ctx)
		al, _ := args.([]any)
		if len(al) >= 2 {
			return strings.ReplaceAll(toString(val), toString(al[0]), toString(al[1])), nil
		}
		return val, nil
	case "join":
		sep := ""
		if argstr != "" {
			a, err := e.evalExpr(argstr, ctx)
			if err != nil {
				return nil, err
			}
			sep = toString(a)
		}
		items := toList(val)
		parts := make([]string, len(items))
		for i, it := range items {
			parts[i] = toString(it)
		}
		return strings.Join(parts, sep), nil
	case "first":
		if items := toList(val); len(items) > 0 {
			return items[0], nil
		}
		return "", nil
	case "last":
		if items := toList(val); len(items) > 0 {
			return items[len(items)-1], nil
		}
		return "", nil
	case "list":
		return toList(val), nil
	case "selectattr", "rejectattr":
		// selectattr('attr') keeps items whose attr is truthy; selectattr('attr','equalto',v)
		// keeps items whose attr == v. rejectattr is the inverse.
		args := splitTop(argstr, ',')
		attrName := ""
		if len(args) >= 1 {
			if a, err := e.evalExpr(strings.TrimSpace(args[0]), ctx); err == nil {
				attrName = toString(a)
			}
		}
		testName := ""
		var cmpVal any
		if len(args) >= 3 {
			if a, err := e.evalExpr(strings.TrimSpace(args[1]), ctx); err == nil {
				testName = toString(a)
			}
			if a, err := e.evalExpr(strings.TrimSpace(args[2]), ctx); err == nil {
				cmpVal = a
			}
		}
		reject := name == "rejectattr"
		out := []any{}
		for _, item := range toList(val) {
			av := attrValue(item, attrName)
			var keep bool
			switch testName {
			case "equalto", "eq", "==":
				keep = equalValues(av, cmpVal)
			case "ne", "!=":
				keep = !equalValues(av, cmpVal)
			default:
				keep = !isFalsey(av)
			}
			if keep != reject {
				out = append(out, item)
			}
		}
		return out, nil
	case "urlencode":
		// Jinja urlencode: a mapping -> "k=v&..." in insertion order, each side URL-escaped;
		// values are Python str()-formatted (lists/ints/etc.).
		if om, ok := val.(*jsonenc.Object); ok {
			parts := make([]string, 0, om.Len())
			for _, k := range om.Keys() {
				v, _ := om.Get(k)
				parts = append(parts, urlQueryEscape(k)+"="+urlQueryEscape(pyStr(v)))
			}
			return strings.Join(parts, "&"), nil
		}
		return urlQueryEscape(toString(val)), nil
	case "mask":
		// app.py _mask_value_for_output: redact sensitive keys/patterns via the per-request
		// DLP rules (installed by SetMaskFunc). Identity when unset, preserving objects for
		// | tojson. The fixture data carries no sensitive content, so corpus output is unchanged.
		if e.maskFunc != nil {
			return e.maskFunc(val), nil
		}
		return val, nil
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
	case float64:
		return formatPyFloat(x)
	case []any:
		return pyStr(x) // {{ list }} -> Python str(list), e.g. "['ERROR']"
	case *jsonenc.Object:
		return pyStr(x) // {{ dict }} -> Python str(dict)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// formatPyFloat mirrors Python str(float): shortest round-trip with a trailing ".0" for
// whole numbers (250.0, not 250).
func formatPyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	return jsonenc.PyFloatRepr(f)
}

// tojson reproduces Jinja's tojson: json.dumps(sort_keys, default separators) then
// HTML-escape <>&' to \uXXXX.
func tojson(v any) string {
	raw := string(jsonenc.Encode(toJSONValue(v), jsonenc.JinjaTojson))
	// Flask/Jinja htmlsafe_json: escape <, >, &, ' as \uXXXX so the JSON is safe inside
	// <script> and HTML attributes.
	m := raw
	m = strings.ReplaceAll(m, "<", "\\u003c")
	m = strings.ReplaceAll(m, ">", "\\u003e")
	m = strings.ReplaceAll(m, "&", "\\u0026")
	m = strings.ReplaceAll(m, "'", "\\u0027")
	return m
}

// toJSONValue coerces interpreter values into types jsonenc understands: native maps
// become ordered Objects (JinjaTojson sorts keys), slices are converted element-wise.
func toJSONValue(v any) any {
	switch x := v.(type) {
	case safeString:
		return x.s
	case map[string]any:
		obj := jsonenc.NewObject()
		for k, vv := range x {
			obj.Set(k, toJSONValue(vv))
		}
		return obj
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = toJSONValue(vv)
		}
		return out
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
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case *jsonenc.Object:
		return x.Len() == 0
	default:
		return false
	}
}

func lengthOf(v any) int {
	switch x := v.(type) {
	case string:
		// Python len() on a str counts code points, not bytes.
		return utf8.RuneCountInString(x)
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

// pyStrFormat implements a focused subset of Python str.format() for the specs templates use:
// auto/positional fields and the format specs "" (plain), "," (thousands grouping), and ".Nf"
// (fixed-point float).
func pyStrFormat(tmpl string, args []any) string {
	var b strings.Builder
	auto := 0
	for i := 0; i < len(tmpl); {
		c := tmpl[i]
		if c == '{' {
			if i+1 < len(tmpl) && tmpl[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			j := strings.IndexByte(tmpl[i:], '}')
			if j < 0 {
				b.WriteByte(c)
				i++
				continue
			}
			inner := tmpl[i+1 : i+j]
			i += j + 1
			field, spec := inner, ""
			if k := strings.IndexByte(inner, ':'); k >= 0 {
				field, spec = inner[:k], inner[k+1:]
			}
			var arg any
			if field == "" {
				if auto < len(args) {
					arg = args[auto]
				}
				auto++
			} else if n, err := strconv.Atoi(field); err == nil && n >= 0 && n < len(args) {
				arg = args[n]
			}
			b.WriteString(applyFormatSpec(arg, spec))
		} else if c == '}' {
			if i+1 < len(tmpl) && tmpl[i+1] == '}' {
				b.WriteByte('}')
				i += 2
				continue
			}
			b.WriteByte(c)
			i++
		} else {
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func applyFormatSpec(arg any, spec string) string {
	switch {
	case spec == ",":
		return commaInt(toInt64Format(arg))
	case strings.HasSuffix(spec, "f"):
		prec := 6
		if dot := strings.IndexByte(spec, '.'); dot >= 0 {
			if p, err := strconv.Atoi(spec[dot+1 : len(spec)-1]); err == nil {
				prec = p
			}
		}
		var f float64
		switch v := arg.(type) {
		case float64:
			f = v
		case int:
			f = float64(v)
		case int64:
			f = float64(v)
		}
		return strconv.FormatFloat(f, 'f', prec, 64)
	default:
		return toString(arg)
	}
}

func toInt64Format(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// commaInt formats an integer with thousands separators, like Python's "{:,}".
func commaInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// jinjaTruncate mirrors Jinja's |truncate(length) with its defaults (end="...", leeway=5,
// killwords=False): short strings (<= length+leeway) pass through; otherwise cut to
// length-len(end), break at the last space, and append the ellipsis.
func jinjaTruncate(s string, length int, killwords bool, end string) string {
	const leeway = 5
	r := []rune(s)
	if len(r) <= length+leeway {
		return s
	}
	cut := length - len([]rune(end))
	if cut < 0 {
		cut = 0
	}
	if cut > len(r) {
		cut = len(r)
	}
	if killwords {
		return string(r[:cut]) + end
	}
	truncated := string(r[:cut])
	if idx := strings.LastIndex(truncated, " "); idx >= 0 {
		truncated = truncated[:idx]
	}
	return truncated + end
}

// jinjaTitle reproduces Jinja's do_title: uppercase the first letter of each word (words are
// delimited by runs of [-\s({[<]), lower-casing the rest. Differs from Go strings.Title /
// Python str.title (which also uppercase after digits and apostrophes): do_title("it's a
// test") == "It's A Test", do_title("abc123def") == "Abc123def".
func jinjaTitle(s string) string {
	var b strings.Builder
	atStart := true
	for _, r := range s {
		switch r {
		case '-', '(', '{', '[', '<', ' ', '\t', '\n', '\r', '\f', '\v':
			b.WriteRune(r)
			atStart = true
			continue
		}
		if atStart {
			b.WriteString(strings.ToUpper(string(r)))
			atStart = false
		} else {
			b.WriteString(strings.ToLower(string(r)))
		}
	}
	return b.String()
}

// jinjaInt mirrors Jinja's do_int / Python int(value, default): truncates floats toward
// zero, parses strings (falling back to float then default).
func jinjaInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		s := strings.TrimSpace(x)
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
	}
	return def
}

// jinjaFloat mirrors Jinja's do_float / Python float(value, default).
func jinjaFloat(v any, def float64) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f
		}
	}
	return def
}

// jinjaRound mirrors Jinja's do_round(value, precision, method). "common" uses Python
// round() (half-to-even on the true double); "ceil"/"floor" scale-then-round.
func jinjaRound(f float64, precision int, method string) float64 {
	switch method {
	case "ceil":
		p := math.Pow(10, float64(precision))
		return math.Ceil(f*p) / p
	case "floor":
		p := math.Pow(10, float64(precision))
		return math.Floor(f*p) / p
	default:
		if precision < 0 {
			p := math.Pow(10, float64(-precision))
			r, _ := strconv.ParseFloat(strconv.FormatFloat(f/p, 'f', 0, 64), 64)
			return r * p
		}
		r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', precision, 64), 64)
		return r
	}
}

// jinjaLess compares two values for | min / | max: numerically when both are numbers,
// otherwise as case-insensitive strings (Jinja's default).
func jinjaLess(a, b any) bool {
	if af, ok := numFloat(a); ok {
		if bf, ok := numFloat(b); ok {
			return af < bf
		}
	}
	return strings.ToLower(toString(a)) < strings.ToLower(toString(b))
}

// attrValue reads item[attr] (dotted) for map / *Object items — used by selectattr.
func attrValue(item any, attr string) any {
	cur := item
	for _, part := range strings.Split(attr, ".") {
		switch m := cur.(type) {
		case map[string]any:
			cur = m[part]
		case *jsonenc.Object:
			v, _ := m.Get(part)
			cur = v
		default:
			return nil
		}
	}
	return cur
}

// pyPercentFormat implements Python's % string formatting for the conversion specs the
// templates use (%.1f, %d, %s, %%): soft_str(value) % args.
func pyPercentFormat(format string, args []any) string {
	var b strings.Builder
	ai := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		if j < len(format) && format[j] == '%' {
			b.WriteByte('%')
			i = j
			continue
		}
		flagsStart := j
		for j < len(format) && strings.IndexByte("-+ #0", format[j]) >= 0 {
			j++
		}
		for j < len(format) && format[j] >= '0' && format[j] <= '9' {
			j++
		}
		if j < len(format) && format[j] == '.' {
			j++
			for j < len(format) && format[j] >= '0' && format[j] <= '9' {
				j++
			}
		}
		if j >= len(format) {
			b.WriteByte('%')
			break
		}
		verb := format[j]
		mid := format[flagsStart:j]
		var arg any
		if ai < len(args) {
			arg = args[ai]
			ai++
		}
		switch verb {
		case 'd', 'i':
			b.WriteString(fmt.Sprintf("%"+mid+"d", toIntVal(arg)))
		case 'x', 'X', 'o':
			b.WriteString(fmt.Sprintf("%"+mid+string(verb), toIntVal(arg)))
		case 'f', 'F', 'e', 'E', 'g', 'G':
			fv, _ := numFloat(arg)
			b.WriteString(fmt.Sprintf("%"+mid+string(verb), fv))
		case 's', 'r':
			b.WriteString(fmt.Sprintf("%"+mid+"s", pyStr(arg)))
		default:
			b.WriteString("%" + mid + string(verb))
		}
		i = j
	}
	return b.String()
}

// sliceBound evaluates an optional slice bound expression; ok=false when omitted (e.g. [:10]).
func (e *Engine) sliceBound(expr string, ctx *scope) (int, bool) {
	if expr == "" {
		return 0, false
	}
	v, err := e.evalExpr(expr, ctx)
	if err != nil {
		return 0, false
	}
	return toIntVal(v), true
}

// sliceValue implements Python slicing base[start:end] for strings (rune-based) and []any.
func sliceValue(base any, start int, hasStart bool, end int, hasEnd bool) any {
	switch b := base.(type) {
	case string:
		r := []rune(b)
		s, e2 := normSlice(len(r), start, hasStart, end, hasEnd)
		return string(r[s:e2])
	case safeString:
		r := []rune(b.s)
		s, e2 := normSlice(len(r), start, hasStart, end, hasEnd)
		return safeString{string(r[s:e2])}
	case []any:
		s, e2 := normSlice(len(b), start, hasStart, end, hasEnd)
		return b[s:e2]
	}
	return ""
}

func normSlice(n, start int, hasStart bool, end int, hasEnd bool) (int, int) {
	s, e := 0, n
	if hasStart {
		s = start
		if s < 0 {
			s += n
		}
	}
	if hasEnd {
		e = end
		if e < 0 {
			e += n
		}
	}
	if s < 0 {
		s = 0
	}
	if e > n {
		e = n
	}
	if s > n {
		s = n
	}
	if e < s {
		e = s
	}
	return s, e
}
