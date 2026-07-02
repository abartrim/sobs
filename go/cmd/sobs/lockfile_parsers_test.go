package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// parseLockfileDependencies dispatches by lockfile kind to a format-specific parser used by the
// GitHub-repo dependency inventory scan. The corpus never fetches a real lockfile (no GitHub repo
// is configured on the fixture), so none of these parsers run.
// Oracle: app.py fragments cited in the surrounding cve_scan.go doc comments.

func TestParseGoSumDeps(t *testing.T) {
	content := "github.com/pkg/errors v0.9.1 h1:abc=\n" +
		"github.com/pkg/errors v0.9.1/go.mod h1:def=\n" + // same version's /go.mod line -> deduped
		"github.com/other/lib v1.2.3 h1:ghi=\n" +
		"\n" + // blank line skipped
		"malformed-line-with-one-field\n"
	got := parseGoSumDeps(content)
	if len(got) != 2 {
		t.Fatalf("want 2 deps (deduped), got %d: %v", len(got), got)
	}
	d0 := got[0].(*jsonenc.Object)
	if v, _ := d0.Get("package"); v != "github.com/pkg/errors" {
		t.Fatalf("unexpected first dep: %v", d0)
	}
	if v, _ := d0.Get("ecosystem"); v != "Go" {
		t.Fatalf("ecosystem should be Go: %v", v)
	}
}

func TestParseGemfileLockDeps(t *testing.T) {
	content := "GEM\n" +
		"  remote: https://rubygems.org/\n" +
		"  specs:\n" +
		"    rails (7.0.4)\n" +
		"      activesupport (= 7.0.4)\n" +
		"    rack (2.2.4, 2.2.3)\n" + // multiple versions -> takes the first
		"\n" +
		"PLATFORMS\n" + // unindented, non-blank line after specs -> stops parsing
		"  ruby\n" +
		"    fake-post-specs-entry (9.9.9)\n"
	got := parseGemfileLockDeps(content)
	names := make([]string, len(got))
	for i, d := range got {
		o := d.(*jsonenc.Object)
		v, _ := o.Get("package")
		names[i] = v.(string)
	}
	if strings.Join(names, ",") != "rails,activesupport,rack" {
		t.Fatalf("unexpected deps: %v", names)
	}
	rack := got[2].(*jsonenc.Object)
	if v, _ := rack.Get("version"); v != "2.2.4" {
		t.Fatalf("rack should take the first listed version: %v", v)
	}
}

func TestParsePackageLockDeps(t *testing.T) {
	t.Run("v2/v3 packages map wins over legacy dependencies", func(t *testing.T) {
		content := `{
			"packages": {
				"": {"name": "root"},
				"node_modules/lodash": {"version": "4.17.21"},
				"node_modules/@scope/pkg": {"version": "1.0.0"},
				"not-node-modules-path": {"version": "9.9.9"}
			},
			"dependencies": {"should-be-ignored": {"version": "0.0.1"}}
		}`
		got := parsePackageLockDeps(content)
		if len(got) != 2 {
			t.Fatalf("want 2 deps, got %d: %v", len(got), got)
		}
		names := map[string]bool{}
		for _, d := range got {
			o := d.(*jsonenc.Object)
			v, _ := o.Get("package")
			names[v.(string)] = true
		}
		if !names["lodash"] || !names["@scope/pkg"] {
			t.Fatalf("unexpected deps: %v", got)
		}
	})

	t.Run("legacy dependencies map used when no packages present", func(t *testing.T) {
		content := `{"dependencies": {"express": {"version": "4.18.0"}, "bad": {}}}`
		got := parsePackageLockDeps(content)
		if len(got) != 1 {
			t.Fatalf("want 1 dep (blank version dropped), got %d: %v", len(got), got)
		}
		o := got[0].(*jsonenc.Object)
		if v, _ := o.Get("package"); v != "express" {
			t.Fatalf("unexpected dep: %v", o)
		}
	})

	if got := parsePackageLockDeps("{not json"); len(got) != 0 {
		t.Fatalf("invalid json: want empty, got %v", got)
	}
	if got := parsePackageLockDeps(`["array","not","object"]`); len(got) != 0 {
		t.Fatalf("non-object top level: want empty, got %v", got)
	}
}

func TestParseLockfileDependencies_Dispatch(t *testing.T) {
	if got := parseLockfileDependencies("go_sum", "github.com/x/y v1.0.0 h1:abc="); len(got) != 1 {
		t.Fatalf("go_sum dispatch: got %v", got)
	}
	if got := parseLockfileDependencies("gemfile_lock", "GEM\n  specs:\n    rails (7.0.4)\n"); len(got) != 1 {
		t.Fatalf("gemfile_lock dispatch: got %v", got)
	}
	if got := parseLockfileDependencies("package_lock", `{"dependencies":{"express":{"version":"4.18.0"}}}`); len(got) != 1 {
		t.Fatalf("package_lock dispatch: got %v", got)
	}
	if got := parseLockfileDependencies("requirements", "flask==2.0.0\n"); len(got) != 1 {
		t.Fatalf("requirements dispatch: got %v", got)
	}
	if got := parseLockfileDependencies("unknown_kind", "irrelevant"); got != nil {
		t.Fatalf("unknown kind: want nil, got %v", got)
	}
}
