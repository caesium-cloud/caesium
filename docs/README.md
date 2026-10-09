# Caesium documentation

New here? Start with [getting-started.md](getting-started.md). The rest is
grouped by what you are trying to do. Guides describe how Caesium behaves
today. Design records and execution plans explain why it is built the way it
is and what is still in flight; they are not operator documentation.

## Author pipelines

| Guide | What it covers |
| --- | --- |
| [getting-started.md](getting-started.md) | Install the CLI, run a server, write and run a job, ask `why`, get a receipt. |
| [job-definitions.md](job-definitions.md) | The authoring reference: DAG wiring, branching, dynamic fan-out, scheduling controls, caching, contracts, freshness, volumes, secrets, Git sync, lint / diff / apply. |
| [job-schema-reference.md](job-schema-reference.md) | Field-by-field schema generated from `pkg/jobdef` by `caesium job schema --doc`. Do not edit by hand. |
| [caesium-job-llm-reference.md](caesium-job-llm-reference.md) | Compact authoring reference for coding assistants, plus the executable harness scenario format. |
| [backfill.md](backfill.md) | Backfills over cron intervals through the API, CLI, and console. |
| [reproduce.md](reproduce.md) | `caesium reproduce`: rebuild one historical task on your own Docker daemon. |
| [temporal.md](temporal.md) | Driving Caesium jobs from Temporal workflows over the REST API. |
| [infrastructure-deployment.md](infrastructure-deployment.md) | Terraform stacks (or any unit-pipeline tool) as dependency-ordered DAGs using the reagent images. |
| [examples/](examples/) | Example manifests loaded by `just hydrate` and exercised by the conformance tests. [examples-k8s/](examples-k8s/) holds the `engine: kubernetes` variants. |

## Run and operate a server

| Guide | What it covers |
| --- | --- |
| [rest-api.md](rest-api.md) | REST endpoints, auth endpoints, the SSE event stream, and the gated operator consoles. |
| [sso-authentication.md](sso-authentication.md) | API keys, roles, and native OIDC / SAML / LDAP sign-in. |
| [distributed-execution.md](distributed-execution.md) | Execution modes, every worker and dqlite environment variable, run-owner mode, Raft log retention, database sharding, troubleshooting. |
| [kubernetes-deployment.md](kubernetes-deployment.md) | The Helm chart, three-node Raft clusters, Kueue delegation, backup and member replacement, air-gapped notes. |
| [sovereignty.md](sovereignty.md) | The air-gapped quickstart and the free-versus-paywalled comparison with other orchestrators. |
| [open-lineage.md](open-lineage.md) | OpenLineage event emission, facets, and transports. |
| [connectors.md](connectors.md) | Execution connectors. In progress, not shipped: records the frozen configuration contract. |
| [upgrade-notes.md](upgrade-notes.md) | Behavior changes to read before upgrading. |
| [ci.md](ci.md) | The CI runbook: required-to-merge checks, the job matrix, per-lane server env parity, and the release procedure. |

## Direction

- [roadmap.md](roadmap.md) is the design principles and the prioritised plan. When a plan and the roadmap disagree on priority, the roadmap wins.
- [design/](design/) holds one design record per feature. Each starts with a `> Status:` banner saying whether it is proposed, active, or shipped, and links the plan that implements it.
- [exec-plans/active/](exec-plans/active/) holds the plans being executed wave by wave with the `exec-plan-wave` skill; finished plans move to [exec-plans/completed/](exec-plans/completed/).
- [archive/](archive/README.md) keeps shipped or superseded records that are no longer the source of truth, with a pointer to their live successor.

### Design records

