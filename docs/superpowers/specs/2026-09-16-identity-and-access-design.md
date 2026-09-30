# Design: Identity and Access — Namespaces, Grants, Policy-as-Code, Corporate SSO

**Status:** Approved design (brainstormed 2026-09-16); exec plan `docs/exec-plans/active/identity-and-access.md`
**Date:** 2026-09-16
**Author:** Christopher Ryan
**Topic:** Multi-tenancy (namespaces), namespace-scoped RBAC, policy-as-code, IdP group freshness, Keycloak, CLI SSO login
**Supersedes:** `docs/archive/design-sso-authentication.md` (the shipped SSO design; its decisions carry forward unchanged unless restated here)

---

## 1. Summary

Make Caesium usable in a corporate environment where identity comes from Active
Directory, LDAP, SAML or an OIDC issuer such as Keycloak, and where several teams
share one cluster without seeing or touching each other's jobs, runs, secrets, or
compute.

The change is one new primitive and one reshaped check:

- **Namespace** — a flat tenancy unit declared on the job manifest
  (`metadata.namespace`, default `default`) and carried by every job-owned resource.
- **Grant** — a role in a namespace (`operator @ marketing`), with `*` meaning
  cluster-wide. A principal carries a grants map instead of one global role. The
  four roles (`viewer < runner < operator < admin`) are unchanged.

Grants come from a **policy-as-code file** that binds IdP groups (or user emails) to
roles in namespaces and declares each namespace's isolation settings (Kubernetes
target, secret allow-list, run quota). The existing auth middleware chokepoint
resolves the target namespace per route and decides; collection handlers filter by
the principal's allowed namespaces. Sessions refresh IdP groups on a bounded
interval, admins can disable users and revoke sessions, Keycloak gets a real
end-to-end CI lane, and `caesium login` gives the CLI an SSO-backed, user-attributed
credential.

Everything is additive and default-preserving: with no policy file the server runs
exactly as today, in one `default` namespace.

## 2. Motivation and current state (verified at master `74e2e731`, 2026-09-16)

Authentication is shipped and native: OIDC, SAML and LDAP providers in
`internal/auth/{oidc,saml,ldap}/`, a shared `ExternalIdentity` (issuer, subject,
email, display name, groups), just-in-time user provisioning keyed on
(issuer, subject) in `internal/auth/users.go`, dqlite-backed sessions with CSRF,
rate limiting, and a structured audit log. Operator guide: `docs/sso-authentication.md`.

Authorization is not corporate-ready:

| Gap | Where it lives today |
| --- | --- |
| No tenancy primitive. Jobs, runs, triggers, datasets share one flat space; one Kubernetes namespace (`CAESIUM_KUBERNETES_NAMESPACE`); secrets are global. Roadmap §3.1 calls it "single-tenant, P3". | `internal/models/job.go` (no namespace column), `internal/atom/kubernetes/engine.go:111,155` (namespace fixed at construction), `internal/jobdef/secret/` (no per-job rules) |
| Roles are a global ladder keyed per HTTP route. | `internal/auth/rbac.go` `endpointPolicy map[string]models.Role` (107 entries), `api/auth_rbac_policy_completeness_test.go` |
| SSO users are always unscoped; group mapping yields a role only. | `internal/auth/principal.go` `PrincipalFromUser` (nil `Scope`), `internal/auth/rolemap.go` |
| The only per-resource control is a job-alias allow-list on API keys. | `models.KeyScope.Jobs`, `api/middleware/auth_scope.go` |
| SSO admins cannot manage API keys: the key endpoints require an API-key caller and 401 a cookie session. | `api/rest/controller/auth/key_{create,revoke,rotate}.go` (`middleware.GetAuthKey`) |
| No user lifecycle: `users.disabled_at` has no writer; `SessionStore.RevokeAllForUser` has no caller; no list/disable endpoints. Role is resolved only at login and frozen for the session (up to 24h). | `internal/auth/users.go`, `internal/auth/session.go` |
| Two static shared secrets bypass the principal model. | `CAESIUM_MANUAL_TRIGGER_API_KEY`, `CAESIUM_EVENT_INGEST_API_KEY` (`api/rest/controller/trigger/put.go`, `event/ingest.go`) |
| Zero SSO end-to-end coverage. The auth-enabled CI lanes exercise API keys only; OIDC/SAML have unit tests with fakes; the OpenLDAP fixture is opt-in and not run in CI. | `.github/workflows/ci.yml` `ui-e2e-auth`, `build-and-integration-test-agent-auth` |
| CLI is API-key only. | `cmd/cliutil/auth.go` (`CAESIUM_API_KEY`) |

Airflow 3.3 ships experimental multi-team (AIP-67) and a Keycloak auth manager that
delegates every decision to Keycloak. Airflow's model bolts teams on as separate
executor/config sets and requires the Keycloak client to be updated per team. Caesium
can do better by making the tenant a first-class, GitOps-reviewed field on the
manifest and keeping one declarative policy that works identically for AD-via-LDAP,
SAML, and any OIDC issuer.

## 3. Goals / non-goals

**Goals**

- A namespace primitive on jobs and every job-owned resource, enforced at the API,
  in the Kubernetes engine, in secret resolution, and at run admission.
- Namespace-scoped grants for SSO users and API keys from one policy file, evaluated
  per request so policy changes apply immediately.
- IdP group changes take effect within a bounded interval without re-login.
- Cluster-admin user lifecycle: list, disable, enable, revoke sessions.
- SSO admins are first-class: every admin surface accepts any principal.
- Keycloak documented as a first-class issuer and exercised end-to-end in CI.
- `caesium login`: an SSO-backed, user-attributed CLI credential for every provider.
- Docs consolidated into one identity-and-access guide; no new top-level docs.

**Non-goals (v1)**

- Hierarchical tenants (org/team/project). Namespaces are flat.
- A resource × action permission matrix or user-defined roles. The four roles stay.
- DB-managed or admin-UI role editing. Policy is a file, reviewed in Git.
- Keycloak authorization services as a policy decision point (possible later provider).
- Per-namespace CPU/memory quotas. v1 has one knob: max concurrent runs.
- Cross-namespace trigger references beyond what alias-addressed trigger chains
  already allow (aliases stay globally unique; see §5.2).
- Renaming the dataset `namespace` field. See §5.1.

## 4. Decisions (brainstorm record)

