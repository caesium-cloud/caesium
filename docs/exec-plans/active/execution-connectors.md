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
Temporal Update. Shared contracts cover the execution identity, observations,
capabilities, relationships and operations required by Temporal now, without
embedding Temporal fields in the core. A test-only adapter checks that boundary;
further generalization requires a second real provider, outside this release.

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

This PR includes the narrow proposed amendment in
[differentiation-strategy.md](../../differentiation-strategy.md): join external
process state to Caesium data evidence without reopening the parked provider
catalog. The optional compiled adapter adds a Go dependency; with the feature
off, Caesium requires no Temporal server, worker or credentials. The current
no-Temporal-dependency statement in [temporal.md](../../temporal.md) describes
shipped behavior. B1 updates both guides in the same PR that introduces the SDK;
N1 reconciles final acceptance rather than deferring that dependency correction.

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
   and a Caesium activity correlation contract. A1 freezes the activity-type/job
   allowlist and envelope schema before F1 consumes them. Unbound workflows remain
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
   nothing, mounts no connector routes and hides navigation. A2 persists a
   canonical non-secret configuration fingerprint/epoch; C1 enforces it across
   nodes before any connector RPC or receipt admission. See configuration
   equality below for coordinated changes and Helm mounting.
3. **Identity/authority.** External executions have opaque Caesium projection
   IDs and immutable provider references. Temporal references include connection,
   external namespace, Workflow ID and Run ID. Preserve native status/metadata
   alongside a small display status. Chains, runs, activities and reported
   attempts are distinct; parent/child, continuation and delegation are distinct
   relationship types. Never create synthetic `JobRun`/`TaskRun` rows. Refuse
   repointing a connection ID with persisted references; use a new ID instead.
4. **Bounded observation.** v1 uses request-driven provider reads and Console
   refresh, plus catalog snapshots of inspected executions. No namespace-wide
   background importer or second scheduler. List/history pages create no catalog
   rows; A1 defines validated opaque references that resolve without inserting
   each discovered execution. Cursors bind to connection/filter/configuration;
   no invented global ordering across providers. Direct reads retain source event
   IDs, observation time, availability and completeness. Persist a snapshot only
   when source event/state evidence advances; unchanged polling returns its live
   observation time without a Raft write. Discovery never overwrites newer direct
   state. Conditional writes reject stale request generations and terminal
   regression for the same run. Coalesce concurrent identical reads; enforce the
   finite budgets below. Outage, missing workers, unavailable history and
   incomplete pages are explicit availability states, not workflow success/failure.
   Cached details are visibly stale and cannot enable mutation.
5. **Capabilities/payloads.** Discovery, inspection, history, relationships,
   status Query, action submission and receipt lookup are optional capabilities.
   Advertise only capabilities wired into the running server, not merely methods
   the provider adapter implements.
   Action descriptors include meaning, schemas, minimum role and availability;
   there is no universal `retry()`/`cancel()`. Only binding-declared Query/Update
   handlers are callable. v1 supports default JSON payloads; encrypted/custom
   payloads remain metadata-only. Exposed fields are explicitly schema-declared,
   bounded and redacted. Do not copy arbitrary workflow inputs/results or full
   history payloads into the catalog. Query is explicit and rate-limited,
   never an automatic polling/replay loop on closed workflows.
6. **One business action.** The pilot exposes `approve_publication`, a Temporal
   Update with validation and a result. Pin observed Run ID and binding version,
   validate input, require `Idempotency-Key`, and persist actor, target, request
   fingerprint and stable external Update ID before sending. Identical retries
   attach to one receipt; changed request/target under the same key is refused.
   A transactional guard permits only one open operation per connection/workflow/
   run/action across different keys and operators; conflicts return the existing
   receipt ID. List execution operations so refresh/new tabs recover server state.
   Distinguish submitted, accepted, completed, rejected and unknown. Timeout is
   not rejection/completion. Background/restart reconciliation only looks up the
   same Update handle/ID; it never resubmits. A rejected-before-acceptance Update
   is absent from history, so lookup `NotFound` leaves an uncertain operation
   unknown, not rejected or safe to retry. Only a fresh explicit confirmation by
   the original principal may resubmit the immutable receipt/Update ID to its
   pinned run; audit that decision and recheck authorization/configuration.
   Known rejection is terminal and duplicate requests never resend it. Restart
   never retargets a newer run. The remote validator remains authoritative for
   eligibility, and the workflow handler enforces one logical approval effect
   even after receipt completion or with a new Update ID.
