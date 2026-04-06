# Automated Agent Research And Repo Issue Orchestration Design

## Goal
Enable SOBS to continuously watch configured applications, detect likely instrumentation gaps/errors/regressions from OTEL and related tables, run asynchronous investigation tasks, and open actionable GitHub issues in the correct repositories.

This design extends the current Agent Rules and Notification systems to support:
- Data-sufficiency gated investigations ("wait until enough signal").
- Cross-signal research tasks (traces, errors, logs, rum, metrics, anomaly signals, release metadata).
- Repo-aware issue routing (FE/BE/service repos) using app/service ownership mappings.
- Permission-aware handling for third-party/open-source repos.
- Stateful async orchestration with reminders, retries, and resumable execution.

## Non-goals
- Unapproved autonomous code changes in any repository.
- Bypassing org policy or repository governance.
- Replacing human on-call ownership.

## Current Foundation (Already In SOBS)
- Tables:
  - `otel_logs`, `otel_traces`, `hyperdx_sessions`, derived signal views.
  - `sobs_agent_rules`, `sobs_agent_runs`.
  - `sobs_notification_channels`, `sobs_notification_rules`, `sobs_notification_log`.
  - `sobs_apps`, `sobs_app_releases`, `sobs_release_artifacts`.
  - `sobs_ai_settings` for model/GitHub settings.
- Existing flow:
  - Rule-triggered async agent run with guard checks, optional DLP check, and optional GitHub issue creation.
  - Notification channels for Slack/webhook/email/browser push.

## Key Product Requirements
1. Watch configured apps over time and trigger agent investigations once enough data exists.
2. Use GitHub MCP integration to investigate and prepare issues per owned service repo.
3. Raise issues for discovered gaps/errors/regressions with evidence and remediation suggestions.
4. If repo is external/open-source dependency, ask user permission before opening issues.
5. If context/config is missing, notify SOBS user and request input.
6. Make all processes asynchronous, stateful, and resumable.
7. Add scheduler/reminder capability so unresolved tasks are revisited.
8. Support GitHub issue creation and, with explicit user approval, assignment/request to Copilot to produce a PR.
9. Allow the built-in AI Assistant to configure triggers, rule conditions, backoff, and throttling using natural-language requests with confirmation and policy guardrails.

## High-Level Architecture

### 1. Orchestrator
A background orchestrator runs periodically and via events.

Inputs:
- Rule triggers (anomaly/tag/manual).
- Scheduled reminders and retry windows.
- New telemetry/release events.

Responsibilities:
- Select eligible apps/tasks.
- Check data sufficiency.
- Create and advance task state machines.
- Dispatch notifications and follow-ups.

### 1.5. AI Configuration Translator
Converts natural-language requests from the built-in AI Assistant into validated agent/rule/scheduler configuration changes.

Responsibilities:
- Parse intent for trigger setup, rule logic, backoff, and throttling.
- Validate requested changes against policy and schema constraints.
- Present diff/preview and require confirmation for medium/high-risk changes.
- Persist approved changes to agent and scheduler configuration tables.

### 2. Research Task Engine
Executes typed research tasks asynchronously.

Task types (initial):
- `instrumentation_gap_audit`
- `error_cluster_audit`
- `release_regression_audit`
- `cross_signal_timeline_audit`

Each task produces:
- Evidence summary.
- Findings with confidence and severity.
- Suggested owner repo(s).
- Recommended actions.

### 3. Repo Router
Maps findings to repositories.

Sources:
- `sobs_apps` (`RepoUrl`, `OwnerTeam`, `MetadataJson`).
- Service to app mapping rules.
- Optional overrides in task/policy tables.

Behavior:
- Internal repo: create issue (subject to guardrails).
- External/open-source repo: request user permission first.

### 4. Issue Publisher
Primary: supported GitHub issue assignment APIs for Copilot cloud agent.
Fallback: existing REST issue creation path when issue reuse/create is needed but agent assignment is not.

Publishes:
- Structured issue title/body/labels.
- Traceability metadata (task id, rule id, app id, signal window).
- Optional Copilot assignment/request action after issue creation (approval-gated), using GitHub's supported issue-assignment flow for `copilot-swe-agent[bot]` plus `agent_assignment` parameters.

### 4.5. Incident Deduplication And Assignment Controller
GitHub issue creation and GitHub Copilot assignment are separate decisions.

Responsibilities:
- Build a deterministic incident fingerprint from local telemetry and trigger context.
- Search existing SOBS work-item history and open GitHub issues before opening new work.
- Ask the configured local LLM to classify candidate matches as `same`, `related`, or `unrelated`.
- Reuse or link existing issues when confidence is high enough instead of opening duplicates.
- Prevent Copilot assignment noise by allowing only a small number of active assignments at a time.

