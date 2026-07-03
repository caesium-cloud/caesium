# Agent-in-the-Loop ETL Remediation — Autonomous Failure Triage & Bounded Remediation

Last updated: 2026-07-03

When a pipeline fails today, Caesium retries per declared policy and then pages a
human, who reads logs, classifies the failure, picks a remediation from a small
known menu (wait / retry / patch / escalate), and acts. This plan builds the
substrate and the autonomy to do that workflow in software: a **leader-gated
incident manager** subscribes to the event bus, a cheap deterministic classifier
buckets each failure into a `failure_class`, and — where a declarative,
tiered, **server-enforced** policy permits — a **container-native agent** (BYO
image, BYO model key, launched through the existing `atom.Engine`) triages the
incident using Caesium's causal primitives (`why`, run diff, receipts, lineage
impact, quarantined replay as a what-if sandbox) and either remediates within a
bounded action catalog or escalates to a human *with the diagnosis already done*.

The design is explicitly **phased** and the streams below map onto it: **Phase 0**
(no LLM) ships the incident model, classifier, dedupe, deterministic rules, the
escalation-with-triage-bundle path, and the `/incidents` UI + CLI — plus the
load-bearing substrate every later phase rides on (`TaskRun.ExitCode` capture, a
free-text log scrubber, a persisted-timer store, and the action-executor
skeleton). **Phase 1** runs read-only agent sessions (human clicks execute).
**Phase 2** is the headline: bounded tier-1/2 autonomy, the tier-3 approval flow,
budgets, loop guards, and take-over. **Phase 3** completes the jobdef-patch Git-PR
route and recurring-pattern proposals. Nothing new is required infrastructurally:
incidents, sessions, and actions are rows in the existing dqlite store, dispatch
rides the existing event bus, and a deployment that never configures an agent
profile pays nothing (the whole feature is gated behind
`CAESIUM_AGENT_REMEDIATION_ENABLED`, default `false`).

Every new CLI verb and REST endpoint ships with an integration test in `test/`
that drives the real surface against a live server (per the `CLAUDE.md`
end-to-end-coverage gate); the linchpin is a **deterministic fake agent image**
(`build/`) that reads the bundle, asserts its shape, and emits a scripted action
sequence through the real `/v1/agent/*` API — a unit test that hand-feeds the
classifier proves the classifier, never the wiring.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work backlog,
`## Sequencing & Dependencies` captures cross-stream order, and
`## Acceptance Criteria` lists the gates that close out the entire plan. Any
agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies are
   satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Source-Of-Truth Note

This plan implements [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md).
**The design doc is authoritative for INTENT and SCOPE** — when this plan and the
design disagree on what a stream must do (the incident lifecycle, the tiered
action catalog and its tier semantics, the security posture, the phasing
boundaries), the **design wins** and the item is reconciled to match. No item may
add a new action type, endpoint, tier, or config knob beyond what the design
enumerates without first amending the design doc. Strategic priority/status is
tracked in [`docs/roadmap.md`](../../roadmap.md) §3.5 Agent-in-the-Loop ETL
Remediation (the roadmap wins on priority/status disagreements). The
job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go); the
`metadata.remediation` block is a **policy addition to `Metadata`, validated at
lint/apply** — it does **not** participate in step-execution cache identity, so
`internal/cache/hash.go` is untouched (Stream E states this negative explicitly).

Two companion designs are load-bearing dependencies the design itself calls out,
and this plan honors them: the quarantine invariants that make `quarantine_replay`
a safe experiment harness are owned by
[`design-quarantined-replay.md`](../../design-quarantined-replay.md) (the
`replaySafe` gate defaults to `false` and the playbook cannot widen it), and full
`oom` classification depends on the OOM-detection substrate
[`design-resource-right-sizing.md`](../../design-resource-right-sizing.md) builds
in its Phase 0 (`atom.ResourceFailure` is defined at
`internal/atom/*` but **never produced** today — every engine maps exit 137 to
`Killed`). This plan captures the raw `TaskRun.ExitCode` (design Phase 0) so the
regex/exit-code classifier works; the `oom` class stays best-effort until
resource-right-sizing lands its detection, and that limit is recorded, not hidden.

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from the
`design-agent-in-the-loop.md` design of record; the first wave is the next
eligible run of the `exec-plan-wave` skill against this doc. The design is
explicitly phased (0→3); the recommended first wave lands Stream A (Phase-0
incident core + substrate) plus the harness fake-agent image, which unblocks
every other stream.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Incident core + Phase-0 substrate — 5 GORM models, `TaskRun.ExitCode`, classifier, leader-gated dedupe subscriber, log scrubber, persisted-timer store, feature gate | **P0** | Not started |
| B | Action executor + tiered catalog + retry safety valves + `apply_jobdef_patch` provenance router | P1 | Not started |
| C | Agent runtime — session supervisor (atom.Engine), triage bundle, scoped short-lived token, `/v1/agent/*` REST tool surface | P1 | Not started |
| D | Approval gates + incident REST reads + `ai_agent` dispatch channel | P1 | Not started |
| E | Declarative policy — jobdef `metadata.remediation` block + `AgentProfile` CRUD + offline/server lint | P1 | Not started |
| F | MCP surface — `/v1/agent/mcp` JSON-RPC/streamable-HTTP over the same handlers | P2 | Not started |
| G | CLI — `caesium incident …` + `caesium agent profile …` | P2 | Not started |
| U | Console incidents surface — feed, triage timeline, approval cards, run/task ribbons, agent activity, fleet analytics | P1 | Not started |
| H-1 | Harness — deterministic fake agent image + integration server gate wiring | — | Not started |
| N-1 | Docs — roadmap §3.5 flip, design banner, schema reference (`remediation` block), examples, README | — | Not started |

