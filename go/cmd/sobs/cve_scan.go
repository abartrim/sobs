package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

func sha256Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

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

type lockfileCandidate struct{ path, contentType, kind string }

var cveLockfileCandidates = []lockfileCandidate{
	{"requirements.txt", "text/plain", "requirements"},
	{"package-lock.json", "application/json", "package_lock"},
	{"go.sum", "text/plain", "go_sum"},
	{"Gemfile.lock", "text/plain", "gemfile_lock"},
}

// fetchReleaseDepsFromGithub mirrors _fetch_release_deps_from_github: backfill dependencies-lockfile
// artifacts from GitHub for releases that lack them. Returns (attempted, inserted, maxReleases). No
// token -> (0, 0, cap). A release whose repo has no fetchable lockfile attempts but inserts nothing.
func (s *server) fetchReleaseDepsFromGithub() (attempted, inserted, maxReleases int) {
	token := strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	maxReleases = s.githubBackfillMaxReleases()
	if token == "" {
		return 0, 0, maxReleases
	}
	existing := map[string]bool{}
	if res, err := s.db.Execute("SELECT DISTINCT ReleaseId FROM sobs_release_artifacts FINAL " +
		"WHERE ArtifactType='dependencies-lockfile' AND IsDeleted=0"); err == nil {
		for _, r := range rowMaps(res) {
			existing[cStr(r, "ReleaseId")] = true
		}
	}
	relRes, err := s.db.Execute("SELECT Id, AppId, ReleaseVersion, CommitSha FROM sobs_app_releases FINAL " +
		"WHERE IsDeleted=0 ORDER BY ReleasedAt DESC LIMIT " + strconv.Itoa(maxReleases))
	if err != nil {
		return 0, 0, maxReleases
	}
	apps := map[string]map[string]string{}
	if ar, err := s.db.Execute("SELECT Id, RepoUrl, Enabled FROM sobs_apps FINAL WHERE IsDeleted=0"); err == nil {
		for _, r := range rowMaps(ar) {
			apps[cStr(r, "Id")] = map[string]string{"repo_url": strings.TrimSpace(cStr(r, "RepoUrl")), "enabled": cStr(r, "Enabled")}
		}
	}
	insertedRows := []map[string]any{}
	for _, row := range rowMaps(relRes) {
		releaseID := cStr(row, "Id")
		releaseVersion := strings.TrimSpace(cStr(row, "ReleaseVersion"))
		commitSha := strings.TrimSpace(cStr(row, "CommitSha"))
		app := apps[cStr(row, "AppId")]
		repoURL := app["repo_url"]
		if releaseID == "" || releaseVersion == "" || existing[releaseID] {
			continue
		}
		if app["enabled"] == "0" || app["enabled"] == "" || repoURL == "" {
			continue
		}
		owner, repo := parseGithubRepoOwnerName(repoURL)
		if owner == "" || repo == "" {
			continue
		}
		attempted++
		if rows := s.githubActionsDependencyRows(commitSha); len(rows) > 0 {
			insertedRows = append(insertedRows, rows...)
			existing[releaseID] = true
			inserted += len(rows)
			continue
		}
		if rows := s.githubContentsLockfileRows(owner, repo, releaseID, releaseVersion); len(rows) > 0 {
			insertedRows = append(insertedRows, rows...)
			existing[releaseID] = true
			inserted += len(rows)
		}
	}
	if len(insertedRows) > 0 {
		if _, err := s.insertRowsNormalized("sobs_release_artifacts", insertedRows); err != nil {
			inserted = 0
		}
	}
	return attempted, inserted, maxReleases
}

// githubActionsDependencyRows mirrors _github_actions_dependency_rows. Without a commit identity (and
// for the parity release, which has none) it returns nil, so the contents-API fallback is used; the
// full GH-Actions-snapshot artifact path is a follow-up.
func (s *server) githubActionsDependencyRows(commitSha string) []map[string]any {
	if strings.TrimSpace(commitSha) == "" {
		return nil
	}
	return nil
}

// githubContentsLockfileRows tries each (ref, lockfile) via the GitHub Contents API, parsing the
// first lockfile found into a dependencies artifact row (mirrors the contents loop in
// _fetch_release_deps_from_github). A repo with no lockfile yields no rows (every fetch 404s).
func (s *server) githubContentsLockfileRows(owner, repo, releaseID, releaseVersion string) []map[string]any {
	for _, ref := range githubRefCandidates(releaseVersion) {
		for _, cand := range cveLockfileCandidates {
			url := "https://api.github.com/repos/" + owner + "/" + repo + "/contents/" + cand.path
			resp, err := s.upstreamGet("GET", url)
			if err != nil || resp.Status == 404 {
				continue
			}
			if resp.Status != 200 {
				break
			}
			body, ok := resp.Body.(*jsonenc.Object)
			if !ok {
				continue
			}
			raw := decodeGithubContentsPayload(body)
			if len(raw) == 0 {
				continue
			}
			deps := parseLockfileDependencies(cand.kind, string(raw))
			if len(deps) == 0 {
				continue
			}
			sum := sha256Sum(raw)
			meta := jsonenc.NewObject().Set("source", "github_contents_api").
				Set("repo", owner+"/"+repo).Set("ref", ref).Set("path", cand.path).Set("dependencies", deps)
			return []map[string]any{{
				"Id": newUUIDv4(), "ReleaseId": releaseID, "ArtifactType": "dependencies-lockfile",
				"Name": cand.path, "ContentType": cand.contentType, "Size": len(raw),
				"StorageRef":     "github://" + owner + "/" + repo + "/" + cand.path + "?ref=" + ref,
				"ChecksumSha256": sum, "Platform": "", "Architecture": "",
				"MetadataJson": string(jsonenc.Encode(meta, jsonenc.Options{SortKeys: false})),
				"UploadedAt":   normalizeCHTimestampNow(), "IsDeleted": 0, "Version": fixedVersionMillis(),
			}}
		}
	}
	return nil
}

