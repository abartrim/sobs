// Package storetest provides an in-memory store.DB double for unit tests. It is only ever
// imported by test code, so it is not linked into the production binary — yet it lets any
// server method or helper that depends on s.db be exercised directly, without the embedded
// engine. This is the same seam the store.DB interface was designed for (see store.go).
package storetest

import "github.com/sobs/sobs/internal/store"

// FakeDB is a configurable store.DB double. Set ExecuteFunc to answer queries; InsertJSONEachRow
// calls are recorded in Inserts in call order. The zero value is usable: Execute returns an empty
// result and inserts are accepted.
type FakeDB struct {
	// ExecuteFunc answers Execute. When nil, Execute returns an empty *store.Result (no rows).
	ExecuteFunc func(query string, params ...any) (*store.Result, error)
	// Inserts records every InsertJSONEachRow call, in order.
	Inserts []Insert
	// InsertErr, when set, is returned by every InsertJSONEachRow call instead of success (the
	// call is still recorded in Inserts first, matching a real driver's partial-write shape).
	InsertErr error
	// Closed is set true by Close.
	Closed bool
}

// Insert is one recorded InsertJSONEachRow call.
type Insert struct {
	Table string
	Rows  []map[string]any
}

// Execute implements store.DB.
func (f *FakeDB) Execute(query string, params ...any) (*store.Result, error) {
	if f.ExecuteFunc != nil {
		return f.ExecuteFunc(query, params...)
	}
	return &store.Result{}, nil
}

// InsertJSONEachRow implements store.DB, recording the call and reporting all rows written.
func (f *FakeDB) InsertJSONEachRow(table string, rows []map[string]any) (int, error) {
	f.Inserts = append(f.Inserts, Insert{Table: table, Rows: rows})
	if f.InsertErr != nil {
		return 0, f.InsertErr
	}
	return len(rows), nil
}

// Close implements store.DB.
func (f *FakeDB) Close() error { f.Closed = true; return nil }

// Result builds a *store.Result from column names and rows (each row parallel to cols).
func Result(cols []string, rows ...[]any) *store.Result {
	return &store.Result{Columns: cols, Rows: rows}
}

// SettingsDB returns a FakeDB whose Execute answers the app-settings read path
// (`SELECT Value FROM sobs_app_settings ... WHERE Key = ?`) from the given map: params[0] is the
// key, and the single-cell "Value" result is returned (empty result for unknown keys). Any other
// query falls through to ExecuteFunc if set, else an empty result. This covers the many helpers
// that read configuration via appSetting/appSettingRaw.
func SettingsDB(settings map[string]string) *FakeDB {
	return &FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		return &store.Result{}, nil
	}}
}
