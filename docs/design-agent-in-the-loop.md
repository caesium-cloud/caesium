# Design: Agent-in-the-Loop ETL Remediation

> Status: Brainstorm/Design — proposal for autonomous failure triage and
> remediation. No implementation yet. Companion roadmap items: §2.3 SLA
> Management (escalation), §3.2 Approval Gates & Human-in-the-Loop.

## Problem

Operating many pipelines across many vendors means a steady stream of
operational noise that today lands on a human pager:

- **Delayed files.** A vendor SFTP drop lands at 04:30 instead of 03:00. The
  extract step fails, the on-call gets paged, and the "fix" is to wait an hour
  and press retry.
- **Schema drift.** A vendor adds a column, renames `customer_id` to
  `customerId`, or changes a type. `outputSchema` validation fails (correctly!),
  but resolving it means a human reads the violation, decides whether it is
  benign, and edits YAML.
- **Bad values.** A currency field arrives as `"N/A"`, a date as `0000-00-00`.
  The load step errors on three rows out of ten million.
- **Transient infrastructure.** Image pull timeouts, node evictions, registry
  blips, OOM on an unusually large batch. `retries`+`retryBackoff` catch some
  of this, but the long tail (retry *after the registry recovers*, retry *with
  more memory*) requires judgement.
- **Expired credentials / quota exhaustion.** The failure is obvious from the
  logs, but nobody was told which secret to rotate or whose quota to bump.

Each of these follows the same human workflow: **read the error → gather
context → classify → pick a remediation from a small known menu → act or
escalate**. That workflow is exactly what an LLM agent with the right tools and
guardrails can do. The goal of this feature is that when a pipeline fails,
Caesium dispatches an agent that triages the failure and either fixes it within
a bounded, auditable action policy, or escalates to a human *with the diagnosis
already done* — turning "3 a.m. page, 40 minutes of log spelunking" into either
no page at all, or a page that reads "vendor file 2h late, I've scheduled a
retry at 06:00 and paused downstream jobs; approve schema patch here."

Caesium is unusually well positioned for this. The data-plane-memory work
already built the *machine-readable diagnosis substrate* an agent needs —
`caesium why` (discriminating-input analysis), `run diff` (causal cache-bust
attribution), receipts (what exactly ran), lineage impact (blast radius), and
quarantined replay (a side-effect-free sandbox to *test* a hypothesis fix
before touching production). Most orchestrators would have to build that first.
We just have to hand it to an agent.

## Fit with Design Principles

1. **Container-native execution.** The agent itself is a container. Caesium
   does not embed an LLM, an SDK, or a vendor dependency; it launches a
   user-supplied agent image (Claude Code, a Claude Agent SDK app, or any
   OCI image that speaks our tool API) through the existing `atom.Engine`
   abstraction, exactly like any other task. BYO image, BYO model key
   (via the existing `secret://` machinery).
2. **Declarative and GitOps-first.** What the agent is *allowed* to do is
   declared in YAML (a `remediation` block on the job, plus shared
   `AgentProfile`/`Playbook` resources), linted by `caesium job lint`,
   reviewed in PRs. The most invasive remediation — changing the job
   definition — is expressed as a *proposed jobdef patch* that flows through
   the normal `diff`/`apply` (or a Git PR), never a live mutation.
3. **Zero-dependency simplicity.** No new infrastructure. Incidents, agent
   sessions, and actions are rows in the existing dqlite store; agent dispatch
   rides the existing event bus; the agent container is scheduled by the
   existing engines. A deployment that never configures an agent profile pays
   nothing.
4. **Smart by default.** Deterministic auto-remediation (typed retries,
   wait-for-file backoff) handles the cheap cases without any LLM call; the
   agent is invoked only for the long tail that deterministic policy can't
   classify.
5. **Data engineering first.** The action catalog is built around ETL verbs:
   wait for late data, quarantine bad rows via replay, propose schema
   evolution, suppress downstream noise via lineage.

