# SOBS Feature Plan: FRAME (Filter, Resolve, Acknowledge, Mute Events)

## 1) Summary

FRAME stands for **Filter, Resolve, Acknowledge, Mute Events**.

Design and implement first-class incident noise management and workflow states across ingest, query, and UI layers.

The implementation should be **feature-flagged by default** and **relatively isolated from a code perspective**, while remaining **integrated from a UX perspective** inside the existing Errors and incident-oriented views.

This plan targets pre-v1 development and **does not require migrations**. Schema changes will be added directly in bootstrap/create-table logic.

## 2) Goals

- Keep rollout low-risk via feature flags and explicit enablement.
- Keep implementation isolated behind a small internal boundary so core ingest/query code is not broadly entangled.
- Keep incoming rule evaluation and aggregation highly efficient, with very small added ingest delay; prioritize ingest-path performance over UI-path completeness.
- Add explicit lifecycle controls for noisy events and incidents:
  - acknowledge
  - resolve
  - archive/hide
  - mute/until
  - regress/reopen
- Add policy-based noise controls:
  - filter/drop at ingest
  - summarize/aggregate instead of full record storage
  - query-time/UI suppression defaults with show-all toggles
- Preserve auditability and operator trust:
  - who changed state
  - why
  - when it expires
  - what was suppressed

## 3) Non-Goals (for this PR series)

- Full external Alertmanager replacement
- Cross-repo or multi-tenant policy synchronization
- Historical data backfill into new incident tables
- Long-term rule authoring UX polish beyond functional baseline
- Large-scale refactor of the current monolithic [app.py](../app.py) beyond what is needed to carve out a small, local integration boundary

## 4) Scope and Deliverables

### 4.1 Core deliverables

1. New schema objects in bootstrap SQL (no migration path).
2. New API endpoints for incident state transitions and noise policy CRUD.
3. Error/incident UI actions for acknowledge, mute, archive/hide, resolve, reopen.
4. Ingest-time policy evaluation hooks (drop/summarize/pass).
5. Query/UI default suppression behavior and show-all toggles.
6. Tests for state machine behavior, policy safety, and UI/API integration.
7. Feature flags and guarded execution paths for all new behavior.
8. A relatively isolated implementation boundary for incident workflow and noise-policy logic.

### 4.2 Recommended implementation slices

- Slice A: data model + state APIs
- Slice B: UI actions + state badges + filters
- Slice C: ingest policy engine (pass/drop/summarize)
- Slice D: observability counters + admin visibility

### 4.3 Isolation and rollout constraints

- The feature must be off by default unless explicitly enabled.
- Existing resolve behavior and current query/filter flows must remain unchanged when the feature flag is off.
- New logic should sit behind a narrow set of helpers or a small dedicated module boundary, rather than being spread through unrelated code paths.
- UX should remain integrated into existing pages instead of introducing a disconnected parallel UI.
- Ingest-path rule lookup and action selection must be optimized for low latency; slower or richer behavior is acceptable on the UI/query side, but not on the write path.

## 5) Feature Flags and Code Isolation Plan

### 5.1 Feature flags

Add app settings and helper accessors for:

- `incident_workflow_enabled`
- `noise_policies_enabled`
- `noise_ingest_summarization_enabled`

Recommended rollout behavior:

- `incident_workflow_enabled = false` by default
- `noise_policies_enabled = false` by default
- `noise_ingest_summarization_enabled = false` by default

Flag semantics:

- `incident_workflow_enabled`
  - gates new incident-state APIs
  - gates new errors-page actions and badges
  - gates suppression-aware default filtering
- `noise_policies_enabled`
  - gates policy CRUD and policy evaluation on ingest
- `noise_ingest_summarization_enabled`
  - gates summarize action specifically, allowing drop/pass to be introduced first if desired

### 5.2 Code isolation approach

Even if implementation begins in [app.py](../app.py), structure it as a relatively isolated subsystem.

