package storetest

import "testing"

// cov95_b12_fake_close_test.go — coverage-gate batch 12 for FakeDB.Close (0% coverage): the
// smallest possible seam gap, a one-line setter that no existing test exercised directly.

func TestFakeDBClose(t *testing.T) {
	f := &FakeDB{}
	if f.Closed {
		t.Fatalf("zero-value FakeDB.Closed = true, want false before Close")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
	if !f.Closed {
		t.Fatalf("Closed = false after Close(), want true")
	}
}
