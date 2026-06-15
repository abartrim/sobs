package main

import (
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// GitHub work-item-links backfill — port of app.py _maybe_backfill_github_work_item_links /
// _backfill_github_work_item_links, launched fire-and-forget from view_work_items. It periodically
// refreshes open work items (issue state/title, linked PR, Copilot assignment) by hitting GitHub
// per item. It is interval-gated (300s) + guarded against concurrent runs, and token-gated: with no
// ai.github_token (the base /work-items fixture) it logs a summary and returns without any DB or
// network access, so it never perturbs parity.

const (
	githubWorkItemBackfillIntervalSec = 300 // _GITHUB_WORK_ITEM_BACKFILL_INTERVAL_SEC
	githubWorkItemBackfillMaxItems    = 25  // _GITHUB_WORK_ITEM_BACKFILL_MAX_ITEMS
	githubCopilotSWEAgentLogin        = "copilot-swe-agent"
)

// Module-level state mirroring app.py's _GITHUB_WORK_ITEM_BACKFILL_LAST_TS / _RUNNING globals: a
// single process-wide interval gate + run guard (the page handler launches one detached goroutine).
var (
	githubWorkItemBackfillMu      sync.Mutex
	githubWorkItemBackfillLastTS  float64
	githubWorkItemBackfillRunning bool
)

// maybeBackfillGithubWorkItemLinks ports app.py _maybe_backfill_github_work_item_links: bail if a
// run is already in flight or the interval has not elapsed; otherwise mark running + stamp the
// timestamp, run the backfill, and clear the running flag. Designed to be launched as a detached
// goroutine so the work-items page render is never blocked.
func (s *server) maybeBackfillGithubWorkItemLinks() {
	now := float64(nowUTC().UnixNano()) / 1e9
	githubWorkItemBackfillMu.Lock()
	if githubWorkItemBackfillRunning || now-githubWorkItemBackfillLastTS < githubWorkItemBackfillIntervalSec {
		githubWorkItemBackfillMu.Unlock()
		return
	}
	githubWorkItemBackfillRunning = true
	githubWorkItemBackfillLastTS = now
	githubWorkItemBackfillMu.Unlock()

	defer func() {
		githubWorkItemBackfillMu.Lock()
		githubWorkItemBackfillRunning = false
		githubWorkItemBackfillMu.Unlock()
	}()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("GitHub work-item backfill failed: %v", r)
		}
	}()
	s.backfillGithubWorkItemLinks()
}

// backfillGithubWorkItemLinks ports app.py _backfill_github_work_item_links: load up to N open
// work items, refresh each from GitHub (issue state/title/assignees, linked open PR, derived
// Copilot assignment status), and re-insert the changed rows. Token-gated: with no default
// ai.github_token it logs the missing-token summary and returns immediately (no DB query, no
// egress), matching app.py.
func (s *server) backfillGithubWorkItemLinks() {
	startedAt := time.Now()
	scannedCount := 0
	updatedCount := 0
	skippedCount := 0
	errorCount := 0

	defaultToken := strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	if defaultToken == "" {
		logBackfillSummary(scannedCount, updatedCount, skippedCount, errorCount, startedAt, "missing_default_token")
		return
	}

	res, err := s.db.Execute(
		"SELECT * FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND IssueUrl != '' "+
			"AND (IssueState = '' OR IssueState = 'open' OR CopilotAssignmentStatus IN ('requested','active')) "+
			"ORDER BY CreatedAt DESC LIMIT ?",
		githubWorkItemBackfillMaxItems)
	if err != nil {
		logBackfillSummary(scannedCount, updatedCount, skippedCount, errorCount, startedAt, "")
		return
	}
	rows := rowMaps(res)
	scannedCount = len(rows)
	if scannedCount == 0 {
		logBackfillSummary(scannedCount, updatedCount, skippedCount, errorCount, startedAt, "")
		return
	}

	updates := []map[string]any{}
	for _, row := range rows {
		issueURL := strings.TrimSpace(cStr(row, "IssueUrl"))
		if issueURL == "" {
			skippedCount++
			continue
		}
		owner := ""
		repo := ""
		issueNumber := 0
		if githubRepo := strings.TrimSpace(cStr(row, "GithubRepo")); githubRepo != "" {
			owner, repo = parseGithubRepoOwnerName(githubRepo)
		}
		if owner == "" || repo == "" {
			owner, repo, issueNumber = parseIssueRefFromURL(issueURL)
		}
		if issueNumber <= 0 {
			issueNumber = cInt(row, "IssueNumber")
		}
		if owner == "" || repo == "" || issueNumber <= 0 {
			skippedCount++
			continue
		}

		githubToken := s.repoScopedGithubToken(owner, repo)
		if githubToken == "" {
			githubToken = defaultToken
		}
		if githubToken == "" {
			skippedCount++
			continue
		}

		issueResp, err := s.upstreamRequest("GET",
			"https://api.github.com/repos/"+owner+"/"+repo+"/issues/"+strconv.Itoa(issueNumber),
			nil, githubAPIHeaders(githubToken, false, nil))
		if err != nil || issueResp.Status < 200 || issueResp.Status >= 300 {
			errorCount++
			skippedCount++
			continue
		}
		issuePayload, _ := issueResp.Body.(*jsonenc.Object)
		if issuePayload == nil {
			issuePayload = jsonenc.NewObject()
		}

		issueState := orFirstNonEmpty(objGetStr(issuePayload, "state"), cStr(row, "IssueState"))
		issueTitle := orFirstNonEmpty(objGetStr(issuePayload, "title"), cStr(row, "IssueTitle"))
		assignees := []string{}
		if av, ok := issuePayload.Get("assignees"); ok {
			if list, ok := av.([]any); ok {
				for _, iv := range list {
					if item, ok := iv.(*jsonenc.Object); ok {
						assignees = append(assignees, objGetStr(item, "login"))
					}
				}
			}
		}

		prInfo := s.searchOpenPRForIssue(githubToken, owner+"/"+repo, issueNumber)
		prURL := ""
		prNumber := 0
		if prInfo != nil {
			prURL = objGetStr(prInfo, "pr_url")
			prNumber = objGetInt(prInfo, "pr_number")
		}
		prLinked := prURL != ""

		nextStatus, nextReason := deriveCopilotAssignmentStatus(
			cStr(row, "CopilotAssignmentStatus"), issueState, assignees, prLinked)

		prLinkedInt := 0
		if prLinked {
			prLinkedInt = 1
		}
		changed := cStr(row, "IssueState") != issueState ||
			cStr(row, "IssueTitle") != issueTitle ||
			cInt(row, "PrLinked") != prLinkedInt ||
			cInt(row, "PrNumber") != prNumber ||
			cStr(row, "PrUrl") != prURL ||
			cStr(row, "CopilotAssignmentStatus") != nextStatus ||
			cStr(row, "CopilotAssignmentReason") != nextReason
		if !changed {
			skippedCount++
			continue
		}

		// updated = dict(row) with the refreshed fields + a fresh Version.
		updated := make(map[string]any, len(row)+1)
		for k, v := range row {
			updated[k] = v
		}
		updated["IssueState"] = issueState
		updated["IssueTitle"] = issueTitle
		updated["PrLinked"] = prLinkedInt
		updated["PrNumber"] = prNumber
		updated["PrUrl"] = prURL
		updated["CopilotAssignmentStatus"] = nextStatus
		updated["CopilotAssignmentReason"] = nextReason
		updated["Version"] = fixedVersionMillis()
		updates = append(updates, updated)
	}

	if len(updates) > 0 {
		_, _ = s.insertRowsNormalized("sobs_github_work_items", updates)
		updatedCount = len(updates)
	}
	logBackfillSummary(scannedCount, updatedCount, skippedCount, errorCount, startedAt, "")
}