Policy:
- New issue creation is only allowed when no strong duplicate exists.
- Copilot assignment is only allowed when the chosen issue is not already assigned, not already linked to an active PR, and no near-duplicate issue is already being worked.
- Rate limits are a relief valve, not the primary control surface. The LLM-backed dedupe step is expected to reduce noise first.

### 5. Missing Context Resolver
Detects missing fields and asks user for clarification/config using notification channels.

Examples:
- Missing repo mapping for `service.name`.
- No ownership/team assignment.
- Unknown external repo permission status.
- Missing GitHub credentials or MCP connectivity settings.

### 6. Reminder Scheduler
Schedules follow-up checks for:
- Pending user input.
- Pending approval for external repo issues.
- Re-audit after issue creation.
- Retry after transient failures.

## Data Model Additions

### New Tables

#### `sobs_agent_tasks`
Tracks long-lived async investigation tasks.

Columns:
- `Id` String
- `TaskType` LowCardinality(String)
- `AppId` String
- `RuleId` String
- `TriggerContext` String
- `Status` LowCardinality(String)  -- queued|running|waiting_input|waiting_approval|ready_to_publish|published|failed|closed
- `Priority` LowCardinality(String) -- low|medium|high|critical
- `CurrentStep` LowCardinality(String)
- `FindingsJson` String
- `EvidenceJson` String
- `OutputSummary` String
- `ErrorMessage` String
- `CreatedAt` DateTime64(3)
- `UpdatedAt` DateTime64(3)
- `CompletedAt` DateTime64(3)
- `IsDeleted` UInt8
- `Version` UInt64

#### `sobs_agent_task_events`
Append-only event log for state transitions and decisions.

Columns:
- `TaskId` String
- `EventType` LowCardinality(String) -- created|step_started|step_completed|awaiting_input|approval_requested|issue_published|retry_scheduled|failed|closed
- `EventPayload` String
- `CreatedAt` DateTime64(3)

#### `sobs_agent_repo_mappings`
Maps app/service/signal scopes to repo targets.

Columns:
- `Id` String
- `AppId` String
- `ServicePattern` String
- `RepoHost` LowCardinality(String) -- github
- `RepoRef` String -- owner/repo
- `RepoType` LowCardinality(String) -- internal|external
- `OwnerTeam` String
- `IsDefault` UInt8
- `IsDeleted` UInt8
- `Version` UInt64

#### `sobs_agent_repo_permissions`
Approval policy for external/open-source issue creation.

Columns:
- `Id` String
- `RepoRef` String
- `PermissionStatus` LowCardinality(String) -- unknown|allowed|denied|one_time_approved|pending
- `ApprovalScope` LowCardinality(String) -- app|repo|task
- `ApprovedBy` String
- `Reason` String
- `ExpiresAt` DateTime64(3)
- `IsDeleted` UInt8
- `Version` UInt64

#### `sobs_agent_reminders`
Scheduler queue for delayed work.

Columns:
- `Id` String
- `TaskId` String
- `ReminderType` LowCardinality(String) -- input_followup|approval_followup|post_issue_check|retry
- `ScheduledAt` DateTime64(3)
- `Status` LowCardinality(String) -- pending|fired|cancelled|expired
- `Attempts` UInt32
- `PayloadJson` String
- `LastError` String
- `IsDeleted` UInt8
- `Version` UInt64

#### `sobs_agent_issue_links`
Links tasks/findings to created GitHub issues.

Columns:
- `Id` String
- `TaskId` String
- `RepoRef` String
- `IssueUrl` String
- `IssueNumber` UInt64
- `IssueState` LowCardinality(String)
- `CopilotAssignmentStatus` LowCardinality(String) -- not_requested|waiting_approval|requested|failed
- `CopilotAssignmentRef` String
- `CreatedAt` DateTime64(3)
- `UpdatedAt` DateTime64(3)
- `IsDeleted` UInt8
- `Version` UInt64

#### `sobs_github_work_items` additive fields
The existing work-item table should be extended so issue creation, dedupe, and agent-assignment decisions are explainable and queryable from the UI and APIs.

Additional columns:
- `DedupKey` String
- `DedupDecision` LowCardinality(String) -- new_issue|reused_existing|related_existing|suppressed_duplicate|skipped_active_work
- `DedupConfidence` Float64
- `CanonicalIssueUrl` String
- `CanonicalIssueNumber` UInt64
- `RelatedIssueUrls` String -- JSON array
- `OccurrenceCount` UInt32
- `CopilotAssignmentRequestedAt` DateTime64(3)
- `CopilotAssignmentStatus` LowCardinality(String) -- not_requested|eligible|requested|active|blocked|failed|completed
- `CopilotAssignmentReason` String
- `LinkedPrUrl` String
- `LinkedPrNumber` UInt64

