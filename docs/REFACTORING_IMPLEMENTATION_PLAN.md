# Code Refactoring Implementation Plan: Monolithic to Modular

**Date:** April 4, 2026  
**Owner:** Engineering Team  
**Status:** Pre-Implementation Planning  
**Review Required Before Merge:** Yes (validation tests must pass)

---

## Executive Summary

This document outlines the refactoring of `app.py` (currently 19k lines) into a modular architecture designed to maximize **LLM effectiveness, maintainability, and testability** without sacrificing cohesion.

**Key Principle:** Vertical domain slicing (feature-based organization) rather than horizontal layering (all models, all routes, etc.).

**Expected Outcome:**
- 7 focused modules replacing 1 monolithic file
- ~500-800 lines per file (vs 19k in one)
- Zero circular dependencies maintained
- **LLM effectiveness validated via real feature-addition tests**

---

## Current State

| Metric | Value |
|--------|-------|
| **File Size** | 19,000 lines |
| **Sections** | 7 major logical domains |
| **Dependencies** | All internal; acyclic |
| **Test Coverage** | Good (conftest.py + test_app.py) |
| **LLM Effectiveness** | High (single context) but limited by file size |
| **Maintenance Pain Points** | Navigation, context switching, large diffs |

---

## Goals

1. **Preserve LLM Effectiveness:** Maintain dependency clarity and logical cohesion
2. **Improve Maintainability:** Clear boundaries, focused files, easier to navigate
3. **Enable Scaling:** New features should follow obvious patterns
4. **Reduce Cognitive Load:** Developers shouldn't need to load entire 19k-line file
5. **Validate Empirically:** Measure LLM performance on refactored code vs original

---

## Target Architecture

```
sobs/
├── app.py                    # ~1,500 lines: Quart app, route registration
├── config.py                 # ~350 lines: Environment, settings, feature flags
│
├── database/
│   ├── __init__.py          # Re-exports for clean imports
│   ├── connection.py         # ~250 lines: ChDbConnection class
│   ├── schema.py             # ~450 lines: Table & view definitions
│   └── queries.py            # ~350 lines: Common query builders
│
├── shared/
│   ├── __init__.py
│   ├── auth.py              # ~120 lines: API key, Basic Auth decorators
│   ├── utils.py             # ~250 lines: Timestamp, parsing, encoding
│   ├── events.py            # ~120 lines: LogEvent, SpanEvent, ErrorEvent, etc.
│   ├── serialization.py      # ~180 lines: JSON handlers, compression
│   └── streaming.py          # ~130 lines: SSE pub/sub, websocket helpers
│
├── ai/
│   ├── __init__.py
│   ├── llm.py               # ~450 lines: OpenAI-compatible integration, streaming
│   ├── guards.py            # ~350 lines: Llama Guard 3, prompt injection, classifiers
│   ├── memory.py            # ~250 lines: Semantic memory, consolidation
│   ├── actions.py           # ~180 lines: UI action tokens, tool calls
│   └── settings.py          # ~120 lines: AI config persistence
│
├── features/
│   ├── __init__.py
│   ├── ingest.py            # ~700 lines: OTLP, RUM, error endpoints
│   ├── logs.py              # ~450 lines: Log query, filtering, tag rules
│   ├── traces.py            # ~350 lines: Trace query, analysis
│   ├── metrics.py           # ~450 lines: Metric query, anomaly detection
│   ├── errors.py            # ~250 lines: Error tracking
│   ├── rum.py               # ~350 lines: RUM ingestion, assets, tokens
│   └── dashboards.py        # ~350 lines: Custom dashboard CRUD
│
└── agents/
    ├── __init__.py
    ├── rules.py             # ~250 lines: Rule definitions, evaluation
    ├── executor.py          # ~250 lines: Agent flow execution
    └── dlp.py               # ~130 lines: DLP integration

tests/
├── test_app.py              # Main app tests
├── test_config.py           # Config encryption
├── test_database/
│   ├── test_connection.py
│   ├── test_schema.py
│   └── test_queries.py
├── test_ai/
│   ├── test_llm.py
│   ├── test_guards.py
│   ├── test_memory.py
│   └── test_settings.py
├── test_features/
│   ├── test_ingest.py
│   ├── test_logs.py
│   ├── test_traces.py
│   ├── test_metrics.py
│   ├── test_errors.py
│   ├── test_rum.py
│   └── test_dashboards.py
├── test_agents/
│   ├── test_rules.py
│   └── test_executor.py
└── conftest.py              # Shared pytest fixtures
```

