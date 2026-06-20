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
- [roadmap.md](roadmap.md): Strategic vision, design principles, and the prioritized feature plan.

## Active Design Records

Forward-looking or partially-shipped designs with open work. Each carries a `> Status:` banner near the top; CI enforces banners on the planning/historical records it tracks.

- [design-airflow-parity.md](design-airflow-parity.md): Airflow-parity workstreams — current shipped subset in `airflow-parity.md`; this tracks the remaining workstreams.
- [design-event-triggers.md](design-event-triggers.md): HTTP webhook triggers (shipped, WS1) plus event-based routing and trigger chaining (proposed, WS2–WS3).
- [design-concurrency-priority.md](design-concurrency-priority.md): Concurrency strategies, rate limiting, and priority-based scheduling (proposed).
- [design-database-locking-fix.md](design-database-locking-fix.md): dqlite contention remediation (Phases 0–3 shipped) and the scale-out path.
- [design-scaling-job-execution.md](design-scaling-job-execution.md): Cluster-wide task-start throughput frontier on sharded dqlite.
- [design-incremental-execution.md](design-incremental-execution.md): Smart incremental execution and task caching (Phase 1 shipped; follow-on phases planned).
- [design-sla-management.md](design-sla-management.md): SLA deadline tracking, predictive completion estimates, and escalation chains (proposed).
- [design-task-templates.md](design-task-templates.md): Reusable, parameterized step templates (proposed).
- [design-data-plane-memory.md](design-data-plane-memory.md): The second-act substrate (digest pinning, decomposed-hash persistence, DAG versioning, lineage datasets, large-object passing) that makes the data-plane queryable — explain/reproduce/skip (proposed).

## Load Testing

- [load-testing-history.md](load-testing-history.md): Consolidated Phase 0 → Phase 2B distributed-execution load-test history (replaces the former per-run `load-baseline-*` series).

## Archive

Completed, shipped, or historical records that are no longer the active source of truth live under [`archive/`](archive/README.md): shipped design docs (ARM64 build support, Helm/Kubernetes deployment, internal mTLS auto-provisioning, parallel job execution), completed plans (job-definition reconciliation, UI implementation), the original feature brainstorm, and early architecture history.
