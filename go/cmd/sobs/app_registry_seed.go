package main

import (
	"encoding/json"
	"log"
	"strings"
)

// seedAppRegistry is a faithful port of app.py _seed_app_release_registry_from_env: when
// SOBS_APP_REGISTRY_SEED_JSON (or _FILE) holds an app/release/artifact graph, insert it at startup,
// reusing existing apps (by slug) and releases (by version+commit+environment). Unset env -> a
// strict no-op, so the parity corpus (which never sets it) is unaffected. Runs once after the
// schema is ready.
func (s *server) seedAppRegistry() {
	raw := readFileOrEnv("SOBS_APP_REGISTRY_SEED_JSON", "SOBS_APP_REGISTRY_SEED_JSON_FILE")
	if raw == "" || s.db == nil {
		return
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("Failed to parse app registry seed JSON: %v", err)
		return
	}
	var apps []any
	switch p := parsed.(type) {
	case map[string]any:
		apps, _ = p["apps"].([]any)
	case []any:
		apps = p
	default:
		log.Printf("Ignoring app registry seed: expected object with 'apps' or an array")
		return
	}

	version := fixedVersionMillis()
	var appRows, releaseRows, artifactRows []map[string]any

	for _, ai := range apps {
		appItem, ok := ai.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(toStr(appItem["name"]))
		if name == "" {
			continue
		}
		rawSlug := strings.TrimSpace(toStr(appItem["slug"]))
		if rawSlug == "" {
			rawSlug = name
		}
		slug := appSlug(rawSlug, name)
		appID := strings.TrimSpace(toStr(appItem["id"]))
		if appID == "" {
			if existing := s.lookupAppIDBySlug(slug); existing != "" {
				appID = existing
			} else {
				appID = newUUIDHex()
			}
		}
		appRows = append(appRows, map[string]any{
			"Id": appID, "Name": name, "Slug": slug,
			"OwnerTeam":          strings.TrimSpace(toStr(appItem["ownerTeam"])),
			"RepoUrl":            strings.TrimSpace(toStr(appItem["repoUrl"])),
			"DefaultEnvironment": strings.TrimSpace(toStr(appItem["defaultEnvironment"])),
			"Enabled":            boolToIntSeed(parseBoolPy(appItem["enabled"], true)),
			"MetadataJson":       safeJSONDumps(appItem["metadata"]),
			"IsDeleted":          0, "Version": version, "CreatedAt": nowISO(), "UpdatedAt": nowISO(),
		})

		releases, _ := appItem["releases"].([]any)
		for _, ri := range releases {
			rel, ok := ri.(map[string]any)
			if !ok {
				continue
			}
			relVersion := strings.TrimSpace(toStr(rel["version"]))
			if relVersion == "" {
				continue
			}
			commitSha := strings.TrimSpace(toStr(rel["commitSha"]))
			environment := strings.TrimSpace(toStr(rel["environment"]))
			relID := strings.TrimSpace(toStr(rel["id"]))
			if relID == "" {
				if existing := s.lookupReleaseID(appID, relVersion, commitSha, environment); existing != "" {
					relID = existing
				} else {
					relID = newUUIDHex()
				}
			}
			releasedAt := strings.TrimSpace(toStr(rel["releasedAt"]))
			if releasedAt == "" {
				releasedAt = nowISO()
			}
			releaseRows = append(releaseRows, map[string]any{
				"Id": relID, "AppId": appID, "ReleaseVersion": relVersion, "CommitSha": commitSha,
				"BuildId":     strings.TrimSpace(toStr(rel["buildId"])),
				"Environment": environment, "ReleasedAt": releasedAt,
				"MetadataJson": safeJSONDumps(rel["metadata"]), "IsDeleted": 0, "Version": version,
			})

			artifacts, _ := rel["artifacts"].([]any)
			for _, ar := range artifacts {
				art, ok := ar.(map[string]any)
				if !ok {
					continue
				}
				artifactType := strings.TrimSpace(toStr(art["artifactType"]))
				artifactName := strings.TrimSpace(toStr(art["name"]))
				if artifactType == "" || artifactName == "" {
					continue
				}
				artID := strings.TrimSpace(toStr(art["id"]))
				if artID == "" {
					artID = newUUIDHex()
				}
				uploadedAt := strings.TrimSpace(toStr(art["uploadedAt"]))
				if uploadedAt == "" {
					uploadedAt = nowISO()
				}
				artifactRows = append(artifactRows, map[string]any{
					"Id": artID, "ReleaseId": relID, "ArtifactType": artifactType, "Name": artifactName,
					"ContentType":    strings.TrimSpace(toStr(art["contentType"])),
					"Size":           int(otlpFloat(art["size"])),
					"StorageRef":     strings.TrimSpace(toStr(art["storageRef"])),
					"ChecksumSha256": strings.TrimSpace(toStr(art["checksumSha256"])),
					"Platform":       strings.TrimSpace(toStr(art["platform"])),
					"Architecture":   strings.TrimSpace(toStr(art["architecture"])),
					"MetadataJson":   safeJSONDumps(art["metadata"]),
					"UploadedAt":     uploadedAt, "IsDeleted": 0, "Version": version,
				})
			}
		}
	}

	if len(appRows) > 0 {
		_, _ = s.db.InsertJSONEachRow("sobs_apps", appRows)
	}
	if len(releaseRows) > 0 {
		_, _ = s.db.InsertJSONEachRow("sobs_app_releases", releaseRows)
	}
	if len(artifactRows) > 0 {
		_, _ = s.db.InsertJSONEachRow("sobs_release_artifacts", artifactRows)
	}
	log.Printf("app registry seed: %d apps, %d releases, %d artifacts", len(appRows), len(releaseRows), len(artifactRows))
}

func (s *server) lookupAppIDBySlug(slug string) string {
	res, err := s.db.Execute("SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1", slug)
	if err == nil && len(res.Rows) > 0 {
		return cStr(rowMaps(res)[0], "Id")
	}
	return ""
}

func (s *server) lookupReleaseID(appID, version, commitSha, environment string) string {
	res, err := s.db.Execute(
		"SELECT Id FROM sobs_app_releases FINAL WHERE AppId=? AND ReleaseVersion=? AND CommitSha=? AND Environment=? AND IsDeleted=0 LIMIT 1",
		appID, version, commitSha, environment)
	if err == nil && len(res.Rows) > 0 {
		return cStr(rowMaps(res)[0], "Id")
	}
	return ""
}

func boolToIntSeed(b bool) int {
	if b {
		return 1
	}
	return 0
}
