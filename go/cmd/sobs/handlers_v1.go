package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// nowISO mirrors app.py _now_iso(): UTC isoformat with millisecond precision, e.g.
// "2024-01-02T03:04:05.000+00:00". Create responses serialize this in-memory value; the GET
// path instead returns the chdb DateTime64(3) string ("2024-01-02 03:04:05.000").
func nowISO() string { return nowUTC().Format("2006-01-02T15:04:05.000-07:00") }

var appSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// appSlug mirrors app.py _app_slug: lowercase, non-alnum runs -> "-", strip "-", fallback,
// truncate to 80.
func appSlug(value, fallback string) string {
	s := appSlugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(value)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = fallback
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// safeJSONDumps mirrors app.py _safe_json_dumps: None/empty/invalid/non-collection -> "{}";
// strings are re-parsed; dict/list are compacted. The stored MetadataJson is always re-parsed
// by a reader before output, so compact separators (vs Python's ", ") are immaterial.
func safeJSONDumps(v any) string {
	switch t := v.(type) {
	case nil:
		return "{}"
	case string:
		st := strings.TrimSpace(t)
		if st == "" {
			return "{}"
		}
		var parsed any
		if json.Unmarshal([]byte(st), &parsed) != nil {
			return "{}"
		}
		b, _ := json.Marshal(parsed)
		return string(b)
	case map[string]any, []any:
		b, _ := json.Marshal(t)
		return string(b)
	}
	return "{}"
}

// parseBoolPy mirrors app.py _parse_bool.
func parseBoolPy(v any, def bool) bool {
	switch t := v.(type) {
	case nil:
		return def
	case bool:
		return t
	}
	switch strings.ToLower(strings.TrimSpace(toStr(v))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// serializeAppRow mirrors app.py _serialize_app_row. createdAt/updatedAt/enabled/metadata are
// read straight from the row map, so the create path (in-memory ISO row) and GET path (chdb
// row) both serialize correctly from the same function.
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
		Set("createdAt", pyDateTimeStr(cStr(m, "CreatedAt"))).
		Set("updatedAt", pyDateTimeStr(cStr(m, "UpdatedAt")))
}

// serializeReleaseRow mirrors app.py _serialize_release_row.
func serializeReleaseRow(m map[string]any) *jsonenc.Object {
	return jsonenc.NewObject().
		Set("id", cStr(m, "Id")).
		Set("appId", cStr(m, "AppId")).
		Set("version", cStr(m, "ReleaseVersion")).
		Set("commitSha", cStr(m, "CommitSha")).
		Set("buildId", cStr(m, "BuildId")).
		Set("environment", cStr(m, "Environment")).
		Set("releasedAt", pyDateTimeStr(cStr(m, "ReleasedAt"))).
		Set("metadata", parseJSONObject(cStr(m, "MetadataJson")))
}

// serializeArtifactRow mirrors app.py _serialize_artifact_row.
func serializeArtifactRow(m map[string]any) *jsonenc.Object {
	return jsonenc.NewObject().
		Set("id", cStr(m, "Id")).
		Set("releaseId", cStr(m, "ReleaseId")).
		Set("artifactType", cStr(m, "ArtifactType")).
		Set("name", cStr(m, "Name")).
		Set("contentType", cStr(m, "ContentType")).
		Set("size", cInt(m, "Size")).
		Set("storageRef", cStr(m, "StorageRef")).
		Set("checksumSha256", cStr(m, "ChecksumSha256")).
		Set("platform", cStr(m, "Platform")).
		Set("architecture", cStr(m, "Architecture")).
		Set("metadata", parseJSONObject(cStr(m, "MetadataJson"))).
		Set("uploadedAt", pyDateTimeStr(cStr(m, "UploadedAt")))
}

// v1err writes a plain {"error": msg} (no "ok" field) — the v1 registry's error shape.
func (s *server) v1err(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, jsonenc.NewObject().Set("error", msg))
}

func (s *server) rowExists(query string, params ...any) bool {
	res, err := s.db.Execute(query, params...)
	return err == nil && len(res.Rows) > 0
}

func (s *server) findAppByID(appID string) (map[string]any, bool) {
	res, err := s.db.Execute("SELECT * FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", appID)
	if err != nil || len(res.Rows) == 0 {
		return nil, false
	}
	return rowMaps(res)[0], true
}

func (s *server) findReleaseByID(relID string) (map[string]any, bool) {
	res, err := s.db.Execute("SELECT * FROM sobs_app_releases FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", relID)
	if err != nil || len(res.Rows) == 0 {
		return nil, false
	}
	return rowMaps(res)[0], true
}

// GET /v1/apps/<app_id> (+ PATCH update, + /releases GET list & POST register) — app.py
// get_app_registry_entry / update_app_registry_entry / list_app_releases / create_app_release.
func (s *server) handleV1AppByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPatch && r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/apps/")

	// POST /v1/apps/<app_id>/releases — create_app_release.
	if appID, ok := strings.CutSuffix(rest, "/releases"); ok && r.Method == http.MethodPost {
		if _, found := s.findAppByID(appID); !found {
			s.v1err(w, http.StatusNotFound, "app not found")
			return
		}
		payload := bodyMap(r)
		relVersion := bstr(payload, "version")
		if relVersion == "" {
			s.v1err(w, http.StatusBadRequest, "version is required")
			return
		}
		releasedAt := bstr(payload, "releasedAt")
		if releasedAt == "" {
			releasedAt = nowISO()
		}
		id := bstr(payload, "id")
		if id == "" {
			id = newUUIDHex()
		}
		row := map[string]any{
			"Id":             id,
			"AppId":          appID,
			"ReleaseVersion": relVersion,
			"CommitSha":      bstr(payload, "commitSha"),
			"BuildId":        bstr(payload, "buildId"),
			"Environment":    bstr(payload, "environment"),
			"ReleasedAt":     releasedAt,
			"MetadataJson":   safeJSONDumps(payload["metadata"]),
			"IsDeleted":      0,
			"Version":        fixedVersionMillis(),
		}
		if _, err := s.insertRowsNormalized("sobs_app_releases", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, serializeReleaseRow(row))
		return
	}

	// PATCH /v1/apps/<app_id> — update_app_registry_entry.
	if r.Method == http.MethodPatch {
		current, found := s.findAppByID(rest)
		if !found {
			s.v1err(w, http.StatusNotFound, "not found")
			return
		}
		payload := bodyMap(r)
		name := payloadStrDefault(payload, "name", cStr(current, "Name"))
		if name == "" {
			s.v1err(w, http.StatusBadRequest, "name is required")
			return
		}
		slugSrc := payloadStrDefault(payload, "slug", cStr(current, "Slug"))
		if slugSrc == "" {
			slugSrc = name
		}
		slug := appSlug(slugSrc, "app")
		if s.rowExists("SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 AND Id!=? LIMIT 1", slug, rest) {
			s.v1err(w, http.StatusConflict, "app slug already exists")
			return
		}
		createdAt := cStr(current, "CreatedAt")
		if createdAt == "" {
			createdAt = nowISO()
		}
		enabled := parseBoolPy(payloadDefault(payload, "enabled", cBool(current, "Enabled")), true)
		row := map[string]any{
			"Id":                 rest,
			"Name":               name,
			"Slug":               slug,
			"OwnerTeam":          payloadStrDefault(payload, "ownerTeam", cStr(current, "OwnerTeam")),
			"RepoUrl":            payloadStrDefault(payload, "repoUrl", cStr(current, "RepoUrl")),
			"DefaultEnvironment": payloadStrDefault(payload, "defaultEnvironment", cStr(current, "DefaultEnvironment")),
			"Enabled":            boolToInt(enabled),
			"MetadataJson":       s.patchMetadata(payload, current),
			"IsDeleted":          0,
			"Version":            fixedVersionMillis(),
			"CreatedAt":          createdAt,
			"UpdatedAt":          nowISO(),
		}
		if _, err := s.insertRowsNormalized("sobs_apps", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, serializeAppRow(row))
		return
	}

	// GET /v1/apps/<app_id>/releases — list_app_releases.
	if appID, ok := strings.CutSuffix(rest, "/releases"); ok {
		if _, found := s.findAppByID(appID); !found {
			s.v1err(w, http.StatusNotFound, "app not found")
			return
		}
		res, err := s.db.Execute("SELECT * FROM sobs_app_releases FINAL WHERE AppId=? AND IsDeleted=0 ORDER BY ReleasedAt DESC", appID)
		if err != nil {
			s.dbError(w, err)
			return
		}
		out := []any{}
		for _, m := range rowMaps(res) {
			out = append(out, serializeReleaseRow(m))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	// GET /v1/apps/<app_id> — get_app_registry_entry.
	row, found := s.findAppByID(rest)
	if !found {
		s.v1err(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, serializeAppRow(row))
}

// patchMetadata returns the stored MetadataJson for a PATCH: payload metadata if present, else
// the current row's metadata re-dumped (app.py update_app_registry_entry).
func (s *server) patchMetadata(payload, current map[string]any) string {
	if v, ok := payload["metadata"]; ok {
		return safeJSONDumps(v)
	}
	return safeJSONDumps(cStr(current, "MetadataJson"))
}

// GET /v1/releases/<release_id> (+ /artifacts GET list & POST meta) — app.py get_release /
// list_release_artifacts / create_release_artifact_meta.
func (s *server) handleV1ReleaseByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/releases/")

	// POST /v1/releases/<release_id>/artifacts/meta — create_release_artifact_meta.
	if relID, ok := strings.CutSuffix(rest, "/artifacts/meta"); ok && r.Method == http.MethodPost {
		if _, found := s.findReleaseByID(relID); !found {
			s.v1err(w, http.StatusNotFound, "release not found")
			return
		}
		payload := bodyMap(r)
		artifactType := bstr(payload, "artifactType")
		name := bstr(payload, "name")
		if artifactType == "" || name == "" {
			s.v1err(w, http.StatusBadRequest, "artifactType and name are required")
			return
		}
		uploadedAt := bstr(payload, "uploadedAt")
		if uploadedAt == "" {
			uploadedAt = nowISO()
		}
		id := bstr(payload, "id")
		if id == "" {
			id = newUUIDHex()
		}
		row := map[string]any{
			"Id":             id,
			"ReleaseId":      relID,
			"ArtifactType":   artifactType,
			"Name":           name,
			"ContentType":    bstr(payload, "contentType"),
			"Size":           bNum(payload, "size"),
			"StorageRef":     bstr(payload, "storageRef"),
			"ChecksumSha256": bstr(payload, "checksumSha256"),
			"Platform":       bstr(payload, "platform"),
			"Architecture":   bstr(payload, "architecture"),
			"MetadataJson":   safeJSONDumps(payload["metadata"]),
			"UploadedAt":     uploadedAt,
			"IsDeleted":      0,
			"Version":        fixedVersionMillis(),
		}
		if _, err := s.insertRowsNormalized("sobs_release_artifacts", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, serializeArtifactRow(row))
		return
	}

	// GET /v1/releases/<release_id>/artifacts — list_release_artifacts.
	if relID, ok := strings.CutSuffix(rest, "/artifacts"); ok {
		if _, found := s.findReleaseByID(relID); !found {
			s.v1err(w, http.StatusNotFound, "release not found")
			return
		}
		writeJSON(w, http.StatusOK, s.artifactsForRelease(relID))
		return
	}

	// GET /v1/releases/<release_id> — get_release (release + artifacts).
	row, found := s.findReleaseByID(rest)
	if !found {
		s.v1err(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("release", serializeReleaseRow(row)).
		Set("artifacts", s.artifactsForRelease(rest)))
}

func (s *server) artifactsForRelease(relID string) []any {
	res, err := s.db.Execute("SELECT * FROM sobs_release_artifacts FINAL WHERE ReleaseId=? AND IsDeleted=0 ORDER BY UploadedAt DESC", relID)
	out := []any{}
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		out = append(out, serializeArtifactRow(m))
	}
	return out
}

// GET /v1/apps — app.py list_apps; POST /v1/apps — create_app_registry_entry.
func (s *server) handleV1Apps(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		payload := bodyMap(r)
		name := bstr(payload, "name")
		if name == "" {
			s.v1err(w, http.StatusBadRequest, "name is required")
			return
		}
		slugSrc := bstr(payload, "slug")
		if slugSrc == "" {
			slugSrc = name
		}
		slug := appSlug(slugSrc, "app")
		if s.rowExists("SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1", slug) {
			s.v1err(w, http.StatusConflict, "app slug already exists")
			return
		}
		id := bstr(payload, "id")
		if id == "" {
			id = newUUIDHex()
		}
		enabled := parseBoolPy(payloadDefault(payload, "enabled", true), true)
		row := map[string]any{
			"Id":                 id,
			"Name":               name,
			"Slug":               slug,
			"OwnerTeam":          bstr(payload, "ownerTeam"),
			"RepoUrl":            bstr(payload, "repoUrl"),
			"DefaultEnvironment": bstr(payload, "defaultEnvironment"),
			"Enabled":            boolToInt(enabled),
			"MetadataJson":       safeJSONDumps(payload["metadata"]),
			"IsDeleted":          0,
			"Version":            fixedVersionMillis(),
			"CreatedAt":          nowISO(),
			"UpdatedAt":          nowISO(),
		}
		if _, err := s.insertRowsNormalized("sobs_apps", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, serializeAppRow(row))
		return
	}
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