## Streams

### Stream A — Incident core + Phase-0 substrate

The foundation every other stream builds on, and the entirety of the design's
Phase-0 spine. It owns the five new GORM models, the deterministic classifier,
the leader-gated dedupe subscriber and status machine, and the three pieces of
substrate the design is explicit that Phase 0 must build before any autonomy: the
`TaskRun.ExitCode` capture, the free-text log scrubber, and the persisted-timer
store. Largest blast radius (models + engines + `cmd/start/start.go` + `env.go` +
`metrics.go`), so it merges first.

- [ ] A1. Add the five new GORM models plus the persisted-timer model, registered
      in `models.All`. `Incident` (job/run/task refs, `Class`, `Status`, dedupe
      key, attempt counter, opened/closed ts, resolution summary),
      `AgentSession` (incident ref, profile, engine + container/atom id, persisted
      session log, token id, budget counters, terminal state — **deliberately not
      a `JobRun`**), `AgentAction` (**non-nullable** incident ref so the timeline
      reconstructs for `actor=policy|human` rows, nullable session ref, type,
      params JSON, tier, status `proposed|approved|rejected|executed|failed`,
      result JSON, actor, ts — the audit spine), `ApprovalRequest` (action ref,
      approvers hint, decision, decider, expiry), `AgentProfile` (name,
      image/engine/limits, `secret://` refs, budgets, defaults), plus a
      `RemediationTimer` (durable snooze row backing B/`snooze_retry`). Every
      model carries a **nullable `namespace` column from day one** (design Open
      Question 4). These are append-mostly, small, NOT hot per-run tables — do
      **not** add them to `hotPathModels()`/`hotTables`.
      Files: new `internal/models/incident.go`, new
      `internal/models/agent_session.go`, new `internal/models/agent_action.go`,
      new `internal/models/approval_request.go`, new
      `internal/models/agent_profile.go`, new `internal/models/remediation_timer.go`,
      `internal/models/models.go`.
- [ ] A2. Capture the raw exit code and emit the schema-violation event the
      classifier reads. Add an `ExitCode` column to `TaskRun`
      (`internal/models/run.go`, beside the existing `SchemaViolations` at
      `run.go:119`) populated at task completion in **all three engines** (each
      folds the raw code into `atom.Result` and discards it today); add a new
      `schema_violation_recorded` event type on the bus emitted from
      `internal/run/schema_validation.go` when mode is `warn` (in `fail` mode the
      task failure already carries the violations). Note the honest limit: full
      `oom` classification still needs the OOM-flag detection
      `design-resource-right-sizing.md` builds — this item ships the `ExitCode`
      substrate, not the OOM flag.
      Files: `internal/models/run.go`, `internal/atom/docker/engine.go`,
      `internal/atom/kubernetes/engine.go`, `internal/atom/podman/engine.go`,
      `internal/run/schema_validation.go`, `internal/event/bus.go`.
- [ ] A3. Implement the deterministic classifier (**no LLM**): map `atom.Result`
      (`StartupFailure`/`ResourceFailure` → `transient_infra`),
      `TaskRun.SchemaViolations` → `schema_violation`, `run_timed_out`/`sla_missed`
      → `sla_risk`, and a **configurable exit-code + log-tail regex table**
      (shipped with sane defaults) → `data_unavailable`, `auth_failure`, `oom`,
      `quota`, with `unknown` fallback (always agent-eligible). Pure logic, fully
      unit-tested (each class, the regex table, the fallback).
      Files: new `internal/incident/classifier.go` (+ `classifier_test.go`).
      Depends on: A2 (the `ExitCode` column + `schema_violation_recorded` event).
- [ ] A4. Add the leader-gated incident subscriber, dedupe/correlation engine,
      status machine, and incident store, wired at startup behind the master
      gate. The subscriber consumes `task_failed`, `run_failed`, `run_timed_out`,
      `sla_missed`, and `schema_violation_recorded`; it is **leader-gated**
      (`LeaderCheck: dqlite.IsLocalLeader`, as the run-queue dequeuer is — unlike
      `internal/notification/subscriber.go`, which runs on every node) so an
      N-node cluster opens one incident per failure, and incident-open is an
      **atomic conditional insert on the dedupe key** so failover races and the
      SLA watcher's per-node `sla_missed` duplicates can't open twins.
      Correlation key `(job_id, task_name, failure_class)` with the design's
      run-boundary rules (declared retries + the trailing `run_failed` fold in;
      agent-initiated runs stamped `RemediationIncidentID` fold in; an independent
      new failure with an open incident appends an **occurrence** and advances the
      remediation target; cooldown only after close; backfill storm-control keyed
      on backfill id). Status machine
      `open → triaging → (awaiting_approval ↔ triaging) → remediated | escalated → closed`
      plus `suppressed`/`abandoned`, with terminal verification (only reach
      `remediated` when a subsequent run succeeds). Add
      `CAESIUM_AGENT_REMEDIATION_ENABLED` (default `false`, master gate),
      `CAESIUM_AGENT_INCIDENT_COOLDOWN`, and surface the flag in the `Features`
      struct (`api/rest/service/system/system.go`). Add
      `caesium_incidents_total{class,status}` and
      `caesium_incident_resolution_seconds` (both edit sites: the `var(...)` block
      + `Register()`).
      Files: new `internal/incident/subscriber.go`, new
      `internal/incident/store.go`, new `internal/incident/incident.go`,
      `cmd/start/start.go`, `pkg/env/env.go`,
      `api/rest/service/system/system.go`, `internal/metrics/metrics.go`.
      Depends on: A1 + A3.
      Acceptance probe (integration): a `task_failed` on a gated job opens exactly
      one incident on a single-node server; a second independent failure with the
      same key appends an occurrence rather than opening a twin.
