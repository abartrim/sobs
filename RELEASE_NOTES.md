# Release Notes

## Unreleased

### Added

- Logs page Query Statistics panel now supports query-scoped summaries (all matching rows across the active query, not only the visible page).
- Added a manual Run advanced analysis action on the Logs page for deeper message intelligence.
- Advanced analysis includes repeated message pattern fingerprints, error-family clustering, top keywords, and optimization hints.
- Added Logs SQL assistant improvements: `/api/logs/field-hints`, `/api/logs/validate-filter`, `has_tag()` helper support, and persisted OTEL log-attribute key hints.
- Added saved Reports across filtered pages (Logs, Traces, Errors, Metrics, RUM, AI) with `/reports` UI and `/api/reports` CRUD API.
- Added Natural-Language Query page (`/query`) with NL→SQL generation, read-only SQL enforcement, schema endpoint, SQL re-run/refine flows, and add-to-dashboard integration.
- Added notification auto-make flow from Metrics Rules (preview + create) and per-rule quick action to generate matching notification rules.
- Added browser push VAPID key lifecycle endpoints and UI management (`/api/notifications/vapid-keygen`, `/api/notifications/vapid-keys`, `/api/notifications/vapid-public-key`).
- Added optional settings-at-rest encryption for sensitive app settings values via `SOBS_SETTINGS_ENCRYPTION_KEY` or `SOBS_SETTINGS_ENCRYPTION_KEY_FILE`.
- Added automated agent trigger execution from notification/anomaly rule checks for matching `anomaly_rule` and `tag_rule` agent rules.
- Added cluster-managed AI configuration overrides via env or file inputs for LLM, guard, and DLP settings (`SOBS_AI_*` and `SOBS_AI_*_FILE`).
- Added an Ollama-first local AI startup script (`scripts/start_ollama_ai_test.sh`) and updated docs to make local Ollama the default manual testing path.
- Added Prometheus/OTEL integration examples under `examples/prometheus/` and expanded `scripts/load_example.py` with Prometheus-style system metric families (CPU, memory, disk, network, filesystem/load).

### Changed

- Query statistics now stay consistent with grep filtering by evaluating stats on the same query result set used for rendering.
- AI guard enforcement is now fail-closed: missing guard config, guard call failures, and invalid guard responses block execution.
- Agent action handling now respects configured actions (analysis only runs when `analyze` is enabled).
- Agent issue-rate controls now clamp max issues per hour server-side and treat latest run activity consistently for cooldown checks.
- SOBS session cookie now defaults to `sobs_session` (`SOBS_SESSION_COOKIE_NAME`) to avoid name/path collisions with same-origin management cookies named `session`.
- Dashboard chart cards now use a dedicated SQL modal and renamed the source button label to **Data Source**.
- Filter bars were unified across major pages with consistent responsive layout and shared report-save modal UX.
- Light-mode contrast/readability fixes were applied across Errors/Traces stack-trace surfaces, chart-help/code panels, metrics rules surfaces, and legacy dark-table usage.

### Testing

- Added UI tests for query-scoped statistics behavior and manual advanced-analysis rendering.
- Added backend tests for guard fail-closed behavior, settings encryption/decryption, action gating, and automated agent trigger execution.
- Added backend tests validating AI settings env/file override precedence over DB-stored values.
- Added regression coverage for report save/apply flows, report JSON hydration hardening, and report dropdown XSS-safe rendering.
- Added tests for query page runner/route behavior, SQL safety enforcement, and query add-to-dashboard flows.
- Added tests for notifications browser push service-worker routing/registration and VAPID key management paths.