Preferred boundary:

- `incident_workflow.py`
  - incident key generation
  - state transition rules
  - activity log writes
  - suppression/default-visibility logic
- `noise_policies.py`
  - policy validation
  - policy matching
  - ingest action decisioning
  - stats updates

If creating new files is deferred, the equivalent helpers should still be grouped in clearly delimited sections with minimal call sites.

### 5.3 Integration rule

The new subsystem may integrate with:

- existing ingest routes
- existing errors queries
- existing error cards/panels

But the surrounding code should call a small set of helpers such as:

- `is_incident_workflow_enabled(db)`
- `load_incident_state_map(db, incident_ids)`
- `transition_incident_state(...)`
- `evaluate_noise_policy(...)`
- `apply_incident_visibility_filters(...)`

This keeps the implementation integrated in UX, but localized in code.

### 5.4 Centralized cached lookup requirement

Incoming filtering and aggregation should use a **centralized cache lookup function** rather than repeatedly querying storage or rebuilding match structures per request.

Recommended runtime boundary:

- `get_noise_policy_runtime(db)`

This function should return a cached runtime snapshot containing only what the hot path needs, for example:

- enabled feature-flag state relevant to ingest
- enabled policies ordered by priority
- precompiled regex matchers
- prevalidated match expressions or normalized predicates
- summarize-action descriptors
- aggregation key definitions

Recommended behavior:

- in-memory cache
- copy-on-write snapshot replacement
- TTL plus explicit invalidation on policy writes
- no per-event DB roundtrip
- no per-event regex compilation
- no per-event policy normalization

The ingest path should depend on this function as the single lookup seam for filtering and summarization decisions.

## 6) Data Model Plan (No Migration Required)

Implement by extending bootstrap table creation in [app.py](../app.py).

### 6.1 New table: sobs_incident_state

Purpose: current state per incident fingerprint.

Suggested columns:

- IncidentId String
- IncidentKey String (stable fingerprint)
- Source LowCardinality(String) (errors|logs|traces|rum|metrics)
- ServiceName LowCardinality(String)
- Status LowCardinality(String)
  - open
  - acknowledged
  - resolved
  - archived
  - muted
- StatusReason String
- Owner String
- MutedUntil DateTime64(3)
- ArchivedUntil DateTime64(3)
- LastSeenAt DateTime64(3)
- FirstSeenAt DateTime64(3)
- OccurrenceCount UInt64
- IsDeleted UInt8
- Version UInt64
- CreatedAt DateTime64(3)
- UpdatedAt DateTime64(3)

Engine pattern: ReplacingMergeTree(Version), ORDER BY IncidentId.

### 6.2 New table: sobs_incident_activity

Purpose: append-only audit trail for state transitions and policy actions.

Suggested columns:

- Id String
- IncidentId String
- Action LowCardinality(String)
  - acknowledge
  - resolve
  - archive
  - unarchive
  - mute
  - unmute
  - reopen
  - auto_regress
  - policy_drop
  - policy_summarize
- PreviousStatus LowCardinality(String)
- NextStatus LowCardinality(String)
- Actor String
- Note String
- MetadataJson String
- CreatedAt DateTime64(3)

Engine pattern: MergeTree, ORDER BY (IncidentId, CreatedAt, Id).

### 6.3 New table: sobs_noise_policies

Purpose: declarative rules for pass/drop/summarize behavior.

Suggested columns:

- Id String
- Name String
- Enabled UInt8
- Priority Int32
- ScopeSource LowCardinality(String) (errors|logs|traces|rum|metrics|all)
- ScopeService String
- MatchExpr String (validated constrained SQL/expr or regex set)
- Action LowCardinality(String) (pass|drop|summarize)
- SampleRate Float64 (optional, for future)
- MaxPerMinute UInt32 (optional rate cap)
- SummaryWindowSec UInt32
- SummaryKeysJson String
- ExpiresAt DateTime64(3)
- CreatedBy String
- UpdatedBy String
- IsDeleted UInt8
- Version UInt64
- CreatedAt DateTime64(3)
- UpdatedAt DateTime64(3)