- [ ] A5. Build the free-text log scrubber. Free-text scrubbing does not exist
      today (`HashInputBlob` redacts structured env maps at hash time; run logs
      are served raw), so add a scrubber that does exact removal of every resolved
      `secret://` value in the task's env plus high-entropy token heuristics, run
      before any log text enters a triage bundle, an agent-readable endpoint, or an
      escalation message. Until the scrubber is in, bundles carry no raw log text.
      Files: new `internal/incident/scrubber.go` (+ `scrubber_test.go`).
- [ ] A6. Add the persisted-timer supervisor behind the durable `RemediationTimer`
      model — every existing delay in Caesium is an in-process `time.NewTimer`
      lost on restart/failover, so `snooze_retry` needs a durable row and a
      leader-gated sweeper that fires due timers. Timers are **owned by their
      incident** and **canceled on any terminal transition or human take-over** (a
      stale timer can never fire a retry against a closed incident). Wire the
      sweeper into `cmd/start/start.go` behind the master gate.
      Files: new `internal/incident/timer.go` (+ `timer_test.go`),
      `cmd/start/start.go`.
      Depends on: A1 (the `RemediationTimer` model) + A4 (incident ownership /
      terminal-cancel hook).

### Stream B — Action executor + tiered catalog + provenance router

The typed, server-enforced action layer. The agent never gets shell, SQL, or
generic HTTP; every mutation is a typed action validated and executed
server-side, mapping onto machinery that already exists. This stream owns the
executor, the deterministic rules that record `AgentAction actor=policy` rows
(Phase 0), the full tier-1/2 catalog (Phase 2), and the tier-3 producers routed
through the approval gate. Tier semantics come from the playbook: tier 1 defaults
autonomous, tier 2 autonomous only if explicitly allowed, **tier 3 always
produces an `ApprovalRequest`, never auto-executed in v1 regardless of config**.

- [ ] B1. Add the action-executor skeleton + the deterministic rules. The executor
      validates a typed action against the effective playbook and executes it
      server-side, recording every attempt as an `AgentAction` row with the right
      `actor`/`tier`/`status`. Phase 0 deterministic rules (`auto_retry_backoff`,
      `snooze_until_cron`) execute directly and record `actor=policy` — same audit
      trail, no container launch. Add `caesium_agent_actions_total{type,tier,actor}`
      (both edit sites) and mirror tier-2/3 executions into `AuditLog`.
      Files: new `internal/incident/executor.go` (+ `executor_test.go`), new
      `internal/incident/rules.go`, `internal/metrics/metrics.go`.
      Depends on: A1 (models) + A4 (incident store).
- [ ] B2. Add the two retry safety valves the underlying store call lacks.
      `store.RetryFromFailure` (`internal/run/store.go:4642`) flips a terminal run
      back to `running` **without** concurrency admission (`admit()` runs only on
      new-run creation) and **without** consulting `Job.Paused`. The executor's
      retry actions (`retry_from_failure`, `snooze_retry`, `retry_callbacks` via
      `Dispatcher.RetryFailed` at `internal/callback/callback.go:126`) must refuse
      while the job is paused (a human pause outranks the agent) and re-admit
      against `metadata.concurrency` using **`queue` semantics regardless of the
      job's declared strategy** (an agent action must never `replace`-cancel a live
      run nor race a cron tick for a slot it was never granted).
      Files: `internal/incident/executor.go`, `internal/run/store.go`
      (an admit-aware retry entry point).
      Depends on: B1.
- [ ] B3. Implement the tier-1/2 typed action catalog: `snooze_retry` (deferred
      `retry_from_failure` on a persisted `RemediationTimer`), `retry_from_failure`,
      `retry_callbacks`, `notify` (structured channel update via
      `internal/notification` senders), `quarantine_replay` (what-if with `--set`
      params-only, hard-gated on baseline `replaySafe` via `internal/replay` — the
      playbook cannot widen the gate), `rerun_with_params` (**whitelisted
      params/values only; new-run semantics — params feed `HashInput` so the DAG
      re-keys and recomputes, and the action record discloses that cost**),
      `pause_job`/`unpause_job` (incl. lineage-adjacent jobs, with agent-pause TTL +
      terminal disposition so an abandoned incident never leaves jobs paused),
      `clear_cache_entry`, `suppress_downstream_alerts`, and `extend_sla_once`
      (a **durable per-run SLA override** — the watcher's miss-dedupe is per-process
      memory, so an in-memory extension would still page from other nodes).
      Files: `internal/incident/executor.go`, new
      `internal/incident/actions.go` (+ `actions_test.go`).
      Depends on: B1 + B2 + A6 (persisted timer for `snooze_retry`).
- [ ] B4. Implement the tier-3 producers, all routed through the approval gate
      (Stream D) and **never auto-executed**: `skip_task` (new store op, honoring
      trigger rules — flag its `all_success`/`all_done` + cache-identity
      interaction per design Open Question 3), `override_schema_gate` (one-run
      schema-validation bypass, recorded), and `apply_jobdef_patch` —
      **provenance-routed, enforced server-side**: for git-synced jobs a direct DB
      apply is rejected (the next sync would revert it) and the approved patch flows
      as a Git PR (or degrades to `escalate` with the rendered diff when no git
      write credentials exist); only jobs without git provenance take the direct
      `jobdefs/diff` + `apply` path. The agent cannot choose the route; the executor
      derives it from the `Job` model's git fields. (Phase 2 ships the direct path;
      the Git-PR route completes in Phase 3 — record which half lands.)
      Files: `internal/incident/actions.go`, `internal/run/store.go`
      (`skip_task` op), new `internal/incident/provenance.go` (patch router).
      Depends on: B3 + D1 (the `ApprovalRequest` flow tier-3 actions create).

