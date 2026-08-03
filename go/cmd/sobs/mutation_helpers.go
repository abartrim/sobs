package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// boolToInt returns 1 for true, 0 for false (as a float64 so cInt/cBool — which read
// float64/string — round-trip it from an in-memory row).
func boolToInt(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// bNum mirrors app.py `int(payload.get(key, 0) or 0)` for a JSON body value (bodyMap decodes
// numbers as float64). Returns a float64 so cInt reads it back from the in-memory row.
func bNum(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

// fixedVersionMillis mirrors app.py `int(time.time() * 1000)` — the ReplacingMergeTree Version.
// Under the parity clock (SOBS_FAKE_EPOCH) this is the frozen 1704164645000.
func fixedVersionMillis() int64 { return nowUTC().UnixMilli() }

// parityUUIDSeed pins the deterministic UUID source (uuidRandBytes) under golden-corpus
// replay to the same fixed epoch SOBS_FAKE_EPOCH uses (clock.go) — no significance beyond
// being a fixed, memorable constant.
const parityUUIDSeed = 1704164645

// parityUUIDRand is initialized as part of normal package init (single-threaded, before any
// goroutine runs — see the Go spec's "Program initialization"), so unlike a lazily-built
// singleton it needs no sync.Once/nil-check: it's safe to read (under parityUUIDMu) the moment
// any goroutine can run. Constructing it is cheap and side-effect-free, so building it
// unconditionally costs nothing on the production path, which never reads it.
var (
	parityUUIDRand = rand.New(rand.NewSource(parityUUIDSeed))
	parityUUIDMu   sync.Mutex
)

// uuidRandBytes returns cryptographically random bytes (randBytes/crypto/rand) in production,
// or a fixed-seed reproducible byte stream when SOBS_PARITY=1 — mirroring nowUTC's
// SOBS_FAKE_EPOCH clock freeze (clock.go). newUUIDHex/newUUIDv4 output is not masked in the
// golden-corpus manifest, so any route whose golden fixture captured a server-generated id
// (rather than an explicit request "id") would otherwise mismatch on every replay: real
// crypto/rand never reproduces the same bytes twice. The golden-corpus harness boots a fresh
// sobs process per profile and replays the identical route sequence every run (replay_test.go),
// so seeding once per process reproduces the identical id sequence byte-for-byte across runs.
// Gated on SOBS_PARITY (not a replay-only build tag) so it stays inert in production — the same
// var already flips this behavior off outside golden-corpus replay (main.go's cfg.Parity).
func uuidRandBytes(n int) []byte {
	if os.Getenv("SOBS_PARITY") != "1" {
		return randBytes(n)
	}
	b := make([]byte, n)
	parityUUIDMu.Lock()
	_, _ = parityUUIDRand.Read(b)
	parityUUIDMu.Unlock()
	return b
}

// newUUIDHex mirrors uuid.uuid4().hex for production inserts. Parity test bodies usually pass
// an explicit `id`, but for the handful that don't (the id is server-generated), uuidRandBytes
// keeps golden-corpus replay deterministic — see its comment.
func newUUIDHex() string { return hex.EncodeToString(uuidRandBytes(16)) }

// newUUIDv4 mirrors str(uuid.uuid4()) — the dashed 36-char v4 form used by app.py inserts
// whose id is server-generated. See uuidRandBytes for why this is parity-deterministic.
func newUUIDv4() string {
	b := uuidRandBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// toStr mirrors Python str(): used for payload values that app.py wraps in str(...).
func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		switch {
		case math.IsNaN(t):
			return "nan"
		case math.IsInf(t, 1):
			return "inf"
		case math.IsInf(t, -1):
			return "-inf"
		}
		return jsonenc.PyFloatRepr(t) // str(5.0) -> "5.0" (keeps trailing .0)
	}
	return fmt.Sprintf("%v", v)
}

// payloadDefault returns payload[key] if present, else def — mirroring dict.get(key, default).
func payloadDefault(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// payloadStrDefault mirrors `str(payload.get(key, default)).strip()`.
func payloadStrDefault(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok {
		return strings.TrimSpace(toStr(v))
	}
	return strings.TrimSpace(def)
}

// reportPageTypes mirrors app.py _REPORT_PAGE_TYPES; sortedReportPageTypes is its sorted join
// source for the validation error message.
var reportPageTypes = map[string]bool{
	"logs": true, "traces": true, "errors": true, "metrics": true, "rum": true,
	"ai": true, "work_items": true, "web_traffic": true,
}
var sortedReportPageTypes = []string{"ai", "errors", "logs", "metrics", "rum", "traces", "web_traffic", "work_items"}

// isFalsy mirrors Python truthiness for JSON-decoded values (used by `x or {}` idioms).
func isFalsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case float64:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// orDefault returns v if non-empty, else def (mirrors `(form.get(k) or default)`).
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// clampInt mirrors `max(lo, min(hi, n))`.
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// chDateTimeRe matches a chdb DateTime64 string "YYYY-MM-DD HH:MM:SS.ffffff" (space separator,
// fractional part). ISO strings (with 'T' / timezone) do not match and pass through untouched.
var chDateTimeRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\.(\d+)$`)

// pyDateTimeStr converts a chdb DateTime64 string to Python's str(datetime) rendering: the Go
// chdb driver returns "2024-01-02 03:00:00.000", but Python's chdb yields a datetime whose
// str() drops an all-zero fraction ("2024-01-02 03:00:00") and otherwise pads microseconds to
// 6 digits ("...00.120000"). Non-matching strings (ISO create-path timestamps, "") pass through.
func pyDateTimeStr(s string) string {
	m := chDateTimeRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	frac := strings.TrimRight(m[2], "0")
	if frac == "" {
		return m[1] // all-zero fraction -> drop it entirely
	}
	// Pad/truncate the (non-zero) fraction to 6-digit microseconds, as datetime str() does.
	for len(frac) < 6 {
		frac += "0"
	}
	return m[1] + "." + frac[:6]
}

// chDateTimeKeys mirrors the dt_keys set in app.py _insert_rows_json_each_row.
var chDateTimeKeys = map[string]bool{
	"Timestamp": true, "TimeUnix": true, "UpdatedAt": true, "CreatedAt": true,
	"CompletedAt": true, "ReleasedAt": true, "UploadedAt": true, "ScannedAt": true,
}

// normalizeCHTimestamp mirrors app.py _normalize_ch_timestamp: any common timestamp form ->
// a ClickHouse DateTime64-compatible "2006-01-02 15:04:05.000000" (UTC).
func normalizeCHTimestamp(v any) string {
	// Mirror Python _normalize_ch_timestamp's isinstance(value, datetime) branch: a time.Time
	// formats directly (toStr would yield Go's "... +0000 UTC" String(), which no layout parses).
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format("2006-01-02 15:04:05.000000")
	}
	// Python: raw = str(value or "").strip() — `value or ""` collapses every FALSY value (None,
	// False, 0, 0.0, -0.0, "", empty container) to "", so they fall through to now() below. Using
	// toStr unconditionally would turn False/0 into "False"/"0" and then leak a bad DateTime string.
	raw := ""
	if rumTruthy(v) {
		raw = strings.TrimSpace(toStr(v))
	}
	if raw == "" {
		return nowUTC().Format("2006-01-02 15:04:05.000000")
	}
	parse := strings.Replace(raw, "Z", "+00:00", 1)
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, parse); err == nil {
			return t.UTC().Format("2006-01-02 15:04:05.000000")
		}
	}
	// Last resort: preserve value (replace T with space) and hope the parser accepts it.
	return strings.Replace(raw, "T", " ", 1)
}

// insertRowsNormalized mirrors app.py _insert_rows_json_each_row: copies each row, normalizes
// the DateTime64 columns, then writes via JSONEachRow. The caller's original rows keep their
// un-normalized (ISO) timestamps for serialization, exactly as Python does.
func (s *server) insertRowsNormalized(table string, rows []map[string]any) (int, error) {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := make(map[string]any, len(row))
		for k, v := range row {
			if chDateTimeKeys[k] {
				item[k] = normalizeCHTimestamp(v)
			} else {
				item[k] = v
			}
		}
		out = append(out, item)
	}
	return s.db.InsertJSONEachRow(table, out)
}
