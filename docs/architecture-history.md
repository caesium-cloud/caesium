# Architecture History

This document consolidates early design notes that informed Caesium's direction. Treat it as historical context, not the current source of truth.

For current behavior, use:

- [job-definitions.md](job-definitions.md)
- [job-schema-reference.md](job-schema-reference.md)
- [job-definition-plan.md](job-definition-plan.md)
- [console.md](console.md)

## Core Design Intent

Early goals for Caesium emphasized:

- DAG-oriented job execution with explicit dependencies.
- Scheduled and on-demand triggering.
- Reproducibility via versioned configs and containerized execution.
- Low operational overhead and strong CLI ergonomics.
- Runtime observability (status, failures, and execution metrics).
- Extensibility for runtimes, triggers, and post-run integrations.

## Primitive Model (Early Terminology)

- `Job`: A DAG of work units grouped under shared triggering context.
- `Trigger`: A schedule or event source that starts a job.
- `Task`: A unit of executable work inside a job.
- `Atom`: The runtime environment backing task execution (for example Docker, Podman, Kubernetes).
- `Callback`: A post-run action with execution metadata.

Originally discussed callback variants included notifications, alerts, and webhooks. Current callback behavior is documented in `docs/job-definitions.md`.

## Dependency Strategy (Project Principles)

The original dependency guidance still maps well to current engineering practice:

1. Prefer external dependencies when quality is high.
2. Favor dependencies with active maintenance.
3. Build in-house only when it creates clear product or operational advantage.

Plugin adoption was intended to follow a "prove externally, graduate internally" model:

- Incubate niche integrations outside the core repository.
- Adopt into core only after clear demand and maintenance confidence.

## Scheduler Landscape Notes

Early comparison work focused on tradeoffs:

- Cron: simple and ubiquitous, but weak DAG and versioning ergonomics.
- Airflow and Luigi: mature DAG ecosystems with heavier operational footprint.
- Kubernetes-native systems: strong for cluster-native execution, but with orchestration prerequisites.
- Specialized systems (for example Hadoop-focused tooling): less suitable for general-purpose workloads.

These comparisons influenced Caesium's focus on container-native DAG execution with simple local workflows.