// logBackfillSummary mirrors the app.logger.info("github_work_item_backfill_summary ...") emit.
func logBackfillSummary(scanned, updated, skipped, errors int, startedAt time.Time, reason string) {
	summary := jsonenc.NewObject().
		Set("scanned", scanned).
		Set("updated", updated).
		Set("skipped", skipped).
		Set("errors", errors).
		Set("duration_ms", int(time.Since(startedAt).Milliseconds())).
		Set("max_items", githubWorkItemBackfillMaxItems)
	if reason != "" {
		summary.Set("reason", reason)
	}
	log.Printf("github_work_item_backfill_summary %s", safeJSONDumps(summary))
}

// searchOpenPRForIssue ports app.py _search_open_pr_for_issue: query the GitHub search API for an
// open PR whose body references "#<issue_number>" in repo owner/repo, returning {pr_number, pr_url}
// for the first hit (or nil). Token/repo/number-gated, mirroring the Python guards.
func (s *server) searchOpenPRForIssue(githubToken, githubRepo string, issueNumber int) *jsonenc.Object {
	if githubToken == "" || githubRepo == "" || issueNumber <= 0 {
		return nil
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return nil
	}
	// q=repo:owner/repo is:pr is:open "#<n>" in:body ; per_page=1.
	q := "repo:" + owner + "/" + repo + " is:pr is:open \"#" + strconv.Itoa(issueNumber) + "\" in:body"
	reqURL := "https://api.github.com/search/issues?q=" + url.QueryEscape(q) + "&per_page=1"
	resp, err := s.upstreamRequest("GET", reqURL, nil, githubAPIHeaders(githubToken, false, nil))
	if err != nil || resp.Status < 200 || resp.Status >= 300 {
		return nil
	}
	payload, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		return nil
	}
	itemsV, ok := payload.Get("items")
	if !ok {
		return nil
	}
	items, ok := itemsV.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	item, ok := items[0].(*jsonenc.Object)
	if !ok {
		return nil
	}
	return jsonenc.NewObject().
		Set("pr_number", objGetInt(item, "number")).
		Set("pr_url", objGetStr(item, "html_url"))
}

// deriveCopilotAssignmentStatus ports app.py _derive_copilot_assignment_status: transition the
// stored Copilot assignment status from the refreshed issue state / assignees / linked-PR signal.
func deriveCopilotAssignmentStatus(currentStatus, issueState string, assignees []string, prLinked bool) (string, string) {
	normalizedCurrent := strings.ToLower(strings.TrimSpace(currentStatus))
	if normalizedCurrent == "" {
		normalizedCurrent = "not_requested"
	}
	normalizedState := strings.ToLower(strings.TrimSpace(issueState))
	copilotAssigned := false
	for _, a := range assignees {
		la := strings.ToLower(strings.TrimSpace(a))
		if la == strings.ToLower(githubCopilotAssignee) || la == githubCopilotSWEAgentLogin {
			copilotAssigned = true
			break
		}
	}

	if normalizedState == "closed" {
		if normalizedCurrent == "requested" || normalizedCurrent == "active" {
			return "completed", "issue is closed"
		}
		return normalizedCurrent, ""
	}
	if prLinked && (normalizedCurrent == "not_requested" || normalizedCurrent == "blocked") {
		return "blocked", "linked pull request already exists"
	}
	if copilotAssigned {
		return "active", "Copilot is assigned on the issue"
	}
	if normalizedCurrent == "requested" || normalizedCurrent == "active" {
		return "requested", "Copilot assignment requested"
	}
	return normalizedCurrent, ""
}

// orFirstNonEmpty returns a if non-empty, else b (the Python `str(x or y)` idiom).
func orFirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
