# Roadmap

> Status: Living document. Features are organized by strategic priority, not chronological order. Presence on this list is not a commitment — it reflects the project's direction and design thinking.

## Design Principles

These principles guide what Caesium builds and, equally importantly, what it doesn't.

1. **Container-native execution.** Any Docker image is a valid task. No SDK required, no language lock-in, no long-running worker process. A bash script, a Spark job, a dbt run, a trained model — if it runs in a container, it runs in Caesium. This is the fundamental differentiator and every feature must preserve it.

2. **Declarative and GitOps-first.** Pipelines are YAML manifests checked into version control. They can be linted, diffed, reviewed in PRs, and applied like infrastructure-as-code. The source of truth is the git repository, not a UI or an SDK.

3. **Zero-dependency simplicity.** Caesium runs as a single binary with distributed SQLite. No PostgreSQL, no Redis, no message broker. This makes it trivial to deploy, operate, and reason about. New features must not introduce mandatory external dependencies.

4. **Smart by default.** Incremental execution, content-addressed caching, and schema validation happen automatically when opted into. The scheduler should do the right thing without requiring users to build custom solutions on top.

5. **Data engineering first.** Caesium is built for teams running data pipelines, ETL workflows, and batch processing. Features are prioritized for this audience — backfills, data contracts, lineage, cost awareness — over application-layer async task queues.

6. **Open source, community-driven.** Caesium is not a managed service. Features are designed for self-hosted deployment. The project succeeds by being genuinely better software, not by creating vendor lock-in.

---

## Phase 1: Close Functional Gaps

These features address the most common reasons a team would choose an alternative orchestrator over Caesium today.

### 1.1 Full-Featured HTTP Triggers & Webhook Ingestion

**Status**: Shipped. HTTP triggers now support `POST /v1/hooks/*`, configured webhook paths, optional request authentication (`hmac-sha256`, `hmac-sha1`, `bearer`, `basic`), payload parameter extraction via `paramMapping`, default parameter merging, and operator-authenticated manual/API fire via `POST /v1/triggers/:id/fire`. The web UI also exposes HTTP trigger configuration and editing.

**Delivered state**: HTTP triggers are now a first-class webhook ingestion layer. External systems (CI/CD, GitHub, Slack, S3 notifications, custom apps) can POST to a dedicated webhook endpoint, and Caesium routes the payload to the correct job with parameter extraction and signature validation.

**Design doc**: [`design-event-triggers.md`](design-event-triggers.md)

### 1.2 Event-Driven Trigger Routing

**Status**: Shipped. The event-routing plan delivered `event` triggers with content filtering, `POST /v1/events` ingestion, the webhook-to-event bridge for `/v1/hooks/*` traffic, trigger chaining through the lifecycle bus, static and runtime cycle detection, and durable event observability with `caesium event push` and `caesium trigger events`.

**Delivered state**: Jobs can declare event-based triggers that fire when matching events arrive from external ingestion, webhook traffic, or internal lifecycle events. Event patterns support type globs, source filters, and JSON payload filters, so one endpoint or event stream can route many event shapes to the right jobs. Chained jobs flow through the same router, with lint/apply cycle checks and the `_trigger_depth` runtime guard preventing runaway loops.

**Design doc**: [`design-event-triggers.md`](design-event-triggers.md)

**Plan**: [Event-Driven Trigger Routing](exec-plans/completed/event-trigger-routing.md)

### 1.3 Concurrency Strategies & Rate Limiting

**Status**: Shipped. Jobs declare a `metadata.concurrency` block (`maxRuns` + `strategy` ∈ `queue`/`replace`/`skip`/`fail`) that gates a new run when the job is already at `maxRuns` — admission is a single atomic conditional insert (no cross-node TOCTOU), `replace` cancels the oldest active run, and `queue` parks overflow in a durable `run_queue` drained by a leader-gated dequeuer. Resource rate limiting (`metadata.rateLimits` / step `rateLimit`) throttles tasks against a named shared resource via a durable atomic sliding-window limiter, re-queuing (not blocking) over-limit tasks. Operator surface: `caesium job queue <alias>` + a run-queue panel on the job-detail page. Per-namespace fairness remains deferred to §3.1.

**Target state**: Teams can configure concurrency strategies per job (queue, replace-oldest, skip-if-running) and rate limits per resource (e.g., "max 100 API calls/minute across all tasks using this endpoint"). In distributed mode, fairness policies ensure shared clusters serve multiple teams equitably.

