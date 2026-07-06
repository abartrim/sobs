package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b13_chdb_encryption_test.go — batch 13: chdb_encryption.go coverage. The existing
// chdb_encryption_test.go already covers setupChdbEncryption's happy path and
// renderClickhouseConfig's disabled/success/bad-hex branches, so this file focuses on the parts
// left untested: validateChdbStartup (25% coverage — the lowest in this batch), and the small
// envTrim/mustAbs helpers it and renderClickhouseConfig depend on.

// clearChdbValidateEnv ensures no stray env var from another test/process leaks into a case.
func clearChdbValidateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SOBS_CHDB_EXPECT_DISK", "SOBS_CHDB_EXPECT_STORAGE_POLICY"} {
		t.Setenv(k, "")
	}
}

// ---- validateChdbStartup ------------------------------------------------------------------------

func TestValidateChdbStartup_NoExpectationsIsNoOp(t *testing.T) {
	clearChdbValidateEnv(t)
	s := &server{}
	if err := s.validateChdbStartup(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateChdbStartup_NilDBErrorsWhenExpectationSet(t *testing.T) {
	clearChdbValidateEnv(t)
	t.Setenv("SOBS_CHDB_EXPECT_DISK", "encrypted_disk")
	s := &server{}
	if err := s.validateChdbStartup(); err == nil {
		t.Fatal("want error for nil db")
	}
}

func TestValidateChdbStartup_ExpectationsSatisfied(t *testing.T) {
	clearChdbValidateEnv(t)
	t.Setenv("SOBS_CHDB_EXPECT_DISK", "encrypted_disk")
	t.Setenv("SOBS_CHDB_EXPECT_STORAGE_POLICY", "encrypted_only")
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if q == "SELECT name FROM system.disks" {
			return storetest.Result([]string{"name"}, []any{"encrypted_disk"}), nil
		}
		return storetest.Result([]string{"policy_name"}, []any{"encrypted_only"}), nil
	}}}
	if err := s.validateChdbStartup(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateChdbStartup_OnlyDiskExpected(t *testing.T) {
	clearChdbValidateEnv(t)
	t.Setenv("SOBS_CHDB_EXPECT_DISK", "encrypted_disk")
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result([]string{"name"}, []any{"encrypted_disk"}), nil
	}}}
	if err := s.validateChdbStartup(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateChdbStartup_MissingDiskAndPolicyReportedTogether(t *testing.T) {
	clearChdbValidateEnv(t)
	t.Setenv("SOBS_CHDB_EXPECT_DISK", "encrypted_disk")
	t.Setenv("SOBS_CHDB_EXPECT_STORAGE_POLICY", "encrypted_only")
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil // neither disk nor policy present
	}}}
	err := s.validateChdbStartup()
	if err == nil {
		t.Fatal("want error for missing disk+policy")
	}
	if got := err.Error(); !contains2(got, "disk 'encrypted_disk'") || !contains2(got, "storage policy 'encrypted_only'") {
		t.Errorf("error message = %q, want both missing items named", got)
	}
}

func TestValidateChdbStartup_QueryErrorYieldsEmptyNameSet(t *testing.T) {
	clearChdbValidateEnv(t)
	t.Setenv("SOBS_CHDB_EXPECT_DISK", "encrypted_disk")
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return nil, errString("boom")
	}}}
	if err := s.validateChdbStartup(); err == nil {
		t.Fatal("want error when the disk query fails and disk is expected")
	}
}

// (chdbNameSet already has a dedicated test in residual_workers_test.go.)

// ---- envTrim / mustAbs ----------------------------------------------------------------------------

func TestEnvTrim(t *testing.T) {
	t.Setenv("SOBS_TEST_ENVTRIM", "  value  ")
	if got := envTrim("SOBS_TEST_ENVTRIM", "def"); got != "value" {
		t.Errorf("envTrim = %q, want trimmed 'value'", got)
	}
	t.Setenv("SOBS_TEST_ENVTRIM_UNSET", "")
	if got := envTrim("SOBS_TEST_ENVTRIM_UNSET", "def"); got != "def" {
		t.Errorf("envTrim = %q, want default", got)
	}
}

func TestMustAbs(t *testing.T) {
	if _, err := mustAbs("relative", "MY_VAR"); err == nil {
		t.Error("want error for relative path")
	}
	abs, err := mustAbs("/tmp/x", "MY_VAR")
	if err != nil || abs != "/tmp/x" {
		t.Errorf("mustAbs = (%q,%v)", abs, err)
	}
}

// contains2 is a tiny substring helper (avoids importing "strings" solely for one assertion).
func contains2(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return len(needle) == 0
}
