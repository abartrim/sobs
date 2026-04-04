# AI Assistant UI Action Framework (Generic + Secure)

## Goal
Provide a generic way for the AI Assistant to propose and execute UI actions while preserving security, user trust, and auditability.

## Non-goals
- No arbitrary DOM control by the model.
- No direct model execution of JavaScript/selectors/XPath.
- No bypass of existing auth, CSRF, or server validation.

## Core Design

### 1. Annotation-driven action manifest (server owned)
Templates are the source of truth for *available* actions via annotations such as:
- `data-ai-action-id`
- `data-ai-action-type`
- `data-ai-label`
- `data-ai-risk`
- `data-ai-confirm`
- `data-ai-action-role`
- optional `data-ai-args` JSON for argument schema overrides

Backend parses template annotations into a per-page manifest at runtime. Model output never defines capabilities.

### 2. Shared action-type schema map (server handlers)
Backend owns a shared map of supported `action_type` handlers. Each handler defines:
- default argument schema
- default risk
- default confirmation policy
- executable client action mapping

Annotated actions are executable only when their `data-ai-action-type` is present in this map.
If an annotation references an unknown `action_type`, SOBS logs a startup warning and marks that action as unimplemented.

### 3. Manifest endpoint
Expose a per-page manifest from backend (derived from template annotations + action-type map):
- used by frontend for local execution mapping and confirm UX
- used by prompt/tooling context so model sees what is actually available

### 4. Generic tool proposal contract
Model proposes one generic action tool call:
- tool: `propose_ui_action`
- args include `action_id`, optional `target_page`, `arguments`, and `notes`

Backend validates `action_id` and arguments against registry and emits normalized action events.

### 5. Execution policy
- `low` risk: may auto-execute if local page context is unambiguous.
- `medium/high`: explicit user confirmation required.
- unsupported/unimplemented action ids: never execute; show explanatory status.

### 6. Security controls
- Allowlist-only actions.
- Argument schema validation and length limits.
- No arbitrary selectors from model.
- Existing auth/CSRF/session checks remain mandatory.
- Guard check still required for state-changing action proposals.

### 7. Truthful assistant behavior
Assistant must not claim action completion unless execution event confirms success.

## Telemetry
Reuse OTEL `gen_ai` events with:
- `gen_ai.chat_id`
- `gen_ai.turn_id`
- `gen_ai.tool.name`
- `sobs.ai.action_id`
- `sobs.ai.action.status` (`proposed`, `executed`, `failed`, `unsupported`)

## Initial implementation slice
1. Add annotation parser and manifest endpoint.
2. Add generic `propose_ui_action` parsing path.
3. Keep existing `apply_sql_filter` tool for compatibility.
4. Map generic `logs.filter.apply_sql` -> existing SQL filter action.
5. Execute actions only through signed server-authorized tokens.

## Planned follow-up
- Full argument schema enforcement per action.
- Broader page coverage (traces/metrics/rules/settings).
- Generic backend execute endpoint for server-authorized actions.
- Stronger UI annotation extraction for maintainability tooling.
