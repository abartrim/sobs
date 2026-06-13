# chdb / chdb-core / chdb-go version pin

**The single most important version decision in this migration.** The Go process must
read and write the *same* on-disk `data/sobs.chdb` that Python `chdb 4.1.9` created. The
underlying ClickHouse storage format must be compatible across both engines, so both
sides must be frozen on a matching kernel through the cutover.

## The pin

| Component | Pinned version | Source |
|-----------|----------------|--------|
| Python `chdb` | `4.1.9` | already in `requirements.txt` |
| `chdb-core` (native) | `>= 26.5.0`, the exact build chDB 4.1.9 links | https://github.com/chdb-io/chdb-core/releases — **pin an exact tag, do not use `latest`** |
| `chdb-go` | built from `main` (tagged releases lag) | https://github.com/chdb-io/chdb-go — vendor a specific commit SHA in `go.mod` |
| ClickHouse kernel | the 25.x kernel chDB 4.x wraps | implied by the chdb-core pin |

## Why not `latest`

- `chdb-go`'s `update_libchdb.sh` fetches `chdb-core` **`latest`** by default. For a
  hard cutover that is dangerous: an unpinned native lib can drift to a newer kernel that
  writes a directory the Python side (older kernel) can no longer open. ClickHouse
  storage is backward-compatible (newer reads older) but **not guaranteed
  forward-compatible**.
- The maintainers warn that bumping libchdb under the binding without re-checking the
  binding can break (chdb-io discussion #295).

## Procedure

1. Determine the exact `chdb-core` build that Python `chdb==4.1.9` ships (inspect the
   installed wheel's bundled lib version).
2. Download/pin that exact `chdb-core` tag for the Go side (do **not** run
   `curl lib.chdb.io | bash` unpinned in CI/prod).
3. `go get github.com/chdb-io/chdb-go@<commit-sha-on-main>` and record the SHA below.
4. Run the Phase 0 gate (`PHASES.md`): Python writes → Go reads/writes → Python re-reads.
   Both directions must PASS before any porting begins.
5. Record the exact pins here once verified:

```
chdb (py)   : 4.1.9
chdb-core   : <TAG>            # filled in during Phase 0
chdb-go     : <commit-sha>     # filled in during Phase 0
verified on : <date>, <OS/arch>, Docker base <image>
gate0 result: PASS/FAIL
```

## Cutover note

Freeze BOTH sides on this kernel for the entire cutover window. Only after the Go image
is stable in prod (and Python is retired) should a kernel upgrade be considered — and
that becomes a normal data-migration task, no longer a cross-engine-sharing constraint.
