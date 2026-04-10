# SOBS v0.1.0-beta.1 Operator Notes

Status: Pre-release
Date: 2026-04-09
Target: Platform operators, SRE, and deployment owners

This document provides operational guidance for validating and rolling out SOBS v0.1.0-beta.1.

## What Changed

### Security and Runtime Hardening

- Hosted security controls and container runtime hardening landed in this beta.
- Expect stricter runtime behavior and verify container/platform policies are compatible with your environment.

### Setup and Onboarding

- First-time setup wizard added to streamline instrumentation bootstrap.
- Validate wizard output against your organization defaults before broad rollout.

### Incident and Investigation UX

- One-click incident evidence view added for faster triage.
- AI trace links now preserve time-window context when navigating.

### Query, Filtering, and Signal Quality

- Regex/free-text filtering expanded across Errors, Traces, Metrics, and RUM.
- Auto anomaly rule generation includes seasonality-aware mode.
- Human-friendly signal labels improve readability and handoff.

### Data and Reporting Workflows

- Saved report import/export/share workflows are available.
- Data management and stats UX updates improve retention/backup and storage observability workflows.

## Pre-Deployment Checklist

- Confirm image/artifact tag is v0.1.0-beta.1 in every environment.
- Review deployment manifests and environment variables for drift from current docs.
- Validate backup and restore paths before production-like testing.
- Confirm retention policy behavior after upgrade.

## Post-Deployment Smoke Tests

- Ingestion: verify traces, metrics, logs, and RUM events continue arriving.
- Query: verify query page behavior and expected filter semantics.
- Trace UX: verify detail panel expansion, raw span loading, and AI trace links.
- Reports: verify report create/import/export/share paths.
- Data Management: verify retention settings and backup workflows.
- Summary/Sidebar: verify version label and navigation semantics display correctly.

## Rollout Strategy

- Start in staging.
- Promote to a limited production slice.
- Monitor error rate, query latency, ingestion lag, and backup success metrics.
- Proceed to wider rollout only after at least one full business cycle of stable behavior.

## Risk and Compatibility Notes

- This is a beta release and may include behavior changes before stable v0.1.0.
- Backward compatibility for beta-only capabilities is not guaranteed.
- Preserve rollback path to prior known-good release artifacts.

## Rollback Guidance

- Revert to the previous known-good release tag.
- Restore prior deployment manifests/config values if changed during beta rollout.
- Validate ingestion continuity and dashboard/query correctness after rollback.

## Source Changes Referenced

- #176 Harden hosted security controls and container runtime.
- #175 Add first-time setup wizard for instrumentation bootstrap.
- #174 Fix AI trace links to preserve time-window context across navigation.
- #173 Add one-click incident evidence view.
- #172 Add seasonality-aware mode for auto metric anomaly rule generation.
- #171 Add free-text regex filter to Errors, Traces, Metrics, and RUM pages.
