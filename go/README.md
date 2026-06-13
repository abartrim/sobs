# SOBS — Go implementation

A from-scratch Go reimplementation of the Python SOBS server. **During migration, the
only definition of "correct" is byte-for-byte parity with the Python app**, enforced by
`../migration/tools/parity_check.py` against the golden corpus. Read `../migration/GOAL.md`
first — do not start here.

## Layout (target)

```
go/
  cmd/sobs/            main, config, server, header middleware (the after_request port)
  internal/
    store/             chdb-go wrapper (ChDbConnection port) + Phase-0 gate test
    render/            text/template engine + Jinja transpiler + filters (the hard part)
      transpile/       Jinja→text/template mechanical rewriter
      filters.go       e, tojson, mask, truncate, … (JINJA_TO_GO_SPEC.md §9)
      macros.go        caller()-style macros ported as funcs
      loader.go        per-page template sets (extends/include/import)
    jsonenc/           byte-exact JSON encoder (ordered keys, Python separators/escaping)
    clock/             fixed-instant clock under SOBS_PARITY=1 (mirrors determinism.py)
    idgen/             deterministic uuid4 / token stream (mirrors determinism.py)
    otlp/              protobuf OTLP ingest (google.golang.org/protobuf + OTEL stubs)
    crypto/            Fernet helper + ECDSA P-256 VAPID (stdlib)
    masking/           port of masking.py (compliance-critical)
    handlers/          one handler per app.py @app.route
  templates/  (symlink or build-time copy of ../templates — same files, Go-compiled)
  static/     (served from ../static — committed assets, byte-identical)
```

## Build & run

```bash
go build ./cmd/sobs && SOBS_PARITY=1 SOBS_DATA_DIR=../data ./sobs    # default: stdlib only
# Phase 0 onward, with the native chdb engine linked:
CGO_ENABLED=1 go test -tags chdb ./internal/store -run TestGate0RoundTrip
```

The base module builds with the **standard library only**. `chdb-go` and
`google.golang.org/protobuf` are the only third-party deps and are added deliberately
(see `DEPENDENCIES.md`). The Phase-0 chdb gate test is behind the `chdb` build tag so it
doesn't force the native dependency on every build.

## The two hard things (front-loaded)

1. **`internal/render`** — reproducing Jinja2's exact bytes (escaping, `tojson` in
   `<script>`, macros, whitespace) in `text/template`. Spec: `../migration/JINJA_TO_GO_SPEC.md`.
2. **`internal/jsonenc`** — reproducing Python `json`/`jsonify` bytes (key order,
   separators, escaping, trailing newline). Spec: `../migration/PARITY_STRATEGY.md` §4.

Get these two byte-perfect against a handful of goldens and most of the corpus follows,
because every route shares them.
