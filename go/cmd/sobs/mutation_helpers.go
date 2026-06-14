package main

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// newUUIDHex mirrors uuid.uuid4().hex for production inserts. Parity test bodies always pass an
// explicit `id`, so this is never reached under capture/replay (uuid is not parity-frozen in Go).
func newUUIDHex() string { return hex.EncodeToString(randBytes(16)) }

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
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
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
	raw := strings.TrimSpace(toStr(v))
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
