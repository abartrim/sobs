package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/sobs/sobs/internal/jsonenc"
)

// fix_rum_helpers.go — helpers ported to close the v1rest/RUM divergences found in the
// function-parity audit (see migration/GO_PARITY_DIVERGENCE_AUDIT.md "v1rest").
//
// All of these only change behaviour on the error-event / populated-data / configured-feature /
// OPTIONS paths the empty golden corpus never exercises, so they are parity-safe by construction.

// bodyMapNumber decodes the request JSON body into a map using UseNumber(), so integers stay
// json.Number "5" and floats stay json.Number "5.0". This mirrors Python's json.loads, which
// yields int for "5" and float for "5.0" — the distinction _stringify_attrs (str(int)=="5",
// str(float 5.0)=="5.0") and json.dumps both rely on. The shared bodyMap helper decodes numbers
// as float64 (losing the distinction), so the ingest handlers that build OTel attr maps decode
// here instead.
func bodyMapNumber(r *http.Request) map[string]any {
	m := map[string]any{}
	if r.Body == nil {
		return m
	}
	raw, _ := io.ReadAll(r.Body)
	if len(raw) == 0 {
		return m
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	_ = dec.Decode(&m)
	return m
}

// rumStr mirrors Python str() for a JSON-decoded value where numbers are json.Number (UseNumber).
// json.Number renders verbatim, so an int "5" -> "5" and a float "5.0" -> "5.0", matching
// str(int)/str(float). nil/bool fall back to toStr's True/False/"" semantics; float64 (when a
// value slipped through without UseNumber) keeps the .0 via toStr.
func rumStr(v any) string {
	if n, ok := v.(json.Number); ok {
		return string(n)
	}
	return toStr(v)
}

// rumStringifyAttrs mirrors app.py _stringify_attrs(values): a string->string map where
// scalars (str/int/float/bool) use Python str(), other values use json.dumps(ensure_ascii=False).
// None values are dropped. Numbers are json.Number (from UseNumber) so str(5)=="5" and
// str(5.0)=="5.0" are both preserved. ensure_ascii=False => literal UTF-8 for non-scalars.
func rumStringifyAttrs(values map[string]any) map[string]any {
	out := map[string]any{}
	if values == nil {
		return out
	}
	for k, val := range values {
		if val == nil {
			continue
		}
		switch x := val.(type) {
		case string:
			out[k] = x
		case bool:
			if x {
				out[k] = "True"
			} else {
				out[k] = "False"
			}
		case json.Number:
			out[k] = string(x)
		case float64:
			out[k] = rumFloatStr(x)
		case int:
			out[k] = strconv.Itoa(x)
		case int64:
			out[k] = strconv.FormatInt(x, 10)
		default:
			// json.dumps(value, ensure_ascii=False): compact separators, no non-ASCII escaping.
			out[k] = string(jsonenc.Encode(val, jsonenc.Compact))
		}
	}
	return out
}

// rumFloatStr mirrors Python str(float): integral floats keep ".0" (str(5.0)=="5.0"). This is the
// _stringify_attrs scalar path for a genuine float that arrived as float64 (e.g. not via
// UseNumber). jsonenc.PyFloatRepr reproduces CPython repr(float) for finite values.
func rumFloatStr(f float64) string {
	return jsonenc.PyFloatRepr(f)
}

// rumStringifyContentAttr mirrors app.py's gen_ai content-attribute handling
// (raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False)) — used for
// gen_ai.input.messages / output.messages / system_instructions on /v1/ai. ensure_ascii=False.
func rumStringifyContentAttr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return string(jsonenc.Encode(v, jsonenc.Compact))
}

