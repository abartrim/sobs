package main

import (
	_ "embed"
	"log"
	"strings"
)

// schemaSQL is a verbatim copy of app.py's SCHEMA block (the 43 CREATE TABLE/VIEW IF NOT EXISTS
// statements). app.py is the frozen oracle and never changes, so the copy stays in sync.
//
//go:embed schema.sql
var schemaSQL string

// ensureSchema mirrors the schema half of Python get_db()'s startup: apply the production SCHEMA so
// a fresh deployment self-initializes its chdb store instead of requiring an external seed — "make
// if not found, just as Python". It is guarded on a sentinel table so it is a STRICT no-op when the
// store already carries the schema (the Python-seeded parity fixture, or any existing prod store),
// which keeps byte parity untouched. The example demo-data seeder (_ensure_post_schema_state) is
// intentionally NOT replayed here: a fresh prod store comes up empty-but-schema'd, and the demo
// content is the parity oracle's concern, not a deployment requirement.
func (s *server) ensureSchema() {
	if s.db == nil || s.schemaPresent() {
		return
	}
	stmts := splitSQLStatements(schemaSQL)
	log.Printf("chdb store has no schema — applying %d DDL statements (first-run init)", len(stmts))
	for _, stmt := range stmts {
		if _, err := s.db.Execute(stmt); err != nil {
			log.Printf("schema init: statement failed (%v): %.80s", err, strings.ReplaceAll(stmt, "\n", " "))
		}
	}
}

// schemaPresent reports whether the canonical otel_logs table already exists in the store.
func (s *server) schemaPresent() bool {
	res, err := s.db.Execute(
		"SELECT count() AS c FROM system.tables WHERE database = currentDatabase() AND name = 'otel_logs'")
	if err != nil {
		return false
	}
	for _, m := range rowMaps(res) {
		if cInt(m, "c") > 0 {
			return true
		}
	}
	return false
}

// splitSQLStatements splits the SCHEMA script on ';' (no statement contains an embedded ';') and
// drops blank fragments — the Go-side equivalent of chdb's executescript(SCHEMA).
func splitSQLStatements(script string) []string {
	out := []string{}
	for _, stmt := range strings.Split(script, ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}
