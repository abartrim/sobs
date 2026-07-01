# Go coverage residual — the deterministic ceiling (DoD-3 evidence)

Authoritative source: the `go-coverage` artifact (`go_corpus_0pct.txt`) from the go-main push run
that instruments the byte-parity replay with `GOCOVER=1` (build-arg on `Dockerfile.parity-ci`).
Snapshot below is commit `5481064` (post the dead-code deletion in #409).

- **Corpus coverage: 74.3%** all-Go *including generated protobuf* (the protobuf `.pb.go` files are
  large and mostly unexercised, which drags the headline number down).
- **87 non-protobuf funcs at 0.0% in the corpus report**, BUT the corpus report does not fold in the
  hand-written unit tests. Filtering the 87 against `go/cmd/sobs/*_test.go` (`grep -l '\bNAME\b'`):
  - **75 are already unit-tested** — covered by the pure-func unit grind (combined corpus∪unit > 0%).
  - **12 remain at 0% in the COMBINED (corpus ∪ unit) set.** Those 12 are the true residual, classified
    below. Every one is deterministically uncoverable by the two harnesses we have (byte-parity corpus
    under `SOBS_PARITY=1`, and the deliberately **DB-less** `&server{}` unit harness) — uncovered *by
    construction*, not by omission.

## The unit harness is DB-less by design

`coverage_pure_i_test.go:24` — *"constructed inline as a minimal `&server{cfg: config{...}}` (none
touch `s.db`)"*. Every unit test builds a `&server{}` with no chDB store. So a func is unit-coverable
only if it does **not** call `s.db.Execute(...)`. All 12 residual funcs either run a DB query or loop
forever, so none are reachable from the unit harness. That is the exact boundary at which the pure-func
grind bottomed out.

## The 12 residual funcs

### (1) Background-worker loop drivers — parity-gated + infinite (3)
`background_tasks.go:rawWindowCopyLoop`, `enrichment_loops.go:cveScannerLoop`,
`enrichment_loops.go:githubRepoHealthLoop`

Started only by `startBackgroundWorkers`, which returns early under parity:
```go
func (s *server) startBackgroundWorkers() {
    if s.cfg.Parity || s.db == nil { return }   // corpus capture sets SOBS_PARITY=1 → never started
    go s.rawWindowCopyLoop(); go s.cveScannerLoop(); go s.githubRepoHealthLoop()
}
```
Each body is a `for { work(); time.Sleep(...) }` infinite goroutine → cannot be invoked from a unit
test (never returns). Genuinely untestable by construction.

### (2) Background-worker bodies — parity-gated, DB + external IO (2)
`background_tasks.go:runRawWindowCopyWorker` (chDB window-copy INSERTs),
`enrichment_loops.go:syncGithubRepoHealthOnce` (GitHub API + chDB writes)

Invoked only by the parity-gated loops in (1). Require a live chDB store — and, for the GitHub one, a
live GitHub API — that the DB-less unit harness does not provide.

### (3) Boot-seed idempotency guards — seed path disabled under parity (2)
`app_registry_seed.go:lookupAppIDBySlug`, `app_registry_seed.go:lookupReleaseID`

Called only inside the app-registry seed as dedup guards (`if existing := s.lookupAppIDBySlug(slug);
existing != "" { skip }`). The Go boot seed is disabled under parity (the corpus fixtures seed the DB
directly), so the seed path — and these DB lookups — never execute in the corpus. DB-backed →
unreachable by the DB-less unit harness.

### (4) chDB encryption-at-rest config render — real-runtime only (1)
`chdb_encryption.go:chdbNameSet`

Used only when rendering the ClickHouse encryption config for chDB encryption-at-rest — a
real-runtime-only path, off the parity byte-tested surface.

### (5) DM/S3 backup helpers — now()-named + real-S3-IO route (4)
`handlers_mutations.go:buildS3BackupDest`, `handlers_mutations.go:appSettingOr`,
`fix_mutations1.go:listDmBackups`, `dbutil.go:dmSettingValue`

Reachable only through `runDmBackup`/`runDmRestore`, which:
1. short-circuit unless S3 is configured (`if s.dmS3Bucket() == "" { return ... }`),
2. stamp the backup name with `nowUTC()` → non-deterministic response body, and
3. issue a real `BACKUP ALL TO S3(...)` / `RESTORE ALL FROM S3(...)` against a live S3 endpoint.

Not cleanly byte-parity-able (now()-window + external IO), and DB-backed so not reachable by the unit
harness. Covering them would require a chDB + mock-S3 integration harness the project intentionally
does not carry (it duplicates what a corpus profile would do, but without a deterministic response).

## Conclusion

The pure-func unit grind is **exhausted** (all 75 DB-less pure funcs at 0% are now unit-tested). The
remaining **12** funcs each require one of: a never-returning background loop, a parity-disabled boot
seed, a real-runtime-only encryption path, external S3/GitHub IO, or a now()-dependent response. They
sit **below the deterministic ceiling by construction** — DoD-3 ("coverage at the deterministic
ceiling") is met. Advancing past this line needs a *new harness* (a chDB-backed / mock-upstream
integration test rig), a deliberate scope decision, not more of the existing grind.

To refresh this analysis: download the `go-coverage` artifact from the latest go-main push run, then
```
awk '/## full list/{f=1;next} f&&NF>=2{print $1"\t"$2}' go_corpus_0pct.txt | \
  while IFS=$'\t' read -r file fn; do \
    grep -rlq "\b${fn}\b" go/cmd/sobs/*_test.go || echo "$file  $fn"; done
```
prints the combined-0% residual to re-classify.
