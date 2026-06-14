package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	chdb "github.com/chdb-io/chdb-go/chdb"
)

// chdbStore is the production DB backed by chdb-go (purego/dlopen of libchdb at runtime;
// no cgo). It mirrors app.py's ChDbConnection: one persistent on-disk session opened on
// the shared sobs.chdb directory, queried with ClickHouse SQL verbatim.
//
// libchdb is located at runtime via CHDB_LIB_PATH (see go/CHDB_PIN.md). Build does NOT
// need the native library; only a running query does.
type chdbStore struct {
	sess *chdb.Session
}

// Open opens (or creates) the chdb session at dataDir/sobs.chdb — the SAME directory the
// Python app uses, enabling the shared-storage hard cutover.
func Open(dataDir string) (DB, error) {
	path := filepath.Join(dataDir, "sobs.chdb")
	sess, err := chdb.NewSession(path)
	if err != nil {
		return nil, fmt.Errorf("chdb open %s: %w", path, err)
	}
	return &chdbStore{sess: sess}, nil
}

// Close persists and closes the session. NOTE: Close, never Cleanup — Cleanup does
// os.RemoveAll on the data directory (see go/CHDB_PIN.md).
func (s *chdbStore) Close() error {
	s.sess.Close()
	return nil
}

// chJSON is ClickHouse's FORMAT JSON envelope.
type chJSON struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data []map[string]any `json:"data"`
}

// Execute runs a query and returns columns + rows. Parameters are substituted by the
// caller for now (chdb-go has no server-side bind); query construction stays identical to
// the Python side, which also builds SQL with ? placeholders filled positionally.
func (s *chdbStore) Execute(query string, params ...any) (*Result, error) {
	sql, err := inlineParams(query, params)
	if err != nil {
		return nil, err
	}
	res, err := s.sess.Query(sql, "JSON")
	if err != nil {
		return nil, fmt.Errorf("chdb query: %w", err)
	}
	if res != nil {
		if e := res.Error(); e != nil {
			return nil, fmt.Errorf("chdb query: %w", e)
		}
	}
	out := res.String()
	if out == "" {
		return &Result{}, nil
	}
	var env chJSON
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, fmt.Errorf("chdb decode JSON: %w (got %.200q)", err, out)
	}
	cols := make([]string, len(env.Meta))
	types := make([]string, len(env.Meta))
	for i, m := range env.Meta {
		cols[i] = m.Name
		types[i] = m.Type
	}
	rows := make([][]any, len(env.Data))
	for i, d := range env.Data {
		row := make([]any, len(cols))
		for j, c := range cols {
			row[j] = d[c]
		}
		rows[i] = row
	}
	return &Result{Columns: cols, Types: types, Rows: rows}, nil
}

// InsertJSONEachRow inserts rows via ClickHouse JSONEachRow, mirroring app.py
// _insert_rows_json_each_row: one JSON object per line after "INSERT INTO t FORMAT
// JSONEachRow". HTML escaping is disabled so stored strings keep <, >, & verbatim
// (Go's json HTML-escapes by default). Key order is irrelevant — JSONEachRow maps by
// column name. Callers must pre-normalize DateTime columns to ClickHouse strings, exactly
// as the Python helper does before insert.
func (s *chdbStore) InsertJSONEachRow(table string, rows []map[string]any) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" FORMAT JSONEachRow\n")
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil { // Encode appends '\n' per row
			return 0, fmt.Errorf("encode row for %s: %w", table, err)
		}
	}
	res, err := s.sess.Query(b.String(), "")
	if err != nil {
		return 0, fmt.Errorf("chdb insert into %s: %w", table, err)
	}
	if res != nil {
		if e := res.Error(); e != nil {
			return 0, fmt.Errorf("chdb insert into %s: %w", table, e)
		}
	}
	return len(rows), nil
}
