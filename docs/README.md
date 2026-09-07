# Caesium Documentation

This directory separates current-source operator documentation from forward-looking design records. Completed, shipped, superseded, and historical records have been moved out of the active set into [`archive/`](archive/README.md).

## Current Source of Truth

- [job-definitions.md](job-definitions.md): Authoring, linting, diffing, schema tooling, Git sync, and operational controls for job manifests.
- [caesium-job-llm-reference.md](caesium-job-llm-reference.md): LLM authoring guide plus executable harness scenario format, including metrics and OpenLineage assertions.
- [job-schema-reference.md](job-schema-reference.md): Generated schema reference from `pkg/jobdef`.
- [backfill.md](backfill.md): Backfill behavior across API, CLI, and UI.
- [parallel-execution-operations.md](parallel-execution-operations.md): Distributed execution configuration, rollout, and troubleshooting.
- [ci.md](ci.md): CI runbook — required-to-merge vs. required-to-publish status checks, the job matrix, per-lane server env, and the release procedure for `v*` tags.
- [sso-authentication.md](sso-authentication.md): Native OIDC, SAML, and LDAP SSO configuration.
- [database-sharding.md](database-sharding.md): Phase 4 database shard layout, routing contract, and constraints.
- [open_lineage.md](open_lineage.md): OpenLineage configuration, transports, and observability.
- [reproduce.md](reproduce.md): Operator reference for `caesium reproduce` flags, exit codes, fidelity, image overrides, and local secret resolution.
- [kubernetes-deployment.md](kubernetes-deployment.md): Deploying Caesium to Kubernetes with Helm.
- [airflow-parity.md](airflow-parity.md): Implemented Airflow-style authoring and operator semantics.
- [infrastructure-deployment.md](infrastructure-deployment.md): Dependency-ordered Terraform (and other unit-pipeline binding) deployment via `cache.chain: values` and the `caesiumcloud/{git-source,tf-discover,tf-warm,tf-runner}` reagent images.
- [examples/](examples/): Example job manifests used by docs and conformance tests.

## Strategy & Roadmap

- [differentiation-strategy.md](differentiation-strategy.md): Positioning thesis — the sovereignty-led funnel, why Caesium wins by constraint not comparison, and the kill-conditions that test it.
- [sovereignty.md](sovereignty.md): Sovereignty proof-points — free vs. paywalled feature comparison (HA, RBAC, SSO, audit, lineage vs. Dagster+/Kestra Enterprise/Prefect Cloud) and a zero-dependency / air-gapped quickstart.
- [roadmap.md](roadmap.md): Strategic vision, design principles, and the prioritized feature plan; Phase 5 is sequenced by the closed-loop arc listed under Active Exec Plans.

## Active Design Records

Forward-looking or partially-shipped designs with open work. Each carries a `> Status:` banner near the top; CI enforces banners on the planning/historical records it tracks.