---

## Implementation Phases

### Phase 1: Foundation

**Goal:** Extract leaf modules with no interdependencies on new code.

**Checklist:**
- [ ] Create directory structure (`database/`, `shared/`, `ai/`, `features/`, `agents/`)
- [ ] Extract `config.py` (env vars, encryption, feature flags)
- [ ] Extract `shared/utils.py` (timestamp, parsing, encoding helpers)
- [ ] Extract `shared/events.py` (LogEvent, SpanEvent dataclasses)
- [ ] Extract `shared/auth.py` (API key, Basic Auth decorators)
- [ ] Extract `shared/serialization.py` (JSON, compression)
- [ ] Update imports in `app.py`
- [ ] Run all existing tests (should pass without changes)
- [ ] Verify no circular imports: `python -m modulefinder app.py`

**Success Criteria:**
- All tests pass
- No circular imports
- app.py reduced to ~15k lines

---

### Phase 2: Database Layer

**Goal:** Isolate data persistence logic.

**Checklist:**
- [ ] Extract `database/connection.py` (ChDbConnection class only)
- [ ] Extract `database/schema.py` (table/view definitions)
- [ ] Extract `database/queries.py` (common query builders)
- [ ] Create `database/__init__.py` with clean exports:
  ```python
  from .connection import ChDbConnection
  from .schema import SCHEMA, Tables
  from .queries import (
      build_log_query,
      build_metric_query,
      # ...
  )
  __all__ = ['ChDbConnection', 'SCHEMA', 'Tables', ...]
  ```
- [ ] Update `app.py` to import from `database`
- [ ] Update all feature code imports
- [ ] Run tests

**Success Criteria:**
- Database module is fully independent
- All schema definitions in one place
- app.py reduced to ~12k lines

---

### Phase 3: AI Subsystem

**Goal:** Extract LLM, guards, memory, and AI-specific logic.

**Checklist:**
- [ ] Extract `ai/llm.py` (endpoint calls, streaming, model configuration)
- [ ] Extract `ai/guards.py` (Llama Guard, prompt injection, classifiers)
- [ ] Extract `ai/memory.py` (embeddings, memory consolidation, chat history)
- [ ] Extract `ai/actions.py` (UI action tokens, tool call handling)
- [ ] Extract `ai/settings.py` (AI setting persistence)
- [ ] Create `ai/__init__.py` with clean exports
- [ ] Verify AI module has no feature dependencies (only shared + database)
- [ ] Create `tests/test_ai/` with focused unit tests
- [ ] Run full test suite

**Success Criteria:**
- AI module can be understood independently
- No circular dependencies with features
- app.py reduced to ~8-10k lines

---

### Phase 4: Feature Domains

**Goal:** Extract route handlers grouped by domain.

**Checklist:**
- [ ] Extract `features/ingest.py` (OTLP logs, traces, metrics; RUM; errors)
- [ ] Extract `features/logs.py` (log queries, search, tag rules)
- [ ] Extract `features/traces.py` (trace queries, span analysis)
- [ ] Extract `features/metrics.py` (metric queries, anomaly views)
- [ ] Extract `features/errors.py` (error tracking, fingerprinting)
- [ ] Extract `features/rum.py` (RUM ingestion, assets, client tokens)
- [ ] Extract `features/dashboards.py` (dashboard CRUD, rendering)
- [ ] Create `features/__init__.py`
- [ ] Update `app.py` to register routes:
  ```python
  from features import ingest, logs, traces, metrics, errors, rum, dashboards
  
  def setup_routes(app):
      ingest.setup_routes(app)
      logs.setup_routes(app)
      traces.setup_routes(app)
      # ... etc
  ```
