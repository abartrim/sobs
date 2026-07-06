package main

import (
	"encoding/json"
	"testing"
)

// cov95_b16_chart_render_floatstrict_test.go — batch 16 targeted coverage for
// cmd/sobs/chart_render_floatstrict.go's pyFloatStrict: the json.Number error branch (a
// json.Number holding non-numeric text, which x.Float64() rejects) that
// TestPyFloatStrict (chart_render_floatstrict_test.go) does not exercise.

func TestPyFloatStrictInvalidJSONNumber(t *testing.T) {
	_, err := pyFloatStrict(json.Number("not-a-number"))
	if err == nil {
		t.Fatal("want error for a malformed json.Number, got nil")
	}
	want := "could not convert string to float: 'not-a-number'"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