**Design doc**: [`design-concurrency-priority.md`](design-concurrency-priority.md)

**Plan**: [Concurrency Strategies & Priority Queues](exec-plans/completed/concurrency-priority-queues.md) (Streams A/C/D)

### 1.4 Priority Queues

**Status**: Shipped. A job-level `metadata.priority` (and a per-run `caesium run start --priority` / REST override) maps `high`/`normal`/`low` → `3`/`2`/`1` and is stamped onto every `TaskRun`; the distributed claimer drains `ORDER BY priority DESC, created_at ASC` (over a supporting composite index), and the run-queue dequeuer drains priority-first. Strictly ordering, never preemptive.

**Target state**: Jobs and individual runs can declare priority levels. The distributed task claimer respects priority ordering so critical pipelines run first when the cluster is saturated.

**Design doc**: [`design-concurrency-priority.md`](design-concurrency-priority.md)

**Plan**: [Concurrency Strategies & Priority Queues](exec-plans/completed/concurrency-priority-queues.md) (Stream B)

### 1.5 API Key Auth Hardening Follow-up

**Status**: Shipped. The API-key layer now adds timing normalization for auth failures, keyed hash storage for newly issued keys, legacy-hash compatibility during rollout, and conflict-safe bootstrap semantics for concurrent startup.

**Delivered state**:

1. Authentication failures (`not found`, `expired`, `revoked`) now share a minimum timing envelope in `ValidateKey`, reducing low-signal timing differences between failure modes.
2. Newly created and rotated API keys are stored as versioned HMAC-SHA256 hashes when `CAESIUM_AUTH_KEY_HASH_SECRET` is configured. Legacy SHA-256 rows still validate so existing deployments can upgrade and rotate keys in place.
3. Bootstrap admin-key creation now uses a reserved database slot plus retry-on-lock behavior so concurrent startup does not emit duplicate bootstrap keys, while still allowing a revoked or expired bootstrap key to be refreshed when no active admin key remains.

---

## Phase 2: Deepen Differentiators

These features widen the gap between Caesium and alternatives in areas where Caesium already leads.

### 2.1 PR Preview Runs & Visual DAG Diff

**Current state**: `caesium job diff` shows textual differences between local YAML and the server. `caesium dev --once` runs a job locally. These are separate manual steps.

**Target state**: A GitHub Action (and GitLab CI template) that automatically runs on PRs touching `*.job.yaml` files: validates the definition, renders a visual DAG diff (added/removed tasks, changed parameters, new edges), executes the job in a sandboxed namespace, and posts results as a PR comment. This makes pipeline changes as reviewable as application code changes.

**Implementation plan**:
1. Extend `caesium job diff` to produce structured JSON output (not just human-readable text)
2. Add `caesium job diff --format=markdown` for PR-comment-ready output with ASCII DAG rendering
3. Build a GitHub Action that chains `lint → diff → dev --once → comment`
4. Support `--namespace` flag on `caesium dev` for isolated execution
5. Publish the Action to the GitHub Marketplace

### 2.2 Composable Task Templates & Registry

**Current state**: Every step is defined inline in the job YAML. Common patterns (dbt-run, spark-submit, s3-sync, pg-dump) are copy-pasted across jobs.

**Target state**: Steps can reference reusable templates with parameterized configuration. Templates can be local files, git references, or entries in a shared registry. This reduces boilerplate, enforces consistency, and creates an ecosystem of community-contributed templates.

**Design doc**: [`design-task-templates.md`](design-task-templates.md)

### 2.3 SLA Management & Predictive ETAs

**Current state**: No SLA support. Users monitor pipeline health manually or build custom alerting on top of Prometheus metrics.

**Target state**: Jobs declare SLA deadlines ("must complete by 06:00 UTC"). Caesium uses historical run durations to predict completion times and escalate proactively — alerting when a pipeline is at risk of missing its SLA, not just when it has already missed. Escalation chains support Slack, PagerDuty, and webhook notifications.

**Design doc**: [`design-sla-management.md`](design-sla-management.md)

### 2.4 UI Refresh (Caesium Console v2) ✅ Shipped 2026-04-28