7. **Authorization.** Connector reads and declared status Query require an
   unscoped viewer or higher. Actions require unscoped operator/admin. Job-scoped
   and agent-session credentials cannot use connector surfaces. Reverse links
   on an existing Caesium run retain job scope and omit external details the
   principal cannot read. Audit actor, target, operation ID and redacted outcome.
   Updates carry an immutable server-derived `_caesium_actor` context containing
   principal kind/stable ID/subject, role, operation ID and binding version.
   The remote envelope separates `input` from this reserved context; public
   input/binding schemas cannot define or override it. Refuse attempted spoofing
   before RPC. The workflow validator/history sees this authenticated principal,
   rather than inferring a human from the connector's service identity. API keys
   remain identified as keys. The workflow trusts the authenticated connector
   service as producer of this context; other authorized Temporal writers are
   outside that assertion. Server checks back UI gating. If identity-and-access
   merges first, integrate into its route classes; do not add parallel grants.
8. **Verified correlation.** Obtain enclosing Workflow ID, Run ID, Activity ID
   and activity type from Temporal evidence. For a binding-recognized Caesium
   activity, derive the admission key exactly as #560's recipe does:
   `<RunID>/<ActivityID>`. Search only its bounded declared local-job allowlist
   using a read-only job/key admission lookup; exactly one consistent match can
   become verified. No payload decoding or HTTP request-body/hash reconstruction
   is required. A1's optional v1 JSON heartbeat/result envelope declares
   `version`, `job_id`, `idempotency_key`, `outcome`, optional `queue_id`/`run_id`;
   every reported value must match the trusted derived identity and local record.
   Missing/ambiguous records and mismatched hints stay unverified. Another
   workflow's otherwise valid admission cannot be borrowed. Queue promotion
   resolves to the admitted run; activity retries reuse that admission. Persist
   source evidence and verification time only when evidence changes. This verifies
   the declared protocol identity, not the identity of #560's HTTP caller or
   arbitrary data provenance. Follow verified local runs to existing data APIs;
   dataset associations are declarations, never proof of freshness,
   materialization, cache coverage or reproducibility.
9. **Storage/lifecycle.** Snapshots, relationship evidence and receipts are
   catalog metadata registered in `models.All`, not hot execution tables.
   Enforce the finite snapshot/page/concurrency/RPC budgets below. Retain explicitly
   referenced identity/evidence and action dedupe records in v1; merely listing
   an execution never makes it referenced. Key pruning awaits an explicit
   retry-horizon/tombstone contract. No full
   Temporal history mirror. Cancellation/shutdown release clients and bounded
   in-flight requests.

Proposed REST grouping, finalized with A1's shared types and implemented by C:
`GET /v1/connectors` lists configured connections; execution list/detail/history/
relationships live under `/v1/connectors/:connection_id/executions` using opaque
projection IDs. Declared Query/Update POSTs live below the execution's
`queries/:query_id` and `actions/:action_id`; receipts use
`GET /v1/connectors/:connection_id/operations/:operation_id`. Execution operation
listing uses `GET .../executions/:execution_id/operations`; an explicitly confirmed
resubmission uses `POST .../operations/:operation_id/resubmit`. Reverse links use
`GET /v1/jobs/:id/runs/:run_id/external-executions`. No endpoint accepts an
arbitrary remote endpoint, namespace, handler or unpinned action target.

### Configuration equality and observation budgets

The fingerprint covers enabled configuration, immutable connection targets,
credential references, bindings/schemas and limits, excluding resolved secret
bytes. Matching nodes join the stored epoch. A mismatch refuses connector
readiness/routes before dialing or admitting operations, while local job service
remains healthy. Configuration changes require a coordinated epoch transition:
`CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT` explicitly names the expected
stored value, C1 atomically activates the new fingerprint after identity checks,
and every connector request rechecks the active epoch. Old-config nodes refuse
connector work until restarted with the new file. No implicit adoption or
connection-target repointing is allowed. Concurrent incompatible transitions
fail closed. Document this rollout and mount the same file on all Helm replicas
using existing `config.extraEnv`, `extraVolumes` and `extraVolumeMounts`; do not
invent new chart hooks. Secret rotation under an unchanged reference does not
change the contract fingerprint.