- [design-airflow-parity.md](design-airflow-parity.md): Airflow-parity workstreams — current shipped subset in `airflow-parity.md`; this tracks the remaining workstreams.
- [design-event-triggers.md](design-event-triggers.md): HTTP webhook triggers, event-based routing, and trigger chaining (WS1–WS3 shipped; reconciliation tracked in `exec-plans/completed/event-trigger-routing.md`).
- [design-concurrency-priority.md](design-concurrency-priority.md): Concurrency strategies, rate limiting, and priority-based scheduling (shipped; completed plan in `exec-plans/completed/concurrency-priority-queues.md`).
- [design-database-locking-fix.md](design-database-locking-fix.md): dqlite contention remediation (Phases 0–3 shipped) and the scale-out path.
- [design-scaling-job-execution.md](design-scaling-job-execution.md): Cluster-wide task-start throughput frontier on sharded dqlite.
- [design-incremental-execution.md](design-incremental-execution.md): Smart incremental execution and task caching (Phase 1 shipped; follow-on phases planned).
- [design-data-plane-memory.md](design-data-plane-memory.md): The second-act substrate (digest pinning, decomposed-hash persistence, DAG versioning, lineage datasets, large-object passing) that makes the data-plane queryable — explain/reproduce/skip. Substrate shipped (streams A–D, #213–#222); the causal query verbs (`run diff`, quarantined `replay`, `blame`) shipped via the completed follow-on plan `exec-plans/completed/data-plane-memory-ii.md`, and those verbs (plus `why`, receipt/`verify`, and the cross-job lineage-impact graph) are now surfaced in the web UI via `exec-plans/completed/data-plane-memory-ui.md`.
- [design-quarantined-replay.md](design-quarantined-replay.md): Authoritative fail-closed safety model for quarantined replay in the data-plane-memory-ii plan.
- [design-agent-in-the-loop.md](design-agent-in-the-loop.md): Agent-in-the-loop ETL remediation — autonomous failure triage and bounded remediation via a container-native agent over the data-plane-memory primitives (runtime shipped; the `metadata.remediation` jobdef block plus the incident manager, tiered executor, agent runtime, approval gates, and Console incident panels land — exec plan `exec-plans/completed/agent-in-the-loop-remediation.md`).
- [design-reproduce.md](design-reproduce.md): `caesium reproduce` — re-execute a single historical production task locally under Docker from its recorded execution descriptor (exact image digest, env, params, predecessor outputs), with secrets resolved locally or not at all (shipped via `exec-plans/completed/reproduce.md`).
- [design-freshness-scheduling.md](design-freshness-scheduling.md): Freshness-driven scheduling — declare freshness SLOs on datasets and derive execution from lineage and data arrival instead of cron guesses (shipped, streams A–G; the `datasets` jobdef surface, freshness evaluator, arrival signals, `GET /v1/datasets*`, Console freshness UI, skip-when-fresh, and `trigger: {type: freshness}` land — exec plan `exec-plans/completed/freshness-scheduling.md`).
- [design-backtesting.md](design-backtesting.md): Pipeline backtesting — replay a code change over recorded production runs in quarantine and report output deltas before merge (active — Plan 3 of the closed-loop arc; exec plan `exec-plans/active/backtesting.md`).
- [design-contract-enforcement.md](design-contract-enforcement.md): Cross-job contract enforcement — schema-compatibility checks across producer/consumer jobs at lint/diff/apply time, with named consumers, Console graph/diff surfaces, and an intentional-break path (shipped; completed plan `exec-plans/completed/contract-enforcement.md`).
- [design-data-circuit-breaker.md](design-data-circuit-breaker.md): Data circuit breaker — statistical assertions on step outputs with dataset holds that stop bad data from propagating downstream (active — Plan 1 of the closed-loop arc; exec plan `exec-plans/active/data-circuit-breaker.md`).
- [design-resource-right-sizing.md](design-resource-right-sizing.md): Learned resource right-sizing — per-step memory/CPU recommendations from run history plus OOM retry escalation (active — Plan 2 of the closed-loop arc; exec plan `exec-plans/active/resource-right-sizing.md`).
- [design-dynamic-fanout.md](design-dynamic-fanout.md): Dynamic fan-out — runtime partition markers materialize data-proportional parallel task instances with per-partition caching (shipped; exec plan `exec-plans/completed/dynamic-fanout.md`).
- [design-window-scheduling.md](design-window-scheduling.md): Deadline-window scheduling — run within a declared window, choosing the start via load/cost/carbon signals with a deadline-safe latest start (active — Plan 4 of the closed-loop arc; exec plan `exec-plans/active/window-scheduling.md`).

## Active Exec Plans

Live execution plans with unchecked work, orchestrated wave-by-wave via the `exec-plan-wave` skill. Feature exec plans that have a design record are linked from that record above; plans whose design of record is a superpowers spec are listed here. All of the current plans belong to the closed-loop arc and run in the order below; the arc doc is the umbrella (not itself a wave target) and wins on cross-plan ordering, while each child plan (and its design record above) wins on how a stream is built.

- `exec-plans/active/closed-loop-arc.md`: **Umbrella — the arc.** Thesis (three loops — data, compute, time — over the shared data-plane memory, with backtesting as the proof), the plan sequence, cross-plan synergies, shared conventions 1–8, file-conflict rules, arc acceptance criteria, and the parked/filed/deleted decisions.
- `exec-plans/active/trust-the-substrate.md`: **Plan 0 — trust the substrate.** Fix the six ledger bugs, close the tier-3 approval loop, widen the auth-enabled integration lane, make CI gate merges, cut `v0.1.0` with a downloadable CLI, name the shipped verbs in the README, delete dead scaffolding, file the unfiled follow-ups.
- `exec-plans/active/data-circuit-breaker.md`: **Plan 1 — the data loop.** Data circuit breaker plus its previously-deferred Phase 3: `data_quality_hold` incidents, `release_hold` action, held ⇒ not-fresh, `why` provenance (design record `design-data-circuit-breaker.md`).
- `exec-plans/active/resource-right-sizing.md`: **Plan 2 — the compute loop.** Resource right-sizing with the apply path recast as an incident action, k8s paths exercised in the kind lane, fan-out partition stats, `why` provenance for escalations (design record `design-resource-right-sizing.md`).
- `exec-plans/active/backtesting.md`: **Plan 3 — the proof loop.** Backtesting after a design refresh (`internal/outputdiff` reuse; reconcile with the fan-out-aware replay core), plus proposal verification in the approval flow and assertion-threshold backtests (design record `design-backtesting.md`).
- `exec-plans/active/window-scheduling.md`: **Plan 4 — the time loop (optional tail).** Window scheduling re-cut to P0 over the shared stats substrate; cost/carbon signals parked; not an arc gate (design record `design-window-scheduling.md`).

Recently completed: `exec-plans/completed/infra-deploy.md` — DAG-native infrastructure deployment — `cache.chain: values` + `ttl: never` (the one core change), the `caesiumcloud/{git-source,tf-discover,tf-warm,tf-runner}` reagent images implementing the generic unit-pipeline pattern with Terraform as the first binding, a multi-writer volume lint warning, reference manifests + mandatory drift job, and a Console proposal panel (drafted 2026-08-26; spec `superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`).

## Load Testing

- [load-testing-history.md](load-testing-history.md): Consolidated Phase 0 → Phase 2B distributed-execution load-test history (replaces the former per-run `load-baseline-*` series).

## Archive

Completed, shipped, or historical records that are no longer the active source of truth live under [`archive/`](archive/README.md): shipped design docs (ARM64 build support, Helm/Kubernetes deployment, internal mTLS auto-provisioning, parallel job execution), completed plans (job-definition reconciliation, UI implementation), the original feature brainstorm, and early architecture history.
