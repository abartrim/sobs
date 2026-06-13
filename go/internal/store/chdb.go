package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"

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
	for i, m := range env.Meta {
		cols[i] = m.Name
	}
	rows := make([][]any, len(env.Data))
	for i, d := range env.Data {
		row := make([]any, len(cols))
		for j, c := range cols {
			row[j] = d[c]
		}
		rows[i] = row
	}
	return &Result{Columns: cols, Rows: rows}, nil
}

// InsertJSONEachRow inserts rows via ClickHouse JSONEachRow (mirrors app.py
// _insert_rows_json_each_row). Implemented when ingest routes land (Phase 3).
func (s *chdbStore) InsertJSONEachRow(table string, rows []map[string]any) (int, error) {
	return 0, fmt.Errorf("InsertJSONEachRow not yet implemented")
}
