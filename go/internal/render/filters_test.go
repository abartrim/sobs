package render

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJinjaTitle(t *testing.T) {
	cases := map[string]string{
		"it's a test": "It's A Test",
		"abc123def":   "Abc123def",
		"ERROR rate":  "Error Rate",
		"foo-bar baz": "Foo-Bar Baz",
	}
	for in, want := range cases {
		if got := jinjaTitle(in); got != want {
			t.Errorf("jinjaTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJinjaRound(t *testing.T) {
	cases := []struct {
		f    float64
		p    int
		want float64
	}{
		{3.14159, 2, 3.14}, {2.675, 2, 2.67}, {8.395, 2, 8.39}, {0.15, 1, 0.1}, {2.5, 0, 2},
	}
	for _, c := range cases {
		if got := jinjaRound(c.f, c.p, "common"); got != c.want {
			t.Errorf("jinjaRound(%v,%d) = %v, want %v", c.f, c.p, got, c.want)
		}
	}
}

func TestJinjaIntFloat(t *testing.T) {
	if got := jinjaInt(2.9, 0); got != 2 {
		t.Errorf("jinjaInt(2.9) = %d, want 2", got)
	}
	if got := jinjaInt("5", 0); got != 5 {
		t.Errorf("jinjaInt(\"5\") = %d, want 5", got)
	}
	if got := jinjaInt("5.0", 0); got != 5 {
		t.Errorf("jinjaInt(\"5.0\") = %d, want 5", got)
	}
	if got := jinjaInt("x", 7); got != 7 {
		t.Errorf("jinjaInt(\"x\",7) = %d, want 7 (default)", got)
	}
	if got := jinjaFloat("1.5", 0); got != 1.5 {
		t.Errorf("jinjaFloat(\"1.5\") = %v, want 1.5", got)
	}
}

func TestJinjaTruncate(t *testing.T) {
	// killwords, end="": hard cut at length once over length+leeway(5).
	if got := jinjaTruncate("abcdefghijklmnopqrst", 8, true, ""); got != "abcdefgh" {
		t.Errorf("truncate(8,true,'') = %q, want abcdefgh", got)
	}
	// Within length+leeway: unchanged (12 <= 8+5).
	if got := jinjaTruncate("abcdefghijkl", 8, true, ""); got != "abcdefghijkl" {
		t.Errorf("truncate within leeway = %q, want unchanged", got)
	}
}

func TestPyPercentFormat(t *testing.T) {
	if got := pyPercentFormat("%.1f", []any{3.14159}); got != "3.1" {
		t.Errorf("%%.1f of 3.14159 = %q, want 3.1", got)
	}
	if got := pyPercentFormat("%d items", []any{5}); got != "5 items" {
		t.Errorf("%%d = %q, want '5 items'", got)
	}
	if got := pyPercentFormat("%.1f%% done", []any{42.5}); got != "42.5% done" {
		t.Errorf("%%%% literal = %q, want '42.5%% done'", got)
	}
}

func TestSplitAddSub(t *testing.T) {
	terms, ops := splitAddSub("offset - limit")
	if len(terms) != 2 || len(ops) != 1 || ops[0] != '-' {
		t.Fatalf("splitAddSub(offset - limit) = %v %q", terms, ops)
	}
	terms, ops = splitAddSub("a + b - c")
	if len(terms) != 3 || string(ops) != "+-" {
		t.Fatalf("splitAddSub(a + b - c) = %v %q", terms, ops)
	}
	// A leading unary minus is not a binary operator.
	_, ops = splitAddSub("-5")
	if len(ops) != 0 {
		t.Fatalf("splitAddSub(-5) ops = %q, want none", ops)
	}
	if v := subValues(5, 3); v != 2 {
		t.Errorf("subValues(5,3) = %v, want 2", v)
	}
}

// TestEngineFilters renders templates end-to-end to verify the ~ operator, filter-vs-arith
// precedence, and the new filters all parse and evaluate.
func TestEngineFilters(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, body string
		ctx        map[string]any
		want       string
	}{
		{"tilde.html", "{{ 'g' ~ gid ~ 'i' ~ idx }}", map[string]any{"gid": 7, "idx": 1}, "g7i1"},
		{"min.html", "{{ [offset + items|length, total]|min }}", map[string]any{"offset": 5, "items": []any{1, 2, 3}, "total": 10}, "8"},
		{"max.html", "{{ [offset - limit, 0]|max }}", map[string]any{"offset": 2, "limit": 5}, "0"},
		{"fmt.html", "{{ '%.1f'|format(n) }}", map[string]any{"n": 3.14159}, "3.1"},
		{"round.html", "{{ x|round(2) }}", map[string]any{"x": 3.14159}, "3.14"},
		{"int.html", "{{ y|int }}", map[string]any{"y": 2.9}, "2"},
		// Autoescape turns the apostrophe into &#39; in HTML text context (Jinja does the same).
		{"title.html", "{{ s|title }}", map[string]any{"s": "it's a test"}, "It&#39;s A Test"},
	}
	eng := New(dir)
	for _, c := range cases {
		write(c.name, c.body)
		got, err := eng.Render(c.name, c.ctx)
		if err != nil {
			t.Errorf("Render(%s) error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("Render(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}