| Record | Status |
| --- | --- |
| [differentiation-strategy.md](design/differentiation-strategy.md) | Positioning. Sovereignty-led, data-plane memory as the second act. |
| [incremental-execution.md](design/incremental-execution.md) | Shipped. Content-addressed task cache and restart-from-failure. |
| [event-triggers.md](design/event-triggers.md) | Shipped. Webhook triggers, event routing, trigger chaining. |
| [concurrency-priority.md](design/concurrency-priority.md) | Shipped. Run concurrency strategies, priorities, rate limits. |
| [airflow-parity.md](design/airflow-parity.md) | Mostly shipped. Tracks the remaining Airflow-style workstreams. |
| [database-locking-fix.md](design/database-locking-fix.md) | Shipped through Phase 3. Phase 4 sharded write path in progress. |
| [scaling-job-execution.md](design/scaling-job-execution.md) | Partially shipped. Run-owner coordination behind `CAESIUM_RUN_OWNER_ENABLED`. |
| [data-plane-memory.md](design/data-plane-memory.md) | Shipped. The substrate behind `why`, receipts, lineage, and value-verified skip. |
| [quarantined-replay.md](design/quarantined-replay.md) | Shipped. The fail-closed safety model for `run replay` and backtesting. |
| [reproduce.md](design/reproduce.md) | Shipped. `caesium reproduce`. |
| [dynamic-fanout.md](design/dynamic-fanout.md) | Shipped. Runtime partitions become parallel task instances. |
| [freshness-scheduling.md](design/freshness-scheduling.md) | Shipped. Datasets, freshness SLOs, schedule on arrival. |
| [contract-enforcement.md](design/contract-enforcement.md) | Shipped. Cross-job schema contracts checked at lint / diff / apply. |
| [agent-in-the-loop.md](design/agent-in-the-loop.md) | Shipped runtime. Incident triage and approval-gated remediation. |
| [data-circuit-breaker.md](design/data-circuit-breaker.md) | Active, Plan 1 of the closed-loop arc. Statistical assertions and dataset holds. |
| [resource-right-sizing.md](design/resource-right-sizing.md) | Active, Plan 2. Learned requests and OOM retry escalation. |
| [backtesting.md](design/backtesting.md) | Active, Plan 3. Replay a change over recorded production runs before merge. |
| [window-scheduling.md](design/window-scheduling.md) | Active, Plan 4 (optional). Deadline windows instead of cron guesses. |
| [2026-05-29-volumes-and-workload-identity-design.md](design/2026-05-29-volumes-and-workload-identity-design.md) | Shipped spec. Named volumes and Kubernetes workload identity. |
| [2026-08-25-dag-native-infrastructure-deployment-design.md](design/2026-08-25-dag-native-infrastructure-deployment-design.md) | Shipped spec. The unit-pipeline pattern behind infrastructure-deployment.md. |
| [2026-09-16-identity-and-access-design.md](design/2026-09-16-identity-and-access-design.md) | Active spec. Namespaces, grants, policy-as-code, corporate SSO. |

### Active execution plans

| Plan | Scope |
| --- | --- |
| [closed-loop-arc.md](exec-plans/active/closed-loop-arc.md) | Umbrella for Plans 0 to 4. Owns cross-plan ordering and shared conventions; not itself a wave target. |
| [data-circuit-breaker.md](exec-plans/active/data-circuit-breaker.md) | Plan 1, the data loop: `##caesium::metrics`, assertions, holds, incidents. |
| [resource-right-sizing.md](exec-plans/active/resource-right-sizing.md) | Plan 2, the compute loop: stats substrate, recommendations, OOM escalation. |
| [backtesting.md](exec-plans/active/backtesting.md) | Plan 3, the proof loop: backtest proposals against recorded runs. |
| [window-scheduling.md](exec-plans/active/window-scheduling.md) | Plan 4, the time loop. Optional tail of the arc. |
| [distributed-testing.md](exec-plans/active/distributed-testing.md) | Cross-cutting: make a green `ci-ok` mean the distributed guarantees hold. 30 of 31 items done; F3 open on product findings. |
| [identity-and-access.md](exec-plans/active/identity-and-access.md) | Namespaces, grants, policy file, Keycloak lane, `caesium login`. |
| [execution-connectors.md](exec-plans/active/execution-connectors.md) | Temporal monitoring and declared operator actions. Planning only. |

Completed plans live in [exec-plans/completed/](exec-plans/completed/); the most recent are
[trust-the-substrate.md](exec-plans/completed/trust-the-substrate.md) (Plan 0, cut `v0.1.0`),
[infra-deploy.md](exec-plans/completed/infra-deploy.md), and
[go-maintenance.md](exec-plans/completed/go-maintenance.md).
