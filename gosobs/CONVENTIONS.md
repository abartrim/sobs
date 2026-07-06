# SOBS Go Port — Conversion Conventions

Faithful port of `app.py` (Quart/Python) to Go. All code lives in **one package**:
`package main` under `gosobs/`. One `.go` file per assigned section, named
`sNN_<topic>.go` (e.g. `s07_otlp_ingest.go`).

## Libraries (already in go.mod)
- HTTP: stdlib `net/http` (Go 1.22+ pattern routing)
- DB: `github.com/chdb-io/chdb-go/chdb` (embedded ClickHouse, session-based)
- OTLP protos: `go.opentelemetry.io/proto/otlp/...` + `google.golang.org/protobuf/encoding/protojson`
- Templates: `github.com/flosch/pongo2/v6` (renders existing Jinja templates in ../templates)
- Fernet: `github.com/fernet/fernet-go`

## Naming (deterministic — do not deviate)
- Strip leading underscores; convert snake_case → camelCase.
- Every word after the first: capitalize FIRST LETTER ONLY (`ai`→`Ai`, `db`→`Db`,
  `http`→`Http`, `id`→`Id`, `sql`→`Sql`, `api`→`Api`, `json`→`Json`, `url`→`Url`).
  Examples: `_load_ai_setting`→`loadAiSetting`, `_now_iso`→`nowIso`,
  `_GEO_ENABLED_SETTING`→`geoEnabledSetting`, `ensure_db_schema`→`ensureDbSchema`.
- Keep everything unexported (lowercase first letter) — single package.
- Python classes → Go structs with same camelCase name but Capitalized if it was
  CapWords in Python minus leading underscore (e.g. `ChDbConnection`→`ChDbConnection`,
  `_WriteTask`→`writeTask`).

## Core API (defined in s00_core.go / s01_setup.go / s02_db.go — use these, do not redefine)
```go
type Row map[string]any                       // replaces RowCompat (dict + index access)
type ChDbResult struct{ Cols []string; Rows []Row }
func (r *ChDbResult) Fetchall() []Row
func (r *ChDbResult) Fetchone() Row           // nil if empty
type ChDbConnection struct{ ... }             // wraps *chdb.Session + write queue
func (c *ChDbConnection) Execute(query string, params ...any) (*ChDbResult, error)
   // params substitute %s / %(name)s placeholders with proper SQL quoting (helper: quoteSqlValue)
func getDb() *ChDbConnection                  // global singleton
func jsonResponse(w http.ResponseWriter, status int, payload any)       // = jsonify
func maskedJsonResponse(w http.ResponseWriter, status int, payload any) // = masked_jsonify
func jsonError(w http.ResponseWriter, message string, status int)       // = _json_error
func renderTemplate(w http.ResponseWriter, name string, ctx map[string]any) // Jinja via pongo2; auto-injects globals
func nowIso() string                          // _now_iso
func envFlag(name string, def bool) bool      // _env_flag
func readEnvOrFile(envVar, fileEnvVar string) string
func encryptSecretValue(v string) string
func decryptSecretValue(v string) string
func loadAiSetting(db *ChDbConnection, key, def string) string
func saveAiSetting(db *ChDbConnection, key, value string)
func loadAllAiSettings(db *ChDbConnection) map[string]string
func registerRoute(method, pattern string, h http.HandlerFunc) // call from init()
func sseBroadcast(event string, payload any)  // SSE tail pub/sub
var logger *slog.Logger                       // logging.getLogger("sobs")
```

## Routes
- Flask `@app.route("/x/<id>", methods=["POST"])` →
  `registerRoute("POST", "/x/{id}", handlerName)` inside `func init()`.
- `<string:foo>`/`<path:foo>` → `{foo}` (`{foo...}` for path:). Read with `r.PathValue("foo")`.
- Default method GET. Multiple methods → register each.
- Query args: `r.URL.Query().Get("x")`; form: `r.ParseForm(); r.FormValue("x")`;
  JSON body: decode into `map[string]any` with a `readJsonBody(r)` helper (in core).
- `redirect(url_for(...))` → `http.Redirect(w, r, "<literal path>", http.StatusFound)`.
- `flash(msg, cat)` → `flashMessage(w, r, msg, cat)` (core helper, cookie-based).

## Semantics mapping
- `async def` → ordinary func; `asyncio` background loops → goroutines started in main.
- `threading.Lock` → `sync.Mutex`; `queue.Queue` → buffered channel.
- Python dict → `map[string]any`; list → `[]any` or typed slices when obvious.
- `datetime.now(timezone.utc)` → `time.Now().UTC()`; ISO format `.Format(time.RFC3339)` (match Python's exact format where it matters: `2006-01-02T15:04:05.999999+00:00` style — use helper `pyIsoFormat(t)` in core).
- `try/except` → explicit error returns; broad `except Exception: pass` → log at debug and continue.
- f-strings building SQL: keep identical SQL text; use `fmt.Sprintf` with the same quoting helpers.
- `re` module: Go `regexp` (RE2 — Python code already validates patterns as RE2-compatible in places; for Python-only constructs like lookbehind, mimic behavior with manual logic and add `// PORT-NOTE:` comment).
- pandas usage (only ~10 call sites, Vanna/query section): replace with plain `[]Row` manipulation.
- `httpx` calls → `net/http` client with timeout (core helper `httpClient`).
- protobuf `ParseDict`/OTLP: use protojson.Unmarshal into otlp proto types; binary body → proto.Unmarshal.
- Imports of `masking`, `mcp`, `telemetry` python modules → ported in s90_masking.go / s91_mcp.go; telemetry self-instrumentation is stubbed as no-ops.

## Fidelity rules
- Preserve route paths, response JSON shapes, status codes, SQL text, template names
  and template context keys EXACTLY.
- Keep comments that explain business logic; translate docstrings to Go comments.
- Where exact Python behavior cannot be replicated, add `// PORT-NOTE: <explanation>`.
- Do NOT invent abstractions, frameworks, or restructure logic. Mirror the Python flow.
- Every symbol you reference that belongs to another section: use the deterministic
  naming rule and assume it exists. The reconcile phase fixes mismatches.
- Your file must be syntactically valid Go. Do not run `go build` (other sections may
  not exist yet); `gofmt` your file before finishing.