- [ ] Create comprehensive `tests/test_features/` tests
- [ ] Run full test suite

**Success Criteria:**
- Each feature module is focused and testable
- Route registration is clear
- app.py is ~2-3k lines (pure orchestration)

---

### Phase 5: Agent Framework

**Goal:** Extract rule execution and agent logic.

**Checklist:**
- [ ] Extract `agents/rules.py` (rule definitions, evaluation logic)
- [ ] Extract `agents/executor.py` (agent flow: guard → LLM → action)
- [ ] Extract `agents/dlp.py` (DLP integration, data classification)
- [ ] Create `agents/__init__.py`
- [ ] Move agent-related tests to `tests/test_agents/`
- [ ] Run full test suite

**Success Criteria:**
- Agent logic is independent
- Rules engine is testable
- No unexpected imports between modules

---

### Phase 6: Testing & Validation

**[See "Validation Testing Strategy" section below]**

---

## Validation Testing Strategy: GitHub Copilot Feature Addition Experiment

This is the **critical validation step** that determines if refactoring actually improves LLM effectiveness.

### Objective
Measure LLM (specifically GitHub Copilot) effectiveness at adding **the same feature** to:
1. **Original monolithic format** (19k line app.py)
2. **New modular format** (7+ focused modules)

### Test Feature: "Add Custom Alert Severity Levels"

**Feature Scope:** Allow users to define 3 custom alert severity levels (currently hardcoded to 5 basic levels), store in DB, and use in anomaly rules.

**Why This Feature?**
- Touches multiple subsystems (schema, rules, UI, API)
- Not in current codebase (requires genuine extension)
- Clear success criteria (does alert rule work end-to-end?)
- Moderate complexity (~1-2 hour feature in ideal conditions)

### Pre-Refactoring Test: Original Format

**Setup:**
```bash
# Create feature branch from current main
git checkout -b copilot-test/alert-levels-original
```

**Copilot Prompt (sent to GitHub Copilot in VS Code):**
```
Feature Request: Add custom alert severity levels

Currently, alerts are hardcoded to 5 severity levels (Critical, High, Medium, Low, Info).
We need to allow users to define up to 10 custom severity levels.

Requirements:
1. Add severity levels table to ChDB schema
2. Add UI endpoints to CRUD severity levels (list, create, update, delete)
3. Update anomaly rule evaluation to use custom levels
4. Add API endpoint GET /api/alert-severity-levels
5. Update rule creation form to select from custom levels

Where should the code go? Help me implement this end-to-end.
```

**Measurement Points:**
- [ ] Number of Copilot prompts needed to complete feature
- [ ] Final code quality (follows patterns, type hints, error handling)
- [ ] Test coverage (can you add tests along the way?)
- [ ] Bugs/issues found in integration testing
- [ ] Time to completion
- [ ] Correctness (feature works as specified)

**Acceptance Criteria:**
- `GET /api/alert-severity-levels` returns list of custom levels
- Anomaly rules can reference custom levels
- UI renders severity selector with custom options
- All existing tests still pass
- New code includes proper error handling

---

### Post-Refactoring Test: New Modular Format

**Setup:**
```bash
# After refactoring complete
git checkout -b copilot-test/alert-levels-refactored
```

**Same Copilot Prompt** (above)

**Measurement Points:** (same as above)

---

### Comparative Analysis

**Success Metrics:**

| Metric | Weight | How to Measure |
|--------|--------|----------------|
| **Prompts Required** | 25% | Count manual follow-up prompts needed |
| **Code Correctness** | 25% | Does feature work? Integration test pass rate |
| **Pattern Recognition** | 20% | Does Copilot follow existing patterns? (schema format, route structure, error handling) |
| **Test Coverage** | 15% | Can Copilot add tests alongside implementation? |
| **Time to Completion** | 15% | Total elapsed time from first prompt to working feature |

**Expected Outcome (Hypothesis):**
- Original format: 5-8 prompts, ~2-3 hours, 70% pattern adherence
- Refactored format: 2-3 prompts, ~45-60 min, 90% pattern adherence

