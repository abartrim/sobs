package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Data-management prune — a 1:1 port of app.py's POST /api/data-management/prune handler
// (`api_dm_prune`) and its helpers `_acquire_dm_prune_lock`, `_parse_dm_prune_period`,
// `_get_dm_column_type`, and `_run_dm_prune` (app.py ~31981-32130, 32296-32321).
//
// The route forces TTL processing on every data-management-managed table via
// `OPTIMIZE TABLE … FINAL`, and — when the caller supplies a one-off prune period — applies a
// preceding `ALTER TABLE … DELETE WHERE <ts> < now() - INTERVAL <n> <unit>` window. The metric
// tables store their timestamp as `DateTime` (TimeUnixMs), so app.py probes the actual column
// type and chooses a primary DELETE expression accordingly, with the alternate expression as a
// fallback. Every statement is best-effort; per-table failures are aggregated into the message.
//
// PARITY: the empty base fixture deletes nothing, so the `json: {}` (and no-content-type
// `body_b64`) base routes hit only the OPTIMIZE pass and report the fixed six-table summary —
// identical to the previous hardcoded stub. The `dmprune` profile seeds retention-eligible rows
// (migration/tools/seed_fixtures.py) so the DELETE branch runs against populated tables and the
// custom-period success message is diffed byte-for-byte against the frozen Python oracle.

// dmPrunePeriodUnits maps the API's period unit to its ClickHouse INTERVAL keyword
// (app.py `_DM_PRUNE_PERIOD_UNITS`).
var dmPrunePeriodUnits = map[string]string{
	"hours": "HOUR",
	"days":  "DAY",
}

// dmTTLTables are the (table, timestamp column) pairs whose Timestamp is a DateTime64
// (app.py `_DM_TTL_TABLES`; the setting-key third element is unused by prune).
var dmTTLTables = []struct{ table, tsCol string }{
	{"otel_logs", "Timestamp"},
	{"otel_traces", "Timestamp"},
	{"hyperdx_sessions", "Timestamp"},
}

// dmMetricTables are the (table, timestamp column) pairs whose TimeUnixMs is a DateTime
// (app.py `_DM_METRIC_TABLES`), handled with the ms/DateTime detection below.
var dmMetricTables = []struct{ table, tsCol string }{
	{"otel_metrics_gauge", "TimeUnixMs"},
	{"otel_metrics_sum", "TimeUnixMs"},
	{"otel_metrics_histogram", "TimeUnixMs"},
}

// dmAllPruneTables is `[t for t,*_ in _DM_TTL_TABLES] + [t for t,_ in _DM_METRIC_TABLES]` — the
// OPTIMIZE-pass order and the `{len}` count in the success message (always 6).
func dmAllPruneTables() []string {
	out := make([]string, 0, len(dmTTLTables)+len(dmMetricTables))
	for _, t := range dmTTLTables {
		out = append(out, t.table)
	}
	for _, t := range dmMetricTables {
		out = append(out, t.table)
	}
	return out
}

// dmPruneMu mirrors app.py's module-level `_dm_prune_lock = threading.Lock()`: a single
// non-reentrant lock that serializes prune runs process-wide. TryLock reproduces the
// non-blocking `lock.acquire(blocking=False)` in `_acquire_dm_prune_lock`.
var dmPruneMu sync.Mutex

// POST /api/data-management/prune — port of app.py `api_dm_prune`. Trigger an immediate prune of
// all TTL-managed tables via OPTIMIZE TABLE … FINAL (plus an optional one-off DELETE window).
func (s *server) handleApiDataManagementPrune(w http.ResponseWriter, r *http.Request) {
	// payload = await request.get_json(silent=True); raw_body = (await get_data()).strip()
	raw, _ := io.ReadAll(r.Body)
	rawBody := strings.TrimSpace(string(raw))
	isJSON := requestIsJSON(r)

	var payload any
	payloadIsNone := true
	// get_json(silent=True) yields a value only when the request advertises JSON AND parses;
	// a non-JSON content-type returns None regardless of the body (Quart's force=False default).
	if isJSON {
		if parsed, err := parseJSONValue(raw); err == nil {
			payload, payloadIsNone = parsed, false
		}
	}
	if payloadIsNone {
		if rawBody != "" && isJSON {
			writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
				Set("ok", false).Set("message", "request body contains invalid JSON"))
			return
		}
		payload = jsonenc.NewObject() // payload = {}
	}

	// if not isinstance(payload, dict): 400
	obj, ok := payload.(*jsonenc.Object)
	if !ok {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("ok", false).Set("message", "request body must be a JSON object"))
		return
	}

	prunePeriod, parseErr := parseDmPrunePeriod(obj)
	if parseErr != "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("ok", false).Set("message", parseErr))
		return
	}

	// prune_lock = _acquire_dm_prune_lock(); if None -> 409
	if !dmPruneMu.TryLock() {
		writeJSON(w, http.StatusConflict, jsonenc.NewObject().
			Set("ok", false).Set("message", "A prune operation is already in progress"))
		return
	}
	defer dmPruneMu.Unlock()

	prok, message := s.runDmPrune(prunePeriod)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", prok).Set("message", message))
}