### Stream C — Agent runtime: session supervisor + triage bundle + scoped token + `/v1/agent/*` REST

The container runtime and its tool surface. An agent session launches the
profile's image through the existing `atom.Engine` but is **deliberately not a
`JobRun`/`TaskRun`** (a session materialized as a run would pollute the eight
`quarantine IS NOT TRUE` stats filters and feed the incident bus its own
exhaust). This stream owns the session supervisor, the triage bundle, the new
scoped-token enforcement, and the `/v1/agent/*` REST tool surface.

- [ ] C1. Add the scoped, short-lived agent credential. Today's `KeyScope`
      (`internal/models/api_key.go:74`) carries only a job-alias list checked by a
      deny-by-default route switch (`api/middleware/auth_scope.go`), so this is new
      enforcement work: add a per-session agent claim type plus explicit switch
      arms for the `/v1/agent/*` routes, valid only for the bound incident and
      expiring with the session. The read scope is **frozen at incident open** —
      the incident manager (an unscoped server-side principal) snapshots the
      lineage-impact graph (`internal/lineage/impact.go` `QueryImpact`) into a
      static job allowlist on the incident, **excluding edges derived from the
      failing run's own outputs** so attacker-crafted `##caesium::output` refs
      can't widen it. Scoped principals stay 403'd from live `/v1/lineage/impact`;
      the agent reads the frozen snapshot from its bundle. Server-side enforcement
      is the boundary — the prompt is not.
      Files: `internal/models/api_key.go`, `api/middleware/auth_scope.go`,
      `internal/incident/store.go` (freeze the allowlist at open).
      Depends on: A1 + A4.
- [ ] C2. Add the session supervisor. A small supervisor drives the profile image
      through `atom.Engine` directly (create → wait → logs → stop) with wall-clock
      enforcement and persisted session logs for the UI, materialized as an
      `AgentSession` record, **not** a run. In distributed mode it runs on the
      leader node in v1 (worker-pool placement is a later refinement). Enforce the
      per-job concurrent-session cap (default 1) and the global session cap in the
      **leader-gated dispatcher** (not per-process). Add
      `CAESIUM_AGENT_MAX_CONCURRENT_SESSIONS`, `CAESIUM_AGENT_SESSION_TIMEOUT`,
      `CAESIUM_AGENT_DEFAULT_PROFILE`. Wire the dispatcher into
      `cmd/start/start.go` behind the master gate.
      Files: new `internal/incident/session.go` (+ `session_test.go`),
      `pkg/env/env.go`, `cmd/start/start.go`.
      Depends on: A1 + A4 + C1.
