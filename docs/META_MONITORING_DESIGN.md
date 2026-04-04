# Meta-Monitoring Design (Who Watches the Watcher?)

## Scope and Assumptions

This document defines a practical meta-monitoring approach for this project.

Assumptions for this environment:
- Single-user operation.
- Embedded database (local storage, not a multi-node managed database).
- Preference for low operational complexity and low alert noise.

Because this deployment model is intentionally simple, the goal is not enterprise-grade NOC processes. The goal is confidence that observability is alive, data is fresh, and alerts are still deliverable.

## Goals

1. Detect when monitoring itself is degraded or broken.
2. Verify the full alerting path end-to-end, not just component health.
3. Keep the system lightweight for a single operator.
4. Prioritize actionable alerts over high cardinality/noise.

## Non-Goals

- Building large-team escalation workflows.
- Aggressive paging for every internal metric anomaly.
- Operating a complex distributed monitoring control plane.

## Recommended Control Layers

Use three layers together:

1. Local white-box health checks
- Process up/down.
- Scrape success.
- Rule evaluation success/latency.
- Notification send success/failure counters.

2. External black-box checks
- Probe key HTTP endpoints from outside the monitored process.
- Validate response code and latency budget.
- Include one probe from outside the host when possible.

3. Dead-man switch (watchdog)
- Keep one always-firing watchdog signal.
- Route watchdog to an independent receiver.
- Alert if watchdog signal is missing.

## Minimal Alert Set for Single-User + Embedded DB

Keep alerts few and high signal.

### A. Critical (immediate attention)

1. Watchdog missing
- Meaning: alert pipeline may be broken.
- Trigger: no watchdog heartbeat for N minutes.
- Action: validate alert manager route, credentials, outbound network.

2. Monitoring data stale
- Meaning: telemetry stopped flowing.
- Trigger: data freshness age exceeds threshold (for example, >5m for fast metrics).
- Action: verify collector/scraper process, endpoint availability, host resource pressure.

3. App unreachable (black-box)
- Meaning: user-visible observability UI/API is down.
- Trigger: repeated probe failures over a short window.
- Action: restart process, inspect logs, verify host/network.

### B. Important (same day)

4. Embedded DB growth risk
- Meaning: local storage may exhaust disk and cause full outage.
- Trigger: free disk below threshold or file growth slope indicates exhaustion within time horizon.
- Action: prune/retention adjustment, move data path, increase disk.

5. Query latency degradation
- Meaning: observability remains up but user workflow is impaired.
- Trigger: p95 query latency over threshold for sustained period.
- Action: reduce retention/cardinality, optimize slow query paths.

6. Rule evaluation failures
- Meaning: alerts may not represent true state.
- Trigger: rule eval errors > 0 for sustained period.
- Action: fix rule syntax/data source mismatch; validate reload state.

### C. Informational (weekly review)

7. Cardinality drift
- Meaning: metric dimensionality growth can degrade memory/perf.
- Trigger: steady growth trend in active series.
- Action: remove high-cardinality labels, normalize paths/IDs.

8. Alert noise trend
- Meaning: operator fatigue risk.
- Trigger: frequent flapping or recurring non-actionable alerts.
- Action: add windowing, tune thresholds, demote to ticket.

## Embedded DB-Specific Practices

For an embedded database, include these checks explicitly:

1. DB file size and growth rate
- Track current size and 24h growth.
- Estimate time-to-disk-full and alert before risk window.

2. Storage health
- Monitor filesystem free space and inode pressure.
- Alert on both absolute free space and rapid depletion.

3. Write/read error surface
- Count failed writes, lock/contention failures, corruption signals, and startup recovery events.

4. Backup/restore confidence
- Run periodic backup success heartbeat.
- Test restore on a schedule (at least monthly).
- Alert on backup missed/failed.

## Alert Delivery Design (Right-Sized)

Single-user does not mean single channel.
Use at least two channels:
- Primary: push/chat/incident channel.
- Secondary: email fallback.

For watchdog/dead-man signal, prefer an independent service path from your normal app path.

## Suggested SLOs for Monitoring Itself

These are starting targets, not strict requirements:

1. Data freshness SLO
- 99.9% of the time, key telemetry lag is < 2x scrape interval.

2. Alert pipeline SLO
- 99.9% of watchdog intervals are delivered within expected delay.

3. Observability UI/API availability SLO
- 99.5%+ from black-box probe perspective.

For single-user deployments, bias toward simpler thresholds and fewer alerts instead of complex burn-rate matrices unless noise requires it.

## Testing and Drill Cadence

1. Weekly quick check (5 minutes)
- Confirm watchdog currently firing and received.
- Confirm last successful backup heartbeat.
- Confirm data freshness graph has no prolonged gaps.

2. Monthly failure drill
- Intentionally break one stage (for example, notification credentials).
- Verify detection by watchdog-missing path.
- Validate runbook steps and recovery time.

## Runbook Skeleton

When a meta-monitoring alert fires:

1. Confirm symptom
- Is this a true detection or a stale/flapping condition?

2. Classify failure domain
- Collection, storage, rule eval, routing, notification, or external dependency.

3. Recover minimally
- Restore alert flow first, then optimize root cause.

4. Prevent recurrence
- Add one measurable guardrail (metric, test, or alert tuning).

## Initial Implementation Checklist

1. Add one always-on watchdog rule and route.
2. Add one independent heartbeat receiver for dead-man monitoring.
3. Add freshness alert for core metrics.
4. Add black-box probe for app endpoint.
5. Add disk + DB-file growth alerts.
6. Add backup success heartbeat and missed-backup alert.
7. Document a 1-page runbook for each critical alert.
8. Schedule monthly alert-pipeline drill.

## Why This Is Enough for This Deployment Model

For single-user + embedded DB, the biggest risks are silent failure, stale data, disk exhaustion, and broken notification delivery. The controls above directly target those failure modes while keeping operational overhead small.

If the system evolves to multi-user or distributed storage, expand this design with HA alert routing, multi-region probes, and stricter SLO burn-rate alerting.
