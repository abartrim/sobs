package main

import (
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Periodic enrichment workers — ports of app.py's _cve_scanner_loop / _github_repo_health_loop
// (started at @app.before_serving) plus their per-iteration scan bodies. They run ONLY in real
// runtime: the parity capture harness drives app.test_client() without the lifespan, so the
// loops never fire during capture. background_tasks.go startBackgroundWorkers gates the Go
// goroutines on !Parity so the parity replay is byte-equivalent to the captured oracle.
//
// The scan bodies themselves are additionally token-gated: with no ai.github_token (the base
// fixture) the GitHub egress is skipped, so the repo-health summary stays the all-zero object the
// current GET already returns and the CVE github backfill stays the 0/0/cap no-op.

const (
	cveScanInitialDelayS          = 30                                           // _CVE_SCAN_INITIAL_DELAY_S
	cveScanIntervalS              = 86400                                        // _CVE_SCAN_INTERVAL_S (24h)
	githubRepoHealthInitialDelayS = 45                                           // _GITHUB_REPO_HEALTH_INITIAL_DELAY_S
	githubRepoHealthIntervalS     = 3600                                         // _GITHUB_REPO_HEALTH_INTERVAL_S (1h)
	githubRepoHealthMaxRepos      = 25                                           // _GITHUB_REPO_HEALTH_MAX_REPOS
	githubRepoHealthMaxItems      = 100                                          // _GITHUB_REPO_HEALTH_MAX_ITEMS_PER_REPO
	githubRepoHealthLastSyncKey   = "enrichment.github_repo_health_last_sync"    // _GITHUB_REPO_HEALTH_LAST_SYNC_SETTING
	githubRepoHealthLastSummary   = "enrichment.github_repo_health_last_summary" // _GITHUB_REPO_HEALTH_LAST_SUMMARY_SETTING
	githubItemSecurityKeywordList = "security,vulnerability,cve,ghsa,dependabot" // _github_item_is_security_related keywords
)

// ---------------------------------------------------------------------------
// CVE scanner loop
// ---------------------------------------------------------------------------

// cveScannerLoop is a port of app.py _cve_scanner_loop: wait the initial delay, then run the CVE
// scan and sleep the interval forever. Real-runtime only (gated off under parity).
func (s *server) cveScannerLoop() {
	time.Sleep(cveScanInitialDelayS * time.Second)
	for {
		func() {
			defer func() { _ = recover() }()
			summary := s.runCveScan()
			if okVal, _ := summary.Get("ok"); okVal == true {
				if vf := objGetInt(summary, "vulns_found"); vf > 0 {
					log.Printf("CVE scan complete: %d libraries, %d vulnerabilities found",
						objGetInt(summary, "libraries_found"), vf)
				}
			}
		}()
		time.Sleep(cveScanIntervalS * time.Second)
	}
}

// ---------------------------------------------------------------------------
// GitHub repo-health loop + scan body + sync-once persistence
// ---------------------------------------------------------------------------

// githubRepoHealthLoop is a port of app.py _github_repo_health_loop: wait the initial delay, then
// sync repo health once and sleep the interval forever. Real-runtime only (gated off under parity).
func (s *server) githubRepoHealthLoop() {
	time.Sleep(githubRepoHealthInitialDelayS * time.Second)
	for {
		func() {
			defer func() { _ = recover() }()
			s.syncGithubRepoHealthOnce()
		}()
		time.Sleep(githubRepoHealthIntervalS * time.Second)
	}
}

// syncGithubRepoHealthOnce is a port of app.py _sync_github_repo_health_once: collect the
// repo-health summary and, when ok, persist the last-sync timestamp + a compact summary setting,
// skipping the write when the compact counts are unchanged (change-dedup against the stored value).
func (s *server) syncGithubRepoHealthOnce() *jsonenc.Object {
	summary := s.collectGithubRepoHealthSummary()
	if okVal, _ := summary.Get("ok"); okVal != true {
		return summary
	}

	compactValues := map[string]int{
		"scanned_repos":          objGetInt(summary, "scanned_repos"),
		"total_repos_considered": objGetInt(summary, "total_repos_considered"),
		"open_issues":            objGetInt(summary, "open_issues"),
		"open_prs":               objGetInt(summary, "open_prs"),
		"security_items":         objGetInt(summary, "security_items"),
	}

	if previousRaw, _ := s.appSetting(githubRepoHealthLastSummary); strings.TrimSpace(previousRaw) != "" {
		previousValues := map[string]int{}
		if parsed, err := parseJSONValue([]byte(previousRaw)); err == nil {
			if obj, ok := parsed.(*jsonenc.Object); ok {
				for _, k := range []string{"scanned_repos", "total_repos_considered", "open_issues", "open_prs", "security_items"} {
					previousValues[k] = objGetInt(obj, k)
				}
			}
		}
		if mapsEqualInt(previousValues, compactValues) {
			return summary
		}
	}

	lastSynced := objGetStr(summary, "last_synced_at")
	_ = s.setAppSetting(githubRepoHealthLastSyncKey, lastSynced)

	// Compact summary: the five counts + last_synced_at, in app.py's dict order with compact
	// separators (json.dumps(..., separators=(",", ":"))).
	compact := jsonenc.NewObject().
		Set("scanned_repos", compactValues["scanned_repos"]).
		Set("total_repos_considered", compactValues["total_repos_considered"]).
		Set("open_issues", compactValues["open_issues"]).
		Set("open_prs", compactValues["open_prs"]).
		Set("security_items", compactValues["security_items"]).
		Set("last_synced_at", lastSynced)
	_ = s.setAppSetting(githubRepoHealthLastSummary, string(jsonenc.Encode(compact, repoHealthCompactOpts)))
	return summary
}

// repoHealthCompactOpts mirrors app.py json.dumps(..., separators=(",", ":")): compact separators,
// insertion order, ensure_ascii default (True).
var repoHealthCompactOpts = jsonenc.Options{SortKeys: false, EnsureASCII: true, ItemSep: ",", KeySep: ":"}

// repoHealthTarget mirrors the per-app repo target built by _collect_github_repo_health_summary.
type repoHealthTarget struct {
	appName, owner, repo string
	versions             []string
}

// collectGithubRepoHealthSummary is a port of app.py _collect_github_repo_health_summary: build a
// repo target per enabled app with a parseable GitHub RepoUrl + at least one release version, then
// (token-gated) hit GitHub /issues?state=open per repo, version-scope each issue/PR, count
// issues/PRs/security items, and assemble the sorted summary. Returns the ok:true summary object
// (or ok:false + error on a DB failure). Without a configured token every repo is skipped, so
// scanned_repos/repos stay empty and the all-zero summary is returned (the base-fixture path).
func (s *server) collectGithubRepoHealthSummary() *jsonenc.Object {
	defaultToken := strings.TrimSpace(s.loadAISetting("ai.github_token", ""))

	appRes, err := s.db.Execute("SELECT Id, Name, Slug, RepoUrl FROM sobs_apps FINAL " +
		"WHERE IsDeleted=0 AND Enabled=1 AND RepoUrl != '' ORDER BY Name ASC")
	if err != nil {
		return jsonenc.NewObject().Set("ok", false).Set("error", err.Error())
	}
	relRes, err := s.db.Execute("SELECT AppId, ReleaseVersion FROM sobs_app_releases FINAL " +
		"WHERE IsDeleted=0 ORDER BY ReleasedAt DESC LIMIT 4000")
	if err != nil {
		return jsonenc.NewObject().Set("ok", false).Set("error", err.Error())
	}

	// versions_by_app: first 5 distinct ReleaseVersion per app, in ReleasedAt-DESC order.
	versionsByApp := map[string][]string{}
	for _, m := range rowMaps(relRes) {
		appID := cStr(m, "AppId")
		relVer := strings.TrimSpace(cStr(m, "ReleaseVersion"))
		if appID == "" || relVer == "" {
			continue
		}
		vs := versionsByApp[appID]
		if len(vs) < 5 && !containsStr(vs, relVer) {
			versionsByApp[appID] = append(vs, relVer)
		}
	}

	var targets []repoHealthTarget
	for _, m := range rowMaps(appRes) {
		appID := cStr(m, "Id")
		appName := cStr(m, "Name")
		if appName == "" {
			appName = cStr(m, "Slug")
		}
		owner, repo := parseGithubRepoOwnerName(cStr(m, "RepoUrl"))
		versions := versionsByApp[appID]
		if owner == "" || repo == "" || len(versions) == 0 {
			continue
		}
		targets = append(targets, repoHealthTarget{appName: appName, owner: owner, repo: repo, versions: versions})
	}
	if len(targets) > githubRepoHealthMaxRepos {
		targets = targets[:githubRepoHealthMaxRepos]
	}

	totalOpenIssues := 0
	totalOpenPRs := 0
	totalSecurityItems := 0
	scannedRepos := 0
	reposSummary := []*jsonenc.Object{}

	for _, t := range targets {
		token := s.repoScopedGithubToken(t.owner, t.repo)
		if token == "" {
			token = defaultToken
		}
		if token == "" {
			continue
		}
		// version_tokens: union of _github_version_tokens over each non-blank version.
		versions := []string{}
		versionTokens := map[string]bool{}
		for _, v := range t.versions {
			if strings.TrimSpace(v) == "" {
				continue
			}
			versions = append(versions, v)
			for tok := range githubVersionTokens(v) {
				versionTokens[tok] = true
			}
		}
		if len(versionTokens) == 0 {
			continue
		}

		scannedRepos++
		reqURL := "https://api.github.com/repos/" + t.owner + "/" + t.repo +
			"/issues?state=open&per_page=" + strconv.Itoa(githubRepoHealthMaxItems)
		resp, err := s.upstreamRequest("GET", reqURL, nil, githubAPIHeaders(token, false, nil))
		if err != nil || resp.Status != 200 {
			continue
		}
		items, ok := resp.Body.([]any)
		if !ok {
			continue
		}

		repoIssues := 0
		repoPRs := 0
		repoSecurity := 0
		for _, itemV := range items {
			item, ok := itemV.(*jsonenc.Object)
			if !ok {
				continue
			}
			text := objGetStr(item, "title") + "\n" + objGetStr(item, "body")
			if !textMentionsVersionTokens(text, versionTokens) {
				continue
			}
			isPR := false
			if pr, ok := item.Get("pull_request"); ok {
				if _, ok := pr.(*jsonenc.Object); ok {
					isPR = true
				}
			}
			if isPR {
				repoPRs++
			} else {
				repoIssues++
			}
			if githubItemIsSecurityRelated(item) {
				repoSecurity++
			}
		}

		totalOpenIssues += repoIssues
		totalOpenPRs += repoPRs
		totalSecurityItems += repoSecurity
		reposSummary = append(reposSummary, jsonenc.NewObject().
			Set("repo", t.owner+"/"+t.repo).
			Set("app_name", t.appName).
			Set("versions", stringsToAny(versions)).
			Set("open_issues", repoIssues).
			Set("open_prs", repoPRs).
			Set("security_items", repoSecurity))
	}

	// Sort by descending (security+issues+prs), then by repo name (case-insensitive) ascending.
	sort.SliceStable(reposSummary, func(i, j int) bool {
		si := objGetInt(reposSummary[i], "security_items") + objGetInt(reposSummary[i], "open_issues") + objGetInt(reposSummary[i], "open_prs")
		sj := objGetInt(reposSummary[j], "security_items") + objGetInt(reposSummary[j], "open_issues") + objGetInt(reposSummary[j], "open_prs")
		if si != sj {
			return si > sj
		}
		return strings.ToLower(objGetStr(reposSummary[i], "repo")) < strings.ToLower(objGetStr(reposSummary[j], "repo"))
	})

	reposAny := make([]any, 0, len(reposSummary))
	for _, r := range reposSummary {
		reposAny = append(reposAny, r)
	}

	return jsonenc.NewObject().
		Set("ok", true).
		Set("scanned_repos", scannedRepos).
		Set("total_repos_considered", len(targets)).
		Set("open_issues", totalOpenIssues).
		Set("open_prs", totalOpenPRs).
		Set("security_items", totalSecurityItems).
		Set("version_scoped", true).
		Set("last_synced_at", nowISO()).
		Set("repos", reposAny)
}

// textMentionsVersionTokens ports app.py _text_mentions_version_tokens: lowercase the text and
// return true if any token appears delimited by non-[0-9a-z] boundaries (start/end count). Empty
// text or token set yields false.
func textMentionsVersionTokens(text string, tokens map[string]bool) bool {
	if text == "" || len(tokens) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for token := range tokens {
		re := regexp.MustCompile(`(^|[^0-9a-z])` + regexp.QuoteMeta(token) + `([^0-9a-z]|$)`)
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}

// githubItemIsSecurityRelated ports app.py _github_item_is_security_related: a GitHub issue/PR is
// security-related if any of the keywords appears in its (lowercased) title, body, or any label
// name.
func githubItemIsSecurityRelated(item *jsonenc.Object) bool {
	keywords := strings.Split(githubItemSecurityKeywordList, ",")
	title := strings.ToLower(objGetStr(item, "title"))
	body := strings.ToLower(objGetStr(item, "body"))
	for _, k := range keywords {
		if strings.Contains(title, k) || strings.Contains(body, k) {
			return true
		}
	}
	if labelsV, ok := item.Get("labels"); ok {
		if labels, ok := labelsV.([]any); ok {
			for _, lv := range labels {
				label, ok := lv.(*jsonenc.Object)
				if !ok {
					continue
				}
				name := strings.ToLower(objGetStr(label, "name"))
				for _, k := range keywords {
					if strings.Contains(name, k) {
						return true
					}
				}
			}
		}
	}
	return false
}

// objGetInt reads a numeric (json.Number) or int field from an object, defaulting to 0.
func objGetInt(o *jsonenc.Object, key string) int {
	v, ok := o.Get(key)
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	default:
		if n, err := strconv.Atoi(strings.TrimSpace(toStr(v))); err == nil {
			return n
		}
	}
	return 0
}

// stringsToAny lifts a []string into []any (for jsonenc list fields).
func stringsToAny(xs []string) []any {
	out := make([]any, 0, len(xs))
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}

// mapsEqualInt reports whether two int maps have identical key/value sets.
func mapsEqualInt(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
