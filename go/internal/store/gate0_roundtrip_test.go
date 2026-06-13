//go:build chdb

// Phase 0 gate (Go side). Build/run with the native chdb library available:
//
//	CGO_ENABLED=1 go test -tags chdb ./go/internal/store -run TestGate0RoundTrip
//
// Pairs with migration/tools/gate0_chdb.py. Sequence:
//  1. python migration/tools/gate0_chdb.py write   (Python creates the dir)
//  2. this test                                    (Go opens it, asserts, writes Id 4,5)
//  3. python migration/tools/gate0_chdb.py verify  (Python re-opens, asserts all 5)
//
// PASS here + PASS in step 3 ⇒ the on-disk chdb format round-trips between Python
// chdb 4.1.9 and Go chdb-go (pinned chdb-core). That is the make-or-break viability
// gate for the hard cutover. FAIL ⇒ STOP and revisit (PHASES.md Phase 0).
package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	chdb "github.com/chdb-io/chdb-go/chdb"
)

func TestGate0RoundTrip(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "migration", "fixtures", "_gate0.chdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("run `python migration/tools/gate0_chdb.py write` first: %v", err)
	}

	sess, err := chdb.NewSession(dir)
	if err != nil {
		t.Fatalf("chdb-go NewSession: %v (is libchdb/chdb-core installed & pinned? see CHDB_PIN.md)", err)
	}
	defer sess.Cleanup()

	// 1. Read the rows Python wrote.
	out, err := sess.Query("SELECT Id,Name FROM gate0 FINAL ORDER BY Id", "CSV")
	if err != nil {
		t.Fatalf("read python rows: %v", err)
	}
	got := out.String()
	for _, want := range []string{"1,", "2,", "3,", "alpha", "beta", "gamma"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in Python-written data, got:\n%s", want, got)
		}
	}

	// 2. Go writes two more rows into the SAME directory.
	if _, err := sess.Query(
		"INSERT INTO gate0 (Id,Name,Version,IsDeleted) VALUES (4,'delta',1,0),(5,'epsilon',1,0)", "CSV",
	); err != nil {
		t.Fatalf("go insert: %v", err)
	}
	if _, err := sess.Query("OPTIMIZE TABLE gate0 FINAL", "CSV"); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	// 3. Go reads back all five.
	out2, _ := sess.Query("SELECT count() FROM gate0 FINAL", "CSV")
	if c := strings.TrimSpace(out2.String()); c != "5" {
		t.Fatalf("expected 5 rows after Go insert, got %q", c)
	}
	t.Log("GATE0 GO ROUND-TRIP: PASS — now run `python migration/tools/gate0_chdb.py verify`")
}
