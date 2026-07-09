package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cov95_b16_ai_prompts_test.go — batch 16 targeted coverage for cmd/sobs/ai_prompts.go:
// loadChartTypesCatalog's missing-file / malformed-JSON / non-object / success branches, and
// buildChartRefinementPrompt's catalog-splice (with and without a usable catalog).

func TestLoadChartTypesCatalog(t *testing.T) {
	t.Run("missing file returns nil", func(t *testing.T) {
		s := &server{cfg: config{StaticDir: t.TempDir()}}
		if got := s.loadChartTypesCatalog(); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("malformed JSON returns nil", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "echarts-chart-types.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{StaticDir: dir}}
		if got := s.loadChartTypesCatalog(); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("valid JSON that is not an object returns nil", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "echarts-chart-types.json"), []byte(`[1,2,3]`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{StaticDir: dir}}
		if got := s.loadChartTypesCatalog(); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("valid catalog object parses", func(t *testing.T) {
		dir := t.TempDir()
		body := `{"chartTypes":{"bar":{"name":"Bar Chart","description":"desc","goodFor":"comparisons",
			"dataStructure":{"type":"array","example":"[1,2,3]"}}}}`
		if err := os.WriteFile(filepath.Join(dir, "echarts-chart-types.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{StaticDir: dir}}
		got := s.loadChartTypesCatalog()
		if got == nil {
			t.Fatal("want non-nil catalog")
		}
		if _, ok := got.Get("chartTypes"); !ok {
			t.Errorf("want chartTypes key present")
		}
	})
}

func TestBuildChartRefinementPrompt(t *testing.T) {
	t.Run("no catalog file leaves the placeholder empty", func(t *testing.T) {
		s := &server{cfg: config{StaticDir: t.TempDir()}}
		got := s.buildChartRefinementPrompt()
		if strings.Contains(got, "{catalog}") {
			t.Errorf("placeholder should be substituted, got: %s", got)
		}
		if strings.Contains(got, "Available Chart Types") {
			t.Errorf("no catalog section expected, got: %s", got)
		}
		if !strings.Contains(got, "You are an expert in Apache ECharts") {
			t.Errorf("expected base template text, got: %s", got)
		}
	})

	t.Run("catalog present splices a section per chart type", func(t *testing.T) {
		dir := t.TempDir()
		body := `{"chartTypes":{
			"bar":{"name":"Bar Chart","description":"A bar chart","goodFor":"comparisons",
				"dataStructure":{"type":"array","example":"[1,2,3]"}},
			"missing_name":{"description":"no name given","goodFor":"x","dataStructure":{}}
		}}`
		if err := os.WriteFile(filepath.Join(dir, "echarts-chart-types.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{StaticDir: dir}}
		got := s.buildChartRefinementPrompt()
		if !strings.Contains(got, "Available Chart Types and Data Requirements") {
			t.Errorf("expected catalog header, got: %s", got)
		}
		if !strings.Contains(got, "**Bar Chart** (bar)") {
			t.Errorf("expected bar chart entry, got: %s", got)
		}
		// Missing "name" falls back to the map key itself.
		if !strings.Contains(got, "**missing_name** (missing_name)") {
			t.Errorf("expected fallback-to-key entry, got: %s", got)
		}
	})
}