- [ ] C3. Build the triage bundle + `GET /v1/agent/incidents/:id/bundle`. A JSON
      document the agent fetches once at startup (env injection can't carry it —
      log tails are capped at 1 MiB, over the ~128 KiB per-variable limit — and
      engine mounts can't deliver a server-generated file to a remote kubelet):
      incident + classification, `TaskRun.Error`, **scrubbed** log tail (via A5),
      `SchemaViolations`, `why` output (`internal/run/why.go`), the job definition
      + DAG topology, recent run history + durations, the **frozen** lineage-impact
      snapshot (C1), and the effective playbook (exactly which actions are allowed,
      which need approval, remaining budgets) so the agent plans within policy.
      Files: new `internal/incident/bundle.go` (+ `bundle_test.go`), new
      `api/rest/controller/agent/bundle.go`, new `api/rest/service/agent/`,
      `api/rest/bind/bind.go`.
      Depends on: A4 + A5 + C1.
- [ ] C4. Add the rest of the `/v1/agent/*` tool surface, all scoped to the
      incident's frozen job allowlist: `GET /v1/agent/incidents/:id/context/*`
      (read-only passthroughs — logs, why, run diff, receipt, run history),
      `POST /v1/agent/incidents/:id/actions` (propose/execute a typed action via
      the Stream B executor), and `POST /v1/agent/incidents/:id/notes` (append
      findings to the timeline). Every route enforced by the C1 scope switch.
      Files: new `api/rest/controller/agent/actions.go`, new
      `api/rest/controller/agent/context.go`, new
      `api/rest/controller/agent/notes.go`, `api/rest/service/agent/`,
      `api/rest/bind/bind.go`.
      Depends on: C1 + C3 + B1 (the executor the actions endpoint calls).

### Stream D — Approval gates + incident REST reads + `ai_agent` dispatch channel

The human-decision layer, the operator read API, and the second dispatch path.
The approval gate is deliberately the same primitive roadmap §3.2 needs for
step-level approval gates — build it once here, reuse it later.

- [ ] D1. Add the tier-3 approval flow. Tier-3 actions create an incident-scoped
      `ApprovalRequest`; resolve via
      `POST /v1/incidents/:id/approvals/:approval_id/{approve,reject}` (audited,
      auth-scoped to an operator role). Two hard preconditions keep "tier 3 always
      terminates at a human" true: the approval endpoints **reject agent session
      tokens outright**, and — because Caesium defaults to
      `CAESIUM_AUTH_MODE=none` (no auth middleware attached) — the master gate
      **refuses to enable anything beyond Phase-0 diagnosed pages unless an auth
      mode is active** (otherwise the approve route is an unauthenticated POST the
      agent container itself could call). While an approval is pending the incident
      parks in `awaiting_approval` and the agent session ends (no idle container
      burning tokens); a fresh session resumes on decision if needed.
      Files: new `api/rest/controller/incident/approvals.go`, new
      `api/rest/service/incident/`, `api/rest/bind/bind.go`,
      `api/middleware/auth_scope.go` (reject agent tokens on approval routes),
      `pkg/env/env.go` (auth-mode precondition in `validate()`).
      Depends on: A1 + A4.
- [ ] D2. Add the incident operator read API: `GET /v1/incidents`
      (filter by status/class/job/needs-approval, bounded + paginated),
      `GET /v1/incidents/:id` (the full timeline — observations, actions,
      approvals, evidence links), and the SSE event types the UI subscribes to
      (`incident_opened`, `incident_status_changed`, `agent_action_recorded`,
      `approval_requested`) emitted on the existing `/events` stream.
      Files: `api/rest/controller/incident/`, `api/rest/service/incident/`,
      `api/rest/bind/bind.go`, `internal/event/bus.go` (new SSE event types).
      Depends on: A1 + A4.
- [ ] D3. Make the reserved `ChannelTypeAIAgent = "ai_agent"`
      (`internal/models/notification.go:19`, accepted by `ValidChannelTypes()` but
      with no registered sender) real: register an `ai_agent` sender in
      `cmd/start/start.go` (beside the webhook/slack/email/pagerduty registrations
      at `start.go:430-433`) that routes matched events into the incident manager,
      so the same `NotificationPolicy` matching that routes to Slack can route to
      the agent — a second, policy-driven dispatch path converging on the same
      incident pipeline as `metadata.remediation`.
      Files: new `internal/notification/sender_aiagent.go`, `cmd/start/start.go`,
      `internal/notification/subscriber.go` (sender registration seam).
      Depends on: A4.

### Stream E — Declarative policy: jobdef `remediation` block + `AgentProfile` CRUD + lint

What the agent is allowed to do is declared in YAML and reviewed in PRs. This
stream owns the job-schema addition and the server-side profile resource.

- [ ] E1. Add the `metadata.remediation` block to the job definition:
      `profile`, `classes`, `maxAttempts`, `autonomy` (`allow`, `paramOverrides`
      whitelist, `perClass` narrowing, `requireApproval`), and `escalation`
      (`channel`, `after`). Extend `Metadata` in `pkg/jobdef/definition.go` +
      `Validate()` + `pkg/jobdef/schema.go`. **Offline `caesium job lint`** (which
      calls `ValidateTriggerChains` with a nil DB) validates everything knowable
      from local YAML — action names, class names, and that `paramOverrides` keys
      exist in `defaultParams` — and emits a **scope note** that `profile:`
      references are unverified offline (they are server-side state). **No
      `internal/cache/hash.go` change**: the remediation block is policy metadata,
      not step-execution input, so cache identity is untouched (the
      `rerun_with_params` override cost is honest new-run semantics through the
      existing `HashInput`, which already hashes params).
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`,
      `internal/jobdef/runtime/spec.go` (carry the block through), `cmd/job/lint.go`.
- [ ] E2. Add the `AgentProfile` server-side resource: REST CRUD (like notification
      channels — image/engine/limits, `secret://` model-credential refs, session
      budgets, default playbook), **server-side lint** (`POST /v1/jobdefs/lint`
      verifies `profile:` references) enforced inside the apply transaction, and the
      shipped default profiles (`triage-only`: tier 0 + `escalate`, zero-risk, so
      teams adopt incrementally). Record the GitOps-YAML-apply path as an explicit
      open question (would mean extending the jobdef `Kind` system beyond `Job`).
      Files: new `api/rest/controller/agentprofile/`, new
      `api/rest/service/agentprofile/`, `api/rest/bind/bind.go`,
      `api/rest/controller/jobdef/` (server-side profile-ref lint).
      Depends on: A1 (the `AgentProfile` model).

### Stream F — MCP surface

- [ ] F1. Expose the same tool surface over MCP's streamable-HTTP transport at
      `/v1/agent/mcp` (POST-carried JSON-RPC 2.0 with an optional SSE channel) so
      off-the-shelf agents (Claude Code, Agent SDK apps) connect without glue code.
      Honest cost: Caesium has no JSON-RPC/MCP dependency today, so this is a small
      vendored MCP server layer or hand-rolled JSON-RPC 2.0 inside Echo over the
      **same handler layer** as Stream C, plus **exempting the route from the
      server's 30s write timeout**. REST ships first (Stream C); MCP follows.
      Files: new `api/rest/controller/agent/mcp.go`, new `internal/mcp/`
      (JSON-RPC layer), `api/rest/bind/bind.go`, `api/api.go` (write-timeout
      exemption for the MCP route).
      Depends on: C4.

### Stream G — CLI

- [ ] G1. Add the `caesium incident` command group: `list [--status] [--class]`,
      `get <id>` (timeline: observations, actions, evidence), `approve <id>
      --approval <id>`, `reject <id> --approval <id> [--reason]`, `escalate <id>`
      (force hand-off, close the agent session). Machine output (`--json`) goes to
      **stdout, clean and parseable, captured separately from stderr** per the
      integration gate; append the group to the `cmds` slice in `cmd/execute.go`.
      Files: new `cmd/incident/`, `cmd/execute.go`.
      Depends on: D1 + D2.
- [ ] G2. Add the `caesium agent profile` command group: `list`, `get`, `apply`,
      appended to `cmd/execute.go`. `--json` to clean stdout.
      Files: new `cmd/agent/`, `cmd/execute.go`.
      Depends on: E2.

### Stream U — Console incidents surface

New feature dir `ui/src/features/incidents/` plus surgical additions to the
existing job/run surfaces. Live updates ride the existing SSE `/events` stream
(the new event types from D2). Every claim in the timeline is one click from
primary evidence — nothing is prose-only.

