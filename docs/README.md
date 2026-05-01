# Caesium Documentation

This directory mixes current-source operator documentation with design records and historical planning notes. Use the sections below to distinguish what describes the product today versus what is future-looking context.

## Current Source of Truth

- [job-definitions.md](job-definitions.md): Authoring, linting, diffing, schema tooling, Git sync, and operational controls for job manifests.
- [caesium-job-llm-reference.md](caesium-job-llm-reference.md): LLM authoring guide plus executable harness scenario format, including metrics and OpenLineage assertions.
- [job-schema-reference.md](job-schema-reference.md): Generated schema reference from `pkg/jobdef`.
- [backfill.md](backfill.md): Backfill behavior across API, CLI, and UI.
- [parallel-execution-operations.md](parallel-execution-operations.md): Distributed execution configuration, rollout, and troubleshooting.
- [open_lineage.md](open_lineage.md): OpenLineage configuration, transports, and observability.
- [kubernetes-deployment.md](kubernetes-deployment.md): Deploying Caesium to Kubernetes with Helm.
- [airflow-parity.md](airflow-parity.md): Implemented Airflow-style authoring and operator semantics.
- [examples/](examples/): Example job manifests used by docs and conformance tests.

## UI and Operator Surface

- [ui_implementation_plan.md](ui_implementation_plan.md): Historical record of shipped UI feature scope (v1 plan + 2026-04 refresh).
- [backfill.md](backfill.md): Jobs view backfill behavior and cancellation semantics.
- [parallel-execution-operations.md](parallel-execution-operations.md): Worker inspection and DAG attribution surfaces.

## Design Records and Roadmaps

These files are useful context, but each should be treated according to its status banner. CI enforces that these docs keep an explicit `> Status:` banner near the top.

- [roadmap.md](roadmap.md): Strategic vision and feature priorities.
- [job-definition-plan.md](job-definition-plan.md)
- [design-airflow-parity.md](design-airflow-parity.md)
- [design-arm64-support.md](design-arm64-support.md)
- [design-event-triggers.md](design-event-triggers.md)
- [design-concurrency-priority.md](design-concurrency-priority.md)
- [design-database-locking-fix.md](design-database-locking-fix.md)
- [design-sla-management.md](design-sla-management.md)
- [design-task-templates.md](design-task-templates.md)
- [design-helm-kubernetes-deployment.md](design-helm-kubernetes-deployment.md)
- [design-incremental-execution.md](design-incremental-execution.md)
- [design-parallel-job-execution.md](design-parallel-job-execution.md)
- [brainstorm-differentiators.md](brainstorm-differentiators.md)

## Historical Notes

- [architecture-history.md](architecture-history.md): Early architectural intent and tradeoffs.
