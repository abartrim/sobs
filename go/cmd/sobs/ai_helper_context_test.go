package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

func TestTextEmbeddingAndCosine(t *testing.T) {
	// A unit-normalized vector dotted with itself is 1.0.
	v := textEmbedding("error rate p99 latency")
	if got := cosineSimilarity(v, v); got < 0.999 || got > 1.001 {
		t.Fatalf("self-cosine = %v, want ~1.0", got)
	}
	// Empty text → zero vector → cosine 0.
	if got := cosineSimilarity(textEmbedding(""), v); got != 0 {
		t.Fatalf("empty-vs-x cosine = %v, want 0", got)
	}
	// Unrelated texts score below identical, and embeddings are deterministic.
	a := textEmbedding("show me kubernetes pods")
	b := textEmbedding("billing invoice totals")
	if cosineSimilarity(a, b) >= cosineSimilarity(a, a) {
		t.Fatalf("unrelated cosine should be < self cosine")
	}
	if cosineSimilarity(textEmbedding("repeatable"), textEmbedding("repeatable")) < 0.999 {
		t.Fatalf("embedding must be deterministic")
	}
}

func TestEmbeddingFromJSON(t *testing.T) {
	emb := embeddingFromJSON("[0.5, 0.25, -0.125]")
	if len(emb) != 3 || emb[0] != 0.5 || emb[2] != -0.125 {
		t.Fatalf("embeddingFromJSON = %v", emb)
	}
	if got := embeddingFromJSON("  "); got != nil {
		t.Fatalf("blank → nil, got %v", got)
	}
	if got := embeddingFromJSON("not json"); got != nil {
		t.Fatalf("bad json → nil, got %v", got)
	}
}

func TestModelSupportsTools(t *testing.T) {
	for _, m := range []string{"gpt-oss:120b-cloud", "llama-guard", "qwen3", "mistral-large", "tool-use", "code-instruct"} {
		if !modelSupportsTools(m) {
			t.Errorf("modelSupportsTools(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "deepseek-r1", "claude"} {
		if modelSupportsTools(m) {
			t.Errorf("modelSupportsTools(%q) = true, want false", m)
		}
	}
}

func TestParseBoolAttr(t *testing.T) {
	cases := []struct {
		in  string
		def bool
		out bool
	}{
		{"true", false, true}, {"1", false, true}, {"yes", false, true}, {"on", false, true},
		{"false", true, false}, {"0", true, false}, {"no", true, false}, {"off", true, false},
		{"", true, true}, {"", false, false}, {"weird", true, true}, {"weird", false, false},
	}
	for _, c := range cases {
		if got := parseBoolAttr(c.in, c.def); got != c.out {
			t.Errorf("parseBoolAttr(%q, %v) = %v, want %v", c.in, c.def, got, c.out)
		}
	}
}

func TestHelperActionManifestAndTools(t *testing.T) {
	dir := t.TempDir()
	// logs.html (the /logs template) with one implemented action and one annotation-only action.
	html := `<div>
  <button data-ai-action-id="logs.filter.apply_sql" data-ai-action-type="filter"
          data-ai-handler="applyLogsFilter" data-ai-label="Apply SQL" data-ai-risk="low"
          data-ai-confirm="false" data-ai-args='{"scope":"page"}'>Apply</button>
  <a data-ai-action-id="logs.export" data-ai-action-type="export">Export</a>
</div>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{TemplateDir: dir}}

	manifest := s.helperActionManifestForPage("/logs")
	if len(manifest) != 2 {
		t.Fatalf("manifest length = %d, want 2", len(manifest))
	}
	// Sorted by action_id: logs.export, logs.filter.apply_sql.
	if id, _ := manifest[0].Get("action_id"); id != "logs.export" {
		t.Fatalf("manifest[0].action_id = %v, want logs.export", id)
	}
	apply := manifest[1]
	if v, _ := apply.Get("implemented"); v != true {
		t.Errorf("apply_sql implemented = %v, want true (has handler)", v)
	}
	if v, _ := apply.Get("requires_confirmation"); v != false {
		t.Errorf("apply_sql requires_confirmation = %v, want false", v)
	}
	if v, _ := apply.Get("risk"); v != "low" {
		t.Errorf("apply_sql risk = %v, want low", v)
	}
	if v, _ := apply.Get("label"); v != "Apply SQL" {
		t.Errorf("apply_sql label = %v", v)
	}
	// export has no handler → implemented false, and label defaults to the action_id.
	export := manifest[0]
	if v, _ := export.Get("implemented"); v != false {
		t.Errorf("export implemented = %v, want false", v)
	}
	if v, _ := export.Get("label"); v != "logs.export" {
		t.Errorf("export label = %v, want logs.export (default)", v)
	}

	// Tools: one action is implemented → the generic propose_ui_action tool.
	tools := s.helperToolsForPage("/logs")
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1", len(tools))
	}

	// An unknown page has no template → empty manifest, no tools.
	if m := s.helperActionManifestForPage("/nope"); m != nil {
		t.Errorf("unknown page manifest = %v, want nil", m)
	}
	if tl := s.helperToolsForPage("/nope"); tl != nil {
		t.Errorf("unknown page tools = %v, want nil", tl)
	}
}

func TestAiHelperGenericUIActionToolShape(t *testing.T) {
	tool := aiHelperGenericUIActionTool()
	if v, _ := tool.Get("type"); v != "function" {
		t.Fatalf("tool type = %v", v)
	}
	fn, _ := tool.Get("function")
	fnObj, ok := fn.(*jsonenc.Object)
	if !ok {
		t.Fatal("function is not a *jsonenc.Object")
	}
	if name, _ := fnObj.Get("name"); name != "propose_ui_action" {
		t.Fatalf("tool function name = %v", name)
	}
}

func TestAiHelperUserContent(t *testing.T) {
	// Page only.
	if got := aiHelperUserContent("why errors?", "/logs", nil); got != "Current page: /logs\n\nQuestion: why errors?" {
		t.Fatalf("page-only user content = %q", got)
	}
	// No page, no context → bare question.
	if got := aiHelperUserContent("hello", "", nil); got != "hello" {
		t.Fatalf("bare user content = %q", got)
	}
	// Page + context (sorted keys, falsy values dropped).
	ctx := map[string]any{"service": "checkout", "empty": "", "region": "us-east"}
	got := aiHelperUserContent("q", "/logs", ctx)
	want := "Current page: /logs\nregion: us-east\nservice: checkout\n\nQuestion: q"
	if got != want {
		t.Fatalf("context user content = %q, want %q", got, want)
	}
}
