package main

import (
	"fmt"
	"net/http"

	"github.com/sobs/sobs/internal/jsonenc"
)

// dmSettingKeys mirrors app.py _DM_SETTING_KEYS.
var dmSettingKeys = []string{
	"data_management.backup_enabled", "data_management.s3_bucket", "data_management.s3_access_key_id",
	"data_management.s3_secret_access_key", "data_management.s3_region", "data_management.s3_path_prefix",
	"data_management.s3_encrypt_backup", "data_management.backup_encryption_password",
	"data_management.backup_schedule_full", "data_management.backup_schedule_incremental",
	"data_management.ttl_logs_days", "data_management.ttl_traces_days", "data_management.ttl_metrics_hours",
	"data_management.ttl_sessions_days", "data_management.ttl_backup_coupling_enabled",
}

// fmtBytes mirrors app.py _fmt_bytes: a human-readable byte count (or "—" for nil).
func fmtBytes(pos []any) string {
	if len(pos) == 0 || pos[0] == nil {
		return "—"
	}
	var n float64
	switch v := pos[0].(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	default:
		return "—"
	}
	switch {
	case n >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", n/(1024*1024*1024))
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", n/1024)
	default:
		return fmt.Sprintf("%d B", int64(n))
	}
}

// buildDBStats mirrors app.py _get_db_stats: overall + per-table system.parts aggregates plus
// the active-query count. The byte sizes / ratios are storage-dependent (masked in parity); the
// row counts, table names, and structure are deterministic.
func (s *server) buildDBStats() *jsonenc.Object {
	stats := jsonenc.NewObject().
		Set("compressed_bytes", nil).Set("uncompressed_bytes", nil).
		Set("compression_ratio", nil).Set("total_rows", nil).
		Set("active_queries", nil).Set("tables", []any{})
	if res, err := s.db.Execute("SELECT sum(data_compressed_bytes) AS comp, " +
		"sum(data_uncompressed_bytes) AS uncomp, sum(rows) AS rws " +
		"FROM system.parts WHERE active = 1 AND database = currentDatabase()"); err == nil && len(res.Rows) > 0 {
		m := rowMaps(res)[0]
		comp, uncomp := cInt(m, "comp"), cInt(m, "uncomp")
		stats.Set("compressed_bytes", comp).Set("uncompressed_bytes", uncomp).Set("total_rows", cInt(m, "rws"))
		if comp > 0 {
			stats.Set("compression_ratio", round2(float64(uncomp)/float64(comp)))
		}
	}
	if res, err := s.db.Execute("SELECT table, sum(data_compressed_bytes) AS comp, " +
		"sum(data_uncompressed_bytes) AS uncomp, sum(rows) AS rws " +
		"FROM system.parts WHERE active = 1 AND database = currentDatabase() " +
		"GROUP BY table ORDER BY comp DESC LIMIT 10"); err == nil {
		tables := []any{}
		for _, m := range rowMaps(res) {
			comp, uncomp := cInt(m, "comp"), cInt(m, "uncomp")
			t := jsonenc.NewObject().Set("table", cStr(m, "table")).
				Set("compressed_bytes", comp).Set("uncompressed_bytes", uncomp).Set("rows", cInt(m, "rws"))
			if comp > 0 {
				t.Set("compression_ratio", round2(float64(uncomp)/float64(comp)))
			} else {
				t.Set("compression_ratio", nil)
			}
			tables = append(tables, t)
		}
		stats.Set("tables", tables)
	}
	stats.Set("active_queries", s.countRows("SELECT COUNT(*) AS cnt FROM system.processes"))
	return stats
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// GET /settings/data-management — app.py view_dm_settings: render the DB-stats + DM-settings
// page. The system.parts byte sizes / ratios are masked in parity; everything else is compared.
func (s *server) handleDataManagementGet(w http.ResponseWriter, r *http.Request) {
	dm := map[string]any{}
	for _, k := range dmSettingKeys {
		v, _ := s.appSetting(k)
		dm[k] = v
	}
	q := r.URL.Query()
	flashType := q.Get("msg_type")
	if flashType == "" {
		flashType = "success"
	}
	s.renderPage(w, "settings_data_management.html", "view_dm_settings", map[string]any{
		"dm_settings": dm,
		"dm_secret_present": map[string]any{
			"s3_secret_access_key": false, "backup_encryption_password": false,
		},
		"flash_msg": q.Get("msg"), "flash_type": flashType,
		"db_stats": s.buildDBStats(),
	})
}