Engine pattern: ReplacingMergeTree(Version), ORDER BY (Priority, Id).

### 6.4 New table: sobs_noise_policy_stats

Purpose: operational transparency for suppression.

Suggested columns:

- BucketStart DateTime
- PolicyId String
- Source LowCardinality(String)
- ServiceName LowCardinality(String)
- Action LowCardinality(String)
- MatchedCount UInt64
- DroppedCount UInt64
- SummarizedCount UInt64

Engine pattern: SummingMergeTree, ORDER BY (BucketStart, PolicyId, Source, ServiceName, Action).

## 7) Incident Identity and State Machine

### 7.1 Incident key

Reuse and formalize grouping logic currently used in errors for stable incident keying.

Candidate key components:

- service
- source
- normalized error type
- normalized summary/message fingerprint
- optional trace/span context

Touchpoints:

- [app.py](../app.py) error item/group key helpers
- [templates/errors.html](../templates/errors.html)

### 7.2 State transition rules

Allowed transitions:

- open -> acknowledged
- open -> resolved
- open -> archived
- open -> muted
- acknowledged -> resolved
- acknowledged -> archived
- acknowledged -> muted
- muted -> open (manual or expiry)
- archived -> open (manual) or escalating/regressed trigger
- resolved -> open (regression)

All transitions write both:

- current snapshot in sobs_incident_state
- append event in sobs_incident_activity

## 8) API Contract Plan

Add endpoints in [app.py](../app.py).

All endpoints below must return a clear feature-disabled response when the corresponding feature flag is off.

### 8.1 Incident state actions

- POST /api/incidents/<incident_id>/acknowledge
- POST /api/incidents/<incident_id>/resolve
- POST /api/incidents/<incident_id>/archive
- POST /api/incidents/<incident_id>/unarchive
- POST /api/incidents/<incident_id>/mute
- POST /api/incidents/<incident_id>/unmute
- POST /api/incidents/<incident_id>/reopen

Payload fields:

- note
- owner
- muted_until (for mute)
- archived_until (optional)

Response:

- ok
- incident_id
- previous_status
- current_status
- updated_at

### 8.2 Incident listing and filters

- GET /api/incidents

Filter params:

- source
- service
- status
- include_suppressed
- from_ts
- to_ts
- q

### 8.3 Noise policy management

- GET /api/noise-policies
- POST /api/noise-policies
- PATCH /api/noise-policies/<id>
- DELETE /api/noise-policies/<id>
- POST /api/noise-policies/<id>/test

Validation requirements:

- strict expression safety checks
- regex safety checks
- deterministic action enum
- bounded windows and limits

### 8.4 Stats endpoint

- GET /api/noise-policies/stats

Return suppression metrics for dashboard widgets and troubleshooting.

## 9) Backend Touchpoints

Primary backend file:

- [app.py](../app.py)

### 9.1 Areas to add/modify

1. Bootstrap SQL section:
   - create new tables above
2. Feature flag helpers:
   - central flag reads
   - default-off behavior
3. Isolated helpers or new module(s):
   - centralized runtime cache lookup for ingest policy evaluation
   - incident key builder
   - state transition writer
   - policy matcher and validator
   - suppression stats writer
   - suppression-aware visibility filter helper
4. Ingest paths:
   - logs route (existing v1/logs flow)
   - traces route (existing v1/traces flow)
   - metrics route (existing v1/metrics flow)
   - direct errors route (existing v1/errors flow)
5. Errors page query:
   - join/apply incident states
   - default suppression logic
   - show-all override behavior

### 9.2 Backend isolation guidance

