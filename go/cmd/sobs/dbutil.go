package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
)

// This file holds the shared plumbing every data-backed JSON handler uses. The porting
// recipe for a route is: issue the SAME SQL as app.py via store.Execute, walk the rows as
// maps, and rebuild the response with the SAME key set + the SAME Python type coercion
// (str()/int()/bool()). jsonenc + QuartJSONify then reproduce jsonify's bytes exactly.

// rowMaps materializes a Result as a slice of column->value maps so handlers can index by
// column name (mirroring app.py's row["Col"] dict access). Values are whatever ClickHouse
// FORMAT JSON produced: Go string for String/UInt64 columns, float64 for small ints.
func rowMaps(res *store.Result) []map[string]any {
	out := make([]map[string]any, len(res.Rows))
	for i, row := range res.Rows {
		m := make(map[string]any, len(res.Columns))
		for j, c := range res.Columns {
			m[c] = row[j]
		}
		out[i] = m
	}
	return out
}

// cStr mirrors Python str(row[key]) for the values FORMAT JSON yields here (String cols
// arrive as Go strings). The numeric/bool branches exist only for safety; parity columns
// fed to str() are String-typed.
func cStr(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// cInt mirrors Python int(row[key]). UInt64/COUNT() columns arrive as strings (ClickHouse
// FORMAT JSON stringifies 64-bit ints); small ints arrive as float64.
func cInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return int(n)
	default:
		return 0
	}
}

// cStrDef mirrors Python `str(row[key]) if row[key] else default` — falsy ("" / 0 / nil)
// yields the default.
func cStrDef(m map[string]any, key, def string) string {
	switch x := m[key].(type) {
	case nil:
		return def
	case string:
		if x == "" {
			return def
		}
		return x
	case float64:
		if x == 0 {
			return def
		}
		return cStr(m, key)
	default:
		return cStr(m, key)
	}
}

// cBool mirrors Python bool(int(row[key])) — the IsDeleted/IsDismissed UInt8 idiom.
func cBool(m map[string]any, key string) bool {
	return cInt(m, key) != 0
}

// toEncodable converts an arbitrary JSON value (as decoded from a stored *Json column)
// into a jsonenc-encodable tree: objects -> *jsonenc.Object, arrays -> []any, numbers kept
// as json.Number so integer literals stay integers (json.dumps of a re-parsed dict does
// not promote 5 to 5.0). Mirrors json.loads(...) feeding back into jsonify.
func toEncodable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		obj := jsonenc.NewObject()
		for k, vv := range x {
			obj.Set(k, toEncodable(vv))
		}
		return obj
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = toEncodable(vv)
		}
		return out
	default:
		return v
	}
}

// parseJSONValue decodes an arbitrary JSON document into a jsonenc-encodable tree
// (objects/arrays/numbers preserved via UseNumber). Mirrors json.load(f) feeding into
// jsonify — used by routes that load a static JSON catalog and re-serialize it.
func parseJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	return toEncodable(parsed), nil
}

// parseJSONObject decodes a stored JSON string into a jsonenc.Object, returning an empty
// object when the value is blank or not a JSON object — matching _parse_report_filters and
// friends (json.loads with a {} fallback). UseNumber preserves int/float literals.
func parseJSONObject(raw string) *jsonenc.Object {
	if strings.TrimSpace(raw) == "" {
		return jsonenc.NewObject()
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return jsonenc.NewObject()
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return jsonenc.NewObject()
	}
	enc := toEncodable(m)
	if obj, ok := enc.(*jsonenc.Object); ok {
		return obj
	}
	return jsonenc.NewObject()
}

// queryIntClamp mirrors Python's `max(lo, min(hi, int(request.args.get(key, def))))` with
// the try/except fallback to def on a bad value.
func queryIntClamp(r *http.Request, key string, def, lo, hi int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// dbError reproduces the generic 500 JSON some handlers return on a query exception
// ({"ok": false, "error": "..."}). Most parity routes never hit this on the fixture DB.
func (s *server) dbError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError,
		jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
}
