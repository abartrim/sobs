package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// CVE-scan GitHub-Actions backfill constants (app.py). The snapshot artifact name and the per-release
// run cap mirror _GITHUB_ACTIONS_SNAPSHOT_ARTIFACT_NAME / _GITHUB_ACTIONS_BACKFILL_MAX_RUNS_PER_RELEASE.
const (
	githubActionsSnapshotArtifactName = "sobs-release-dependency-snapshots"
	githubActionsBackfillMaxRuns      = 20
)

// cveCompactDumpsOpts mirrors json.dumps(d, separators=(",", ":")) — compact separators with
// ensure_ascii=True (the json.dumps default), insertion order. Used for the MetadataJson columns the
// GitHub-backfill artifact rows persist (app.py uses separators=(",", ":") at both the actions and
// contents sites).
var cveCompactDumpsOpts = jsonenc.Options{SortKeys: false, EnsureASCII: true, ItemSep: ",", KeySep: ":"}

// pyStrOr mirrors str(value or "") for a parsed-JSON value: a falsy value (None/0/""/False) yields
// "", otherwise the Python str() rendering (json.Number -> its digits).
func pyStrOr(v any, present bool) string {
	if !present || !isTruthyVal(v, present) {
		return ""
	}
	return pyStrAny(v)
}

// base64AlphabetRE matches every character that is NOT in the standard base64 alphabet (incl. padding).
var base64AlphabetRE = regexp.MustCompile(`[^A-Za-z0-9+/=]`)

// decodeBase64Lenient mirrors Python base64.b64decode(content, validate=False): non-alphabet
// characters (newlines, carriage returns, spaces, tabs, etc.) are discarded before decoding. Returns
// nil on a decode error. GitHub's Contents API wraps base64 at column 60 with "\n"; this also tolerates
// "\r\n" and stray whitespace exactly as the Python oracle does.
func decodeBase64Lenient(content string) []byte {
	clean := base64AlphabetRE.ReplaceAllString(content, "")
	dec, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil
	}
	return dec
}

// githubActionsSnapshotName mirrors _github_actions_snapshot_name: parse a "pip-freeze-<platform>-
// <arch>.txt" basename (case-insensitive) into (dep_name, platform, architecture), lowercasing the
// platform/arch. Returns ok=false when the basename does not match.
var githubActionsSnapshotRE = regexp.MustCompile(`^pip-freeze-([a-z0-9_-]+)-([a-z0-9_-]+)\.txt$`)

func githubActionsSnapshotName(filename string) (depName, platform, architecture string, ok bool) {
	base := path.Base(strings.TrimSpace(filename))
	if base == "" || base == "." || base == "/" {
		return "", "", "", false
	}
	m := githubActionsSnapshotRE.FindStringSubmatch(strings.ToLower(base))
	if m == nil {
		return "", "", "", false
	}
	platform = m[1]
	architecture = m[2]
	return "pip-freeze-" + platform + "-" + architecture, platform, architecture, true
}

