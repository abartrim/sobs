package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// These exercise the DM/S3 backup helpers through the store.DB seam with an injected settings
// fake. The real path is reachable only via runDmBackup/runDmRestore, which need S3 configured,
// stamp a nowUTC() backup name, and issue a real BACKUP ALL TO S3(...) — so the byte-parity
// corpus never covers them. Oracle: app.py _build_s3_backup_dest / _list_dm_backups.

func TestBuildS3BackupDest(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{
		"data_management.s3_bucket":            "my-bucket",
		"data_management.s3_region":            "us-east-1",
		"data_management.s3_access_key_id":     "AKIAEXAMPLE",
		"data_management.s3_secret_access_key": "secret123",
		// s3_path_prefix intentionally absent → empty prefix branch
	})}
	dest, err := s.buildS3BackupDest("sobs-manual-20240101T000000Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "S3('https://s3.us-east-1.amazonaws.com/my-bucket/sobs-manual-20240101T000000Z', 'AKIAEXAMPLE', 'secret123')"
	if dest != want {
		t.Fatalf("dest mismatch:\n got: %s\nwant: %s", dest, want)
	}
}

func TestBuildS3BackupDest_WithPrefix_NoCreds(t *testing.T) {
	// A path prefix is inserted; with no access/secret key the two-arg S3(endpoint) form is used.
	s := &server{db: storetest.SettingsDB(map[string]string{
		"data_management.s3_bucket":        "my-bucket",
		"data_management.s3_region":        "us-east-1",
		"data_management.s3_path_prefix":   "backups/prod",
		"data_management.s3_access_key_id": "", // no creds
	})}
	dest, err := s.buildS3BackupDest("sobs-auto-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "S3('https://s3.us-east-1.amazonaws.com/my-bucket/backups/prod/sobs-auto-1')"
	if dest != want {
		t.Fatalf("dest mismatch:\n got: %s\nwant: %s", dest, want)
	}
}

func TestBuildS3BackupDest_Validation(t *testing.T) {
	// Invalid backup name is rejected first (before any field check).
	sName := &server{db: storetest.SettingsDB(map[string]string{"data_management.s3_bucket": "my-bucket"})}
	if _, err := sName.buildS3BackupDest("bad name!"); err == nil ||
		err.Error() != "backup_name contains unsupported characters" {
		t.Fatalf("want backup_name validation error, got: %v", err)
	}

	// A valid name but an unsafe bucket value → the s3_bucket field error.
	sBucket := &server{db: storetest.SettingsDB(map[string]string{"data_management.s3_bucket": "bad bucket"})}
	if _, err := sBucket.buildS3BackupDest("sobs-manual"); err == nil ||
		err.Error() != "s3_bucket contains unsupported characters" {
		t.Fatalf("want s3_bucket validation error, got: %v", err)
	}
}

func TestListDmBackups(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if !strings.Contains(q, "system.backups") {
			t.Fatalf("unexpected query: %s", q)
		}
		cols := []string{"name", "status", "start_time", "end_time", "num_files", "total_size", "error"}
		return storetest.Result(cols,
			[]any{"sobs-manual-1", "BACKUP_COMPLETE", nil, nil, nil, nil, nil},
			[]any{"sobs-manual-2", "CREATING_BACKUP", nil, nil, nil, nil, nil},
		), nil
	}}}
	got := s.listDmBackups()
	if len(got) != 2 || got[0].name != "sobs-manual-1" || got[0].status != "BACKUP_COMPLETE" ||
		got[1].name != "sobs-manual-2" {
		t.Fatalf("unexpected rows: %+v", got)
	}

	// Query error → nil (the Python try/except fallback).
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.listDmBackups(); got != nil {
		t.Fatalf("want nil on error, got %+v", got)
	}
}