## Overview

```
                     ┌─────────────────────────────────────────────┐
 event bus           │              Incident Manager               │
 task_failed ───────▶│  dedupe → classify → playbook match         │
 run_failed          │        │                                    │
 sla_missed          │        ├── deterministic rule? ──▶ act      │
 run_timed_out       │        └── else: open Incident              │
 schema_violation*   └────────────────┬────────────────────────────┘
                                      ▼
                     ┌─────────────────────────────────────────────┐
                     │              Agent Session                  │
                     │  launch agent container (atom.Engine)       │
                     │  + triage bundle (why/logs/lineage/receipt) │
                     │  + scoped short-lived token                 │
                     │  + tool surface (/v1/agent/* REST + MCP)    │
                     └────────────────┬────────────────────────────┘
                                      ▼
                 ┌────────────────────┼─────────────────────┐
                 ▼                    ▼                      ▼
          autonomous action    approval-gated action     escalate
          (tier 1/2, per       (tier 3: jobdef patch,   (Slack/PagerDuty
           playbook allowlist)  skip task, gate override) with RCA summary)
                 │                    │                      │
                 └──────────── AgentAction audit log ────────┘
                                      │
                              UI incident timeline
```

Everything the agent observes and does is recorded as typed `AgentAction`
rows — the incident timeline is replayable evidence, not chat scrollback.

## Scenario Walkthroughs

These are the acceptance narratives the design must satisfy.

### 1. Delayed vendor file

`extract-vendor-x` fails at 03:05: `s3://vendor-x/2026-07-03.csv: not found`.
The classifier tags it `data_unavailable` (log-pattern + exit-code heuristics).
The playbook for this class permits `snooze_retry`. The agent confirms from
run history that this vendor has been late 4 of the last 30 days (tool:
`list_runs`), checks lineage impact (`lineage/impact`) and sees three
downstream jobs, then schedules `retry_from_failure` at +45m with two
follow-ups, and posts a note to the channel: *"vendor-x file late again
(4th time this month); retrying at 03:50; downstream `reporting-daily` SLA
at 06:00 still safe (p95 runtime 22m)."* No page. If the 3rd retry fails, it
escalates with that history attached. A recurring pattern in the incident
record becomes evidence for a proposed cron change (tier 3, human-approved).

### 2. Schema drift

`transform` fails with `outputSchema` violations (`SchemaViolations` on the
`TaskRun`): new field `customerSegment`, and `customer_id` missing. The agent
inspects the violation detail and a sample of the step's captured output,
determines it is an additive rename, and *tests* the hypothesis with a
quarantined replay (`replay --set` a relaxed schema / mapping param) — zero
production side effects. The replay succeeds. Because `apply_jobdef_patch` is
tier 3, the agent files a **proposed patch** (updated `outputSchema` +
downstream `inputSchema`, rendered as a `jobdefs/diff` the human can read) and
pauses the job. The on-call gets one actionable approval card, not a debugging
session. On approval, Caesium applies the patch and retries the run.

### 3. Bad values

Nightly load fails on 3 rows of 10M (`invalid date '0000-00-00'`). Playbook
allows `retry_with_params` where the job has declared a quarantine parameter
(e.g. `defaultParams: {badRowPolicy: fail}` → agent may set
`badRowPolicy: quarantine`, because the param and its allowed override values
are whitelisted in the playbook). Agent retries with quarantine on, run
succeeds, agent attaches the quarantined-row count and sample to the incident
and opens a low-priority notification instead of a page.

### 4. Transient infra

`StartupFailure` pulling the image (registry 503). Deterministic rule — no
LLM needed: the classifier maps `atom.Result=StartupFailure` + retryable log
signature to `auto_retry_backoff` and re-runs. The agent is only engaged if
deterministic remediation exhausts. This tier keeps cost near zero for the
most common failure class.

