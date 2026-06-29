package main

// coverage_pure_f_test.go — oracle-anchored unit tests for SLICE F pure helpers.
//
// SCOPE NOTE: of the 12 functions originally assigned to slice F, 10 turned out to be ALREADY
// covered by earlier slices in current go-main (verified by grep over *_test.go for real call
// sites). Re-testing them adds ~0 coverage, so they are listed under SKIPPED with the existing
// test file that covers them. Only two functions were genuinely untested; they are the TESTED set.
//
//   TESTED (Go-internal plumbing — no direct app.py function; behavioral assertions anchored to the
//   small Python idiom each helper reproduces, cited in its own doc-comment):
//     strOrBrace (upstream.go:275)  behavioral — mirrors `str(ConfigJson) or "{}"` (blank -> "{}",
//                                   else returned verbatim, NOT trimmed)
//     unwrapType (query_exec.go:69) behavioral — peels one chDB type wrapper:
//                                   "Wrapper(inner)" -> ("inner", true); else ("", false).
//                                   Drives chBaseType's Nullable/LowCardinality unwrap loop.
//
//   SKIPPED — already covered elsewhere in go-main (would only duplicate):
//     requireDmSafeValue            — cmd/sobs/safeguard_dm_validate_test.go
//     validateDmBackupName          — cmd/sobs/safeguard_dm_validate_test.go
//     normalizeNotificationCondition— cmd/sobs/coverage_pure_b_test.go, notif_chart_mcp_helpers_test.go
//     parseNotificationConditionsJSON— cmd/sobs/notif_chart_mcp_helpers_test.go
//     objStrOr                      — cmd/sobs/ai_action_normalize_test.go
//     objSub                        — cmd/sobs/ai_action_normalize_test.go
//     objTruthy                     — cmd/sobs/ai_action_normalize_test.go
//     upstreamFixtureKeyBody        — cmd/sobs/upstream_key_test.go
//     rumAssetSignature             — cmd/sobs/remaining_pure_helpers_test.go
//     quotePathSafeSlash            — internal/store/chdb_target_test.go (package store, not main)
//
//   DIVERGENCE: none found.

import "testing"

// ---------------------------------------------------------------------------
// strOrBrace — upstream.go:275 — Go-internal, behavioral.
//   Mirrors `str(ConfigJson) or "{}"`: a blank (after TrimSpace) input yields "{}"; a non-blank
//   input is returned VERBATIM (no trimming applied to the returned value).
// ---------------------------------------------------------------------------

func TestSliceF_strOrBrace(t *testing.T) {
	cases := []struct {
		in, want, desc string
	}{
		{"", "{}", "empty -> {}"},
		{"   ", "{}", "spaces only -> {}"},
		{"\t\n", "{}", "whitespace only -> {}"},
		{`{"a":1}`, `{"a":1}`, "non-blank object returned verbatim"},
		{"[]", "[]", "non-blank array returned verbatim"},
		{"null", "null", "literal null string returned verbatim"},
		{"  {\"a\":1}  ", "  {\"a\":1}  ", "non-blank with surrounding spaces is NOT trimmed"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := strOrBrace(c.in); got != c.want {
				t.Errorf("strOrBrace(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// unwrapType — query_exec.go:69 — Go-internal, behavioral.
//   Peels one chDB type wrapper: "Wrapper(inner)" -> ("inner", true); otherwise ("", false).
//   Must require BOTH the "Wrapper(" prefix and the trailing ")"; the inner may itself be wrapped
//   (chBaseType calls it in a loop) or empty.
// ---------------------------------------------------------------------------

func TestSliceF_unwrapType(t *testing.T) {
	cases := []struct {
		typ, wrapper, wantInner string
		wantOK                  bool
		desc                    string
	}{
		{"Nullable(String)", "Nullable", "String", true, "simple unwrap"},
		{"LowCardinality(String)", "LowCardinality", "String", true, "low-cardinality unwrap"},
		{"Nullable(LowCardinality(String))", "Nullable", "LowCardinality(String)", true, "nested inner preserved (one peel)"},
		{"Array(UInt8)", "Array", "UInt8", true, "array wrapper"},
		{"Nullable()", "Nullable", "", true, "empty inner is valid"},
		{"String", "Nullable", "", false, "no wrapper"},
		{"Nullable(String)", "LowCardinality", "", false, "wrong wrapper name"},
		{"NullableString)", "Nullable", "", false, "missing open paren"},
		{"Nullable(String", "Nullable", "", false, "missing close paren"},
		{"Nullable", "Nullable", "", false, "bare wrapper name, no parens"},
		{"", "Nullable", "", false, "empty type"},
		{"xNullable(String)", "Nullable", "", false, "wrapper not at start"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			inner, ok := unwrapType(c.typ, c.wrapper)
			if ok != c.wantOK || inner != c.wantInner {
				t.Errorf("unwrapType(%q,%q) = (%q,%v), want (%q,%v)", c.typ, c.wrapper, inner, ok, c.wantInner, c.wantOK)
			}
		})
	}
}
