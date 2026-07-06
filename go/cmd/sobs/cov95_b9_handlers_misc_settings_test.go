package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// This file covers batch-9 undertested branches in cmd/sobs/handlers_misc.go and
// cmd/sobs/fix_pages_settings.go — neither file had a dedicated test file before this one. Focus:
// validation/error branches, empty-vs-populated shaping, and small pure helpers
// (loadJSONStringListSetting, orderedMapFromKV, condWindowMinutes/pyIntTrunc, mcpAuthenticated).
// Oracle references are the doc comments on each handler.

// ==================================== handlers_misc.go ========================================

// ---- handleApiAiSpanAttributes -----------------------------------------------------------------

func TestHandleApiAiSpanAttributes_MissingParams(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiAiSpanAttributes(w, httptest.NewRequest("GET", "/api/ai/span-attributes", nil))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiSpanAttributes_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiAiSpanAttributes(w, httptest.NewRequest("GET", "/api/ai/span-attributes?ts=t1&service=svc", nil))
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiSpanAttributes_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiAiSpanAttributes(w, httptest.NewRequest("GET", "/api/ai/span-attributes?ts=t1&service=svc&trace_id=tr1&span_name=sp1", nil))
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiSpanAttributes_Found(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"attr_keys", "attr_values"},
			[]any{[]any{"gen_ai.request.model"}, []any{"gpt-4o"}}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiAiSpanAttributes(w, httptest.NewRequest("GET", "/api/ai/span-attributes?ts=t1&service=svc", nil))
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gen_ai.request.model") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// ---- handleApiCveFindings ----------------------------------------------------------------------

func TestHandleApiCveFindings_Disabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 && params[0] == "enrichment.cve_enabled" {
			return storetest.Result([]string{"Value"}, []any{"0"}), nil
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiCveFindings(w, httptest.NewRequest("GET", "/api/enrichment/cve/findings", nil))
	if w.Code != 403 {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiCveFindings_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_cve_findings") {
			return nil, errors.New("boom")
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiCveFindings(w, httptest.NewRequest("GET", "/api/enrichment/cve/findings", nil))
	if w.Code < 400 {
		t.Fatalf("want error status, got %d", w.Code)
	}
}

func TestHandleApiCveFindings_Empty(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiCveFindings(w, httptest.NewRequest("GET", "/api/enrichment/cve/findings", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"findings":[]`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiWebTrafficGeo --------------------------------------------------------------------

func TestHandleApiWebTrafficGeo_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficGeo(w, httptest.NewRequest("GET", "/api/web-traffic/geo", nil))
	if w.Code < 400 {
		t.Fatalf("want error status, got %d", w.Code)
	}
}

func TestHandleApiWebTrafficGeo_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "hyperdx_sessions") {
			return storetest.Result([]string{"ip", "cnt"}, []any{"127.0.0.1", 4.0}, []any{"10.0.0.1", 2.0}), nil
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficGeo(w, httptest.NewRequest("GET", "/api/web-traffic/geo", nil))
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Private/Local") {
		t.Fatalf("private IPs should be labeled Private/Local: %s", w.Body.String())
	}
}

// ---- handleApiDashboardsSpecOptions -------------------------------------------------------------

func TestHandleApiDashboardsSpecOptions_UnsupportedSource(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiDashboardsSpecOptions(w, httptest.NewRequest("GET", "/api/dashboards/spec/options?source_view=bogus", nil))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiDashboardsSpecOptions_OtelLogs(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"v"}, []any{"svc-a"}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiDashboardsSpecOptions(w, httptest.NewRequest("GET", "/api/dashboards/spec/options?source_view=otel_logs", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "log_volume") || !strings.Contains(w.Body.String(), "svc-a") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiDashboardsSpecOptions_MetricsSource(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"v"}, []any{"cpu.usage"}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiDashboardsSpecOptions(w, httptest.NewRequest("GET", "/api/dashboards/spec/options?source_view=otel_metrics_gauge", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "cpu.usage") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiDashboardsSpecOptions_ErrorResolutions(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiDashboardsSpecOptions(w, httptest.NewRequest("GET", "/api/dashboards/spec/options?source_view=sobs_error_resolutions", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "resolved_error_volume") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiDashboardsSpecOptions_QueryErrorFallsBackEmpty(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiDashboardsSpecOptions(w, httptest.NewRequest("GET", "/api/dashboards/spec/options?source_view=otel_traces", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"services":[]`) {
		t.Fatalf("query error should fall back to empty, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiMaskingRules ----------------------------------------------------------------------

func TestHandleApiMaskingRules_Defaults(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiMaskingRules(w, httptest.NewRequest("GET", "/api/settings/masking/rules", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"custom_keys":[]`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- loadJSONStringListSetting ------------------------------------------------------------------

func TestLoadJSONStringListSetting(t *testing.T) {
	t.Run("missing setting -> empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.loadJSONStringListSetting("some.key"); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("not-a-list JSON -> empty", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"some.key": `{"a":1}`})}
		if got := s.loadJSONStringListSetting("some.key"); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("mixed list drops falsy/whitespace, coerces non-strings", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{
			"some.key": `["a", " b ", "", 0, false, null, 5, true, [], {}]`,
		})}
		got := s.loadJSONStringListSetting("some.key")
		want := []string{"a", "b", "5", "True"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("index %d: got %q, want %q (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("malformed JSON -> empty", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"some.key": `[not json`})}
		if got := s.loadJSONStringListSetting("some.key"); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})
}

// ---- handleApiTagRuleConditionSuggestions --------------------------------------------------------

func TestHandleApiTagRuleConditionSuggestions_Defaults(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiTagRuleConditionSuggestions(w, httptest.NewRequest("GET", "/api/settings/tags/condition-suggestions", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"scope":"tag_rule"`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiTagRuleConditionSuggestions_TagKeyTarget(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiTagRuleConditionSuggestions(w, httptest.NewRequest("GET",
		"/api/settings/tags/condition-suggestions?scope=notification&target=tag_key", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"target":"tag_key"`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiTagRuleConditionSuggestions_UnknownTargetEmptySuggestions(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiTagRuleConditionSuggestions(w, httptest.NewRequest("GET",
		"/api/settings/tags/condition-suggestions?scope=notification&target=bogus", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"suggestions":[]`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiAiConversation --------------------------------------------------------------------

func TestHandleApiAiConversation_MissingParams(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiAiConversation(w, httptest.NewRequest("GET", "/api/ai/conversation", nil))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiConversation_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiAiConversation(w, httptest.NewRequest("GET", "/api/ai/conversation?ts=t1&service=svc", nil))
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiConversation_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiAiConversation(w, httptest.NewRequest("GET", "/api/ai/conversation?ts=t1&service=svc&trace_id=tr1&span_name=sp1", nil))
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiMetricsAnomaly --------------------------------------------------------------------

func TestHandleApiMetricsAnomaly_MissingParams(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiMetricsAnomaly(w, httptest.NewRequest("GET", "/api/metrics/anomaly", nil))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiMetricsAnomaly_QueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiMetricsAnomaly(w, httptest.NewRequest("GET", "/api/metrics/anomaly?service=svc&metric=cpu&attr_fp=fp1", nil))
	if w.Code != 400 {
		t.Fatalf("want 400 (query errors surface as 400 here), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiMetricsAnomaly_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result(
			[]string{"time", "value", "sample_count", "baseline_mean", "baseline_stddev",
				"baseline_lower", "baseline_upper", "anomaly_score", "anomaly_state", "metric_kind", "attr_fp"},
			[]any{"t1", 1.5, 10.0, 1.0, 0.1, 0.5, 2.0, 0.2, "normal", "gauge", "fp1"},
		), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiMetricsAnomaly(w, httptest.NewRequest("GET", "/api/metrics/anomaly?service=svc&metric=cpu", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "svc") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiAiHelperChats ---------------------------------------------------------------------

func TestHandleApiAiHelperChats_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiAiHelperChats(w, httptest.NewRequest("GET", "/api/ai/helper/chats", nil))
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiHelperChats_FilteredAndPaginated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result(
			[]string{"chat_id", "first_ts", "last_ts", "first_question", "first_request", "turn_count"},
			[]any{"c1", "t0", "t1", "How do I fix this error?", "", 3.0},
			[]any{"c2", "t0", "t1", "Show me the dashboard", "", 1.0},
		), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiAiHelperChats(w, httptest.NewRequest("GET", "/api/ai/helper/chats?q=error&limit=5", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "c1") || strings.Contains(w.Body.String(), "c2") {
		t.Fatalf("query filter should keep c1 and drop c2, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiHelperChats_SkipsBlankChatID(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result(
			[]string{"chat_id", "first_ts", "last_ts", "first_question", "first_request", "turn_count"},
			[]any{"", "t0", "t1", "", "", 1.0},
		), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiAiHelperChats(w, httptest.NewRequest("GET", "/api/ai/helper/chats", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"chats":[]`) {
		t.Fatalf("blank chat_id row should be skipped, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiDashboardsSpecTemplates ------------------------------------------------------------

func TestHandleApiDashboardsSpecTemplates(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	s.handleApiDashboardsSpecTemplates(w, httptest.NewRequest("GET", "/api/dashboards/spec/templates", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "templates") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- MCP endpoint (GET/POST) --------------------------------------------------------------------

func TestHandleMcpEndpointGet_Disabled(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"mcp.enabled": "0"})}
	w := httptest.NewRecorder()
	s.handleMcpEndpointGet(w, httptest.NewRequest("GET", "/mcp", nil))
	if w.Code != 503 {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleMcpEndpointGet_EnabledDescriptor(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleMcpEndpointGet(w, httptest.NewRequest("GET", "/mcp", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "protocolVersion") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleMcpEndpointGet_DelegatesPostToPostHandler(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	r.RemoteAddr = "192.0.2.10:1234"
	s.handleMcpEndpointGet(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "protocolVersion") {
		t.Fatalf("want delegated initialize response, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleMcpEndpointPost_Branches(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}

	t.Run("parse error -> -32700", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`not json`))
		r.RemoteAddr = "198.51.100.1:1"
		s.handleMcpEndpointPost(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "-32700") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("notifications/* -> 202 empty body", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		r.RemoteAddr = "198.51.100.2:1"
		s.handleMcpEndpointPost(w, r)
		if w.Code != 202 || w.Body.Len() != 0 {
			t.Fatalf("want 202 empty body, got %d: %q", w.Code, w.Body.String())
		}
	})

	t.Run("ping -> ok result", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"ping"}`))
		r.RemoteAddr = "198.51.100.3:1"
		s.handleMcpEndpointPost(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"result":{}`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("unauthenticated non-init method -> 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.RemoteAddr = "198.51.100.4:1"
		s.handleMcpEndpointPost(w, r)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "-32002") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("disabled -> 503", func(t *testing.T) {
		sDisabled := &server{db: storetest.SettingsDB(map[string]string{"mcp.enabled": "0"})}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.RemoteAddr = "198.51.100.5:1"
		sDisabled.handleMcpEndpointPost(w, r)
		if w.Code != 503 {
			t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("unknown method (authenticated) -> -32601", func(t *testing.T) {
		keyHash := hashMcpKey("test-key")
		sAuthed := &server{db: storetest.SettingsDB(map[string]string{
			"mcp.api_keys": `[{"id":"k1","key_hash":"` + keyHash + `"}]`,
		})}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"bogus/method"}`))
		r.RemoteAddr = "198.51.100.6:1"
		r.Header.Set("X-MCP-API-Key", "test-key")
		sAuthed.handleMcpEndpointPost(w, r)
		if w.Code != 404 || !strings.Contains(w.Body.String(), "-32601") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("tools/list (authenticated)", func(t *testing.T) {
		keyHash := hashMcpKey("test-key-2")
		sAuthed := &server{db: storetest.SettingsDB(map[string]string{
			"mcp.api_keys": `[{"id":"k2","key_hash":"` + keyHash + `"}]`,
		})}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
		r.RemoteAddr = "198.51.100.7:1"
		r.Header.Set("X-MCP-API-Key", "test-key-2")
		sAuthed.handleMcpEndpointPost(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"tools"`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestMcpAuthenticated(t *testing.T) {
	t.Run("empty header -> false", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		r := httptest.NewRequest("POST", "/mcp", nil)
		if s.mcpAuthenticated(r) {
			t.Fatal("want false for empty header")
		}
	})

	t.Run("empty registry -> false", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("X-MCP-API-Key", "anything")
		if s.mcpAuthenticated(r) {
			t.Fatal("want false for empty registry")
		}
	})

	t.Run("matching hash -> true", func(t *testing.T) {
		keyHash := hashMcpKey("secret-key")
		s := &server{db: storetest.SettingsDB(map[string]string{
			"mcp.api_keys": `[{"id":"k1","key_hash":"` + keyHash + `"}]`,
		})}
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("X-MCP-API-Key", "secret-key")
		if !s.mcpAuthenticated(r) {
			t.Fatal("want true for matching hash")
		}
	})

	t.Run("non-matching hash -> false", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{
			"mcp.api_keys": `[{"id":"k1","key_hash":"deadbeef"}]`,
		})}
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("X-MCP-API-Key", "wrong-key")
		if s.mcpAuthenticated(r) {
			t.Fatal("want false for non-matching hash")
		}
	})

	t.Run("non-object list entries are skipped", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"mcp.api_keys": `["not-an-object"]`})}
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("X-MCP-API-Key", "whatever")
		if s.mcpAuthenticated(r) {
			t.Fatal("want false")
		}
	})
}

func TestHandleMcpListTools(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	s.handleMcpListTools(w, httptest.NewRequest("GET", "/mcp/tools", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "tools") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiMcpKeys (GET) ----------------------------------------------------------------------

func TestHandleApiMcpKeys_Get(t *testing.T) {
	t.Run("no setting -> empty keys", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		w := httptest.NewRecorder()
		s.handleApiMcpKeys(w, httptest.NewRequest("GET", "/api/mcp/keys", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"keys":[]`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("populated keys, expires_at defaults to null when absent", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{
			"mcp.api_keys": `[{"id":"k1","label":"CI key","created_at":"2026-01-01"}]`,
		})}
		w := httptest.NewRecorder()
		s.handleApiMcpKeys(w, httptest.NewRequest("GET", "/api/mcp/keys", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "CI key") || !strings.Contains(w.Body.String(), `"expires_at":null`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("malformed JSON setting -> empty keys", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"mcp.api_keys": `[not json`})}
		w := httptest.NewRecorder()
		s.handleApiMcpKeys(w, httptest.NewRequest("GET", "/api/mcp/keys", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"keys":[]`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-map list entries are skipped", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"mcp.api_keys": `["not-a-map"]`})}
		w := httptest.NewRecorder()
		s.handleApiMcpKeys(w, httptest.NewRequest("GET", "/api/mcp/keys", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"keys":[]`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})
}

// ---- orderedMapFromKV ---------------------------------------------------------------------------

func TestOrderedMapFromKV(t *testing.T) {
	got := orderedMapFromKV([]any{"a", "b"}, []any{"1", "2"})
	if v, _ := got.Get("a"); v != "1" {
		t.Fatalf("key a: got %v", v)
	}
	if v, _ := got.Get("b"); v != "2" {
		t.Fatalf("key b: got %v", v)
	}

	t.Run("non-string key is skipped", func(t *testing.T) {
		got := orderedMapFromKV([]any{"a", 5}, []any{"1", "2"})
		if got.Len() != 1 {
			t.Fatalf("want 1 key, got %d: %v", got.Len(), got)
		}
	})

	t.Run("values array shorter than keys pads with empty string", func(t *testing.T) {
		got := orderedMapFromKV([]any{"a", "b"}, []any{"1"})
		if v, _ := got.Get("b"); v != "" {
			t.Fatalf("want empty string for missing value, got %v", v)
		}
	})

	t.Run("JSON-array strings are parsed as a fallback", func(t *testing.T) {
		got := orderedMapFromKV(`["a"]`, `["1"]`)
		if v, _ := got.Get("a"); v != "1" {
			t.Fatalf("got %v", v)
		}
	})

	t.Run("non-array input yields empty object", func(t *testing.T) {
		got := orderedMapFromKV(5, 5)
		if got.Len() != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})
}

// ==================================== fix_pages_settings.go ====================================

// ---- normalizeNotificationCondition / condWindowMinutes / pyIntTrunc ---------------------------

func TestNormalizeNotificationConditionTagAndSignalBranches(t *testing.T) {
	t.Run("tag type normalizes with invalid enums falling back to defaults", func(t *testing.T) {
		raw := jsonenc.NewObject().
			Set("type", "TAG").
			Set("record_type", "bogus").
			Set("tag_match_operator", "bogus").
			Set("comparator", "bogus").
			Set("tag_key", "env").
			Set("tag_value", "prod").
			Set("threshold", 3).
			Set("window_minutes", 999)
		got := normalizeNotificationCondition(raw)
		if got["type"] != "tag" || got["record_type"] != "all" || got["tag_match_operator"] != "eq" ||
			got["comparator"] != "gt" || got["window_minutes"] != 60 {
			t.Fatalf("unexpected: %+v", got)
		}
	})

	t.Run("signal type default comparator/window", func(t *testing.T) {
		raw := jsonenc.NewObject().Set("source", "cpu").Set("signal", "usage").Set("service", "svc-a")
		got := normalizeNotificationCondition(raw)
		if got["type"] != "signal" || got["comparator"] != "gt" || got["window_minutes"] != 5 {
			t.Fatalf("unexpected: %+v", got)
		}
	})
}

func TestCondWindowMinutes(t *testing.T) {
	cases := []struct {
		name string
		v    any
		has  bool
		want int
	}{
		{"absent -> 5", nil, false, 5},
		{"zero (falsy) -> 5", 0, true, 5},
		{"int 30", 30, true, 30},
		{"float 45.9 truncates", 45.9, true, 45},
		{"clamped above 60", 999, true, 60},
		{"clamped below 1", -5, true, 1},
		{"numeric string", "20", true, 20},
		{"unparseable string -> 5", "abc", true, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := jsonenc.NewObject()
			if c.has {
				o.Set("window_minutes", c.v)
			}
			if got := condWindowMinutes(o); got != c.want {
				t.Errorf("condWindowMinutes(%v) = %d, want %d", c.v, got, c.want)
			}
		})
	}
}

func TestPyIntTrunc(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		wantN  int
		wantOK bool
	}{
		{"bool true", true, 1, true},
		{"bool false", false, 0, true},
		{"int", 7, 7, true},
		{"int64", int64(8), 8, true},
		{"float truncates toward zero", 5.9, 5, true},
		{"negative float truncates toward zero", -5.9, -5, true},
		{"numeric string", "42", 42, true},
		{"non-numeric string", "abc", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok := pyIntTrunc(c.in)
			if n != c.wantN || ok != c.wantOK {
				t.Errorf("pyIntTrunc(%v) = (%d,%v), want (%d,%v)", c.in, n, ok, c.wantN, c.wantOK)
			}
		})
	}
}

// ---- loadConfirmedAiPricingModels / sortedConfirmedAiPricingModels -----------------------------

func TestLoadConfirmedAiPricingModels(t *testing.T) {
	t.Run("empty setting -> empty set", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.loadConfirmedAiPricingModels(); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("malformed JSON -> empty set", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"ai.model_pricing_confirmed": `[bad`})}
		if got := s.loadConfirmedAiPricingModels(); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("non-list JSON -> empty set", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"ai.model_pricing_confirmed": `{"a":1}`})}
		if got := s.loadConfirmedAiPricingModels(); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("populated and sorted", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{
			"ai.model_pricing_confirmed": `["GPT-4O", " claude-3-opus "]`,
		})}
		got := s.sortedConfirmedAiPricingModels()
		if len(got) != 2 || got[0] != "claude-3-opus" || got[1] != "gpt-4o" {
			t.Fatalf("unexpected: %v", got)
		}
	})
}

// ---- loadSavedAiPricing / coerceAiPricingEntry -------------------------------------------------

// ---- inferAiPricingForModel / copyAiPricingEntry ------------------------------------------------

func TestInferAiPricingForModelNeedleRules(t *testing.T) {
	defaults := jsonenc.NewObject().
		Set("gpt-4o", jsonenc.NewObject().Set("in", 2.5).Set("out", 10.0)).
		Set("gpt-4o-mini", jsonenc.NewObject().Set("in", 0.15).Set("out", 0.6)).
		Set("claude-3-5-sonnet", jsonenc.NewObject().Set("in", 3.0).Set("out", 15.0))

	t.Run("blank model -> generic gpt-4o", func(t *testing.T) {
		got := inferAiPricingForModel(defaults, "").(*jsonenc.Object)
		in, _ := got.Get("in")
		if in != 2.5 {
			t.Fatalf("want gpt-4o default, got %v", got)
		}
	})

	t.Run("exact known key", func(t *testing.T) {
		got := inferAiPricingForModel(defaults, "gpt-4o-mini").(*jsonenc.Object)
		in, _ := got.Get("in")
		if in != 0.15 {
			t.Fatalf("want gpt-4o-mini pricing, got %v", got)
		}
	})

	t.Run("substring match against a known key", func(t *testing.T) {
		got := inferAiPricingForModel(defaults, "gpt-4o-2024-08-06").(*jsonenc.Object)
		in, _ := got.Get("in")
		if in != 2.5 {
			t.Fatalf("want gpt-4o substring match, got %v", got)
		}
	})

	t.Run("inference rule needle match (haiku not in defaults, falls to generic)", func(t *testing.T) {
		got := inferAiPricingForModel(defaults, "claude-3-5-haiku-20241022").(*jsonenc.Object)
		// "haiku" rule maps to "claude-3-5-haiku" which ISN'T in our small defaults set here, so
		// copyEntry falls back to the generic gpt-4o via copyAiPricingEntry(mustGet(defaults, "gpt-4o")).
		in, _ := got.Get("in")
		if in != 2.5 {
			t.Fatalf("want fallback to gpt-4o generic, got %v", got)
		}
	})

	t.Run("sonnet needle matches an existing default key directly", func(t *testing.T) {
		got := inferAiPricingForModel(defaults, "claude-3-5-sonnet-20241022").(*jsonenc.Object)
		in, _ := got.Get("in")
		if in != 3.0 {
			t.Fatalf("want claude-3-5-sonnet pricing via substring match, got %v", got)
		}
	})

	t.Run("no rule matches -> generic gpt-4o", func(t *testing.T) {
		got := inferAiPricingForModel(defaults, "totally-unknown-model").(*jsonenc.Object)
		in, _ := got.Get("in")
		if in != 2.5 {
			t.Fatalf("want generic fallback, got %v", got)
		}
	})
}

func TestCopyAiPricingEntry(t *testing.T) {
	src := jsonenc.NewObject().Set("in", 1.0).Set("out", 2.0)
	got := copyAiPricingEntry(src).(*jsonenc.Object)
	in, _ := got.Get("in")
	outv, _ := got.Get("out")
	if in != 1.0 || outv != 2.0 {
		t.Fatalf("unexpected copy: %v", got)
	}
	if !copyAiPricingEntryIsIndependent(src, got) {
		t.Fatalf("copy should not alias the source object")
	}
	// non-object input -> empty object.
	if empty := copyAiPricingEntry("not-an-object").(*jsonenc.Object); empty.Len() != 0 {
		t.Fatalf("want empty object for non-object input, got %v", empty)
	}
}

// copyAiPricingEntryIsIndependent mutates the copy and confirms the source is unaffected.
func copyAiPricingEntryIsIndependent(src, cpy *jsonenc.Object) bool {
	cpy.Set("in", 999.0)
	v, _ := src.Get("in")
	return v == 1.0
}

// ---- loadAiPricingWithSources -------------------------------------------------------------------

func TestLoadAiPricingWithSources_EmptyFixtureMatchesDefaults(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	merged, sources := s.loadAiPricingWithSources()
	if merged.Len() == 0 {
		t.Fatal("want the embedded defaults to populate merged")
	}
	if sources.Len() != merged.Len() {
		t.Fatalf("sources/merged key count mismatch: %d vs %d", sources.Len(), merged.Len())
	}
	for _, k := range sources.Keys() {
		v, _ := sources.Get(k)
		if v != "default" {
			t.Fatalf("key %q: want source 'default' on the empty fixture, got %v", k, v)
		}
	}
}

func TestLoadAiPricingWithSources_ObservedAndSavedOverrides(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "otel_traces") && strings.Contains(q, "DISTINCT"):
			return storetest.Result([]string{"model"}, []any{"some-new-observed-model"}), nil
		case (strings.Contains(q, "sobs_app_settings") || strings.Contains(q, "sobs_ai_settings")) && len(params) == 1:
			if params[0] == "ai.model_pricing" {
				return storetest.Result([]string{"Value"}, []any{
					`{"gpt-4o": {"in": 99, "out": 199}, "some-new-observed-model": {"in": 1, "out": 2}}`,
				}), nil
			}
			if params[0] == "ai.model_pricing_confirmed" {
				return storetest.Result([]string{"Value"}, []any{`["some-new-observed-model"]`}), nil
			}
		}
		return &store.Result{}, nil
	}}}
	merged, sources := s.loadAiPricingWithSources()
	if _, ok := merged.Get("some-new-observed-model"); !ok {
		t.Fatalf("observed model should be inferred into merged: %v", merged)
	}
	if src, _ := sources.Get("some-new-observed-model"); src != "confirmed" {
		t.Fatalf("observed+confirmed+saved model should be marked confirmed, got %v", src)
	}
	gpt4o, _ := merged.Get("gpt-4o")
	in, _ := gpt4o.(*jsonenc.Object).Get("in")
	if in != 99.0 {
		t.Fatalf("saved override should replace the default gpt-4o pricing, got %v", in)
	}
}
