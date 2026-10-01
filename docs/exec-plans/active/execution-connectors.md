# Execution Connectors — Temporal Monitoring and Operator Actions

Last updated: 2026-10-01

Caesium should let an operator follow a data process across Temporal workflows
and Caesium jobs, understand the data consequence, and perform a declared
business action from the Console. A generic **execution connector** provides
discovery, inspection, relationships, capability discovery and action invocation
for an external execution system. Temporal is the first implementation.
Temporal-only workflows are included; moving their ETL into Caesium is not a
prerequisite for monitoring them.

Today, [PR #560](https://github.com/caesium-cloud/caesium/pull/560) supplies
idempotent Caesium starts, explicit created/queued/skipped/dropped outcomes and
an operator recipe. There is no native Temporal client, external execution
model, monitoring connector or workflow action surface. The Console's existing
run, dataset, freshness, lineage, receipt and incident views are the data
evidence this integration should join to external process state.

The first release includes one optional Temporal connector, file-configured
connections and optional workflow bindings, authenticated monitoring, verified
links to Caesium admissions/runs, and one approval action implemented as a
Temporal Update. Generic contracts must accommodate another implementation
without embedding Temporal concepts in the core; a second production provider
is outside this release.

Non-goals: replacing Temporal's engine/debugger; translating arbitrary code into
a static DAG; a Temporal container engine; a marketplace/dynamic plugin loader;
database/SaaS extract connectors; inferred dataset lineage for opaque workflows;
external agent actions; schedule administration; arbitrary Signals/Updates;
workflow start/reset/terminate controls; propagated cancellation; or changing
the existing job-definition YAML contract.

## Source-Of-Truth Note

This plan records the proposed contract from the connector discussion of
2026-10-01. Its review decides this initiative's scope and contract. Applicable
`AGENTS.md` and shipped APIs/schemas govern existing behavior; changes are
additive and carry real-surface tests. Unmerged sibling work is not a prerequisite.

This is a narrow proposed amendment to
[differentiation-strategy.md](../../differentiation-strategy.md): join external
process state to Caesium data evidence without reopening the parked provider
catalog. The optional compiled adapter adds a Go dependency; with the feature
off, Caesium requires no Temporal server, worker or credentials. The current
no-Temporal-dependency statement in [temporal.md](../../temporal.md) describes
shipped behavior and changes only when the adapter ships.

The [identity-and-access plan](identity-and-access.md) owns namespace grants,
policy reload and tenancy. This plan uses shipped role/scope machinery. The
[closed-loop arc](closed-loop-arc.md) retains dataset hold/incident/remediation
ownership; observations do not manufacture watermarks, release holds or add
remediation policy. [Distributed testing](distributed-testing.md) owns common
evidence/gate infrastructure; this plan adds connector-specific scenarios.
The completed [operator loop](../completed/console-operator-loop-ux.md) and
[data-plane UI](../completed/data-plane-memory-ui.md) are reused, not duplicated.

External protocol references checked while drafting:
[Visibility](https://docs.temporal.io/visibility),
[Workflow/Run identity](https://docs.temporal.io/workflow-execution/workflowid-runid),
[Queries and Updates](https://docs.temporal.io/develop/go/workflows/message-passing),
and the [Go client](https://pkg.go.dev/go.temporal.io/sdk/client).
Visibility is eventually consistent and serves discovery. Direct execution
reads/history supply detail; bindings supply explicit business semantics.

### Proposed v1 contract

1. **Three objects.** Connector implementations register behind a neutral
   interface. Connections have stable IDs, provider type, endpoint, credential
   references and external scope. Optional versioned workflow bindings declare
   display names, one status Query, named actions with JSON input/result schemas
   and a Caesium activity correlation envelope. Unbound workflows remain
   inspectable with no business action buttons. Configured workflow-type scope
   applies to direct reads/action targets as well as lists.
2. **Gate/configuration.** `CAESIUM_CONNECTORS_ENABLED=false` by default;
   `CAESIUM_CONNECTORS_CONFIG_FILE` selects separate versioned YAML, not a job
   manifest. Read at startup; hot reload and UI configuration writes are
   deferred. Reuse `secret://env/...` resolution and mounted certificate paths;
   reject inline credentials, unknown fields/providers and invalid schemas.
   Enabled connections require Caesium API-key or SSO authentication. Invalid
   enabled config fails startup with redacted errors; remote outage leaves
   local readiness healthy. Disabled mode ignores the optional file, dials
   nothing, mounts no connector routes and hides navigation.
3. **Identity/authority.** External executions have opaque Caesium projection
   IDs and immutable provider references. Temporal references include connection,
   external namespace, Workflow ID and Run ID. Preserve native status/metadata
   alongside a small display status. Chains, runs, activities and reported
   attempts are distinct; parent/child, continuation and delegation are distinct
   relationship types. Never create synthetic `JobRun`/`TaskRun` rows. Refuse
   repointing a connection ID with persisted references; use a new ID instead.
4. **Bounded observation.** v1 uses request-driven provider reads and Console
   refresh, plus catalog snapshots of inspected executions. No namespace-wide
   background importer or second scheduler. Cursors bind to connection/filter/
   configuration; no invented global ordering across providers. Direct reads
   retain source event IDs, observation time, availability and completeness.
   Discovery never overwrites newer direct state. Conditional snapshot writes
   reject stale request generations and terminal regression for the same run.
   Outage, missing workers, unavailable history and incomplete pages are explicit
   availability states, not workflow success/failure. Cached details are visibly
   stale and cannot enable mutation.
5. **Capabilities/payloads.** Discovery, inspection, history, relationships,
   status Query, action submission and receipt lookup are optional capabilities.
   Advertise only capabilities wired into the running server, not merely methods
   the provider adapter implements.
   Action descriptors include meaning, schemas, minimum role and availability;
   there is no universal `retry()`/`cancel()`. Only binding-declared Query/Update
   handlers are callable. v1 supports default JSON payloads; encrypted/custom
   payloads remain metadata-only. Exposed fields are explicitly schema-declared,
   bounded and redacted. Do not copy arbitrary workflow inputs/results or full
   history payloads into the catalog.
6. **One business action.** The pilot exposes `approve_publication`, a Temporal
   Update with validation and a result. Pin observed Run ID and binding version,
   validate input, require `Idempotency-Key`, and persist actor, target, request
   fingerprint and stable external Update ID before sending. Identical retries
   attach to one receipt; changed request/target under the same key is refused.
   Distinguish submitted, accepted, completed, rejected and unknown. Timeout is
   not rejection/completion. Resolve through the same Update handle/ID; any
   resubmission addresses that same Update. Restart never retargets a newer run.
   The remote validator remains authoritative for approval eligibility.
7. **Authorization.** Connector reads and declared status Query require an
   unscoped viewer or higher. Actions require unscoped operator/admin. Job-scoped
   and agent-session credentials cannot use connector surfaces. Reverse links
   on an existing Caesium run retain job scope and omit external details the
   principal cannot read. Audit actor, target, operation ID and redacted outcome.
   Server checks back UI gating. If identity-and-access merges first, integrate
   into its route classes; do not introduce parallel grants/tenancy.
8. **Verified correlation.** Recognized Caesium activities report a versioned
   JSON heartbeat/result containing job ID, admission key and returned queue/run
   ID. Obtain enclosing workflow/run/activity identity from Temporal. Verify
   the local admission/job/queue/run record before linking; incomplete/mismatched
   reports stay unverified. Queue promotion resolves to the admitted run;
   activity retries reuse that admission. Persist source evidence and verification
   time. Follow verified local runs to existing data APIs; binding-declared
   dataset associations are declarations, never proof of freshness,
   materialization, cache coverage or reproducibility.
9. **Storage/lifecycle.** Snapshots, relationship evidence and receipts are
   catalog metadata registered in `models.All`, not hot execution tables.
   Configure finite snapshot age, page/metadata limits, concurrency and RPC
   deadlines. Retain referenced identity/evidence and action dedupe records in
   v1; key pruning awaits an explicit retry-horizon/tombstone contract. No full
   Temporal history mirror. Cancellation/shutdown release clients and bounded
   in-flight requests.

Proposed REST grouping, finalized with A1's shared types and implemented by C:
`GET /v1/connectors` lists configured connections; execution list/detail/history/
relationships live under `/v1/connectors/:connection_id/executions` using opaque
projection IDs. Declared Query/Update POSTs live below the execution's
`queries/:query_id` and `actions/:action_id`; receipts use
`GET /v1/connectors/:connection_id/operations/:operation_id`. Reverse links use
`GET /v1/jobs/:id/runs/:run_id/external-executions`. No endpoint accepts an
arbitrary remote endpoint, namespace, handler or unpinned action target.

## Progress (as of 2026-10-01)

No implementation waves have shipped. This is planning only; none of the
proposed behavior has runtime acceptance evidence. Drafting base:
`a0e862171b898a64b64828b1a5376226e85b106a`.

The orchestrator owns this dashboard: PR URLs, exact tested heads/merge SHAs,
executed scenarios, evidence and blockers. Workers update assigned items/notes.
An open PR or fixture-only pass is not completion. Resume unfinished waves.

### Stream Status

| Stream | Scope | Priority | Status |
| --- | --- | --- | --- |
| A | Generic contract, configuration and catalog (2 items) | P0 | Not started |
| B | Temporal inspection and declared messages (2 items) | P0 | Not started |
| C | Authenticated REST and composition (2 items) | P0 | Not started |
| D | Verified correlation and action durability (2 items) | P0 | Not started |
| E | Console monitoring, data links and approval (3 items) | P0 | Not started |
| F | Real fixture, CI and qualification (3 items) | P0 | Not started |
| N | Operator guidance and reconciliation (1 item) | P0 | Not started |

## Streams

### Stream A — Generic contract, configuration and catalog

- [ ] A1. Implement provider-neutral contracts/registry and strict versioned
  connection/binding configuration: schemas, secret references, gate validation,
  resource limits and immutable connection identity. A test-only adapter with
  different opaque identity/capabilities proves the core needs no Temporal fields.
  Files: new `internal/connector/types.go`, new `internal/connector/registry.go`,
  new `internal/connector/config.go`, new `internal/connector/config_test.go`,
  `pkg/env/env.go`, `pkg/env/env_test.go`, `internal/jobdef/secret/env.go` (reuse),
  new `docs/connectors.md` (configuration/contract sections).
  Depends on: none.
  Verify: parser/registry tests refuse unsupported providers, duplicate IDs,
  invalid schemas, inline secrets and invalid enabled/auth combinations. Disabled
  configuration preserves defaults. C1 supplies actual startup/HTTP gate proof.

- [ ] A2. Persist external identities, bounded direct snapshots, typed relation
  evidence and action receipt/dedupe records in the catalog. Unique indexes and
  conditional writes enforce observation-generation and receipt invariants.
  Files: new `internal/models/external_execution.go`,
  new `internal/models/connector_operation.go`, `internal/models/models.go`,
  new `internal/connector/store.go`, new `internal/connector/store_test.go`,
  `pkg/db/migrations_test.go`, `docs/connectors.md` (storage/retention sections).
  Depends on: A1.
  Verify: concurrent callers cannot regress terminal observations or admit two
  operations for one key. Database reopen preserves references/receipts; fresh
  and upgraded catalogs migrate, including sharded routing. New tables stay
  absent from `hotTables`/`hotPathModels`; C1/C2 prove public persistence reads.

### Stream B — Temporal adapter

- [ ] B1. Implement health, filtered paginated discovery, direct descriptions
  and bounded history. Preserve native identity/status, reported activity attempts,
  parent/child/continuation evidence and workflow-task problems separately from
  workflow failure. Configure TLS/API-key/mTLS; plaintext is explicitly local.
  Files: new `internal/connector/temporal/client.go`,
  new `internal/connector/temporal/observe.go`,
  new `internal/connector/temporal/observe_test.go`, `go.mod`, `go.sum`,
  `docs/connectors.md` (Temporal/authentication sections).
  Depends on: A1.
  Verify: adapter tests cover pagination, deadlines, redaction and status mapping;
  F1 runs this adapter against real Temporal. Start from SDK v1.49.0 (#560's
  example version), record justified changes, and regenerate sums with the
  containerized toolchain. Local tests do not certify a Temporal Cloud account.

- [ ] B2. Implement only binding-declared status Queries/Update actions, JSON
  input/result validation, pinned targets, stable Update IDs and existing handle
  lookup. Separate missing workers, validator rejection and ambiguous RPC outcomes.
  Files: new `internal/connector/temporal/messages.go`,
  new `internal/connector/temporal/messages_test.go`,
  `docs/connectors.md` (message semantics sections).
  Depends on: B1.
  Verify: refuse undeclared handlers/custom conversion. F1 proves real Query,
  accepted/completed Update, validator rejection and same-ID reconnection. No
  action implicitly targets the latest run. C2 supplies the HTTP proof.

### Stream C — Authenticated API and composition

This stream owns startup, API dependency plumbing, route binding and shared
auth chokepoints. Every endpoint includes a live HTTP integration scenario.

- [ ] C1. Wire clients/observation into startup and feature discovery. Add
  connection list, per-connection execution list/detail/history/relationships
  and reverse links from a Caesium run. Persist bounded direct observations;
  return dated stale detail explicitly on failed refresh. Enforce read/scope policy.
  Files: new `internal/connector/observe.go`, new `internal/connector/observe_test.go`,
  new `api/rest/service/connector/reads.go`,
  new `api/rest/controller/connector/reads.go`, `api/rest/bind/bind.go`,
  `api/rest/bind/bind_test.go`, `api/api.go`, `cmd/start/start.go`,
  `api/rest/service/system/system.go`, `api/rest/service/system/system_test.go`,
  `internal/auth/rbac.go`, `api/middleware/auth_scope.go`,
  `api/auth_rbac_policy_completeness_test.go`,
  new `test/connectors_read_test.go`, `docs/connectors.md` (REST/read policy).
  Depends on: A2, B1, D1, F1.
  Verify: live HTTP finds a Temporal-only workflow, pages the unchanged fixture
  without duplicates, exposes genuine IDs/relations, preserves stale detail on
  outage and recovers after Caesium restart. Auth, scoped/agent denial, gate-off
  startup, scope-safe reverse links and refused connection repointing are observed.

- [ ] C2. Expose declared status Queries, action submissions and operation
  receipts. Enforce operator policy, schema/conflict errors and audited outcomes;
  receipt lookup resolves the external operation without inventing completion.
  Files: new `api/rest/service/connector/actions.go`,
  new `api/rest/controller/connector/actions.go`, `api/rest/bind/bind.go`,
  `api/rest/bind/bind_test.go`, `internal/auth/rbac.go`,
  `api/middleware/auth_scope.go`, `api/middleware/auth.go`,
  `internal/auth/audit.go`, `api/auth_rbac_policy_completeness_test.go`,
  new `test/connectors_actions_test.go`, `docs/connectors.md` (action REST/policy).
  Depends on: C1, B2, D2.
  Verify: live HTTP covers Query/approval, viewer/runner/scoped/agent denial,
  undeclared handlers, stale binding/run targets, bad schema, concurrent duplicate
  keys, conflicting reuse, rejection and response loss. Assert actual Temporal
  effect count plus durable receipt/audit, not merely status codes.

### Stream D — Correlation and action durability

- [ ] D1. Validate declared Caesium activity heartbeat/result envelopes against
  #560's job/admission/queue/run records and persist typed links with source
  evidence. Follow queued promotion without starting work. Extend the maintained
  Temporal recipe with the envelope; activity retries retain admission identity.
  Files: new `internal/connector/correlation.go`,
  new `internal/connector/correlation_test.go`, `internal/run/start_idempotency.go`,
  `internal/run/start_idempotency_test.go`, `docs/temporal.md` (activity envelope).
  Depends on: A2, B1, F1.
  Verify: the real fixture reports queued/running/finished admission. C1 proves
  bidirectional public links. Wrong/missing key, another job's run or an unrelated
  activity cannot become verified. Repeat reads/retries create one relation;
  promotion/restart preserve identity. Existing #560 regressions pass.

- [ ] D2. Implement transactional receipt admission/dedupe and provider-backed
  resolution. Snapshot the binding contract and stable Update ID before dispatch;
  concurrent callers/restart attach to the original pinned operation. Bound and
  redact persisted content; transport uncertainty is not rejection.
  Files: new `internal/connector/operations.go`,
  new `internal/connector/operation_store.go`,
  new `internal/connector/operations_test.go`,
  `docs/connectors.md` (receipt/recovery sections).
  Depends on: A2, B2, F1.
  Verify: real fixture plus store concurrency tests cover lost accepted reply,
  crash after local admission, restarted lookup and changed-input refusal.
  One approval effect occurs; unknown stays unknown absent evidence. C2 provides
  the public-surface proof.

### Stream E — Console operator experience

Use one external-execution feature area with provider/connection selection,
joined to existing Caesium pages. No cross-provider global pagination in v1.

- [ ] E1. Add connection/execution navigation, typed API calls, filters,
  paginated detail/history, native status, observation age and connection health.
  Show unbound workflows, unavailable payloads/partial history, error/recovery
  states and configured Temporal UI links. Gate navigation and direct routes.
  Files: new `ui/src/features/connectors/ConnectionsPage.tsx`,
  new `ui/src/features/connectors/ExecutionDetailPage.tsx`,
  new `ui/src/features/connectors/types.ts`,
  new `ui/src/features/connectors/__tests__/monitoring.test.tsx`,
  `ui/src/lib/api.ts`, `ui/src/router.tsx`,
  `ui/src/components/layout/Sidebar.tsx`, `ui/src/features/auth/access.ts`,
  new `ui/e2e/auth/connectors-monitoring.spec.ts`, `docs/connectors.md` (Console tour).
  Depends on: C1.
  Verify: browser filters/inspects the actual external-only workflow, sees dated
  stale details on outage, recovers and renders unsupported capabilities honestly.
  Gate-off/unauthorized routes disclose no workflow details.

- [ ] E2. Render typed parent/child/delegated-run relations and reverse links
  on existing run detail. Follow verified runs to logs, receipts and data evidence
  with original permissions. Label dataset declarations separately from evidence.
  Files: new `ui/src/features/connectors/RelationshipsPanel.tsx`,
  new `ui/src/features/connectors/__tests__/relationships.test.tsx`,
  `ui/src/features/connectors/ExecutionDetailPage.tsx`,
  `ui/src/features/jobs/RunDetailPage.tsx`,
  new `ui/e2e/auth/connectors-correlation.spec.ts`, `docs/connectors.md` (data tour).
  Depends on: E1, D1.
  Verify: browser follows workflow -> admitted run -> existing receipt/data
  evidence -> workflow; queue promotion becomes the same run. Opaque external
  work has no invented lineage/freshness; scoped users see permitted local data.

- [ ] E3. Render the declared approval form and receipt, exact target,
  acceptance/completion distinction and validator result. Retain operation key
  across retries and reconcile after browser refresh. Gate on live observation,
  binding compatibility, authorization and capability; no timeout success toast.
  Files: new `ui/src/features/connectors/ActionPanel.tsx`,
  new `ui/src/features/connectors/OperationReceipt.tsx`,
  new `ui/src/features/connectors/__tests__/actions.test.tsx`,
  `ui/src/features/connectors/ExecutionDetailPage.tsx`, `ui/src/lib/api.ts`,
  new `ui/e2e/auth/connectors-actions.spec.ts`, `docs/connectors.md` (approval tour).
  Depends on: E2, C2.
  Verify: real approval advances the workflow; rejection is explicit. Lost reply
  stays pending/unknown until resolved; refresh/duplicate click attaches to one
  receipt. Denied users/unbound/stale workflows cannot invoke an action.

### Stream F — Real Temporal fixture, CI and qualification

One owner serializes the local Docker/browser lane. Reuse the builder and
compiled integration-runner approach; fixture code stays in the root module.

- [ ] F1. Build a pinned isolated Temporal development fixture with real SDK
  worker: external-only workflow, and publication workflow that invokes a Caesium
  job through #560, reports correlation, awaits approval and invokes a second
  job. Include Query, validator rejection, worker restart, child/continuation
  identity and connector-path interruption. Independently record effects/starts.
  Files: new `test/connectors/fixture/main.go`,
  new `test/connectors/fixture/workflows.go`,
  new `test/connectors/fixture/activity.go`, new `test/connectors_fixture_test.go`,
  new `test/connectors/testdata/connections.yaml`,
  new `build/Dockerfile.connector-fixture`, new `scripts/connectors-fixture.sh`,
  `justfile` (fixture-check recipe), `docs/connectors.md` (fixture instructions).
  Depends on: B1.
  Verify: container-built fixture drives actual Temporal/Caesium APIs without
  seeded application rows. Pin server/CLI artifact/image digests and record the
  SDK/server matrix; local certificates verify TLS option handling. Prove fault
  activation. Missing worker/fixture is failure. C1/C2 complete adapter qualification.

- [ ] F2. Add authenticated connector HTTP/browser recipe and CI job with the
  gate enabled on server and runner. Select every expected scenario, feed `ci-ok`
  on connector-affecting changes and verify candidate/scenario/evidence identity.
  Files: `justfile`, `.github/workflows/ci.yml`, `scripts/test_ci.py`,
  new `scripts/connectors-test.sh`, new `scripts/test_connectors_evidence.py`,
  `ui/playwright.config.ts`, new `ui/e2e/helpers/connectors.ts`,
  `docs/ci.md` (connector lane), `docs/connectors.md` (verification commands).
  Depends on: C2, E3, F1.
  Verify: proposed `just integration-test-connectors` executes all named live
  scenarios. Gate-off misconfiguration, zero count, fixture failure, failed
  first browser attempt, missing artifacts or wrong candidate SHA fail the
  lane/gate. Run actionlint and the workflow's existing CI regression command.

- [ ] F3. Record first-release acceptance on the merged candidate: external-only
  monitoring, correlated publication, approval/rejection, activity retry, reply
  loss, server/worker restart, outage/recovery, duplicate action and continuation
  without retargeting old actions. Also run default-disabled regression gates.
  Files: `docs/exec-plans/active/execution-connectors.md` (acceptance record only).
  Depends on: F2.
  Verify: record source/image identity, fixture pins, HTTP/browser evidence,
  independent effect counts and receipts. Missing fault proof or unresolved
  identity is inconclusive. Local evidence does not certify Cloud/all releases.

### Stream N — Operator guidance and reconciliation

- [ ] N1. Consolidate accepted behavior/tour in the connector guide; reconcile
  the Temporal recipe and narrow strategy amendment. Update roadmap/navigation
  only from merged evidence. Preserve at-least-once and cancellation limitations,
  deferred scope and the later second-provider abstraction check.
  Files: `docs/connectors.md`, `docs/temporal.md`, `docs/README.md`,
  `docs/roadmap.md`, `docs/differentiation-strategy.md`,
  `docs/exec-plans/active/execution-connectors.md`.
  Depends on: F3.
  Verify: documented local tour reproduces actual API/Console inspection and
  approval. Schema/commands match merged code, links resolve and shipped claims
  cite accepted compatibility/evidence. Deferred capabilities stay deferred.

## Sequencing & Dependencies

**15 included items in seven streams.** Dependencies are explicit above;
unchecked sibling work is not a hidden prerequisite.

- First ready item: **A1**; no Temporal account/server required.
- After A1: **A2 and B1** can proceed independently.
- After B1: **B2 and F1**; F1 supplies the pinned external fixture.
- **D1** needs A2/B1/F1; **D2** needs A2/B2/F1. Core files are disjoint.
- **C1** joins A2/B1/D1/F1; **C2** follows C1/B2/D2.
- **E1** can run on merged C1 while C2 develops; **E2 -> E3** serializes
  shared detail UI. E3 also needs merged C2.
- **F2 -> F3 -> N1** wires, qualifies and documents the release.

### Shared file ownership and merge order

| Surface | One writer / explicit order |
| --- | --- |
| Generic contracts/env | A1 owns types/registry/config and `pkg/env`; later changes return to A's integration owner |
| Models/base store | A2 owns model registration/base store; D1/D2 own separate correlation/operation files |
| Module files | B1 alone owns `go.mod`/`go.sum`; F1 uses that SDK, no nested module |
| Startup/API composition/features | C1 alone owns startup, `api/api.go` and feature composition |
| Routes/auth/audit | C1 then C2, both Stream C; no parallel writer |
| #560 lookup seam | D1 only; preserve admission behavior/regressions |
| UI client/navigation | E1 then E3; detail page E1 -> E2 -> E3 |
| Fixture/CI recipes | F1 then F2 for `justfile`; F2 alone owns CI/playwright configuration |
| Connector guide | One integration writer; section integration order A1 -> A2 -> B1 -> B2 -> F1 -> D2 -> C1 -> C2 -> E1 -> E2 -> E3 -> F2 -> N1 |
| Other docs/Progress | D1 owns Temporal envelope, N1 reconciles later; F2 owns CI lane text. Orchestrator owns Progress; F3 contributes acceptance evidence |

Guide sections ship with implementation. Independent streams may overlap, but
only the assigned integration writer edits the guide; serialize its edits in
the listed order before publication/rebase. Do not defer required tests/docs to
unowned PRs. Refresh shared-file ownership against live plans/branches at dispatch.

### External prerequisites and visible decisions

F1 supplies the pinned reachable Temporal fixture/worker; successful workflow
start/Query/Update and independent effect records prove readiness. A server
without a worker is insufficient. F2 requires Docker, existing builder/runner
images and Chromium. Own task-created containers, networks, volumes, ports and
fault controls; never reuse/remove foreign resources. Serialize shared tags/ports.

If identity-and-access merges before C1/C2, reconcile route/principal integration
in C and record its merged identity. A/B/D are independent of that unshipped
plan. No unanswered product decision blocks A1. Concrete configuration/resource
budget values and the SDK/server matrix are A1/F1 implementation decisions
recorded before dependent work. Identity/authority/replay/auth/scope changes
require revising this contract before dependent implementation.

Deferred: second provider; dynamic loading; config hot reload/admin UI; custom
payload codecs; background fleet import/global pagination; live Temporal Cloud
qualification; workflow start/Signals/reset/terminate/schedule administration;
and propagated Caesium cancellation. Nexus may be evaluated later for reusable
execution handoffs; it is not a monitoring prerequisite.

## Verification (Run For Every PR)

For this planning-only PR: validate unique IDs, required fields, existing versus
explicitly new paths, dependencies/acyclicity, ownership, template-token absence,
relative links and `git diff --check`. These are not runtime qualification.

Runtime PR baseline, using the existing containerized recipes:

```sh
just lint
just unit-test
just integration-test
```

UI changes add `just ui-ci` and existing authenticated regression coverage via
`just ui-e2e-auth`. Connector changes run proposed `just integration-test-connectors`
once F2 lands. Before F2, use the F1 fixture/candidate server with each item's
live HTTP/browser scenarios and record the exact containerized command. The new
recipe is not shipped. Pure adapter/store tests supplement real-surface proof.

Each new REST query/action needs a named live-server scenario under `test/`.
Infrastructure-dependent tests/packages carry the integration tag; fixture
executables use explicit container builds in the root module. No new CLI verbs
are included; any later addition requires binary tests with separate parseable
stdout/stderr. Feature/auth flags go on actual server and runner. An off-lane
skip is not acceptance; the selected lane proves all expected scenarios ran.
Reuse existing auth helpers where appropriate without labeling this a
remediation lane.

CI/recipe changes run actionlint and the existing CI regression suite. Connector
source/module/fixture/UI/recipe/CI paths select the new job in both PR and merge
group events; its result feeds `ci-ok`. Capture Caesium/Temporal/worker logs,
fault activation, history/effects, receipts, browser reports and candidate/image
IDs. One owner holds the local Docker/browser lane through cleanup. No production
account, external message delivery or repository-settings change is required.
Default-disabled tests continue to run without any Temporal process.

## Acceptance Criteria

1. Default Caesium starts/runs jobs without Temporal configuration/processes.
   Connector routes/navigation are absent; invalid enabled config fails redacted;
   remote outage preserves local readiness. Proof: startup/HTTP/browser scenarios.
2. Real configured Temporal workflows, including external-only/unbound work,
   are discoverable/inspectable with native identity/status, reported activity
   metadata, typed relationships, pagination and completeness. Proof: API/browser
   against the pinned real fixture.
3. Outage, missing workers/payloads, unavailable history and list lag appear as
   explicit stale/unavailable/partial information. Direct state cannot regress;
   restart preserves identity and recovery restores observation. Proof: activated
   faults, out-of-order writes and live restart/recovery scenarios.
4. Verified publication activity links resolve queued promotion/retry to one
   admitted Caesium run. Both directions reach existing run/data evidence;
   opaque external work gains no inferred lineage/freshness/receipt. Proof:
   public reads/browser journey, admission counts and mismatch refusals.
5. Declared authorized approval returns validated receipt/result and advances
   the actual workflow to its second Caesium job. Rejection is explicit;
   duplicate/reply-loss/restart produces one pinned operation/effect. Continuation
   never retargets it. Proof: HTTP/browser, Update history and independent effects.
6. Server-side auth/scope checks permit intended reads and refuse unauthorized,
   scoped/agent access and runner mutations. Reverse links retain job scope.
   Secrets/undeclared payloads do not leak; audit attributes the operator/outcome.
   Proof: authenticated HTTP/browser and secret sentinel/audit assertions.
7. All selected HTTP/browser scenarios actually execute with real servers.
   Failure/absence/flake/wrong-SHA evidence fails `ci-ok`; default-disabled gates
   pass. Proof identifies the candidate and fixture pins, with Cloud/all-version
   qualification explicitly outside the claim.
8. Completion requires verified merged evidence. Progress/docs/status links
   match accepted behavior and retain deferred work. Proof: F3 record and N1's
   reproduced tour. Planning or an open PR alone satisfies none of these gates.

## How To Pick Up Work

1. Read this plan, its contracts, and the applicable `AGENTS.md`.
2. Select unchecked, included items whose dependencies are verified ready.
3. Use an assigned worktree and branch from the verified base. Keep changes
   within stream ownership and include tests and behavior docs.
4. Run required verification on the actual candidate. Record passed, failed or
   unavailable checks.
5. Update assigned items/notes only; the orchestrator records verified merged
   completion in Progress.
6. Follow the requested publication endpoint. Titles use
   `<Imperative subject> (execution-connectors W<n>-<Greek stream suffix>)`.

Use `$exec-plan-wave` with this plan path to orchestrate a wave in Codex.
Use `$draft-exec-plan` to revise the plan without starting implementation.

## Cross-References

- [Current Temporal recipe](../../temporal.md) and
  [run-start contract](../../job-definitions.md).
- [Strategy](../../differentiation-strategy.md) and [roadmap](../../roadmap.md).
- [Identity/access](identity-and-access.md), [arc](closed-loop-arc.md) and
  [distributed testing](distributed-testing.md).
- [Console operator loop](../completed/console-operator-loop-ux.md),
  [data-plane UI](../completed/data-plane-memory-ui.md) and
  [freshness](../completed/freshness-scheduling.md).
- [CI runbook](../../ci.md) and [docs index](../../README.md).
- [Draft skill](../../../.codex/skills/draft-exec-plan/SKILL.md) and
  [wave skill](../../../.codex/skills/exec-plan-wave/SKILL.md).
