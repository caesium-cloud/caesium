# Identity and Access — Namespaces, Grants, Policy-as-Code, Corporate SSO

Last updated: 2026-09-16

Caesium's authentication is shipped and native (OIDC, SAML, LDAP; `docs/sso-authentication.md`),
but authorization is a single global role ladder keyed per HTTP route, SSO users cannot be
scoped, there is no tenancy primitive anywhere, SSO admins cannot manage API keys, users have
no lifecycle, and no SSO path has end-to-end coverage. That is not usable by a corporation
that wants Active Directory, LDAP, SAML or Keycloak identity to flow through into
per-team permissions.

This plan ships the design in
`docs/superpowers/specs/2026-09-16-identity-and-access-design.md`: a flat **namespace**
declared on the job manifest and carried by every job-owned resource; **grants**
(`role @ namespace`, `*` for cluster-wide) resolved per request from one **policy-as-code
file** that binds IdP groups or user emails to roles and declares each namespace's
isolation settings (Kubernetes namespace and service account, secret allow-list, run
quota); enforcement at the existing middleware chokepoint plus one shared collection
filter; bounded-staleness **IdP group refresh**; **user administration**; namespaced
notification channels, policies and agent profiles; a **Keycloak** CI lane that drives
real logins; a namespace switcher and Access page in the Console; and `caesium login`
for an SSO-backed, user-attributed CLI credential. Everything is additive and
default-preserving: with no policy file the server behaves exactly as today.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work
backlog, `## Sequencing & Dependencies` captures cross-stream order,
and `## Acceptance Criteria` lists the gates that close out the entire
plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies
   are satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every
   PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Strategic Decisions

Recorded in the spec's §4 decision table; the ones that shape the streams:

- **Namespace, flat, declared in the policy file** (not label selectors, not a hierarchy).
  The name stays `namespace` even though datasets carry an OpenLineage `namespace`
  field for a different axis; job aliases stay globally unique.
- **Policy-as-code**, GitOps-reviewed, reloadable. No DB-managed roles, no admin editing
  of bindings in the Console.
- **Role per namespace** with the existing four roles; the job-alias allow-list on API
  keys remains as a further narrowing. No verb taxonomy in v1.
- **Hybrid enforcement**: the middleware decides per route class and injects the
  allowed-namespace set; collection handlers apply one shared filter; a completeness
  test fails the build on an unclassified route.
- **Keycloak is a first-class OIDC issuer**, exercised end-to-end in CI. Keycloak as a
  policy decision point is future work.
- **Docs consolidate**: the SSO guide is renamed to `docs/identity-and-access.md` and
  grows; the SSO spec is archived as superseded; this plan and the new spec are the only
  new files.

## Source-Of-Truth Note