| # | Decision | Chosen | Alternatives considered |
| --- | --- | --- | --- |
| D1 | Tenancy unit | Flat `metadata.namespace` on the manifest, declared in the policy file | Label selectors (fuzzy isolation, no single key for K8s/secrets/quotas); hierarchical (too large for v1) |
| D2 | Policy source | Policy-as-code YAML file referenced by env, reloadable | DB-managed via API/UI; file-plus-DB-cache |
| D3 | Granularity | Role per namespace plus `*` cluster-wide; job-alias allow-list retained as further narrowing | Resource × action matrix; role-per-namespace modelled as verbs internally |
| D4 | Isolation depth (v1) | Jobs/runs/triggers/backfills/datasets always; **plus** secrets, Kubernetes namespace + service account, run quota + fairness, namespaced notification channels/policies/agent profiles | Fewer boundaries |
| D5 | Keycloak | OIDC `groups` claim + Caesium policy; Keycloak container in CI driving real logins | Keycloak as PDP; groups-only with no CI lane |
| D6 | Freshness | Policy evaluated per request (immediate); stored groups refreshed from the IdP on an interval (default 15m), async single-flight; SAML gets a shorter absolute TTL | Login-time only + admin revoke; IdP check per request |
| D7 | CLI SSO login | In scope, last wave, server-brokered browser/loopback flow minting a user-bound key | Defer |
| D8 | Docs | Rename `docs/sso-authentication.md` → `docs/identity-and-access.md` and grow it; new spec supersedes the SSO spec (archived with banner); roadmap §3.1 becomes a pointer | Grow in place without rename; one doc per topic |
| D9 | Enforcement | Hybrid: middleware decides per route class and injects allowed namespaces; collection handlers apply one shared filter; completeness test on route classes | Middleware only (aggregates leak or are unusable); data-layer scoping (every store, scheduler bypass risk) |
| D10 | Name | `namespace` (matches roadmap, agent profiles, Kubernetes) despite the OpenLineage dataset `namespace` field | `team` |
| D11 | Job alias uniqueness | Stays global; namespace is ownership, not identity | Unique per namespace (every alias-addressed surface would need a qualifier) |

## 5. Concepts

### 5.1 Namespace

- **Syntax:** DNS label, `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`. `default` always
  exists. `*` is reserved (never a real namespace).
- **Declared, not implicit.** The policy file's `namespaces:` map declares every
  namespace and its settings. Server-side lint and apply reject an undeclared
  namespace. `default` is implicitly declared with empty settings.
- **Carried by:** `Job.Namespace` (text, not null, default `'default'`, indexed).
  Denormalised copies on `JobRun.Namespace`, `Backfill.Namespace`,
  `Incident.Namespace` so global lists filter without a join. Triggers, task runs,
  events, receipts, lineage and contract rows derive it by joining through the job.
- **Dataset namespace is a different axis.** `DatasetDeclaration.Namespace`,
  `DatasetMetric.Namespace`, `DatasetDerivation.Namespace` and the
  `/v1/datasets/:ns/:name` path segment are the OpenLineage dataset namespace
  (reserved for cross-instance datasets, unused in v1). They are not the tenancy
  namespace and are not changed. In v1 a dataset's tenancy is derived from its
  **declared producers**: the tenancy namespaces of the jobs referenced by
  `DatasetDeclaration.JobID` (a `DatasetDerivation` records a scheduling decision and
  often has no run, so it is not an ownership record). A dataset with producers in
  several namespaces is readable from each; one with no declared producer is
  cluster-only. `DatasetDerivation` gains `ProducerJobID` and `JobNamespace`,
  stamped at decision time for every outcome including skips (most skips have no
  run); metrics filter by the producing run's persisted namespace, derivations by
  `JobNamespace`, and legacy rows with neither fall back to the declared producers'
  namespaces. The operator guide says "tenancy namespace" vs "dataset namespace"
  exactly once.
- **Agent profiles** already carry `AgentProfile.Namespace *string`; it becomes the
  tenancy namespace (§10.4).

### 5.2 Job alias

`Job.Alias` keeps its global unique index. Every alias-addressed surface (CLI verbs,
`KeyScope.Jobs`, lineage, receipts, `caesium why/blame`, trigger chains,
`/v1/jobs/:id` lookups by alias) is unchanged. Moving a job between namespaces is
an apply that changes `metadata.namespace`; it requires operator in both the old
and the new namespace (§9.4).

### 5.3 Roles and grants

- Roles: `viewer < runner < operator < admin`, `models.RoleLevel` unchanged.
- A **grant** is `(role, namespace)`; namespace `*` means cluster-wide.
- A principal carries `Grants map[string]models.Role` (namespace → highest role).
- **Effective role in namespace N** = `max(Grants[N], Grants["*"])`.
- **Cluster-only routes** require the role at `*` (see §8.1 for the list).
- **Namespace admin** (`admin @ marketing`) = operator in `marketing` plus the
  ability to mint, list, rotate and revoke API keys whose grants are confined to
  `marketing`. It does not reach cluster-only routes.

### 5.4 Principal

`internal/auth/principal.go` becomes:

```go
type Principal struct {
    Kind      PrincipalKind      // api_key | user | agent_session
    Grants    Grants             // namespace -> role; "*" for cluster-wide
    Scope     []byte             // raw KeyScope JSON (job allow-list / agent claim), unchanged
    Subject   string             // audit actor: key prefix or user email
    UserID    *uuid.UUID
    KeyID     *uuid.UUID
}

func (p *Principal) RoleIn(namespace string) models.Role
func (p *Principal) ClusterRole() models.Role            // Grants["*"]
func (p *Principal) AllowedNamespaces() []string         // nil == unrestricted (holds viewer+ at "*")
```

`Principal.Role` is removed; the two places that read it (`api/middleware/auth.go`,
`api/rest/controller/auth/sso.go` whoami) move to `RoleIn`/`ClusterRole`. Audit and
metrics label the cluster role.

## 6. Policy file

### 6.1 Location and lifecycle

- `CAESIUM_AUTH_POLICY_FILE=/etc/caesium/policy.yaml`. Optional.
- Parsed at startup; a parse or validation error is a fatal startup error.
- **Reload:** the server polls the file's modification time every
  `CAESIUM_AUTH_POLICY_RELOAD_INTERVAL` (default `10s`) and on
  `POST /v1/auth/policy/reload` (cluster admin). A new revision is validated in full
  and swapped atomically (`atomic.Pointer`). An invalid revision never replaces the
  last good one: it logs at error, increments
  `caesium_auth_policy_reload_failures_total`, and writes audit action
  `policy.reload_failed`. A successful swap writes `policy.reloaded` with the file
  hash.
- Distributed mode: every node reads its own copy of the file (Helm mounts one
  ConfigMap/Secret). `GET /v1/auth/policy` returns the revision hash so drift is
  visible; `caesium auth policy show` prints it.

### 6.2 Schema

```yaml
apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  marketing:
    kubernetes:                       # optional; §10.1
      namespace: caesium-marketing
      serviceAccountName: caesium-jobs
    secrets:                          # optional; §10.2. Omitted == allow all (compat).
      allow:
        - vault/secret/data/marketing/*
        - k8s/marketing-*
        - env/MARKETING_*
    quotas:                           # optional; §10.3
      maxConcurrentRuns: 8
bindings:
  - subjects:
      groups: ["CN=Caesium Admins,OU=Groups,DC=example,DC=com"]
    role: admin
    namespaces: ["*"]
  - subjects:
      groups: ["marketing-eng"]
      users: ["alice@example.com"]
    role: operator
    namespaces: ["marketing"]
  - subjects:
      groups: ["*"]                   # every authenticated SSO user
    role: viewer
    namespaces: ["*"]
```

Validation rules (all enforced by `internal/auth/policy` and by
`caesium auth policy lint`):