// requestIsJSON mirrors Quart/Werkzeug `request.is_json`: the bare mimetype (Content-Type minus
// any parameters) is `application/json` or an `application/…+json` subtype.
func requestIsJSON(r *http.Request) bool {
	mimetype := r.Header.Get("Content-Type")
	if i := strings.IndexByte(mimetype, ';'); i >= 0 {
		mimetype = mimetype[:i]
	}
	mimetype = strings.ToLower(strings.TrimSpace(mimetype))
	return mimetype == "application/json" ||
		(strings.HasPrefix(mimetype, "application/") && strings.HasSuffix(mimetype, "+json"))
}

// dmPrunePeriod is the parsed one-off retention window; present=false means no custom period
// (the OPTIMIZE-only path).
type dmPrunePeriod struct {
	present bool
	value   int
	unit    string // raw unit key ("hours"/"days"); rendered verbatim in the message
}

// parseDmPrunePeriod ports app.py `_parse_dm_prune_period`. Returns the parsed period and an
// empty error string on success; a non-empty string is the 400 message (the ValueError text).
func parseDmPrunePeriod(payload *jsonenc.Object) (dmPrunePeriod, string) {
	rawValue, _ := payload.Get("prune_period_value")
	// str(payload.get("prune_period_unit", "")): an ABSENT key takes the "" default (-> ""), but a
	// present null is the value None, whose str() is "None" — distinguished by Get's presence flag.
	rawUnit := ""
	if uv, present := payload.Get("prune_period_unit"); present {
		s := toStr(uv)
		if uv == nil {
			s = "None" // Python str(None)
		}
		rawUnit = strings.ToLower(strings.TrimSpace(s))
	}

	// raw_value in (None, "")
	valueEmpty := rawValue == nil || rawValue == ""

	if valueEmpty && rawUnit == "" {
		return dmPrunePeriod{}, "" // no custom period
	}
	if valueEmpty {
		return dmPrunePeriod{}, "prune_period_value is required when prune_period_unit is provided"
	}
	if rawUnit == "" {
		return dmPrunePeriod{}, "prune_period_unit is required when prune_period_value is provided"
	}
	if _, ok := dmPrunePeriodUnits[rawUnit]; !ok {
		return dmPrunePeriod{}, "prune_period_unit must be 'hours' or 'days'"
	}

	// period_value = int(str(raw_value).strip()); ValueError or <= 0 -> the same message
	periodValue, err := strconv.Atoi(strings.TrimSpace(toStr(rawValue)))
	if err != nil || periodValue <= 0 {
		return dmPrunePeriod{}, "prune_period_value must be a positive integer"
	}
	return dmPrunePeriod{present: true, value: periodValue, unit: rawUnit}, ""
}

// dmColumnType ports app.py `_get_dm_column_type`: the lowercased declared type of `column` in
// `table` from DESCRIBE TABLE, or ("", false) when the probe fails or the column is absent.
func (s *server) dmColumnType(table, column string) (string, bool) {
	res, err := s.db.Execute("DESCRIBE TABLE " + table)
	if err != nil {
		return "", false
	}
	for _, m := range rowMaps(res) {
		if cStr(m, "name") == column {
			return strings.ToLower(strings.TrimSpace(cStr(m, "type"))), true
		}
	}
	return "", false
}

// runDmPrune ports app.py `_run_dm_prune`. Optionally applies a one-off DELETE window, then forces
// TTL processing via OPTIMIZE TABLE … FINAL across every managed table, aggregating per-table
// errors into the response message exactly as Python does.
func (s *server) runDmPrune(period dmPrunePeriod) (bool, string) {
	allTables := dmAllPruneTables()
	var errs []string

	if period.present {
		unitSQL := dmPrunePeriodUnits[period.unit]
		for _, t := range dmTTLTables {
			stmt := fmt.Sprintf("ALTER TABLE %s DELETE WHERE %s < now() - INTERVAL %d %s",
				t.table, t.tsCol, period.value, unitSQL)
			if _, err := s.db.Execute(stmt); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.table, err))
			}
		}
		for _, t := range dmMetricTables {
			detected, ok := s.dmColumnType(t.table, t.tsCol)
			useMsExpr := !ok || !strings.Contains(detected, "datetime")

			msExpr := fmt.Sprintf(
				"ALTER TABLE %s DELETE WHERE toDateTime(intDiv(%s, 1000)) < now() - INTERVAL %d %s",
				t.table, t.tsCol, period.value, unitSQL)
			plainExpr := fmt.Sprintf("ALTER TABLE %s DELETE WHERE %s < now() - INTERVAL %d %s",
				t.table, t.tsCol, period.value, unitSQL)

			primarySQL, fallbackSQL := plainExpr, msExpr
			if useMsExpr {
				primarySQL, fallbackSQL = msExpr, plainExpr
			}

			if _, err := s.db.Execute(primarySQL); err != nil {
				if _, ferr := s.db.Execute(fallbackSQL); ferr != nil {
					errs = append(errs, fmt.Sprintf("%s: %v (fallback after: %v)", t.table, ferr, err))
				}
			}
		}
	}

	for _, table := range allTables {
		if _, err := s.db.Execute(fmt.Sprintf("OPTIMIZE TABLE %s FINAL", table)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", table, err))
		}
	}

	if len(errs) > 0 {
		return false, "Prune completed with errors: " + strings.Join(errs, "; ")
	}
	if period.present {
		return true, fmt.Sprintf(
			"Prune completed successfully (%d tables processed, custom period: %d %s)",
			len(allTables), period.value, period.unit)
	}
	return true, fmt.Sprintf("Prune completed successfully (%d tables processed)", len(allTables))
}