These fields support both operator visibility in the Work Items page and throttling logic for future assignment decisions.

## Dedupe And Noise-Control Decision Flow

### Step 1: Build Incident Fingerprint
Construct a stable local fingerprint from:
- service name
- trigger source / signal / state
- normalized exception or error signature when available
- release version and environment when available
- short normalized analysis summary

The fingerprint is used as a first-pass candidate search key, not as the final truth.

### Step 2: Gather Candidate Matches
Before creating or assigning anything, query:
- recent `sobs_github_work_items`
- existing open GitHub issues for the target repo
- open PRs linked to candidate issues
- assignment state for candidate issues

The search should bias toward recency and identical service/repo scope, then widen to semantically related incidents.

### Step 3: Local LLM Triage
The configured local LLM should receive the proposed incident and a bounded set of candidate incidents/issues and return structured JSON:
- `classification`: `same|related|unrelated`
- `canonical_issue_url`
- `confidence`
- `reason`
- `recommendation`: `reuse|link|create_new|skip_assignment`

The model should not be given unconstrained database access by default. SOBS should provide bounded search results and, if needed later, carefully scoped helper tools to fetch more evidence.

### Step 4: Act On Existing Work
If the LLM classifies the incident as `same`:
- do not create a new GitHub issue
- attach the observation to the canonical issue in SOBS
- increment local occurrence counters
- optionally post a GitHub comment or reaction only when configured thresholds are crossed

If classified as `related`:
- link the new observation to the related issue
- avoid Copilot assignment when the related issue already has active work

If classified as `unrelated`:
- create a new issue only if rate limits and policy checks allow

### Step 5: Copilot Assignment Gate
Copilot assignment should only be requested when all of the following are true:
- the repo has Copilot cloud agent enabled
- the issue is actionable and has sufficient telemetry context
- the issue is not already assigned or otherwise in active agent work
- there is no linked open PR already addressing the issue
- global and per-repo active-assignment limits allow another request

Suggested initial limits:
- `ai.agent_max_issues_per_hour = 1`
- one active Copilot assignment globally
- one active Copilot assignment per repo

## Validation Expectations

Validation should prove the following separately:
- SOBS can create or reuse GitHub issues correctly.
- SOBS records dedupe and reuse decisions in work items.
- SOBS suppresses duplicate Copilot requests when existing work is already active.
- SOBS only requests Copilot assignment when the chosen issue is eligible.

Validation should not treat a mere comment mention as equivalent to formal agent assignment. The product should align with GitHub's explicit agent workflow surfaced in the issue UI.

## App/Service Watch Eligibility
A task should not start until data is sufficient.

Warm-up criteria (configurable):
- Minimum lookback: 24h (default).
- Per app/service minimum volume thresholds:
  - Traces >= N spans.
  - Errors >= N events.
  - RUM >= N sessions/events (if app has FE telemetry).
  - Logs >= N records.
- Optional release correlation available (if configured).

If not sufficient:
- Keep app in `observing` state.
- Re-evaluate at scheduled cadence.

## Investigation Pipeline

### Step A: Scope Construction
Input dimensions:
- App, service, environment, release window, signal windows.
- Trace gap intervals and coverage indicators.
- Error clusters and recurring signatures.

### Step B: Cross-Signal Correlation
Correlate by:
- `trace_id`, `span_id`, service, time window, release metadata.

Example checks:
- Logs/errors inside no-span intervals -> potential instrumentation gap.
- Error spikes aligned with release version -> regression hypothesis.
- RUM frontend failures without backend trace linkage -> FE instrumentation gap.

### Step C: Findings and Confidence
Classify finding:
- `expected_idle`
- `possible_instrumentation_gap`
- `likely_partial_trace`
- `clock_or_ordering_issue`
- `release_regression`

For each finding store:
- Evidence snippets (query-derived).
- Confidence.
- Suggested owner (team/repo).
- Proposed remediation tasks.

### Step D: Repo Resolution
Resolve target repo(s):
1. Exact mapping from `sobs_agent_repo_mappings`.
2. `sobs_apps.RepoUrl` fallback.
3. Heuristic by `service.name` patterns.
4. If unresolved -> request user context.

### Step E: Publication Decision
- Internal repo and policy allows: publish issue.
- External/open-source repo:
  - If permission missing/unknown, request approval first.
  - If denied, keep recommendation in SOBS task only.
