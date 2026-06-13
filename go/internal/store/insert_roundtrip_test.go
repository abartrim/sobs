//go:build chdb

// Verifies InsertJSONEachRow round-trips through chdb-go: create a table, insert rows
// (including a string with <, >, & to prove HTML escaping is off), read them back via
// Execute, and assert the values + types match. Run with the native library:
//
//	CHDB_LIB_PATH=$PWD/../../../.libchdb/libchdb.so go test -tags chdb ./internal/store -run TestInsertJSONEachRow
package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInsertJSONEachRow(t *testing.T) {
	dir, err := os.MkdirTemp("", "sobs-insert-*.chdb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := Open(filepath.Dir(dir + "/x")) // Open expects a data dir; it appends sobs.chdb
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Execute(
		"CREATE TABLE t (Id String, Name String, N UInt64) ENGINE = MergeTree() ORDER BY Id"); err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := db.InsertJSONEachRow("t", []map[string]any{
		{"Id": "a", "Name": "x<y>&z", "N": 7},
		{"Id": "b", "Name": "plain", "N": 12},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n != 2 {
		t.Fatalf("inserted %d, want 2", n)
	}
	res, err := db.Execute("SELECT Id, Name, N FROM t ORDER BY Id")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(res.Rows))
	}
	m := rowMapsForTest(res)
	if m[0]["Name"] != "x<y>&z" {
		t.Fatalf("HTML-escape leaked or value wrong: %q", m[0]["Name"])
	}
	// chdb-go returns 64-bit ints as JSON numbers (float64); cInt/cStr normalize either.
	if m[0]["N"] != float64(7) && m[0]["N"] != "7" {
		t.Fatalf("N = %v (%T), want 7", m[0]["N"], m[0]["N"])
	}
	t.Log("InsertJSONEachRow round-trip: PASS")
}

func rowMapsForTest(res *Result) []map[string]any {
	out := make([]map[string]any, len(res.Rows))
	for i, row := range res.Rows {
		mm := map[string]any{}
		for j, c := range res.Columns {
			mm[c] = row[j]
		}
		out[i] = mm
	}
	return out
}