- Namespace keys match §5.1 syntax; `*` is rejected as a key; `default` may be
  declared to attach settings.
- `bindings[].role` ∈ the four roles; `namespaces` non-empty, each either `*` or a
  declared namespace; `subjects` has at least one of `groups`, `users`.
- `groups` entries are exact strings (LDAP DNs with `=` and `,` are fine because
  YAML quotes them; the old "split on the last `=`" hack goes away). `"*"` matches
  every authenticated user. `users` entries are compared case-insensitively to
  `User.Email`.
- `secrets.allow` globs: `provider/path` with `*` matching within a path segment
  and `**` across segments (`path.Match` semantics extended with `**`).
- Lint warnings (non-fatal): a namespace other than `default` without `secrets`
  rules; a binding whose group never matched any known user (server-side only).

### 6.3 Resolution

`Resolve(groups []string, email string) Grants`:

1. For each binding whose subjects match (any group exact-match, `"*"`, or email),
   for each namespace in the binding: `grants[ns] = max(grants[ns], role)`.
2. Empty result ⇒ login denied (`auth.login_denied`, reason `no_binding`), unless
   `CAESIUM_AUTH_DEFAULT_ROLE` is set, in which case it becomes `grants["*"]`.
3. Grants are computed **on every request** from `User.Groups` + `User.Email`
   (cheap: map lookups over a small binding list). Nothing about grants is stored on
   the user or the session. `User.Role` is retained only as a denormalised
   "cluster role at last login" for the users list; it is no longer read by the
   middleware.

### 6.4 Compatibility with the env mapping

- No policy file ⇒ `CAESIUM_AUTH_ROLE_MAPPING` and `CAESIUM_AUTH_DEFAULT_ROLE` are
  translated at startup into cluster-wide bindings (`group=role` ⇒
  `{groups:[group], role, namespaces:["*"]}`) and a single `default` namespace.
  Behaviour is byte-for-byte today's.
- Policy file **and** `CAESIUM_AUTH_ROLE_MAPPING` both set ⇒ fatal startup error
  naming both. The env mapping is documented as deprecated.

### 6.5 CLI

- `caesium auth policy lint --path policy.yaml` — offline validation, exit 1 on
  error, warnings on stderr, `--json` for machine output.
- `caesium auth policy show [--json]` — effective policy from the server
  (`GET /v1/auth/policy`, cluster admin), including revision hash and the
  translated env mapping when no file is configured.
- `caesium auth policy reload` — `POST /v1/auth/policy/reload`.

## 7. Principals and credentials

### 7.1 SSO users

`PrincipalFromUser(u, policy)` sets `Grants = policy.Resolve(u.Groups, u.Email)`.
The login tail (`internal/auth/provider.go` `SSOService.Complete`) today calls
`RoleMapper.Resolve(groups)` before provisioning; it is rewritten to call
`policy.Resolve(groups, email)`, deny with `no_binding` when empty (today's
deny-by-default, preserved), and store the cluster role as the denormalised
`User.Role`. Session validation (§11) may refresh `u.Groups` first.

### 7.2 API keys

- `APIKey` gains `Namespaces datatypes.JSON` (list of namespace names or `["*"]`).
  Existing rows have NULL ⇒ treated as `["*"]`, so every existing key keeps its
  meaning (role at cluster level).