- Copilot assignment/request:
  - Requires explicit user approval before issuing assignment/request action.
  - If approved, request Copilot against the created issue and persist assignment status.
  - If denied or timed out, keep issue open without Copilot assignment and record decision.

### Step F: Follow-up Scheduling
Create reminders for:
- No user response to context request.
- Pending external approval.
- Post-issue check after 24-72h.
- Retry transient failures with backoff.

## GitHub MCP Integration Design

### Preferred Path
Use GitHub MCP operations for:
- Repo existence/visibility validation.
- Issue creation.
- Optional label and assignee enrichment.
- Optional link-back comment updates.
- Optional Copilot assignment/request action (approval-gated).

### Fallback Path
Use existing GitHub REST flow when MCP is unavailable.

Copilot fallback behavior:
- If direct assignment is unsupported, post an explicit issue comment requesting Copilot to prepare a PR.
- Preserve the resulting reference/comment URL in task artifacts.

### Issue Template
Required blocks:
- Executive summary.
- Evidence timeline and affected windows.
- Cross-signal evidence table.
- Suspected root causes.
- Proposed instrumentation/code audit checklist.
- Reproduction/verification queries.
- SOBS metadata:
  - Task ID, App ID, Rule ID, signal window, environment.

## Missing Context And User Prompting
When required context is absent, create `waiting_input` task state and notify user.

Missing context examples:
- Service -> repo mapping missing.
- Unknown app owner team.
- External repo policy unknown.
- Missing MCP/GitHub credentials.

Notification behavior:
- Use configured channels from notification subsystem.
- Include compact call-to-action and deep link to relevant settings page.
- Persist prompt and response status in task events.

## Async State Machine

States:
- `queued`
- `running`
- `waiting_input`
- `waiting_approval`
- `waiting_copilot_approval`
- `ready_to_publish`
- `published_pending_copilot`
- `published`
- `failed`
- `closed`

Transitions:
- `queued -> running`
- `running -> waiting_input` (context required)
- `running -> waiting_approval` (external repo permission required)
- `running -> ready_to_publish` (all requirements met)
- `ready_to_publish -> published` (issue only)
- `published -> waiting_copilot_approval` (copilot request configured and approval required)
- `waiting_copilot_approval -> published_pending_copilot` (approval granted and request submitted)
- `published_pending_copilot -> published` (copilot request confirmed or terminally skipped)
- Any state -> `failed` (terminal or retryable)
- `published -> closed` (after verification/remediation window)

Retry policy:
- Exponential backoff with capped attempts.
- Retryable errors: transient network/MCP/GitHub failures.
- Non-retryable errors: policy denial, invalid config schema.

## Scheduler Model
Two trigger paths:
1. Time-based poller (every X minutes):
- Evaluate due reminders.
- Re-check warm-up eligibility.
- Resume paused tasks.

2. Event-based wake-up:
- New anomaly state changes.
- New release ingestion.
- New context/approval from user.

Implementation note:
- Keep scheduler lightweight and idempotent.
- Use DB row-level versioning semantics already in use (`ReplacingMergeTree` with `Version`).

## Security And Governance
- Guard model required before issue publication.
- Optional DLP scan required for outbound issue content.
- Hard redaction of sensitive keys and secrets before persistence/publication.
- External repo issue creation must require explicit user approval unless policy says allowed.
- Copilot assignment/request must require explicit user approval and be fully auditable.
- Rate limits per app/team/global to avoid issue spam.
- Natural-language config changes from AI Assistant must be validated, policy-checked, and confirmation-gated before apply.

## Configuration

### New AI/App Settings
- `ai.agent_scheduler_enabled`
- `ai.agent_scheduler_interval_seconds`
- `ai.agent_warmup_hours`
- `ai.agent_min_spans`
- `ai.agent_min_errors`
- `ai.agent_min_logs`
- `ai.agent_min_rum_events`
- `ai.agent_external_repo_default_policy`  -- deny|ask|allow
- `ai.agent_copilot_assignment_default_policy` -- deny|ask|allow
- `ai.agent_post_issue_recheck_hours`
- `ai.agent_retry_max_attempts`
- `ai.agent_nl_config_enabled`
- `ai.agent_nl_config_confirmation_default` -- ask|always
- `ai.agent_nl_config_max_rate_limit_per_rule`
- `ai.agent_nl_config_backoff_min_seconds`
- `ai.agent_nl_config_backoff_max_seconds`
- `ai.agent_nl_config_throttle_min_interval_seconds`
- `ai.agent_nl_config_throttle_max_interval_seconds`