- [ ] U1. Add the Incidents page (`/incidents`, nav-level): filterable feed
      (status, class, job, needs-approval), each row showing job alias, failing
      task, class badge, status, age, and a one-line agent summary; open-incident +
      awaiting-approval counts as a top-nav badge. Add the route + nav entry + an
      `api.ts` method.
      Files: new `ui/src/features/incidents/IncidentsPage.tsx`,
      `ui/src/router.tsx`, `ui/src/components/layout/Sidebar.tsx`,
      `ui/src/lib/api.ts`.
      Depends on: D2.
- [ ] U2. Add the incident-detail triage timeline (the centerpiece): a vertical
      timeline interleaving the triggering failure, classification, each agent
      observation (notes deep-linking to the existing `TaskWhyView`, `LogViewer`,
      `LineageGraph`, `RunDiffView`, `ReplayDialog` components), each action
      (params, tier, actor, result), approval cards, and resolution.
      Files: new `ui/src/features/incidents/IncidentDetailPage.tsx`,
      `ui/src/router.tsx`.
      Depends on: U1.
- [ ] U3. Add approval cards / inbox: tier-3 proposals render as decision cards —
      `apply_jobdef_patch` as an inline YAML diff (reuse jobdefs diff rendering),
      `rerun_with_params` with the exact overrides + the recompute-cost disclosure,
      `skip_task` with the DAG highlighting what becomes reachable/skipped —
      approve/reject with reason; a "Pending approvals" list on the dashboard and as
      a `/incidents` filter.
      Files: new `ui/src/features/incidents/ApprovalCard.tsx`,
      `ui/src/features/incidents/IncidentDetailPage.tsx`, `ui/src/lib/api.ts`.
      Depends on: U2 + D1.
- [ ] U4. Add the run/task incident ribbons and agent-activity view: a
      `RunDetailPage`/`TaskDetailPanel` incident ribbon (status, timeline link,
      "retry by incident #42" badges on agent-initiated runs); a `JobDetailPage`
      incident history + read-only remediation-policy summary; and a live
      `triaging` view (elapsed wall-clock vs budget, tool calls used, streaming
      agent-container logs via the existing `LogViewer`) with a prominent **"Take
      over"** button that ends the session and marks the incident human-owned.
      Files: `ui/src/features/runs/RunDetailPage.tsx`,
      `ui/src/features/jobs/JobDetailPage.tsx`, new
      `ui/src/features/incidents/AgentActivity.tsx`.
      Depends on: U2.
- [ ] U5. Add the fleet-analytics additions to the stats page: incidents by class
      over time, autonomous-resolution rate, MTTR with/without agent, pages
      avoided, top recurring incidents, and token/cost per profile if the image
      reports usage — driven by the D2 reads and the
      `caesium_incidents_total`/`caesium_agent_actions_total`/`caesium_incident_resolution_seconds`
      metrics.
      Files: `ui/src/features/stats/StatsPage.tsx`, new
      `ui/src/features/stats/components/IncidentAnalytics.tsx`, `ui/src/lib/api.ts`.
      Depends on: U1.

## Harness Strengthening

