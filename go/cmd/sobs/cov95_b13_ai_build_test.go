package main

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b13_ai_build_test.go — batch 13: unit tests for cmd/sobs/ai_build.go's generateNamedQueries,
// generateChartSpec, and the handleApiDashboardsSpecAiBuild handler's early-return guard branches.
// Reuses the llmChatServer/llmTestServer test helpers already defined in cov95_b8_fix_query_test.go
// (same package) which stand up a real httptest.Server answering the OpenAI-compatible
// /chat/completions shape, with SOBS_UPSTREAM_FIXTURES cleared so callLLMChat takes the real-HTTP
// path instead of the file-fixture path.

// ---- generateNamedQueries -----------------------------------------------------------------------

func TestGenerateNamedQueries_Success(t *testing.T) {
	reply := "```json\n" + `{"datasets":[
		{"name":"bad_sql","sql":"DELETE FROM x","purpose":"not select"},
		{"name":"same_as_base","sql":"SELECT 1 AS a","purpose":"identical to base"},
		{"name":"Aux_Dataset","sql":"SELECT 2 AS b;","purpose":"aux"}
	]}` + "\n```"
	endpoint := llmChatServer(t, reply)
	s := llmTestServer()
	out := s.generateNamedQueries(endpoint, "how many?", "SELECT 1 AS a", "bar", "make it colorful")
	if len(out) != 1 {
		t.Fatalf("want 1 surviving dataset, got %d: %#v", len(out), out)
	}
	ds := out[0].(*jsonenc.Object)
	if name, _ := ds.Get("name"); name != "aux_dataset" {
		t.Errorf("name = %v, want lowercased aux_dataset", name)
	}
	if sql, _ := ds.Get("sql"); sql != "SELECT 2 AS b" {
		t.Errorf("sql = %v, want trailing ; stripped", sql)
	}
}

func TestGenerateNamedQueries_EmptyDatasetsArray(t *testing.T) {
	endpoint := llmChatServer(t, `{"datasets":[]}`)
	s := llmTestServer()
	out := s.generateNamedQueries(endpoint, "q", "SELECT 1", "", "")
	if len(out) != 0 {
		t.Errorf("want 0 datasets, got %#v", out)
	}
}

func TestGenerateNamedQueries_NonJSONReplyYieldsEmpty(t *testing.T) {
	endpoint := llmChatServer(t, "not json at all")
	s := llmTestServer()
	out := s.generateNamedQueries(endpoint, "q", "SELECT 1", "", "")
	if len(out) != 0 {
		t.Errorf("want 0 datasets for non-JSON reply, got %#v", out)
	}
}

func TestGenerateNamedQueries_DatasetsNotAList(t *testing.T) {
	endpoint := llmChatServer(t, `{"datasets":"not-a-list"}`)
	s := llmTestServer()
	out := s.generateNamedQueries(endpoint, "q", "SELECT 1", "", "")
	if len(out) != 0 {
		t.Errorf("want 0 datasets, got %#v", out)
	}
}

func TestGenerateNamedQueries_CapsAtThreeAndFiltersBadName(t *testing.T) {
	reply := `{"datasets":[
		{"name":"ok1","sql":"SELECT 1"},
		{"name":"Bad Name!","sql":"SELECT 2"},
		{"name":"ok2","sql":"SELECT 3"},
		{"name":"ok3","sql":"SELECT 4"},
		{"name":"ok4_excluded_by_cap","sql":"SELECT 5"}
	]}`
	endpoint := llmChatServer(t, reply)
	s := llmTestServer()
	out := s.generateNamedQueries(endpoint, "q", "SELECT 0", "", "")
	// Only the first 3 entries are even considered (i>=3 break); of those, "Bad Name!" is filtered
	// by the name regex, leaving ok1 and ok2.
	if len(out) != 2 {
		t.Fatalf("want 2 surviving datasets (of first 3 considered), got %d: %#v", len(out), out)
	}
}