- Do not embed policy/state logic inline across each route if a shared helper can own it.
- Prefer one integration call per hot path, for example:
  - ingest route -> `get_noise_policy_runtime(db)` then `evaluate_noise_policy(runtime, event)`
  - errors query -> `apply_incident_visibility_filters(...)`
  - action route -> `transition_incident_state(...)`
- Keep the feature-flag check near the integration seam, not duplicated deeply through implementation internals.

### 9.3 Ingest-path performance guidance

- Treat ingest rule matching as a hot path.
- Favor memory lookups over DB lookups.
- Precompute everything possible at policy-write time or cache-refresh time.
- Use simple first-match-wins evaluation over a preordered policy list.
- Keep aggregation updates bounded and deterministic.
- Avoid expensive JSON reshaping or broad object copying before a policy match is known.
- If summarize mode is enabled, prefer fixed key extraction over arbitrary dynamic grouping.
- If cache refresh fails, continue using the last known good runtime snapshot instead of falling back to synchronous DB reads on ingest.

## 10) Frontend/UI Touchpoints

### 10.1 Errors page

- [templates/errors.html](../templates/errors.html)
- [templates/_error_panels.html](../templates/_error_panels.html)

Add:

- action buttons: Ack, Mute, Archive, Resolve, Reopen
- state badge rendering (Open, Acked, Muted, Archived, Resolved, Regressed)
- filter options for status and include-suppressed/show-all
- mute duration quick picks (1h, 24h, 7d, custom)
- render all new controls only when `incident_workflow_enabled` is on

### 10.2 Optional follow-on pages

- [templates/work_items.html](../templates/work_items.html) for status badge pattern reuse
- [templates/cve.html](../templates/cve.html) disposition hide/show-all pattern reuse

### 10.3 UX behavior principles

- Mute suppresses alerts and default lists, not data existence.
- Archive/hide removes from default triage view but remains searchable.
- Resolve can regress automatically when recurrence threshold is hit.
- UX remains inside existing pages and components so operators do not need to learn a parallel workflow surface.
- When feature flags are off, the UI should fall back cleanly to the existing experience.

## 11) Ingest Noise Control Plan

### 11.1 Policy decision flow

For each event at ingest:

0. Check `noise_policies_enabled`; if off, bypass with zero behavior change.
1. Retrieve current runtime snapshot via `get_noise_policy_runtime(db)`.
2. Build lightweight match context (source, service, severity, key attrs).
3. Evaluate enabled policies by priority using cached, precompiled match structures.
4. First-match-wins action:
   - pass: normal path
   - drop: do not store full event, increment policy stats, emit activity record if incident known
   - summarize: update aggregate window table/counter, optionally keep exemplar event only

Performance note:

- This flow must avoid synchronous policy reads from storage on every event.
- The event context used for matching should be intentionally small and normalized for fast access.

### 11.2 Summary strategy

Summarize mode stores:

- count
- first_seen
- last_seen
- top attrs
- exemplar event reference

Goal: reduce storage and UI noise without losing trend and ownership context.

### 11.3 Aggregation implementation guidance

- Aggregation logic should be optimized for bounded write amplification.
- Prefer centralized key derivation such as `build_noise_summary_key(event, policy)`.
- Keep summary windows coarse and explicit, for example minute or short rolling window buckets.
- Avoid complex query-time reconstruction requirements for the ingest fast path.
- If exemplar retention is supported, store at most one exemplar per key/window by default.

## 12) Testing Plan

Primary suite file:

- [tests/test_app.py](../tests/test_app.py)

### 12.1 Unit and API tests

Add tests for:

- feature disabled behavior preserves current outputs
- feature enabled behavior activates new APIs and UI paths
- centralized cache lookup returns stable runtime snapshots
- cache invalidation/refresh updates policy behavior without per-event DB reads
- valid/invalid state transitions
- acknowledge/mute/archive/resolve/reopen endpoints
- mute expiry behavior
- regression reopening behavior
- policy validation rejects unsafe expressions
- policy action execution: pass/drop/summarize