### 5. Expired credential

Extract fails with 401. Playbook forbids autonomous action on `auth_failure`
(you don't want an agent hammering a locked account). Agent verifies the
failure is auth (not transient) with one read-only probe of the logs, walks
lineage to find every other job sharing that `secret://` ref, pre-emptively
pauses them (tier 2, allowed), and escalates with: which secret, which
provider, every affected job, and the unpause checklist.

## Backend Design

### Incident lifecycle

New package `internal/incident`. A subscriber on the existing event bus
(mirroring `internal/notification/subscriber.go`) consumes `task_failed`,
`run_failed`, `run_timed_out`, `sla_missed` (`internal/event/bus.go`) plus a
new `schema_violation_recorded` event emitted from
`internal/run/schema_validation.go` when mode is `warn` (in `fail` mode the
task failure already carries the violations).

Pipeline per event:

1. **Dedupe/correlate.** Key on `(job_id, task_name, failure_class)` with a
   cooldown window; a retry of the same failing task folds into the open
   incident rather than opening a new one. A `run_failed` following a
   `task_failed` we already own is correlated, not duplicated.
2. **Classify.** A deterministic, cheap classifier — no LLM — maps signals to
   a `failure_class`:
   - `atom.Result` (`StartupFailure`, `ResourceFailure` vs `Failure`)
   - `TaskRun.SchemaViolations` → `schema_violation`
   - `run_timed_out`/`sla_missed` → `sla_risk`
   - exit code + log-tail regex table (configurable, shipped with sane
     defaults) → `data_unavailable`, `auth_failure`, `oom`, `quota`, …
   - fallback → `unknown` (always agent-eligible)
3. **Playbook match.** Resolve the effective playbook (job-level
   `metadata.remediation` overriding profile defaults). If the class maps to a
   deterministic rule (`auto_retry_backoff`, `snooze_until_cron`), execute it
   directly and record it as an `AgentAction` with `actor=policy` — same audit
   trail, no container launch.
4. **Open incident + dispatch agent** if the playbook enables agentic triage
   for this class and attempt/budget counters permit.

Incident status machine:
`open → triaging → (awaiting_approval ↔ triaging) → remediated | escalated → closed`,
plus `suppressed` (deduped) and `abandoned` (budget exhausted). Terminal
verification: an incident only moves to `remediated` when a subsequent run (or
retried run) *succeeds*; a remediation that fails re-enters `triaging` against
the attempt budget.

**Loop safety.** Agent-initiated runs are stamped (a `RemediationIncidentID`
on `JobRun`, analogous to the existing `_trigger_depth` guard for event
chaining). A failure of an agent-initiated run increments the owning
incident's attempt counter instead of opening a new incident. Hard caps:
`maxAttempts` per incident, per-job concurrent-incident cap of 1, global
concurrent agent-session cap (env), and a cooldown after `abandoned`.

### Agent runtime: a container, like everything else

An agent session launches the profile's image through the existing
`atom.Engine` (docker/podman/kubernetes) — it *is* an atom run, so it
inherits logging, timeouts, resource limits, and receipts for free. Caesium
provides three things to the container:

1. **The triage bundle** — a JSON document mounted/env-injected at session
   start so the agent's first tokens are spent reasoning, not fetching:
   - incident + classification, `TaskRun.Error`, log tail (secret-redacted),
     `SchemaViolations`
   - `why` output for the failed task (`internal/run/why.go`)
   - the job definition and DAG topology, recent run history + durations
   - lineage impact for the task's output datasets
     (`internal/lineage/impact.go` `QueryImpact`)
   - the effective playbook: exactly which actions are allowed, which need
     approval, and remaining budgets — so the agent plans within policy
     instead of discovering rejections by trial.
2. **A scoped, short-lived credential.** A per-session API key (existing
   `APIKey` model, new `scope` claims) that expires with the session and is
   only valid for `/v1/agent/*` routes bound to this incident. Server-side
   enforcement is the security boundary — the prompt is not.
3. **The tool surface.** New route group:
   - `GET  /v1/agent/incidents/:id/bundle` — refreshed bundle
   - `GET  /v1/agent/incidents/:id/context/*` — read-only passthroughs
     (logs, why, run diff, receipt, lineage, run history) scoped to the
     incident's job + lineage neighborhood
   - `POST /v1/agent/incidents/:id/actions` — propose/execute a typed action
     (below)
   - `POST /v1/agent/incidents/:id/notes` — append findings to the timeline
   - `GET  /v1/agent/mcp` — the same tools exposed as a streamable-HTTP MCP
     server, so off-the-shelf agents (Claude Code, Agent SDK apps) connect
     with zero glue code. REST and MCP share one handler layer.

Because the tool surface is plain HTTP + MCP, "agent" is pluggable: teams can
run the reference image, their own harness, or in Phase 1, a human driving the
same tools from the UI.

### Action catalog: typed, server-enforced, tiered

The agent never gets shell, SQL, or generic HTTP against Caesium. Every
mutation is a typed action validated and executed server-side, mapping onto
machinery that already exists:

| Action | Tier | Maps to |
|---|---|---|
| `read_*` (logs, why, diff, receipt, lineage, history) | 0 — always | existing stores/controllers |
| `quarantine_replay` (what-if with `--set`) | 1 | `internal/replay` — already side-effect-free and `replaySafe`-gated |
| `snooze_retry` (schedule retry at T) | 1 | `store.RetryFromFailure` + a durable timer |
| `retry_from_failure` | 1 | `store.RetryFromFailure` (`internal/run/store.go`) |
| `retry_callbacks` | 1 | `Dispatcher.RetryFailed` |
| `notify` (post structured update to a channel) | 1 | `internal/notification` senders |
| `retry_with_params` (whitelisted params/values only) | 2 | run start with param overrides |
| `pause_job` / `unpause_job` (incl. lineage-adjacent jobs) | 2 | `Job.Paused`, `PUT /jobs/:id/pause` |
| `clear_cache_entry` | 2 | existing cache DELETE endpoints |
| `suppress_downstream_alerts` (while incident open) | 2 | notification policy interplay |
| `extend_sla_once` | 2 | SLA watcher interplay |
| `skip_task` (mark failed task skipped, honor trigger rules) | 3 | new store op |
| `override_schema_gate` (one run) | 3 | schema-validation bypass, recorded |
| `apply_jobdef_patch` (schema/image/cron change) | 3 | `jobdefs/diff` + `apply`, or a Git PR when git-sync provenance exists |
| `escalate` (page with RCA) | 1 | notification channels (Slack/PagerDuty/email/webhook) |

Tier semantics come from the playbook: tier 1 defaults to autonomous, tier 2
autonomous only if explicitly allowed, tier 3 always produces an
`ApprovalRequest` (never auto-executed in v1, regardless of config).
`quarantine_replay` deserves emphasis: it is the agent's **sandbox** — the
existing quarantine invariants (no production cache writes, no authoritative
lineage, no callbacks, no stats pollution; see
[`design-quarantined-replay.md`](design-quarantined-replay.md)) mean the agent
can *empirically verify* a hypothesis fix before proposing it. "I replayed
with the relaxed mapping and it succeeds" is a far stronger approval card than
"I think this will work."

### Declarative policy

Job-level opt-in (`pkg/jobdef/definition.go` `Metadata` gains a field):

```yaml
metadata:
  alias: vendor-x-daily
  remediation:
    profile: default-triage          # references an AgentProfile
    classes: [data_unavailable, schema_violation, transient_infra, unknown]
    maxAttempts: 2
    autonomy:
      allow: [retry_from_failure, snooze_retry, quarantine_replay, pause_job]
      paramOverrides:                 # whitelist for retry_with_params
        badRowPolicy: [quarantine]
      requireApproval: [apply_jobdef_patch, skip_task, override_schema_gate]
    escalation:
      channel: data-oncall            # NotificationChannel name
      after: 15m                      # wall-clock cap before forced escalation
```

`AgentProfile` is a small server-side resource (REST CRUD + optional YAML
apply, like notification channels today): agent image, engine, resource
limits, model credentials as `secret://` refs, session budget (wall-clock,
max tool calls), and default playbook. Shipped default profiles: a
`triage-only` profile (tier 0 + `escalate` — pure RCA, zero risk) so teams
can adopt incrementally.

Everything lints: `caesium job lint` validates action names, class names,
profile references, and that `paramOverrides` keys exist in
`defaultParams`.

### Approval gates

Tier-3 actions create an `ApprovalRequest` (incident-scoped), surface via
notification channels and the UI, and resolve via
`POST /v1/incidents/:id/approvals/:approval_id/{approve,reject}` (audited,
auth-scoped to an operator role). This is deliberately the same primitive
roadmap §3.2 needs for step-level approval gates — build it once here,
reuse it for `gate: {type: approval}` steps later. While an approval is
pending the incident parks in `awaiting_approval`; the agent session ends
(no idle container burning tokens) and a fresh session resumes on decision
if follow-up work is needed.

### Data model (new GORM models, `internal/models/`)

- `Incident` — id, job/run/task refs, `Class`, `Status`, dedupe key, attempt
  counter, opened/closed timestamps, resolution summary.
- `AgentSession` — incident ref, profile, atom-run ref (the agent container's
  own run record), token id, budget counters, terminal state.
- `AgentAction` — session ref (nullable for `actor=policy|human`), type,
  params JSON, tier, status (`proposed|approved|rejected|executed|failed`),
  result JSON, actor, timestamps. **This is the audit spine.**
- `ApprovalRequest` — action ref, approvers hint, decision, decider, expiry.
- `AgentProfile` — name, image/engine/limits, secret refs, budgets, defaults.

All flow through `models.All` AutoMigrate as usual; incidents and actions are
append-mostly and small.

### Dispatch wiring

The reserved-but-unimplemented `ChannelTypeAIAgent = "ai_agent"`
(`internal/models/notification.go:19`, accepted by `ValidChannelTypes()` but
with no registered sender in `cmd/start/start.go`) becomes real: an
`ai_agent` notification channel routes matched events into the incident
manager. This gives operators a second, policy-driven dispatch path — the
same `NotificationPolicy` matching (event types, job selectors) that routes
to Slack can route to the agent — while the `metadata.remediation` block
remains the job-owner-facing opt-in. Both converge on the same incident
pipeline.

### Config (env, `pkg/env/env.go`)

- `CAESIUM_AGENT_REMEDIATION_ENABLED` (default `false`) — master gate.
- `CAESIUM_AGENT_MAX_CONCURRENT_SESSIONS`, `CAESIUM_AGENT_SESSION_TIMEOUT`,
  `CAESIUM_AGENT_INCIDENT_COOLDOWN`.
- `CAESIUM_AGENT_DEFAULT_PROFILE` (optional bootstrap).

Feature-gated end to end: disabled ⇒ no subscriber registered, no routes
bound (reported by `GET /system/features`).

### CLI

```
caesium incident list [--status open] [--class schema_violation]
caesium incident get <id>            # timeline: observations, actions, evidence
caesium incident approve <id> --action <action_id>
caesium incident reject  <id> --action <action_id> [--reason ...]
caesium incident escalate <id>       # force hand-off, close agent session
caesium agent profile list|get|apply
```

Machine output (`--json`) goes to stdout, clean and parseable, per the
integration-test gate in the repo guidelines.

## Frontend Design (Caesium Console)

New feature dir `ui/src/features/incidents/`, plus surgical additions to the
existing job/run surfaces. Live updates ride the existing SSE `/events`
stream (new event types: `incident_opened`, `incident_status_changed`,
`agent_action_recorded`, `approval_requested`).

1. **Incidents page** (`/incidents`, nav-level). Filterable feed (status,
   class, job, needs-approval). Each row: job alias, failing task, class
   badge, status, age, and a one-line agent summary ("retry #2 scheduled
   03:50"). Open-incident and awaiting-approval counts surface as a badge in
   the top nav — this is the operator's morning-coffee view replacing the
   pager scroll.

2. **Incident detail: the triage timeline.** The centerpiece. A vertical
   timeline interleaving: the triggering failure, classification, each agent
   observation (notes with links to the *actual evidence* — the `TaskWhyView`,
   `LogViewer`, `LineageGraph`, `RunDiffView`, `ReplayDialog` components
   already exist and deep-link), each action (params, tier, actor, result),
   approval cards, and resolution. Design intent: a human arriving mid-incident
   understands in 30 seconds what happened and what the agent did, and every
   claim is a click away from primary evidence. Nothing is prose-only.

3. **Approval cards / inbox.** Tier-3 proposals render as decision cards:
   for `apply_jobdef_patch`, an inline YAML diff (reuse jobdefs diff
   rendering); for `retry_with_params`, the exact overrides; for `skip_task`,
   the DAG highlighting what downstream becomes reachable/skipped. Approve /
   reject with reason. A "Pending approvals" list also appears on the
   dashboard and as a filter on `/incidents`. Slack notifications deep-link to
   the card (interactive approve-from-Slack is a later phase).

4. **Run/task surfaces.** `RunDetailPage`/`TaskDetailPanel` gain an incident
   ribbon when a run belongs to an incident: status, link to timeline, and
   badges on agent-initiated runs ("retry by incident #42") so run history
   stays interpretable. The `JobDetailPage` shows incident history and a
   remediation-policy summary (effective playbook, read-only — policy is
   GitOps-owned).

5. **Agent activity transparency.** An incident in `triaging` shows the live
   session: elapsed wall-clock vs budget, tool calls used, and a streaming
   view of the agent container's own logs (it's an atom run — `LogViewer`
   already does this). A prominent **"Take over"** button escalates
   immediately, ends the session, and marks the incident human-owned. Trust
   is built by making the agent boring and observable, not magical.

6. **Fleet analytics** (stats page addition): incidents by class over time,
   autonomous-resolution rate, MTTR with/without agent, pages avoided,
   top recurring incidents ("vendor-x late 9× this month — consider moving
   the cron"), token/cost per profile if the image reports usage. These are
   also Prometheus metrics (`caesium_incidents_total{class,status}`,
   `caesium_agent_actions_total{type,tier,actor}`,
   `caesium_incident_resolution_seconds`).

## Security Posture

- **Server-enforced allowlist.** Policy lives in the playbook and is enforced
  in the action executor, not in the prompt. A confused or hijacked agent can
  only call typed endpoints its token scopes permit; tier-3 always terminates
  at a human.
- **Prompt-injection surface.** Log tails and vendor error messages are
  attacker-influenced input to the agent. Mitigations: the bundle wraps
  external content in clearly delimited untrusted blocks; tier boundaries are
  enforced regardless of agent "intent"; `apply_jobdef_patch` bodies are
  diffed and human-reviewed; agent may not modify playbooks, profiles,
  notification config, or auth — those aren't actions.
- **Secrets.** Bundles and log excerpts pass the existing redaction machinery
  (the secret-redacted `HashInputBlob` path); the agent token cannot read
  `/v1/database/*` or secret providers. Model API keys enter the agent
  container via `secret://` refs, never through Caesium's API.
- **Audit.** Every action row carries actor + params + result; approvals carry
  decider; agent-initiated runs are stamped. `AuditLog` entries mirror tier
  2/3 executions.
- **Blast-radius caps.** Per-incident attempt caps, per-job single open
  incident, global session cap, wall-clock and tool-call budgets, cooldowns
  after abandonment, and quarantined replay as the default place to try
  anything uncertain.

## Testing Strategy

Per the repo's end-to-end gate: every CLI command and REST endpoint above
ships with an integration test in `test/` driving the real surface. The
linchpin is a **deterministic fake agent image** (a small script image in
`build/`): it reads the triage bundle, asserts its shape, and emits a scripted
action sequence via the real `/v1/agent/*` API. Integration scenarios then
cover: dedupe/correlation, deterministic-rule short-circuit (no container
launched), tier-1 autonomous retry that turns a red run green, tier-3
approval flow end-to-end (propose → card → approve → apply → verify), budget
exhaustion → escalation, loop-guard (agent-initiated failure folds into the
incident), and disabled-gate inertness. The MCP surface gets a
protocol-level test client. UI flows (incident feed, timeline, approval card)
get Playwright e2e against the live backend, including the auth-enabled lane,
matching the data-plane-memory-ui precedent.

## Phasing

- **Phase 0 — Diagnosed pages (no LLM).** Incident model, classifier,
  dedupe, deterministic rules, escalation with the triage bundle attached,
  `/incidents` UI + CLI. Immediate value: every page arrives pre-triaged with
  why/logs/lineage links. This also hardens the substrate before any autonomy.
- **Phase 1 — Read-only agent (human in the loop).** Agent sessions run
  tier-0 tools only and produce an RCA + recommended action; a human clicks
  execute on the recommendation (which exercises the same action executor).
  Builds trust and a labeled corpus of (incident → good action).
- **Phase 2 — Bounded autonomy (agent in the loop).** Tier 1/2 autonomous
  execution per playbook, tier-3 approval flow, budgets, loop guards,
  take-over. This is the headline release.
- **Phase 3 — Learning and reach.** Jobdef patches as Git PRs on git-synced
  jobs; recurring-pattern proposals (cron shifts, retry tuning); cross-job
  coordination (lineage-aware downstream pausing/suppression as a first-class
  behavior); approve-from-Slack; optional step-level approval gates (roadmap
  §3.2) reusing the `ApprovalRequest` primitive.

## Non-Goals (v1)

- No embedded LLM, no model proxying, no Caesium-managed API keys.
- No free-form remediation: no shell on the server, no arbitrary HTTP, no
  DB access, no playbook self-modification.
- No auto-execution of tier-3 actions, even if configured.
- No cross-Caesium-cluster agents; one incident, one job's blast radius
  (lineage-adjacent pausing is the only cross-job action, and it is
  pause-only).
- Not a general incident-management product — incidents exist to drive
  remediation and hand off cleanly to Slack/PagerDuty, not to replace them.

## Open Questions

1. **Reference agent image.** Ship a maintained `caesiumcloud/triage-agent`
   (Claude Agent SDK–based, MCP client) or only document the contract?
   Leaning: ship it, gated behind "you provide the model key," because the
   empty-image cold-start will otherwise stall adoption.
2. **Bundle size vs freshness.** Pre-baked bundle keeps sessions cheap and
   deterministic; large log/lineage graphs may need paging through the
   read-only tools. Likely both: small bundle + tools for depth.
3. **`skip_task` semantics.** Marking a failed task skipped interacts with
   trigger rules (`all_success` vs `all_done`) and cache identity; needs its
   own mini-design before tier-3 implementation.
4. **Multi-tenancy interplay.** When namespaces land (roadmap §3.1),
   profiles/playbooks and incident visibility must scope per namespace;
   design the models with a nullable `namespace` column from day one.
5. **Learning loop.** Phase-1 recommendations + human decisions form a
   labeled dataset. Do we surface it (exportable) so teams can tune playbooks,
   or keep it internal analytics only?
