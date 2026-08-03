# chdb / chdb-core / chdb-go version pin

> The live CHDB_VERSION pin used by every build (CI and local) is `versions.env` at the
> repo root — `scripts/check_version_pins.py` fails CI if any Dockerfile drifts from it.
> The `v26.5.0` you'll see below is that same current value; the "Record the exact pins
> here once verified" block further down is a dated, frozen record of the one-time
> Python↔Go compatibility gate, not something this doc re-derives automatically — bump
> `versions.env` first on a version change, then update this file's own copy by hand.

**The single most important version decision in this migration.** The Go process must
read and write the *same* on-disk `data/sobs.chdb` that Python `chdb 4.1.9` created. The
underlying ClickHouse storage format must be compatible across both engines, so both
sides must be frozen on a matching kernel through the cutover.

## The pin

| Component | Pinned version | Source |
|-----------|----------------|--------|
| Python `chdb` | `4.1.9` | historical — the version the now-retired Python oracle pinned; recorded here as the basis for the Go-side pin below |
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
4. Run the Phase 0 gate: Python writes → Go reads/writes → Python re-reads. Both
   directions must PASS before any porting begins. (This gate — proving the two engines
   share an on-disk format — ran once and passed; see the recorded result below. Its job
   is permanently settled and the Python side no longer exists to re-run it against.)
5. Record the exact pins here once verified:

```
chdb (py)   : 4.1.9  (pulls chdb-core 26.5.0)
chdb-core   : v26.5.0  (asset: macos-arm64-libchdb.tar.gz -> libchdb.so, 326 MB)
chdb-go     : v1.11.0  (purego/dlopen — no cgo; loads libchdb via CHDB_LIB_PATH)
verified on : 2026-06-13, darwin/arm64 (macOS 26.5, Apple silicon)
gate0 result: PASS (bidirectional — Python wrote 1-3, Go read+wrote 4-5, Python re-read all 5)
```

## Runtime wiring (how Go finds libchdb)

chdb-go v1.11.0 uses `github.com/ebitengine/purego` to `dlopen` libchdb at runtime — no
cgo, no C toolchain. It resolves the library path in this order (chdb-purego):
1. `CHDB_LIB_PATH` env var (an explicit path to `libchdb.so`) — **use this for pinning**.
2. `libchdb.so` on `PATH`.
3. `/usr/local/lib/libchdb.so`, `/opt/homebrew/lib/libchdb.dylib`.

For parity/dev we keep the pinned `libchdb.so` under `.libchdb/` (gitignored, 326 MB)
and export `CHDB_LIB_PATH=$REPO/.libchdb/libchdb.so`. For the Docker image, COPY the
pinned `libchdb.so` into the image and set `CHDB_LIB_PATH` (Phase 5).

Download the pinned lib (sourcing the version from `versions.env` so this can't drift;
works from anywhere inside the repo):
```
CHDB_VERSION=$(grep '^CHDB_VERSION=' "$(git rev-parse --show-toplevel)/versions.env" | cut -d= -f2)
curl -sL -o libchdb.tar.gz \
  "https://github.com/chdb-io/chdb-core/releases/download/${CHDB_VERSION}/macos-arm64-libchdb.tar.gz"
# (linux-x86_64-libchdb.tar.gz / linux-aarch64-libchdb.tar.gz for the deploy targets)
tar -xzf libchdb.tar.gz   # -> libchdb.so, chdb.h, chdb.hpp
```

## Gotcha: Session.Close() vs Cleanup()

chdb-go `Session.Cleanup()` does `os.RemoveAll(path)` — it DELETES the session
directory. For a persistent shared directory always use `Session.Close()` (which only
removes the dir for temp `chdb_*` sessions). Using Cleanup() on the shared dir destroys
the data — this bit the gate test once.

## Cutover note

Freeze BOTH sides on this kernel for the entire cutover window. Only after the Go image
is stable in prod (and Python is retired) should a kernel upgrade be considered — and
that becomes a normal data-migration task, no longer a cross-engine-sharing constraint.
