# SOBS Beta 1 (v0.1.0-beta.1)

Status: Pre-release
Date: 2026-04-09

SOBS v0.1.0-beta.1 is the first public beta milestone focused on stability, security hardening, and practical usability for operators.

## Highlights

- Security hardening for hosted controls and container runtime.
- First-time setup wizard for instrumentation bootstrap.
- One-click incident evidence view for faster response workflows.
- AI trace link improvements that preserve time-window context across navigation.
- Regex and free-text filtering enhancements across Errors, Traces, Metrics, and RUM.
- Seasonality-aware mode for auto metric anomaly rule generation.
- Lazy-loaded raw span accordion in trace details for improved performance and UX.
- Human-friendly labels for metric and anomaly signals.
- Import/export/share workflows for saved reports.
- Data management and observability UX updates (database stats, retention/backups, explorer improvements).

## Beta Scope

- This is a beta pre-release intended for staging and non-critical production validation.
- APIs and UI behavior may evolve before stable v0.1.0.
- Feedback on regressions, upgrade friction, and dashboard/query workflows is encouraged.

## Getting Started

- Deploy artifacts tagged v0.1.0-beta.1.
- Confirm deployment config and environment variables match current project docs.
- Run smoke checks for query/search, trace navigation, reports import/export, and data management flows.

## Known Gaps

- Beta-only features do not yet carry a formal long-term compatibility guarantee.
- Additional hardening and polish are planned before stable v0.1.0.

## Included Change Set (Recent)

- #176 Harden hosted security controls and container runtime.
- #175 Add first-time setup wizard for instrumentation bootstrap.
- #174 Fix AI trace links to preserve time-window context across navigation.
- #173 Add one-click incident evidence view.
- #172 Add seasonality-aware mode for auto metric anomaly rule generation.
- #171 Add free-text regex filter to Errors, Traces, Metrics, and RUM pages.