// githubActionsDependencyRows mirrors app.py _github_actions_dependency_rows: locate the successful
// GH-Actions workflow run for a release's commit, find its dependency-snapshot artifact, download +
// unzip it, parse each pip-freeze-* entry into PyPI deps, and emit dependencies-lockfile artifact rows
// (source "github_actions_artifact"). Returns nil without a commit identity, on any HTTP/zip error, or
// when no snapshot is found, so the caller falls through to the Contents-API path. Under parity the
// empty corpus carries no GitHub token, so this branch is never reached and stays byte-identical.
func (s *server) githubActionsDependencyRows(token, owner, repo, releaseID, releaseVersion, commitSha string) []map[string]any {
	commit := strings.TrimSpace(commitSha)
	if commit == "" {
		// Without commit identity we cannot safely bind a workflow run to this release.
		return nil
	}

	// params={"status":"completed","per_page":str(max_runs),"head_sha":commit} in insertion order
	// (httpx renders them in that order, so the fixture key matches the Python oracle's URL).
	runsURL := "https://api.github.com/repos/" + owner + "/" + repo + "/actions/runs" +
		"?status=completed&per_page=" + itoa(githubActionsBackfillMaxRuns) +
		"&head_sha=" + url.QueryEscape(commit)
	runsResp, err := s.upstreamRequest("GET", runsURL, nil, githubAPIHeaders(token, false, nil))
	if err != nil || runsResp.Status != 200 {
		return nil
	}
	runsBody, _ := runsResp.Body.(*jsonenc.Object)
	if runsBody == nil {
		return nil
	}
	wrV, _ := runsBody.Get("workflow_runs")
	workflowRuns, _ := wrV.([]any)
	if workflowRuns == nil {
		return nil
	}

	for _, rv := range workflowRuns {
		run, ok := rv.(*jsonenc.Object)
		if !ok {
			continue
		}
		conclV, conclOK := run.Get("conclusion")
		if strings.ToLower(pyStrOr(conclV, conclOK)) != "success" {
			continue
		}
		idV, idOK := run.Get("id")
		runID := strings.TrimSpace(pyStrOr(idV, idOK))
		if runID == "" {
			continue
		}

		artURL := "https://api.github.com/repos/" + owner + "/" + repo + "/actions/runs/" + runID +
			"/artifacts?per_page=100"
		artResp, err := s.upstreamRequest("GET", artURL, nil, githubAPIHeaders(token, false, nil))
		if err != nil || artResp.Status != 200 {
			continue
		}
		artBody, _ := artResp.Body.(*jsonenc.Object)
		if artBody == nil {
			continue
		}
		artsV, _ := artBody.Get("artifacts")
		artifacts, _ := artsV.([]any)
		if artifacts == nil {
			continue
		}

		var snapshot *jsonenc.Object
		for _, av := range artifacts {
			artifact, ok := av.(*jsonenc.Object)
			if !ok {
				continue
			}
			nameV, _ := artifact.Get("name")
			if pyStrOr(nameV, true) != githubActionsSnapshotArtifactName {
				continue
			}
			expV, expOK := artifact.Get("expired")
			if isTruthyVal(expV, expOK) {
				continue
			}
			snapshot = artifact
			break
		}
		if snapshot == nil {
			continue
		}

		archiveURLV, archiveOK := snapshot.Get("archive_download_url")
		archiveURL := strings.TrimSpace(pyStrOr(archiveURLV, archiveOK))
		artifactIDV, artifactIDOK := snapshot.Get("id")
		artifactID := strings.TrimSpace(pyStrOr(artifactIDV, artifactIDOK))
		if archiveURL == "" {
			continue
		}

		archiveResp, err := s.upstreamRequest("GET", archiveURL, nil,
			githubAPIHeaders(token, false, map[string]string{"Accept": "application/octet-stream"}))
		if err != nil || archiveResp.Status != 200 || len(archiveResp.RawBytes) == 0 {
			continue
		}

		rows := parseGithubActionsSnapshotZip(
			archiveResp.RawBytes, owner, repo, runID, artifactID, releaseID, releaseVersion,
			pyStrOr(getObjField(run, "head_sha"), true), pyStrOr(getObjField(snapshot, "name"), true))
		if len(rows) > 0 {
			return rows
		}
	}
	return nil
}

// getObjField is a tiny accessor returning (value, present-as-true) for pyStrOr at call sites.
func getObjField(o *jsonenc.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// parseGithubActionsSnapshotZip mirrors the zip-parsing loop in _github_actions_dependency_rows: for
// each non-dir entry whose basename matches pip-freeze-<platform>-<arch>.txt, parse the requirements
// content into PyPI deps and emit a dependencies-lockfile artifact row. Any zip error yields no rows
// (mirroring the broad Python try/except returning the rows accumulated so far — empty on failure).
func parseGithubActionsSnapshotZip(archive []byte, owner, repo, runID, artifactID, releaseID, releaseVersion, runHeadSha, artifactName string) []map[string]any {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil
	}
	rows := []map[string]any{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		depName, platform, architecture, ok := githubActionsSnapshotName(f.Name)
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			// Python's broad except returns the rows gathered so far; mirror by stopping.
			return rows
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			rc.Close()
			return rows
		}
		rc.Close()
		rawBytes := buf.Bytes()
		// raw_bytes.decode("utf-8", errors="replace")
		text := strings.ToValidUTF8(string(rawBytes), "�")
		deps := parseRequirementsDeps(text)
		if len(deps) == 0 {
			continue
		}
		meta := jsonenc.NewObject().
			Set("source", "github_actions_artifact").
			Set("repo", owner+"/"+repo).
			Set("run_id", runID).
			Set("run_head_sha", runHeadSha).
			Set("release_version", releaseVersion).
			Set("artifact_name", artifactName).
			Set("dependencies", deps)
		rows = append(rows, map[string]any{
			"Id": newUUIDv4(), "ReleaseId": releaseID, "ArtifactType": "dependencies-lockfile",
			"Name": depName, "ContentType": "text/plain", "Size": len(rawBytes),
			"StorageRef": "github-actions://" + owner + "/" + repo + "/runs/" + runID +
				"/artifacts/" + artifactID + "/" + path.Base(f.Name),
			"ChecksumSha256": sha256Sum(rawBytes), "Platform": platform, "Architecture": architecture,
			"MetadataJson": string(jsonenc.Encode(meta, cveCompactDumpsOpts)),
			"UploadedAt":   normalizeCHTimestampNow(), "IsDeleted": 0, "Version": fixedVersionMillis(),
		})
	}
	return rows
}
