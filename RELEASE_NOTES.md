# Release Notes

## Unreleased

### Added

- Logs page Query Statistics panel now supports query-scoped summaries (all matching rows across the active query, not only the visible page).
- Added a manual Run advanced analysis action on the Logs page for deeper message intelligence.
- Advanced analysis includes repeated message pattern fingerprints, error-family clustering, top keywords, and optimization hints.
- Added optional settings-at-rest encryption for sensitive app settings values via `SOBS_SETTINGS_ENCRYPTION_KEY` or `SOBS_SETTINGS_ENCRYPTION_KEY_FILE`.
- Added automated agent trigger execution from notification/anomaly rule checks for matching `anomaly_rule` and `tag_rule` agent rules.
- Added cluster-managed AI configuration overrides via env or file inputs for LLM, guard, and DLP settings (`SOBS_AI_*` and `SOBS_AI_*_FILE`).
- Added an Ollama-first local AI startup script (`scripts/start_ollama_ai_test.sh`) and updated docs to make local Ollama the default manual testing path.

### Changed

- Query statistics now stay consistent with grep filtering by evaluating stats on the same query result set used for rendering.
- AI guard enforcement is now fail-closed: missing guard config, guard call failures, and invalid guard responses block execution.
- Agent action handling now respects configured actions (analysis only runs when `analyze` is enabled).
- Agent issue-rate controls now clamp max issues per hour server-side and treat latest run activity consistently for cooldown checks.

### Testing

- Added UI tests for query-scoped statistics behavior and manual advanced-analysis rendering.
- Added backend tests for guard fail-closed behavior, settings encryption/decryption, action gating, and automated agent trigger execution.
- Added backend tests validating AI settings env/file override precedence over DB-stored values.
