package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// These cover the last DB-dependent residual funcs — background-worker bodies and the chdb
// encryption name-set helper — through the store.DB seam with an injected fake. They run only from
// parity-gated background loops / real-runtime startup, so the byte-parity corpus never reaches
// them. Oracle: app.py _run_raw_window_copy_worker / _sync_github_repo_health_once and the chDB
// encryption config assertion.

func TestChdbNameSet(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if !strings.Contains(q, "SELECT name") {
			t.Fatalf("unexpected query: %s", q)
		}
		// Duplicate + empty values collapse into a set of the non-empty distinct names.
		return storetest.Result([]string{"name"}, []any{"a"}, []any{"b"}, []any{""}, []any{"a"}), nil
	}}}
	got := s.chdbNameSet("SELECT name FROM t", "name")
	want := map[string]bool{"a": true, "b": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chdbNameSet: got %v, want %v", got, want)
	}

	// Query error → empty set.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.chdbNameSet("SELECT name FROM t", "name"); len(got) != 0 {
		t.Fatalf("chdbNameSet on error: want empty, got %v", got)
	}
}

func TestRunRawWindowCopyWorker_EarlyReturns(t *testing.T) {
	// nil store → all zero.
	if a, b, c := (&server{}).runRawWindowCopyWorker(); a != 0 || b != 0 || c != 0 {
		t.Fatalf("nil db: want 0,0,0 got %d,%d,%d", a, b, c)
	}
	// No registered windows → all zero.
	sEmpty := &server{db: &storetest.FakeDB{}}
	if a, b, c := sEmpty.runRawWindowCopyWorker(); a != 0 || b != 0 || c != 0 {
		t.Fatalf("no windows: want 0,0,0 got %d,%d,%d", a, b, c)
	}
}

func TestRunRawWindowCopyWorker_CopyPath(t *testing.T) {
	fake := &storetest.FakeDB{}
	fake.ExecuteFunc = func(q string, _ ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_raw_windows"):
			return storetest.Result(
				[]string{"Id", "WindowStart", "WindowEnd", "ServiceName", "Namespace", "NodeName"},
				[]any{"w1", "2024-01-01 00:00:00", "2024-01-01 01:00:00", "web", "ns1", "node1"}), nil
		case strings.Contains(q, "sobs_raw_window_copy_state"):
			return &store.Result{}, nil // not yet copied
		case strings.Contains(q, "NOT IN"):
			return storetest.Result([]string{"cnt"}, []any{float64(2)}), nil // missing > 0 → INSERT
		case strings.Contains(q, "count() AS cnt"):
			return storetest.Result([]string{"cnt"}, []any{float64(5)}), nil // matched > 0
		default:
			return &store.Result{}, nil // INSERT INTO ... and anything else
		}
	}
	s := &server{db: fake}
	// One window × three raw metric tables, all copied.
	wa, ok, ce := s.runRawWindowCopyWorker()
	if wa != 3 || ok != 3 || ce != 0 {
		t.Fatalf("copy path: want 3,3,0 got %d,%d,%d", wa, ok, ce)
	}
	stateInserts := 0
	for _, in := range fake.Inserts {
		if in.Table == "sobs_raw_window_copy_state" {
			stateInserts++
		}
	}
	if stateInserts != 3 {
		t.Fatalf("want 3 copy-state inserts, got %d", stateInserts)
	}
}

func TestSyncGithubRepoHealthOnce(t *testing.T) {
	// The zero fake answers every SELECT with an empty result, so there are no repo targets: the
	// summary is the all-zero ok:true object, no GitHub HTTP is issued, and (no previous summary
	// stored) the last-sync + compact-summary settings are written.
	fake := &storetest.FakeDB{}
	s := &server{db: fake}
	summary := s.syncGithubRepoHealthOnce()
	if okVal, _ := summary.Get("ok"); okVal != true {
		t.Fatalf("want ok:true summary, got %v", okVal)
	}
	settingsWrites := 0
	for _, in := range fake.Inserts {
		if in.Table == "sobs_app_settings" {
			settingsWrites++
		}
	}
	if settingsWrites != 2 {
		t.Fatalf("want 2 settings writes (last-sync + summary), got %d", settingsWrites)
	}
}
