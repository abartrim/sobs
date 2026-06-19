# Go dependency policy

**Default answer to "should I add a dependency?" is no.** The Python app's behavior is
ported with the standard library wherever possible. Every third-party module must be
justified here before it enters `go.mod`. This is a deliberate guardrail — dependency
sprawl was not the cause of past failures, but a lean module keeps the cgo/cutover story
simple and the binary reproducible.

## Approved dependencies (the whole list)

| Module | Why it's unavoidable | Notes |
|--------|----------------------|-------|
| `github.com/chdb-io/chdb-go` | The embedded ClickHouse engine. The hard cutover requires reading the *same* on-disk `data/sobs.chdb`. No stdlib substitute exists; reimplementing ClickHouse is out of the question. | cgo; links native libchdb/chdb-core. Pin per `CHDB_PIN.md`. Build from `main`. |
| `google.golang.org/protobuf` | OTLP ingest parses OpenTelemetry protobuf (`/v1/logs\|traces\|metrics`). The wire format is protobuf; hand-rolling a parser is error-prone and pointless. | Generate OTEL `.pb.go` from `open-telemetry/opentelemetry-proto` into `internal/otlp/genpb`. |
| `github.com/dlclark/regexp2` | DLP output masking and tag-rule matching compile a (possibly user-supplied) pattern in-process and apply it to arbitrary text, mirroring Python `re`. Go's stdlib `regexp` is RE2: ASCII-only `\d`/`\w`/`\s`/`\b` and no lookahead/lookbehind/backreferences — so it under/over-redacts non-ASCII and silently drops patterns Python accepts. regexp2 is a backtracking, Unicode-aware engine that matches Python `re` far more closely. Pure Go, no cgo, no transitive deps. | Used ONLY at the in-process sites (see `mask_regex.go`). RE2 (stdlib `regexp`) is kept for `validate-regex`/query filters — those patterns are *executed by ClickHouse* (itself RE2), so RE2 is the correct validator there. A `MatchTimeout` bounds backtracking on user input. |

## Explicitly NOT used (and the stdlib we use instead)

| Tempting dep | Use instead | Reason |
|--------------|-------------|--------|
| gin / echo / chi | `net/http` + `ServeMux` | 188 routes don't need a framework; parity needs full control of headers/bytes. |
| html/template engines, pongo2, etc. | `text/template` + our transpiler | Only `text/template` gives byte-control; `html/template` mangles `<script>` (see JINJA_TO_GO_SPEC.md). pongo2 ≠ Jinja byte-for-byte. |
| encoding/json defaults | `internal/jsonenc` (built on stdlib) | stdlib `json` sorts map keys + HTML-escapes + no trailing newline — all wrong for parity. We need explicit ordering/escaping. |
| an ORM / sqlx | raw SQL via `store.DB` | SQL is reused verbatim from Python; an ORM would obscure it. |
| a Fernet library | `internal/crypto` (~80 LOC on stdlib `crypto/aes`,`crypto/hmac`) | Trivial to implement; avoids a dep for a tiny, security-sensitive primitive. |
| a sourcemap library | `internal/sourcemap` (small, Phase 6) | Feature-flagged off during parity; small enough to own. |
| maxmind/geoip2 | parse geoip2fast's bundled data, or vendor the file | Keep the *same* geo data so output matches; a different DB would change bytes. |

If a genuine need for another dependency appears, add a row to "Approved" with the
justification and the stdlib alternatives you rejected — in the same spirit as above.