Design system, status semantics, and full page refreshes shipped across PRs [#146](https://github.com/caesium-cloud/caesium/pull/146), [#147](https://github.com/caesium-cloud/caesium/pull/147), and [#148](https://github.com/caesium-cloud/caesium/pull/148). Phases 0–4 complete. See [`ui_implementation_plan.md`](archive/ui_implementation_plan.md) for the shipped feature record.

**Hardening (active)**: a hands-on walkthrough of the shipped Console surfaced a batch of correctness defects (every cron shown "Invalid cron", double-listed Live Activity events, JobDefs "No steps" for multi-step manifests, undecoded command escapes, unsurfaced failed callbacks, and more). Fixes are tracked in [`exec-plans/active/console-v2-bug-sweep.md`](exec-plans/active/console-v2-bug-sweep.md).

**Operator-loop refinement (follow-up, active)**: a focused UX review of the job-detail, run-detail, and DAG surfaces found the core operator loop (*what is this pipeline, is it healthy, what happened last run, let me act*) obscured — the run page leads with the reproducibility receipt and buries status/timeline/DAG/logs, healthy `SUCCEEDED` runs are dressed in red, the trigger fires with no params/confirmation, and the header overflows so `Pause` is clipped. The remediation is tracked in [`exec-plans/active/console-operator-loop-ux.md`](exec-plans/active/console-operator-loop-ux.md) (4 UI-first streams; overlaps the remaining UI surface of §3.4).

### 2.5 Cost Tracking & Resource Awareness

**Current state**: Prometheus metrics track run counts and durations. No resource consumption data (CPU, memory) and no cost attribution.

**Target state**: Caesium collects container resource usage per task via cgroup stats (Docker) or the metrics API (Kubernetes), rolls it up to per-job and per-run totals, and surfaces it in the UI and Prometheus. Configurable cost models map resource usage to dollar amounts. Anomaly detection alerts when a job's resource consumption spikes vs. its rolling average.

**Implementation plan**:
1. Add resource stats collection to the Docker and Kubernetes atom engines (`Stats()` method on the `Engine` interface)
2. Store per-task resource snapshots (peak memory, CPU seconds) on `TaskRun`
3. Add aggregation endpoints: `GET /v1/jobs/:id/costs`, `GET /v1/stats/costs`
4. New Prometheus metrics: `caesium_task_cpu_seconds_total`, `caesium_task_memory_peak_bytes`
5. Cost model configuration via `CAESIUM_COST_MODEL` env var (JSON mapping resource types to $/unit)
6. UI: cost column on job list, cost breakdown on run detail, anomaly badges
7. Anomaly detection: rolling average over last N runs, alert when current run exceeds 2x

---

## Phase 3: Expand Capabilities

These features open Caesium to new use cases and larger organizations.

### 3.1 Multi-Tenancy & Namespace Isolation

**Current state**: Single-tenant. All jobs, runs, and resources share a flat namespace.

**Target state**: Logical namespaces isolate teams' jobs, runs, and resources. Per-namespace quotas limit concurrent runs, CPU, and memory. The UI supports scoped views. An audit log tracks who changed what, when.

**Implementation plan**:
1. Add `namespace` field to `metadata` in job definitions (default: `default`)
2. Add `Namespace` column to `Job`, `Trigger`, and related models
3. Scope all API queries by namespace (header or query param)
4. Per-namespace quotas: `CAESIUM_NAMESPACE_QUOTAS` env var (JSON)
5. Namespace-scoped UI views with a namespace switcher
6. Audit log table: `audit_events(namespace, actor, action, resource, diff, timestamp)`
7. Cross-namespace trigger references (controlled, explicit opt-in)

### 3.2 Approval Gates & Human-in-the-Loop

**Current state**: All tasks execute automatically once dependencies are satisfied. No mechanism for human approval before execution.

**Target state**: Steps can declare approval gates that pause execution until a human approves (or the gate times out). This supports production deployment pipelines, sensitive data processing, and compliance workflows.

**YAML example**:
```yaml
steps:
  - name: deploy-to-prod
    gate:
      type: approval
      approvers: ["@data-leads"]
      timeout: 4h
      on_timeout: fail  # or skip
    dependsOn: [validate]
    image: deploy:latest
    command: ["deploy.sh"]
```

**Implementation plan**:
1. New task status: `awaiting_approval`
2. Gate definition in step schema with `type`, `approvers`, `timeout`, `on_timeout`
3. API endpoints: `POST /v1/jobs/:id/runs/:run_id/tasks/:task_id/approve`, `POST .../reject`
4. Notification callbacks when a gate is reached (reuse existing callback infrastructure)
5. UI: approval button on task detail, pending approvals list on dashboard
6. Timeout enforcement in the executor (background goroutine checks pending gates)

### 3.3 Pipeline-as-a-Service / Self-Serve Triggers

**Current state**: Triggering a job requires API access or CLI usage. Non-engineers cannot interact with pipelines.

**Target state**: The UI auto-generates input forms from run parameter schemas. Shareable trigger links allow non-engineers to run pipelines with validated inputs. Slack/Teams integration enables triggering and monitoring from chat.

**Implementation plan**:
1. Extend `defaultParams` schema to support `type`, `description`, `enum`, `default`, `format`
2. UI form generator that renders appropriate inputs per parameter type
3. Shareable trigger URLs: `GET /ui/jobs/:alias/trigger` renders the form
4. Slack integration: slash command `/caesium run <alias>`, status notifications via webhook callbacks
5. Read-only vs. operator role distinction (view runs vs. trigger runs vs. edit definitions)

### 3.4 Live DAG Debugging & Run Diff

**Status**: Partially shipped via the [data-plane-memory](design-data-plane-memory.md) substrate. `caesium why <run> --task <t>` reimagines the causal half of this item — a field-level, machine-checkable explanation of why a task ran/skipped/re-ran (the discriminating `HashInput` field + trigger causation), reconstructed from the persisted decomposed hash + event store rather than a UI state-viewer. A git-committable reproducibility receipt + `caesium verify` and append-only DAG-topology history also land here. The remaining causal verbs have now **shipped** ([Exec Plan: Data-Plane Memory II](exec-plans/completed/data-plane-memory-ii.md)): `caesium run diff` (causal cache-bust attribution across two runs), `caesium blame` (commit/snapshot topology attribution over `dag_snapshot`), and the quarantined what-if `caesium run replay --set … --diff` (a descriptor-reconstructed, replay-safe-gated, side-effect-free run via the distributed worker). These causal verbs have now **shipped in the web UI** — run diff (causal cache-bust attribution), the quarantined what-if replay, the per-task `why` explainer, blame, the reproducibility receipt + `verify`, and the cross-job lineage-impact graph are all first-class affordances on the run/task surfaces, each gated by a Playwright e2e (incl. an auth-enabled lane) that drives the real UI against a live backend — see the completed [Data-Plane Memory UI](exec-plans/completed/data-plane-memory-ui.md) plan. What remains of the original §3.4 vision is the literal interactive timeline scrubber (state-at-each-point step-through); the causal verbs above deliver its diagnostic substance.

**Current state**: Debugging failed runs requires manual log inspection. No way to compare two runs side-by-side.

**Target state**: The UI supports step-through replay of completed/failed runs showing the state at each point in time. A one-click root-cause trace follows the dependency chain from a failed task back to the unexpected input. Run diff compares two runs side-by-side (env, params, timing, outputs).

**Implementation plan**:
1. Run snapshot API: `GET /v1/jobs/:id/runs/:run_id/snapshot` returns full task state (inputs, outputs, timing, attempt count) at each DAG node
2. Run diff API: `GET /v1/jobs/:id/runs/diff?left=:run_id&right=:run_id`
3. UI: timeline view with task state at each point, diff view with highlighted changes
4. Root-cause trace: from a failed task, walk predecessors highlighting unexpected outputs or missing env vars

### 3.5 Agent-in-the-Loop ETL Remediation

**Status**: Shipped (runtime). A failing run now opens a persisted incident that a deterministic classifier triages. The `metadata.remediation` jobdef block (`profile`/`classes`/`maxAttempts`/`autonomy`/`escalation`) declares a tiered, server-enforced action policy, and a container-native agent runtime — scoped session token, session supervisor, triage bundle, the `/v1/agent/*` tool surface plus an MCP surface — executes bounded tier-1/2 actions autonomously while tier-3 actions are gated behind a human approval. AgentProfiles are managed via `/v1/agentprofiles`; incident reads, the `ai_agent` dispatch channel, and Console incident/agent-activity/analytics panels ship alongside. BYO agent image and model key; deterministic rules handle the cheap failure classes without any LLM call. Feature-gated behind `CAESIUM_AGENT_REMEDIATION_ENABLED` with an active auth mode.

**Delivered state**: Failures open an incident that a container-native LLM agent triages using Caesium's causal primitives (`why`, run diff, receipts, lineage impact, quarantined replay as a what-if sandbox) and remediates within a declarative, tiered, server-enforced action policy — retrying late-file extracts on a schedule, proposing human-approved schema patches for vendor drift, pausing lineage-adjacent jobs on credential failures — escalating to humans with the diagnosis already done.

**Design doc**: [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md)

**Plan**: [`agent-in-the-loop-remediation.md`](exec-plans/active/agent-in-the-loop-remediation.md) — decomposed into 8 streams, phased 0→3 (Phase 0 = diagnosed pages, no LLM); all runtime streams merged. The `metadata.remediation` surface is documented in [`job-schema-reference.md`](job-schema-reference.md#remediation).

---

## Phase 4: Data-Plane Differentiators (Design Wave)

A brainstormed wave of proposed designs that compound the shipped data-plane-memory substrate (descriptors, receipts, lineage, cache identity, quarantined replay) and the agent-in-the-loop direction. Each is a standalone design doc. **Freshness-driven scheduling — the strategic flagship — has since shipped** (streams A–G merged); the rest are not yet committed to implementation. The first three decompose the "Dataflow-style compute sized to the ETL" instinct into tractable, container-native slices (vertical, horizontal, temporal) without Caesium ever owning the computation model.

Each design has a drafted execution plan under `docs/exec-plans/active/` decomposing it into parallelizable streams. Freshness-driven scheduling has shipped its full wave (see the row below and the [Completed Features](#completed-features) table); the remaining plans are eligible for the `exec-plan-wave` skill but have not yet shipped an implementation wave.

| Design | One-liner | Doc | Plan |
|--------|-----------|-----|------|
| Resource right-sizing | Learn per-step memory/CPU from run history; propose right-sized requests (GitOps PR) and retry OOM at escalated memory | [`design-resource-right-sizing.md`](design-resource-right-sizing.md) | [`resource-right-sizing.md`](exec-plans/active/resource-right-sizing.md) |
| Dynamic fan-out | A step emits a partition list; Caesium materializes N parallel task instances with per-partition cache identity | [`design-dynamic-fanout.md`](design-dynamic-fanout.md) | [`dynamic-fanout.md`](exec-plans/active/dynamic-fanout.md) |
| Deadline-window scheduling | Declare a window + deadline instead of a cron minute; scheduler picks the start from load/cost/carbon signals with a deadline-safe latest start | [`design-window-scheduling.md`](design-window-scheduling.md) | [`window-scheduling.md`](exec-plans/active/window-scheduling.md) |
| Freshness-driven scheduling | **Shipped.** Declare freshness SLOs on datasets; execution derives from lineage + data arrival instead of cron guesses — the `datasets` jobdef surface, freshness evaluator, arrival signals, `GET /v1/datasets*`, Console freshness UI, P1 skip-when-fresh, and P2 `trigger: {type: freshness}` all land | [`design-freshness-scheduling.md`](design-freshness-scheduling.md) | [`freshness-scheduling.md`](exec-plans/active/freshness-scheduling.md) |
| Pipeline backtesting | Replay a code change over recorded production runs in quarantine; report output deltas in the PR before merge | [`design-backtesting.md`](design-backtesting.md) | [`backtesting.md`](exec-plans/active/backtesting.md) |
| Contract enforcement | Cross-job schema-compatibility checks at lint/diff/apply with named consumers and an intentional-break path | [`design-contract-enforcement.md`](design-contract-enforcement.md) | [`contract-enforcement.md`](exec-plans/active/contract-enforcement.md) |
| Data circuit breaker | Statistical assertions on step outputs; violations hold the dataset so downstream jobs skip poison instead of consuming it | [`design-data-circuit-breaker.md`](design-data-circuit-breaker.md) | [`data-circuit-breaker.md`](exec-plans/active/data-circuit-breaker.md) |
| `caesium reproduce` | Re-execute any historical task locally from its execution descriptor — production debugging on a laptop | [`design-reproduce.md`](design-reproduce.md) | [`reproduce.md`](exec-plans/active/reproduce.md) |

---

## Execution Priority

| Priority | Feature | Rationale |
|----------|---------|-----------|
| **P0** | 1.2 Event-driven routing | Completes the trigger story. Without events, Caesium can only do time-based and manual scheduling. |
| **P1** | 1.3 Concurrency strategies | Table-stakes for shared clusters. Blocks multi-team adoption. |
| **P1** | 1.4 Priority queues | Small scope, high impact for distributed deployments. |
| **P1** | 2.1 PR preview runs | Leverages existing CLI capabilities. Uniquely strong differentiator. |
| **P2** | 2.2 Task templates | Creates ecosystem and reduces boilerplate. Medium scope. |
| **P2** | 2.3 SLA management | Genuinely unique. No orchestrator does this well. |
| **P2** | 2.4 UI refresh | Visual identity + primitive consolidation. Phased so foundations land first and propagate automatically. |
| **P2** | 2.5 Cost tracking | FinOps for pipelines. Large scope but high value. |
| **P3** | 3.1 Multi-tenancy | Required for larger orgs. Large scope, touches every layer. |
| **P3** | 3.2 Approval gates | Niche but important for compliance-heavy teams. |
| **P3** | 3.3 Self-serve triggers | Expands the user base beyond engineers. |
| **P3** | 3.4 Live DAG debugging | High wow-factor. Mostly UI work. |
| **P3** | 3.5 Agent-in-the-loop remediation | Runtime shipped. Converts the data-plane-memory substrate into autonomous ops; the `metadata.remediation` policy + agent runtime land, Phase 0 (diagnosed pages) first. |
| **P3** | Phase 4 design wave | Eight proposed designs compounding the data-plane substrate; freshness-driven scheduling has **shipped** its full wave, contract enforcement and right-sizing remain the near-term standouts. |

---

## Completed Features

Features that were previously on the roadmap and are now shipped:

| Feature | Design Doc | Status |
|---------|-----------|--------|
| Smart incremental execution & task caching | [`design-incremental-execution.md`](design-incremental-execution.md) | Shipped (Phase 1) |
| Data contracts / schema validation | [`brainstorm-differentiators.md`](archive/brainstorm-differentiators.md) §2 | Shipped |
| Local dev experience (`caesium dev`, `caesium test`) | [`brainstorm-differentiators.md`](archive/brainstorm-differentiators.md) §4 | Shipped |
| Backfill with reprocess modes | [`backfill.md`](backfill.md) | Shipped |
| OpenLineage integration | [`open_lineage.md`](open_lineage.md) | Shipped |
| Git-based job synchronization | — | Shipped |
| Harness testing framework | — | Shipped |
| Full-featured HTTP triggers & webhook ingestion | [`design-event-triggers.md`](design-event-triggers.md) WS1 | Shipped |
| Task retries with exponential backoff | [`airflow-parity.md`](airflow-parity.md) | Shipped |
| Trigger rules (all_success, all_done, etc.) | [`airflow-parity.md`](airflow-parity.md) | Shipped |
| Embedded web UI with DAG visualization | [`ui_implementation_plan.md`](archive/ui_implementation_plan.md) | Shipped |
| Native SSO authentication | [`sso-authentication.md`](sso-authentication.md) | Shipped (OIDC, SAML, LDAP) |
| Freshness-driven scheduling | [`design-freshness-scheduling.md`](design-freshness-scheduling.md) | Shipped ([plan](exec-plans/active/freshness-scheduling.md); streams A–G) |
| Agent-in-the-loop ETL remediation | [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) | Shipped ([plan](exec-plans/active/agent-in-the-loop-remediation.md); runtime streams) |

---

## Related Documents

- [Differentiation Strategy: Where Caesium Wins](differentiation-strategy.md) — positioning thesis; re-ranks this roadmap behind a sovereignty-led funnel
- [Exec Plan: Sovereignty Execution](exec-plans/completed/sovereignty-execution.md) — operationalizes the positioning pivot (README repositioning + Kueue delegation); shipped
- [Design: Data-Plane Memory](design-data-plane-memory.md) — the second-act substrate enabling explain/reproduce/skip
- [Exec Plan: Data-Plane Memory](exec-plans/completed/data-plane-memory.md) — the substrate build plan (streams A–D); shipped (#213–#222)
- [Exec Plan: Data-Plane Memory II](exec-plans/completed/data-plane-memory-ii.md) — the completed follow-on: causal `run diff`, quarantined `replay`, and `blame` (all shipped)
- [Brainstorm: Killer Features Beyond Airflow Parity](archive/brainstorm-differentiators.md) — original idea backlog
- [Design: Smart Incremental Execution](design-incremental-execution.md) — shipped cache system
- [Design: Event-Driven Triggers](design-event-triggers.md) — P0 trigger overhaul
- [Design: Concurrency & Priority](design-concurrency-priority.md) — P1 scheduling controls
- [Design: Task Templates](design-task-templates.md) — P2 reusable steps
- [Design: SLA Management](design-sla-management.md) — P2 deadline tracking
- [Design: Agent-in-the-Loop ETL Remediation](design-agent-in-the-loop.md) — P3 autonomous failure triage & remediation
