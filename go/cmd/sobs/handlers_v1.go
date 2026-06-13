package main

import (
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// serializeAppRow mirrors app.py _serialize_app_row: a sobs_apps row -> public JSON shape.
func serializeAppRow(m map[string]any) *jsonenc.Object {
	enabled := true
	if _, ok := m["Enabled"]; ok {
		enabled = cBool(m, "Enabled")
	}
	return jsonenc.NewObject().
		Set("id", cStr(m, "Id")).
		Set("name", cStr(m, "Name")).
		Set("slug", cStr(m, "Slug")).
		Set("ownerTeam", cStr(m, "OwnerTeam")).
		Set("repoUrl", cStr(m, "RepoUrl")).
		Set("defaultEnvironment", cStr(m, "DefaultEnvironment")).
		Set("enabled", enabled).
		Set("metadata", parseJSONObject(cStr(m, "MetadataJson"))).
		Set("createdAt", cStr(m, "CreatedAt")).
		Set("updatedAt", cStr(m, "UpdatedAt"))
}

// GET /v1/apps — app.py list_apps (app.py:10301). All non-deleted apps, optional ?q filter
// on name/slug. Empty registry on the fixture -> [].
func (s *server) handleV1Apps(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute("SELECT * FROM sobs_apps FINAL WHERE IsDeleted=0 ORDER BY Name ASC")
	if err != nil {
		s.dbError(w, err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	apps := []any{}
	for _, m := range rowMaps(res) {
		if q != "" {
			name := strings.ToLower(cStr(m, "Name"))
			slug := strings.ToLower(cStr(m, "Slug"))
			if !strings.Contains(name, q) && !strings.Contains(slug, q) {
				continue
			}
		}
		apps = append(apps, serializeAppRow(m))
	}
	writeJSON(w, http.StatusOK, apps)
}
