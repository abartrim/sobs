package store

import (
	"fmt"
	"strconv"
	"strings"
)

// inlineParams substitutes positional `?` placeholders with ClickHouse-quoted literals.
// The Python side passes params to chdb's cursor which does the same substitution; we
// reproduce it so the SQL (and thus results) match. Placeholders inside string literals
// are left alone.
func inlineParams(query string, params []any) (string, error) {
	if len(params) == 0 {
		return query, nil
	}
	var b strings.Builder
	pi := 0
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			b.WriteByte(c)
		case '?':
			if pi >= len(params) {
				return "", fmt.Errorf("not enough params for placeholders")
			}
			b.WriteString(quoteLiteral(params[pi]))
			pi++
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func quoteLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + chEscape(x) + "'"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return "'" + chEscape(fmt.Sprintf("%v", x)) + "'"
	}
}

// chEscape escapes a string for a ClickHouse single-quoted literal.
func chEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// --- Result accessors -------------------------------------------------------------

// First returns the first row, or nil if empty.
func (r *Result) First() []any {
	if len(r.Rows) == 0 {
		return nil
	}
	return r.Rows[0]
}

// ColumnIndex returns the index of a named column, or -1.
func (r *Result) ColumnIndex(name string) int {
	for i, c := range r.Columns {
		if c == name {
			return i
		}
	}
	return -1
}
