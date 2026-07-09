package main

import (
	"strings"
	"testing"
)

// TestBuildOnboardingIssueBody covers the port of app.py's _build_ci_metadata_issue_body /
// _build_otel_audit_issue_body (buildOnboardingIssueBody's real markdown bodies, previously a
// 2-line stand-in — see PR #352 review). These assert on structural content (CI-secrets table,
// setup checklist, code samples) rather than full-body byte equality, since the corpus doesn't
// carry a golden case for these bodies (they're sent to GitHub and never echoed in the JSON
// response). Byte-for-byte parity against app.py was verified manually against the frozen oracle
// during the port.
func TestBuildOnboardingIssueBody(t *testing.T) {
	t.Run("ci kind with GitHub Actions detected", func(t *testing.T) {
		body := buildOnboardingIssueBody("ci", "acme", "widgets", true)

		mustContain(t, body, "# Sobs CI Metadata Setup")
		mustContain(t, body, "This issue defines how `acme/widgets` should integrate with Sobs CI metadata.")
		mustContain(t, body, "This repository uses **GitHub Actions**.")
		mustNotContain(t, body, "No GitHub Actions workflows were detected.")

		// CI secrets table (Step 3).
		mustContain(t, body, "| Secret | Description |")
		mustContain(t, body, "| `SOBS_URL` | Base URL of your Sobs instance (for example `https://sobs.internal`) |")
		mustContain(t, body, "| `SOBS_INGEST_API_KEY` | Sobs ingest API key from Settings -> Repositories |")
		mustContain(t, body, "| `SOBS_APP_ID` | Application ID from Settings -> Repositories |")

		// curl code sample for the release push.
		mustContain(t, body, "```bash")
		mustContain(t, body, `curl -sS -X POST "${SOBS_URL}/v1/apps/${SOBS_APP_ID}/releases" \`)
		mustContain(t, body, `"version":    "${VERSION}",`)

		// Lockfile + source-map artifact upload samples.
		mustContain(t, body, "## Step 4 - Upload dependency lockfile metadata")
		mustContain(t, body, `"artifactType": "dependencies-lockfile",`)
		mustContain(t, body, "## Step 5 - Upload JS source maps (web front-end only)")
		mustContain(t, body, `"artifactType": "js_sourcemap",`)

		// Manual verification checklist.
		mustContain(t, body, "## Manual verification checklist")
		mustContain(t, body, "- Confirm first pushed release appears in Sobs")
		mustContain(t, body, "- Confirm polling-only fallback works if CI push or webhook path is blocked")

		mustContain(t, body, "*This issue was created automatically by the Sobs Onboarding Wizard for repository `acme/widgets`.*")
	})

	t.Run("ci kind without GitHub Actions", func(t *testing.T) {
		body := buildOnboardingIssueBody("ci", "acme", "widgets", false)

		mustContain(t, body, "No GitHub Actions workflows were detected. The steps below are provider-agnostic and can")
		mustContain(t, body, "be adapted for Jenkins, CircleCI, GitLab CI, Buildkite, or other CI systems.")
		mustNotContain(t, body, "This repository uses **GitHub Actions**.")
	})

	t.Run("otel kind", func(t *testing.T) {
		body := buildOnboardingIssueBody("otel", "acme", "widgets", true)

		mustContain(t, body, "# OTEL & RUM Telemetry Audit")
		mustContain(t, body, "This issue requests a comprehensive audit of the `acme/widgets` repository")

		// Audit checklist sections.
		mustContain(t, body, "## Audit Checklist")
		mustContain(t, body, "### 1. Core OTEL SDK Setup")
		mustContain(t, body, "- [ ] Install and configure the OTEL SDK for the primary language(s) used in this repository")
		mustContain(t, body, "### 2. Web Front-End — RUM Snippet (if applicable)")
		mustContain(t, body, "### 3. AI / LLM Workloads (if applicable)")
		mustContain(t, body, "### 4. Infrastructure & Web Logs (if applicable)")
		mustContain(t, body, "### 5. Error & Exception Capture")
		mustContain(t, body, "### 6. Telemetry Verification")

		// Code samples.
		mustContain(t, body, "```python")
		mustContain(t, body, "from opentelemetry.sdk.trace import TracerProvider")
		mustContain(t, body, "```html")
		mustContain(t, body, "window.SobsRumConfig = {")

		mustContain(t, body, "## What remains manual")
		mustContain(t, body, "*This issue was created automatically by the Sobs Onboarding Wizard for repository `acme/widgets`.*")
	})
}

func mustContain(t *testing.T, body, substr string) {
	t.Helper()
	if !strings.Contains(body, substr) {
		t.Errorf("expected body to contain %q, but it did not.\nbody:\n%s", substr, body)
	}
}

func mustNotContain(t *testing.T, body, substr string) {
	t.Helper()
	if strings.Contains(body, substr) {
		t.Errorf("expected body NOT to contain %q, but it did.\nbody:\n%s", substr, body)
	}
}