**Success Criteria for Refactoring:**
- Refactored version requires **≤50% of prompts** vs original
- Refactored version has **≥85% pattern adherence** vs original
- Same feature works correctly in both versions
- Refactored code is **more readable/maintainable**

---

## Implementation Test Plan

### Before Refactoring Begins

**Baseline Testing:**
```bash
# 1. Run existing test suite
pytest tests/ -v --cov

# 2. Verify app starts correctly
python -m hypercorn app:app --bind 127.0.0.1:5000

# 3. Test core endpoints (smoke test)
curl http://localhost:5000/api/health
curl http://localhost:5000/api/logs?query=...
```

**Record:**
- [ ] Test pass rate: _____
- [ ] Startup time: _____ seconds
- [ ] Response time for key endpoints: _____ ms

---

### During Each Phase

**After Each Phase Completion:**
```bash
# Ensure app still runs
python -c "from app import app; print('Import successful')"

# Run tests
pytest tests/ -v

# Check for circular imports
python -m modulefinder app.py 2>&1 | grep -i circular

# Type check (if configured)
mypy app.py database/ shared/ ai/ features/ agents/
```

---

### Post-Refactoring

**Comprehensive Validation:**

```bash
# 1. All tests pass
pytest tests/ -v --cov

# 2. App startup unchanged
time python -m hypercorn app:app --bind 127.0.0.1:5000 &
sleep 2
curl http://localhost:5000/api/health
kill %1

# 3. No regressions on key features
. tests/regression_test.sh

# 4. Code quality metrics
pylint database/ shared/ ai/ features/ agents/ --disable=all --enable=E,F
black --check .
isort --check-only .
mypy database/ shared/ ai/ features/ agents/ --ignore-missing-imports
```

---

### Feature Addition Experiment

**Steps:**

| Step | Owner |
|------|-------|
| Setup original test branch | Engineer A |
| Test feature on original format | Copilot + Engineer A |
| Merge refactored code to feature branch | Engineer B |
| Test feature on refactored format | Copilot + Engineer C |
| Comparative analysis | Engineer A |
| Document findings | Engineering team |

**Go/No-Go Decision Gate:**
- If refactored version is **≥20% more effective** (prompts, time, correctness), proceed to merge
- If original is **≥20% more effective**, roll back refactoring or redesign approach
- If **~equal**, refactoring still recommended for maintainability (less urgent)

---

## Risk Management

### Risk: Breaking Changes

**Mitigation:**
- Run full test suite after each phase
- No code changes to business logic during refactoring (pure extraction)
- Keep same function signatures; deprecate old names if needed

**Rollback Plan:**
```bash
git revert <commit-hash>
# Full test suite should pass
```

### Risk: Import Errors

**Mitigation:**
- Use `python -m modulefinder` to detect circular imports
- Add explicit `__all__` to each module's `__init__.py`
- Test imports explicitly: `python -c "from module import Symbol"`

### Risk: Performance Regression

**Mitigation:**
- Benchmark startup time before/after
- Benchmark query execution (metrics, logs, traces)
- Monitor memory usage

**Success Criteria:** No >5% regression in any metric

### Risk: LLM Effectiveness Validation is Inconclusive

**Mitigation:**
- Use **multiple features** (not just one) if time allows
- Measure with **multiple LLMs** (Copilot, Claude in context, etc.)
- Include both happy-path and edge-case implementations

---

## Success Criteria (Go/No-Go for Merge)

| Category | Criteria | Status |
|----------|----------|--------|
| **Correctness** | All existing tests pass, zero regressions | [ ] |
| **No Breaking Changes** | API routes unchanged, same response format | [ ] |
| **Code Quality** | No new type errors, linting passes, no circular imports | [ ] |
| **Modularity** | Largest module ≤800 lines, average ~500 lines | [ ] |
| **Documentation** | Module docstrings added, import patterns clear | [ ] |
| **LLM Effectiveness** | Refactored ≥20% more effective OR equal with better maintainability | [ ] |
| **Maintainability** | Code review confirms improved readability/clarity | [ ] |
| **Performance** | No >5% regression in startup or query latency | [ ] |

**Merge Approval:** All boxes checked + code review approval