When this plan and `docs/superpowers/specs/2026-09-16-identity-and-access-design.md`
disagree, the spec wins. When the spec and `pkg/jobdef/definition.go` disagree about
the manifest contract, the schema wins once Stream B1 has landed. Per-namespace run
fairness was parked by `docs/exec-plans/active/window-scheduling.md` (its §"Global
vs. per-job load ceiling"); Stream D of this plan owns the v1 slice (issue #395) and
window-scheduling's parked note points here.

## Progress (as of 2026-09-16)

No implementation waves have shipped yet. The plan was published with the
brainstorm alignment of 2026-09-16 (spec §4); the first wave is the next eligible
run of the `exec-plan-wave` skill against this doc. Wave-1 leaf items are listed under
`## Sequencing & Dependencies`.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Policy file, grants, principal, route classes, namespace-aware middleware, keys with namespaces, SSO-admin key management | **P0** | Not started |
| B | `metadata.namespace` on jobs: schema, columns, importer, lint, apply/prune/move, git-sync allowlist, collection filtering, aggregate collapse | **P0** | Not started |
| C | Runtime isolation: per-namespace Kubernetes namespace + service account, scoped secret resolution, Helm RBAC | P1 | Not started |
| D | Run quota + round-robin fairness (#395 v1 slice); namespaced notification channels, policies, agent profiles | P1 | Not started |
| E | Identity lifecycle: IdP group refresh, user/session administration, user-bound keys, `caesium login` | P1 | Not started |
| F | Console: grants-aware gating, namespace switcher, namespace chips, quota label, Access page | P1 | Not started |
| G | Keycloak: realm fixture, CI lane, Playwright + Go scenarios, operator guidance | P1 | Not started |
| H- | Harness: integration server policy fixture, `--json` stdout discipline for new verbs | — | Not started |
| N- | Docs: rename + grow the identity-and-access guide; roadmap, README, sovereignty | — | Not started |

## Streams

### Stream A — Authorization core: policy file, grants, route classes

The spine everything else hangs off. Ships the `internal/auth/policy` package (parse,
validate, resolve, glob, reload), replaces `Principal.Role` with a grants map, turns the
route policy table into classified `RoutePolicy` entries, teaches the middleware to
resolve a target namespace per route and to inject the allowed-namespace set for
collections, gives API keys namespaces, and makes every `/v1/auth/*` controller accept
any principal so SSO admins are first-class. With no policy file the env role mapping is
translated into cluster-wide bindings and behaviour is unchanged.

- [ ] A1. Add the policy package: `AccessPolicy` schema (namespaces with `kubernetes`,
      `secrets.allow`, `quotas`; bindings with `subjects.{groups,users}`, `role`,
      `namespaces`), YAML parse, validation per spec §6.2 (DNS-label namespaces, `*`
      rejected as a key, declared-namespace check on bindings, role validity,
      non-empty subjects), `Resolve(groups, email) Grants` (highest role per namespace,
      `"*"` group wildcard, case-insensitive email), the `provider/path` glob matcher
      with `*` and `**`, and lint warnings (non-`default` namespace without secret
      rules). Pure package, table-driven unit tests, no wiring.
      Files: new `internal/auth/policy/policy.go`, new `internal/auth/policy/resolve.go`,
      new `internal/auth/policy/glob.go`, new `internal/auth/policy/policy_test.go`.
- [ ] A2. Replace `Principal.Role` with `Grants` (`RoleIn`, `ClusterRole`,
      `AllowedNamespaces`); `PrincipalFromUser(user, policy)` resolves grants from the
      stored groups + email on every request; `PrincipalFromKey` maps the key's
      `Namespaces` (NULL ⇒ `["*"]`) to grants; translate `CAESIUM_AUTH_ROLE_MAPPING` +
      `CAESIUM_AUTH_DEFAULT_ROLE` into cluster-wide bindings when no policy file is set
      (`rolemap.go` becomes that translation); update the two readers of
      `Principal.Role` (middleware metrics/audit label, whoami) to the cluster role.
      `User.Role` stays as "cluster role at last login" for the users list only.
      Own the login tail: `SSOService.Complete` (`internal/auth/provider.go`) calls
      `RoleMapper.Resolve(groups)` before provisioning; rewrite it to
      `policy.Resolve(groups, email)` through a `policy.Source` interface that A2
      defines and wires in `cmd/start/start.go` with the env-translated
      implementation (A4 later swaps in the file loader behind the same interface
      without touching that block again), deny with `no_binding`, store the cluster
      role on the user. Unit tests: policy-only and email-only bindings log in;
      no binding is denied.
      Files: `internal/auth/principal.go`, `internal/auth/rolemap.go`,
      `internal/auth/provider.go`, `internal/auth/provider_test.go`,
      `internal/auth/principal_test.go`, `internal/auth/rolemap_test.go`,
      `internal/models/api_key.go` (`Namespaces datatypes.JSON`),
      `api/middleware/auth.go`, `api/rest/controller/auth/sso.go`, `cmd/start/start.go`.
      Depends on: A1.
- [ ] A3. Classify every route and make the middleware namespace-aware: `endpointPolicy`
      values become `RoutePolicy{Role, Class}` with classes `cluster | namespaced |
      collection | identity` per spec §8.1; the middleware requires the cluster role for
      `cluster`, resolves the target namespace(s) for `namespaced` (jobs by id, runs,
      backfills, incidents, triggers via job, datasets via producing job, channels /
      policies / profiles by object, `POST /v1/jobs` and `/v1/jobdefs/{apply,lint,diff}`
      from every definition's `metadata.namespace`, apply-with-prune against the
      server-computed prune set, a namespace move requiring operator in both, replay
      requiring viewer on the historical run's namespace **and** runner in the job's
      current one, key routes against the key's namespaces, audit against
      `?namespace=`, dataset routes including the manual `advance` against the
      declared producers) and requires the
      role in every resolved namespace; injects `ContextKeyAllowedNamespaces` for
      `collection`; passes `identity` (`/auth/whoami`, `/auth/logout`,
      `/v1/system/features` only — `GET /metrics` is **cluster** viewer because the
      registry carries job aliases). The existing `authorizeScope` (job allow-list,
      agent claim) runs unchanged after the namespace decision. Add `internal/authz`
      with `AllowedNamespaces(ctx)`, `NamespaceFilter(db, column)`, `MarkFiltered(c)`.
      Add `AuditLog.Namespace` and fill it from the decision; `caesium auth audit
      --namespace`. Extend the completeness test so an unclassified mounted route fails
      the build, and add the test-only post-handler check that a `collection` handler
      marked the filter.
      Files: `internal/auth/rbac.go`, `internal/auth/rbac_test.go`,
      `api/middleware/auth.go`, `api/middleware/auth_scope.go`,
      `api/middleware/auth_test.go`, new `internal/authz/authz.go`,
      new `internal/authz/authz_test.go`, `internal/models/audit_log.go`,
      `internal/auth/audit.go`, `api/rest/controller/auth/audit_query.go`,
      `cmd/auth/audit.go`, `api/auth_rbac_policy_completeness_test.go`.
      Depends on: A2 + B1 + D3.
- [ ] A4. Wire the policy loader: `CAESIUM_AUTH_POLICY_FILE`,
      `CAESIUM_AUTH_POLICY_RELOAD_INTERVAL` (default `10s`); parse at startup (fatal on
      error; fatal when both the file and `CAESIUM_AUTH_ROLE_MAPPING` are set); an
      `atomic.Pointer` holder consulted by the middleware and by every consumer of
      namespace settings; mtime poll + `POST /v1/auth/policy/reload` (cluster admin);
      invalid revision keeps the last good one, logs, bumps
      `caesium_auth_policy_reload_failures_total`, audits `policy.reload_failed`;
      success audits `policy.reloaded` with the file hash; `GET /v1/auth/policy`
      returns the effective policy + revision; CLI `caesium auth policy lint --path`
      (offline, `--json`), `show [--json]`, `reload`. Integration test: reload a valid
      revision (grants change on the next request), then an invalid one (last good
      retained, audit row present), through the CLI with `runCLIStdout`.
      Files: `pkg/env/env.go`, `cmd/start/start.go`, new `internal/auth/policy/loader.go`,
      new `api/rest/controller/auth/policy.go`, `api/rest/bind/bind.go`,
      new `cmd/auth/policy.go`, `internal/metrics/metrics.go`, `internal/auth/audit.go`,
      new `test/auth_policy_test.go`. A4 adds its two route entries to `rbac.go` in
      whatever shape the table has when it lands (plain role before A3, classified
      after).
      Depends on: A1 + A2.
- [ ] A5. API keys with namespaces and SSO-admin key management: `Namespaces` column
      (NULL ⇒ cluster), `caesium auth key create --role R --namespace NS [--namespace …]
      [--jobs …]`, `POST /v1/auth/keys` accepts `namespaces`; the minting constraint (a
      caller may only mint/rotate grants ⊆ its own, per namespace); `GET /v1/auth/keys`
      becomes a `collection` (a namespace admin sees only keys confined to its
      namespaces); every `/v1/auth/*` controller switches from `middleware.GetAuthKey`
      to `middleware.GetPrincipal` with `CreatedBy` / audit actor = `principal.Subject`;
      `/auth/whoami` adds `grants`, `namespaces`, `cluster_role` (keeps `role`).
      Integration: key lifecycle via CLI and REST with namespaced keys, the
      escalation refusal, whoami shape (`runCLIStdout` for `--json`).
      Files: `internal/models/api_key.go`, `internal/auth/service.go`,
      `internal/auth/service_test.go`, `api/rest/controller/auth/key_create.go`,
      `api/rest/controller/auth/key_list.go`, `api/rest/controller/auth/key_revoke.go`,
      `api/rest/controller/auth/key_rotate.go`, `api/rest/controller/auth/sso.go`,
      `cmd/auth/key_create.go`, `cmd/auth/key_list.go`, `test/auth_keys_test.go`.
      Depends on: A2 + A3.
- [ ] A6. Deprecate the static shared secrets: `CAESIUM_MANUAL_TRIGGER_API_KEY` and
      `CAESIUM_EVENT_INGEST_API_KEY` still work for one release, log a startup
      deprecation naming the replacement (a runner key confined to a namespace or job
      list), and audit their requests with actors `legacy:manual-trigger` /
      `legacy:event-ingest`. `POST /v1/events` becomes `namespaced` over the routed
      jobs' namespaces and reports `delivered` / `skipped_namespaces`. Integration:
      the legacy actor appears in `caesium auth audit --json`; a namespaced runner key
      ingests into its namespace only and the response reports the skip.
      Files: `pkg/env/env.go`, `api/rest/controller/trigger/put.go`,
      `api/rest/controller/event/ingest.go`, `internal/auth/audit.go`,
      `test/event_trigger_test.go`, `docs/identity-and-access.md`.
      Depends on: A3 + A5 + N-1.
- [ ] A7. The namespace allow/deny matrix: `TestNamespaceAllowDenyMatrix` in `test/`
      (auth lane, `requireAuthLane`) drives one key per role × namespace (`default`,
      `marketing`, `finance`, `*`) from the H-1 policy fixture through every route
      class against the live api-key server — including a job moved between
      namespaces read by principals confined to each side, `/metrics` refused to a
      confined key, and `jobdefs/diff` with an empty body from a confined viewer —
      single-resource reads and writes in an allowed and a denied namespace, collection
      lists containing only allowed rows, cluster routes refused to namespace-confined
      keys, identity routes open to all, aggregate collapse (B5), events without
      `run_id` filtered — asserting status codes and the audit `namespace` column.
      Files: new `test/auth_namespace_matrix_test.go`, `test/auth_scoped_test.go`
      (shared helpers).
      Depends on: A3 + A5 + B2 + B4 + B5 + B6 + D4 + H-1.
- [ ] A8. Confine agent-session allow-lists to the incident's namespace:
      `FreezeAllowlist` today walks the global lineage graph, so a frozen job list can
      name jobs in other namespaces. At freeze, keep only jobs in the incident's
      namespace and record foreign downstream jobs as an `external` count in the
      bundle; at serve time the agent context, bundle and MCP handlers re-check each
      allow-listed job's current namespace and drop any that moved out; the agent
      history service (`api/rest/service/agent` `History`, which today always admits
      the incident's own job) serves only runs whose **persisted** namespace equals
      the incident's, for allow-listed jobs and the own job alike. Integration on the
      agent lane: an incident whose lineage crosses namespaces yields a bundle with no
      foreign alias; the agent token gets 403/404 on the foreign job's context route;
      a job moved into the namespace brings no foreign history through REST or MCP.
      Files: `internal/incident/allowlist.go`, `internal/incident/session.go`,
      `api/rest/service/agent/context.go`, `api/rest/controller/agent/context.go`,
      `api/rest/controller/agent/mcp.go`, `api/middleware/auth_scope.go`,
      `test/agent_mcp_test.go`.
      Depends on: A3 + B1.

#### Rejected for v1 (recorded)

- Verb-level permissions / custom roles — spec §19; `RoutePolicy` leaves room without
  another schema change.
- Keycloak authorization services as a policy decision point — a later `PolicySource`.

### Stream B — Namespace on jobs: manifest, columns, apply semantics, collections

Gives every job an owner and makes every job-derived read and write honour it.
`metadata.namespace` on the manifest, the columns and their denormalised copies, the
importer, server-side lint, apply/prune/move semantics, the git-sync per-source
allowlist, and the collection filtering and aggregate collapse that Stream A's
middleware relies on.

- [ ] B1. Add `metadata.namespace` to the manifest and the columns behind it:
      `Metadata.Namespace` in the definition (syntax validation in `Validate()`,
      default `default`), `Job.Namespace` / `JobRun.Namespace` / `Backfill.Namespace`
      (text, not null, default `'default'`, indexed) and `Incident.Namespace`
      tightened from nullable to the same shape; the importer writes it on apply and
      the run/backfill/incident creators copy it from the job; JSON tags so every API
      object carries `namespace`; `caesium job export` round-trips it (the exporter writes it back into the manifest); the report generator
      documents it so `docs/job-schema-reference.md` regenerates; one example manifest
      declares a namespace. **Every** job and run constructor stamps it: the legacy
      `POST /v1/jobs` service (`MetadataRequest` gains `namespace`), `Store.admit` /
      `Start*`, `StartForBackfill`, the incident store, and the quarantined replay
      path, which inserts `JobRun` rows directly and must stamp the job's current
      namespace. **The namespace is part of the cache key** (`internal/cache/hash.go`)
      and B1 owns its propagation from the persisted run into **both** hash-input
      construction sites (`buildTaskHashInput` in `internal/job/job.go` and the
      worker's `Execute` in `internal/worker/runtime_executor.go`) and the persisted
      hash blob, so a move invalidates cached outputs in local and distributed
      execution; pin with a hash test and an integration scenario (cached run → move →
      miss). Incident deduplication (`incident.DedupeKey`) gains the run's persisted
      namespace so a post-move failure opens a new incident in the new namespace.
      Offline `caesium job lint` validates syntax only.
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/definition_test.go`,
      `pkg/jobdef/schema.go`, `internal/jobdef/report/report.go`,
      `docs/job-schema-reference.md`, `internal/models/job.go`, `internal/models/run.go`,
      `internal/models/backfill.go`, `internal/models/incident.go`,
      `internal/jobdef/importer.go`, `internal/run/store.go`,
      `api/rest/controller/job/post.go`, `api/rest/service/job/job.go`,
      `internal/replay/replay.go`, `internal/backfill/` (creator),
      `internal/incident/store.go` (namespace + `DedupeKey`), `internal/cache/hash.go`,
      `internal/cache/hash_test.go`, `internal/job/job.go` (`buildTaskHashInput`),
      `internal/worker/runtime_executor.go` (hash input), new `test/cache_namespace_move_test.go`,
      `internal/jobdef/exporter.go`, `docs/examples/`,
      `docs/caesium-job-llm-reference.md`, `docs/job-definitions.md`.
- [ ] B2. Server-side namespace semantics: `POST /v1/jobdefs/lint` and apply reject an
      undeclared namespace (from the A4 policy holder) naming it; a namespace move keeps
      history (runs, receipts, incidents) on the original namespace and rewrites only
      `jobs.namespace`; the apply preview lists namespaces touched; `caesium job diff`
      shows a namespace change as a metadata diff. **Diff and lint baselines are
      filtered**: `POST /v1/jobdefs/diff` compares against every persisted job
      (`jobdiff.LoadDatabaseSpecs`) and reports absent jobs as `removed`, so the
      baseline, the `removed` list and cross-job contract findings are restricted to
      allowed namespaces before comparison, and a request alias that already exists in
      a namespace the caller lacks is a 403 naming it. Integration: lint rejection,
      apply into `marketing`, move `marketing` → `finance` with a key holding operator
      in both (and refusal with one), prune refused when the prune set crosses into a
      namespace the caller lacks, diff with an empty `definitions` array from a
      marketing viewer returns no finance job.
      Files: `internal/jobdef/lint/`, `internal/jobdef/importer.go`,
      `api/rest/controller/jobdef/apply.go`, `api/rest/controller/jobdef/lint.go`,
      `api/rest/controller/jobdef/diff.go`, `internal/jobdef/diff/`,
      `test/jobdef_runtime_test.go`, new `test/jobdef_namespace_test.go`.
      Depends on: B1 + A3 + A4.
- [ ] B3. Git-sync per-source namespace allowlist: `GitSourceConfig.Namespaces
      []string` in the JSON source entry; a source with the field applies only
      manifests whose namespace is listed, reports and skips violations per manifest
      (their aliases stay **protected from pruning** in `desiredAliases`, so a
      forbidden move leaves the existing job active and unchanged), and audits the sync as
      `system:git-sync` with the source URL; a source without the field is
      unrestricted. Enforced **inside the importer transactions**, not only on the
      incoming manifest: `guardJobMutationTx` refuses a source-owned job whose stored
      or requested namespace is outside the allowlist (a narrowed source can neither
      move a job it used to own into its allowlist nor out of it), and `PruneMissing`
      with a source allowlist prunes only jobs whose namespace is in it, leaving the
      rest untouched and reported. Package tests: allowlist narrowing against existing
      jobs, an out-of-allowlist move attempt leaving the original job active, prune
      with a foreign job present. Git sync has no `test/` scenario today (coverage is
      `internal/jobdef/git/git_sync_test.go` against a temp repo); add one that
      points the integration server at a local bare repo with two manifests, one
      outside the allowlist — the wave must first confirm a `file://` source is
      reachable from the integration container, else extend the package test with
      the same two-manifest repo and record the gap.
      Files: `pkg/env/jobdef.go`, `pkg/env/jobdef_test.go`,
      `internal/jobdef/git/git_sync.go`, `internal/jobdef/git/git_sync_test.go`,
      `internal/jobdef/importer.go` (`guardJobMutationTx`, `PruneMissing`),
      `internal/jobdef/importer_test.go`, `internal/jobdef/runtime/config.go`,
      new `test/git_sync_test.go`.
      Depends on: B1.
- [ ] B4. Collection filtering in job-derived handlers: job list (replacing the
      scoped-alias branch with `authz.NamespaceFilter` layered on it), triggers list,
      incidents list, stats and stats summary, the events stream (with `run_id`: the
      existing rule; without: only allowed namespaces reach the subscriber), ingested
      events list, backfills. **History under a job filters by the run's persisted
      namespace**: `/v1/jobs/:id/runs`, the latest-run summary attached to job
      responses (`attachLatestRun`), run counts, `runs/diff`, blame and topology
      history authorise on the job's current namespace for access and then drop rows
      whose run namespace the caller lacks, so a moved job's old history stays with
      its old owner. Writes on history: the replay controller requires viewer on the
      baseline run's namespace and runner in the job's current namespace (A3 resolves
      both); retry (which reuses the historical row and launches the current job
      model) returns `409 namespace_moved` when the run's persisted namespace differs
      from the job's current one. Every handler calls `MarkFiltered`. Unit tests per handler with a
      two-namespace fixture; the end-to-end assertion (including the moved-job case)
      lives in A7.
      Files: `api/rest/controller/job/list.go`, `api/rest/controller/job/run/list.go`,
      `api/rest/controller/job/run/retry.go`, `api/rest/controller/replay/replay.go`,
      `api/rest/service/job/job.go`, `internal/run/store.go` (`List`),
      `api/rest/controller/trigger/`, `api/rest/controller/incident/`,
      `api/rest/controller/stats/`, `api/rest/controller/event/`,
      `api/rest/service/stats/`, `api/rest/service/event/`.
      Depends on: A3 + B1.
- [ ] B5. Aggregate redaction: `/v1/lineage/impact` and `/v1/contracts/graph` compute
      over the whole graph then redact at the boundary with a wire-level contract — a
      foreign node becomes `{id: <opaque per-response hash>, kind: "external"}` with
      alias, namespace, producing step and provenance removed; an edge touching a
      foreign node keeps only its endpoints and drops dataset names, producer /
      consumer / previous schemas, compatibility findings and provenance
      (`contract.Edge`, `lineage.ImpactNode`). Job-scoped keys keep today's outright
      denial. A response-level test greps the serialised body for any foreign alias,
      dataset name or schema text; the Console lineage graph renders the placeholder
      (F2 consumes the shape).
      Files: `api/rest/controller/lineage/`, `api/rest/service/lineage/`,
      `api/rest/controller/contract/`, `internal/lineage/impact.go`,
      `internal/contract/graph.go`, new `internal/authz/redact.go`.
      Depends on: A3.
- [ ] B6. Dataset tenancy via declared producers: a dataset's namespaces are those of
      the jobs referenced by `DatasetDeclaration.JobID` (a `DatasetDerivation` is a
      scheduling decision, frequently without a run, and is not an ownership record);
      `GET /v1/datasets` and `/datasets/holds` include a dataset when the caller holds
      viewer in any producer namespace; the single-dataset routes resolve the same set
      in A3's table (any for reads, every for hold release and for the manual
      `advance`); no declared producer ⇒ cluster-only; `DatasetDerivation` gains
      `ProducerJobID` + `JobNamespace` stamped by the freshness evaluator at decision
      time for every outcome including skips (most skips have no run), derivation
      history filters on it, metrics on the producing run's persisted namespace, and
      legacy rows with neither fall back to the declared producers' namespaces. Unit
      tests: ownership before the first run, skipped-decision visibility, two
      producers, legacy rows. Document "tenancy namespace vs dataset
      namespace" once in the guide.
      Files: `api/rest/controller/dataset/`, `api/rest/service/dataset/dataset.go`,
      `internal/freshness/evaluator.go`, `internal/freshness/` (producer lookup helper),
      `internal/models/dataset_declaration.go`, `internal/models/dataset_derivation.go`,
      `docs/identity-and-access.md`.
      Depends on: A3 + B1 + N-1.

### Stream C — Runtime isolation: Kubernetes target and scoped secrets

Turns a namespace into a real security boundary at execution time. The Kubernetes
engine resolves the pod namespace and default service account per request from the
policy, the resolved target travels with the dispatch request so workers never read
the policy file, Helm renders RBAC for each mapped namespace, and a scoped secret
resolver refuses references outside the namespace's allow-list at lint and at run time.

- [ ] C1. Kubernetes engine resolves the target per request: `KubeTarget{Namespace,
      ServiceAccountName}` on `EngineCreateRequest`, and `Namespace` on **every** other
      request type (`EngineGetRequest`, `EngineWaitRequest`, `EngineStopRequest`,
      `EngineLogsRequest`, `EngineListRequest`; Docker/Podman ignore it); the engine
      holds the core client instead of one `PodInterface`; `Create`, `Get`, `Wait`,
      `Logs`, `Stop` and the label-based reaper use the request's namespace (the
      reaper walks every namespace the policy maps plus the default); the task's
      persisted runtime id is namespace-qualified so recovery after a restart finds
      the pod without the policy; precedence step `serviceAccountName` > namespace
      default > none; `secret://k8s/…` and volume claims resolve in the mapped
      namespace; the resolved target is part of the execution descriptor and the cache
      key. Unit tests with the fake clientset, including wait/stop/logs in a mapped
      namespace.
      Files: `internal/atom/atom.go`, `internal/atom/kubernetes/engine.go`,
      `internal/atom/kubernetes/engine_test.go`, `internal/atom/docker/engine.go`,
      `internal/atom/podman/engine.go` (accept and ignore the field),
      `internal/jobdef/secret/kubernetes.go`, `internal/jobdef/runtime/spec.go`,
      `internal/cache/hash.go`, `internal/run/store.go` (descriptor capture, runtime id),
      `internal/job/job.go`, `internal/worker/runtime_executor.go` (call sites).
      Depends on: B1 + A4.
- [ ] C2. Carry an isolation context through dispatch, fail closed on old workers:
      the owner resolves `IsolationContext{KubeTarget, SecretRules, PolicyRevision}`
      from the run's namespace when building `DispatchRequest`; it travels through
      `HandleDispatch` → `InboundDispatch` → the worker's dispatch metadata
      (`internal/worker/worker.go`) to the runtime executor, which builds its scoped
      resolver (C4) from it — workers never read the policy file. Workers advertise
      `isolation_v1` in `GET /internal/capabilities` (`CapabilitiesResponse.Supports`);
      the owner fails closed whenever the run's isolation context is non-default — a
      mapped Kubernetes target **or** any secret rule, on any engine — and never
      dispatches such a task to a worker without it
      (`caesium_dispatch_incompatible_worker_total`; the run waits for a capable
      peer). Old owner → new worker carries no context and runs as today; the guide
      states the upgrade order (workers first). Local executor path resolves the
      context in-process. Distributed-lane scenario: a mapped-namespace task and a
      Docker task under secret rules both land on a capable worker; a worker without
      the capability is skipped, not used.
      Files: `internal/dispatch/dispatch.go`, `internal/dispatch/loop.go`,
      `internal/worker/worker.go`, `internal/worker/runtime_executor.go`,
      `internal/job/job.go`, `internal/metrics/metrics.go`, `test/` (distributed lane).
      Depends on: C1 + C4.
- [ ] C3. Helm multi-namespace RBAC and the kind-lane proof: `jobNamespaces: []` in
      values renders a `Role` + `RoleBinding` (pods, pods/log, secrets get,
      persistentvolumeclaims) per namespace; the kind CI values map one namespace and
      the k8s integration scenario asserts a job in it lands its pod, PVC and a
      `secret://k8s` read there with the mapped service account.
      Files: `helm/caesium/values.yaml`, `helm/caesium/templates/role.yaml`,
      `helm/caesium/templates/rolebinding.yaml`, `helm/caesium/ci/test-values-k8s.yaml`,
      `test/` (k8s lane scenario), `docs/kubernetes-deployment.md`.
      Depends on: C1 + C2.
- [ ] C4. Scoped secret resolution: `secret.ScopedResolver{inner, namespace, rules}`
      **canonicalises before matching** — the Kubernetes resolver accepts a
      namespace in the path and `?namespace=`, `?name=`, `?key=` overrides
      (`kubernetes.go` `parseReference`), so the scoped resolver computes the effective
      provider / namespace / name / key exactly as the inner resolver would and
      matches rules against `k8s/<effective-namespace>/<secret>` (an allow entry
      `k8s/<secret>` means the mapped namespace; any other effective namespace is
      denied); **every provider canonicalises with its own effective-target parser**
      — the env resolver honours `?name=` over the path (`env.go`) and Vault reads
      `?field=` (`vault.go`) — via a new `CanonicalTarget(ref)` on the `Resolver`
      interface (`env/<effective name>`, `vault/<mount>/<path>#<field>`,
      `k8s/<namespace>/<secret>`), never by path alone. Returns `ErrSecretDenied` (redacted to `provider/<first segment>/…`). Wired
      at the executor and worker `ResolveContainerSpecSecretsWithIdentities` call
      sites, the HTTP trigger's per-job secret, and server-side `lint.CheckSecrets`;
      the executor runs the allow-check (parse + canonicalise + match, no resolution)
      **before the cache lookup** so a now-denied reference is never served from
      cache; omitted rules ⇒ allow-all; a denied reference fails the task with a
      `config` incident class, not `transient_infra`. Git sync, registry auth and
      channel credentials keep the unscoped resolver as `system`. Unit tests include
      path-form and query-override escape attempts.
      Files: new `internal/jobdef/secret/scoped.go`, new
      `internal/jobdef/secret/scoped_test.go`, `internal/jobdef/secret/resolver.go`,
      `internal/jobdef/secret/kubernetes.go`, `internal/jobdef/secret/env.go`,
      `internal/jobdef/secret/vault.go`, `internal/job/job.go`,
      `internal/worker/runtime_executor.go` (cache path and resolution), `internal/trigger/http/http.go`, `internal/jobdef/lint/secret.go`,
      `internal/incident/classifier.go`.
      Depends on: A4 + B1.
- [ ] C5. Secret-scoping end to end: with the H-1 fixture's `marketing` allow-list, a
      manifest referencing `env/FINANCE_*` is rejected by `caesium job lint` (server)
      naming the redacted reference; applied with lint bypassed it fails the task with
      the `config` class visible in `caesium why --json`; the allowed reference resolves
      and the task succeeds.
      Files: new `test/secret_scope_test.go`, `test/testdata/policy/two-namespaces.yaml`.
      Depends on: C4 + H-1.

### Stream D — Run quota and fairness; namespaced shared objects

Two small isolation boundaries with disjoint files. The quota lives where a queued run
becomes running; shared objects get a namespace column with a `*` sentinel for
cluster-shared instances.

- [ ] D1. Per-namespace `quotas.maxConcurrentRuns` as **one transactional store
      primitive on every path that makes a run running**: `namespaceCapacityTx(tx,
      namespace)` (count running runs on the indexed `job_runs.namespace` + status
      inside the caller's transaction) called by `Store.admit`, by `readmitRetryTx`
      (manual and agent retries re-admit an existing row with admission disabled),
      by the partition-retry path, and by the quarantined-replay insert in
      `internal/replay/replay.go` (replays consume quota). At cap, per run kind:
      trigger-originated starts are enqueued into `run_queue` with `held_reason:
      namespace_quota` regardless of the job's queue policy and the dequeuer drops
      its "job must have a queue policy" prerequisite for quota holds so policy-free
      jobs drain; retries, partition retries and replays return `409 namespace_quota`
      with `Retry-After` (the agent `retry` action records the deferral); backfills
      (never enqueued — `ErrRunQueued` is a completed-skip in the backfill runner)
      wait and retry the date with backoff on `ErrNamespaceQuota`. `DrainOnce`
      iterates namespaces round-robin (in-memory cursor, deterministic order) then
      jobs and re-admits through the same gate; per-job concurrency still applies
      inside. Metrics `caesium_namespace_running_runs{namespace}` and
      `caesium_namespace_quota_deferrals_total{namespace}`. Unit tests: two
      namespaces, cap of one, concurrent direct triggers, a retry and a replay 409'd,
      a backfill waiting then completing with correct accounting and cancellable
      while held; the round-robin order. The sibling window scheduler must enter
      through `admit` too (noted in its plan).
      Files: `internal/run/store.go`, `internal/run/store_test.go`,
      `internal/replay/replay.go`, `internal/job/backfill.go`,
      `api/rest/controller/job/run/retry.go`, `api/rest/controller/replay/replay.go`,
      `internal/runqueue/dequeuer.go`, `internal/runqueue/dequeuer_test.go`,
      `internal/models/run_queue.go` (`HeldReason`), `internal/metrics/metrics.go`,
      `cmd/start/start.go` (policy holder into the run store and dequeuer).
      Depends on: B1 + A4.
- [ ] D2. Surface the hold: `GET /v1/jobs/:id/queue` rows carry the `run_queue`
      row's `held_reason: namespace_quota`; `caesium job queue` prints it; the Console queue
      page label is F4. Integration (no-auth server, H-1 settings): two policy-free
      jobs in a capped namespace, the second run is queued with the reason and
      released when the first completes; a retry while held gets 409; a two-date
      backfill completes with correct accounting.
      Files: `api/rest/controller/job/queue/`, `api/rest/service/job/`,
      `cmd/job/queue.go`, `test/job_queue_cli_test.go`.
      Depends on: D1.
- [ ] D3. Namespace column on shared objects: `NotificationChannel.Namespace`,
      `NotificationPolicy.Namespace` (text, not null, default `'default'`),
      `AgentProfile.Namespace` tightened to the same shape; `*` is the cluster-shared
      sentinel; JSON tags; create bodies accept `namespace`, **update bodies may not
      change it** (400 — moving is delete + create authorised on both sides, and
      creating a `*` object requires operator at `*`, enforced by A3/D4); lint rejects
      a job referencing a channel/policy/profile outside its own namespace or `*`.
      Pure model + lint change so A3 can resolve object namespaces.
      Files: `internal/models/notification.go`, `internal/models/agent_profile.go`,
      `api/rest/controller/notification/`, `api/rest/controller/agentprofile/`,
      `internal/jobdef/lint/`, `internal/jobdef/agent_profile_refs.go`.
      Depends on: B1.
- [ ] D4. Shared-object semantics through the surface: lists are collections
      (`*` objects always included); writes to `*` objects require operator at `*`;
      notification policies match runs in their own namespace only, shared policies
      everywhere. Integration: a marketing operator cannot edit a finance channel (403),
      can read a shared one, a shared policy fires for a marketing run and a finance
      policy does not.
      Files: `api/rest/controller/notification/`, `api/rest/controller/agentprofile/`,
      `internal/notification/watcher.go`, `test/notification_routes_test.go`,
      `test/agent_profile_test.go`.
      Depends on: D3 + A3.

### Stream E — Identity lifecycle: group refresh, user administration, CLI login

Keeps grants honest after login and gives admins and users the levers they expect.
Refresh handles on sessions, an asynchronous single-flight group refresher, the user and
session admin surface, user-bound API keys that resolve grants at use time, and the
server-brokered `caesium login` flow that works for every provider.

- [ ] E1. One refresh grant per user: `User.RefreshGrant` (AES-256-GCM under an
      HKDF-SHA256 key derived from `CAESIUM_AUTH_KEY_HASH_SECRET`, info
      `caesium/session-refresh/v1`, nonce per row), `User.RefreshGrantVersion` and
      `User.GroupsRefreshedAt`, shared by every credential type — sessions and keys
      carry nothing. Login replaces the grant (newest IdP session wins). The OIDC
      provider stores the refresh token and gains `RefreshGroups(grant) (groups,
      rotatedGrant)`; refresh ownership is a short **lease claimed by CAS before the
      IdP call** (`refresh_lease_owner`, `refresh_lease_until`, fenced on
      `RefreshGrantVersion`), success and failure effects are applied under the same
      fence, a login bumping the version discards any in-flight outcome, and an
      uncertain redemption marks the grant `refresh_uncertain` so a later
      `invalid_grant` clears the grant instead of revoking credentials (the E2
      staleness bound then forces re-login); LDAP stores the user DN and re-queries
      groups with the service account; SAML stores nothing. Crypto round-trip, lease
      and fence unit tests (two claimants, login racing a failing refresh, uncertain
      outcome) and provider tests including token rotation; the OpenLDAP fixture test
      covers the LDAP re-query.
      Files: `internal/models/user.go`, `internal/auth/users.go`,
      new `internal/auth/refresh_crypto.go`, `internal/auth/provider.go`
      (`GroupRefresher` interface; after A2's `Complete` rewrite),
      `internal/auth/oidc/provider.go`, `internal/auth/ldap/provider.go`,
      `internal/auth/ldap/openldap_integration_test.go`.
      Depends on: A2.
- [ ] E2. The async refresher for **both** credential types: on `SessionStore.Validate`
      and on `ValidateKey` for a user-bound key (E4), a user whose `GroupsRefreshedAt`
      is older than `CAESIUM_AUTH_GROUP_REFRESH_INTERVAL` (default `15m`) is queued to
      a bounded single-flight-per-user worker using the user's grant; success updates
      `users.groups` + `GroupsRefreshedAt` (+ the rotated grant via CAS), audits
      `auth.groups_refreshed` when the set changed; definitive failure revokes the
      user's sessions **and** user-bound keys (`auth.sessions_revoked`, reason
      `idp_refresh_denied`); transient failure retries next time. **Maximum staleness
      is unconditional**: past `CAESIUM_AUTH_GROUP_MAX_STALENESS` (default `24h`) for
      any reason (no grant, repeated transient failures) the first request attempts
      one synchronous refresh bounded by `CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT` (default
      `5s`) and otherwise every credential of that user is rejected 401 `stale_groups`
      until re-login; `CAESIUM_AUTH_SAML_SESSION_ABSOLUTE_TTL` (default `8h`);
      `caesium_auth_group_refresh_total{provider,outcome}`. Unit tests with a fake
      refresher and clock: browser and CLI-only users, prolonged transient failure
      hitting the bound, alternating browser/CLI refresh with rotation; the live
      assertion is G3.
      Files: new `internal/auth/refresher.go`, new `internal/auth/refresher_test.go`,
      `internal/auth/session.go`, `internal/auth/service.go`, `pkg/env/env.go`,
      `cmd/start/start.go`, `internal/metrics/metrics.go`, `internal/auth/audit.go`.
      Depends on: E1 + E4.
- [ ] E3. User and session administration: `GET /v1/auth/users` (filters, paging),
      `GET /v1/auth/users/:id` (with sessions; user-bound keys join in E4),
      `POST …/disable` (revokes sessions, audits `user.disable`; E4 extends the
      cascade to user-bound keys), `POST …/enable`, `POST …/sessions/revoke`,
      `GET /v1/auth/sessions?user_id=`; CLI `caesium auth user
      list|get|disable|enable|logout` with `--json` on stdout. Cluster-admin routes
      (class `cluster`). Unit tests on the store; the end-to-end scenario needs an
      SSO user and is G3.
      Files: `internal/auth/users.go`, `internal/auth/session.go`,
      new `api/rest/controller/auth/users.go`, new `api/rest/controller/auth/sessions.go`,
      `api/rest/bind/bind.go`, new `cmd/auth/user.go`, `internal/auth/audit.go`,
      `internal/auth/rbac.go` (entries).
      Depends on: A3.
- [ ] E4. User-bound API keys: `APIKey.UserID`; `csk_user_` prefix; on validation the
      service loads the user (disabled ⇒ invalid) and resolves grants from the policy;
      user-bound keys are listed under the user in `GET /v1/auth/users/:id` and
      revoked by the E3 disable cascade;
      `Principal.Kind = user`, `Subject` = email; listed under the user; revoked by
      disable, by admin revoke, and by `caesium logout`; `CAESIUM_AUTH_CLI_KEY_TTL`
      (default `12h`, max `7d`). Unit tests on validation and the disable cascade.
      Files: `internal/models/api_key.go`, `internal/auth/service.go`,
      `internal/auth/service_test.go`, `internal/auth/principal.go`, `pkg/env/env.go`,
      `api/rest/controller/auth/users.go`.
      Depends on: A5 + E3.
- [ ] E5. `caesium login`, `logout`, `whoami`: server routes `GET /auth/cli/start`
      (public; stores a login transaction — `state`, loopback port, PKCE-style
      `challenge`, hostname hint, 5m expiry), `GET /auth/cli/finish` (identity class;
      **mints nothing** — renders a session-bound confirmation page),
      `POST /auth/cli/authorize` (identity class, CSRF-protected cookie-session POST;
      consumes the transaction atomically, mints the user-bound key, stores a
      single-use 60s code, audits `auth.cli_login`, redirects to the loopback callback
      or shows the code for `--no-browser`), `POST /auth/cli/exchange {code,
      code_verifier}` (public, single use, IP rate-limited, verifies
      `S256(verifier) == challenge`); the CLI generates the verifier, starts the
      loopback listener, opens the browser, verifies `state`, exchanges the code,
      and stores `{server, key (plaintext), key_id, key_prefix, expires_at, subject}` at
      `$XDG_CONFIG_HOME/caesium/credentials.yaml` (0600, dir 0700) — the plaintext key
      is the bearer later processes present (`ValidateKey` hashes the full token);
      `cmd/cliutil/auth.go` becomes the single credential resolver (`--api-key` >
      `CAESIUM_API_KEY` > stored) and the commands that resolve the env var
      themselves (`cmd/why`, `cmd/blame`, `cmd/run/diff`, `cmd/auth`) move to it;
      `POST /auth/logout` (identity class) revokes **the credential that
      authenticated the request** — the cookie session today, and a user-bound bearer
      key from now on — so `caesium logout` self-revokes without an admin grant and
      deletes the local entry; `caesium whoami [--json]` prints principal and grants.
      Integration in the api-key lane: exchange error paths (unknown code, reuse,
      expired, wrong verifier), a `finish` GET with an unknown or consumed state
      minting nothing, an `authorize` POST without CSRF refused, the credential store
      precedence, and login → whoami in a **separate process with the env var
      cleared**; the full browser-less flow is G4.
      Files: new `api/rest/controller/auth/cli.go`, `api/rest/controller/auth/sso.go`
      (`Logout`), `api/api.go`, `api/middleware/auth.go` (public paths),
      new `cmd/login/login.go`,
      new `cmd/logout/logout.go`, new `cmd/whoami/whoami.go`, `cmd/cliutil/auth.go`,
      `cmd/execute.go`, `cmd/why/why.go`, `cmd/blame/blame.go`, `cmd/run/diff.go`,
      `cmd/auth/auth.go`, new `test/cli_login_test.go`.
      Depends on: E4.

### Stream F — Console: grants, switcher, chips, quota label, Access page

The UI is the corporate user's window into all of this; it only needs to read the new
shapes. Owned separately from the Go streams so it can run on a lighter model.

- [ ] F1. Grants-aware principal: `PrincipalState` gains `grants`, `namespaces`,
      `clusterRole`, `roleIn(ns)`; gating in `RunDetailPage` (replay), `HoldPanel`
      (release) and the job pages uses `roleIn(job.namespace)`; the login page shows
      the `no_binding` denial reason. Vitest on `roleIn` and the whoami parser.
      Files: `ui/src/lib/auth.ts`, `ui/src/features/auth/`, `ui/src/features/jobs/RunDetailPage.tsx`,
      `ui/src/features/datasets/HoldPanel.tsx`, `ui/src/lib/api.ts`.
      Depends on: A5.
- [ ] F2. Namespace switcher and chips: a header switcher fed by `namespaces` (hidden
      with one), selection persisted in `localStorage` as a view filter, list pages send
      `?namespace=`, job list and job detail show a namespace chip, the lineage graph
      renders B5's `external` placeholder. Playwright in the existing auth project:
      switch namespace, list narrows; a deep link to a job outside the grants renders
      `InsufficientAccess`.
      Files: `ui/src/components/layout/Header.tsx`, new
      `ui/src/features/auth/NamespaceSwitcher.tsx`, `ui/src/features/jobs/`,
      `ui/src/features/jobs/LineageGraph.tsx`, `ui/src/lib/api.ts`, `ui/e2e/`.
      Depends on: F1 + B4 + B5.
- [ ] F3. Access page at `/access`: my grants; declared namespaces and their settings
      (cluster viewer+); users list with disable / enable / log-out-everywhere (cluster
      admin); policy revision hash and last reload result. Sidebar entry. Playwright
      coverage lands with G2.
      Files: new `ui/src/features/access/AccessPage.tsx`, `ui/src/router.tsx`,
      `ui/src/components/layout/Sidebar.tsx`, `ui/src/lib/api.ts`.
      Depends on: F1 + A4 + E3.
- [ ] F4. Queue page shows "held by namespace quota" from `held_reason`.
      Files: `ui/src/features/jobs/` (queue view), `ui/src/lib/api.ts`.
      Depends on: D2 + F1.

### Stream G — Keycloak: fixture, CI lane, scenarios, guidance

Gives SSO its first real end-to-end coverage and makes Keycloak a documented,
proven issuer. Every SSO-only behaviour from Streams A, C and E (SSO-admin key mint,
group refresh, user disable, CLI login) is asserted here against a live Keycloak.

- [ ] G1. Realm fixture and lane: `test/testdata/keycloak/caesium-realm.json` (client
      `caesium`, group-membership mapper on `groups` with full path off, users
      `admin@example.com` ∈ `caesium-admins`, `bob@example.com` ∈ `marketing-eng`,
      `carol@example.com` in no group), `just integration-test-keycloak` starting
      `quay.io/keycloak/keycloak --import-realm` and a Caesium server with OIDC +
      `CAESIUM_AUTH_POLICY_FILE=test/testdata/policy/two-namespaces.yaml` + two jobs
      applied; CI jobs `build-and-integration-test-keycloak` and `ui-e2e-keycloak`
      added to `ci-ok`'s `needs` **and** to `scripts/ci-ok.py`'s `SELECTORS` map (a
      required job missing from the map fails `ci-ok` even when green) with path
      selectors and `scripts/test_ci.py` cases; `ci-ok` gates `v*` publication rather
      than merge today (`docs/ci.md` §1) and the lane inherits exactly that.
      Files: new `test/testdata/keycloak/caesium-realm.json`, `justfile`,
      `scripts/integration-test.sh`, `.github/workflows/ci.yml`, `scripts/ci-ok.py`,
      `scripts/test_ci.py`, `docs/ci.md`.
      Depends on: A4 + A5 + H-1.
- [ ] G2. Playwright project `keycloak`: admin logs in through Keycloak's real login
      page and sees both namespaces and the switcher; bob sees only `marketing`, the
      finance job is absent and its deep link renders `InsufficientAccess`; carol is
      denied with `no_binding`; the Access page shows grants and, for admin, the users
      list.
      Files: `ui/playwright.config.ts`, new `ui/e2e/keycloak.spec.ts`,
      `ui/e2e/helpers/auth.ts`.
      Depends on: G1 + F2 + F3.
- [ ] G3. Go scenarios on the lane (`CAESIUM_KEYCLOAK_LANE=true`): a cookie-jar client
      drives the OIDC code flow through Keycloak's HTML login form; the SSO
      allow/deny matrix; an SSO admin mints a namespaced key (A5's fix); group refresh
      (move bob via Keycloak's admin REST API, advance past the interval, grants change
      without re-login); `caesium auth user disable` kills bob's session within one
      request; `caesium auth user list --json` clean on stdout.
      Files: new `test/keycloak_lane_test.go`, new `test/keycloak_helpers_test.go`,
      `test/auth_lane_test.go` (lane detection).
      Depends on: G1 + E2 + E3.
- [ ] G4. CLI login on the lane: `caesium login --no-browser` prints the start URL, the
      test completes the SSO flow with the cookie-jar client, pastes the code with
      `caesium login --code`, `caesium whoami --json` shows bob's grants, disabling bob
      makes the next call 401, `caesium logout` cleans the credential file.
      Files: `test/keycloak_lane_test.go`.
      Depends on: G1 + E5.
- [ ] G5. Keycloak operator guidance in the guide (client, mapper, scopes, refresh
      tokens, issuer URL discovery) and the lane in `docs/ci.md`.
      Files: `docs/identity-and-access.md`, `docs/ci.md`.
      Depends on: N-1 + G1.

## Harness Strengthening

- [ ] H-1. Policy fixture on **both** integration servers: `test/testdata/policy/two-namespaces.yaml`
      (`default`, `marketing` with secret rules and a quota of one, `finance`;
      bindings for the three lane groups) mounted with `CAESIUM_AUTH_POLICY_FILE` into
      `just integration-up` (no auth mode — exercises namespace *settings*: secret
      rules, quotas, Kubernetes mapping; C5, D2) and into `just integration-up-agent`
      (the api-key auth lane whose key helpers `createAPIKeyCLI` / `requireAuthLane`
      already exist — exercises *bindings*, namespaced keys and the matrix; A7, A8,
      D4, E5), with the runner env propagated in `integration-test-agent`; auth
      scenarios **fail rather than skip** when the lane is up but the policy file is
      absent; helpers to mint namespaced keys per role (`s.mintNamespacedKey(role,
      ns…)`).
      Files: new `test/testdata/policy/two-namespaces.yaml`, `justfile`
      (`integration-up`, `integration-up-agent`, `integration-test-agent`),
      `test/auth_lane_test.go`, `test/auth_keys_test.go` (helpers),
      `test/integration_test.go` (suite wiring).
      Depends on: A4.
- [ ] H-2. `--json` stdout discipline for every new CLI verb (`auth policy`, `auth user`,
      `login`/`whoami`, `job queue` held reason): each scenario captures stdout with
      `runCLIStdout` and parses it; a shared assertion helper fails on any non-JSON
      byte. Lands with the first verb (A4) and is reused after.
      Files: new `test/cli_json_helpers_test.go` (`runCLIStdout` lives in
      `test/data_plane_e2e_test.go`).
      Depends on: A4.

## Navigational / Organizational Improvements

- [ ] N-1. Rename `docs/sso-authentication.md` → `docs/identity-and-access.md` (`git mv`)
      and grow it into the single operator guide with the section order from spec §16:
      concepts (namespace, grant, role); providers (OIDC, SAML, LDAP/AD; Keycloak
      section arrives with G5); policy file; namespaces & isolation; API keys & CLI
      login; users & sessions; audit & metrics; security checks; migration from the env
      mapping. Update every inbound link (`docs/README.md`, `docs/roadmap.md`,
      `docs/sovereignty.md`, `README.md`, `helm/caesium/README.md` if any) and the
      README index line. Ships with A4 so the policy file is documented the moment it
      exists.
      Files: `docs/sso-authentication.md` → `docs/identity-and-access.md`,
      `docs/README.md`, `docs/roadmap.md`, `docs/sovereignty.md`, `README.md`.
      Depends on: A4.
- [ ] N-2. Close-out: `docs/sovereignty.md` gains a **Multi-tenancy** row; roadmap §3.1
      flips to Shipped and §3.3's "read-only vs operator role distinction" line is
      marked shipped; `docs/README.md` moves this plan to the completed list; this
      plan's Progress dashboard matches merged PRs.
      Files: `docs/sovereignty.md`, `docs/roadmap.md`, `docs/README.md`,
      `docs/exec-plans/active/identity-and-access.md`.
      Depends on: every stream's last item.

## Sequencing & Dependencies

**Cross-stream order**

- Stream B1 is the first leaf everything namespaced needs: A3, B2–B6, C1, C4, D1, D3
  all depend on it. Land B1 and A1 in the first wave.
- Stream A3 (the classified middleware) depends on A2 + B1 + D3 and is the gate for
  B4, B5, B6, D4, E3, F1 (via A5). Nothing that enforces namespaces ships before A3.
- Stream A4 (the policy holder, after A2) gates B2, C1, C4, D1, H-1, N-1 and the
  Keycloak lane.
- Stream C depends on Stream B for the column and on A4 for the settings; C3 (Helm)
  is the only item that touches `helm/`.
- Stream D's quota item (D1) is independent of Stream C and can run alongside it;
  D3 is a leaf-after-B1 that A3 needs, so D3 lands in the same wave as A2.
- Stream E: E1 after A2 (both touch `internal/auth/provider.go`); E3 after A3;
  E4 after A5 + E3; E2 after E1 + E4 (the refresher needs the user-bound key
  validation path); E5 after E4. E is the longest chain: E1 in W2, E3 W3, E4 W4,
  E2 and E5 W5.
- Stream A: A4 depends on A2 (the `policy.Source` wiring in `cmd/start/start.go`),
  so A4 lands in W2, and H-1 / H-2 / N-1 / D1 / C4 follow in W3.
- Stream F depends on A5 for the whoami shape (F1), on B4/B5 for the switcher (F2),
  on A4 + E3 for the Access page (F3), on D2 for the quota label (F4).
- Stream G depends on A4 + A5 + H-1 (G1), then on F2/F3 (G2), E2/E3 (G3), E5 (G4),
  N-1 (G5). It is the last stream to close.
- Streams A, B, C, D, E, F, G are otherwise independent and parallelise per wave as
  the dependency edges allow.

**Within-stream order**

- A: A1 → A2 → (A3 ∥ A4) → A5 → A6 → A7; A8 after A3 + B1.
- B: B1 → (B2, B3 ∥ B4, B5, B6 after A3).
- C: C1 → C2 → C3 with C2 also needing C4; C4 → C5 (C4 independent of C1).
- D: D1 → D2; D3 → D4.
- E: E1 (after A2) → …; E3 (after A3) → E4 → (E2 ∥ E5); E2 also needs E1.
- F: F1 → (F2 ∥ F3 ∥ F4).
- G: G1 → (G2 ∥ G3 ∥ G4 ∥ G5).

**Suggested waves** (the orchestrator decides; this is the shape the edges allow)

| Wave | Items |
|------|-------|
| W1 | A1, A2, B1, D3 |
| W2 | A3, A4, B3, E1 |
| W3 | A5, A8, B2, B5, C4, D1, E3, H-1, H-2, N-1 |
| W4 | B6, C1, C5, D2, D4, E4, F1, G1 |
| W5 | B4, C2, E2, E5, F3, F4, G5 |
| W6 | A6, A7, C3, F2, G3, G4 |
| W7 | G2 |
| W8 | N-2 |

The table was regenerated from the `Depends on:` edges and the file rules below:
`internal/run/store.go` B1 → D1 → C1 → B4 in W1 / W3 / W4 / W5; `cmd/start/start.go`
A2 → A4 → D1 → E2 in W1 / W2 / W3 / W5; `pkg/env/env.go` A4 → E4 → E2 → A6 in W2 /
W4 / W5 / W6; `internal/auth/provider.go` A2 W1 → E1 W2; `api/rest/bind/bind.go`
A4 W2 → E3 W3; `internal/models/api_key.go` A2 W1 → E4 W4; `internal/job/job.go` +
`internal/worker/runtime_executor.go` B1 → C4 → C1 → C2 in W1 / W3 / W4 / W5;
`api/middleware/auth.go` A2 → A3 → E5 in W1 / W2 / W5. The only same-wave overlaps
left are `internal/auth/rbac.go` (A3 + A4 in W2, merge order A3 first) and the
additive `internal/metrics/metrics.go` (C2 + E2 in W5, appended collectors — rebase).
Regenerate the table from the edges if items move.

**Cross-stream file conflicts**

- `pkg/env/env.go`: A4 (policy file + reload interval), E4 (CLI key TTL), E2 (refresh
  interval, staleness, SAML TTL), A6 (deprecations). Additive fields; sequence
  A4 → E4 → E2 → A6, never two in the same wave.
- `cmd/start/start.go`: A2 (the SSO block: `policy.Source` wiring, replacing the
  `RoleMapper`/`SSOService` construction), A4 (file loader behind the same interface,
  reload goroutine), D1 (run store + dequeuer config), E2 (refresher). Sequence
  A2 → A4 → D1 → E2, one per wave.
- `api/rest/bind/bind.go`: A4 (policy routes), E3 (users/sessions). Line appends;
  different waves preferred.
- `api/api.go`: only E5 touches `Start()`/route registration for `/auth/cli/*`.
- `api/middleware/auth.go` + `auth_scope.go`: A2, A3, E5 (public paths). A2 → A3 → E5.
- `internal/auth/service.go`: A5 (namespaced keys) → E4 (user-bound keys). Sequential.
- `internal/auth/rbac.go`: A3 reshapes the table (classes) and A4 adds two entries;
  both sit in W2, so the orchestrator merges A3 first and A4 rebases its entries onto
  the classified shape. E3 adds classified entries after A3.
- `internal/auth/audit.go`: A3, A4, A6, E2, E3 add action constants. Additive; rebase.
- `internal/metrics/metrics.go`: A4, D1, E2 add collectors (two edit sites). Additive.
- `internal/models/api_key.go`: A2 (`Namespaces`) → E4 (`UserID`). Sequential.
- `internal/models/user.go`: E1 (`RefreshGrant*`, `GroupsRefreshedAt`) only.
- `internal/models/models.go`: **no new tables** in this plan; columns only.
- `internal/run/store.go`: B1 (namespace on run creation), D1 (`namespaceCapacityTx`
  in `admit` / `readmitRetryTx`), C1 (descriptor / runtime id), B4 (`List` filter).
  Sequence B1 → D1 → C1 → B4; never two in one wave.
- `internal/replay/replay.go`: B1 (namespace stamp) → D1 (quota on the insert).
- `internal/job/job.go` and `internal/worker/runtime_executor.go`: B1 (hash input)
  → C4 (allow-check before cache, resolution) → C1 (engine call sites) → C2
  (isolation context). One per wave.
- `api/rest/controller/job/run/retry.go`: D1 (409 quota) → B4 (409 namespace_moved).
- `internal/auth/provider.go`: A2 (`SSOService.Complete`) → E1 (`GroupRefresher`).
- `api/rest/service/job/job.go`: B1 (`Create` namespace) → B4 (`attachLatestRun`).
- `internal/incident/allowlist.go` + `session.go`: A8 here **and** the arc plans
  (`data-circuit-breaker` F). Additive; rebase, different waves preferred.
  `internal/incident/store.go`: B1 (`DedupeKey`) only in this plan.
- `internal/worker/worker.go`: C2 only. `scripts/ci-ok.py`, `scripts/test_ci.py`: G1 only.
- `pkg/jobdef/definition.go`: B1 only.
- `internal/cache/hash.go`: B1 (namespace in the key + test) → C1 (hash the resolved
  Kubernetes target). Sequential.
- `internal/jobdef/lint/`: B2, C4, D3. Different files where possible; B2 → D3 → C4.
- `internal/job/job.go` and `internal/worker/runtime_executor.go`: C2 and C4 both edit
  the secret-resolution call site region. Sequence C4 → C2.
- `internal/runqueue/dequeuer.go`: D1 here **and** `window-scheduling.md` (sibling,
  not started). Whichever plan starts first owns the file for that wave; the other
  rebases. The parked fairness note in window-scheduling points here.
- `internal/atom/kubernetes/engine.go`: C1 here **and** `resource-right-sizing.md`
  Stream A (OOM evidence on inspect). Disjoint functions (`Create`/namespace plumbing
  vs. terminated-state inspection) but the same file; do not run C1 and right-sizing A2
  in the same wave.
- `internal/incident/classifier.go`: C4 (`config` class) and the arc plans
  (`data-circuit-breaker` F, `resource-right-sizing` A). Additive case; rebase.
- `justfile`: H-1 (policy fixture in `integration-up`), G1 (Keycloak lane). H-1 first.
- `.github/workflows/ci.yml`: G1 only.
- `ui/src/lib/api.ts`, `ui/src/router.tsx`, `ui/src/components/layout/Sidebar.tsx`:
  Stream F only (F1 → F2/F3/F4).
- `docs/identity-and-access.md`: N-1 creates it; A6, B6, G5 add sections. N-1 first.
- `docs/README.md`, `docs/roadmap.md`: N-1 and N-2 only (this PR added the pointers).
- `go.sum`: E1 may promote `golang.org/x/crypto` from indirect to direct for HKDF
  (Go ≥ 1.24 has `crypto/hkdf` in the standard library; prefer it and avoid the
  dependency change). If a dep change happens, resolve with `go mod tidy`.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:

- `ui/**` (Stream F, G2): `just ui-lint && just ui-test && just ui-e2e`, plus the
  `auth` and (once G1 lands) `keycloak` Playwright projects.
- `helm/**` / Kubernetes engine (C1–C3): `just helm-lint && just helm-template`, and
  the kind lane in CI.
- Job-schema change (B1): `caesium job lint --path docs/examples/` and
  `TestGeneratedSchemaReferenceIsCurrent` green (regenerate, never hand-edit).
- New metric (A4, D1, E2): asserted via `internal/metrics/testutil` and present in
  `Register()`.
- Any change under `internal/` : run `gofmt -l` on the changed directories (the root
  `just lint` formats the root package only).
- Every new CLI verb or REST route: a `test/` scenario through the real surface, with
  `--json` captured by `runCLIStdout`.
- This plan's checkbox ticked, per-stream `## Progress` bullet appended for the active
  wave, and any cross-linked doc refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — authorization core** is live: `TestNamespaceAllowDenyMatrix` is green in
   CI on the auth lane against the two-namespace policy fixture (moved-job history,
   `/metrics`, diff baseline and agent allow-list confinement included); an unclassified route fails the build;
   an SSO admin mints a namespaced key (asserted on the Keycloak lane); policy reload
   (valid and invalid) is covered by `test/auth_policy_test.go`; the legacy static keys
   log a deprecation and audit as `legacy:*`.
2. **Stream B — namespace on jobs**: `metadata.namespace` is in the generated schema
   reference and one example; apply/lint/move/prune scenarios are green; git-sync
   allowlist scenario green; every collection route filters (matrix) and lineage /
   contracts collapse to `external` for a confined principal.
3. **Stream C — runtime isolation**: the kind lane proves a mapped namespace (pod, PVC,
   `secret://k8s`, service account, and wait/stop/logs after a restart); the
   distributed lane proves a worker without `kube_target` is skipped; `test/secret_scope_test.go` proves lint-time and
   run-time denial with the `config` incident class; Helm renders per-namespace RBAC.
4. **Stream D — quota and shared objects**: the queue scenario shows `namespace_quota`
   held at admission for a job with no queue policy, and release; both quota metrics registered; shared-object 403/read/match scenarios
   green.
5. **Stream E — identity lifecycle**: group refresh changes grants without re-login for
   a browser user and for a CLI-only user, token rotation survives alternating
   browser/CLI refreshes, prolonged IdP failure ends in `stale_groups`, and user
   disable kills a session within one request (Keycloak lane); user-bound keys die on
   disable; the CLI mint requires the confirmation POST; `caesium login --no-browser` → `whoami` → `logout` round-trips on the
   lane; exchange error paths covered in the api-key lane.
6. **Stream F — Console**: the `keycloak` Playwright project proves the switcher, chips,
   `InsufficientAccess` on a foreign deep link, and the Access page; `ui-e2e-auth`
   stays green.
7. **Stream G — Keycloak**: `build-and-integration-test-keycloak` and `ui-e2e-keycloak`
   are required by `ci-ok`; the guide has a Keycloak section; `docs/ci.md` lists the
   lane.
8. **Harness and docs**: the integration server runs with the policy fixture; every new
   verb's `--json` is stdout-clean; `docs/identity-and-access.md` is the single guide
   and `docs/sso-authentication.md` no longer exists; the SSO spec is archived as
   superseded.
9. `docs/roadmap.md` (§3.1 → Shipped, §3.3 role line → shipped), `docs/sovereignty.md`
   (Multi-tenancy row), `docs/README.md` and `window-scheduling.md`'s parked note reflect
   every shipped stream; this plan's per-stream Progress entries match merged PRs.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line
   is satisfied (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every
   PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the
   active wave subsection in `## Progress` (or open a new wave
   subsection if none exists yet), and update any cross-linked design
   doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (<plan-slug> <wave>-<stream>)` —
   e.g. `Add the access policy package (identity-and-access W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- `docs/superpowers/specs/2026-09-16-identity-and-access-design.md` — the design of
  record (source of truth for this plan).
- `docs/archive/design-sso-authentication.md` — the shipped SSO design this spec
  supersedes; its provider, session and CSRF decisions carry forward.
- `docs/sso-authentication.md` → `docs/identity-and-access.md` (N-1) — the operator
  guide.
- `docs/roadmap.md` §3.1 Multi-Tenancy & Namespace Isolation, §3.3 item 5.
- `docs/sovereignty.md` — the free-vs-paid comparison this plan adds a row to.
- `docs/exec-plans/active/window-scheduling.md` — parked per-namespace fairness
  (owned here by Stream D); shared `internal/runqueue/dequeuer.go`.
- `docs/exec-plans/active/resource-right-sizing.md` — shares
  `internal/atom/kubernetes/engine.go` with C1.
- `docs/exec-plans/active/closed-loop-arc.md` — this plan is outside the arc; its
  file-conflict rules for `internal/incident/` apply to C4.
- `pkg/jobdef/definition.go` — the manifest contract (`metadata.namespace`, B1).
- Issue #395 — per-namespace fairness; Stream D ships the v1 slice.
