package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 7: cmd/sobs/ai_helper_context.go — the remaining branches of helperActionManifestForPage/
// helperToolsForPage (the /dashboards/_detail fallback + the manifest-args-parse-error path),
// loadChatMemories, semanticMemoryMatches, consolidateMemoryCandidates, loadRecentTurnSummaries,
// buildAIHelperContext, cosineSimilarity's mismatched-length branch, embeddingFromJSON's
// json.Number/float64 item kinds. ai_helper_context_test.go already covers the happy path (single
// action manifest, tools-for-page, text embedding/cosine self-similarity); this file targets what
// it does not.

// ---- helperActionManifestForPage / helperToolsForPage ----

func TestHelperActionManifestForPage_DashboardDetailFallback(t *testing.T) {
	dir := t.TempDir()
	html := `<button data-ai-action-id="dashboards.widget.edit" data-ai-action-type="edit_widget"
          data-ai-handler="editWidget" data-ai-label="Edit widget">Edit</button>`
	if err := os.WriteFile(filepath.Join(dir, "custom_dashboard_view.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}}
	// "/dashboards/abc123" has no direct template mapping, but the /dashboards/ prefix falls back
	// to the "/dashboards/_detail" template set (custom_dashboard_view.html).
	manifest := s.helperActionManifestForPage("/dashboards/abc123")
	if len(manifest) != 1 {
		t.Fatalf("manifest length = %d, want 1 (dashboard-detail fallback)", len(manifest))
	}
	if id := objStrOr(manifest[0], "action_id"); id != "dashboards.widget.edit" {
		t.Errorf("action_id = %q", id)
	}
}

func TestHelperActionManifestForPage_BlankPageDefaultsToLogs(t *testing.T) {
	dir := t.TempDir()
	html := `<a data-ai-action-id="logs.export" data-ai-action-type="export">Export</a>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}}
	manifest := s.helperActionManifestForPage("   ")
	if len(manifest) != 1 || objStrOr(manifest[0], "action_id") != "logs.export" {
		t.Fatalf("blank page should default to /logs manifest, got %v", manifest)
	}
}

func TestHelperActionManifestForPage_BadArgsJSONFallsBackToEmpty(t *testing.T) {
	dir := t.TempDir()
	// data-ai-args is not valid JSON -> arguments stays a fresh empty object (not a parse panic).
	html := `<button data-ai-action-id="logs.filter.bad" data-ai-action-type="filter"
          data-ai-handler="applyFilter" data-ai-args='{not valid json'>Apply</button>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}}
	manifest := s.helperActionManifestForPage("/logs")
	if len(manifest) != 1 {
		t.Fatalf("manifest length = %d, want 1", len(manifest))
	}
	args, ok := objSub(manifest[0], "arguments")
	if !ok || args == nil || args.Len() != 0 {
		t.Errorf("arguments should be an empty object on bad JSON, got %v", args)
	}
}

