# Go integration coverage (byte-parity corpus replay)

A second coverage lens alongside `coverage_app.json` (which measures the **Python oracle**). This
one measures **how much of the Go server the byte-parity corpus actually exercises** — built with
`go build -cover`, replayed across every profile, merged with `go tool covdata`.

It is *integration* coverage (the corpus replay), not unit-test coverage.

## Latest snapshot — 2026-06-20 (go-main `d855ad9`, corpus = 689 parity routes, GREEN 689/0)

| Scope | Coverage | Statements |
|---|---:|---|
| All Go | 69.4% | 15,403 / 22,202 |
| Generated OTLP protobuf (`internal/otlp/genpb/*`) | 16.3% | 249 / 1,527 |
| **Hand-written Go (excl. generated)** | **73.3%** | 15,154 / 20,675 |

By area (hand-written):

| Coverage | Package | Notes |
|---:|---|---|
| 72.9% | `cmd/sobs` (13,844 / 18,991) | handlers, server, business logic — the bulk |
| 79.7% | `internal/jsonenc` | order-preserving JSON encoder |
| 78.6% | `internal/render` | the Jinja template engine |
| 66.4% | `internal/store` | chdb wrapper |

### Combined with the Go unit suite

The corpus is one lens; the `go test ./...` unit suite is another (it reaches corpus-unreachable
pure logic directly). Their **union** is the honest "verified Go" surface:

| Lens | Hand-written Go | All Go |
|---|---:|---:|
| byte-parity corpus only | 73.3% | 69.4% |
| unit tests only | — | 12.4% |
| **corpus ∪ unit** | **75.4%** | 71.4% |

Reproduce the union: run a corpus profile (above) and a unit profile
(`go test -count=1 -coverpkg=./... -coverprofile=../migration/go_unit_cover.txt ./...`), then OR the
two textfmt profiles per statement span. Targeted unit tests for corpus-unreachable functions (e.g.
the AI-guard parsers and the data-management validators in `safeguard_dm_validate_test.go`) raise
this union without needing the parity harness.

### Reading it

- **73.3%** (corpus) / **75.4%** (corpus ∪ unit) is the meaningful number (generated protobuf is
  auto-generated getters/marshalers we do not hand-test; excluding it is correct).
- It runs **~6 pts below the oracle's 79.7%**, and that gap is expected, not a parity hole: the Go
  server carries code with **no `app.py` counterpart** that the parity corpus (`SOBS_PARITY=1`)
  deliberately never exercises — real network egress, WebPush/notification dispatch, chdb disk
  encryption, Fernet settings encryption, source-map real fetch, the write-queue, non-parity RUM
  token signing, boot/config. Those lower Go coverage but cannot lower oracle coverage (they do not
  exist in the oracle).

## How to reproduce

Inside the parity Docker image (bind-mount the repo, supply libchdb), with `SOBS_GOCOVER=1` so
`parity_check._build_go` adds `-cover -coverpkg=./...`, and `GOCOVERDIR` on the mounted volume so
the per-profile counter files survive the container:

```bash
mkdir -p migration/.gocov
docker run --rm -v "<repo>:/repo" -w "/repo/<worktree>" \
  -e CHDB_LIB_PATH=/repo/.libchdb/libchdb.so \
  -e SOBS_GOCOVER=1 -e GOCOVERDIR=/repo/<worktree>/migration/.gocov \
  sobs-parity:latest \
  bash -c "python migration/tools/seed_fixtures.py && python migration/tools/parity_check.py"

# then, in the go/ module dir:
go tool covdata percent -i=../migration/.gocov
go tool covdata textfmt -i=../migration/.gocov -o ../migration/go_coverage.txt
go tool cover -func=../migration/go_coverage.txt | tail -1   # overall total
go tool cover -html=../migration/go_coverage.txt             # optional HTML drill-down
```

Coverage flush relies on the `GOCOVERDIR`-gated SIGTERM/SIGINT hook in `cmd/sobs/main.go` (a
`-cover` binary only writes on a clean exit, but the harness stops the server with SIGTERM). The
hook is a **no-op unless `GOCOVERDIR` is set**, so normal/parity/production runs are unchanged — the
instrumented replay above is itself parity GREEN 689/0.
