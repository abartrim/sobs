# SOBS — Go implementation

The Go server is now the whole of SOBS — the from-scratch reimplementation this directory
started as has fully replaced the original Python app (`app.py`, `mcp.py`, `masking.py`,
`telemetry/`), which has been deleted. Historically, "correct" during that migration meant
byte-for-byte parity with the Python app, verified route-by-route against a frozen golden
corpus. That verification lives on as a Go-native test suite — see `goldenreplay/` below —
but the Python oracle it once diffed against no longer exists.

## Layout

```
go/
  cmd/sobs/            main, config, server, ~190 handler/helper files (routes, auth,
                       masking, crypto, AI/LLM, notifications, OTLP ingest, …)
  internal/
    store/             chdb-go wrapper — the embedded ClickHouse session
    render/            text/template-based Jinja-compatible template engine + filters
    jsonenc/           byte-exact JSON encoder (ordered keys, Python-style separators/escaping)
    otlp/              protobuf OTLP ingest (google.golang.org/protobuf + generated OTEL stubs)
  goldenreplay/         Go-native golden-corpus regression suite (chdb build tag) — boots the
                        compiled sobs binary per profile and byte-diffs its responses against
                        testdata/golden.tar.gz
  testdata/             the frozen golden corpus + fixtures goldenreplay replays against, as
                        gzip'd tar archives (golden.tar.gz, fixtures/{base,seeds,upstream}.tar.gz)
                        rather than thousands of loose files — see archive.go
  templates/, static/  symlinks to ../templates, ../static (repo-root assets, Go-rendered)
```

## Build & run

```bash
go build ./cmd/sobs && SOBS_DATA_DIR=../data ./sobs
# Run the golden-corpus regression suite (needs the native chdb lib — see CHDB_PIN.md):
CHDB_LIB_PATH=/path/to/libchdb.so go test -tags chdb -run TestGoldenCorpus ./goldenreplay/...
```

The base module builds with the **standard library only**. `chdb-go` and
`google.golang.org/protobuf` are the only third-party deps and are added deliberately
(see `DEPENDENCIES.md`). The chdb-tagged tests are behind the `chdb` build tag so they
don't force the native dependency on every build.

## The two hard things (still true)

1. **`internal/render`** — reproducing Jinja2's exact bytes (escaping, `tojson` in
   `<script>`, macros, whitespace) in `text/template`.
2. **`internal/jsonenc`** — reproducing Python `json`/`jsonify` bytes (key order,
   separators, escaping, trailing newline).

Every route shares them, so `goldenreplay`'s corpus exercises both heavily.