- [ ] H-1. Build the deterministic **fake agent image** (`build/`) — the linchpin
      of the integration suite: a small script image that reads the triage bundle,
      asserts its shape, and emits a **scripted action sequence** via the real
      `/v1/agent/*` API (and, once Stream F lands, an MCP protocol-level test
      client). Wire the integration server so the event path executes in CI: set
      `CAESIUM_AGENT_REMEDIATION_ENABLED=true`, an active `CAESIUM_AUTH_MODE` (the
      approval-gate precondition), a bootstrap `CAESIUM_AGENT_DEFAULT_PROFILE`
      pointing at the fake image, and low session/attempt caps on the
      `just integration-up` server + CI job, so scenarios drive the live surface
      rather than an internal call (mirroring the lineage
      `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the `CLAUDE.md` gate names).
      Files: new `build/Dockerfile.triage-agent` (+ the agent script),
      `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [ ] N-1. Flip `docs/roadmap.md` §3.5 to reflect shipped phases; update the
      [`design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md)
      `> Status:` banner from "Brainstorm/Design — no implementation yet" to the
      shipped phase(s), and reconcile any design amendments this plan surfaced back
      into the doc body; document the `metadata.remediation` block
      (profile/classes/autonomy/escalation) in `docs/job-schema-reference.md`,
      `docs/job-definitions.md`, and `docs/caesium-job-llm-reference.md`; add a
      remediation-enabled `docs/examples/*.job.yaml` (pinned image); index this plan
      in `docs/README.md` **in backtick/inline-code form** (the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects clickable
      subdirectory links). Runs last.
      Files: `docs/roadmap.md`, `docs/design-agent-in-the-loop.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–U + F + G (runs last, after the runtime ships).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — every other stream consumes its models,
  incident store, classifier, scrubber, or persisted timer. A merges first
  (largest blast radius: models + all three engines + `cmd/start/start.go` +
  `env.go` + `metrics.go`).
- **Stream B** (executor) depends on A1 + A4; B3 also needs A6 (persisted timer);
  B4 (tier-3 producers) depends on D1 (the approval flow tier-3 actions create).
- **Stream C** (agent runtime) depends on A1 + A4; C1 (scoped token) gates C2/C3/C4;
  C4's actions endpoint calls B1's executor.
- **Stream D** (approvals + reads + dispatch) depends on A1 + A4; D1 is a
  prerequisite for B4 and U3.
- **Stream E**: E1 (jobdef block) is independent of the runtime (leaf); E2
  (AgentProfile CRUD) depends on A1.
- **Stream F** (MCP) depends on C4 (reuses the same handler layer).
- **Stream G**: G1 depends on D1 + D2; G2 depends on E2.
- **Stream U** (UI): U1 depends on D2; U2→U1; U3 depends on U2 + D1; U4→U2; U5→U1.
- **H-1** is largely independent (build image + justfile/CI + test harness) but
  the CI env wiring references A4's `CAESIUM_AGENT_REMEDIATION_ENABLED` and C2's
  `CAESIUM_AGENT_DEFAULT_PROFILE`; land the fake image early, finalize the env
  wiring once those envs exist.
- **N-1** runs last, after every runtime stream ships, so roadmap/schema/design
  docs reflect reality.

**Suggested waves:**
- **W1 = A (A1→A2→A3→A4, plus A5/A6) + E1 + H-1 (fake image).** A2, A3, A5, E1,
  H-1 are the leaf items; A4 chains after A1+A3; A6 after A1+A4.
- **W2 = B + C + D + E2.** All unblocked once A's store + models are in; they
  touch different core files (B → `internal/incident/executor|actions`; C →
  `internal/incident/session|bundle` + `api/rest/controller/agent`; D →
  `api/rest/controller/incident` + notification; E2 → `api/rest/controller/agentprofile`).
  Sequence B4 after D1 within the wave.
- **W3 = F + G + U + N-1.** MCP after C4's handlers; CLI after D/E; UI after D2/D1;
  N-1 last.

**Within-stream order:** A1 → A3 (via A2) → A4 → A6; A5 standalone. B1 → B2 → B3;
B4 after B1 + D1. C1 → (C2, C3) → C4. D1, D2, D3 independent of each other.
E1 standalone; E2 after A1. U1 → U2 → (U3, U4); U5 after U1.

**Cross-stream file conflicts:**

- `internal/models/models.go` — **A1 owns all model registrations** (single
  stream); no other stream appends here. Clean.
- `pkg/env/env.go` — A4 (`CAESIUM_AGENT_REMEDIATION_ENABLED`,
  `CAESIUM_AGENT_INCIDENT_COOLDOWN`), C2 (`CAESIUM_AGENT_MAX_CONCURRENT_SESSIONS`,
  `CAESIUM_AGENT_SESSION_TIMEOUT`, `CAESIUM_AGENT_DEFAULT_PROFILE`), and D1 (the
  auth-mode precondition in `validate()`) all touch it. A4 (W1) lands first; C2 +
  D1 (both W2) append on different lines — flag for a clean rebase, and note D1
  edits the shared `validate()` body (true-conflict risk with C2 only if both edit
  `validate()`; C2 adds fields only).
- `internal/metrics/metrics.go` — A4 (incidents_total, resolution_seconds, W1),
  then B1 (agent_actions_total, W2). Two edit sites each (`var(...)` +
  `Register()`); additive across waves.
- `cmd/start/start.go` — A4 (subscriber + gate), A6 (timer sweeper), C2 (session
  dispatcher), D3 (ai_agent sender) all add startup composition. A4 + A6 are the
  same stream/wave (W1). C2 + D3 (W2) each add a distinct wiring block — **sequence
  C2 → D3 within W2** rather than parallel, since both edit the startup composition
  function.
- `api/rest/bind/bind.go` — C3, C4 (agent routes), D1 (approvals), D2 (incident
  reads), E2 (agentprofile), F1 (mcp) all add routes. Additive line-appends, but
  the import block is the conflict-prone part — flag all W2/W3 route-adders.
- `api/middleware/auth_scope.go` — **C1** (new agent claim + switch arms) and
  **D1** (reject agent tokens on approval routes) both edit the scope switch.
  **Sequence C1 → D1** (both W2) or bundle the D1 edit behind C1's claim type.
- `pkg/jobdef/definition.go` — **E1 owns it** (single stream); no other stream
  edits the schema. No `internal/cache/hash.go` change from any stream (remediation
  is policy metadata, not step-execution input).
- `cmd/execute.go` — G1 + G2 append command groups (additive, both W3).
- `internal/event/bus.go` — A2 (`schema_violation_recorded`) and D2 (the four SSE
  incident event types) both add event types. A2 (W1) before D2 (W2); additive.
- `api/api.go` — only F1 (W3) touches it (write-timeout exemption); no conflict.
- `ui/src/router.tsx`, `ui/src/lib/api.ts`, `ui/src/components/layout/Sidebar.tsx`
  — U1–U5 append (same stream U); additive list edits.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (A4, B, C, D, E2, G):** an integration scenario
  in `test/` that drives the **real surface** against the live server (the
  deterministic fake agent image emitting a scripted action sequence through
  `/v1/agent/*`, or the CLI binary via `s.runCLI*`) and asserts observed output —
  dedupe/correlation, deterministic-rule short-circuit (no container launched),
  tier-1 autonomous retry that turns a red run green, tier-3 approval end-to-end
  (propose → card → approve → apply → verify), budget exhaustion → escalation,
  loop-guard (agent-initiated failure folds into the incident), and disabled-gate
  inertness. A unit test that hand-feeds the classifier proves the classifier, not
  the wiring — both are required.
- **Machine-readable CLI output (G, `--json`):** assert **clean, parseable stdout
  captured SEPARATELY from stderr** (`runCLIStdout`, not a stream-merging capture).
- **New metric (A4, B1):** assert via `internal/metrics/testutil` in a `*_test.go`;
  the collector must also appear in `Register()`.
- **Job-schema change (E1):** `caesium job lint --path docs/examples/` green on the
  new remediation-enabled example, with the offline scope-note for unverified
  `profile:` references emitted.
- **UI changes (Stream U):** `just ui-lint && just ui-test && just ui-e2e`
  (the incident feed, timeline, and approval-card flows as Playwright e2e against
  the live backend, including the auth-enabled lane `just ui-e2e-auth`, matching
  the data-plane-memory-ui precedent).
- **MCP surface (F1):** an MCP protocol-level test client exercises the JSON-RPC
  handshake + a tool call over the streamable-HTTP transport.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema/design) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the incident core** is a runtime feature: with
   `CAESIUM_AGENT_REMEDIATION_ENABLED=true` on a single-node server, a
   `task_failed` opens exactly one classified incident, an independent same-key
   failure appends an occurrence (no twin), the status machine advances only to
   `remediated` on a subsequent success, `TaskRun.ExitCode` is captured, and the
   log scrubber strips resolved `secret://` values before any bundle text. Closed
   by `test/` integration scenarios for dedupe/correlation and the disabled-gate
   inertness path, green in CI.
2. **Stream B — the action executor** enforces the tiered catalog server-side:
   deterministic rules record `actor=policy` rows with no container launch, a
   tier-1 autonomous `retry_from_failure` turns a red run green (refused while the
   job is paused, re-admitted under `queue` semantics), and `apply_jobdef_patch`
   is provenance-routed (git-synced → PR/escalate, otherwise direct). Closed by an
   autonomous-retry scenario + a provenance-routing scenario.
3. **Stream C — the agent runtime** launches a container through `atom.Engine` as
   an `AgentSession` (not a run), serves the scrubbed triage bundle at
   `GET /v1/agent/incidents/:id/bundle`, and enforces the incident-frozen scoped
   token across `/v1/agent/*` (a scoped token cannot widen its job allowlist or
   read live `/v1/lineage/impact`). Closed by a fake-agent scenario that reads the
   bundle and drives a scripted action sequence, plus a scope-enforcement negative
   test.
4. **Stream D — approval gates + reads + dispatch** are live: a tier-3 proposal
   creates an `ApprovalRequest`, the approve/reject endpoints reject agent tokens
   and require an active auth mode, `GET /v1/incidents` + `/:id` return the
   timeline, and an `ai_agent` notification channel routes matched events into the
   incident manager. Closed by an end-to-end approval scenario (propose → approve →
   apply → verify) hitting the live server.
5. **Stream E — declarative policy** works: a `metadata.remediation` block passes
   `caesium job lint` (with the offline scope-note for `profile:` refs), server-side
   lint verifies profile references inside the apply transaction, and `AgentProfile`
   CRUD + the shipped `triage-only` default profile exist. Closed by a lint
   scenario + an AgentProfile CRUD scenario.
6. **Stream F — the MCP surface** exposes the tool set at `/v1/agent/mcp` over
   JSON-RPC/streamable-HTTP with the write-timeout exemption. Closed by an MCP
   protocol-level integration test.
7. **Stream G — the CLI** ships `caesium incident …` and `caesium agent profile …`
   driving the real endpoints, asserted via `runCLIStdout` (clean, parseable stdout
   captured separately from stderr).
8. **Stream U — the Console incidents surface** ships the feed, the triage
   timeline (every claim one click from primary evidence), approval cards, run/task
   ribbons, the live agent-activity view with "Take over", and the fleet-analytics
   stats additions. Closed by Playwright e2e for the feed, timeline, and approval
   card against the live backend (including the auth-enabled lane).
9. **H-1 — the integration server** exercises the real event path: the fake agent
   image is built, the master gate + auth mode + default profile are configured on
   `just integration-up` and CI, so the Stream A–G scenarios drive the live binary
   rather than an internal call.
10. **N-1 — docs reflect reality:** `docs/roadmap.md` §3.5 updated to the shipped
    phase(s), the design-doc `> Status:` banner flipped, the `metadata.remediation`
    block documented across the schema references with a working `docs/examples/`
    manifest, and this plan indexed in `docs/README.md` (backtick form).
11. **Cross-cutting:** `docs/roadmap.md`, `docs/design-agent-in-the-loop.md`, and
    this plan's per-stream `## Progress` entries reflect every shipped stream and
    match the merged PRs.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (agent-in-the-loop-remediation <wave>-<stream>)` — e.g.
   `Add the leader-gated incident subscriber (agent-in-the-loop-remediation W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) — the
  design of record. Source of truth for intent and scope.
- [`docs/roadmap.md`](../../roadmap.md) §3.5 Agent-in-the-Loop ETL Remediation —
  the P3 strategic entry this plan closes; §3.2 Approval Gates & Human-in-the-Loop
  (the `ApprovalRequest` primitive this plan builds and §3.2 reuses); §2.3 SLA
  Management (the `extend_sla_once` / `sla_risk` interplay).
- [`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md) — the
  quarantine invariants that make `quarantine_replay` a safe experiment harness
  (`replaySafe` gate).
- [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md) —
  the OOM-detection substrate the full `oom` classification depends on.
- [`docs/design-contract-enforcement.md`](../../design-contract-enforcement.md) —
  the offline/server-side lint split this plan's E1/E2 adopt.
- [`docs/job-schema-reference.md`](../../job-schema-reference.md),
  `docs/job-definitions.md`, `docs/caesium-job-llm-reference.md` — the schema docs
  N-1 extends with the `metadata.remediation` block.
- [Data-Plane Memory UI](../completed/data-plane-memory-ui.md) — the precedent for
  the Console incidents surface and the auth-enabled Playwright lane.
- `internal/event/bus.go`, `internal/notification/subscriber.go`,
  `internal/replay/`, `internal/lineage/impact.go`, `internal/run/why.go`,
  `api/middleware/auth_scope.go` — the shipped substrates this plan builds on.