- `PrincipalFromKey` sets `Grants[ns] = key.Role` for each listed namespace.
- **Minting constraint:** a caller may only create/rotate a key whose grants are a
  subset of its own effective grants (per namespace, role ≤ caller's role there). A
  cluster admin can mint anything; a namespace admin can mint keys confined to its
  namespaces at ≤ admin.
- `KeyScope.Jobs` (alias allow-list) remains a further narrowing inside the key's
  namespaces. `caesium auth key create --role operator --namespace marketing
  [--namespace finance] [--jobs a,b]`. `/v1/auth/keys` list shows namespaces; a
  namespace admin sees only keys confined to its namespaces.
- **All `/v1/auth/*` controllers switch from `middleware.GetAuthKey(c)` to
  `middleware.GetPrincipal(c)`.** `CreatedBy` and the audit actor become
  `principal.Subject`. This is the fix for SSO admins being 401'd.

### 7.3 User-bound keys (for `caesium login`, §13)

- `APIKey.UserID *uuid.UUID` (nullable, indexed). A user-bound key stores no role
  or namespaces; on validation the service loads the user and resolves grants from
  the policy, so the key tracks group changes and dies when the user is disabled.
- Prefix `csk_user_`, default TTL `CAESIUM_AUTH_CLI_KEY_TTL` (default `12h`, max
  `7d`), audit actor = the user's email, `Principal.Kind = user`, `UserID` set.
- Listed under the user in `GET /v1/auth/users/:id`; revoked by `caesium logout`,
  by disabling the user, or by an admin revoke.

### 7.4 Agent-session tokens

They are intercepted first in `authorizeScope` and confined to their incident's
`/v1/agent/*` surface, unchanged. Their frozen job allow-list is built by
`internal/incident/allowlist.go` `FreezeAllowlist` from the **global** lineage graph,
so today it can contain jobs from other namespaces. Two rules close that:

- **Freeze:** only jobs in the incident's namespace enter the allow-list; foreign
  downstream jobs are recorded as `external` counts in the bundle, never by alias.
- **Serve:** the agent context, bundle and MCP handlers (`api/rest/service/agent`)
  serve only runs whose **persisted** namespace equals the incident's namespace —
  for allow-listed jobs and for the incident's own job alike, which today bypasses
  the allow-list (`History`) — so a job moved into the namespace brings no foreign
  history and a job moved out exposes no new history.

### 7.5 Legacy static secrets

`CAESIUM_MANUAL_TRIGGER_API_KEY` and `CAESIUM_EVENT_INGEST_API_KEY` are deprecated:

- Still honoured for one release. Startup logs a deprecation warning naming the
  replacement (a runner key confined to the namespace or job list).
- Requests authenticated by them are audited with actor `legacy:manual-trigger` /
  `legacy:event-ingest` so they are visible in `caesium auth audit`.
- The webhook prefix `/v1/hooks/` stays public (per-job HMAC secrets are the
  authentication there); its secret resolves through the job's scoped resolver
  (§10.2).

## 8. Enforcement

### 8.1 Route classes

`endpointPolicy` values change from `models.Role` to:

```go
type RoutePolicy struct {
    Role  models.Role
    Class RouteClass // cluster | namespaced | collection | identity
}
```

- **cluster** — requires `ClusterRole() ≥ Role`. Members: `/v1/auth/users*`,
  `/v1/auth/sessions*`, `/v1/auth/policy*`, `/v1/logs/*`, `/v1/database/*`,
  `/v1/system/nodes`, `/v1/nodes/:id/workers`, `/v1/cache/prune`, `/v1/atoms*`,
  and `GET /metrics` (viewer at `*`): the Prometheus registry is global and its
  series are labelled with job aliases and task names, so a namespace-confined
  principal must not scrape it. A tenant-filtered metrics view is future work.
- **namespaced** — the middleware resolves the target namespace(s) and requires
  `RoleIn(ns) ≥ Role` for **every** resolved namespace (a cluster grant satisfies
  all of them). Resolution table (extends `resolveScopedJobAlias`):

  | Route shape | Namespace source |
  | --- | --- |
  | `/v1/jobs/:id/**` | `jobs.namespace` by job id |
  | `…/runs/:id/**` | `job_runs.namespace` by run id |
  | `…/backfills/:id/**` | `backfills.namespace` |
  | `/v1/incidents/:id/**`, `/v1/agent/incidents/:id/**` | `incidents.namespace` |
  | `/v1/triggers/:id/**` | the trigger's job namespace |
  | `/v1/notifications/{channels,policies}/:id`, `/v1/agentprofiles/:id` | the object's namespace; `*` (shared) objects require the role at `*` for writes and viewer anywhere for reads |
  | `/v1/datasets/:ns/:name/**`, `/v1/datasets/holds/:id/release`, `POST /v1/datasets/:ns/:name/advance` | the namespaces of the dataset's declared producers (`DatasetDeclaration.JobID`); the caller needs the role in at least one for reads, in every one for the hold release and the manual watermark advance; no declared producer ⇒ cluster-only |
  | `POST /v1/jobs` | body `metadata.namespace` (default `default`) |
  | `POST /v1/jobdefs/apply`, `/lint`, `/diff` | every definition's `metadata.namespace`; apply additionally checks operator on every namespace in the server-computed prune set when `prune: true` |
  | `POST /v1/events` (ingest) | the namespaces of every job the event routes to; the event is delivered to matching jobs in namespaces where the caller holds runner, and the response reports `delivered` and `skipped_namespaces` so a partial delivery is never silent |
  | `POST /v1/notifications/*`, `POST /v1/agentprofiles` | body `namespace` |
  | `POST /v1/auth/keys` | every namespace in the body's `namespaces` (or `*`); required role is **admin** in each (§7.2) |
  | `/v1/auth/keys/:id/{revoke,rotate}` | every namespace the key is confined to (NULL ⇒ `*`); admin in each |
  | `GET /v1/auth/audit` | the `?namespace=` query value; omitted ⇒ `*` (cluster admin sees everything) |

- **collection** — the middleware sets `ContextKeyAllowedNamespaces` (nil when the
  principal holds ≥ viewer at `*`). The handler **must** call
  `authz.NamespaceFilter(c, db, column)` (or the equivalent scoped query helper)
  before returning rows. Members: `GET /v1/jobs`, `/v1/events`, `/v1/events/ingested`,
  `/v1/stats*`, `/v1/triggers`, `/v1/incidents`, `/v1/datasets`, `/v1/datasets/holds`,
  `/v1/notifications/*` lists, `/v1/agentprofiles`, `/v1/contracts/graph`,
  `/v1/lineage/impact`, `/v1/auth/keys` (namespace admins).
- **identity** — reachable by any authenticated principal regardless of grants:
  `/auth/whoami`, `/auth/logout` (which also self-revokes a user-bound key, §13.2),
  `/auth/cli/finish`, `/v1/system/features`.

The completeness test (`api/auth_rbac_policy_completeness_test.go`) is extended: an
unclassified mounted route fails the build. A second guard: the `collection`
filter helper marks the context; a test-only post-handler check in the
completeness harness (and the integration matrix in §17) catches a collection
handler that never called it.

### 8.2 Middleware flow (`api/middleware/auth.go`)

1. Authenticate → `Principal` (unchanged sources: bearer key, cookie session; new:
   user-bound key).
2. Normalise route; look up `RoutePolicy`; unknown ⇒ `unknown_route` 403 (unchanged).
3. By class: `cluster` → `ClusterRole()`; `namespaced` → resolve target, `RoleIn`;
   `collection` → check `RoleIn(any)`: the principal must hold the minimum role in at
   least one namespace, then inject the allowed set; `identity` → pass.
4. Existing `authorizeScope` (job allow-list, agent claim) runs **after** the
   namespace decision, unchanged in semantics, so a job-scoped key inside a
   namespace grant is narrowed further, never widened.
5. Decision context (namespace, class, effective role) is attached for audit.

### 8.3 Collections and aggregates

- `internal/authz` (new, small): `AllowedNamespaces(ctx) ([]string, bool)`,
  `NamespaceFilter(db *gorm.DB, column string) *gorm.DB` (no-op when unrestricted),
  and `MarkFiltered(c)`.
- Job list, incidents, triggers, datasets, holds, channels, policies, profiles:
  `WHERE <table>.namespace IN (?)`.
- Events stream (`GET /v1/events`): with `run_id`, the existing rule (resolve the
  run's job, require viewer there). Without `run_id`, a restricted principal
  receives only events whose run's namespace is allowed (server-side filter in the
  fan-out; the scoped-key `EventsScopedDenyMessage` rule is retained for job-scoped
  keys).
- Stats: computed over allowed namespaces only.
- **Job diff and lint baselines**: `POST /v1/jobdefs/diff` compares the request
  against every persisted job (`jobdiff.LoadDatabaseSpecs`) and reports absent jobs
  as removed; the baseline, the `removed` list and contract findings are filtered to
  allowed namespaces before comparison, and an alias in the request that already
  exists in a namespace the caller lacks is a 403 naming the namespace. Server-side
  lint's cross-job contract findings are filtered the same way.
- **History under a moved job**: a job's nested reads (`/v1/jobs/:id/runs`, the
  latest-run summary attached to job responses, run counts, `runs/diff`, blame,
  topology history) authorise on the job's current namespace for access to the job
  and then filter rows by the run's **persisted** namespace, so history that stayed
  in the old namespace is invisible to a principal confined to the new one (and vice
  versa). **Writes on history are checked in both namespaces**: replay requires
  viewer on the historical run's namespace (the baseline) **and** runner in the
  job's current namespace (where the replay executes); retry, which reuses the
  historical run row and launches the *current* job model, is refused with
  `409 namespace_moved` when the run's persisted namespace differs from the job's
  current one (use replay instead). A7's matrix includes a moved job with principals
  confined to each side, for reads and for both writes.
- Lineage impact and contracts graph: computed over the whole graph, then redacted
  at the boundary with a wire-level contract: a foreign node becomes
  `{id: <opaque per-response hash>, kind: "external"}` with alias, namespace,
  producing step and provenance removed; an edge touching a foreign node keeps only
  its endpoints — dataset names, producer/consumer schemas, previous schemas,
  compatibility findings and provenance are dropped (`internal/contract/graph.go`
  `Edge`, `internal/lineage/impact.go` `ImpactNode`). A response-level test asserts
  that no foreign alias, dataset name or schema text appears anywhere in the
  serialised body. Job-scoped keys keep today's outright denial.
- `/v1/database/query` stays cluster-admin only (it is raw SQL).

### 8.4 Audit

- `AuditLog.Namespace string` (indexed) populated from the decision (`*` for
  cluster routes, the resolved namespace otherwise, empty for identity routes).
