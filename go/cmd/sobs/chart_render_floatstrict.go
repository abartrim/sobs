package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// pyFloatStrict mirrors Python float(x): it RAISES (returns an error) on a non-numeric string,
// None, or any unhandled type — it does NOT best-effort coerce to 0. Accepted forms: int/float/
// bool (float(True)==1.0) and a numeric string (whitespace-trimmed, incl. inf/nan/scientific
// notation), matching CPython's float() acceptance for the chDB cell types this code can see
// (json.Number, float64, int, string, bool). The returned error text mirrors CPython's exception
// message so _public_dashboard_query_error surfaces the same 400 body as app.py.
func pyFloatStrict(v any) (float64, error) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, floatStrConvErr(x.String())
		}
		return f, nil
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, floatStrConvErr(x)
		}
		return f, nil
	case nil:
		// Python float(None) -> TypeError.
		return 0, errors.New("float() argument must be a string or a real number, not 'NoneType'")
	}
	// Any other type (e.g. chDateTime) -> TypeError in Python.
	return 0, errors.New("float() argument must be a string or a real number")
}

// floatStrConvErr mirrors CPython's ValueError text for float() on a non-numeric string.
func floatStrConvErr(s string) error {
	return fmt.Errorf("could not convert string to float: '%s'", s)
}
