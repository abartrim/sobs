// Package jsonenc produces JSON bytes that match Python's json.dumps / Quart's jsonify
// exactly. Go's encoding/json is unusable for parity: it sorts map keys, HTML-escapes
// <>&, emits no trailing newline, and renders floats differently. This encoder gives
// explicit control over every byte-affecting option and preserves key order via an
// ordered Object type.
//
// The exact option values Quart's jsonify uses (sort_keys, ensure_ascii, item/key
// separators, trailing newline) are pinned against the golden corpus — see
// migration/PARITY_STRATEGY.md §4. Encode(), Member ordering, and the escaping below
// are tuned until the /health golden (and the rest) diff empty.
package jsonenc

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Options mirrors the knobs of Python's json.dumps.
type Options struct {
	SortKeys    bool   // json.dumps(sort_keys=...)
	EnsureASCII bool   // json.dumps(ensure_ascii=...) -> non-ASCII as \uXXXX
	ItemSep     string // separator between items: ", " (default) or "," (compact)
	KeySep      string // separator between key and value: ": " (default) or ":"
	TrailingNL  bool   // Flask/Quart jsonify appends "\n"
}

// QuartJSONify is the option set Quart's jsonify uses for response bodies. Pinned from
// the /health golden (`{"status":"ok","version":"1.0.0"}\n`): COMPACT separators plus a
// trailing newline, sort_keys + ensure_ascii on (Flask/Quart DefaultJSONProvider
// defaults). This single struct governs every JSON route; revisit only if a golden with
// non-ASCII or out-of-order keys disproves a field.
var QuartJSONify = Options{
	SortKeys:    true,
	EnsureASCII: true,
	ItemSep:     ",",
	KeySep:      ":",
	TrailingNL:  true,
}

// Compact mirrors json.dumps(separators=(",", ":")).
var Compact = Options{SortKeys: false, EnsureASCII: false, ItemSep: ",", KeySep: ":"}

// JinjaTojson mirrors Jinja/Flask's `| tojson`: json.dumps with DEFAULT separators
// (", ", ": "), sort_keys + ensure_ascii on. (The template filter additionally
// HTML-escapes <>&' — see render.tojson.) Pinned from the settings_notifications golden:
// `["signal", "tag"]` (spaces) and sorted object keys.
var JinjaTojson = Options{SortKeys: true, EnsureASCII: true, ItemSep: ", ", KeySep: ": "}

// Object is an ordered JSON object. Use it instead of map[string]any so key order is
// explicit and controllable (Python preserves dict insertion order; with SortKeys the
// encoder sorts, matching json.dumps(sort_keys=True)).
type Object struct {
	keys []string
	vals map[string]any
}

func NewObject() *Object { return &Object{vals: map[string]any{}} }

func (o *Object) Set(k string, v any) *Object {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

// Encode renders v to JSON bytes per opts.
func Encode(v any, opts Options) []byte {
	var b strings.Builder
	enc := encoder{opts: opts, b: &b}
	enc.value(v)
	if opts.TrailingNL {
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

type encoder struct {
	opts Options
	b    *strings.Builder
}

func (e *encoder) value(v any) {
	switch x := v.(type) {
	case nil:
		e.b.WriteString("null")
	case bool:
		if x {
			e.b.WriteString("true")
		} else {
			e.b.WriteString("false")
		}
	case json.Number:
		// Verbatim literal — preserves int vs float as parsed (UseNumber), matching
		// json.dumps of a re-parsed value (5 stays 5, not 5.0).
		e.b.WriteString(string(x))
	case string:
		e.encodeString(x)
	case int:
		e.b.WriteString(strconv.Itoa(x))
	case int64:
		e.b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		e.encodeFloat(x)
	case *Object:
		e.object(x)
	case []any:
		e.array(x)
	default:
		// Fall back to a string rendering; callers should pass supported types so this
		// path is never hit in parity-critical output.
		e.encodeString(fmt.Sprintf("%v", x))
	}
}

func (e *encoder) object(o *Object) {
	e.b.WriteByte('{')
	keys := o.keys
	if e.opts.SortKeys {
		keys = append([]string(nil), o.keys...)
		sortStrings(keys)
	}
	for i, k := range keys {
		if i > 0 {
			e.b.WriteString(e.opts.ItemSep)
		}
		e.encodeString(k)
		e.b.WriteString(e.opts.KeySep)
		e.value(o.vals[k])
	}
	e.b.WriteByte('}')
}

func (e *encoder) array(a []any) {
	e.b.WriteByte('[')
	for i, v := range a {
		if i > 0 {
			e.b.WriteString(e.opts.ItemSep)
		}
		e.value(v)
	}
	e.b.WriteByte(']')
}

// encodeFloat mirrors Python's repr for floats inside json (e.g. 1.0 -> "1.0").
func (e *encoder) encodeFloat(f float64) {
	if math.IsInf(f, 1) {
		e.b.WriteString("Infinity")
		return
	}
	if math.IsInf(f, -1) {
		e.b.WriteString("-Infinity")
		return
	}
	if math.IsNaN(f) {
		e.b.WriteString("NaN")
		return
	}
	// NOTE: Python's json renders floats via repr() (shortest round-trip). Go's 'g'/-1
	// is also shortest round-trip but exponent formatting can differ at extremes —
	// refine against goldens if a float-bearing route diverges.
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0" // Python renders whole floats with a trailing .0
	}
	e.b.WriteString(s)
}

// encodeString matches json.dumps string escaping. With EnsureASCII, non-ASCII runes
// become \uXXXX (surrogate pairs for >0xFFFF), matching CPython.
func (e *encoder) encodeString(s string) {
	e.b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			e.b.WriteString(`\"`)
		case '\\':
			e.b.WriteString(`\\`)
		case '\n':
			e.b.WriteString(`\n`)
		case '\r':
			e.b.WriteString(`\r`)
		case '\t':
			e.b.WriteString(`\t`)
		case '\b':
			e.b.WriteString(`\b`)
		case '\f':
			e.b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(e.b, `\u%04x`, r)
			} else if r < 0x80 || !e.opts.EnsureASCII {
				e.b.WriteRune(r)
			} else if r <= 0xFFFF {
				fmt.Fprintf(e.b, `\u%04x`, r)
			} else {
				r1, r2 := utf16Pair(r)
				fmt.Fprintf(e.b, `\u%04x\u%04x`, r1, r2)
			}
		}
	}
	e.b.WriteByte('"')
}

func utf16Pair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xD800 + (r >> 10), 0xDC00 + (r & 0x3FF)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

var _ = utf8.RuneLen // reserved for future surrogate validation