- `GET /v1/auth/audit?namespace=` and `caesium auth audit --namespace`.
- New actions: `policy.reloaded`, `policy.reload_failed`, `user.disable`,
  `user.enable`, `auth.sessions_revoked`, `auth.groups_refreshed` (metadata: diff
  count), `auth.cli_login`. Quota deferrals are a metric (§10.3), not audit rows.

## 9. Namespace lifecycle on jobs

### 9.1 Manifest

`pkg/jobdef/definition.go` `Metadata.Namespace string` (`yaml:"namespace,omitempty"`),
documented via `internal/jobdef/report` so `docs/job-schema-reference.md` regenerates.
Offline `caesium job lint` validates syntax only; server-side lint
(`POST /v1/jobdefs/lint`) also requires the namespace to be declared and checks
secret references against the namespace's rules (§10.2).

### 9.2 Apply

`Importer.Apply` writes `Job.Namespace`. Every job and run constructor carries it:
the legacy `POST /v1/jobs` service (`api/rest/service/job` `Create`, whose request
gains `metadata.namespace`), `Store.admit`/`Start*` for runs, `StartForBackfill`,
the incident store, and the quarantined replay path, which inserts `JobRun` rows
directly (`internal/replay/replay.go`) and must stamp the job's current namespace.
`caesium job diff` shows namespace changes as a metadata diff. The apply preview
lists the namespaces touched.

### 9.3 Git sync

`CAESIUM_JOBDEF_GIT_SOURCES` is a JSON array of source objects
(`pkg/env/jobdef.go` `GitSourceConfig`); each object gains an optional
`"namespaces": ["a", "b"]` field. A source with the field may only apply manifests
whose `metadata.namespace` is in the list; violations are reported per manifest and
skipped, and the sync run is audited with actor `system:git-sync` and the source
URL. A source without the field is unrestricted (compat). The boundary is
enforced **inside the importer transactions**, not only on the incoming manifest:
`guardJobMutationTx` refuses a source-owned job whose stored **or** requested
namespace is outside the source's allowlist (so a narrowed source cannot move a
job it used to own into its allowlist, nor out of it), and `PruneMissing` with a
source allowlist prunes only jobs whose namespace is in it — jobs outside are left
untouched and reported. A manifest rejected for a namespace violation is **not
applied but its alias stays protected from pruning** (`desiredAliases` keeps it),
so a forbidden move leaves the existing job active and unchanged rather than
turning into a deletion.

### 9.4 Moving a job

An apply whose `metadata.namespace` differs from the stored value requires operator
in both namespaces (the middleware resolves both: stored by alias, requested from the
body). History (runs, receipts, incidents) keeps its original namespace; the
denormalised columns are not rewritten. Incident deduplication
(`internal/incident/store.go` `DedupeKey`) gains the run's persisted namespace, so a
failure after a move opens a new incident in the new namespace instead of folding
into an open one from the old owner; incidents open at move time stay where they
are. **The namespace is part of the cache key**
(`internal/cache/hash.go`): a move invalidates cached step outputs, so results
produced under one owner's secrets and Kubernetes identity are never republished
under another. Replays of a moved job run under the job's current namespace.

### 9.5 Prune

Unchanged semantics (same provenance source, not in the set). The middleware checks
operator in the namespace of every job in the server-computed prune set; any miss
fails the whole apply with 403 naming the namespace. Job-scoped keys still cannot
prune at all.

## 10. Isolation

### 10.1 Kubernetes namespace and service account

- Policy: `namespaces.<ns>.kubernetes.{namespace, serviceAccountName}`. Both
  optional; fall back to `CAESIUM_KUBERNETES_NAMESPACE` and no service account.
- `internal/atom/kubernetes/engine.go`: the engine holds the core client, not a
  single `PodInterface`. `EngineCreateRequest` carries a resolved
  `KubeTarget{Namespace, ServiceAccountName}`; `Create`, `Get`, `Logs`, `Stop`,
  `Remove` and the label-based list/reaper use the request's namespace (the reaper
  lists every namespace named in the policy plus the default).
- Precedence for the pod's service account: step-level
  `kubernetes.serviceAccountName` > namespace default > none.
- Volumes/PVCs and `secret://k8s/<secret>/<key>` resolve in the mapped namespace.
- **Every** engine request carries the target: `EngineGetRequest`, `EngineWaitRequest`,
  `EngineStopRequest`, `EngineLogsRequest` and `EngineListRequest` gain `Namespace`
  (Kubernetes ignores it for Docker/Podman), and the task's persisted runtime id is
  namespace-qualified so recovery, wait, stop, logs and reaping after a restart find
  the pod without the policy.
- Distributed mode: the owner resolves an **isolation context**
  `{KubeTarget, SecretRules, PolicyRevision}` from the run's namespace when it builds
  the dispatch request and carries it through `HandleDispatch` → `InboundDispatch` →
  the worker's dispatch metadata to the runtime executor; workers never read the
  policy file. Workers advertise `isolation_v1` in `GET /internal/capabilities`
  (`CapabilitiesResponse.Supports`, `internal/dispatch/dispatch.go`); the owner
  **fails closed** whenever the run's isolation context is non-default — a mapped
  Kubernetes target **or** any secret rule, on any engine — and never dispatches
  such a task to a worker without the capability (metric
  `caesium_dispatch_incompatible_worker_total`; the run waits for a capable peer).
  Mixed versions the other way round (old owner, new worker) carry no context and
  execute exactly as today; the upgrade order is therefore workers first, owners
  second, and the guide says so.
- Helm: `values.yaml` gains `jobNamespaces: []`; the chart renders a `Role` +
  `RoleBinding` (pods, pods/log, secrets get, persistentvolumeclaims) for each, and
  the guide tells operators to pre-create the namespaces.

### 10.2 Secrets

- Policy: `namespaces.<ns>.secrets.allow: [globs]` over the `provider/path` form
  (`vault/secret/data/marketing/*`, `k8s/marketing-*`, `env/MARKETING_*`).
  Omitted ⇒ allow all (compat, lint warning).
- **Canonicalise before matching.** The Kubernetes resolver accepts
  `secret://k8s/<secret>/<key>`, `secret://k8s/<namespace>/<secret>/<key>` and the
  query overrides `?namespace=`, `?name=`, `?key=` (`kubernetes.go` `parseReference`).
  The scoped resolver therefore first resolves the **effective** provider, namespace,
  name and key exactly as the inner resolver would, then matches the allow rules
  against the canonical form `k8s/<effective-namespace>/<secret>`. An allow entry
  `k8s/<secret>` means "in this namespace's mapped Kubernetes namespace"; any
  effective namespace not named by a rule is denied, whatever the reference's
  surface form. **Every provider canonicalises with its own effective-target
  parser**, never by path alone: the env resolver gives `?name=` precedence over
  the path (`env.go`), and the Vault resolver reads `?field=` and path semantics
  (`vault.go`), so each `Resolver` exports `CanonicalTarget(ref) (string, error)`
  and the scoped resolver matches rules against that string
  (`env/<effective name>`, `vault/<mount>/<path>#<field>`,
  `k8s/<namespace>/<secret>`). A reference whose canonical target differs from its
  surface path is matched on the target only.
