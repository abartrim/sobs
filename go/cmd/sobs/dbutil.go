package main

import (
	"encoding/json"
	"fmt"
	"math"
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
// FORMAT JSON stringifies 64-bit ints); small ints arrive as float64. Narrows int64 -> int:
// this is a theoretical concern only on a 32-bit platform (every SOBS deployment target is
// amd64/arm64, where int is 64-bit, per Dockerfile.go), and strconv.ParseInt already clamps
// an out-of-range string to +/-MaxInt64 rather than wrapping. cInt has ~120 call sites across
// the handler package, many feeding pagination/slice-index int params directly, so widening
// its return type is a much larger, riskier change than the low practical severity here
// justifies — see [[go-security-findings]] memory for the fuller writeup.
func cInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0
		}
		if n < math.MinInt64 || n > math.MaxInt64 {
			return 0
		}
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

// cFloat mirrors Python float(row[key]) — Float64 columns arrive as JSON numbers.
func cFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	default:
		return 0
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

// parseJSONValue decodes a JSON document into a jsonenc-encodable tree, PRESERVING object
// key insertion order (objects -> *jsonenc.Object, arrays -> []any, numbers -> json.Number).
// Order matters for `.keys()`/`.items()`/`.values()` in templates (jsonify/tojson sort, but
// e.g. default_ai_pricing.keys()|list does not).
func parseJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return decodeOrdered(dec)
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			obj := jsonenc.NewObject()
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(keyTok.(string), val)
			}
			_, _ = dec.Token() // consume '}'
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			_, _ = dec.Token() // consume ']'
			return arr, nil
		}
	}
	return tok, nil // string / json.Number / bool / nil
}

// parseJSONObjectOrdered is parseJSONObject but PRESERVES key insertion order (via
// decodeOrdered). Use it for values rendered by an insertion-order-preserving serializer
// (json.dumps(indent=2) exports), not the key-sorting jsonify path. parseJSONObject's map
// decode loses order, which only matters when the output keeps it.
func parseJSONObjectOrdered(raw string) *jsonenc.Object {
	if strings.TrimSpace(raw) == "" {
		return jsonenc.NewObject()
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return jsonenc.NewObject()
	}
	if obj, ok := v.(*jsonenc.Object); ok {
		return obj
	}
	return jsonenc.NewObject()
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

// appSetting mirrors _get_app_setting: SELECT Value FROM sobs_app_settings FINAL WHERE
// Key=? — returns the trimmed value and whether it is present+non-empty. The VAPID private key is
// the one key stored encrypted at rest, so it is Fernet-decrypted here (a bad token / missing key
// resolves to "", exactly as Python's _decrypt_secret_value does).
func (s *server) appSetting(key string) (string, bool) {
	v := s.appSettingRaw(key)
	if key == vapidPrivateKeySetting {
		v = s.decryptSecretValue(v)
	}
	return v, v != ""
}

// appSettingRaw mirrors _get_app_setting_raw: the trimmed stored value with NO decryption — used
// to detect whether an (encrypted-at-rest) secret is present without exposing its plaintext.
func (s *server) appSettingRaw(key string) string {
	res, err := s.db.Execute("SELECT Value FROM sobs_app_settings FINAL WHERE Key = ? LIMIT 1", key)
	if err != nil || len(res.Rows) == 0 {
		return ""
	}
	return strings.TrimSpace(cStr(rowMaps(res)[0], "Value"))
}

// dmSettingValue reads a data-management setting and decrypts it when it is one of the sensitive
// secrets — the include_sensitive_values=True path of app.py _load_dm_settings that the backup /
// restore consumers run on. Without this, the secrets encrypted at rest by setDMSetting would be
// fed verbatim (as enc:v1: ciphertext) into the S3 BACKUP statement. Non-sensitive keys pass
// through unchanged; a no-op when SOBS_SETTINGS_ENCRYPTION_KEY is unset (the parity invariant).
func (s *server) dmSettingValue(key string) string {
	raw := s.appSettingRaw(key)
	if isSensitiveDMSettingKey(key) {
		return s.decryptSecretValue(raw)
	}
	return raw
}

// appSettingBool mirrors _is_truthy_setting(value, default): when the setting is absent, def
// is the fallback; otherwise the value is truthy for {1,true,yes,on}. (app.py accepts "on" —
// the read path must match the write path's truthiness or DLP could silently disable.)
func (s *server) appSettingBool(key string, def bool) bool {
	v, ok := s.appSetting(key)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// queryPageEnabled mirrors app.py _query_page_enabled: the Query page is available when an AI
// endpoint and model are configured (DB settings, honoring env/file overrides). Evaluated per
// request — configuring AI via the Settings UI takes effect without a restart.
func (s *server) queryPageEnabled() bool {
	return s.loadAISetting("ai.endpoint_url", "") != "" && s.loadAISetting("ai.model", "") != ""
}

// kubernetesEnabled mirrors app.py _kubernetes_enabled: on when the kubernetes.enabled app
// setting == "1" (DB-driven, per request).
func (s *server) kubernetesEnabled() bool {
	v, _ := s.appSetting("kubernetes.enabled")
	return v == "1"
}

// parseReportFiltersNative decodes a stored FiltersJson into a NATIVE Go value (maps,
// slices, json.Number) for template rendering / the tojson filter — mirrors
// _parse_report_filters with a {} fallback. (parseJSONObject builds a jsonenc.Object for
// JSON responses; templates need native maps.)
func parseReportFiltersNative(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return jsonenc.NewObject()
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return jsonenc.NewObject()
	}
	if _, ok := v.(*jsonenc.Object); ok {
		return v
	}
	return jsonenc.NewObject()
}

// distinctStrings runs a query returning one string column and collects the values.
func (s *server) distinctStrings(query string, params ...any) []any {
	out := []any{}
	res, err := s.db.Execute(query, params...)
	if err != nil || len(res.Columns) == 0 {
		return out
	}
	col := res.Columns[0]
	for _, m := range rowMaps(res) {
		out = append(out, cStr(m, col))
	}
	return out
}

// countRows runs a `SELECT count() AS c …` and returns the integer (0 on error).
func (s *server) countRows(query string) int {
	res, err := s.db.Execute(query)
	if err != nil || len(res.Rows) == 0 {
		return 0
	}
	return cInt(rowMaps(res)[0], "c")
}

// countRowsParams is countRows with positional `?` params — for parameterized count queries
// (e.g. the filtered /rum session/event totals). Reads the single `c`-aliased column.
func (s *server) countRowsParams(query string, params ...any) int {
	res, err := s.db.Execute(query, params...)
	if err != nil || len(res.Rows) == 0 {
		return 0
	}
	return cInt(rowMaps(res)[0], "c")
}

// dbError reproduces the generic 500 JSON some handlers return on a query exception
// ({"ok": false, "error": "..."}). Most parity routes never hit this on the fixture DB.
func (s *server) dbError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError,
		jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
}