---



---

## Documentation Updates

**Required:**

- [ ] Update `CONTRIBUTING.md` with new module layout
- [ ] Add module docstrings to each new `__init__.py`
- [ ] Create `ARCHITECTURE.md` describing module organization and dependencies
- [ ] Update imports in existing documentation
- [ ] Add examples of how to add new features (by domain)

**Example: `ARCHITECTURE.md` outline**
```markdown
# Architecture Overview

## Module Organization

- `database/` — ClickHouse schema, connection management
- `shared/` — Utilities, auth, common data structures
- `ai/` — LLM integration, guards, memory
- `features/` — Domain logic (logs, traces, metrics, etc.)
- `agents/` — Rule execution and automation

## Dependency Graph

[ASCII diagram showing module dependencies]

## Adding a New Feature

1. Determine which domain it belongs to
2. Add code to `features/<domain>.py`
3. Add tests to `tests/test_features/test_<domain>.py`
4. Update imports in `app.py` if adding new routes
5. Run full test suite
```

---

## Monitoring Post-Merge

**After refactoring is merged to `main`:**

1. **Monitor Issues:** Watch for reports of confusing imports or hard-to-find code
2. **Monitor Performance:** Track metrics for regressions in production
3. **Gather Feedback:** Ask team (and Copilot users) about experience after 2 weeks
4. **Iteration:** Be prepared to adjust module boundaries if feedback suggests changes

---

## Appendix A: Module Dependency Graph

```
                    app.py
                      |
        ══════════════╬══════════════
        |             |              |
     config.py   [route setup]  [middleware]
        |
      ╔═════════════════════════════════════════════╗
      ║           Shared Infrastructure             ║
      ║  (auth, utils, events, serialization, etc)  ║
      ╚═════════════════════════════════════════════╝
        |
      ╔═════════════════════════════════════════════╗
      ║       Database Layer (connection, schema)   ║
      ╚═════════════════════════════════════════════╝
        |
      ╔═════════════════════════════════════════════╗
      ║              AI Subsystem                   ║
      ║   (llm, guards, memory, actions, settings)  ║
      ╚═════════════════════════════════════════════╝
        |
      ╔════════════════════════════════════════════════════════════╗
      ║           Feature Domains                               ║
      ║  (ingest, logs, traces, metrics, errors, rum, dashboards) ║
      ╚════════════════════════════════════════════════════════════╝
        |
      ╔═════════════════════════════════════════════╗
      ║            Agent Framework                 ║
      ║    (rules, executor, dlp)                  ║
      ╚═════════════════════════════════════════════╝

KEY:
- Arrows point downward (dependencies)
- Acyclic — no module depends on anything above it
- ✓ LLM can understand each layer independently
```

---

## Appendix B: Copilot Testing Setup Script

```bash
#!/bin/bash
# refactor_test.sh — Automated setup for feature addition testing

set -e

echo "=== Pre-Refactoring Test Setup ==="
git checkout -b copilot-test/alert-levels-original
git log -1 --oneline

echo ""
echo "=== Baseline Information ==="
wc -l app.py
find . -name "*.py" -type f | grep -v __pycache__ | sort

echo ""
echo "NEXT STEPS:"
echo "1. Open VS Code"
echo "2. Use GitHub Copilot to add 'Custom Alert Severity Levels' feature"
echo "3. Record number of prompts, time elapsed, and final code quality"
echo "4. Run: pytest tests/ -v"
echo "5. Test endpoints: curl http://localhost:5000/api/alert-severity-levels"
echo ""
echo "When done, commit and wait for refactoring to complete on separate branch."
```

---

## Approval Sign-Off

- [ ] Engineering Lead: Approval to begin Phase 1
- [ ] Architecture Review: Approval after Phase 3 (AI module complete)
- [ ] Full Team: Review findings from Phase 6 (LLM testing)
- [ ] Release Manager: Approval for merge to main (all gates pass)

---

**Next Steps:**
1. Review this plan with team
2. Assign Phase 1 tasks
3. Set up git branches for parallel work
4. Schedule weekly sync meetings
5. Begin Phase 1 implementation