// rumInt mirrors Python int(payload.get(key, 0) or 0): accepts ints, integral/truncated floats,
// AND numeric strings like "5" (Python int("5") == 5). A non-numeric / unparseable value yields 0.
// With UseNumber, numbers arrive as json.Number; without, as float64. Returns int64 (not
// narrowed to int) since m/key come from an externally-submitted RUM payload and Python's
// int() has arbitrary precision — an oversized value shouldn't be truncated by the Go
// conversion itself; jsonenc renders int and int64 identically, so callers that serialize
// this value are unaffected.
func rumInt(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(string(x)), 64); err == nil {
			return int64(f)
		}
		return 0
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case bool:
		// Python `False or 0` -> 0; `True` -> int(True) == 1.
		if x {
			return 1
		}
		return 0
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		// Python int() only parses integer literals; a float-looking string raises ValueError.
		// The payloads in practice send ints, so a non-integer string maps to 0.
		return 0
	}
	return 0
}

// traceparentRe mirrors app.py _TRACEPARENT_RE.
var traceparentRe = regexp.MustCompile(`^[0-9a-fA-F]{2}-([0-9a-fA-F]{32})-([0-9a-fA-F]{16})-([0-9a-fA-F]{2})$`)

// extractTraceFields mirrors app.py _extract_trace_fields(event): trim+lowercase traceId/spanId,
// parse traceFlags (hex when a string, int otherwise), with a W3C traceparent fallback when
// traceId/spanId are absent.
// traceFlags is returned as int64 (not narrowed to int): the raw event["traceFlags"] path
// parses an externally-submitted RUM payload value with no bound (unlike the traceparent
// fallback below, whose flags are always exactly 2 hex digits, max 0xFF), so keeping the
// wider type avoids a silent Go-side truncation before chdb's own TraceFlags UInt8 column
// validates the range on insert.
func extractTraceFields(event map[string]any) (traceID, spanID string, traceFlags int64) {
	traceID = strings.ToLower(strings.TrimSpace(rumStrOrEmpty(event["traceId"])))
	spanID = strings.ToLower(strings.TrimSpace(rumStrOrEmpty(event["spanId"])))
	traceFlags = 0

	if raw, ok := event["traceFlags"]; ok && raw != nil {
		rawStr := rumStr(raw)
		if strings.TrimSpace(rawStr) != "" {
			if s, isStr := raw.(string); isStr {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 16, 64); err == nil {
					traceFlags = n
				} else {
					traceFlags = 0
				}
			} else {
				// int(raw) for a numeric value.
				if n, err := strconv.ParseInt(strings.TrimSpace(rawStr), 10, 64); err == nil {
					traceFlags = n
				} else if f, err := strconv.ParseFloat(strings.TrimSpace(rawStr), 64); err == nil {
					traceFlags = int64(f)
				} else {
					traceFlags = 0
				}
			}
		}
	}

	if traceID != "" && spanID != "" {
		return traceID, spanID, traceFlags
	}

	traceparent := strings.TrimSpace(rumStrOrEmpty(event["traceparent"]))
	m := traceparentRe.FindStringSubmatch(traceparent)
	if m == nil {
		return traceID, spanID, traceFlags
	}
	parsedTrace := strings.ToLower(m[1])
	parsedSpan := strings.ToLower(m[2])
	parsedFlags, _ := strconv.ParseInt(m[3], 16, 64)

	if parsedTrace == "" {
		parsedTrace = traceID
	}
	if parsedSpan == "" {
		parsedSpan = spanID
	}
	return parsedTrace, parsedSpan, parsedFlags
}

// rumStrOrEmpty mirrors str(x or "") — a falsy value (nil, "", 0, false) yields "".
func rumStrOrEmpty(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return ""
	case json.Number:
		// `0 or ""` -> "" in Python; any non-zero number stringifies.
		s := string(x)
		if s == "0" || s == "0.0" || s == "-0" || s == "-0.0" {
			return ""
		}
		return s
	case float64:
		if x == 0 {
			return ""
		}
		return rumFloatStr(x)
	}
	return toStr(v)
}

// rumStrRaw mirrors str(event.get(key, "")) — a plain str() (no `or ""` truthiness gate).
func rumStrRaw(v any) string {
	if v == nil {
		return ""
	}
	return rumStr(v)
}

