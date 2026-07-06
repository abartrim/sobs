package main

import "testing"

// cov95_b16_sql_validate_test.go — batch 16 targeted coverage for cmd/sobs/sql_validate.go:
// suggestAllowedTableNames' empty-table-component branch (a reference ending in "." has no table
// name to match against), and sequenceRatio/findLongestMatch's empty-input and no-common-run
// edge cases that the indirect TestSuggestAllowedTableNames coverage doesn't isolate directly.

func TestSuggestAllowedTableNamesEmptyComponent(t *testing.T) {
	// "schema." splits to ["schema", ""] -> tableName == "" -> returns []string{} immediately,
	// without ever calling getCloseMatches.
	got := suggestAllowedTableNames("schema.", 5)
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestSequenceRatioDirectEdgeCases(t *testing.T) {
	t.Run("both empty yields 1.0", func(t *testing.T) {
		if got := sequenceRatio(nil, nil); got != 1.0 {
			t.Errorf("got %v, want 1.0", got)
		}
	})
	t.Run("identical strings yield 1.0", func(t *testing.T) {
		r := []rune("otel_logs")
		if got := sequenceRatio(r, r); got != 1.0 {
			t.Errorf("got %v, want 1.0", got)
		}
	})
	t.Run("completely disjoint strings yield 0.0", func(t *testing.T) {
		if got := sequenceRatio([]rune("abc"), []rune("xyz")); got != 0.0 {
			t.Errorf("got %v, want 0.0", got)
		}
	})
	t.Run("one empty one non-empty yields 0.0", func(t *testing.T) {
		if got := sequenceRatio([]rune(""), []rune("abc")); got != 0.0 {
			t.Errorf("got %v, want 0.0", got)
		}
	})
}

func TestFindLongestMatchDirectEdgeCases(t *testing.T) {
	t.Run("no common substring returns zero-size match", func(t *testing.T) {
		a := []rune("abc")
		b := []rune("xyz")
		b2j := map[rune][]int{}
		for j, c := range b {
			b2j[c] = append(b2j[c], j)
		}
		_, _, size := findLongestMatch(a, b, b2j, 0, len(a), 0, len(b))
		if size != 0 {
			t.Errorf("size = %d, want 0", size)
		}
	})
	t.Run("full match on identical strings", func(t *testing.T) {
		a := []rune("hello")
		b := []rune("hello")
		b2j := map[rune][]int{}
		for j, c := range b {
			b2j[c] = append(b2j[c], j)
		}
		i, j, size := findLongestMatch(a, b, b2j, 0, len(a), 0, len(b))
		if i != 0 || j != 0 || size != 5 {
			t.Errorf("got (%d,%d,%d), want (0,0,5)", i, j, size)
		}
	})
	t.Run("empty ranges yield zero-size match", func(t *testing.T) {
		_, _, size := findLongestMatch([]rune("abc"), []rune("abc"), map[rune][]int{}, 0, 0, 0, 0)
		if size != 0 {
			t.Errorf("size = %d, want 0", size)
		}
	})
}
