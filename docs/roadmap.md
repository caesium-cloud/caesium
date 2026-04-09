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

**Current state**: The internal event bus handles lifecycle events (run started, task completed, etc.) for SSE streaming and UI updates. There is no mechanism for external events to trigger jobs, no content-based routing, and no trigger chaining (one job's completion triggering another).

**Target state**: Jobs can declare event-based triggers that fire when matching events arrive — from external webhooks, internal lifecycle events (job completion chaining), or a new event ingestion API. Events support content-based filtering so a single webhook endpoint can route to different jobs based on payload content.

**Design doc**: [`design-event-triggers.md`](design-event-triggers.md)

### 1.3 Concurrency Strategies & Rate Limiting

**Current state**: Concurrency control is a single numeric `maxParallelTasks` knob (defaults to CPU count). No rate limiting, no fairness policies, no strategy for handling overlapping runs.

**Target state**: Teams can configure concurrency strategies per job (queue, replace-oldest, skip-if-running) and rate limits per resource (e.g., "max 100 API calls/minute across all tasks using this endpoint"). In distributed mode, fairness policies ensure shared clusters serve multiple teams equitably.

**Design doc**: [`design-concurrency-priority.md`](design-concurrency-priority.md)

### 1.4 Priority Queues

**Current state**: All tasks are equal. In distributed mode, workers claim tasks in arbitrary order. There is no way to express "this pipeline is more important than that one."

**Target state**: Jobs and individual runs can declare priority levels. The distributed task claimer respects priority ordering so critical pipelines run first when the cluster is saturated.

**Design doc**: [`design-concurrency-priority.md`](design-concurrency-priority.md)

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

### 2.4 Cost Tracking & Resource Awareness

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

**Current state**: Debugging failed runs requires manual log inspection. No way to compare two runs side-by-side.

**Target state**: The UI supports step-through replay of completed/failed runs showing the state at each point in time. A one-click root-cause trace follows the dependency chain from a failed task back to the unexpected input. Run diff compares two runs side-by-side (env, params, timing, outputs).

**Implementation plan**:
1. Run snapshot API: `GET /v1/jobs/:id/runs/:run_id/snapshot` returns full task state (inputs, outputs, timing, attempt count) at each DAG node
2. Run diff API: `GET /v1/jobs/:id/runs/diff?left=:run_id&right=:run_id`
3. UI: timeline view with task state at each point, diff view with highlighted changes
4. Root-cause trace: from a failed task, walk predecessors highlighting unexpected outputs or missing env vars

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
| **P2** | 2.4 Cost tracking | FinOps for pipelines. Large scope but high value. |
| **P3** | 3.1 Multi-tenancy | Required for larger orgs. Large scope, touches every layer. |
| **P3** | 3.2 Approval gates | Niche but important for compliance-heavy teams. |
| **P3** | 3.3 Self-serve triggers | Expands the user base beyond engineers. |
| **P3** | 3.4 Live DAG debugging | High wow-factor. Mostly UI work. |

---

## Completed Features

Features that were previously on the roadmap and are now shipped:

| Feature | Design Doc | Status |
|---------|-----------|--------|
| Smart incremental execution & task caching | [`design-incremental-execution.md`](design-incremental-execution.md) | Shipped (Phase 1) |
| Data contracts / schema validation | [`brainstorm-differentiators.md`](brainstorm-differentiators.md) §2 | Shipped |
| Local dev experience (`caesium dev`, `caesium test`) | [`brainstorm-differentiators.md`](brainstorm-differentiators.md) §4 | Shipped |
| Backfill with reprocess modes | [`backfill.md`](backfill.md) | Shipped |
| OpenLineage integration | [`open_lineage.md`](open_lineage.md) | Shipped |
| Git-based job synchronization | — | Shipped |
| Harness testing framework | — | Shipped |
| Full-featured HTTP triggers & webhook ingestion | [`design-event-triggers.md`](design-event-triggers.md) WS1 | Shipped |
| Task retries with exponential backoff | [`airflow-parity.md`](airflow-parity.md) | Shipped |
| Trigger rules (all_success, all_done, etc.) | [`airflow-parity.md`](airflow-parity.md) | Shipped |
| Embedded web UI with DAG visualization | [`ui_implementation_plan.md`](ui_implementation_plan.md) | Shipped |

---

## Related Documents

- [Brainstorm: Killer Features Beyond Airflow Parity](brainstorm-differentiators.md) — original idea backlog
- [Design: Smart Incremental Execution](design-incremental-execution.md) — shipped cache system
- [Design: Event-Driven Triggers](design-event-triggers.md) — P0 trigger overhaul
- [Design: Concurrency & Priority](design-concurrency-priority.md) — P1 scheduling controls
- [Design: Task Templates](design-task-templates.md) — P2 reusable steps
- [Design: SLA Management](design-sla-management.md) — P2 deadline tracking