v1 defaults: Console refresh no faster than 10 seconds; per-principal read
budget 60/minute and explicit Query budget 6/minute; four in-flight RPCs per
connection; 10-second RPC deadline; at most 100 entries and 64 KiB of exposed
metadata per page. Unreferenced detail snapshots expire after 24 hours and are
capped at 1,000 per connection, evicting oldest eligible snapshots before insert.
Referenced rows exist only for explicit verified relationships or operations.
Coalescing/freshness caches are bounded and do not turn each GET or Query into a
write. Limits are validated configuration with conservative hard ceilings fixed
by A1 before dependent work. C1/F3 measure unchanged-poll write count, catalog
cardinality and closed-workflow Query dispatch count through the live surface.

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
| F | Real fixture, CI, qualification and promotion (4 items) | P0 | Not started |
| N | Operator guidance and reconciliation (1 item) | P0 | Not started |

## Streams

### Stream A — Generic contract, configuration and catalog

- [ ] A1. Implement provider-neutral contracts/registry and strict versioned
  connection/binding configuration: schemas, secret references, gate validation,
  resource limits, canonical config fingerprint, immutable connection identity,
  stateless execution references, reserved actor envelope and the frozen v1
  correlation schema/activity-type/job allowlist. A test-only adapter with
  different opaque identity/capabilities proves the core needs no Temporal fields.
  Files: new `internal/connector/types.go`, new `internal/connector/registry.go`,
  new `internal/connector/config.go`, new `internal/connector/config_test.go`,
  `pkg/env/env.go`, `pkg/env/env_test.go`, `internal/jobdef/secret/env.go` (reuse),
  new `docs/connectors.md` (configuration/contract/Helm mounting sections),
  `docs/README.md` (index the new top-level guide as in progress in this PR).
  Depends on: none.
  Verify: parser/registry tests refuse unsupported providers, duplicate IDs,
  invalid schemas, reserved actor-field declarations/overrides, inline secrets,
  excessive budgets and invalid enabled/auth combinations. Equivalent config
  canonicalizes identically; contract changes alter the fingerprint and resolved
  secret bytes do not. Disabled configuration preserves defaults. The guide/index
  ship together and pass the docs-index guardrail. C1 supplies actual gate proof.

- [ ] A2. Persist external identities, bounded direct snapshots, typed relation
  evidence, active configuration epoch and action receipt/dedupe records in the
  catalog. Unique indexes and conditional writes enforce observation-generation,
  one-open-operation and receipt invariants. Coalesce unchanged evidence and
  enforce finite unreferenced snapshot retention/cardinality.
  Files: new `internal/models/external_execution.go`,
  new `internal/models/connector_operation.go`, `internal/models/models.go`,
  new `internal/models/connector_configuration.go`,
  new `internal/connector/store.go`, new `internal/connector/store_test.go`,
  `pkg/db/migrations_test.go`, `docs/connectors.md` (storage/retention sections).
  Depends on: A1.
  Verify: concurrent callers cannot regress terminal observations or admit two
  operations for one key or one target/action across different keys. Unknown
  retains the open guard. Concurrent epoch transitions have one winner. Database
  reopen preserves references/receipts/epoch; unchanged reads add no snapshot
  writes, and unreferenced retention/cap enforcement is bounded. Fresh
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
  `docs/connectors.md` (Temporal/authentication sections), `docs/temporal.md`,
  `docs/differentiation-strategy.md` (update dependency claims with SDK addition).
  Depends on: A1.
  Verify: adapter tests cover pagination, deadlines, redaction and status mapping;
  F1 runs this adapter against real Temporal. Start from SDK v1.49.0 (#560's
  example version), record justified changes, and regenerate sums with the
  containerized toolchain. Local tests do not certify a Temporal Cloud account.

- [ ] B2. Implement only binding-declared status Queries/Update actions, JSON
  input/result validation, server-derived actor envelope, pinned targets, stable
  Update IDs and lookup-only reconciliation. Separate missing workers, validator
  rejection and ambiguous RPC outcomes, including not-found after lost rejection.
  Files: new `internal/connector/temporal/messages.go`,
  new `internal/connector/temporal/messages_test.go`,
  `docs/connectors.md` (message semantics sections).
  Depends on: B1.
  Verify: refuse undeclared handlers/custom conversion. F1 proves real Query,
  accepted/completed Update, workflow-visible principal, spoof refusal, validator
  rejection and same-ID lookup. Record actual RPC statuses for the pinned server;
  rejected-but-unaccepted absence is never proof of rejection/no effect. No
  action implicitly targets the latest run or replays during lookup. C2 supplies
  the HTTP proof.

