package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// mcpClampArg mirrors mcp.py _clamp(args.get("limit"), lo, hi, default).
//
// Python's _clamp accepts whatever args.get(name) yields — a JSON int/float, a numeric string, or
// None — and computes max(lo, min(hi, int(value))), returning default on None or any
// ValueError/TypeError. The Go MCP request body is decoded with json.Number (parseJSONValue uses
// UseNumber), so a JSON `"limit": 50` arrives here as json.Number("50"), NOT a Go string. The old
// objGetStr-based path saw "" for every non-string and silently fell back to default, ignoring a
// caller-supplied numeric limit. This helper honors json.Number / float64 / int / numeric string,
// truncating toward zero like Python's int(), then clamps to [lo, hi].
func mcpClampArg(o *jsonenc.Object, key string, lo, hi, def int) int {
	v, ok := o.Get(key)
	if !ok {
		return def // args.get(name) -> None -> default
	}
	n, ok := mcpToInt(v)
	if !ok {
		return def // mirrors int(value) raising ValueError/TypeError -> default
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// mcpToInt converts a JSON-decoded scalar to an int the way Python's int(value) would, reporting
// false when int(value) would raise (None, bool-like-via-string, non-numeric text, NaN/Inf).
func mcpToInt(v any) (int, bool) {
	switch x := v.(type) {
	case json.Number:
		// int(json int) is exact; int(json float) truncates toward zero. Try int first, then float.
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return int(i), true
		}
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, false
			}
			return int(math.Trunc(f)), true
		}
		return 0, false
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(math.Trunc(x)), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case bool:
		// Python: int(True)==1, int(False)==0 (bool is an int subclass).
		if x {
			return 1, true
		}
		return 0, true
	case string:
		// int("100") works; int("3.9") raises ValueError -> default (we do NOT fall through to float).
		s := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(i), true
		}
		return 0, false
	default:
		// None / bool / dict / list: int(...) raises TypeError -> default.
		return 0, false
	}
}