### Existing Settings Reused
- `ai.endpoint_url`, `ai.model`, `ai.api_key`
- `ai.guard_endpoint_url`, `ai.guard_model`
- `ai.dlp_endpoint_url`
- `ai.github_token`, `ai.github_repo` (fallback path)
- `ai.agent_max_issues_per_hour`

## APIs And UI Additions

### New/Extended APIs
- `POST /api/agents/tasks/run` (manual task start)
- `GET /api/agents/tasks` (task list/status)
- `POST /api/agents/tasks/<id>/ack-context`
- `POST /api/agents/tasks/<id>/approve-external-repo`
- `POST /api/agents/tasks/<id>/approve-copilot-assignment`
- `POST /api/agents/scheduler/tick` (manual scheduler tick)
- `POST /api/agents/repo-mappings` CRUD
- `POST /api/agents/repo-permissions` CRUD
- `POST /api/agents/config/plan-from-nl` (natural-language to config diff preview)
- `POST /api/agents/config/apply` (apply confirmed config diff)

### UI Surfaces
- Settings -> Agents:
  - Task queue, reminders, approvals, context requests.
  - Repo mapping manager and policy controls.
  - AI Assistant-driven "Configure from natural language" panel with preview/apply flow.
- Settings -> Notifications:
  - Rule templates for context/approval reminders.

## Observability Of The Agent System
Emit OTEL events for each step:
- `sobs.agent.task.created`
- `sobs.agent.task.state_changed`
- `sobs.agent.finding.generated`
- `sobs.agent.context.requested`
- `sobs.agent.approval.requested`
- `sobs.agent.github.issue_published`
- `sobs.agent.github.copilot_assignment_requested`
- `sobs.agent.github.copilot_assignment_result`
- `sobs.agent.reminder.fired`

Include dimensions:
- `task_id`, `rule_id`, `app_id`, `repo_ref`, `status`, `latency_ms`.

## Rollout Plan

### Phase 1: Stateful Task Framework
- Add task/event/reminder tables.
- Add scheduler tick and idempotent state transitions.
- Add basic task UI.

### Phase 1.5: AI Natural-Language Config
- Add natural-language config planner endpoint and config diff model.
- Add confirmation-gated apply endpoint with audit events.
- Add agent settings UI for NL config policy limits (backoff/throttling bounds).

### Phase 2: Cross-Signal Research Tasks
- Implement `instrumentation_gap_audit` and `error_cluster_audit`.
- Add confidence and evidence schema.

### Phase 3: Repo Routing And Publishing
- Add repo mappings and permission policies.
- Integrate GitHub MCP issue creation path.
- Keep REST fallback.

### Phase 3.5: Copilot Assignment Workflow
- Add approval-gated Copilot assignment/request step after issue publication.
- Add persistence and telemetry for assignment request lifecycle.
- Add fallback comment-based request behavior when direct assignment is unavailable.

### Phase 4: Context/Approval Workflows
- Add user prompts via notification channels.
- Add reminders and escalation behavior.

### Phase 5: Verification Loop
- Post-issue recheck tasks and closure suggestions.
- Quality metrics and tuning.

## Success Metrics
- Mean time from trigger to actionable issue draft.
- Percent tasks with sufficient evidence quality.
- False-positive rate of "instrumentation gap" findings.
- Percent of tasks blocked by missing context (should decrease over time).
- Percent of eligible issues with approved Copilot assignment requests.
- Issue reopen rate after remediation.

## Open Questions
1. Should external repo approvals be one-time, per-repo, or per-org by default?
2. Should issue publication require human approval for severity below `high`?
3. Should scheduler run in-process only, or support external worker mode?
4. Should Copilot assignment approval be per-task, per-repo, or policy-based with expiry?
5. Should we support non-GitHub destinations (Jira/Linear) in same framework?

## Example End-To-End Flow
1. Rule fires for app `checkout-web` (warning state).
2. Task queued and passes warm-up/data thresholds.
3. Agent correlates traces/logs/errors/rum and finds repeated no-span intervals with error activity.
4. Repo router maps FE findings -> `org/checkout-web`, BE findings -> `org/checkout-api`.
5. FE repo internal: issue published immediately with evidence and remediation checklist.
6. BE finding points to external OSS lib repo: permission request sent via Slack + browser push.
7. Task enters `waiting_approval` and schedules 24h reminder.
8. User approves in settings; scheduler resumes and publishes issue.
9. Copilot assignment request is queued, user approves, and Copilot is requested to prepare a PR.
10. Post-issue reminder triggers recheck in 48h and suggests close/follow-up.