// githubRefCandidates mirrors _github_ref_candidates.
func githubRefCandidates(releaseVersion string) []string {
	v := strings.TrimSpace(releaseVersion)
	if v == "" {
		return nil
	}
	cands := []string{"refs/tags/" + v}
	if !strings.HasPrefix(v, "v") {
		cands = append(cands, "refs/tags/v"+v)
	}
	cands = append(cands, "refs/heads/"+v, v)
	seen := map[string]bool{}
	out := []string{}
	for _, c := range cands {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// decodeGithubContentsPayload mirrors _decode_github_contents_payload (base64 content field).
func decodeGithubContentsPayload(body *jsonenc.Object) []byte {
	content := objGetStr(body, "content")
	if content == "" || strings.ToLower(objGetStr(body, "encoding")) != "base64" {
		return nil
	}
	dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil {
		return nil
	}
	return dec
}

// parseLockfileDependencies dispatches to the per-ecosystem lockfile parser.
func parseLockfileDependencies(kind, content string) []any {
	switch kind {
	case "requirements":
		return parseRequirementsDeps(content)
	case "package_lock":
		return parsePackageLockDeps(content)
	case "go_sum":
		return parseGoSumDeps(content)
	case "gemfile_lock":
		return parseGemfileLockDeps(content)
	}
	return nil
}

func depObj(pkg, version, ecosystem string) *jsonenc.Object {
	return jsonenc.NewObject().Set("package", pkg).Set("version", version).Set("ecosystem", ecosystem)
}

func parseRequirementsDeps(content string) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		line = strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		if !strings.Contains(line, "==") {
			continue
		}
		parts := strings.SplitN(line, "==", 2)
		pkg, ver := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if pkg == "" || ver == "" {
			continue
		}
		key := strings.ToLower(pkg) + "==" + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, depObj(pkg, ver, "PyPI"))
	}
	return out
}

func parseGoSumDeps(content string) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 {
			continue
		}
		name, ver := fields[0], fields[1]
		ver = strings.TrimSuffix(ver, "/go.mod")
		if name == "" || ver == "" {
			continue
		}
		key := strings.ToLower(name) + " " + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, depObj(name, ver, "Go"))
	}
	return out
}

var gemfileLockRE = regexp.MustCompile(`^([A-Za-z0-9_\-.]+)\s+\(([^)]+)\)`)

func parseGemfileLockDeps(content string) []any {
	out := []any{}
	seen := map[string]bool{}
	inSpecs := false
	for _, raw := range strings.Split(content, "\n") {
		if strings.TrimSpace(raw) == "specs:" {
			inSpecs = true
			continue
		}
		if !inSpecs {
			continue
		}
		if raw != "" && !strings.HasPrefix(raw, " ") {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := gemfileLockRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := strings.TrimSpace(m[1])
		ver := strings.TrimSpace(strings.SplitN(m[2], ",", 2)[0])
		if name == "" || ver == "" {
			continue
		}
		key := strings.ToLower(name) + " " + ver
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, depObj(name, ver, "RubyGems"))
	}
	return out
}

func parsePackageLockDeps(content string) []any {
	out := []any{}
	seen := map[string]bool{}
	parsed, err := parseJSONValue([]byte(content))
	if err != nil {
		return out
	}
	body, ok := parsed.(*jsonenc.Object)
	if !ok {
		return out
	}
	addDep := func(name, ver string) {
		if name == "" || ver == "" {
			return
		}
		key := strings.ToLower(name) + " " + ver
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, depObj(name, ver, "npm"))
	}
	if pv, ok := body.Get("packages"); ok {
		if packages, ok := pv.(*jsonenc.Object); ok {
			for _, pkgPath := range packages.Keys() {
				if pkgPath == "" || pkgPath == "." || !strings.HasPrefix(pkgPath, "node_modules/") {
					continue
				}
				info, _ := packages.Get(pkgPath)
				io, ok := info.(*jsonenc.Object)
				if !ok {
					continue
				}
				idx := strings.LastIndex(pkgPath, "node_modules/")
				addDep(pkgPath[idx+len("node_modules/"):], strings.TrimSpace(objGetStr(io, "version")))
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	if lv, ok := body.Get("dependencies"); ok {
		if legacy, ok := lv.(*jsonenc.Object); ok {
			for _, name := range legacy.Keys() {
				info, _ := legacy.Get(name)
				if io, ok := info.(*jsonenc.Object); ok {
					addDep(name, strings.TrimSpace(objGetStr(io, "version")))
				}
			}
		}
	}
	return out
}

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
