# Archived documentation

These records are kept for history and design rationale. They are **not** the
current source of truth: each describes work that shipped, a plan that
completed, or evidence for a change that has since merged. For current
behavior follow the "live successor" link on each entry. Everything in this
directory is indexed here; a guardrail test enforces that.

## Shipped design records

- [design-arm64-support.md](design-arm64-support.md) — Multi-architecture (amd64/arm64) build and CI infrastructure. **Shipped.** Live behavior: root `README.md`, `justfile`, and CI config.
- [design-helm-kubernetes-deployment.md](design-helm-kubernetes-deployment.md) — Helm chart design (StatefulSet peer discovery, headless services for dqlite Raft, health probes, kind CI). **Shipped.** Live successor: [kubernetes-deployment.md](../kubernetes-deployment.md) and `helm/caesium/`.
- [design-parallel-job-execution.md](design-parallel-job-execution.md) — Single-node worker pool plus distributed task-claiming design (Phases 1–3). **Shipped.** Live successor: [distributed-execution.md](../distributed-execution.md).
- [design-sso-authentication.md](design-sso-authentication.md) — Native SSO (OIDC, SAML, LDAP) design: providers, dqlite sessions, CSRF, declarative group-to-role mapping (PRs #192–#203). **Shipped; superseded** as the design of record by [2026-09-16-identity-and-access-design.md](../design/2026-09-16-identity-and-access-design.md). Live successor: [sso-authentication.md](../sso-authentication.md).
- [design-internal-mtls-auto-provisioning.md](design-internal-mtls-auto-provisioning.md) — Zero-operator-effort internal mTLS via catalog-mediated, leader-signed CA enrollment (PR #181). **Shipped.** Live behavior: `internal/dispatch/pki/`.

## Completed plans

- [job-definition-plan.md](job-definition-plan.md) — Job-definition system implementation plan (Phases 0–4: schema, importer, Git sync, DAG execution, reconciliation/prune). **Largely implemented.** Live successors: [job-definitions.md](../job-definitions.md), [job-schema-reference.md](../job-schema-reference.md).
- [2026-05-27-sso-foundation-plan.md](2026-05-27-sso-foundation-plan.md) — The step-by-step implementation plan for the SSO foundation. **Completed.** Live successor: [sso-authentication.md](../sso-authentication.md).
- [ui_implementation_plan.md](ui_implementation_plan.md) — Embedded Console v1 scope and the 2026-04 UI refresh (PRs #146–#148). **Closed.** Live behavior: the embedded UI under `ui/`.

## Evidence records

- [operator-console-facelift-2026-10.md](operator-console-facelift-2026-10.md) — Design decisions, data-availability limits, finding dispositions, and qualification runs for the Standard operator console facelift (PR #617) and its motion follow-up (PR #619). Live behavior: `ui/`.
- [load-testing-history.md](load-testing-history.md) — The consolidated Phase 0 to Phase 2B distributed-execution load-test record (May 2026), formerly the `load-baseline-*` series. Live successors: [distributed-execution.md](../distributed-execution.md) and [design/scaling-job-execution.md](../design/scaling-job-execution.md).

## Historical context

- [brainstorm-differentiators.md](brainstorm-differentiators.md) — The original "killer features beyond Airflow parity" idea backlog. Every idea here has shipped, graduated to a design record, or been parked. Superseded by [roadmap.md](../roadmap.md).
- [architecture-history.md](architecture-history.md) — Early architectural intent, the primitive model, and the scheduler-landscape rationale that shaped Caesium's direction.
