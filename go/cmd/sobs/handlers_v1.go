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

// v1err writes a plain {"error": msg} (no "ok" field) at the status — the v1 registry's
// 404 shape.
func (s *server) v1err(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, jsonenc.NewObject().Set("error", msg))
}

func (s *server) rowExists(query string, params ...any) bool {
	res, err := s.db.Execute(query, params...)
	return err == nil && len(res.Rows) > 0
}

// GET /v1/apps/<app_id>  and  /v1/apps/<app_id>/releases — app.py get_app_registry_entry /
// list_app_releases. Empty registry on the fixture -> 404.
func (s *server) handleV1AppByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/apps/")
	appExists := func(id string) bool {
		return s.rowExists("SELECT 1 FROM sobs_apps FINAL WHERE (Id=? OR Slug=?) AND IsDeleted=0 LIMIT 1", id, id)
	}
	if appID, ok := strings.CutSuffix(rest, "/releases"); ok {
		if !appExists(appID) {
			s.v1err(w, http.StatusNotFound, "app not found")
			return
		}
		writeJSON(w, http.StatusOK, []any{}) // found: releases (empty on fixture)
		return
	}
	if !appExists(rest) {
		s.v1err(w, http.StatusNotFound, "not found")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented) // found-app serialize: follow-up
}

// GET /v1/releases/<release_id>  and  /v1/releases/<release_id>/artifacts — app.py
// get_release / list_release_artifacts. Empty registry -> 404.
func (s *server) handleV1ReleaseByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/releases/")
	relExists := func(id string) bool {
		return s.rowExists("SELECT 1 FROM sobs_app_releases FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", id)
	}
	if relID, ok := strings.CutSuffix(rest, "/artifacts"); ok {
		if !relExists(relID) {
			s.v1err(w, http.StatusNotFound, "release not found")
			return
		}
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	if !relExists(rest) {
		s.v1err(w, http.StatusNotFound, "not found")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented) // found-release serialize: follow-up
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