- `internal/jobdef/secret.ScopedResolver{inner Resolver, namespace string, rules}`
  implements `Resolver`; a denied reference returns
  `ErrSecretDenied{Namespace, Ref}` (redacted to `provider/<first segment>/…` in
  logs and lint output).
- **Before the cache.** The executor runs the allow-check (parse + canonicalise +
  rule match, no resolution) over every secret reference in the step **before** the
  cache lookup, so a now-denied reference can never be satisfied from a cached
  result.
- Wired at: `runtime.ResolveContainerSpecSecrets*` call sites in the executor and
  the local runner (the job's namespace is on the run), the HTTP trigger's per-job
  secret, and `lint.CheckSecrets` (server-side lint / apply preview). Git sync,
  registry auth and notification-channel credentials resolve with the unscoped
  cluster resolver as `system`.
- A denied reference fails the task with a clear reason and an incident class of
  `config` (not `transient_infra`).

### 10.3 Run quota and fairness (closes #395's v1 slice)

- Policy: `namespaces.<ns>.quotas.maxConcurrentRuns` (0/omitted ⇒ unlimited).
- **One transactional gate, every path that makes a run running.** Not every run
  start passes through `Store.admit`: manual and agent retries re-admit an existing
  row through `readmitRetryTx` (admission disabled), partition retries use the same
  path, and quarantined replay inserts a running `JobRun` directly
  (`internal/replay/replay.go`). So the namespace gate is a store-level primitive,
  `namespaceCapacityTx(tx, namespace)` (count running runs in the namespace on the
  indexed `job_runs.namespace` + status inside the caller's transaction), called by
  `admit`, by `readmitRetryTx`, by the partition-retry path and by the replay insert.
  Quarantined replays consume quota: they are real compute.
- **What happens at cap, per run kind** (no new queue representation is invented):
  - *Trigger-originated starts* (cron, event, HTTP, manual `run`) are enqueued into
    `run_queue` with `held_reason: namespace_quota`, regardless of the job's own
    queue policy; the dequeuer drops its "job must have a queue concurrency policy"
    prerequisite for rows held by quota, so policy-free jobs drain.
  - *Retries, partition retries and replays* return `409 namespace_quota` with
    `Retry-After`; they are on-demand operator or agent actions and the caller
    retries. An agent `retry` action records the deferral on the incident.
  - *Backfills* are never enqueued (`ErrRunQueued` is a completed-skip in the
    backfill runner); on `ErrNamespaceQuota` the runner **waits and retries the
    date** with backoff, so backfill accounting and cancellation stay intact.
- Fairness lives in the dequeuer: `DrainOnce` iterates namespaces round-robin
  (in-memory cursor, deterministic order), then jobs, and re-admits through the
  same gate. Per-job concurrency (`loadConcurrencyConfig`) still applies inside.
  Window-scheduled runs (sibling plan) must enter through `admit` as well.
- Metrics: `caesium_namespace_running_runs{namespace}`,
  `caesium_namespace_quota_deferrals_total{namespace}`.
- `GET /v1/jobs/:id/queue` and the Console queue page label a run held by quota
  (`held_reason: namespace_quota`).
- Not in v1: CPU/memory quotas, queued-run caps, priority across namespaces.

### 10.4 Shared objects

- `NotificationChannel.Namespace`, `NotificationPolicy.Namespace` (text, not null,
  default `'default'`), `AgentProfile.Namespace` becomes not null default `'default'`.
- The sentinel `*` marks a **cluster-shared** object: readable by any principal,
  writable only with operator at `*`.
- `namespace` is **create-only**. Update bodies may not change it (400); moving an
  object is delete + create by a principal authorised in both namespaces, and
  creating a `*` object requires operator at `*`. Referenced channels are validated
  at the same boundary.
- Lint: a job may reference channels/policies/profiles in its own namespace or
  shared ones; anything else is an error naming the object and namespace.
- Notification policies match runs in their own namespace only (a shared policy
  matches everywhere).

## 11. Identity freshness and user administration

### 11.1 Group refresh

- Refresh state lives on the **user**, shared by every credential type: one
  encrypted **refresh grant** per user (`User.RefreshGrant []byte`,
  `User.RefreshGrantVersion int64`, `User.GroupsRefreshedAt time.Time`). A login
  replaces the grant (the newest IdP session wins); sessions and user-bound keys
  carry nothing and refresh through their user. **Ownership is taken before the
  IdP is contacted, and every effect is fenced on it.** A refresher first claims a
  short lease with one CAS (`UPDATE users SET refresh_lease_owner=?,
  refresh_lease_until=now+timeout WHERE id=? AND refresh_grant_version=? AND
  (refresh_lease_until IS NULL OR refresh_lease_until < now)`); only the node
  holding the lease redeems the grant. The catalog is one Raft-replicated dqlite
  database, so the CAS serialises claims across nodes: two nodes can never redeem
  the same token. Success persists the rotated token, the groups and the timestamp
  with a second CAS on the same `refresh_grant_version`; failure effects (revocation)
  are applied under the same fence. A login that replaces the grant bumps the
  version, so any in-flight refresh's outcome — success or `invalid_grant` — is
  discarded rather than applied to the newer grant. An **uncertain** redemption
  (request sent, no definitive answer) marks the grant `refresh_uncertain`; a
  subsequent `invalid_grant` on an uncertain grant clears the grant instead of
  revoking credentials, and the unconditional staleness bound below then forces a
  re-login. Rotating refresh tokens therefore never strand a copy, and a race never
  turns into a mass revocation.
- Handle by provider: OIDC — the refresh token (requires the issuer to return one;
  Keycloak does by default; `offline_access` is not required); LDAP — the user DN;
  SAML — none.
- Encryption: AES-256-GCM with a key derived by HKDF-SHA256 from
  `CAESIUM_AUTH_KEY_HASH_SECRET` with info `caesium/session-refresh/v1`. Nonce per
  row. Rotating the secret invalidates handles (sessions then fall back to
  login-time groups until re-login; documented).
- On `SessionStore.Validate` **and** on `ValidateKey` for a user-bound key, if
  `now − user.GroupsRefreshedAt > CAESIUM_AUTH_GROUP_REFRESH_INTERVAL` (default `15m`),
  schedule an async refresh using the user's grant (single-flight per user id per
  node, CAS across nodes, bounded worker pool). The current request proceeds with
  current groups. **Maximum staleness is unconditional**: once
  `now − GroupsRefreshedAt > CAESIUM_AUTH_GROUP_MAX_STALENESS` (default `24h`),
  whatever the reason — no grant (SAML, rotated hash secret), repeated transient IdP
  failures, or a node that never got to refresh — the first request past the bound
  attempts one **synchronous** refresh (bounded by `CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT`,
  default `5s`); on success it proceeds, otherwise every credential of that user is
  rejected with 401 `stale_groups` until re-login. This bounds CLI-only users and a
  seven-day key alike. Outcomes:
  - success ⇒ `users.groups` updated, `GroupsRefreshedAt = now`, audit
    `auth.groups_refreshed` only when the set changed;
  - definitive failure (OIDC `invalid_grant`, LDAP user not found) ⇒
    `RevokeAllForUser` **and** revoke the user's user-bound keys, audit
    `auth.sessions_revoked` reason `idp_refresh_denied`;
  - transient failure ⇒ keep, retry at next validation (the unconditional bound
    above still applies), `caesium_auth_group_refresh_total{provider,outcome}`.
- SAML sessions: `CAESIUM_AUTH_SAML_SESSION_ABSOLUTE_TTL` (default `8h`) overrides
  the general absolute TTL; documented as the freshness bound for SAML.

### 11.2 User administration (cluster admin)

| Surface | Behaviour |
| --- | --- |
| `GET /v1/auth/users?email=&disabled=&limit=&offset=` | list with issuer, email, display name, groups, cluster role at last login, last login, disabled at |
| `GET /v1/auth/users/:id` | detail plus active sessions and user-bound keys |
| `POST /v1/auth/users/:id/disable` | sets `disabled_at`, revokes all sessions and user-bound keys; audit `user.disable` |
| `POST /v1/auth/users/:id/enable` | clears `disabled_at`; audit `user.enable` |
| `POST /v1/auth/users/:id/sessions/revoke` | `RevokeAllForUser`; audit `auth.sessions_revoked` reason `admin` |
| `GET /v1/auth/sessions?user_id=` | active sessions (id, method, created, last seen, IP, UA) |
| CLI | `caesium auth user list|get|disable|enable|logout` (`logout` = revoke sessions), `--json` on stdout |

## 12. Keycloak and the CI lane

### 12.1 Operator guidance (in the identity-and-access guide)

- Confidential client, standard flow, redirect URI
  `<CAESIUM_AUTH_PUBLIC_BASE_URL>/auth/sso/oidc/callback`.
- A client scope with a **Group Membership** mapper on claim `groups`, "Full group
  path" **off** (so bindings use `marketing-eng`, not `/marketing-eng`); or leave it
  on and bind the slash-prefixed name. `CAESIUM_AUTH_OIDC_SCOPES` must include that
  scope's name.
- Refresh tokens are issued by default; the SSO Session Idle/Max realm settings bound
  how long group refresh works before a re-login.
- Discovery via `CAESIUM_AUTH_OIDC_ISSUER_URL=https://kc.example.com/realms/<realm>`.

### 12.2 CI lane `ui-e2e-keycloak`

- `quay.io/keycloak/keycloak` started with `--import-realm` from
  `test/testdata/keycloak/caesium-realm.json` (users: `admin@example.com` in
  `caesium-admins`; `bob@example.com` in `marketing-eng`; `carol@example.com` in no
  group; direct-access grants enabled for the Go scenario). Caesium started with
  OIDC enabled, `CAESIUM_AUTH_POLICY_FILE` = `test/testdata/policy/two-namespaces.yaml`
  (`default`, `marketing`, `finance`; bindings as in §6.2), and two jobs applied,
  one per namespace.
- Playwright (`ui/e2e`, project `keycloak`): admin logs in via the real Keycloak
  login page and sees both namespaces and the switcher; bob sees only `marketing`,
  the finance job is absent from the list and its detail URL renders
  `InsufficientAccess`; carol's login is denied with the `no_binding` reason.
- Go integration (`test/`, `-tags=integration`, lane env `CAESIUM_KEYCLOAK_LANE=true`):
  a cookie-jar HTTP client drives the real OIDC authorization-code flow
  (`GET /auth/sso/oidc/login` → Keycloak's HTML login form → POST credentials →
  callback → session cookie), then runs `TestNamespaceAllowDenyMatrix` for SSO
  principals, the SSO-admin key-mint path, group refresh (change bob's group through
  Keycloak's admin REST API, advance past the interval, assert grants change without
  re-login), and `caesium auth user disable` killing the session.
- `ci-ok` depends on the lane like it does on `ui-e2e-auth`: the two job names are
  added to its `needs` list **and** to `scripts/ci-ok.py`'s `SELECTORS` map (a
  required job missing from the map fails `ci-ok` even when green), with the
  path-filter selectors and `scripts/test_ci.py` cases that go with them. `ci-ok`
  gates `v*` publication rather than merge today (`docs/ci.md` §1); the Keycloak
  lane inherits exactly that guarantee, no more.