func TestGenerateNamedQueries_LLMErrorYieldsEmpty(t *testing.T) {
	s := llmTestServer()
	// No endpoint configured/reachable -> callLLMChat errors -> generateNamedQueries returns [].
	out := s.generateNamedQueries("http://127.0.0.1:1", "q", "SELECT 1", "", "")
	if len(out) != 0 {
		t.Errorf("want 0 datasets on LLM error, got %#v", out)
	}
}

// ---- generateChartSpec ----------------------------------------------------------------------------

func TestGenerateChartSpec_Success(t *testing.T) {
	endpoint := llmChatServer(t, `{"tooltip":{"trigger":"axis"},"series":[{"type":"line"}]}`)
	s := llmTestServer()
	columns := []any{"a", "b"}
	sample := []any{jsonenc.NewObject().Set("a", 1).Set("b", 2)}
	named := []any{jsonenc.NewObject().Set("name", "main").Set("purpose", "p").
		Set("columns", columns).Set("rows", []any{[]any{1, 2}})}
	specJSON, errMsg := s.generateChartSpec(endpoint, "chart this", columns, sample, named, "line", "make it blue")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if specJSON == "" {
		t.Error("want non-empty spec JSON")
	}
}

func TestGenerateChartSpec_EmptyReplyErrors(t *testing.T) {
	endpoint := llmChatServer(t, "")
	s := llmTestServer()
	_, errMsg := s.generateChartSpec(endpoint, "q", []any{"a"}, nil, nil, "", "")
	if errMsg != "LLM did not return a chart spec." {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestGenerateChartSpec_EmptyObjectErrors(t *testing.T) {
	endpoint := llmChatServer(t, "{}")
	s := llmTestServer()
	_, errMsg := s.generateChartSpec(endpoint, "q", []any{"a"}, nil, nil, "", "")
	if errMsg != "LLM returned an empty chart spec object." {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestGenerateChartSpec_MoreThan20SampleRowsTruncated(t *testing.T) {
	endpoint := llmChatServer(t, `{"series":[{"type":"bar"}]}`)
	s := llmTestServer()
	rows := make([]any, 30)
	for i := range rows {
		rows[i] = jsonenc.NewObject().Set("x", i)
	}
	_, errMsg := s.generateChartSpec(endpoint, "q", []any{"x"}, rows, nil, "", "")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
}

func TestGenerateChartSpec_NamedDatasetsCondensed(t *testing.T) {
	endpoint := llmChatServer(t, `{"series":[{"type":"bar"}]}`)
	s := llmTestServer()
	rows := make([]any, 25)
	for i := range rows {
		rows[i] = i
	}
	named := []any{
		jsonenc.NewObject().Set("name", "aux").Set("purpose", "p").Set("columns", []any{"x"}).Set("rows", rows),
		"not-an-object", // non-object dataset entries must be skipped safely
	}
	_, errMsg := s.generateChartSpec(endpoint, "q", []any{"x"}, nil, named, "", "")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
}

// Note: chartSpecParseError, buildFallbackCustomOptionJSON, and objGetOr already have dedicated
// tests in ai_build_chat_helpers_test.go (pre-existing file, not touched here) — skipped to avoid
// duplicate test-function names in the shared package.

// ---- handleApiDashboardsSpecAiBuild: early-return guard branches -----------------------------------

func TestHandleApiDashboardsSpecAiBuild_MissingQuestion(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest("POST", "/api/dashboards/spec/ai-build", bytes.NewBufferString(`{"question":"  "}`))
	rec := httptest.NewRecorder()
	s.handleApiDashboardsSpecAiBuild(rec, req)
	if rec.Code != 400 {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestHandleApiDashboardsSpecAiBuild_EndpointNotConfigured(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{})}
	req := httptest.NewRequest("POST", "/api/dashboards/spec/ai-build", bytes.NewBufferString(`{"question":"how many errors?"}`))
	rec := httptest.NewRecorder()
	s.handleApiDashboardsSpecAiBuild(rec, req)
	if rec.Code != 503 {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("AI endpoint not configured")) {
		t.Errorf("body = %s", rec.Body.String())
	}
}
