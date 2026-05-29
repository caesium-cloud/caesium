# Archived Documentation

These records are retained for historical and design-rationale context. They are **not** the current source of truth — each describes work that has shipped, a plan that completed, or an idea that has since graduated to the [roadmap](../roadmap.md). For current behavior, follow the "live successor" link on each entry.

## Shipped design records

- [design-arm64-support.md](design-arm64-support.md) — Multi-architecture (amd64/arm64) build and CI infrastructure. **Shipped.** Live behavior: root `README.md`, `justfile`, and CI config.
- [design-helm-kubernetes-deployment.md](design-helm-kubernetes-deployment.md) — Helm chart design (StatefulSet peer discovery, headless services for dqlite RAFT, health probes, kind CI). **Shipped.** Live successor: [kubernetes-deployment.md](../kubernetes-deployment.md) and `helm/caesium/`.
- [design-parallel-job-execution.md](design-parallel-job-execution.md) — Single-node worker pool plus distributed task-claiming design (Phases 1–3). **Shipped.** Live successor: [parallel-execution-operations.md](../parallel-execution-operations.md).
- [design-internal-mtls-auto-provisioning.md](design-internal-mtls-auto-provisioning.md) — Zero-operator-effort internal mTLS via catalog-mediated, leader-signed CA enrollment (PR #181). **Shipped.** Live behavior: `internal/dispatch/pki/`.

## Completed plans

- [job-definition-plan.md](job-definition-plan.md) — Job-definition system implementation plan (Phases 0–4: schema, importer, Git sync, DAG execution, reconciliation/prune). **Largely implemented.** Live successors: [job-definitions.md](../job-definitions.md), [job-schema-reference.md](../job-schema-reference.md).
- [ui_implementation_plan.md](ui_implementation_plan.md) — Embedded Console v1 scope and the 2026-04 UI refresh (PRs #146–#148). **Closed.** Live behavior: the embedded UI under `ui/`.

## Historical context

- [brainstorm-differentiators.md](brainstorm-differentiators.md) — The original "killer features beyond Airflow parity" idea backlog. Every idea here has shipped, graduated to a dedicated design doc, or been parked. Superseded by [roadmap.md](../roadmap.md).
- [architecture-history.md](architecture-history.md) — Early architectural intent, primitive model, and the scheduler-landscape rationale that shaped Caesium's direction.

See also [../load-testing-history.md](../load-testing-history.md) for the consolidated distributed-execution load-test record (formerly the `load-baseline-*` series).
