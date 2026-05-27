# Design: Native SSO Authentication (OIDC, SAML, LDAP)

**Status:** Proposed
**Date:** 2026-05-27
**Author:** Christopher Ryan
**Topic:** User login / identity federation for the Caesium Web UI

---

## 1. Summary

Add native, self-contained Single Sign-On to Caesium so human users can log into the
Web UI with their organization's identity provider over **OIDC**, **SAML 2.0**, or
**LDAP** — with no external proxy or hosted dependency. IdP groups/claims map to
Caesium's existing role + job-scope authorization model. Sessions are server-side,
stored in the shared dqlite cluster. API keys remain unchanged for CLI/CI/machine
access; SSO is added **alongside** them for interactive users.

All three protocols are co-equal in scope and ship in the same release, built on a
shared identity/session foundation so each provider is an independently testable
increment.

## 2. Motivation & current state

Caesium today has exactly two auth modes (`CAESIUM_AUTH_MODE` ∈ `none` | `api-key`,
`pkg/env/env.go:137`). What exists:

- A mature **API-key system** (`internal/auth/service.go`): create/list/revoke/rotate,
  HMAC-SHA256 keyed hashing with legacy SHA-256 compat, timing-failure normalization,
  per-IP rate limiting, async `last_used_at` flushing, conflict-safe admin bootstrap,
  TLS required by default.
- **RBAC**: roles `admin>operator>runner>viewer` (`internal/models/api_key.go:23`)
  enforced by a static endpoint→min-role map (`internal/auth/rbac.go`); optional per-key
  **job-alias scope** (`api/middleware/auth_scope.go`).
- A single auth **middleware chokepoint** (`api/middleware/auth.go`) that validates a
  `Bearer` API key, checks role + scope, stashes the key in `ContextKeyAuth`, and audits.
- An **audit log** (`internal/auth/audit.go`, `models.AuditLog`) keyed on an actor string.
- A UI "login" that is an **API-key paste box** (`ui/src/features/auth/LoginPage.tsx`,
  `ui/src/lib/auth.ts`) — key held in memory only, verified against `GET /v1/jobs?limit=1`.

What is missing (the gap this design fills):

1. No human **identity model** (no user / session / group concept).
2. No **SSO protocols** and no login/logout/session lifecycle, cookies, CSRF, or
   external-token validation.
3. No **group/claim → role(+scope) mapping**.
4. UI login is key-paste only — no "Sign in with…" redirect.
5. The middleware understands only Bearer API keys.

There is also **no auth/SSO documentation** today; the only security design doc is
`docs/design-internal-mtls-auto-provisioning.md` (node mTLS). No IdP libraries are
present (`golang.org/x/oauth2` is only an indirect dependency).

## 3. Goals / Non-goals

**Goals**

- Human users authenticate to the Web UI via OIDC, SAML, or LDAP, configured by the operator.
- External identities are just-in-time provisioned into a `users` table.
- IdP groups/claims map to existing Caesium roles (+ optional job scope).
- Server-side sessions in dqlite, revocable, with idle + absolute timeouts.
- Audit log attributes actions to the real user identity.
- API keys continue to work unchanged for machine/CLI access, concurrently with SSO.
- Single binary, no external proxy or hosted service required.

**Non-goals (v1)**

- No managed/hosted identity offering (project is self-hosted OSS only).
- No CLI SSO device-code flow — the CLI keeps API keys (a future extension).
- No DB-backed / admin-UI role management — role mapping is declarative config in v1.
- No per-user ACLs beyond the existing role + job-scope model.
- No SCIM / directory provisioning, no GraphQL auth integration (GraphQL stays disabled
  when auth is enabled, as today).

## 4. Constraints & principles

- **Reuse the API-key hardening.** Session tokens are hashed at rest, last-seen writes are
  coalesced, the existing per-IP rate limiter and audit logger are reused. SSO inherits the
  security work already done for keys rather than re-inventing it.