func TestHelperToolsForPage_NoImplementedActions(t *testing.T) {
	dir := t.TempDir()
	// Only an annotation-only action (no data-ai-handler) -> implemented=false for all -> no tools.
	html := `<a data-ai-action-id="logs.export" data-ai-action-type="export">Export</a>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}}
	if tools := s.helperToolsForPage("/logs"); tools != nil {
		t.Errorf("no implemented actions -> nil tools, got %v", tools)
	}
}

// ---- cosineSimilarity mismatched lengths ----

func TestCosineSimilarity_MismatchedLengths(t *testing.T) {
	a := []float64{1, 0, 0, 0}
	b := []float64{1, 0} // shorter -> only the shared prefix is dotted
	if got := cosineSimilarity(a, b); got != 1.0 {
		t.Errorf("mismatched-length cosine = %v, want 1.0 (shared prefix dot)", got)
	}
}

// ---- embeddingFromJSON item kinds ----

func TestEmbeddingFromJSON_MixedItemKinds(t *testing.T) {
	// A non-numeric array element (e.g. a nested object) coerces to 0, not a panic.
	got := embeddingFromJSON(`[1.5, "x", null, true]`)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0] != 1.5 {
		t.Errorf("got[0] = %v, want 1.5", got[0])
	}
	for i, v := range got[1:] {
		if v != 0 {
			t.Errorf("got[%d] = %v, want 0 for non-numeric item", i+1, v)
		}
	}
}

func TestEmbeddingFromJSON_NotAnArray(t *testing.T) {
	if got := embeddingFromJSON(`{"a":1}`); got != nil {
		t.Errorf("object (not array) input should return nil, got %v", got)
	}
}

// ---- loadChatMemories / semanticMemoryMatches ----

func chatMemoriesDB(rows []map[string]any) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_ai_memories") {
			cols := []string{"Id", "MemoryText", "EmbeddingJson", "SourceTurnId", "UpdatedAt"}
			out := make([][]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, []any{r["Id"], r["MemoryText"], r["EmbeddingJson"], r["SourceTurnId"], r["UpdatedAt"]})
			}
			return &store.Result{Columns: cols, Rows: out}, nil
		}
		return &store.Result{}, nil
	}}
}

func TestLoadChatMemories_HappyAndError(t *testing.T) {
	s := &server{db: chatMemoriesDB([]map[string]any{
		{"Id": "m1", "MemoryText": "  likes dashboards  ", "EmbeddingJson": "", "SourceTurnId": "t1", "UpdatedAt": "2026-01-01"},
	})}
	got := s.loadChatMemories("chat-1")
	if len(got) != 1 || got[0].text != "likes dashboards" {
		t.Fatalf("loadChatMemories = %+v", got)
	}

	errDB := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return nil, assertErr("boom")
	}}
	sErr := &server{db: errDB}
	if got := sErr.loadChatMemories("chat-1"); got != nil {
		t.Errorf("DB error should yield nil, got %v", got)
	}
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
func assertErr(s string) error    { return simpleErr(s) }

func TestSemanticMemoryMatches_MinScoreAndMaxResults(t *testing.T) {
	memories := []chatMemory{
		{id: "m1", text: "error rate p99 latency spikes"},
		{id: "m2", text: "error rate p99 latency spikes again"},
		{id: "m3", text: "totally unrelated billing invoice text"},
	}
	got := semanticMemoryMatches(memories, "error rate p99 latency", 1, 0.5)
	if len(got) != 1 {
		t.Fatalf("maxResults=1 should cap results, got %d", len(got))
	}
	if got[0].id != "m1" && got[0].id != "m2" {
		t.Errorf("top match should be one of the similar memories, got %s", got[0].id)
	}
	// A very high min score excludes everything.
	none := semanticMemoryMatches(memories, "error rate p99 latency", 5, 0.999)
	if len(none) != 0 {
		t.Errorf("min score 0.999 should exclude all, got %d", len(none))
	}
}

func TestSemanticMemoryMatches_FallsBackToTextEmbeddingWhenStoredEmbeddingEmpty(t *testing.T) {
	memories := []chatMemory{{id: "m1", text: "trace latency spike", embedding: nil}}
	got := semanticMemoryMatches(memories, "trace latency spike", 5, 0.9)
	if len(got) != 1 {
		t.Fatalf("expected a self-similar match via the recomputed embedding, got %d", len(got))
	}
}

// ---- consolidateMemoryCandidates ----

func TestConsolidateMemoryCandidates_NotConfiguredKeepsNew(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	got := s.consolidateMemoryCandidates("new fact", nil)
	if got.action != "keep_new" || got.memory != "new fact" {
		t.Errorf("got %+v, want keep_new/new fact", got)
	}
}

func TestConsolidateMemoryCandidates_MergeDecisionViaFixture(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	reply := `{"action":"merge","memory":"merged fact","drop_ids":["old-1","old-2"]}`
	writeChatCompletionFixture(t, dir, "http://llm.test", 200, reply)
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.endpoint_url": "http://llm.test",
		"ai.model":        "gpt-4o",
	}), wq: newWQ()}
	got := s.consolidateMemoryCandidates("new fact", []memoryMatch{{id: "old-1", text: "old", score: 0.9}})
	if got.action != "merge" {
		t.Fatalf("action = %q, want merge", got.action)
	}
	if got.memory != "merged fact" {
		t.Errorf("memory = %q", got.memory)
	}
	if len(got.dropIDs) != 2 || got.dropIDs[0] != "old-1" {
		t.Errorf("dropIDs = %v", got.dropIDs)
	}
}

func TestConsolidateMemoryCandidates_InvalidActionFallsBackToKeepNew(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// "action" is present but not one of merge/keep_new/ignore -> keep_new.
	reply := `{"action":"discard_everything","memory":"whatever"}`
	writeChatCompletionFixture(t, dir, "http://llm.test", 200, reply)
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.endpoint_url": "http://llm.test",
		"ai.model":        "gpt-4o",
	}), wq: newWQ()}
	got := s.consolidateMemoryCandidates("new fact", nil)
	if got.action != "keep_new" {
		t.Errorf("action = %q, want keep_new (invalid value normalized)", got.action)
	}
}

func TestConsolidateMemoryCandidates_IgnoreAction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeChatCompletionFixture(t, dir, "http://llm.test", 200, `{"action":"ignore"}`)
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.endpoint_url": "http://llm.test",
		"ai.model":        "gpt-4o",
	}), wq: newWQ()}
	got := s.consolidateMemoryCandidates("noise", nil)
	if got.action != "ignore" {
		t.Errorf("action = %q, want ignore", got.action)
	}
}

func TestConsolidateMemoryCandidates_EmptyOrBadReplyFallsBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// No fixture -> 404 -> callLLMChat errors/empty -> keep_new fallback.
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.endpoint_url": "http://llm-missing.test",
		"ai.model":        "gpt-4o",
	}), wq: newWQ()}
	got := s.consolidateMemoryCandidates("new fact", nil)
	if got.action != "keep_new" || got.memory != "new fact" {
		t.Errorf("got %+v, want keep_new/new fact fallback on empty reply", got)
	}
}

// ---- loadRecentTurnSummaries ----

func turnSummaryDB(rows []map[string]any) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "otel_logs") && strings.Contains(q, "turn.summary") {
			cols := []string{"Timestamp", "request", "action", "result", "turn_id"}
			out := make([][]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, []any{r["Timestamp"], r["request"], r["action"], r["result"], r["turn_id"]})
			}
			return &store.Result{Columns: cols, Rows: out}, nil
		}
		return &store.Result{}, nil
	}}
}

func TestLoadRecentTurnSummaries_RanksBySimilarityAndSkipsBlank(t *testing.T) {
	s := &server{db: turnSummaryDB([]map[string]any{
		{"Timestamp": "t1", "request": "how many errors in checkout service", "action": "query", "result": "5 errors", "turn_id": "turn-1"},
		{"Timestamp": "t2", "request": "", "action": "", "result": "", "turn_id": "turn-2"}, // blank -> skipped
		{"Timestamp": "t3", "request": "billing invoice totals", "action": "query", "result": "unrelated", "turn_id": "turn-3"},
	})}
	got := s.loadRecentTurnSummaries("chat-1", "how many errors in checkout", 5)
	for _, ts := range got {
		if ts.turnID == "turn-2" {
			t.Error("blank-request/result turn should have been skipped")
		}
	}
	if len(got) == 0 {
		t.Fatal("expected at least one semantically-relevant turn summary")
	}
}

func TestLoadRecentTurnSummaries_DBErrorReturnsNil(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return nil, assertErr("boom")
	}}}
	if got := s.loadRecentTurnSummaries("chat-1", "q", 5); got != nil {
		t.Errorf("DB error should yield nil, got %v", got)
	}
}

// ---- buildAIHelperContext ----

func TestBuildAIHelperContext_AssemblesSystemPromptAndTools(t *testing.T) {
	dir := t.TempDir()
	html := `<button data-ai-action-id="logs.filter.apply_sql" data-ai-action-type="apply_sql_filter"
          data-ai-handler="applySqlFilter" data-ai-label="Apply SQL">Apply</button>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}, db: &storetest.FakeDB{}}
	systemPrompt, userContent, tools := s.buildAIHelperContext(
		"how many errors", "/logs", "chat-1", "gpt-4o-instruct", map[string]any{"service": "checkout"})
	if !strings.Contains(systemPrompt, "Page action manifest:") {
		t.Errorf("system prompt should splice in the action manifest, got %q", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "logs.filter.apply_sql") {
		t.Errorf("system prompt should mention the page's action id, got %q", systemPrompt)
	}
	if userContent != "Current page: /logs\nservice: checkout\n\nQuestion: how many errors" {
		t.Errorf("userContent = %q", userContent)
	}
	if len(tools) != 1 {
		t.Errorf("modelSupportsTools(gpt-4o-instruct) should yield the page's tool, got %v", tools)
	}
}

func TestBuildAIHelperContext_NoToolsForNonToolModel(t *testing.T) {
	dir := t.TempDir()
	html := `<button data-ai-action-id="logs.filter.apply_sql" data-ai-action-type="apply_sql_filter"
          data-ai-handler="applySqlFilter" data-ai-label="Apply SQL">Apply</button>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}, db: &storetest.FakeDB{}}
	_, _, tools := s.buildAIHelperContext("q", "/logs", "chat-1", "claude", nil)
	if tools != nil {
		t.Errorf("a model without tool support should get no tools, got %v", tools)
	}
}

func TestBuildAIHelperContext_CustomSystemPromptOverride(t *testing.T) {
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.system_prompt": "Custom prompt override.",
	})}
	systemPrompt, _, _ := s.buildAIHelperContext("q", "/logs", "chat-1", "claude", nil)
	if !strings.HasPrefix(systemPrompt, "Custom prompt override.") {
		t.Errorf("system prompt should start with the DB override, got %q", systemPrompt)
	}
	if strings.Contains(systemPrompt, "Page action manifest:") {
		t.Errorf("override should skip the default action-manifest splice, got %q", systemPrompt)
	}
}
