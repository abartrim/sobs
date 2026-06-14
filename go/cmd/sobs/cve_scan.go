package main

import (
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// cveLib is one collected library inventory item (mirrors the dicts from _collect_library_inventory).
type cveLib struct {
	pkg, version, ecosystem, service, source, appName, releaseVersion, environment string
}

func langToOSVEcosystem(lang string) string {
	switch strings.ToLower(lang) {
	case "python":
		return "PyPI"
	case "javascript", "nodejs":
		return "npm"
	case "java":
		return "Maven"
	case "go":
		return "Go"
	case "ruby":
		return "RubyGems"
	case "dotnet":
		return "NuGet"
	case "rust":
		return "crates.io"
	case "php":
		return "Packagist"
	case "dart":
		return "Pub"
	}
	return ""
}

func inventoryScopeEcosystem(scopeName string) string {
	if strings.HasPrefix(scopeName, "io.opentelemetry") || strings.HasPrefix(scopeName, "com.") || strings.HasPrefix(scopeName, "org.") {
		return "Maven"
	}
	if strings.HasPrefix(scopeName, "@") {
		return "npm"
	}
	if strings.HasPrefix(scopeName, "opentelemetry-") {
		last := scopeName
		if i := strings.LastIndex(scopeName, "/"); i >= 0 {
			last = scopeName[i+1:]
		}
		if !strings.Contains(last, "_") {
			return "PyPI"
		}
	}
	return ""
}

var cveSourcePriority = map[string]int{"release_registry": 0, "otel_sdk": 1, "otel_scope": 2}

// collectLibraryInventory mirrors _collect_library_inventory: dedup libraries from release-registry
// lockfile artifacts (tier 1), telemetry.sdk.* resource attrs (tier 2), and ScopeName/Version (tier 3).
func (s *server) collectLibraryInventory() []cveLib {
	inv := map[string]cveLib{}
	order := []string{}
	add := func(it cveLib) {
		it.pkg = strings.TrimSpace(it.pkg)
		it.version = strings.TrimSpace(it.version)
		if it.pkg == "" || it.version == "" {
			return
		}
		it.ecosystem = strings.TrimSpace(it.ecosystem)
		it.service = strings.TrimSpace(it.service)
		key := it.ecosystem + "::" + it.pkg + "::" + it.version + "::" + cveServiceLabel(it)
		cur, ok := inv[key]
		if !ok {
			inv[key] = it
			order = append(order, key)
			return
		}
		if cveSourcePriority[it.source] < cveSourcePriorityOr(cur.source) {
			inv[key] = it
		}
	}

	// Tier 1: dependencies-lockfile artifacts registered via CI/release metadata.
	if artRes, err := s.db.Execute(
		"SELECT ReleaseId, Name, MetadataJson FROM sobs_release_artifacts FINAL " +
			"WHERE ArtifactType='dependencies-lockfile' AND IsDeleted=0 ORDER BY UploadedAt DESC LIMIT 500"); err == nil {
		releasesByID := map[string]map[string]string{}
		if rr, err := s.db.Execute("SELECT Id, AppId, ReleaseVersion, Environment FROM sobs_app_releases FINAL WHERE IsDeleted=0"); err == nil {
			for _, r := range rowMaps(rr) {
				releasesByID[cStr(r, "Id")] = map[string]string{
					"app_id": cStr(r, "AppId"), "release_version": cStr(r, "ReleaseVersion"), "environment": cStr(r, "Environment"),
				}
			}
		}
		appsByID := map[string]map[string]string{}
		if ar, err := s.db.Execute("SELECT Id, Name, Slug FROM sobs_apps FINAL WHERE IsDeleted=0"); err == nil {
			for _, r := range rowMaps(ar) {
				appsByID[cStr(r, "Id")] = map[string]string{"name": cStr(r, "Name"), "slug": cStr(r, "Slug")}
			}
		}
		for _, row := range rowMaps(artRes) {
			rel := releasesByID[cStr(row, "ReleaseId")]
			app := appsByID[rel["app_id"]]
			appName := app["name"]
			if appName == "" {
				appName = app["slug"]
			}
			deps := cveMetadataDependencies(cStr(row, "MetadataJson"))
			for _, dep := range deps {
				pkg := objGetStr(dep, "package")
				if _, has := dep.Get("package"); !has {
					pkg = objGetStr(dep, "name")
				}
				add(cveLib{
					pkg:            pkg,
					version:        objGetStr(dep, "version"),
					ecosystem:      objGetStr(dep, "ecosystem"),
					service:        appName,
					source:         "release_registry",
					appName:        appName,
					releaseVersion: rel["release_version"],
					environment:    rel["environment"],
				})
			}
		}
	}

	// Tier 2: telemetry.sdk.* from traces then logs.
	for _, table := range []string{"otel_traces", "otel_logs"} {
		if res, err := s.db.Execute(
			"SELECT ResourceAttributes['telemetry.sdk.name'] AS sdk_name, " +
				"ResourceAttributes['telemetry.sdk.version'] AS sdk_version, " +
				"ResourceAttributes['telemetry.sdk.language'] AS sdk_lang, ServiceName " +
				"FROM " + table + " WHERE ResourceAttributes['telemetry.sdk.version'] != '' " +
				"GROUP BY sdk_name, sdk_version, sdk_lang, ServiceName LIMIT 200"); err == nil {
			for _, r := range rowMaps(res) {
				add(cveLib{pkg: cStr(r, "sdk_name"), version: cStr(r, "sdk_version"),
					ecosystem: langToOSVEcosystem(cStr(r, "sdk_lang")), service: cStr(r, "ServiceName"), source: "otel_sdk"})
			}
		}
	}

	// Tier 3: instrumentation library versions via ScopeName/ScopeVersion from traces then logs.
	for _, table := range []string{"otel_traces", "otel_logs"} {
		if res, err := s.db.Execute(
			"SELECT ScopeName, ScopeVersion, ServiceName FROM " + table +
				" WHERE ScopeVersion != '' AND ScopeName != '' GROUP BY ScopeName, ScopeVersion, ServiceName LIMIT 300"); err == nil {
			for _, r := range rowMaps(res) {
				scope := cStr(r, "ScopeName")
				add(cveLib{pkg: scope, version: cStr(r, "ScopeVersion"),
					ecosystem: inventoryScopeEcosystem(scope), service: cStr(r, "ServiceName"), source: "otel_scope"})
			}
		}
	}

	out := make([]cveLib, 0, len(order))
	for _, k := range order {
		out = append(out, inv[k])
	}
	return out
}

func cveServiceLabel(it cveLib) string {
	if it.service != "" {
		return it.service
	}
	return it.appName
}

func cveSourcePriorityOr(source string) int {
	if p, ok := cveSourcePriority[source]; ok {
		return p
	}
	return 99
}

// cveMetadataDependencies parses MetadataJson.dependencies (a list of {package/name, version, ecosystem}).
func cveMetadataDependencies(metadataJSON string) []*jsonenc.Object {
	out := []*jsonenc.Object{}
	parsed, err := parseJSONValue([]byte(metadataJSON))
	if err != nil {
		return out
	}
	o, ok := parsed.(*jsonenc.Object)
	if !ok {
		return out
	}
	depsV, ok := o.Get("dependencies")
	if !ok {
		return out
	}
	deps, ok := depsV.([]any)
	if !ok {
		return out
	}
	for _, d := range deps {
		if do, ok := d.(*jsonenc.Object); ok {
			out = append(out, do)
		}
	}
	return out
}

// runCveOSVScan mirrors the OSV scan loop in _run_cve_scan: query OSV.dev for each library and
// record findings. Returns (libraries_found, vulns_found). The findings rows (uuid-less, ScannedAt
// wall-clock) are inserted but never returned, so only the counts affect the response.
func (s *server) runCveOSVScan(scanTS string, libraries []cveLib) (int, int) {
	findings := []map[string]any{}
	newCount := 0
	for _, lib := range libraries {
		if lib.pkg == "" || lib.ecosystem == "" {
			continue
		}
		resp, err := s.upstreamGet("POST", "https://api.osv.dev/v1/query")
		if err != nil || resp.Status != 200 {
			continue
		}
		body, _ := resp.Body.(*jsonenc.Object)
		if body == nil {
			continue
		}
		vulnsV, _ := body.Get("vulns")
		vulns, _ := vulnsV.([]any)
		for i, vv := range vulns {
			if i >= 10 { // _CVE_MAX_VULNS_PER_PKG
				break
			}
			v, ok := vv.(*jsonenc.Object)
			if !ok {
				continue
			}
			findings = append(findings, map[string]any{
				"Package": lib.pkg, "Ecosystem": lib.ecosystem, "Version": lib.version,
				"ServiceName": lib.service, "OsvId": objGetStr(v, "id"),
				"CveIds": strings.Join(cveAliasIDs(v), ","), "Summary": truncate(objGetStr(v, "summary"), 500),
				"Severity": osvSeverity(v), "Published": truncate(objGetStr(v, "published"), 10),
				"ScannedAt": scanTS,
			})
			newCount++
		}
	}
	if len(findings) > 0 {
		_, _ = s.insertRowsNormalized("sobs_cve_findings", findings)
	}
	_ = s.setAppSetting("enrichment.cve_last_scan", scanTS)
	return len(libraries), newCount
}

// cveAliasIDs returns the CVE-prefixed aliases of an OSV vuln.
func cveAliasIDs(v *jsonenc.Object) []string {
	out := []string{}
	if av, ok := v.Get("aliases"); ok {
		if aliases, ok := av.([]any); ok {
			for _, a := range aliases {
				if str, ok := a.(string); ok && strings.HasPrefix(str, "CVE-") {
					out = append(out, str)
				}
			}
		}
	}
	return out
}

// osvSeverity mirrors the severity extraction (first severity score/type, else database_specific).
func osvSeverity(v *jsonenc.Object) string {
	if sv, ok := v.Get("severity"); ok {
		if sevs, ok := sv.([]any); ok && len(sevs) > 0 {
			if s0, ok := sevs[0].(*jsonenc.Object); ok {
				if sc := objGetStr(s0, "score"); sc != "" {
					return sc
				}
				if t := objGetStr(s0, "type"); t != "" {
					return t
				}
			}
		}
	}
	if dbV, ok := v.Get("database_specific"); ok {
		if db, ok := dbV.(*jsonenc.Object); ok {
			if sev := objGetStr(db, "severity"); sev != "" {
				return sev
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