- **One authorization model.** SSO does not add a parallel authz system; it feeds the
  existing `Role` + `KeyScope` and the static endpoint policy.
- **One chokepoint.** All authentication continues to resolve at the single auth middleware.
- **dqlite read/write discipline.** Hot-path reads use the read connection; writes are
  coalesced and use the write connection (consistent with the #188 read/write split).
- **Deny by default.** A user with no mapped role is denied, not granted a default role,
  unless the operator explicitly configures a default.

## 5. Architecture overview

### 5.1 The `Principal` abstraction (Phase 0 — no behavior change)

Today the middleware resolves an `*models.APIKey` and RBAC reads `key.Role`/`key.Scope`.
Introduce a `Principal` that both API keys and user sessions resolve into:

```go
type PrincipalKind string

const (
    PrincipalAPIKey PrincipalKind = "api_key"
    PrincipalUser   PrincipalKind = "user"
)

type Principal struct {
    Kind    PrincipalKind
    Role    models.Role
    Scope   *models.KeyScope // nil == unrestricted
    Subject string           // audit Actor: key prefix, or user email/id
    UserID  *uuid.UUID       // set when Kind == PrincipalUser
    KeyID   *uuid.UUID       // set when Kind == PrincipalAPIKey
}
```

The middleware resolves a `Principal` from **either** a `Bearer` API key **or** a session
cookie. `HasRole`, `CheckScope`, and audit logging operate on the `Principal`. The
existing helper `GetAuthKey(c)` is preserved (returns the underlying key when
`Kind == api_key`) and a new `GetPrincipal(c)` is added. Phase 0 refactors API-key auth to
flow through `Principal` with **identical** observable behavior and green tests.

### 5.2 Provider interface (symmetry across the three protocols)

Two flow shapes converge on one shared pipeline:

```go
// OIDC, SAML — browser redirect flows.
type RedirectAuthenticator interface {
    Name() string
    Begin(w http.ResponseWriter, r *http.Request, returnTo string) (redirectURL string, err error)
    Complete(r *http.Request) (*ExternalIdentity, error)
}

// LDAP — credential (username/password) flow.
type CredentialAuthenticator interface {
    Name() string
    Authenticate(ctx context.Context, username, password string) (*ExternalIdentity, error)
}

type ExternalIdentity struct {
    Issuer      string   // provider id, e.g. "oidc", "saml", "ldap"
    Subject     string   // stable IdP subject id
    Email       string
    DisplayName string
    Groups      []string
}
```

All providers converge on one shared tail: **provision/update the `users` row → map
groups → role(+scope) → mint a session → set cookie → redirect**. The security-sensitive
session logic lives once, in that shared tail.

### 5.3 Request flow (UI login, OIDC example)

```
Browser            Caesium                              IdP
  | GET /auth/sso/oidc/login (returnTo)                  |
  |--------------->| Begin: build authz URL w/ PKCE,      |
  |                | state+nonce in short-lived cookie    |
  |<---------------| 302 redirect ----------------------->|
  |                |                          (user authenticates)
  |<-----------------------------------------------------|  302 -> /auth/sso/oidc/callback?code&state
  | GET /callback  |                                      |
  |--------------->| Complete: verify state, exchange     |
  |                | code (PKCE), verify id_token+nonce ->|
  |                | ExternalIdentity                     |
  |                | provision user, map role, mint       |
  |                | session, Set-Cookie (httpOnly)       |
  |<---------------| 302 -> returnTo                      |
  | (subsequent requests carry the session cookie; middleware resolves Principal)
```

LDAP differs only at the front: the UI posts username+password to
`POST /auth/sso/ldap/login`, the server calls `Authenticate`, then runs the identical tail.

### 5.4 Reuse map — extend vs. build new

The authorization layer, security primitives, and DB/config/metrics plumbing already exist;
SSO **grows the `internal/auth` subsystem** rather than standing up a new one. The hard,
security-sensitive primitives are reused, not re-implemented.

**Reuse (extend in place):**

| Capability | Location | SSO use |
|---|---|---|
| Auth middleware chokepoint | `api/middleware/auth.go` | `Principal` resolves from API key *or* session cookie here; RBAC/scope/audit unchanged. |
| RBAC + roles | `internal/auth/rbac.go`, `models.Role`/`RoleLevel` | SSO users carry a `Role`; "highest matched" mapping reuses `RoleLevel`. |
| Job-scope | `internal/auth/scope.go`, `models.KeyScope` | `Principal.Scope` is the same type (`CheckScope`/`ScopeJobs`). |
| Token hashing | `internal/auth/hash.go` `HashKey` | Session tokens hashed with the same keyed scheme + `AUTH_KEY_HASH_SECRET`. |
| Secure random | `internal/auth/keygen.go` (`crypto/rand`+base62) | Session-token generator (`GenerateToken` beside `GenerateKey`). |
| Per-IP rate limiter | `internal/auth/ratelimit.go` | LDAP/login endpoints reuse verbatim. |
| Audit log + query + CLI | `internal/auth/audit.go`, `models.AuditLog`, `cmd/auth/audit.go` | New action constants only; `Actor` becomes the user. |
| Metrics pattern | `internal/metrics/metrics.go` | New `sso_*` counters follow the existing register pattern. |
| Coalesced last-used flusher | `Service.RunLastUsedFlusher` | Template for session `last_seen_at`. |
| dqlite read/write split (automatic) | `pkg/db` `installDqliteReadWriteSplit`, `Router` | `users`/`sessions` are catalog-tier; read/write routing is transparent. |
| Migrations | `pkg/db` `Migrate`/`migrateModels`, `pkg/dqlite/migrator.go` | Add the two models. |
| Config + TLS guard | `pkg/env/env.go`, `initAuth` (`cmd/start/start.go`) | New `AUTH_*` vars; extend the "TLS required" check to cookies. |
| `/auth/status` + proxy-aware IP | `api/api.go` (`authStatus`, `configureIPExtractor`) | Advertise methods; reuse trusted-proxy IP for session/audit source IP. |
| UI auth scaffold | `ui/src/lib/auth.ts`, `AuthGate.tsx`, `LoginPage.tsx` | Extended with SSO affordances + cookie/session mode. |
| `golang.org/x/oauth2` | indirect dep today | Promoted to direct for OIDC. |

**Build new (greenfield):**

- Protocol adapters + deps: `coreos/go-oidc/v3`, `crewjam/saml`, `go-ldap/ldap/v3`.
- `users` + `sessions` models/tables and the session store (mint/validate/revoke/expire/reap).
- Cookie + session-bound CSRF (synchronizer token) handling.
- Login/logout/callback/whoami endpoints + the `RedirectAuthenticator`/`CredentialAuthenticator`
  interface and the shared provision→map→mint tail.
- Group→role resolver and JIT user provisioning.
- OIDC PKCE/state/nonce, SAML signature/replay, LDAP search-then-bind.
- UI SSO affordances (buttons, LDAP form, session mode).

SAML protocol correctness is the only piece with no in-repo analog (see §18 risks).

## 6. Data model

New tables, created via `pkg/dqlite/migrator.go` migrations.

### 6.1 `users`

| column          | type        | notes |
|-----------------|-------------|-------|
| `id`            | uuid PK     | |
| `issuer`        | text        | provider id (`oidc`/`saml`/`ldap`) or issuer URL |
| `subject`       | text        | stable IdP subject; **unique index on (issuer, subject)** |
| `email`         | text        | |
| `display_name`  | text        | |
| `groups`        | json        | last-seen group snapshot (for display/debug) |
| `role`          | text        | resolved role cached at last login |
| `created_at`    | timestamp   | |
| `last_login_at` | timestamp   | |
| `disabled_at`   | timestamp?  | operator can disable a user without deleting history |

Users are **JIT-provisioned** on first successful login and updated (email, name, groups,
role, `last_login_at`) on each subsequent login.

### 6.2 `sessions`

| column                | type       | notes |
|-----------------------|------------|-------|
| `id`                  | uuid PK    | |
| `user_id`             | uuid FK    | indexed |
| `token_hash`          | text       | HMAC-SHA256 of the opaque token; **unique index**. Plaintext never stored. |
| `csrf_token`          | text       | per-session CSRF token (synchronizer pattern, §11.1); surfaced via `/auth/whoami` |
| `auth_method`         | text       | `oidc`/`saml`/`ldap` |
| `created_at`          | timestamp  | |
| `idle_expires_at`     | timestamp  | bumped on activity (coalesced) |
| `absolute_expires_at` | timestamp  | hard cap, never extended |
| `last_seen_at`        | timestamp  | coalesced write |
| `revoked_at`          | timestamp? | logout / admin revoke |
| `source_ip`           | text       | |
| `user_agent`          | text       | |

Roles/scopes reuse `models.Role` and `models.KeyScope` — no new authorization tables.

## 7. Configuration (envconfig, `CAESIUM_` prefix)

`CAESIUM_AUTH_MODE` remains the API-key/none baseline. SSO is **additive**: configuring any
provider enables SSO login alongside whatever `AUTH_MODE` provides. **SSO requires auth to
be enabled** — startup fails fast if a provider is configured while `AUTH_MODE=none`.

```
# General + sessions (shared by all providers)
CAESIUM_AUTH_PUBLIC_BASE_URL         external URL; used to build OIDC redirect, SAML ACS/metadata
CAESIUM_AUTH_SESSION_IDLE_TTL        default 8h
CAESIUM_AUTH_SESSION_ABSOLUTE_TTL    default 24h
CAESIUM_AUTH_SESSION_COOKIE_NAME     default "caesium_session"

# Role mapping (declarative; see §9)
CAESIUM_AUTH_ROLE_MAPPING            ';'-separated group=role (split on last '='); e.g. "caesium-admins=admin;data-eng=operator;*=viewer"
CAESIUM_AUTH_DEFAULT_ROLE            default "" (empty == deny if no group matches)

# OIDC
CAESIUM_AUTH_OIDC_ENABLED            default false
CAESIUM_AUTH_OIDC_ISSUER_URL         discovery base (…/.well-known/openid-configuration)
CAESIUM_AUTH_OIDC_CLIENT_ID
CAESIUM_AUTH_OIDC_CLIENT_SECRET
CAESIUM_AUTH_OIDC_SCOPES             default "openid profile email groups"
CAESIUM_AUTH_OIDC_GROUPS_CLAIM       default "groups"
CAESIUM_AUTH_OIDC_REDIRECT_URL       SP callback (derived from public base URL if unset)

# SAML
CAESIUM_AUTH_SAML_ENABLED            default false
CAESIUM_AUTH_SAML_IDP_METADATA_URL   (or _IDP_METADATA_XML for inline/file)
CAESIUM_AUTH_SAML_SP_ENTITY_ID
CAESIUM_AUTH_SAML_SP_CERT            SP signing/encryption cert (PEM path)
CAESIUM_AUTH_SAML_SP_KEY             SP private key (PEM path)
CAESIUM_AUTH_SAML_GROUPS_ATTRIBUTE   default "groups"

# LDAP
CAESIUM_AUTH_LDAP_ENABLED            default false
CAESIUM_AUTH_LDAP_URL                ldaps://dc.example.com:636
CAESIUM_AUTH_LDAP_START_TLS          default false (true to use StartTLS on ldap://)
CAESIUM_AUTH_LDAP_BIND_DN            service account for search
CAESIUM_AUTH_LDAP_BIND_PASSWORD
CAESIUM_AUTH_LDAP_USER_BASE_DN
CAESIUM_AUTH_LDAP_USER_FILTER        e.g. "(sAMAccountName=%s)"
CAESIUM_AUTH_LDAP_GROUP_BASE_DN
CAESIUM_AUTH_LDAP_GROUP_FILTER       e.g. "(member=%s)"
```

A new `AuthRequireTLS`-style guard: SSO providers require either `AUTH_REQUIRE_TLS=true`
(served behind TLS) or an explicit opt-out, because session cookies are `Secure`.

## 8. Authentication providers

### 8.1 OIDC (`github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`)

- **Flow:** Authorization Code with **PKCE**. `state` (CSRF) and `nonce` (replay) are
  generated per login and stored in a short-lived, signed, httpOnly pre-login cookie
  alongside the validated `returnTo`.
- **Validation:** provider discovery via issuer URL; verify ID-token signature against the
  JWKS, `iss`, `aud` (== client id), `exp`, and `nonce`. Groups read from the configured
  claim.
- **Endpoints:** `GET /auth/sso/oidc/login`, `GET /auth/sso/oidc/callback`.

### 8.2 SAML 2.0 (`github.com/crewjam/saml`)

- **Flow:** SP-initiated. IdP metadata loaded from a file or an **HTTPS URL with full TLS
  certificate verification** — a MITM of the metadata fetch could swap the IdP signing key
  and forge assertions, so plain HTTP and unverified TLS are rejected. SP exposes its own
  metadata for IdP configuration.
- **Validation:** **XML digital signature** verification on the assertion/response;
  enforce `Audience`, `Recipient`, `NotBefore`/`NotOnOrAfter` (with bounded clock skew);
  **replay protection** via a **dqlite-backed** assertion-ID cache (a short-TTL
  `saml_assertion_ids` catalog table). A per-node in-memory cache is insufficient — an
  attacker could replay a captured assertion against a different node in the cluster — so it
  is not used. RelayState carries the validated `returnTo`.
- **Endpoints:** `GET /auth/sso/saml/login`, `POST /auth/sso/saml/acs`,
  `GET /auth/sso/saml/metadata`.

### 8.3 LDAP (`github.com/go-ldap/ldap/v3`)

- **Flow:** search-then-bind. Connect over **LDAPS** or **StartTLS** (anonymous bind
  rejected); bind as the service account; search `USER_BASE_DN` with `USER_FILTER` (its `%s`
  placeholder = the supplied username); rebind as the found user DN with the supplied password
  to verify; query group membership with `GROUP_FILTER` (its `%s` placeholder = the
  authenticated user's **full DN**, e.g. `(member=%s)`; for `memberUid`-style schemas use the
  uid form, e.g. `(memberUid=%s)`). Groups are matched in role mapping by their **CN** by
  default (see §9).
- **Validation:** **all user-supplied input (username, DN) is escaped with `ldap.EscapeFilter`
  before substitution into any filter** — prevents LDAP filter injection (`*`, `(`, `)`, `\`).
  TLS verification on by default; empty password rejected pre-bind (LDAP treats
  empty-password bind as anonymous success); connection pooling/timeouts.
- **Endpoint:** `POST /auth/sso/ldap/login` (form: username + password). Reuses the per-IP
  rate limiter, since this endpoint accepts credentials.

## 9. Authorization: group/claim → role (+scope)

**Declarative config mapping (v1).** `CAESIUM_AUTH_ROLE_MAPPING` is a **semicolon-separated**
list of `group=role` entries, each split on its **last** `=`; `role` ∈
{`admin`,`operator`,`runner`,`viewer`}. The split-on-last-`=` rule lets a group key contain
`=` and `,`, which is required because LDAP/AD groups are often Distinguished Names
(`CN=Caesium Admins,OU=Groups,DC=example,DC=com`). Example:
`CN=Caesium Admins,OU=Groups,DC=example,DC=com=admin;data-eng=operator;*=viewer`. For LDAP,
the provider matches on the group **CN** by default (short keys); full-DN keys also work via
the last-`=` rule. A literal `;` inside a group name is unsupported. Resolution:

1. Collect the user's groups from the `ExternalIdentity`.
2. For each mapping entry whose group matches, take its role.
3. The user's effective role is the **highest** matched role (by `RoleLevel`).
4. If no group matches: use `CAESIUM_AUTH_DEFAULT_ROLE` if set, else **deny login**.

A wildcard entry `*=viewer` is supported as an explicit default. Job-scope per group is
**out of scope for v1** (all SSO users are unscoped within their role); the data model and
`Principal` already carry `Scope`, so it can be added later without migration churn.

Mapping is validated at startup (unknown roles fail fast). It lives in version-controlled
config — auditable, GitOps-friendly, no admin UI. DB-backed/admin-managed mapping is
explicitly **future work**.

## 10. Session management

- **Token:** 256-bit opaque token from `crypto/rand` (reuse `internal/auth/keygen.go`
  patterns), returned to the browser in the cookie; only its HMAC-SHA256 hash is stored
  (reuse `internal/auth/hash.go` with `AUTH_KEY_HASH_SECRET`). A DB read cannot resurrect a
  live token.
- **Cookie:** `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`, name configurable. The SPA
  never reads it; it makes same-origin credentialed requests.
- **Validation (hot path):** middleware hashes the cookie token, looks up the session via
  `db.Connection()` (catalog tier), checks `revoked_at`, `idle_expires_at`,
  `absolute_expires_at`, loads the user and **rejects if `users.disabled_at` is set** (a
  disabled user is locked out immediately, not only at session expiry), then builds a
  `Principal`. The dqlite read/write splitter (`installDqliteReadWriteSplit`, #188) routes the
  lookup to the read pool automatically — no manual connection selection.
- **Activity / last-seen:** `last_seen_at` and `idle_expires_at` bumps are **buffered and
  flushed** by a background flusher modeled on `Service.RunLastUsedFlusher`, so the hot path
  issues no per-request write. Coalescing matters because writes traverse Raft; the
  read/write split alone does not remove write-contention cost.
- **Lifecycle:** idle timeout (`AUTH_SESSION_IDLE_TTL`) and absolute timeout
  (`AUTH_SESSION_ABSOLUTE_TTL`, never extended). Logout deletes the session row;
  "log out everywhere" deletes all sessions for a user. Session-fixation safe: a fresh session
  token **and CSRF token** are always minted after successful authentication. Expired/revoked
  rows are reaped by a periodic sweep.
- **Role staleness:** the effective role is resolved at login and cached (`users.role`). A
  change to `CAESIUM_AUTH_ROLE_MAPPING` takes effect for an existing session only at its next
  login, or within at most `AUTH_SESSION_ABSOLUTE_TTL`. To drop elevated access sooner, use
  admin session revocation / "log out everywhere". (Per-request re-resolution is deferred —
  the user's groups are themselves a login-time snapshot.)

## 11. Middleware, endpoints, and UI

### 11.1 Middleware (`api/middleware/auth.go`)

Principal resolution order:

1. If an `Authorization: Bearer` header is present → validate as an **API key** (today's
   path) → `Principal{Kind: api_key}`.
2. Else if the session cookie is present → validate session → `Principal{Kind: user}`.
3. Else → 401.

RBAC (`RequiredRole`/`HasRole`) and scope checks then run against the `Principal`
unchanged. **CSRF (session-bound synchronizer token):** each session row carries a random
`csrf_token` minted at login. For **cookie-authenticated** unsafe methods
(POST/PUT/PATCH/DELETE), the request must send an `X-CSRF-Token` header that matches
(constant-time) the resolved session's `csrf_token`; the check runs in the middleware *after*
the session is resolved, so it validates against server-side state rather than a second
cookie. The SPA reads the token from `/auth/whoami` (which itself requires the httpOnly
session cookie and same origin) and keeps it in memory. This avoids the subdomain
cookie-injection weakness of a plain double-submit cookie. Bearer/API-key requests are
**exempt** (no ambient cookie → not forgeable cross-site). `SameSite=Lax` on the session
cookie is defense-in-depth.

### 11.2 Endpoints

- `GET /auth/status` — extended to advertise enabled methods so the UI renders the right
  affordances: `{ enabled, methods: [ {type:"api-key"}, {type:"oidc", loginUrl}, {type:"saml", loginUrl}, {type:"ldap"} ] }`.
- `GET /auth/sso/{oidc,saml}/login`, `GET /auth/sso/oidc/callback`,
  `POST /auth/sso/saml/acs`, `GET /auth/sso/saml/metadata`, `POST /auth/sso/ldap/login`.
- `GET /auth/whoami` — returns the current principal (kind, email, role) and, for a session
  principal, its `csrf_token`, for the SPA.
- `POST /auth/logout` — revokes the current session, clears cookies.

All `returnTo`/RelayState values are validated **same-origin only** (open-redirect guard).

### 11.3 UI (`ui/`)

- `LoginPage.tsx` gains "Sign in with <IdP>" button(s) (redirect to the provider login
  endpoint) and, when LDAP is enabled, a username/password form. The existing API-key box is
  retained for power users. Method list comes from `/auth/status`.
- `lib/auth.ts` gains a **session mode**: when authenticated via cookie, it relies on the
  httpOnly cookie + `/auth/whoami` (rather than the in-memory Bearer key), caches the
  `csrf_token` returned by `/auth/whoami`, and sends it as `X-CSRF-Token` on unsafe requests.
  The existing in-memory API-key mode is preserved.
- `AuthGate.tsx` consults `/auth/whoami` to detect an established session in addition to the
  in-memory key.

### 11.4 GraphQL

GraphQL remains disabled while auth is enabled (today's behavior, `api/api.go:172`). Routing
it through the `Principal` middleware is noted as follow-up, out of v1 scope.

## 12. Security requirements (consolidated, non-optional)

- OIDC: PKCE + `state` + `nonce`; JWKS signature, `iss`/`aud`/`exp` validation.
- SAML: XML-dsig verification; audience/recipient/time-window checks with bounded clock
  skew; **dqlite-backed** assertion replay cache (not per-node in-memory); IdP metadata
  fetched only over **HTTPS with full TLS certificate verification**.
- LDAP: LDAPS/StartTLS required; reject anonymous/empty-password bind; TLS cert verification;
  **all user-supplied input escaped with `ldap.EscapeFilter` before filter substitution**.
- Sessions: opaque token hashed at rest; `HttpOnly`+`Secure`+`SameSite=Lax` cookie;
  session-fixation safe; idle + absolute expiry; revocation; **validation rejects sessions
  whose user is disabled (`users.disabled_at`)**.
- CSRF: **session-bound synchronizer token** (per-session `csrf_token` checked in-middleware
  against `X-CSRF-Token`) for cookie-authenticated unsafe methods — not a plain double-submit
  cookie (which is exposed to subdomain cookie injection).
- Open-redirect guard on all `returnTo`/RelayState.
- Reuse the existing per-IP **rate limiter** on credential/login endpoints and the **audit
  logger** (Actor becomes the user email/id).
- Constant-time comparison for tokens/CSRF (reuse `crypto/subtle`).

## 13. Distributed / dqlite considerations

- Sessions live in the shared dqlite catalog store → valid across all nodes; any node can
  validate a cookie. No sticky sessions required.
- **The read/write split is transparent** (`pkg/db` `installDqliteReadWriteSplit`, #188): it
  routes reads and writes at the connection-pool level, so session code uses a single
  `db.Connection()` handle and never picks a connection by hand. `users`/`sessions` are
  catalog-tier tables (not run-sharded hot tables).
- What we *do* implement is **write coalescing**: last-seen/idle bumps are buffered and
  flushed periodically (mirroring the API-key `last_used_at` flusher) to avoid per-request
  Raft writes.
- Clock skew: SAML/OIDC time-window checks use a small configurable leeway.

## 14. Observability

- New metrics paralleling the existing auth metrics: `sso_logins_total{provider,outcome}`,
  `sso_login_duration_seconds{provider}`, `sessions_active`, `session_validations_total{outcome}`.
- Audit actions: `auth.login`, `auth.logout`, `auth.session_revoked`, `user.provisioned`,
  `auth.login_denied` (with provider + reason). Actor = user email/id.

## 15. Testing strategy

- **Unit tests** beside code for: role mapping resolution, session lifecycle/expiry,
  principal resolution precedence, CSRF enforcement, cookie attributes, open-redirect guard.
- **Integration tests** under `test/` (`-tags=integration`), deterministic/hermetic:
  - OIDC against a mock OpenID provider (in-process or a test container).
  - SAML against a test IdP / static signed-assertion fixtures, including signature-failure
    and replay cases.
  - LDAP against a containerized OpenLDAP with seeded users/groups.
- Existing API-key middleware tests (`api/middleware/auth_test.go`,
  `internal/auth/service_test.go`) must stay green after the Phase 0 `Principal` refactor.

## 16. Phasing / milestones

All three providers ship in the **same release**; phases are independently
committable/testable stages (matching the project's incremental-stage workflow), not a
priority ordering.

- **P0 — Principal refactor.** Introduce `Principal`; route API-key auth through it; no
  behavior change; all existing tests green.
- **P1 — Identity + session foundation.** `users`/`sessions` migrations; session
  mint/validate/revoke + coalesced last-seen flusher; cookie + CSRF; `/auth/whoami`,
  `/auth/logout`; role-mapping resolver; provider interface + shared provision/authorize
  tail; `/auth/status` extension; UI login chrome.
- **P2 — OIDC provider.**
- **P3 — SAML provider.**
- **P4 — LDAP provider.**
  (P2–P4 are parallelizable; each is its own commit + integration test.)
- **P5 — Docs + hardening.** Operator SSO setup guide; end-to-end security pass.

## 17. Documentation deliverables

- This design doc.
- `docs/sso-authentication.md` — operator-facing setup guide for OIDC/SAML/LDAP, role
  mapping, session tuning, and TLS requirements (fills the current docs gap).
- README + roadmap updates noting SSO support.

## 18. Risks & mitigations

- **SAML correctness is the highest risk** (signature validation, replay, clock skew). Lean
  on `crewjam/saml`'s validated SP implementation; add explicit negative tests
  (tampered/expired/replayed assertions).
- **Trusting IdP groups** for privilege escalation: deny-by-default, validate role-mapping
  config at startup, log denied logins with the unmatched groups.
- **Cookie/CSRF gaps in a SPA**: httpOnly session cookie + **session-bound synchronizer CSRF
  token** (validated server-side; immune to subdomain cookie injection) + `SameSite=Lax`;
  Bearer path exempt and unchanged.
- **dqlite write contention from session activity**: coalesced last-seen flusher + read/write
  split, mirroring the proven API-key `last_used_at` approach.

## 19. Open questions / future work

- DB-backed/admin-UI role management (v1 is declarative config).
- Per-group job **scope** mapping (data model already supports it).
- CLI SSO (device-code / PKCE) issuing a session-scoped token.
- GraphQL under the `Principal` middleware.
- RP-initiated logout / SAML SLO (v1 does local logout only).
- **Cross-provider identity linking.** Identities are keyed on `(issuer, subject)`, so one
  person authenticating via two providers (e.g. OIDC *and* SAML) gets two `users` rows. v1
  does **not** auto-merge on `email`: email is not a trustworthy cross-provider join key
  (providers differ on email verification, and auto-merging would risk account takeover).
  Most deployments enable a single provider; operator-driven account linking is future work.