// rumTruthy mirrors bool(x) for a JSON-decoded value: nil/false/0/""/[]/{} -> false.
func rumTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		return x != "0" && x != "0.0" && x != "-0" && x != "-0.0" && x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return v != nil
}

// RUM browser-context delta cache — mirrors app.py _RUM_BROWSER_CONTEXT_CACHE and its lock.
// Session ID -> { contextHash, fullContext }.
const rumBrowserContextCacheMax = 10000

type rumBrowserContextEntry struct {
	contextHash string
	fullContext *jsonenc.Object
}

var (
	rumBrowserContextCache     = map[string]rumBrowserContextEntry{}
	rumBrowserContextCacheKeys []string // insertion order, for FIFO trim like Python's dict order
	rumBrowserContextCacheMu   sync.Mutex
)

// handleBrowserContextDelta mirrors app.py _handle_browser_context_delta(event): per-session
// browser-context cache + delta. When the event carries the full browserContext, cache it; when
// contextUnchanged (or only a hash) is sent, retrieve the cached full context. Returns the
// browser.context.<key> attrs to merge into LogAttributes. browserContext is an ordered
// *jsonenc.Object (from decodeOrdered); a non-object value behaves as an empty context.
func handleBrowserContextDelta(event map[string]any) map[string]any {
	sessionID := rumStrRaw(event["sessionId"])
	browserContext, _ := event["browserContext"].(*jsonenc.Object)
	contextHash := rumStrRaw(event["contextHash"])
	contextUnchanged := rumTruthy(event["contextUnchanged"])

	if sessionID == "" || contextHash == "" {
		return map[string]any{}
	}

	hasContext := browserContext != nil && browserContext.Len() > 0
	rumBrowserContextCacheMu.Lock()
	if hasContext {
		if _, existed := rumBrowserContextCache[sessionID]; !existed {
			rumBrowserContextCacheKeys = append(rumBrowserContextCacheKeys, sessionID)
		}
		rumBrowserContextCache[sessionID] = rumBrowserContextEntry{
			contextHash: contextHash,
			fullContext: browserContext,
		}
		if len(rumBrowserContextCache) > rumBrowserContextCacheMax {
			toRemove := len(rumBrowserContextCache) - rumBrowserContextCacheMax
			for i := 0; i < toRemove && len(rumBrowserContextCacheKeys) > 0; i++ {
				oldest := rumBrowserContextCacheKeys[0]
				rumBrowserContextCacheKeys = rumBrowserContextCacheKeys[1:]
				delete(rumBrowserContextCache, oldest)
			}
		}
	}
	if contextUnchanged || (!hasContext && contextHash != "") {
		if cached, ok := rumBrowserContextCache[sessionID]; ok && cached.contextHash == contextHash {
			browserContext = cached.fullContext
		}
	}
	rumBrowserContextCacheMu.Unlock()

	attrs := map[string]any{}
	if browserContext == nil {
		return attrs
	}
	for _, k := range browserContext.Keys() {
		v, _ := browserContext.Get(k)
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		attrs["browser.context."+k] = rumStr(v)
	}
	return attrs
}

// objShallowMap copies an ordered object's top-level entries into a plain map (nested values are
// left as-is — a nested *jsonenc.Object stays an *jsonenc.Object so json.dumps of it stays
// correct). Used to feed the field-reading / _stringify_attrs helpers that operate on a map.
func objShallowMap(o *jsonenc.Object) map[string]any {
	m := make(map[string]any, o.Len())
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		m[k] = v
	}
	return m
}

// rumSplitlines mirrors Python str.splitlines(): split on \r\n, \r, and \n (the common universal
// newline boundaries) without a trailing empty element. Only the ASCII line boundaries are
// reproduced (the Unicode ones \v\f\x1c-\x1e\x85   are vanishingly rare in JS stacks).
func rumSplitlines(s string) []string {
	if s == "" {
		return nil
	}
	// Normalize \r\n and lone \r to \n, then split on \n; drop a single trailing empty element
	// to match splitlines() (which does not yield a final "" for a trailing newline).
	norm := strings.ReplaceAll(s, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	parts := strings.Split(norm, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
