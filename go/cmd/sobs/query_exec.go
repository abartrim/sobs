package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
)

var limitRe = regexp.MustCompile(`(?i)\bLIMIT\b`)
var codePrefixRe = regexp.MustCompile(`^Code:\s*\d+\.\s*DB::Exception:\s*`)
var dbExcRe = regexp.MustCompile(`^DB::Exception:\s*`)

// publicDashboardQueryError mirrors app.py _public_dashboard_query_error: a concise, user-safe
// one-line message from a chdb exception.
func publicDashboardQueryError(err error) string {
	raw := strings.TrimSpace(err.Error())
	message := strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	message = codePrefixRe.ReplaceAllString(message, "")
	message = dbExcRe.ReplaceAllString(message, "")
	message = strings.TrimSpace(strings.SplitN(message, ": while executing function", 2)[0])
	message = strings.TrimSpace(strings.SplitN(message, ". Stack trace", 2)[0])
	if message == "" {
		return "Query execution failed"
	}
	if (strings.Contains(raw, "NO_COMMON_TYPE") || strings.Contains(raw, "TYPE_MISMATCH")) &&
		!strings.Contains(message, "Check casts and column types.") {
		message += ". Check casts and column types."
	}
	if len(message) > 280 {
		message = strings.TrimRight(message[:277], " \t\n\r") + "..."
	}
	return message
}

// chBaseType strips Nullable(...)/LowCardinality(...) wrappers from a ClickHouse type.
func chBaseType(t string) string {
	for {
		if inner, ok := unwrapType(t, "Nullable"); ok {
			t = inner
			continue
		}
		if inner, ok := unwrapType(t, "LowCardinality"); ok {
			t = inner
			continue
		}
		return t
	}
}

func unwrapType(t, wrapper string) (string, bool) {
	if strings.HasPrefix(t, wrapper+"(") && strings.HasSuffix(t, ")") {
		return t[len(wrapper)+1 : len(t)-1], true
	}
	return "", false
}

// chQueryValue converts a FORMAT JSON cell value to the jsonenc-encodable form the Python chdb
// driver + jsonify would produce: Int/UInt -> unquoted integer (json.Number), Float -> float64
// (Python repr), everything else as decoded (string/bool/null).
func chQueryValue(v any, chType string) any {
	if v == nil {
		return nil
	}
	base := chBaseType(chType)
	switch {
	case strings.HasPrefix(base, "Int") || strings.HasPrefix(base, "UInt"):
		switch x := v.(type) {
		case float64:
			return json.Number(strconv.FormatInt(int64(x), 10))
		case string: // 64-bit ints arrive as JSON strings
			return json.Number(strings.TrimSpace(x))
		}
	case strings.HasPrefix(base, "Float"):
		switch x := v.(type) {
		case float64:
			return x
		case string:
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return f
			}
		}
	case strings.HasPrefix(base, "DateTime") || strings.HasPrefix(base, "Date"):
		// The Python chdb driver returns Date/DateTime cells as datetime objects, which Flask
		// jsonify renders as an RFC-822 http_date ("Mon, 02 Jan 2006 15:04:05 GMT").
		if str, ok := v.(string); ok {
			if d := flaskHTTPDate(str); d != "" {
				return d
			}
		}
	}
	return v
}

// flaskHTTPDate parses a chdb Date/DateTime string and renders Flask's http_date (UTC, RFC-822,
// no sub-second). Returns "" when unparseable.
func flaskHTTPDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range drilldownTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format("Mon, 02 Jan 2006 15:04:05") + " GMT"
		}
	}
	// Date-only.
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC().Format("Mon, 02 Jan 2006 15:04:05") + " GMT"
	}
	return ""
}

// serializeQueryResult mirrors app.py's `columns = keys if rows else []` + row-as-list shaping.
func serializeQueryResult(res *store.Result) (columns []any, rows []any) {
	columns, rows = []any{}, []any{}
	if len(res.Rows) == 0 {
		return
	}
	for _, c := range res.Columns {
		columns = append(columns, c)
	}
	for _, row := range res.Rows {
		rec := []any{}
		for j, v := range row {
			t := ""
			if j < len(res.Types) {
				t = res.Types[j]
			}
			rec = append(rec, chQueryValue(v, t))
		}
		rows = append(rows, rec)
	}
	return
}

// serializeQueryDictRows mirrors the render handlers' shaping: columns = first row's keys (or
// [] when empty), rows = list-of-dicts (column name -> typed-serialized value).
func serializeQueryDictRows(res *store.Result) ([]any, []map[string]any) {
	if len(res.Rows) == 0 {
		return []any{}, []map[string]any{}
	}
	columns := make([]any, len(res.Columns))
	for i, c := range res.Columns {
		columns[i] = c
	}
	rows := make([]map[string]any, len(res.Rows))
	for i, row := range res.Rows {
		m := map[string]any{}
		for j, c := range res.Columns {
			t := ""
			if j < len(res.Types) {
				t = res.Types[j]
			}
			m[c] = chQueryValue(row[j], t)
		}
		rows[i] = m
	}
	return columns, rows
}

