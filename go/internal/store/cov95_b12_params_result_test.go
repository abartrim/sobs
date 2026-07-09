package store

import "testing"

// cov95_b12_params_result_test.go — coverage-gate batch 12 for internal/store/params.go's
// Result accessors (First / ColumnIndex), both at 0% combined coverage: they are small, pure
// methods on *Result with no external dependency, so a direct unit test is all that's needed.

func TestResultFirst(t *testing.T) {
	// Empty Rows -> nil.
	empty := &Result{Columns: []string{"a"}}
	if got := empty.First(); got != nil {
		t.Fatalf("First() on empty result = %v, want nil", got)
	}

	// Non-empty -> the first row, verbatim.
	row0 := []any{"x", 1}
	row1 := []any{"y", 2}
	r := &Result{Columns: []string{"name", "n"}, Rows: [][]any{row0, row1}}
	got := r.First()
	if len(got) != 2 || got[0] != "x" || got[1] != 1 {
		t.Fatalf("First() = %v, want %v", got, row0)
	}
}

func TestResultColumnIndex(t *testing.T) {
	r := &Result{Columns: []string{"Id", "Name", "Value"}}
	cases := map[string]int{
		"Id":      0,
		"Name":    1,
		"Value":   2,
		"Missing": -1,
		"":        -1,
		"id":      -1, // case-sensitive: lowercase does not match "Id"
	}
	for name, want := range cases {
		if got := r.ColumnIndex(name); got != want {
			t.Errorf("ColumnIndex(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestResultColumnIndexNoColumns(t *testing.T) {
	r := &Result{}
	if got := r.ColumnIndex("anything"); got != -1 {
		t.Fatalf("ColumnIndex on empty Columns = %d, want -1", got)
	}
}