### 12.2 Ingest performance-focused tests

Add tests for:

- ingest evaluation works against cached policy runtime
- no policy-match recompilation is required per event
- summarize key derivation remains deterministic across repeated events
- last known good cache snapshot continues to function if refresh fails

### 12.3 UI integration tests

Add tests for:

- feature flag off hides new actions and preserves current Errors UX
- feature flag on shows integrated controls in existing Errors UX
- errors page shows new action buttons
- status filters include new lifecycle states
- default view hides archived/muted/resolved where applicable
- show-all toggle restores hidden states

### 12.4 Safety and regression tests

- ensure existing resolve flow continues working
- ensure grouped errors mode remains functional
- ensure existing regex filters still work with new state filters
- ensure ingest routes are behavior-identical when flags are off

## 13) Observability and Operational Controls

Add counters and dashboards for:

- matched policies
- dropped events
- summarized events
- incident transitions by action
- regressed incidents count
- active mutes and upcoming expiries

This can be exposed via lightweight JSON APIs first; charts can follow.

## 14) Issue Template (Ready to Paste)

Title:

- Feature: FRAME incident lifecycle states + ingest/query noise controls

Description:

- Implement FRAME: Filter, Resolve, Acknowledge, Mute Events. This includes first-class incident states (acknowledge, mute, archive/hide, resolve, reopen) and policy-driven noise controls (drop/summarize/pass), with auditability and suppression visibility. The implementation must be feature-flagged by default, relatively isolated from a code perspective, and integrated into the existing UX.

Acceptance Criteria:

- New bootstrap schema objects are present (no migration required).
- Feature flags exist and default to off.
- Existing behavior is unchanged when the feature flags are off.
- New incident state APIs are available and validated.
- Errors UI supports Ack/Mute/Archive/Resolve/Reopen actions.
- Default errors view suppresses archived/muted/resolved unless show-all is enabled.
- Ingest policy engine supports pass/drop/summarize actions.
- Core logic is implemented behind a relatively isolated helper/module boundary.
- Test coverage added for state machine, API behavior, and suppression paths.

## 15) PR Plan (Implementation Sequence)

1. Add feature flags and isolated helper/module scaffolding.
2. Add bootstrap schema and state/policy helper implementations.
3. Add incident state APIs + tests in [tests/test_app.py](../tests/test_app.py).
4. Integrate Errors UI actions and filters in [templates/errors.html](../templates/errors.html) and [templates/_error_panels.html](../templates/_error_panels.html), gated by feature flag.
5. Integrate ingest policy evaluation in existing ingest routes in [app.py](../app.py), gated by feature flag.
6. Add suppression stats endpoint and basic visibility hooks.
7. Final documentation update in docs (new design/feature doc).

## 16) Risks and Mitigations

- Risk: over-suppression hides real incidents.
  - Mitigation: show-all toggle + suppression stats + policy test endpoint + audit trail.
- Risk: policy matcher latency on hot ingest path.
  - Mitigation: compile/cache policy predicates, first-match short-circuit, strict limits.
- Risk: centralized cache becomes stale or unavailable and slows ingest.
  - Mitigation: use TTL + explicit invalidation + last-known-good snapshot fallback + no synchronous fallback to per-event DB lookups.
- Risk: status confusion between incident state and issue/work-item state.
  - Mitigation: explicit labels in UI and separate filter dimensions.
- Risk: feature code spreads through unrelated areas and becomes hard to remove or iterate.
  - Mitigation: enforce helper/module boundary and explicit feature-gated integration seams.

## 17) Definition of Done

- Feature complete for errors lifecycle and noise controls.
- No migration required.
- Feature flags default to off and preserve existing behavior.
- Implementation is relatively isolated in code and integrated in UX.
- Tests pass in CI.
- Issue and PR include clear operator-facing behavior notes and rollback guidance (policy disable).