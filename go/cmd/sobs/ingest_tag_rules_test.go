package main

import "testing"

// Reference md5 values computed from app.py _record_id_for_log / _record_id_for_span.
func TestRecordIDParity(t *testing.T) {
	if got := recordIDForLog("2024-01-02T03:04:05.000+00:00", "api", "t1", "s1"); got != "667f6497261245feecf442a2be2179bd" {
		t.Fatalf("recordIDForLog: %s", got)
	}
	if got := recordIDForSpan("t1", "s1"); got != "8469f3d8f93cfa83d08e95e7dca14ee2" {
		t.Fatalf("recordIDForSpan: %s", got)
	}
	if got := recordIDForLog("", "", "", ""); got != "2edf2958166561c5c08cd228e53bbcdc" {
		t.Fatalf("recordIDForLog empty: %s", got)
	}
	if got := recordIDForSpan("", ""); got != "b99834bc19bbad24580b3adfa04fb947" {
		t.Fatalf("recordIDForSpan empty: %s", got)
	}
}

func rule(recordTypes []any, conditions []any, mf, mo, mv, mak string) map[string]any {
	return map[string]any{
		"record_types": recordTypes, "conditions": conditions,
		"match_field": mf, "match_operator": mo, "match_value": mv, "match_attr_key": mak,
	}
}

func TestMatchTagRule(t *testing.T) {
	attrs := map[string]any{"env": "prod", "team": "payments"}

	// Legacy single-condition eq on service_name.
	r := rule([]any{"log"}, nil, "service_name", "eq", "api", "")
	if !matchTagRule(r, "log", "api", "INFO", "boom", attrs, "", "") {
		t.Fatal("eq service_name should match")
	}
	if matchTagRule(r, "log", "web", "INFO", "boom", attrs, "", "") {
		t.Fatal("eq service_name should NOT match a different service")
	}
	// record-type gate: rule is log-only, evaluate a trace record.
	if matchTagRule(r, "trace", "api", "INFO", "boom", attrs, "", "") {
		t.Fatal("record-type gate should reject trace for a log-only rule")
	}
	// "all" record_types bypasses the gate.
	rAll := rule([]any{"all"}, nil, "service_name", "eq", "api", "")
	if !matchTagRule(rAll, "trace", "api", "INFO", "boom", attrs, "", "") {
		t.Fatal("all record_types should match any record type")
	}

	// contains (case-insensitive) on body.
	rc := rule(nil, nil, "body", "contains", "BOO", "")
	if !matchTagRule(rc, "log", "api", "INFO", "boom happened", attrs, "", "") {
		t.Fatal("contains should be case-insensitive")
	}

	// regex on span_name.
	rr := rule([]any{"trace"}, nil, "span_name", "regex", "^GET ", "")
	if !matchTagRule(rr, "trace", "api", "", "", attrs, "GET /", "") {
		t.Fatal("regex span_name should match")
	}
	// invalid regex -> no match (re.error -> False).
	rb := rule([]any{"trace"}, nil, "span_name", "regex", "([", "")
	if matchTagRule(rb, "trace", "api", "", "", attrs, "GET /", "") {
		t.Fatal("invalid regex should not match")
	}

	// attribute condition.
	ra := rule([]any{"log"}, nil, "attribute", "eq", "prod", "env")
	if !matchTagRule(ra, "log", "api", "", "", attrs, "", "") {
		t.Fatal("attribute eq should match")
	}

	// composite (all conditions must match).
	composite := rule([]any{"log"}, []any{
		map[string]any{"match_field": "service_name", "match_operator": "eq", "match_value": "api", "match_attr_key": ""},
		map[string]any{"match_field": "severity", "match_operator": "eq", "match_value": "ERROR", "match_attr_key": ""},
	}, "", "", "", "")
	if !matchTagRule(composite, "log", "api", "ERROR", "", attrs, "", "") {
		t.Fatal("composite all-match should match")
	}
	if matchTagRule(composite, "log", "api", "INFO", "", attrs, "", "") {
		t.Fatal("composite should fail when one condition fails")
	}

	// The base-fixture "Inert Sample Tagger": matches no fixture service.
	inert := rule([]any{"log"}, []any{
		map[string]any{"match_field": "service_name", "match_operator": "eq", "match_value": "no-such-service-zzz", "match_attr_key": ""},
	}, "service_name", "eq", "no-such-service-zzz", "")
	for _, svc := range []string{"", "api", "rum", "browser"} {
		if matchTagRule(inert, "log", svc, "ERROR", "boom", attrs, "", "exception") {
			t.Fatalf("inert rule must not match service %q", svc)
		}
	}
}

func TestRowAttrsFallback(t *testing.T) {
	la := map[string]any{"a": "1"}
	sa := map[string]any{"b": "2"}
	if got := rowAttrs(map[string]any{"LogAttributes": la}); got["a"] != "1" {
		t.Fatal("LogAttributes preferred")
	}
	if got := rowAttrs(map[string]any{"SpanAttributes": sa}); got["b"] != "2" {
		t.Fatal("SpanAttributes used when no LogAttributes")
	}
	if got := rowAttrs(map[string]any{}); len(got) != 0 {
		t.Fatal("empty fallback")
	}
}

func TestExtractAttrMaps(t *testing.T) {
	rows := []map[string]any{
		{"LogAttributes": map[string]any{"k": "v"}, "ResourceAttributes": map[string]any{"r": "x"}},
		{"LogAttributes": "not-a-map"},
	}
	if got := extractLogAttrMaps(rows); len(got) != 1 || got[0]["k"] != "v" {
		t.Fatalf("extractLogAttrMaps: %+v", got)
	}
	if got := extractAttrMaps(rows, "ResourceAttributes"); len(got) != 1 || got[0]["r"] != "x" {
		t.Fatalf("extractAttrMaps resource: %+v", got)
	}
}

func TestSpanAttrStr(t *testing.T) {
	attrs := map[string]any{"exception.type": "ValueError", "n": int64(5)}
	if got := spanAttrStr(attrs, "exception.type", "SpanError"); got != "ValueError" {
		t.Fatalf("present: %s", got)
	}
	if got := spanAttrStr(attrs, "missing", "SpanError"); got != "SpanError" {
		t.Fatalf("default: %s", got)
	}
	if got := spanAttrStr(attrs, "n", ""); got != "5" {
		t.Fatalf("int str(): %s", got)
	}
}