// injectLimit appends " LIMIT n" when the query has no LIMIT (mirrors the spec/query routes).
func injectLimit(query string, n int) string {
	if limitRe.MatchString(query) {
		return query
	}
	return strings.TrimRight(strings.TrimSpace(query), ";") + " LIMIT " + strconv.Itoa(n)
}

// pythonTypeName maps a serialized cell value to the Python type name _infer_column_types reports
// (json.Number from Int columns -> "int", float64 -> "float", string -> "str", bool -> "bool").
func pythonTypeName(v any) string {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case string:
		return "str"
	default:
		return "str"
	}
}

// inferColumnTypes mirrors _infer_column_types: the type name of the first non-null value per
// column (or "null").
func inferColumnTypes(columns, rows []any) []any {
	out := []any{}
	for idx := range columns {
		detected := "null"
		for _, rowAny := range rows {
			row, ok := rowAny.([]any)
			if !ok || idx >= len(row) {
				continue
			}
			if row[idx] == nil {
				continue
			}
			detected = pythonTypeName(row[idx])
			break
		}
		out = append(out, detected)
	}
	return out
}

// executeNamedQueries mirrors _execute_chart_spec_named_queries (include_records=False): run each
// {name, sql, purpose} with a LIMIT and collect {name, purpose, columns, rows, error}.
func (s *server) executeNamedQueries(named []any, defaultLimit int) []any {
	results := []any{}
	for _, nqAny := range named {
		nq, ok := nqAny.(*jsonenc.Object)
		if !ok {
			continue
		}
		nameV, nameOK := nq.Get("name")
		name := pyStrOrStrip(nameV, nameOK)
		sqlV, sqlOK := nq.Get("sql")
		nqSQL := pyStrOrStrip(sqlV, sqlOK)
		purposeV, purposeOK := nq.Get("purpose")
		purpose := pyStr2(purposeV, purposeOK)
		if name == "" || nqSQL == "" {
			continue
		}
		item := jsonenc.NewObject().Set("name", name).Set("purpose", purpose)
		res, err := s.db.Execute(injectLimit(nqSQL, defaultLimit))
		if err != nil {
			results = append(results, item.Set("columns", []any{}).Set("rows", []any{}).
				Set("error", publicDashboardQueryError(err)))
			continue
		}
		cols, rows := serializeQueryResult(res)
		results = append(results, item.Set("columns", cols).Set("rows", rows).Set("error", ""))
	}
	return results
}

// pyStr2 mirrors str(x or "") without the .strip() (used for `purpose`).
func pyStr2(v any, present bool) string {
	if !present || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyStr(v, present)
}

// handleApiDashboardsSpecDryRun — app.py dry_run_chart_spec_api: compile, execute (LIMIT 20),
// infer column types, run named queries, and return all of it.
func (s *server) handleApiDashboardsSpecDryRun(w http.ResponseWriter, r *http.Request) {
	tid, query, spec, errMsg := s.compileChartSpec(specFromBody(r))
	if errMsg != "" {
		errorOnly(w, http.StatusBadRequest, errMsg)
		return
	}
	res, err := s.db.Execute(injectLimit(query, 20))
	if err != nil {
		errorOnly(w, http.StatusBadRequest, publicDashboardQueryError(err))
		return
	}
	columns, rows := serializeQueryResult(res)
	var named []any
	if nqV, ok := spec.Get("named_queries"); ok {
		named, _ = nqV.([]any)
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("template_id", tid).Set("query", query).Set("spec", spec).
		Set("columns", columns).Set("column_types", inferColumnTypes(columns, rows)).
		Set("rows", rows).Set("named_query_results", s.executeNamedQueries(named, 5)))
}

// handleApiDashboardsQuery — app.py execute_chart_query: validate + execute a SELECT and return
// {columns, rows}. Not query-page gated.
func (s *server) handleApiDashboardsQuery(w http.ResponseWriter, r *http.Request) {
	query := bstr(bodyMap(r), "query")
	if e := validateChartQuery(query); e != "" {
		errorOnly(w, http.StatusBadRequest, e)
		return
	}
	res, err := s.db.Execute(injectLimit(query, 1000))
	if err != nil {
		errorOnly(w, http.StatusBadRequest, publicDashboardQueryError(err))
		return
	}
	columns, rows := serializeQueryResult(res)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("columns", columns).Set("rows", rows))
}