### Stream C — Authenticated API and composition

This stream owns startup, API dependency plumbing, route binding and shared
auth chokepoints. Every endpoint includes a live HTTP integration scenario.

- [ ] C1. Wire clients/observation into startup and feature discovery. Add
  connection list, per-connection execution list/detail/history/relationships
  and reverse links from a Caesium run. Persist bounded direct observations;
  return dated stale detail explicitly on failed refresh. Enforce read/scope,
  rate/concurrency/retention and configuration-epoch policy. Own guarded activation
  and connector readiness on multi-node config mismatch.
  Files: new `internal/connector/observe.go`, new `internal/connector/observe_test.go`,
  new `api/rest/service/connector/reads.go`,
  new `api/rest/controller/connector/reads.go`, `api/rest/bind/bind.go`,
  `api/rest/bind/bind_test.go`, `api/api.go`, `cmd/start/start.go`,
  `api/rest/service/system/system.go`, `api/rest/service/system/system_test.go`,
  `internal/auth/rbac.go`, `api/middleware/auth_scope.go`,
  `api/auth_rbac_policy_completeness_test.go`,
  new `test/connectors_read_test.go`, new `test/connectors_config_cluster_test.go`,
  `scripts/connectors-fixture.sh` (extend F1's fixture for the config-skew proof),
  `docs/connectors.md` (REST/read policy).
  Depends on: A2, B1, D1, F1.
  Verify: live HTTP finds a Temporal-only workflow, pages the unchanged fixture
  without duplicates, exposes genuine IDs/relations, preserves stale detail on
  outage and recovers after Caesium restart. Auth, scoped/agent denial, gate-off
  startup, scope-safe reverse links and refused connection repointing are observed.
  Live unchanged polling/list pages meet the write/cardinality budgets. Two real
  Caesium nodes sharing the catalog accept matching files, refuse mismatches
  before RPC, and enforce a guarded epoch transition including stale-node requests. Reuse
  the existing distributed fixture rather than inventing another cluster harness.

- [ ] C2. Expose declared status Queries, action submissions, per-execution
  operation listing, receipt lookup and explicit original-principal resubmission.
  Enforce operator policy, reserved actor context, schema/conflict errors and
  audited outcomes; lookup never sends an Update or invents completion.
  Files: new `api/rest/service/connector/actions.go`,
  new `api/rest/controller/connector/actions.go`, `api/rest/bind/bind.go`,
  `api/rest/bind/bind_test.go`, `internal/auth/rbac.go`,
  `api/middleware/auth_scope.go`, `api/middleware/auth.go`,
  `internal/auth/audit.go`, `api/auth_rbac_policy_completeness_test.go`,
  new `test/connectors_actions_test.go`, `docs/connectors.md` (action REST/policy).
  Depends on: C1, B2, D2.
  Verify: live HTTP covers Query/approval, viewer/runner/scoped/agent denial,
  undeclared handlers, stale binding/run targets, bad schema, concurrent duplicate
  keys, different operators/keys, conflicting reuse, actor spoofing, rejection,
  lost accepted/rejected replies and explicitly confirmed resubmission. Refresh
  with no browser storage discovers the original operation. Assert actual
  workflow-visible principal, effect count and durable receipt/audit, not merely
  status codes. Different-principal/background resend and config-mismatch RPCs
  are refused. Closed-workflow Queries respect limits and are never auto-polled.

### Stream D — Correlation and action durability

- [ ] D1. Derive `<RunID>/<ActivityID>` from trusted Temporal identity and use a
  read-only admission lookup over A1's activity/job allowlist. Validate optional
  A1-defined heartbeat/result hints against that identity and #560's records;
  persist typed links with source evidence. Follow queued promotion without
  starting work. Extend the Temporal recipe using the already-frozen envelope.
  Files: new `internal/connector/correlation.go`,
  new `internal/connector/correlation_test.go`, `internal/run/start_idempotency.go`,
  `internal/run/start_idempotency_test.go`, `docs/temporal.md` (activity envelope).
  Depends on: A2, B1, F1.
  Verify: the real fixture reports queued/running/finished admission. C1 proves
  bidirectional public links. Wrong/missing/ambiguous admission, another job's
  run, another workflow's valid admission or an unrelated activity cannot become
  verified. Metadata-only/custom-payload activity links work without decoding
  payloads or reconstructing start request hashes. Repeat reads/retries create
  one relation; promotion/restart preserve identity. Existing #560 regressions pass.

- [ ] D2. Implement transactional receipt admission/dedupe, the target/action
  open-operation guard and lookup-only provider resolution. Snapshot the binding,
  actor context and stable Update ID before dispatch; concurrent callers/restart
  attach to the original pinned operation. Only explicit original-principal
  confirmation can resend an unknown operation. Bound/redact persisted content.
  Files: new `internal/connector/operations.go`,
  new `internal/connector/operation_store.go`,
  new `internal/connector/operations_test.go`,
  `docs/connectors.md` (receipt/recovery sections).
  Depends on: A2, B2, F1.
  Verify: real fixture plus store concurrency tests cover lost accepted reply,
  crash after local admission, restarted lookup and changed-input refusal. Drop
  a validator-rejection reply, later make the workflow eligible, then prove
  background lookup/restart never approves it: not-found stays unknown until an
  explicit operator decision. Different fresh keys/actors cannot bypass the open
  guard; terminal/new-ID attempts cause no second logical approval effect. C2
  provides the public-surface proof and records actual pinned-server behavior.

### Stream E — Console operator experience

Use one external-execution feature area with provider/connection selection,
joined to existing Caesium pages. No cross-provider global pagination in v1.

- [ ] E1. Add connection/execution navigation, typed API calls, filters,
  paginated detail/history, native status, observation age and connection health.
  Show unbound workflows, unavailable payloads/partial history, error/recovery
  states and configured Temporal UI links. Gate navigation and direct routes.
  Introduce an isolated `connectors` Playwright project now, outside `auth`, with
  no default/auth dependencies; exclude its directory from the default project.
  Its helper asserts connector/auth/real-fixture lane markers and server features
  when selected, rather than skipping absent prerequisites.
  Files: new `ui/src/features/connectors/ConnectionsPage.tsx`,
  new `ui/src/features/connectors/ExecutionDetailPage.tsx`,
  new `ui/src/features/connectors/types.ts`,
  new `ui/src/features/connectors/__tests__/monitoring.test.tsx`,
  `ui/src/lib/api.ts`, `ui/src/router.tsx`,
  `ui/src/components/layout/Sidebar.tsx`, `ui/src/features/auth/access.ts`,
  `ui/playwright.config.ts`, new `ui/e2e/helpers/connectors.ts`,
  new `ui/e2e/connectors/monitoring.spec.ts`, `docs/connectors.md` (Console tour).
  Depends on: C1.
  Verify: browser filters/inspects the actual external-only workflow, sees dated
  stale details on outage, recovers and renders unsupported capabilities honestly.
  Gate-off/unauthorized routes disclose no workflow details. Playwright discovery
  proves existing default/auth projects select no connector specs; the connector
  project selects all its specs and fails on missing gate/auth/fixture markers.

- [ ] E2. Render typed parent/child/delegated-run relations and reverse links
  on existing run detail. Follow verified runs to logs, receipts and data evidence
  with original permissions. Label dataset declarations separately from evidence.
  Files: new `ui/src/features/connectors/RelationshipsPanel.tsx`,
  new `ui/src/features/connectors/__tests__/relationships.test.tsx`,
  `ui/src/features/connectors/ExecutionDetailPage.tsx`,
  `ui/src/features/jobs/RunDetailPage.tsx`,
  new `ui/e2e/connectors/correlation.spec.ts`, `docs/connectors.md` (data tour).
  Depends on: E1, D1.
  Verify: browser follows workflow -> admitted run -> existing receipt/data
  evidence -> workflow; queue promotion becomes the same run. Opaque external
  work has no invented lineage/freshness; scoped users see permitted local data.

- [ ] E3. Render the declared approval form, server operation list/receipt,
  exact target, acceptance/completion distinction and validator result. Recover
  from server state after refresh/new tab; browser key storage is not authority.
  Unknown receipts expose an explicit original-principal resend confirmation;
  refresh/poll never resends. Gate on live observation, binding compatibility,
  authorization and capability; no timeout success toast.
  Files: new `ui/src/features/connectors/ActionPanel.tsx`,
  new `ui/src/features/connectors/OperationReceipt.tsx`,
  new `ui/src/features/connectors/__tests__/actions.test.tsx`,
  `ui/src/features/connectors/ExecutionDetailPage.tsx`, `ui/src/lib/api.ts`,
  new `ui/e2e/connectors/actions.spec.ts`, `docs/connectors.md` (approval tour).
  Depends on: E2, C2.
  Verify: real approval advances the workflow; rejection is explicit. Lost reply
  stays pending/unknown until resolved; refresh without local storage, duplicate
  clicks and two operators with fresh keys recover/conflict on one open receipt.
  A lost rejection reply followed by eligibility never sends approval until the
  operator explicitly confirms it. Denied users/unbound/stale workflows cannot
  invoke an action; terminal/new-key attempts produce no second business effect.

### Stream F — Real Temporal fixture, CI and qualification

One owner serializes the local Docker/browser lane. Reuse the builder and
compiled integration-runner approach; fixture code stays in the root module.

- [ ] F1. Build a pinned isolated Temporal development fixture with real SDK
  worker: external-only workflow, and publication workflow that invokes a Caesium
  job through #560, reports A1's frozen correlation envelope, awaits approval
  and invokes a second job. Include Query, validator rejection, worker restart,
  child/continuation identity and connector-path interruption. Record the server-derived principal
  in workflow state/history; reject forged context and enforce one logical
  approval effect, including distinct Update IDs. Include lost validator-reply
  followed by changed eligibility, and metadata-only activity correlation.
  Independently record effects/starts.
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
  activation, including dropped accepted/rejected replies and exact subsequent
  lookup statuses for the pinned server. Missing worker/fixture is failure.
  The fixture consumes A1's schema and does not wait for D1 to define it.
  C1/C2 complete adapter qualification.

- [ ] F2. Add `integration-up-connectors` and `integration-test-connectors`, an
  authenticated HTTP/browser CI lane initially registered in `UNPROMOTED_LANES`.
  The tracked server recipe preserves baseline `integration-up` envs and adds
  connector/auth/config flags plus `CAESIUM_RUN_QUEUE_ENABLED=true` and
  `CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true`. Runner/fixture markers agree.
  The script orchestrates this recipe, never starts an untracked alternate server.
  Include C1's config-skew proof using the existing isolated kind/Helm fixture;
  preserve baseline server envs there too and record the tested node topology.
  Select with existing broad `go`/`ui`/`ci` outputs on PR/merge-group/push, covering
  shared auth/run/UI dependencies without a new `changes` output. Reuse the
  committed scenario manifest and strict checker for candidate-bound evidence;
  do not introduce a connector-specific validator or immediately require the lane.
  Files: `justfile`, `internal/guardrails/guardrails_test.go`
  (`integrationUpTrackingRecipes`), `.github/workflows/ci.yml`,
  `scripts/ci-ok.py` (`UNPROMOTED_LANES`), `scripts/test_ci.py`,
  `test/contracts/scenarios.json`, `scripts/test_test_evidence.py`,
  new `scripts/connectors-test.sh`, `scripts/connectors-fixture.sh`,
  `docs/ci.md` (connector lane), `docs/connectors.md` (verification commands).
  Depends on: C2, E3, F1.
  Verify: proposed `just integration-test-connectors` executes all manifest-named
  live scenarios and E1's dedicated Playwright project. Run
  `scripts/check-test-evidence.py --manifest test/contracts/scenarios.json --report <report> --require connectors --strict`.
  Gate-off/queue-off misconfiguration, zero count, fixture failure, failed first
  browser attempt, missing artifacts or wrong candidate SHA fail the lane, which
  remains explicitly advisory to `ci-ok` until F4. Env-parity guardrail includes
  the new recipe with no bypass; report/mutation tests use the existing checker.
  Run actionlint and existing CI regression tests, including shared-path selection.

- [ ] F3. Record first-release acceptance on the merged candidate: external-only
  monitoring, correlated publication, approval/rejection, activity retry, reply
  loss (accepted and rejected), server/worker restart, outage/recovery, distinct
  actors/keys, config skew/transition, bounded polling/storage and continuation
  without retargeting old actions. Also run default-disabled regression gates.
  Files: `docs/exec-plans/active/execution-connectors.md` (acceptance record only).
  Depends on: F2.
  Verify: record source/image identity, fixture pins, HTTP/browser evidence,
  independent effect counts and receipts. Missing fault proof or unresolved
  identity is inconclusive. Local evidence does not certify Cloud/all releases.

- [ ] F4. Promote the connector lane only after 10 consecutive green selected
  `master` runs finish within 15 minutes after `changes`, per the CI runbook. Record
  exact runs, candidate identities, evidence and durations; a local/PR pass is
  not promotion evidence. In one PR, remove its advisory registration, add the
  same broad selectors to `SELECTORS`, register gate `connectors` in
  `EVIDENCE_LANES`, and wire `ci-ok` needs/artifact retrieval/`--lane-evidence`.
  Reconcile the CI runbook/required-set records named by its promotion procedure;
  preserve `ci-ok` as the repository's required check without settings changes.
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`,
  `docs/ci.md`, `docs/exec-plans/completed/trust-the-substrate.md`
  (required-set record), `docs/connectors.md` (gate status),
  `docs/exec-plans/active/execution-connectors.md` (promotion record only).
  Depends on: F3.
  Verify: replay fail-closed gate cases for selected failed/skipped/missing jobs,
  absent/foreign/hollow reports and failed scenario/fault proof. Unselected skips
  are permitted only by successful broad selectors; shared dependencies select
  the lane. Promotion proof includes 10 runs and timing, plus actual promoted
  PR/merge-group evidence. If evidence/budget is unmet, F4 remains open/advisory.

### Stream N — Operator guidance and reconciliation

- [ ] N1. Consolidate accepted behavior/tour in the connector guide; reconcile
  the Temporal recipe and the strategy amendment already included here/B1.
  Update roadmap/navigation only from merged evidence. Preserve at-least-once
  and cancellation limitations, deferred scope and the later second-provider check.
  Files: `docs/connectors.md`, `docs/temporal.md`, `docs/README.md`,
  `docs/roadmap.md`, `docs/differentiation-strategy.md`,
  `docs/exec-plans/active/execution-connectors.md`.
  Depends on: F4.
  Verify: documented local tour reproduces actual API/Console inspection and
  approval. Schema/commands match merged code, links resolve and shipped claims
  cite accepted compatibility/evidence. Deferred capabilities stay deferred.

## Sequencing & Dependencies

**16 included items in seven streams.** Dependencies are explicit above;
unchecked sibling work is not a hidden prerequisite.

- First ready item: **A1**; no Temporal account/server required.
- After A1: **A2 and B1** can proceed independently.
- After B1: **B2 and F1**; F1 supplies the pinned external fixture.
- **D1** needs A2/B1/F1; **D2** needs A2/B2/F1. Core files are disjoint.
- **C1** joins A2/B1/D1/F1; **C2** follows C1/B2/D2.
- **E1** can run on merged C1 while C2 develops; **E2 -> E3** serializes
  shared detail UI. E3 also needs merged C2.
- **F2 -> F3 -> F4 -> N1** wires an advisory lane, qualifies the release,
  accumulates master promotion evidence, promotes and documents the release.

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
| Fixture/CI recipes/guardrail | F1 then F2 for `justfile`; F2 owns tracked recipe/guardrail and advisory CI wiring; F4 owns later promotion in the same CI files |
| Fixture orchestration | F1 creates `scripts/connectors-fixture.sh`; C1 adds config-skew support, then F2 wires lane execution |
| Playwright selection/helper | E1 owns configuration/helper when the first specs land; E2/E3 add specs in its isolated project; F2 selects that existing project |
| Connector guide | One integration writer; section integration order A1 -> A2 -> B1 -> B2 -> F1 -> D2 -> C1 -> C2 -> E1 -> E2 -> E3 -> F2 -> F4 -> N1 |
| Other docs/Progress | A1 indexes its new guide; B1 corrects Temporal/strategy dependency claims, D1 extends the frozen envelope recipe, then N1 reconciles. F2 -> F4 own CI lane text/records. Orchestrator owns Progress; F3/F4 contribute evidence |

Guide sections ship with implementation. Independent streams may overlap, but
only the assigned integration writer edits the guide; serialize its edits in
the listed order before publication/rebase. Do not defer required tests/docs to
unowned PRs. Refresh shared-file ownership against live plans/branches at dispatch.

### External prerequisites and visible decisions

F1 supplies the pinned reachable Temporal fixture/worker; successful workflow
start/Query/Update and independent effect records prove readiness. A server
without a worker is insufficient. C1/F2 also require the existing isolated
kind/Helm cluster fixture for config equality. F2 requires Docker, existing
builder/runner images and Chromium. Own task-created containers, networks,
volumes, ports and fault controls; never reuse/remove foreign resources.
Serialize shared tags/ports.

If identity-and-access merges before C1/C2, reconcile route/principal integration
in C and record its merged identity. A/B/D are independent of that unshipped
plan. No unanswered product decision blocks A1. A1 freezes schemas, hard budget
ceilings and config equality; F1 pins the SDK/server matrix before qualification.
The defaults above apply unless a reviewed contract revision changes them.
Identity/authority/replay/auth/scope changes
require revising this contract before dependent implementation.

Deferred: second provider; dynamic loading; config hot reload/admin UI; custom
payload codecs; background fleet import/global pagination; live Temporal Cloud
qualification; workflow start/Signals/reset/terminate/schedule administration;
and propagated Caesium cancellation. Nexus may be evaluated later for reusable
execution handoffs; it is not a monitoring prerequisite.

## Verification (Run For Every PR)

For this planning-only PR: validate unique IDs, required fields, existing versus
explicitly new paths, dependencies/acyclicity, ownership, template-token absence,
relative links and `git diff --check`. Run the existing `internal/guardrails`
package in the builder container, including the top-level docs index check.
These are not connector runtime qualification.

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

CI/recipe changes run actionlint and the existing CI regression suite. Existing
broad `go`/`ui`/`ci` outputs select the new job on PR and merge-group events;
push selects it too. F2 validates its report against the shared manifest/checker
while advisory; only F4 makes result and evidence required by `ci-ok` after the
10-run/timing promotion proof. Capture Caesium/Temporal/worker logs,
fault activation, history/effects, receipts, browser reports and candidate/image
IDs. One owner holds the local Docker/browser lane through cleanup. No production
account, external message delivery or repository-settings change is required.
Default-disabled tests continue to run without any Temporal process.

## Acceptance Criteria

1. Default Caesium starts/runs jobs without Temporal configuration/processes.
   Connector routes/navigation are absent; invalid enabled config fails redacted;
   remote outage preserves local readiness. Shared config fingerprints match or
   refuse connector work before RPC, including epoch changes/stale nodes. Proof:
   startup/multi-node HTTP/browser scenarios and documented Helm mount/rollout.
2. Real configured Temporal workflows, including external-only/unbound work,
   are discoverable/inspectable with native identity/status, reported activity
   metadata, typed relationships, pagination and completeness. Proof: API/browser
   against the pinned real fixture.
3. Outage, missing workers/payloads, unavailable history and list lag appear as
   explicit stale/unavailable/partial information. Direct state cannot regress;
   restart preserves identity and recovery restores observation. Proof: activated
   faults, out-of-order writes and live restart/recovery scenarios. Unchanged
   polling creates no snapshot writes, lists create no catalog rows, unreferenced
   snapshots stay within finite age/cardinality and Queries are explicit/bounded.
4. Verified publication activity links resolve queued promotion/retry to one
   admitted Caesium run. Both directions reach existing run/data evidence;
   opaque external work gains no inferred lineage/freshness/receipt. Proof:
   public reads/browser journey, admission counts, trusted RunID/ActivityID key
   derivation and another-workflow mismatch refusals, including opaque payloads.
5. Declared authorized approval returns validated receipt/result and advances
   the actual workflow to its second Caesium job. Rejection is explicit;
   different keys/operators recover or conflict on one open operation. Lost
   rejection/lookup absence remains unknown with no background resend; only an
   explicit original-principal confirmation may resend. The workflow enforces
   one logical effect even across terminal/new-ID attempts. Continuation never
   retargets it. Proof: HTTP/browser, Update history and independent effects.
6. Server-side auth/scope checks permit intended reads and refuse unauthorized,
   scoped/agent access and runner mutations. Reverse links retain job scope.
   Secrets/undeclared payloads do not leak; reserved server-derived actor context
   reaches the remote validator/history and spoofing is refused. Proof:
   authenticated HTTP/browser, workflow-principal and secret/audit assertions.
7. All selected HTTP/browser scenarios actually execute with real servers.
   Failure/absence/flake/wrong-SHA evidence fails the shared strict checker and,
   after F4's 10-master-run/budget proof, `ci-ok`; default-disabled gates pass.
   Existing default/auth projects exclude connector specs. Proof identifies the
   candidate and fixture pins; Cloud/all-version qualification is outside the claim.
8. Completion requires verified merged evidence. Progress/docs/status links
   match accepted behavior and retain deferred work. Proof: F3/F4 records and N1's
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
