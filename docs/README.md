# Caesium Documentation

This directory separates current-source operator documentation from forward-looking design records. Completed, shipped, superseded, and historical records have been moved out of the active set into [`archive/`](archive/README.md).

## Current Source of Truth

- [job-definitions.md](job-definitions.md): Authoring, linting, diffing, schema tooling, Git sync, and operational controls for job manifests.
- [caesium-job-llm-reference.md](caesium-job-llm-reference.md): LLM authoring guide plus executable harness scenario format, including metrics and OpenLineage assertions.
- [job-schema-reference.md](job-schema-reference.md): Generated schema reference from `pkg/jobdef`.
- [backfill.md](backfill.md): Backfill behavior across API, CLI, and UI.
- [parallel-execution-operations.md](parallel-execution-operations.md): Distributed execution configuration, rollout, and troubleshooting.
- [sso-authentication.md](sso-authentication.md): Native OIDC, SAML, and LDAP SSO configuration.
- [database-sharding.md](database-sharding.md): Phase 4 database shard layout, routing contract, and constraints.
- [open_lineage.md](open_lineage.md): OpenLineage configuration, transports, and observability.
- [kubernetes-deployment.md](kubernetes-deployment.md): Deploying Caesium to Kubernetes with Helm.
- [airflow-parity.md](airflow-parity.md): Implemented Airflow-style authoring and operator semantics.
- [examples/](examples/): Example job manifests used by docs and conformance tests.

## Strategy & Roadmap

- [differentiation-strategy.md](differentiation-strategy.md): Positioning thesis — the sovereignty-led funnel, why Caesium wins by constraint not comparison, and the kill-conditions that test it.
- [sovereignty.md](sovereignty.md): Sovereignty proof-points — free vs. paywalled feature comparison (HA, RBAC, SSO, audit, lineage vs. Dagster+/Kestra Enterprise/Prefect Cloud) and a zero-dependency / air-gapped quickstart.
- [roadmap.md](roadmap.md): Strategic vision, design principles, and the prioritized feature plan.

## Active Design Records

Forward-looking or partially-shipped designs with open work. Each carries a `> Status:` banner near the top; CI enforces banners on the planning/historical records it tracks.

- [design-airflow-parity.md](design-airflow-parity.md): Airflow-parity workstreams — current shipped subset in `airflow-parity.md`; this tracks the remaining workstreams.
- [design-event-triggers.md](design-event-triggers.md): HTTP webhook triggers, event-based routing, and trigger chaining (WS1–WS3 shipped; reconciliation tracked in `exec-plans/completed/event-trigger-routing.md`).
- [design-concurrency-priority.md](design-concurrency-priority.md): Concurrency strategies, rate limiting, and priority-based scheduling (shipped; completed plan in `exec-plans/completed/concurrency-priority-queues.md`).
- [design-database-locking-fix.md](design-database-locking-fix.md): dqlite contention remediation (Phases 0–3 shipped) and the scale-out path.
- [design-scaling-job-execution.md](design-scaling-job-execution.md): Cluster-wide task-start throughput frontier on sharded dqlite.
- [design-incremental-execution.md](design-incremental-execution.md): Smart incremental execution and task caching (Phase 1 shipped; follow-on phases planned).
- [design-sla-management.md](design-sla-management.md): SLA deadline tracking, predictive completion estimates, and escalation chains (proposed).
- [design-task-templates.md](design-task-templates.md): Reusable, parameterized step templates (proposed).
- [design-data-plane-memory.md](design-data-plane-memory.md): The second-act substrate (digest pinning, decomposed-hash persistence, DAG versioning, lineage datasets, large-object passing) that makes the data-plane queryable — explain/reproduce/skip. Substrate shipped (streams A–D, #213–#222); the causal query verbs (`run diff`, quarantined `replay`, `blame`) shipped via the completed follow-on plan `exec-plans/completed/data-plane-memory-ii.md`, and those verbs (plus `why`, receipt/`verify`, and the cross-job lineage-impact graph) are now surfaced in the web UI via `exec-plans/completed/data-plane-memory-ui.md`.
- [design-quarantined-replay.md](design-quarantined-replay.md): Authoritative fail-closed safety model for quarantined replay in the data-plane-memory-ii plan.
- [design-agent-in-the-loop.md](design-agent-in-the-loop.md): Agent-in-the-loop ETL remediation — autonomous failure triage and bounded remediation via a container-native agent over the data-plane-memory primitives (proposed; exec plan `exec-plans/active/agent-in-the-loop-remediation.md`).
- [design-reproduce.md](design-reproduce.md): `caesium reproduce` — re-execute a single historical production task locally under Docker from its recorded execution descriptor (exact image digest, env, params, predecessor outputs), with secrets resolved locally or not at all (proposed; exec plan `exec-plans/active/reproduce.md`).
- [design-freshness-scheduling.md](design-freshness-scheduling.md): Freshness-driven scheduling — declare freshness SLOs on datasets and derive execution from lineage and data arrival instead of cron guesses (proposed; exec plan `exec-plans/active/freshness-scheduling.md`).
- [design-backtesting.md](design-backtesting.md): Pipeline backtesting — replay a code change over recorded production runs in quarantine and report output deltas before merge (proposed; exec plan `exec-plans/active/backtesting.md`).
- [design-contract-enforcement.md](design-contract-enforcement.md): Cross-job contract enforcement — schema-compatibility checks across producer/consumer jobs at lint/diff/apply time, with named consumers and an intentional-break path (proposed; exec plan `exec-plans/active/contract-enforcement.md`).
- [design-data-circuit-breaker.md](design-data-circuit-breaker.md): Data circuit breaker — statistical assertions on step outputs with dataset holds that stop bad data from propagating downstream (proposed; exec plan `exec-plans/active/data-circuit-breaker.md`).
- [design-resource-right-sizing.md](design-resource-right-sizing.md): Learned resource right-sizing — per-step memory/CPU recommendations from run history plus OOM retry escalation (proposed; exec plan `exec-plans/active/resource-right-sizing.md`).
- [design-dynamic-fanout.md](design-dynamic-fanout.md): Dynamic fan-out — runtime partition markers materialize data-proportional parallel task instances with per-partition caching (proposed; exec plan `exec-plans/active/dynamic-fanout.md`).
- [design-window-scheduling.md](design-window-scheduling.md): Deadline-window scheduling — run within a declared window, choosing the start via load/cost/carbon signals with a deadline-safe latest start (proposed; exec plan `exec-plans/active/window-scheduling.md`).
- `exec-plans/active/console-operator-loop-ux.md`: Operator-loop UX refinement of the job-detail, run-detail, and DAG surfaces (follow-up to the shipped Console v2 refresh; 4 UI-first streams).

## Load Testing

- [load-testing-history.md](load-testing-history.md): Consolidated Phase 0 → Phase 2B distributed-execution load-test history (replaces the former per-run `load-baseline-*` series).

## Archive

Completed, shipped, or historical records that are no longer the active source of truth live under [`archive/`](archive/README.md): shipped design docs (ARM64 build support, Helm/Kubernetes deployment, internal mTLS auto-provisioning, parallel job execution), completed plans (job-definition reconciliation, UI implementation), the original feature brainstorm, and early architecture history.
