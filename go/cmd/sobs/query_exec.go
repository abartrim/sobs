package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

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
	}
	return v
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

// injectLimit appends " LIMIT n" when the query has no LIMIT (mirrors the spec/query routes).
func injectLimit(query string, n int) string {
	if limitRe.MatchString(query) {
		return query
	}
	return strings.TrimRight(strings.TrimSpace(query), ";") + " LIMIT " + strconv.Itoa(n)
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