## 13. CLI login

### 13.1 Flow (server-brokered, provider-agnostic)

1. `caesium login --server https://caesium.example.com` generates a random
   `code_verifier`, starts a loopback listener on `127.0.0.1:<random>` and opens
   the browser at `GET /auth/cli/start?port=<p>&state=<s>&challenge=<S256(verifier)>`
   (public path).
2. The server stores the **login transaction** `(state, port, challenge, hostname
   hint, expiry 5m, unconsumed)` and redirects into the normal SSO flow with
   `returnTo=/auth/cli/finish?state=<s>` (LDAP shows the credential form in the
   Console as today).
3. `GET /auth/cli/finish` (identity class, session cookie required) **mints
   nothing**: it renders a session-bound confirmation page ("Authorise the Caesium
   CLI on <hostname hint>, loopback port <p>, as <email>?"). Minting happens only
   on `POST /auth/cli/authorize {state}` from that page, which is an unsafe
   cookie-session request and therefore carries the CSRF token; it consumes the
   transaction atomically (a second POST is a 409), mints the user-bound key (§7.3),
   stores a one-time `code` (60s) bound to the transaction, audits `auth.cli_login`,
   and redirects the browser to `http://127.0.0.1:<p>/callback?code=<c>&state=<s>`.
   A GET that arrives with an unknown or consumed `state` shows an error, never a
   key. This closes the ambient-cookie mint: an attacker-planted listener cannot
   obtain a key without the signed-in user clicking the confirmation.
4. The CLI receives the callback, verifies `state`, and calls
   `POST /auth/cli/exchange {code, code_verifier}` (public path, single use,
   IP-rate-limited); the server checks `S256(code_verifier) == challenge` and
   returns the plaintext key once. A callback intercepted without the verifier is
   useless.
5. `--no-browser`: step 1 prints the start URL; step 3's confirmation page shows
   the code instead of redirecting; the user runs `caesium login --code <c>` (the
   CLI still holds the verifier).
6. Credential stored at `$XDG_CONFIG_HOME/caesium/credentials.yaml` (mode 0600,
   directory 0700), keyed by server URL: `{server, key: <plaintext csk_user_…>,
   key_id, key_prefix, expires_at, subject}`. The plaintext key is what later
   processes present as the bearer token (`ValidateKey` hashes the full token); it
   is never logged or printed. An OS keychain backend is future work.

Precedence for CLI credentials: `--api-key` flag > `CAESIUM_API_KEY` > stored
credential for the target server. `cmd/cliutil/auth.go` is the single place that
resolves it; every command already routes through it or is moved to.

### 13.2 Verbs

`caesium login`, `caesium logout`, `caesium whoami [--json]` (whoami + grants).

**Self-revocation.** An ordinary user holds no admin grant, so logout cannot use the
admin `POST /v1/auth/keys/:id/revoke`. Instead `POST /auth/logout` (identity class)
revokes **the credential that authenticated the request**: the cookie session today,
and, when the bearer is a user-bound key, that key. `caesium logout` calls it and then
deletes the local entry. Administrative revocation stays separate.

## 14. UI

- `ui/src/lib/auth.ts` `PrincipalState` gains `grants: Record<string, Role>`,
  `namespaces: string[]`, `roleIn(ns)`, `clusterRole`. Gating in `RunDetailPage`,
  `HoldPanel` and the job pages uses `roleIn(job.namespace)`.
- A namespace switcher in the top bar (hidden when the principal has exactly one
  namespace); selection is a **view filter** stored in `localStorage`; list pages
  send `?namespace=` (the server still filters by grants, the switcher only narrows).
- Job list and job detail show the namespace as a chip; the queue page shows
  "held by namespace quota".
- Access page (`/access`): my grants; declared namespaces and their settings
  (cluster viewer+); users list with disable/enable/log-out-everywhere (cluster
  admin); policy revision hash and last reload result. No editing of bindings.
- Login page: unchanged except the `no_binding` denial message.

## 15. Migration and compatibility

- Schema: `jobs.namespace`, `job_runs.namespace`, `backfills.namespace`,
  `incidents.namespace`, `notification_channels.namespace`,
  `notification_policies.namespace` (all `NOT NULL DEFAULT 'default'`),
  `agent_profiles.namespace` tightened to not null default, `api_keys.namespaces`
  (JSON, NULL ⇒ `["*"]`), `api_keys.user_id`, `sessions.refresh_handle`,
  `sessions.groups_refreshed_at`, `audit_logs.namespace`. All additive; GORM
  AutoMigrate; no data backfill. Rolling upgrade: old nodes ignore the columns.
- Behaviour with no policy file: identical to today (one namespace, env mapping,
  allow-all secrets, single Kubernetes namespace, no quotas).
- API: `job`, `run`, `incident`, channel, policy, profile JSON gain `namespace`.
  `whoami` gains `grants`, `namespaces`, `cluster_role`; `role` is kept and equals
  `cluster_role` for compatibility.
- Keys: `role` kept; `namespaces` added.
- Deprecations with one-release grace: `CAESIUM_AUTH_ROLE_MAPPING` (translated),
  `CAESIUM_MANUAL_TRIGGER_API_KEY`, `CAESIUM_EVENT_INGEST_API_KEY`.

## 16. Documentation (consolidate, do not sprawl)

- `git mv docs/sso-authentication.md docs/identity-and-access.md`; sections:
  Overview & concepts (namespace, grant, role) · Providers (OIDC incl. **Keycloak**,
  SAML, LDAP/Active Directory) · Policy file · Namespaces & isolation (Kubernetes,
  secrets, quotas, shared objects) · API keys & CLI login · Users & sessions ·
  Audit & metrics · Security checks · Migration from the env mapping. Update the
  `docs/README.md` index line (guardrail `TestDocsREADMEIndexesEveryTopLevelDoc`).
- This spec supersedes `docs/superpowers/specs/2026-05-27-sso-authentication-design.md`,
  which moves to `docs/archive/` with a "Superseded by" banner.
- `docs/roadmap.md` §3.1 becomes a three-line pointer to this spec and the exec plan;
  the "Read-only vs operator role distinction" line in §3.3 is marked shipped.
- `docs/sovereignty.md` comparison table gains a **Multi-tenancy** row.
- `docs/job-schema-reference.md` regenerates from `internal/jobdef/report` (never
  hand-edited).
- `docs/kubernetes-deployment.md` gains the `jobNamespaces` values and RBAC note.
- `docs/ci.md` lists the Keycloak lane.
- Net new files: this spec and the exec plan.

## 17. Testing strategy (the gate is end-to-end)

- **Unit:** policy parser/validator, `Resolve`, glob matcher, `ScopedResolver`,
  `RoutePolicy` classification, `NamespaceFilter`, quota gate, refresh-handle
  crypto round-trip, CLI credential store.
- **Completeness guards:** every mounted route classified; every `collection`
  handler marks the filter.
- **Integration (`test/`, real server).** The policy fixture
  `test/testdata/policy/two-namespaces.yaml` is mounted into **both** servers:
  `just integration-up` (no auth mode; exercises namespace *settings* — secret
  rules, quotas, Kubernetes mapping) and `just integration-up-agent` (the api-key
  auth lane; exercises *bindings*, namespaced keys and the allow/deny matrix). Auth
  scenarios use `requireAuthLane` and **fail, not skip,** when the lane is up but
  `CAESIUM_AUTH_POLICY_FILE` is missing. Scenarios:
  `TestNamespaceAllowDenyMatrix` (one key per role × namespace, every route class,
  expected 200/403/404 with `runCLIStdout` for `--json` verbs); apply/prune/move
  semantics; secret denial surfaced by lint and at run time; quota deferral and
  round-robin with two namespaces; shared-object reference lint; audit namespace
  column; users/sessions endpoints and CLI; policy reload (valid, invalid keeps last
  good); legacy static keys audited as `legacy:*`; every new CLI verb through
  `s.runCLI*`.
- **Kubernetes (kind lane):** a job in a mapped namespace lands its pod, PVC and
  `secret://k8s` read in that namespace with the mapped service account.
- **Keycloak lane:** §12.2.
- **UI:** Playwright `keycloak` project; Vitest for switcher, `roleIn`, Access page.
- **CLI login:** integration scenario drives `caesium login --no-browser` against
  the Keycloak lane, exchanges the code, runs `caesium whoami --json`, disables the
  user, asserts the key is dead.

## 18. Security requirements (non-optional)

- Deny by default at every layer: unknown route, unclassified route, undeclared
  namespace, empty grants, denied secret reference.
- A caller can never mint a credential wider than itself (per namespace).
- User-bound keys resolve grants at use time; disabling a user kills its sessions
  and keys within one validation.
- Refresh handles are encrypted at rest; plaintext refresh tokens are never logged.
- `/auth/cli/*`: minting requires an explicit, CSRF-protected confirmation POST from
  the signed-in browser; transactions and codes are single-use, short-lived, bound
  to `state` and to a CLI-held PKCE-style verifier; rate-limited by the existing IP
  limiter; the loopback callback verifies `state`.
- Cross-namespace aggregates never reveal another namespace's aliases or names
  (placeholder collapse).
- Audit every decision that changes access: policy reload, user disable/enable,
  session revoke, key mint with namespaces, group-refresh changes.
- The policy file is read-only to the server process in the Helm chart.

## 19. Open questions / future work

- **Keycloak authorization services as a PDP** (Airflow-style) — a later
  `PolicySource` implementation behind the same `Resolve` interface.
- **Verbs / custom roles** — if role-per-namespace proves too coarse, add a verb
  taxonomy behind `RoutePolicy` without another schema change.
- **Per-namespace CPU/memory quotas and queued-run caps** — #395's remainder.
- **Dataset tenancy as a first-class column** if datasets stop being job-derived.
- **OIDC device-code flow** for IdPs that support it (would remove the loopback
  listener requirement on locked-down hosts).
- **Account linking** across providers remains future work (unchanged from the SSO spec).
