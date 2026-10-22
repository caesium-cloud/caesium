# Identity and Access

> Status: Current operator guide for authentication, authorization, and tenancy.
> Paragraphs marked **Planned** name the item in
> [the identity-and-access plan](exec-plans/active/identity-and-access.md) that
> delivers them; they are not available yet.

Caesium supports API keys for machines and optional browser SSO for interactive
users. SSO is additive: keep `CAESIUM_AUTH_MODE=api-key` enabled and turn on one
or more providers with `CAESIUM_AUTH_OIDC_ENABLED`,
`CAESIUM_AUTH_SAML_ENABLED`, or `CAESIUM_AUTH_LDAP_ENABLED`. What each signed-in
user may do is decided by roles granted per namespace, either from an access
policy file (recommended) or from the legacy `CAESIUM_AUTH_ROLE_MAPPING`
variable.

Contents: [Overview and concepts](#overview-and-concepts) |
[Providers](#providers) | [Policy file](#policy-file) |
[Namespaces and isolation](#namespaces-and-isolation) |
[API keys and CLI login](#api-keys-and-cli-login) |
[Users and sessions](#users-and-sessions) |
[Audit and metrics](#audit-and-metrics) | [Security checks](#security-checks) |
[Migration from the env mapping](#migration-from-the-env-mapping)

## Overview and concepts

- **Role.** Roles form a ladder: `viewer < runner < operator < admin`.
- **Namespace.** A tenancy boundary that owns jobs and, through them, their
  runs, backfills and incidents. A namespace name is a DNS label
  (`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`). `default` always exists. `*` is
  reserved for cluster-wide grants and is never a real namespace. A job joins a
  namespace with `metadata.namespace`; omitting it means `default`. Namespaces
  are declared in the policy file's `namespaces:` map.
- **Grant.** A grant is a `(role, namespace)` pair; namespace `*` means
  cluster-wide. A principal carries a map from namespace to its highest role.
  The effective role in namespace N is `max(grants[N], grants["*"])`.
- **Principal.** The authenticated caller: an API key or an SSO user. The audit
  actor is the key prefix or the user's email. The cluster role (the grant at
  `*`) is what audit rows, metrics labels, and the `role` and `cluster_role`
  fields of `GET /auth/whoami` report; whoami's `grants` and `namespaces`
  fields list every grant.
- **Tenancy namespace versus dataset namespace.** These are two unrelated
  axes. The tenancy namespace described in this guide decides which jobs, and
  which objects that belong to them, a principal may see and change. The
  dataset namespace is the OpenLineage namespace carried by dataset
  declarations, dataset metrics, and the `/v1/datasets/:ns/:name` path; it is
  reserved for cross-instance datasets and has nothing to do with tenancy or
  grants. A dataset's tenancy namespaces are those of the jobs that declare
  they produce it (`datasets.produces`), read at request time, so a moved
  producer takes the dataset with it. A consumer or a `sources` declaration
  owns nothing, and a dataset with no declared producer is visible only to
  unconfined principals. See [Datasets](#datasets). Everywhere else this guide
  says "namespace" for the tenancy axis.
- **Job alias.** `metadata.alias` keeps its global unique index. Every
  alias-addressed surface (CLI verbs, key job scopes, lineage, receipts) is
  unchanged by namespaces.

With no policy file every principal holds its role at `*`, so authorization
behaves exactly as before namespaces existed. Namespaces themselves still take
effect without a file: a manifest may set any DNS-label `metadata.namespace`,
and the step cache key, incident deduplication, the git-sync `namespaces`
allowlist and the shared-object reference rules all use it. Without a policy
file nothing checks a manifest's namespace against a declared list, so a typo
creates a new namespace; with one, undeclared namespaces are rejected (see
[Namespaces on jobs](#namespaces-on-jobs)).

## Providers

### Common settings

All SSO providers use the same server-side sessions and role mapper.

```sh
CAESIUM_AUTH_MODE=api-key
CAESIUM_AUTH_KEY_HASH_SECRET=<32+ byte random secret>
CAESIUM_AUTH_REQUIRE_TLS=true
CAESIUM_AUTH_PUBLIC_BASE_URL=https://caesium.example.com
CAESIUM_AUTH_ROLE_MAPPING='CN=Caesium Admins,OU=Groups,DC=example,DC=com=admin;data-eng=operator;job-runners=runner;*=viewer'
CAESIUM_AUTH_SESSION_IDLE_TTL=8h
CAESIUM_AUTH_SESSION_ABSOLUTE_TTL=24h
CAESIUM_AUTH_SESSION_COOKIE_NAME=caesium_session
CAESIUM_AUTH_DEFAULT_ROLE=
```

`CAESIUM_AUTH_ROLE_MAPPING` is a semicolon-separated list of `group=role`
entries. Caesium chooses the highest mapped role when a user belongs to several
groups. Valid roles are `viewer`, `runner`, `operator`, and `admin`. Use `*`
only when a catch-all login policy is intended.

`CAESIUM_AUTH_ROLE_MAPPING` is deprecated in favour of the [policy file](#policy-file).
When `CAESIUM_AUTH_POLICY_FILE` is set, the mapping must be unset; see [Migration
from the env mapping](#migration-from-the-env-mapping).

If no group mapping matches, an empty `CAESIUM_AUTH_DEFAULT_ROLE` denies login.
Set it to `viewer`, `runner`, `operator`, or `admin` only when unmapped SSO
users should receive that fallback role.

Caesium stores each user's groups at login and resolves the role again on every
request from those stored groups. A mapping changed across a restart therefore
applies to existing sessions without a new login; a user's group changes still
take effect at their next login.

Disabling SSO does not end existing browser sessions. With a policy file in
`api-key` mode the file is the grant source whether or not an SSO provider is
enabled, so a leftover unexpired cookie session keeps resolving grants from its
stored groups. Even without a file, a session with no grants is still accepted
on the identity routes (`GET /auth/whoami`, `POST /auth/logout`,
`GET /v1/system/features`; `whoami` reports an empty role) and refused
elsewhere. A session ends at logout, at its idle or absolute TTL, or when a
cluster admin revokes it: `caesium auth user logout USER_ID` revokes one user's
sessions (and [user-bound keys](#user-bound-keys)) and
`caesium auth user disable USER_ID` also blocks the user's next login (see [User administration](#user-administration)). There is no single
"revoke every session" call; removing a user's bindings from the policy file
also cuts them off at once (grants are resolved on every request).

The browser session cookie is HTTP-only and `SameSite=Lax`. Unsafe requests from
cookie sessions must include the CSRF token surfaced by `GET /auth/whoami`.
`CAESIUM_AUTH_SESSION_COOKIE_NAME` changes the cookie name; use it when multiple
Caesium deployments share a parent domain.

If Caesium is served behind a TLS-terminating proxy, set
`CAESIUM_TRUSTED_PROXIES` to a comma-separated list of immediate proxy IPs or
CIDRs. Caesium trusts `X-Forwarded-Proto: https` only from those peers; that
controls both `Secure` session cookies and same-origin redirect checks. Leave
`CAESIUM_AUTH_REQUIRE_TLS=true` in production so auth startup requires either
direct TLS certs or a trusted proxy path. Trusted proxy entries must be valid
IP addresses or CIDR ranges; invalid entries fail the auth startup TLS guard.

#### Common Operator Reference

| Env | Default | Operator note |
| --- | --- | --- |
| `CAESIUM_AUTH_PUBLIC_BASE_URL` | unset | External base URL used to derive OIDC redirect and SAML ACS/metadata URLs. |
| `CAESIUM_AUTH_DEFAULT_ROLE` | empty | Fallback role when no group mapping matches; empty denies login. |
| `CAESIUM_AUTH_SESSION_COOKIE_NAME` | `caesium_session` | Browser session cookie name for SSO callbacks, `/auth/whoami`, and logout. |
| `CAESIUM_AUTH_SESSION_IDLE_TTL` | `8h` | Sliding idle timeout refreshed by valid session use. |
| `CAESIUM_AUTH_SESSION_ABSOLUTE_TTL` | `24h` | Hard session expiry. |
| `CAESIUM_AUTH_CLI_KEY_TTL` | `12h` | Lifetime of a [user-bound API key](#user-bound-keys); must be above 0 and at most `168h` (7 days), or the server refuses to start. |
| `CAESIUM_TRUSTED_PROXIES` | empty | Comma-separated trusted proxy IPs/CIDRs for forwarded-proto handling. |

### Browser Auth Endpoint Contract

Browser clients discover available auth paths with `GET /auth/status`. The
response is:

```json
{
  "enabled": true,
  "methods": [
    { "type": "api-key" },
    {
      "type": "oidc",
      "id": "oidc",
      "label": "Sign in with OIDC",
      "loginUrl": "/auth/sso/oidc/login",
      "mode": "redirect"
    }
  ]
}
```

SSO method objects include `type`, stable `id`, display `label`, same-origin
`loginUrl`, and `mode`. `mode` is `redirect` for OIDC and SAML, and
`credential` for LDAP. API-key auth is advertised as `{ "type": "api-key" }`
when `CAESIUM_AUTH_MODE=api-key` is enabled.

`GET /auth/whoami` requires a valid API key or session cookie. It returns the
principal: `kind`, `subject`, `email` (users), `role` and `cluster_role` (the
role at `*`, empty for a principal confined to namespaces), `grants` (namespace
to role, `*` for cluster-wide) and `namespaces` (the sorted namespaces it can
read, `["*"]` with a cluster grant, `[]` with none). A job-scoped key also
gets `scope`. Cookie-session responses include `csrf_token`; browser clients
should keep it in memory and send it as `X-CSRF-Token` on unsafe cookie-session
requests.

`POST /auth/logout` revokes the current cookie session and clears the session
cookie. Because logout is an unsafe cookie-session request, browser callers
must include `X-CSRF-Token` with the value returned by `/auth/whoami`.
Bearer API-key requests do not require CSRF, and logout does not revoke API
keys; use the API-key revoke endpoints for key lifecycle.

The operator console sign-out control uses this endpoint and clears local
in-memory auth state even if the server cannot be reached.

API keys and browser SSO can coexist. When both are enabled, `/auth/status`
lists both the API-key method and the configured SSO methods. Requests with an
`Authorization: Bearer ...` header are evaluated as API-key requests before any
session cookie; an invalid or malformed bearer header fails instead of falling
back to the cookie session.

### OIDC

```sh
CAESIUM_AUTH_OIDC_ENABLED=true
CAESIUM_AUTH_OIDC_ISSUER_URL=https://idp.example.com
CAESIUM_AUTH_OIDC_CLIENT_ID=caesium
CAESIUM_AUTH_OIDC_CLIENT_SECRET=<client secret>
CAESIUM_AUTH_OIDC_SCOPES='openid profile email groups'
CAESIUM_AUTH_OIDC_GROUPS_CLAIM=groups
```

If `CAESIUM_AUTH_OIDC_REDIRECT_URL` is unset, Caesium derives
`/auth/sso/oidc/callback` from `CAESIUM_AUTH_PUBLIC_BASE_URL`.

Each OIDC login stores the identity provider's refresh token (when the IdP
issues one) for that user, sealed with AES-256-GCM under a key derived from
`CAESIUM_AUTH_KEY_HASH_SECRET`; no grant is stored when that secret is empty.
LDAP and SAML logins store none. Nothing redeems the grant yet: the refresher is
planned (E2), so group changes still apply at the next login.

**Caution (applies once group refresh, E2, lands):** a grant is bound to the
client ID and issuer that minted it. After a client ID change the IdP is
expected to answer `invalid_grant` for the old tokens, which ends the affected
sessions. After an issuer change the old grants are not sent to the IdP at all;
the refresh fails as transient, and those sessions end only when the staleness
bound forces a re-login. Plan a re-login for all users when you change either.

### Keycloak

Keycloak is an OIDC issuer like any other; this is the setup Caesium is tested
against. The Keycloak CI lane (`just integration-test-keycloak`, see the
[CI runbook](ci.md)) imports `test/testdata/keycloak/caesium-realm.json`, a
working example of every step below.

1. **Client.** Create an OpenID Connect client (for example `caesium`) with
   *Client authentication* on (a confidential client), *Standard flow* on and
   *Implicit flow* off. Set *Valid redirect URIs* to exactly
   `<CAESIUM_AUTH_PUBLIC_BASE_URL>/auth/sso/oidc/callback` and *Web origins*
   to the public base URL. Caesium always sends a PKCE `S256` challenge, so
   you can require it (*PKCE method* `S256` in the client's advanced
   settings). Leave *Direct access grants* off; the CI lane enables it only for
   its test harness. Put the client secret in `CAESIUM_AUTH_OIDC_CLIENT_SECRET`.
2. **Groups scope and mapper.** Create a client scope named `groups` (OpenID
   Connect) with a *Group Membership* mapper: token claim name `groups`, *Full
   group path* off, *Add to ID token* on. Caesium reads groups from the ID
   token. Assign the scope to the client as *Optional* (Caesium requests it)
   or *Default*. With *Full group path* off a binding names the group
   (`marketing-eng`); with it on, the binding must name the path
   (`/marketing-eng`). `CAESIUM_AUTH_OIDC_SCOPES` (default
   `openid profile email groups`) must include the scope's name, and every
   scope it names must be assigned to the client: Keycloak answers a request
   for a scope the client does not have with `invalid_scope`, and the login
   fails at Caesium's callback.
3. **Email.** `users:` bindings match only an email Keycloak reports as
   verified (`email_verified`); an unverified email never satisfies one, and
   that user resolves through groups alone. Mark addresses verified (or turn on
   the realm's *Verify email*) before relying on email bindings.
4. **Refresh tokens.** Keycloak issues a refresh token with every code-flow
   login by default (no `offline_access` scope needed), and Caesium stores it
   sealed (see [OIDC](#oidc)). Its lifetime is bounded by the realm's *SSO
   Session Idle* and *SSO Session Max*. **Planned (E2):** once group refresh
   redeems it during a session, those two settings bound how long group
   changes are picked up before a re-login is required.
5. **Issuer.** Set `CAESIUM_AUTH_OIDC_ISSUER_URL` to the realm URL,
   `https://kc.example.com/realms/<realm>` (no `/auth` prefix unless Keycloak
   sets `http-relative-path`). Caesium reads
   `<issuer>/.well-known/openid-configuration` at startup and does not start
   if discovery fails or the document's `issuer` differs from that URL.
   Keycloak derives the issuer from its `hostname` option (from each request
   when unset), so pin `hostname` to the one URL that both browsers and
   Caesium use. Keep the realm's *Require SSL* at `external` or `all` and
   serve both over HTTPS; the CI lane uses `none` over loopback HTTP only.

```sh
CAESIUM_AUTH_MODE=api-key
CAESIUM_AUTH_PUBLIC_BASE_URL=https://caesium.example.com
CAESIUM_AUTH_OIDC_ENABLED=true
CAESIUM_AUTH_OIDC_ISSUER_URL=https://kc.example.com/realms/acme
CAESIUM_AUTH_OIDC_CLIENT_ID=caesium
CAESIUM_AUTH_OIDC_CLIENT_SECRET=<client secret>
# CAESIUM_AUTH_OIDC_SCOPES and CAESIUM_AUTH_OIDC_GROUPS_CLAIM: the defaults
# match the client scope above.
```

### SAML

```sh
CAESIUM_AUTH_SAML_ENABLED=true
CAESIUM_AUTH_SAML_IDP_METADATA_URL=https://idp.example.com/metadata
CAESIUM_AUTH_SAML_SP_CERT=/etc/caesium/saml/sp.crt
CAESIUM_AUTH_SAML_SP_KEY=/etc/caesium/saml/sp.key
CAESIUM_AUTH_SAML_SP_ENTITY_ID=https://caesium.example.com/auth/sso/saml/metadata
CAESIUM_AUTH_SAML_ACS_URL=https://caesium.example.com/auth/sso/saml/acs
CAESIUM_AUTH_SAML_METADATA_URL=https://caesium.example.com/auth/sso/saml/metadata
CAESIUM_AUTH_SAML_GROUPS_ATTRIBUTE=groups
```

Configure exactly one IdP metadata source:
`CAESIUM_AUTH_SAML_IDP_METADATA_URL`, `CAESIUM_AUTH_SAML_IDP_METADATA_XML`, or
`CAESIUM_AUTH_SAML_IDP_METADATA_FILE`. Metadata URLs must use HTTPS.

If `CAESIUM_AUTH_SAML_ACS_URL` or `CAESIUM_AUTH_SAML_METADATA_URL` is unset,
Caesium derives `/auth/sso/saml/acs` and `/auth/sso/saml/metadata` from
`CAESIUM_AUTH_PUBLIC_BASE_URL`. If `CAESIUM_AUTH_SAML_SP_ENTITY_ID` is unset,
it defaults to the metadata URL. Configure those same SP values in the IdP so
audience, recipient, and metadata checks match.

### LDAP

LDAP uses a search-then-bind flow: Caesium binds with a service account, searches
for one matching user, binds as that user's full DN with the submitted password,
then re-binds with the service account to resolve groups.

```sh
CAESIUM_AUTH_LDAP_ENABLED=true
CAESIUM_AUTH_LDAP_URL=ldaps://ldap.example.com:636
CAESIUM_AUTH_LDAP_BIND_DN='cn=caesium,ou=svc,dc=example,dc=com'
CAESIUM_AUTH_LDAP_BIND_PASSWORD=<service account password>
CAESIUM_AUTH_LDAP_USER_BASE_DN='ou=users,dc=example,dc=com'
CAESIUM_AUTH_LDAP_USER_FILTER='(uid={username})'
CAESIUM_AUTH_LDAP_GROUP_BASE_DN='ou=groups,dc=example,dc=com'
CAESIUM_AUTH_LDAP_GROUP_FILTER='(member={dn})'
CAESIUM_AUTH_LDAP_GROUP_ATTRIBUTE=cn
CAESIUM_AUTH_LDAP_TIMEOUT=10s
CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE=uid
CAESIUM_AUTH_LDAP_EMAIL_ATTRIBUTE=mail
CAESIUM_AUTH_LDAP_DISPLAY_NAME_ATTRIBUTE=displayName
```

Use `ldaps://` where possible. To use StartTLS on `ldap://`, set
`CAESIUM_AUTH_LDAP_START_TLS=true`. TLS certificate verification is enabled by
default through Go's system trust store.

Group lookup is optional. If `CAESIUM_AUTH_LDAP_GROUP_BASE_DN` is set and
`CAESIUM_AUTH_LDAP_GROUP_FILTER` is omitted, Caesium uses `(member={dn})`.

Filter placeholders are escaped before substitution. The user filter replaces
only `{username}`, the submitted username. The group filter supports `{dn}`, the
resolved user DN, and `{username}`, which there is the value of the user entry's
`CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE` (the typed name only when that attribute
is missing at login). For Active Directory, common filters are:

```sh
CAESIUM_AUTH_LDAP_USER_FILTER='(&(sAMAccountName={username})(!(userAccountControl:1.2.840.113556.1.4.803:=2)))'
CAESIUM_AUTH_LDAP_GROUP_FILTER='(member={dn})'
CAESIUM_AUTH_LDAP_GROUP_ATTRIBUTE=dn
CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE=sAMAccountName
CAESIUM_AUTH_LDAP_EMAIL_ATTRIBUTE=userPrincipalName
CAESIUM_AUTH_LDAP_DISPLAY_NAME_ATTRIBUTE=cn
```

Keep the disabled-account clause
`(!(userAccountControl:1.2.840.113556.1.4.803:=2))` in Active Directory user
filters. Once LDAP group refresh lands (planned, E1b/E2), re-checking the login
filter against the user's directory entry still cannot observe AD rejecting a
disabled account's own bind, so the filter is what excludes disabled accounts.

LDAP attribute and timeout reference:

| Env | Default | Operator note |
| --- | --- | --- |
| `CAESIUM_AUTH_LDAP_TIMEOUT` | `10s` | Dial, bind, and search timeout; keep below your load balancer timeout. |
| `CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE` | `uid` | Attribute whose value fills `{username}` in the group filter, and the display-name fallback. |
| `CAESIUM_AUTH_LDAP_EMAIL_ATTRIBUTE` | `mail` | Attribute used for the Caesium user email. |
| `CAESIUM_AUTH_LDAP_DISPLAY_NAME_ATTRIBUTE` | `displayName` | Attribute used for the display name. |
| `CAESIUM_AUTH_LDAP_GROUP_ATTRIBUTE` | `cn` | Group values matched against the policy file's `groups:` entries (or `CAESIUM_AUTH_ROLE_MAPPING` when no file is set); use `dn` for full-DN entries. |

Empty passwords are rejected before any LDAP bind so directories that permit
anonymous binds cannot accidentally authenticate a user.

The LDAP integration test can be pointed at a seeded OpenLDAP or Active
Directory-compatible test directory. It is behind the `integration` build tag,
is skipped unless `CAESIUM_TEST_LDAP_*` variables are set, and should be run in
the repo builder image:

```sh
docker run --rm --platform linux/arm64 \
  -v "$PWD":/bld/caesium -w /bld/caesium \
  caesiumcloud/caesium-builder:latest-full \
  sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./internal/auth/ldap -tags=integration -run TestProviderAuthenticateIntegration -v'
```

Set `CAESIUM_TEST_LDAP_EXPECTED_GROUPS`, `CAESIUM_TEST_LDAP_ROLE_MAPPING`, and
`CAESIUM_TEST_LDAP_EXPECTED_ROLE` to make the test assert group extraction and
role resolution against the seeded directory.

The same package also includes a self-contained OpenLDAP fixture. It starts an
`osixia/openldap:1.5.0` container seeded from
`internal/auth/ldap/testdata/openldap/bootstrap.ldif`, skips cleanly when Docker
is unavailable, and can be run with Docker socket access:

```sh
docker run --rm --platform linux/arm64 \
  -v "$PWD":/bld/caesium \
  -v "$HOME/.docker/run/docker.sock":/var/run/docker.sock \
  -w /bld/caesium caesiumcloud/caesium-builder:latest-full \
  sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./internal/auth/ldap -tags=integration -run TestProviderAuthenticateOpenLDAPFixture -v'
```

## Policy file

The access policy file declares namespaces and binds SSO groups and users to
roles per namespace. It is optional; without it Caesium translates
`CAESIUM_AUTH_ROLE_MAPPING` (see [Migration from the env
mapping](#migration-from-the-env-mapping)).

### Schema

```yaml
apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  marketing:
    kubernetes:                       # enforced; see Kubernetes target
      namespace: caesium-marketing
      serviceAccountName: caesium-jobs
    secrets:                          # enforced; see Scoped secrets
      allow:
        - vault/secret/data/marketing/*
        - k8s/marketing-*
        - env/MARKETING_*
    quotas:                           # at most 8 marketing runs running at once
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

The `kubernetes` block ([Kubernetes target](#kubernetes-target)), the
`secrets` rules ([Scoped secrets](#scoped-secrets)) and `quotas` ([Run
quotas](#run-quotas)) are enforced.

### Validation rules

The loader (`internal/auth/policy`) and `caesium auth policy lint` apply the
same rules. Any violation is a fatal startup error, or a rejected reload.

- The file must be a single YAML mapping with `apiVersion: v1` and
  `kind: AccessPolicy`. Unknown fields, duplicate keys, YAML merge keys and
  cyclic aliases are rejected.
- Namespace keys must be DNS labels; `*` is rejected as a key. `default` may be
  declared to attach settings and always exists.
- `bindings[].role` must be one of `viewer`, `runner`, `operator`, `admin`.
- `bindings[].namespaces` must be non-empty, and each entry must be `*` or a
  declared namespace.
- `bindings[].subjects` needs at least one of `groups` or `users`, and no entry
  may be empty.
- `groups` entries are exact strings, including whitespace and case. LDAP DNs
  containing `=` and `,` must be quoted in a flow sequence
  (`groups: ["CN=Admins,OU=Groups,DC=example,DC=com"]`); unquoted, the commas
  split the DN into separate groups that never match. Block style entries do not
  need quotes. The group `"*"` matches every authenticated user.
- `users` entries are compared with the user's email by folding only ASCII
  `A`-`Z` to lowercase. Non-ASCII bytes stay exact and neither side is trimmed.
- `kubernetes.namespace` must be a DNS label and
  `kubernetes.serviceAccountName` a DNS subdomain; `quotas.maxConcurrentRuns`
  must not be negative.
- `secrets.allow` entries are `provider/path` globs: `*` matches within one path
  segment and `**` across segments. Literal `.` and `..` segments are rejected.
  The provider must be `env`, `k8s` (alias `kubernetes`) or `vault`; any other
  provider is an error. Vault rules select the secret path and allow every field
  at that path; `#` is reserved as the field delimiter and `#field` patterns are
  unsupported.

Non-fatal warnings: a namespace other than `default` that has no `secrets`
rules; user bindings when no enabled provider supplies a trusted email (only
OIDC logins with `email_verified=true` match user bindings; SAML and LDAP email
is untrusted); and, server-side, binding groups that no stored user holds
(`"*"` is ignored; a database error skips this check and never fails a load).

### Resolution

On every request Caesium computes grants from the user's stored groups and
email; nothing about grants is stored on the user or session.

1. For each binding whose subjects match (an exact group, the `"*"` group, or a
   trusted email), each of its namespaces gets `max(current, role)`.
2. If nothing matched, the login is denied (`auth.login_denied`, reason
   `no_binding`) unless `CAESIUM_AUTH_DEFAULT_ROLE` is set, in which case that
   role is granted at `*`. The default role never adds to a matched result.
3. A user's email counts only when the provider vouched for it: OIDC with
   `email_verified` true (or the string `"true"`). SAML and LDAP email is not
   trusted in this release, so a `users:` entry never matches those logins.

`User.Role` is stored as the cluster role at last login. It is not read for authorization; today it appears only in the metadata of `auth.login` and `user.provisioned` audit rows.

### Loading and reload

| Variable | Default | Meaning |
| --- | --- | --- |
| `CAESIUM_AUTH_POLICY_FILE` | unset | Path of the access policy file. Unset keeps the `CAESIUM_AUTH_ROLE_MAPPING` translation. |
| `CAESIUM_AUTH_POLICY_RELOAD_INTERVAL` | `10s` | How often each node re-reads and hashes its file. With a file set, `<= 0` is a startup configuration error. Ignored when no file is set. |

- **Startup.** The file is read (regular files only, at most 1 MiB), parsed and
  fully validated. A missing, unreadable, non-regular, oversize or invalid file
  is fatal (`invalid CAESIUM_AUTH_POLICY_FILE`, naming the path).
- **Conflicts.** Setting a file together with a non-blank
  `CAESIUM_AUTH_ROLE_MAPPING` fails startup with an error naming both.
- **Default role.** With a file, `CAESIUM_AUTH_DEFAULT_ROLE` is granted at `*`
  only when no binding matches, and an invalid value fails startup.
- **Auth modes.** In `api-key` mode the file is the grant source for sessions
  and SSO logins, with or without an SSO provider. In `none` mode the file is
  still validated at startup and polled, but no RBAC applies.
- **Reload.** A poller re-reads and hashes the file every
  `CAESIUM_AUTH_POLICY_RELOAD_INTERVAL` (content hash, not modification time, so
  Kubernetes ConfigMap symlink swaps are seen). A poll-detected change is
  published only once it is stable (the same hash on two consecutive polls with no
  write observed between them, so a half-written in-place rewrite is never
  published), so a file edit takes effect within about two reload intervals.
  `POST /v1/auth/policy/reload` (and `caesium auth policy reload`) reloads
  immediately. Both share one lock, and
  readers never block. A valid new revision is swapped in atomically and audited
  as `policy.reloaded`. An invalid or unreadable file keeps the last good
  revision, logs at error (never the file contents), increments
  `caesium_auth_policy_reload_failures_total{trigger}` and audits
  `policy.reload_failed`. The poller reports a given bad file once, not once per
  interval, and stops with the server.
- **Distributed mode.** Each node polls its own copy of the file, and
  `POST /v1/auth/policy/reload` reloads only the node that served the request.
  Compare `revision` across nodes to spot drift.

### REST API

Both routes require a cluster admin (401 without credentials, 403 below admin). They are mounted only when authentication is on: with `CAESIUM_AUTH_MODE=none` they answer 404 and `caesium auth policy show` and `reload` fail, so the poller is the only reload path there.

**`GET /v1/auth/policy`** returns the effective policy:

```json
{
  "source": "file",
  "path": "/etc/caesium/policy.yaml",
  "revision": "<hex sha256 of the file bytes>",
  "loaded_at": "2026-10-15T10:00:00Z",
  "default_role": "viewer",
  "warnings": ["..."],
  "last_reload": {
    "at": "...", "trigger": "poll", "ok": false, "changed": false,
    "revision": "<hash of the bytes attempted; omitted if unreadable>",
    "error": "access policy file /etc/caesium/policy.yaml: ..."
  },
  "policy": {"apiVersion": "v1", "kind": "AccessPolicy", "namespaces": {}, "bindings": []}
}
```

`source` is `file` or `env`; `path` is omitted for `env`; `revision` is empty
for `env`; `default_role` is empty when unset; `last_reload` is `null` before
the first attempt and `trigger` is `poll` or `api`. With no file, `source` is
`env` and `policy` is the translated `CAESIUM_AUTH_ROLE_MAPPING` (cluster-wide
bindings, `default_role` = `CAESIUM_AUTH_DEFAULT_ROLE`) when SSO is enabled in
`api-key` mode, otherwise the empty single-`default` policy. The raw file is
never returned.

**`POST /v1/auth/policy/reload`** is synchronous:

- `200` `{"changed": bool, "revision": "...", "previous_revision": "...", "loaded_at": "...", "warnings": [...]}`.
  An unchanged reload (`changed: false`) is still audited and refreshes the
  known-group warnings.
- `422` `{"message": "access policy file <path>: <validation or read error>", "revision": "<retained revision>"}`
  for an invalid, missing, unreadable, non-regular or oversize file. The retained
  revision stays in force.
- `409` `{"message": "no access policy file is configured (CAESIUM_AUTH_POLICY_FILE is unset)"}`
  with no file. Nothing is audited.

**`GET /v1/auth/namespaces`** (cluster viewer, so the Console can show it to
everyone who may read the cluster) returns the declared namespaces without any
identity data:

```json
{
  "source": "file", "revision": "<hex sha-256>", "loaded_at": "...", "last_reload": null,
  "namespaces": [
    {"name": "default", "secrets": null},
    {"name": "marketing", "secrets": {"allow": ["env/MARKETING_*"]}, "quotas": {"maxConcurrentRuns": 1}}
  ]
}
```

`default` is always listed first, then the rest sorted by name. `secrets` is
`null` when the namespace allows every secret. The response never carries
bindings, subjects, groups, emails or the file path. A namespace-confined key
and a job-scoped key get `403` (cluster route); with authentication disabled the
route does not exist.

### CLI

- `caesium auth policy lint --path FILE [--json]` validates a file offline,
  with no server. It exits 1 on an invalid file and prints warnings to stderr.
  `--json` prints exactly one document on stdout for valid and invalid files:
  `{"path","valid","revision"?,"error"?,"warnings":[],"namespaces":[],"bindings":N}`
  (`revision` is omitted when the file cannot be read, is not a regular file, or exceeds 1 MiB).
  Offline warnings are the namespace-without-secrets rule and a note on each
  user binding; the known-group warning comes only from the server.
- `caesium auth policy show [--json] [--server URL] [--api-key KEY]` calls
  `GET /v1/auth/policy`. The default output is a human summary; `--json` prints
  the response.
- `caesium auth policy reload [--json] [--server URL] [--api-key KEY]` calls
  `POST /v1/auth/policy/reload`. It exits 1 when the file is rejected (the error
  names the retained revision); `--json` prints the 200 body, or the 422 body
  before exiting 1.
- `--server` defaults to `http://localhost:8080`. The key comes from
  `CAESIUM_API_KEY` (preferred) or `--api-key`.

## Namespaces and isolation

### Namespaces on jobs

Set `metadata.namespace` in the job manifest (`caesium job schema` documents the
field). It defaults to `default` and must be a DNS label.

While a policy file is loaded, the namespace must also be declared in it
(`default` always is). Server lint (`caesium job lint --server`,
`POST /v1/jobdefs/lint`) reports every undeclared namespace as
`metadata.namespace "x" of job "y" is not declared in the access policy`, and
apply (`caesium job apply`, `POST /v1/jobdefs/apply`), the legacy
`POST /v1/jobs`, git sync and agent remediation patches refuse the whole batch
with that message before writing anything. The check reads the policy in force
at the time, so removing a namespace from the file takes effect at the next
apply; its existing jobs keep running. A git-sync source that still holds
manifests in a removed namespace fails its next sync, which today stops the
server like any invalid batch (a known follow-up makes such failures
non-fatal). Offline `caesium job lint` checks syntax only.

No policy file: any syntactically valid namespace is accepted (B1
compatibility); undeclared namespaces are rejected only when a policy file is
loaded. Amended in identity-and-access W4 (orchestrator decision, pending
maintainer sign-off).

**Canonical aliases.** Job aliases are matched exactly and are case-sensitive.
Every writer (apply, the legacy `POST /v1/jobs`, git sync, `caesium dev`, lint)
refuses an alias with leading or trailing whitespace, Unicode spaces included,
with `400 metadata.alias "payroll " must not have leading or trailing
whitespace`, so a second job differing only by whitespace can no longer be
created. Git sync skips such a manifest on its own (reported with reason
`alias_not_canonical`) and syncs the rest; the alias stays protected from the
source's prune. A job stored under a non-canonical alias before this rule is
not migrated: re-applying its manifest is refused and changes nothing. To
recover, retire the old job first with `DELETE /v1/jobs/:id` (its own ID, from
`GET /v1/jobs`), then apply the corrected manifest: it updates a job that
already holds the canonical alias, if one exists, and creates it otherwise.
Retire first because dataset and contract validation compare aliases with
whitespace trimmed, so while both are active they cannot tell the two jobs
apart. A git source holding both spellings fails its sync as a duplicate
alias, as it did before this rule.

- Runs, backfills and incidents carry a copy of the job's namespace when they
  are created, so global lists can filter without a join. Triggers, task runs,
  events and lineage rows derive it through their job. Run-scoped data
  (receipts, task descriptors and partitions, logs) is authorised on both the
  job's current namespace and the run's persisted namespace.
- Moving a job is an apply that changes `metadata.namespace`. History (runs,
  receipts, incidents) keeps its original namespace. `caesium job diff` shows the
  change as a metadata diff and lists both namespaces under
  `Namespaces touched` (`namespaces` in `--json` and in the
  `POST /v1/jobdefs/diff` response; `--prune` adds the namespaces of the jobs
  it would delete).
- The namespace is part of the step cache key, so a move invalidates cached
  step outputs; results produced under one namespace are never republished under
  another. A replay of a moved job re-executes every step instead of reusing
  cached output, and fails closed, naming the old and new namespaces, when a
  step that must re-execute is not replay-safe.
- Incident deduplication includes the run's namespace, so a failure after a
  move opens a new incident in the new namespace instead of folding into an open
  one from the old owner.
- A moved job's history stays with its old owner. Reads under the job are
  authorised on its current namespace, then each run, backfill and cache entry
  is shown only to callers who may read the namespace it was written in (see
  [History under a job](#collections-and-history-under-a-job)). A retry of a run
  left in the previous namespace answers `409 namespace_moved` for every caller,
  because it would execute the current job model inside the old owner's run;
  replay the run instead. A backfill counts only runs in its own namespace when
  it skips dates, and stops as failed if the job moves while it runs. A
  `deltaFromBaseline` data assertion is judged only against samples of runs
  in the evaluated run's namespace, so after a move it starts from no
  baseline instead of reading (and recording) the old owner's. Such verdicts
  carry `baseline_scope: namespace`; a confined caller reading a verdict
  recorded without it (before this release) gets it without `baseline_median`,
  `baseline_samples`, the derived `bound` and the message that renders them,
  on run reads, the event stream, lifecycle event history, an incident list's
  `last_error`, and a fanned step's `why` group (`firstFailure.error` and
  `summary`).
- DAG snapshots (the topology history behind `caesium blame`,
  `GET /v1/jobs/:id/topology` and `topology/history`) record the namespace
  they were written in. A move writes a fresh snapshot in the new namespace
  even when the topology is unchanged, so the new owner's topology and blame
  start at the move and the old owner's history stays with it. Snapshots
  written before this release record no namespace and are shown only to
  unconfined callers; the first apply of each existing job after the upgrade
  writes one namespaced snapshot (same content) so its current owner sees its
  topology.

### Git sync allowlist

Each entry of the `CAESIUM_JOBDEF_GIT_SOURCES` JSON array accepts an optional
`"namespaces": ["a", "b"]` field. A source with the field may only apply
manifests whose `metadata.namespace` is listed. Violations are reported per
manifest and skipped, and each applied checkout (a new commit; an unchanged HEAD or a failed
fetch writes nothing) writes one `jobdef.git_sync` audit row with
actor `system:git-sync` and the redacted source URL. A source without the field
is unrestricted and writes no such row.

- The boundary is enforced inside the importer transactions: a source-owned job
  whose stored or requested namespace is outside the allowlist is refused, so a
  narrowed source can neither move a job into its allowlist nor out of it.
  Pruning removes only jobs whose namespace is listed and reports the rest.
  A refused manifest keeps its alias protected from pruning, so a forbidden move
  leaves the existing job active and unchanged.
- An allowlisted source applies a commit's allowed manifests in one transaction.
  A namespace refusal skips only the refused manifest, and so does a
  [trigger tenancy](#confined-job-writes-and-trigger-tenancy) refusal (a
  manifest whose trigger or webhook path also serves a job outside the
  allowlist); a skipped manifest writes nothing. If the stored job graph
  (trigger chains, dataset producers) would be invalid with that job left in
  place, or any other error occurs, nothing from the commit is applied and the
  sync fails.
- With git sync enabled, startup rejects an empty list (`[]`), `""`, `"*"`,
  non-DNS labels, an allowlist without a `source_id`, and a `source_id` shared
  with a restricted source. Parsing rejects `"namespaces": null`, a singular
  `"namespace"` key and duplicate case-variant keys.

### Confined job writes and trigger tenancy

A principal confined to some namespaces, and a job-scoped API key (even one
holding its role at `*`), can create, apply, move and prune jobs only within
its reach. A job-scoped key writes only the exact aliases its scope lists (an
alias with leading or trailing whitespace is refused with 400 for every writer,
see [Canonical aliases](#namespaces-on-jobs)). In detail:

- **Apply** (`caesium job apply`, `POST /v1/jobdefs/apply`) needs operator in
  every definition's namespace and in the stored namespace of every alias it
  names, so moving a job needs operator in both namespaces. For such a writer
  every definition and the prune run in one transaction: a refusal anywhere,
  including one only the transaction can see (a job created or moved in
  another namespace since the request was authorised), is a 403 naming the
  namespace the caller named and applies nothing. On PostgreSQL that
  transaction runs at `REPEATABLE READ` after taking an advisory lock per alias
  and webhook path, so two writers racing for one alias or path are
  serialised and the second is refused cleanly. A principal whose role holds
  at `*` with an unscoped key keeps the previous per-definition apply.
- **Prune** by a namespace-confined caller retires only the absent jobs in the
  namespaces where it is operator; jobs elsewhere are never candidates and
  are not reported. Job-scoped keys cannot prune. Pruning a job keeps its
  trigger when another live job still uses it.
- **The legacy `POST /v1/jobs`** is namespaced on the body's
  `metadata.namespace` (default `default`) and the stored namespace of its
  alias.
- **`PATCH /v1/triggers/:id`** needs operator in every namespace the trigger
  serves (the transaction re-checks it).
- **The manifest** (`GET /v1/jobs/:id/manifest`): for a reader confined to
  namespaces or job aliases the job is re-checked, and the configuration of a
  trigger it shares with a job the reader cannot read (webhook secret
  included) is replaced by a `redacted` marker, in one read snapshot.
- **Contract enforcement:** a blocking finding or a deprecation warning that
  involves a job the caller cannot read is withheld whole (no alias, team,
  dataset, schema key, path or detail). The 409's message says `a contract
  with a job outside your namespaces is affected (N finding(s))` and the body
  carries `withheld_findings`; a successful apply reports
  `withheld_contract_warnings`. Validation errors that involve such a job name
  neither it nor its dataset.

**Trigger tenancy.** A trigger row serves every job that points at it, and a
webhook delivery fans out to every HTTP trigger on its path. Two rules apply
to every write of a job or trigger (apply, the legacy create, git sync, a
trigger `PATCH`), checked on the state the write proposes before it writes:

1. A confined writer (a namespace-confined principal, a job-scoped key, or an
   allowlisted git-sync source) may not write a job whose trigger also serves,
   or whose webhook path is also subscribed by, a live job outside its reach.
   The 403 says `webhook path "p" is subscribed by a job outside your access`
   or `the trigger of job "a" also serves a job outside your access`, naming
   neither that job nor its namespace. This holds with or without a policy
   file.
2. With a policy file loaded, a webhook path may be subscribed from one
   namespace only, and a trigger may serve jobs in one namespace only, for
   every writer. A second namespace subscribing a path is refused: 409 naming
   the namespace that holds it for an unconfined writer, a non-disclosing 403
   for a confined one. Jobs of the same namespace still share a path freely.
   Server lint (`caesium job lint --server`) reports both rules, with the same
   redaction.

Older catalogs can hold a trigger row shared by jobs in two namespaces (no
current API path creates one). Rule 2 repairs such a row on the next apply of
one of its jobs by a writer allowed to write every job the row serves: the
applied job moves onto a new trigger row carrying its configuration, and the
old row, with its trigger history, stays with the other jobs. If both still
use the same webhook path the apply is refused until one of them changes its
path. A trigger `PATCH` of such a row is refused (409) until it is repaired.

### Shared objects

Notification channels, notification policies and agent profiles carry a tenancy
`namespace`, returned on every read. A create body may set it: omitted or blank
means `default`, `*` makes the object cluster-shared, and anything else must be
a DNS label (400 otherwise). The namespace is create-only: a `PATCH` may echo
the stored value but one that changes it returns 400 and writes nothing. Moving
an object is delete and create. A notification policy may reference only a
channel in its own namespace or `*`, on create and on a `channel_id` patch; a
channel the caller cannot read fails with the same 400 as a channel that does
not exist. Server-side lint and apply reject a job whose remediation profile or
escalation channel lives outside the job's namespace and `*`. The built-in
`triage-only` agent profile lives in `*`, so every namespace can reference it.
See [rest-api.md](rest-api.md).

- **Who may do what.** Reading an object needs viewer in its namespace; a `*`
  object is readable with viewer in any namespace. Creating, editing and
  deleting need operator in the object's namespace (the create body's), and an
  operator at `*` for a `*` object. The lists return the caller's namespaces'
  objects plus every `*` one.
- **Declared namespaces.** While a policy file is loaded, creating an object in
  a namespace the file does not declare returns 400 naming it, as for jobs and
  keys. `*` is always valid.
- **Notification routing.** Notification isolation needs a policy file. With
  no policy file, routing is unchanged: every policy matches by event type and
  filters, whatever its namespace, and receives the event as built. While a
  policy file is loaded, a policy delivers only the events its own
  namespace owns; a `*` policy delivers every event. An event about an incident
  belongs to the incident's namespace, one about a run to the run's persisted
  namespace (a run that outlived its job's move alerts the old owner), a
  `dataset_released` to the namespace of the runs that opened the hold and
  last breached it (none when those differ), and a
  job-only event (`sla_missed` without a run, `contract_break_declared`) to the
  job's namespace. An event whose owner cannot be found reaches only `*`
  policies. The job labels an event carries are delivered to a namespace's
  policies only when the job lives in that namespace and still has exactly
  those labels when the event is dispatched; otherwise they are removed, a
  label filter does not match, and a contract break is not delivered to that
  namespace. A contract break reaches a namespace's
  policies only with the findings between that namespace's jobs, and only
  while each remaining consumer's recorded team is still its `team` label; a
  break by a producer in another namespace reaches only `*` policies.
  Data-assertion verdicts recorded before baselines were namespace-owned
  reach a namespace's policies without their baseline evidence, as confined
  reads of runs and events show them. With or without a policy file, a policy
  whose channel is disabled, missing or outside its namespace delivers
  nothing, and an agent escalation reports itself routed only when some policy
  delivers it.
- **Agent profiles at incident time.** While a policy file is loaded, an
  incident uses a profile (the job's or the deployment default,
  `CAESIUM_AGENT_DEFAULT_PROFILE`) only when it lives in the incident's
  namespace or `*`. Any other is treated as a missing profile: a job-declared
  one denies every autonomous action, and so does a configured default that
  lives in another namespace, whatever the job's own autonomy block says (a
  default that does not exist leaves the playbook unconfigured, as before).
  With no policy file, profiles resolve by name alone, as before.
- **Names are global (v1).** Jobs reference channels and profiles by name, so a
  name is unique across namespaces. A create or rename onto a name held
  elsewhere returns 409 `conflict` without saying where. A namespace can
  therefore take a name another namespace wanted; lint and the rules above stop
  it from using another namespace's objects, so the cost is the name only. A
  cluster operator recovers it by renaming or deleting the holder: deleting an
  object releases its name.
- A `*` policy is readable by every viewer, including its job filters, which
  may name other namespaces' jobs. Cluster operators author `*` policies.

### Route classes and enforcement

Every REST route has a minimum role and a class. The classes decide how a
principal confined to some namespaces is treated:

| Class | Rule for a confined principal |
| --- | --- |
| identity | Allowed whatever its grants, even none: `GET /auth/whoami`, `POST /auth/logout`, `GET /v1/system/features`. |
| cluster | Always denied (403 `insufficient_role`): metrics, nodes and workers, atoms, logs, database, cache prune, `POST /v1/triggers`, the connector reads (`/v1/connectors*`), `GET /v1/auth/namespaces`, and `GET /v1/auth/policy` plus `POST /v1/auth/policy/reload`. |
| namespaced | Caesium resolves the namespace of the target the handler will act on and requires the route's role in it (in every resolved namespace; a dataset read needs it in any one of its producers' namespaces). A target with no owning namespace is cluster-only. |
| collection | The handler returns only rows from the namespaces where the principal holds the route's role. Live today: the API key list, the shared-object lists ([Shared objects](#shared-objects)), the job-derived collections in [Collections](#collections-and-history-under-a-job), the dataset and hold lists ([Datasets](#datasets)), and the redacted cross-namespace graphs ([Cross-namespace aggregates](#cross-namespace-aggregates)). |

A principal is *confined* on a route when its cluster role (its grant at `*`)
is below the route's role. An unconfined principal is authorised exactly as
before namespaces existed: no namespace lookup, the same status codes, bodies
and denial reasons. So with no policy file, where every principal holds its
role at `*`, behaviour is unchanged. A confined principal whose best grant is
below the route's role is refused before anything is parsed or looked up, and
a job-scoped key's alias allow-list is still applied afterwards.

What a confined principal can do today, when it holds the needed role in the
target's namespace:

- **Jobs:** tasks, dag, queue and manifest reads; delete, pause and unpause;
  run (admission re-checks the grant inside its transaction); backfill;
  queue-entry and cache deletion; the legacy create (`POST /v1/jobs`).
- **Runs** (the job's current namespace and the run's persisted namespace; a run
  of another job is 404): read a run, its logs, receipt, task descriptors and task partitions;
  verify a receipt; retry runs, partitions and callbacks. Replay needs runner in
  the job's namespace and viewer in the baseline run's namespace. A run's
  external executions are listed only to a cluster viewer, so a confined
  principal gets an empty list.
- **Job history** (the job's current namespace, then filtered per row): read a
  job with its latest run, list its runs and backfills, read its cache entries,
  explain a run (`why`), and diff two runs (both runs' namespaces are required).
- **Backfills** (the job's and the backfill's namespace): create, read and
  cancel. Create re-checks runner on the job row it stamps the backfill with.
- **Triggers:** `GET /v1/triggers/:id`, its event history,
  `POST /v1/triggers/:id/fire` and `PATCH /v1/triggers/:id`, requiring the
  role in every job the trigger serves (see
  [trigger tenancy](#confined-job-writes-and-trigger-tenancy)).
- **Shared objects:** read, create, edit and delete a notification channel,
  notification policy or agent profile (a create by the body's `namespace`).
  `*` objects are readable by viewers anywhere and writable only with the role
  at `*`.
- **Incidents:** reject an approval.
- **API keys:** list, create, revoke and rotate keys confined to namespaces
  where it is admin (see [API keys and CLI login](#api-keys-and-cli-login)).
- **Job definitions:** server lint and diff (`POST /v1/jobdefs/lint`,
  `/diff`) need viewer, and apply operator, in every definition's namespace
  and in the stored namespace of every alias they name; with no definitions,
  any viewer grant passes (apply then answers 400). Apply is described in
  [Confined job writes](#confined-job-writes-and-trigger-tenancy). Validation still runs against every job, but the answer is
  restricted to the caller's namespaces: the diff baseline (and so its
  `removed` list) holds only their jobs, a trigger a baseline job shares with
  a job elsewhere has its configuration replaced by a `redacted` marker,
  contract findings keep only edges between jobs the caller can read (and
  drop inferred findings, which come from a consumer's trigger, when that
  trigger also serves a job elsewhere), and validator errors (trigger-chain
  cycles, dataset producers and consumers, remediation references) name
  another namespace's job only as "a job in another namespace", without its
  dataset or namespace. Each of these is read in one snapshot together with
  the ownership checks that decide what it may show, so a job moving between
  namespaces mid-request is judged by where it was when it was read.
- **Audit:** `GET /v1/auth/audit` is authorised on its `?namespace=` filter (a
  namespace or `*`). An omitted filter means `*`, so a namespace admin must pass
  one.

An idempotent `POST /v1/jobs/:id/run` that would return a recorded run from a
namespace the confined caller cannot read answers an audited 403 with no run
body. The request body of a namespaced route is read (up to 8 MiB, else `413`)
before the decision. A confined caller's shared-object create body must be
`application/json` up to 1 MiB; any other body type is `415`.

**Pending routes.** A route whose handler can still return or touch another
namespace's rows is cluster-only for confined principals until the plan item
that makes it namespace-aware lands (unconfined principals are unaffected):

| Item | Routes |
| --- | --- |
| A6 | `POST /v1/events` |

Every namespace denial is `403 insufficient permissions` and is audited as
`auth.denied` with reason `insufficient_role`. Lint and diff, where the caller
named the namespaces itself, say which it lacks:
`insufficient permissions: viewer required in namespace "finance"`. For a confined principal the row
also lists the namespaces checked and, for a pending route, the plan item.

One residual race is accepted for now: a concurrent move of the exact target by
a principal who is operator in both namespaces can land between the namespace
check and the handler (run admission and replay re-check inside their own
transactions); retries and backfill children re-check inside theirs (B4), and
so do apply, the legacy create, trigger `PATCH` and the manifest export (B9).

### Scoped secrets

A namespace's `secrets.allow` rules decide which `secret://` references its jobs
may resolve. A namespace without a `secrets` block may resolve every reference
(the pre-namespace behaviour, flagged by a lint warning); `secrets: {allow: []}`
or `secrets: {}` resolves none.

Rules match the reference's **canonical target**: the provider's own parser
computes what would actually be read, query overrides included, and only that
target is matched, never the reference's surface path.

| Provider | Canonical target | Notes |
| --- | --- | --- |
| `env` | `env/<NAME>` | `?name=` wins over the path; path segments join with `_` |
| `vault` | `vault/<path>#<field>` | `?field=` or the final path segment is the field; rules grant every field of an allowed path; a decoded `#` (`%23`) in the path or field is denied |
| `k8s`, `kubernetes` | `k8s/<namespace>/<secret>` | the path namespace; for `secret://k8s/<secret>/<key>`, the namespace's mapped `kubernetes.namespace`, else the secret resolver's `CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE`; `?namespace=` and `?name=` override; the key is not part of the target |

A Kubernetes rule with one path segment (`k8s/marketing-*`, `k8s/**`) is
relative: it means that secret in the namespace's mapped Kubernetes namespace,
`kubernetes.namespace`, else `CAESIUM_KUBERNETES_NAMESPACE`. Any other effective
namespace must be named by a rule with two segments (`k8s/shared/common-*`).
A relative reference (`secret://k8s/<secret>/<key>`) of a namespace that maps
`kubernetes.namespace` reads in that mapped namespace, whatever engine runs the
step, so a relative rule and a relative reference meet. In a namespace without
a mapping a relative reference reads in `CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE`
while a relative rule means `CAESIUM_KUBERNETES_NAMESPACE`; when those differ
the reference does not match a relative rule and is denied. Set the two
variables to the same namespace, map the namespace, or use
`secret://k8s/<namespace>/<secret>/<key>`.

Where the rules apply, each reading the policy at the point of use:

- **Task execution**, local and distributed: every secret reference in the step
  environment is checked before the cache lookup, so a reference the namespace
  no longer allows is never satisfied from a cached result, and again by each
  attempt's resolution and by quarantined-replay identity verification. A denial
  fails the task at once, without retries, with an error naming only the
  redacted reference (`secret env/FINANCE_TOKEN/… is not allowed by namespace
  "marketing" secret rules: no rule matches`), and the incident class is
  `config` (never auto-retried), not `transient_infra`; `caesium why --json`
  reports it as `"failureClass": "config"`. Each check reads the
  policy as it is then, so a reload while one reference is being read applies
  to the next reference of the same step.
- **Replay** (`caesium run replay`, `POST /v1/jobs/:id/runs/:run_id/replay`): a
  fully cached replay never reaches an executor, so before a step is reused from
  cache or its baseline, or re-executed, its references must be allowed by the
  job namespace's rules; otherwise the replay is refused with 422 naming the
  redacted reference.
- **HTTP trigger** webhook secret: every namespace of the trigger's jobs must
  allow it (a trigger with no jobs uses `default`); a denial answers 500.
- **Server lint** (`caesium job lint --server`, `POST /v1/jobdefs/lint`) and the
  apply preview (`caesium job diff`, `POST /v1/jobdefs/diff`) report each
  denied reference, redacted, without resolving it. `caesium job apply` does not
  check secret rules; a bypassed lint fails at run time. Offline
  `caesium job lint --check-secrets` resolves with the operator's own
  credentials and ignores the server's policy.
- Git sync credentials, registry credentials (`CAESIUM_REGISTRY_AUTH`) and
  connector credentials are system secrets and are not scoped.

A run in a namespace this node's policy file does not declare (it was removed
after the job was applied) resolves no secrets. Work that starts while the
server is still loading its policy file (queued or freshness-derived runs at
startup) waits up to 30 seconds for it and resolves no secrets if it is not
loaded by then. Client-side execution (`caesium dev`, `caesium reproduce`, and
`caesium run retry` without `--server`) never loads the server's policy: like
offline lint, it resolves every reference with the operator's own credentials
and does not wait. `caesium dev` and `caesium reproduce` do so even with
`CAESIUM_AUTH_POLICY_FILE` set in their environment; `caesium run retry`
without `--server` refuses to run when it is set (see Run quotas).
With no policy file every
namespace resolves every reference. In distributed mode each node enforces the
rules from its own copy of the policy file: a worker whose file is missing,
stale within a reload interval, or of another version enforces older or no
rules until the owner carries the rules in the dispatch (C2). Mount the same
file on every node.

### Collections and history under a job

For a confined principal the collection routes return only rows from the
namespaces where it holds the route's role, filtered in SQL before paging, so
limits, offsets and totals describe what it can see. An unconfined principal
gets exactly the rows it got before.

| Route | What a confined principal gets |
| --- | --- |
| `GET /v1/jobs` | Jobs in its namespaces (a job-scoped key's alias list narrows this further). Each job's recent runs and latest run hold only runs it may read. `order_by` takes only column names (`id`, `alias`, `namespace`, `trigger_id`, `priority`, `paused`, `created_at`, `updated_at`) and `asc`/`desc`, for every caller; anything else is 400. |
| `GET /v1/triggers` | Triggers that serve at least one live job, where it may read every namespace the trigger serves (the `GET /v1/triggers/:id` rule). `order_by` takes only `id`, `alias`, `type`, `created_at`, `updated_at` (400 otherwise, for every caller). |
| `GET /v1/incidents` | Incidents in its namespaces, counted in `total`, without the frozen agent allowlist (`allowed_jobs`), which can name other namespaces' jobs. `last_error` loses a pre-upgrade baseline's median, sample count and the message that renders them. |
| `GET /v1/stats`, `/v1/stats/summary` | Every total, rate, top list and trend point computed over its namespaces' jobs and runs only. |
| `GET /v1/events` | Events of runs persisted in its namespaces, of the run and task lifecycle types plus `incident_opened`. Events naming no run, and types whose payloads can name other namespaces' jobs or datasets (dataset, freshness, contract, SLA, held-upstream, agent action, approval and escalation events), are not sent. Events of a run admission refused because a consumed dataset is held are not sent either, and job labels are removed from payloads. Filtering happens on the server for the catch-up and for every live event. A `run_id` in another namespace is a 403. |
| `GET /v1/events/ingested` | Ingested events whose every delivery started runs, all in its namespaces, with no skip and no error. An event the lifecycle bridge derived from a run (source `caesium`) also needs that run to be readable, and its data loses the job labels it copied from the job. |

History under a job (`/v1/jobs/:id/...`) is authorised on the job's current
namespace and then filtered by each row's persisted namespace: runs (and the
`X-Caesium-Total-Count` header), the latest run on a job read, backfills, cache
entries (by the run that filled them), and the baseline `why` compares against
(none when the only candidate is unreadable). Blame, topology and topology
history are computed over the DAG snapshots written in a namespace the caller
may read (legacy snapshots with no namespace only for unconfined callers): an
element is attributed to the first readable snapshot that introduces it, the
latest topology is the newest readable snapshot, and a `commit`, `snapshot`,
`from` or `to` that only an unreadable snapshot carries answers like one that
does not exist. A fanned step's group explanation
withholds that same pre-upgrade baseline text from `firstFailure.error` and
`summary`. A run diff needs viewer in both
runs' namespaces. A trigger's event history shows only deliveries whose every
started run the caller may read (the same rule, lifecycle provenance and label removal included). A job read embeds the job's trigger only when
the caller may read every namespace that trigger serves. The run queue
(`GET /v1/jobs/:id/queue`) shows only queued requests recorded in a namespace
the caller may read, numbered among themselves; a row queued before the
upgrade records no namespace and is shown only to unconfined callers.
`DELETE /v1/jobs/:id/queue/:queue_id` answers 404 for a row the caller cannot
see, and 403 unless the caller also holds `operator` in the namespace the row
was queued in (not only in the job's current namespace).

### Datasets

A dataset belongs to the current namespaces of its declared producers (see
the tenancy versus dataset namespace note at the top). For a confined
principal:

| Route | Rule |
| --- | --- |
| `GET /v1/datasets`, `GET /v1/datasets/holds` | Only datasets (and holds on datasets) with a producer in a namespace where it holds `viewer`, filtered in SQL, so `total` counts what it sees. A declared dataset is listed for its producer's namespace before its first run. |
| `GET /v1/datasets/:ns/:name`, `/derivations`, `/metrics` | `viewer` in any producer namespace. |
| `POST /v1/datasets/:ns/:name/advance` | `runner` in every producer namespace. |
| `POST /v1/datasets/holds/:id/release` | `operator` in every producer namespace. |

A dataset the caller may not read and a dataset that does not exist both
answer the same audited `403`. The two writes repeat the producer check
inside their own transaction, holding the advisory lock that job applies
take for each producer's alias on PostgreSQL. So an apply that moves or
re-declares a producer is either seen or waited for. If the producer set
keeps changing under the write, the write answers `409`.

What a confined reader sees of a dataset it may read is decided by who owns
each piece:

- **Derivations:** each decision is recorded with its producer and that
  producer's namespace at decision time, skips included, and is shown only in
  that namespace. A decision recorded before this change is shown only to a
  caller holding `viewer` in every current producer namespace. A covering
  run's id is dropped when that run is persisted elsewhere.
- **Metrics:** the series, its total and the baseline (median, samples,
  membership) use only samples from runs persisted in the caller's
  namespaces, the same rule the live assertion evaluator judges a run by.
- **State:** `last_run_id` and `consumed_watermarks` are kept only when that
  run is persisted in a readable namespace. `reason` is kept only when the
  producer whose evaluation wrote it is in one (a reason written before this
  change is hidden).
- **Holds:** the holder (job, run, task, step) is kept only when the opening
  run is persisted in a readable namespace. The latest breach (its run and
  violations) and the clearing run follow their runs the same way. The
  `impact` cone, which names downstream jobs anywhere, is always left out.
- **Producer:** `producing_job` and `declaration` name a producer in a
  readable namespace. Other producers are left out without a marker.

Unconfined principals, and every request without a policy file, get the
same rows and bodies as before.

### Cross-namespace aggregates

`GET /v1/lineage/impact` and `GET /v1/contracts/graph` are computed over every
namespace and then redacted for a confined principal, so it sees how its own
jobs connect to other namespaces without learning what they connect to:

- A node it may not read becomes `{"id": "external:<opaque>", "kind":
  "external"}`; an impact node keeps its `depth` too. A lineage node is
  readable when both its job's current namespace and the namespace persisted on
  the run that wrote it are; a contract job node when its job's namespace is.
  A node no single job owns (an inferred edge's unknown producer) is never
  readable.
- An edge touching such a node keeps only `{"id", "from", "to"}`: the dataset,
  the producer, consumer and previous schemas, the verdict, the findings, the
  class and the last-seen time are dropped, and edges that collapse onto the
  same endpoints are one edge. An evidence edge observed from a run in another
  namespace is collapsed the same way. An inferred edge whose job shares its
  trigger with a job the principal cannot read is withheld; the job stays.
- A dataset node is shown only when an edge shown whole carries it.
- The collapsed topology is kept: an edge between two foreign jobs is an edge
  between two placeholders.
- Placeholder ids are stable within one response and unrelated across
  responses (an HMAC under a per-response random key).
- An impact root it does not own answers exactly as a root nobody recorded
  (`200`, empty `downstream`). A declared dataset is owned by its declared
  producers; an undeclared one by the runs that wrote it; consuming a dataset
  never grants it. The graph's `?dataset=` filter is applied after redaction,
  so a dataset seen only through collapsed edges matches nothing.

Job-scoped API keys and agent-session tokens are still refused both routes.
An unconfined principal gets the unredacted body, byte for byte.

### Incidents and agent sessions

An incident belongs to the namespace of the run that failed, and never changes
namespace. The incident detail (`GET /v1/incidents/:id`), its approvals and the
agent tool surface (`/v1/agent/incidents/:id/*`) need the route's role in that
namespace. An agent session is confined to it, even though its token is minted
at `runner` cluster-wide and bound to the one incident:

- **Freeze.** The agent's job allowlist is computed from the lineage graph when
  the incident opens. Only jobs that live in the incident's namespace enter it;
  downstream jobs in other namespaces are counted in the incident's
  `allowed_jobs_external` and the bundle's `lineage_impact.external`, never
  named.
- **Serve.** Every read checks the job's namespace again: an allow-listed job
  that has moved out since is dropped from the bundle and refused by
  `context/history` (403, or an MCP error). History, the bundle's run history
  and the `why` baseline contain only runs recorded in the incident's
  namespace, for the incident's own job too, so a job moved in brings no
  foreign history and a job moved out exposes none of its new owner's. When the
  incident's own job has moved out, the bundle shows neither its current DAG
  (`job.namespace_moved: true`, no tasks or edges) nor its remediation block:
  the playbook and the session profile resolve as for a job that declares no
  remediation.
- **Act.** An action on the job or one of its runs is refused, and recorded
  `failed` with `namespace_moved`, when the job has moved out of the incident's
  namespace or the run was recorded in another one. A snoozed retry checks again
  when its timer fires. An `apply_jobdef_patch` may not change
  `metadata.namespace`, on the direct and the git-synced route alike, and a
  direct apply is confined to the incident's namespace, triggers and webhook
  paths included. Approving an action therefore needs `operator` only in the
  incident's namespace.
- **Earlier sessions.** A session started before this confinement could read
  other namespaces' jobs. For a namespace-confined reader, the incident detail
  withholds such a session's transcript (`session_log_withheld: true`) and the
  parameters and result of the agent actions and notes it wrote
  (`content_withheld: true`), the bundle omits its notes for an agent session
  and a confined reader, and the detail's `allowed_jobs` drops jobs that have
  moved out. A reader holding viewer at `*` sees every row as stored.

An agent profile's `secret_refs` are stored but never resolved by Caesium (the
model credential is injected by the deployment), so they need no namespace
scoping.

### Run quotas

`namespaces.<ns>.quotas.maxConcurrentRuns` caps how many of a namespace's runs
may be `running` at once (0 or omitted is unlimited). Every running run
counts, including quarantined replays and backfill children. The cap is one
check inside the transaction that makes a run running: a new run's admission,
a retry reopening a finished run (whole-run, partition and agent retries), and
a quarantined replay that will execute. On dqlite and SQLite the check and the
write are serialized with every other writer; on PostgreSQL a
transaction-scoped advisory lock per namespace serializes the check.

What happens at the cap depends on the kind of run:

| Run kind | At the cap |
| --- | --- |
| Cron, HTTP, event, freshness triggers and manual `POST /v1/jobs/:id/run` | Parked in the run queue with `held_reason: namespace_quota`, whatever the job's own concurrency strategy says. That includes a trigger firing again while its job's own run holds the slot: the fire is queued, not folded into the running run. The manual start answers `202 {"outcome":"queued","reason":"namespace_quota","queue_id":…}`. |
| Retry, partition retry, quarantined replay | Refused: `409 {"error":"namespace_quota","namespace":…,"max_concurrent_runs":…}` with `Retry-After: 5`. Nothing is written. |
| Incident agent `retry_from_failure` / `snooze_retry` | Deferred: the action records `retry deferred` with the quota as its cause, and a snoozed retry re-arms. |
| Backfill child | Never queued. The backfill waits (backoff up to 15 s) and retries the same date, so its counters stay exact; it stays cancellable while it waits, and a server shutdown leaves it to recovery. |

The run queue dequeuer drains quota-held runs as slots free. Namespaces are
served in rotating order and, inside a namespace with a quota, jobs take turns
one run at a time, so one job's deep queue cannot starve the others; every
promotion re-checks the quota and the job's own concurrency. A queued run
belongs to the namespace it was queued in: if its job moves before it starts,
it is dropped (an idempotent start replays as `dropped`, reason
`namespace_moved`) rather than started under the new owner. A promotion
removes its queued row in the same transaction that records its outcome (the
run it creates, a skip, or a run held upstream), and only while the dequeuer
still holds the row's claim; drops and releases are fenced by the claim too.
So a node whose claim went stale and was taken over by another (on PostgreSQL
every node drains) decides nothing for that request. When a policy
file is configured the dequeuer always runs: with `CAESIUM_RUN_QUEUE_ENABLED`
and `CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED` both off it drains only quota-held
runs and leaves rows a job's own queue policy parked untouched. A quota added
by a reload applies to the next admission. During server startup, until the
configured policy file is loaded, no run is admitted and the dequeuer promotes
nothing, so a restart cannot start queued runs past their quota. If the policy
file is removed while quota-held runs are queued, the server still starts the
dequeuer at its next start, in the same quota-only mode, and those runs drain
with no quota.

`caesium run retry` without `--server` writes straight to the catalog and
cannot see the server's policy, so it refuses when `CAESIUM_AUTH_POLICY_FILE`
is set in its environment; retry through the server with `--server`.

While a policy file is loaded, a namespace it does not declare (removed by a
reload while jobs remain in it) admits no run at all: starts, retries and
replays answer `409 {"error":"namespace_undeclared"}`, a backfill stops as
failed, and a queued run is dropped with reason `namespace_undeclared`.
Without a policy file every namespace is unlimited and no quota check runs.

`caesium job queue <alias>` shows the hold in its `REASON` column (and
`held_reason` in `--json`). Prometheus exposes
`caesium_namespace_running_runs{namespace}` (on the internal database exported
by the leader only, so a sum across nodes is the cluster value; on PostgreSQL,
where every node drains the queue, each node reports the cluster count) and
`caesium_namespace_quota_deferrals_total{namespace}`, both labelled only with
namespaces the policy declares with a quota.

### Kubernetes target

A namespace's `kubernetes` block selects where its Kubernetes steps run:
`kubernetes.namespace` is the pod namespace and `kubernetes.serviceAccountName`
the default service account. Either may be omitted: pods then run in
`CAESIUM_KUBERNETES_NAMESPACE`, and without a default account under the
namespace's `default` service account. A step's own
`kubernetes.serviceAccountName` wins over the namespace default. Volume claims
are namespace-local, so a step's PVCs are the mapped namespace's, and relative
`secret://k8s/<secret>/<key>` references read there too (see
[Scoped secrets](#scoped-secrets)).

- **Resolution.** Each task execution reads the mapping once, from the policy
  as it is when the execution starts, before its cache lookup, and keeps it for
  that execution's retries and fan-out instances: the pod, the cache key, the
  secret reads and the recorded result all use one revision. A reload applies
  to the next execution. Docker and Podman steps ignore the pod target.
- **Fail closed.** A Kubernetes step whose namespace this node's policy file
  does not declare, or that starts while a configured policy file never loads,
  fails at once with the `config` class (`kubernetes target is not allowed by
  namespace "…" access policy: …`) and starts no pod.
- **Runtime ids.** Every new Kubernetes pod's runtime id (`runtime_id` on the
  run's tasks, in callbacks and in OpenLineage `runtimeId`) is
  namespace-qualified, `<namespace>/<pod>`, mapped or not, so log streaming and
  recovery after a restart find the pod without reading the policy. An
  unqualified id recorded before this release still resolves in
  `CAESIUM_KUBERNETES_NAMESPACE`. Docker and Podman ids are unchanged.
- **Cache identity and evidence.** A Kubernetes step's mapping (namespace and
  default account), and for other engines the namespace their relative
  `secret://k8s` references read in, enter the cache key only when a mapping
  applies, so unmapped keys are unchanged and a remap is a cache miss, never a
  stale hit. The execution descriptor records the mapping
  (`runtime.kubernetesTarget`), `caesium why` and `run diff` name a changed
  `kubernetesTarget.namespace`, `kubernetesTarget.serviceAccountName` or
  `kubernetesSecretNamespace`, and a replay re-executes a step whose baseline
  ran under a different mapping (a baseline recorded before this release counts
  as unmapped) instead of reusing it.
- **Permissions.** Caesium's service account needs pods, pods/log, secrets
  (get) and persistentvolumeclaims in each mapped namespace; pre-create the
  namespaces and grant that access yourself until the Helm chart renders it
  (C3).
- **Distributed mode.** Like secret rules, each node resolves the mapping from
  its own copy of the policy file until the owner carries it in the dispatch
  (C2). Mount the same file on every node.
- There is no label-based pod reaper; a reaper would list
  `CAESIUM_KUBERNETES_NAMESPACE` plus every mapped namespace.

## API keys and CLI login

API keys authenticate machines. They are presented as
`Authorization: Bearer <key>`, and a bearer header is always evaluated as an
API key before any session cookie. Key lifecycle is `caesium auth key` and the
`/v1/auth/keys` endpoints, and a key may be further narrowed to a list of job
aliases.

A key holds its role in a set of namespaces. Without `--namespace` it is
cluster-wide, as every key minted before namespaces existed is (their
`namespaces` column is NULL):

```sh
caesium auth key create --role operator --namespace marketing [--namespace finance] [--jobs a,b] [--expires 90d]
```

`--namespace` is repeatable (or comma-separated); `POST /v1/auth/keys` takes
the same list as `namespaces`. Names must be DNS labels; `*` alone is a
cluster key, and an empty list, an empty name (`--namespace ""`) or `*` mixed
with names is `400`. With a policy file, every named namespace must be
declared in it; with no file any valid name is accepted. A cluster admin who
names an undeclared namespace gets a `400` naming it. A namespace admin holds
no grant in an undeclared namespace, so its request is refused first by the
minting constraint below, with an audited `403` that does not name the
namespace. `--jobs` (or its older spelling `--scope-jobs`) narrows the key
further to a list of job aliases. An empty value (`scope.jobs` of `[]` or only
blank aliases over REST) is a `400` for every caller, cluster admins included;
such a key used to be minted unscoped.

**Minting is limited to the caller's own grants.** Creating a key needs admin
in every namespace it names (`*` for a cluster key), so a namespace admin
(`admin` in `marketing`) can create keys confined to `marketing` at any role,
and only a cluster admin can create a cluster key. Revoking or rotating a key
needs admin in every namespace the key is confined to. A refused request is an
audited `403` (`auth.denied`). Job-scoped and agent-session keys cannot manage
keys. `GET /v1/auth/keys` (`caesium auth key list`) shows a cluster admin every
key; a namespace admin sees only the keys confined to namespaces where it is
admin, never a cluster key. A listed key shows `namespaces` only when it is
confined.

SSO users manage keys exactly like key-authenticated callers, from their
browser session (unsafe requests still need the `X-CSRF-Token` header). The
key's `created_by` and the audit actor are the user's email.

Rotation replaces the secret, not the grant: the new key has the same role,
job list, namespaces and expiry as the old one, and the old key stays valid
for the grace period (`--grace-period`, default 24h) but never past its own
expiry. A revoked or expired key cannot be rotated, nor can a key whose
namespaces are no longer all declared in the policy file (`409`); revoke it and
create a new one. A key whose stored namespaces are unreadable grants nothing;
only a cluster admin can revoke it, and it cannot be rotated.

### User-bound keys

A user-bound key (`csk_user_…`) belongs to an SSO user instead of holding a
role of its own; it is the credential `caesium login` (E5, below) will store.
It stores no role, namespaces or job list. On every request the server loads
its user and resolves the user's grants from the policy exactly as for that
user's browser session, so a policy reload or a change of the user's stored
groups applies on the next request. Until E2 refreshes groups during a
session, those are the groups stored at the user's last login, and only a
verified email matches a `users:` binding. The principal is the user:
`GET /auth/whoami` reports `kind` `user`, the user's email as `subject` and the
user id as `stable_id`, and audit rows name the user's email as the actor. A
user whose groups match no binding is refused (`403`) on every route but the
identity routes, as a grant-less session is.

Every user-bound key expires `CAESIUM_AUTH_CLI_KEY_TTL` (default `12h`, at most
7 days) after it is minted. It stops working when:

- it expires;
- a cluster admin revokes it (`caesium auth key revoke --id ID`). It is a
  cluster-only target: a namespace admin can neither revoke nor list it;
- its user is disabled (`caesium auth user disable`), in the same transaction
  as the user's sessions; enabling the user again does not revive it;
- its user is logged out everywhere (`caesium auth user logout`). This revokes
  the user's user-bound keys as well as its sessions, since both are the
  user's sign-in credentials (the design's §11.2 table names only sessions for
  this route);
- its user is disabled by any means, even if the key itself was left unrevoked.

A refused user-bound key gets the same `401` as a revoked key whatever the
reason (the `auth.denied` row and the failure metric say `user_disabled` when
its user is disabled). It cannot be rotated (`409`): signing in again mints a
new one. `GET /v1/auth/keys` lists user-bound keys only to a cluster admin,
with their `user_id` and an empty `role`; `GET /v1/auth/users/:id` lists a
user's active ones.

No route mints a user-bound key yet, and `POST /auth/logout` does not revoke
one: E5's `caesium login` will mint them, and E5 makes `POST /auth/logout`
with a user-bound key revoke exactly that key.

**Planned (E5):** `caesium login`, `caesium logout` and `caesium whoami` for
interactive CLI sign-in through the server's configured provider.

## Users and sessions

Caesium stores each user's groups at login and resolves grants again on every
request from those stored groups, so a policy or mapping change applies to
existing sessions without a new login. A user's own group changes still take
effect at their next login. See [Resolution](#resolution) for how groups become
grants.

The session cookie, idle and absolute lifetimes, CSRF, logout and trusted-proxy
settings are described under [Common settings](#common-settings) and [Browser
auth endpoint contract](#browser-auth-endpoint-contract).

Each OIDC login stores the IdP refresh token as a sealed refresh grant (see
[OIDC](#oidc)); LDAP and SAML logins store none, and nothing redeems the grant
yet. **Planned (E2):** refreshing OIDC users' groups during a session (and
behind a user-bound key), so group changes apply without a new login. **Planned (E1b):** the same for LDAP, with a
live LDAP coverage journey.

### User administration

A cluster admin (admin at `*`; a namespace-confined admin is refused with 403)
can list users, see their active sessions and [user-bound
keys](#user-bound-keys), disable and re-enable them, and log them out
everywhere. The routes are mounted only when authentication is on
(with `CAESIUM_AUTH_MODE=none` they answer 404, or 405 for the POST routes).
Users appear after their first SSO
login; Caesium never creates them otherwise.

| Route | Behaviour |
| --- | --- |
| `GET /v1/auth/users?issuer=&email=&disabled=&limit=&offset=` | Users, newest first. `issuer` matches exactly, `email` is a case-insensitive substring (SQLite and dqlite fold only ASCII letters), `disabled` is `true` or `false`. |
| `GET /v1/auth/users/:id` | One user plus its 100 newest active sessions and `sessions_total`, and its 100 newest active user-bound keys and `user_keys_total`. |
| `POST /v1/auth/users/:id/disable` | Disables the user. In the same transaction it revokes every session and user-bound key of the user and clears the stored refresh grant. |
| `POST /v1/auth/users/:id/enable` | Re-enables the user. Revoked sessions and keys stay revoked; the user signs in again. |
| `POST /v1/auth/users/:id/sessions/revoke` | Revokes every session and user-bound key of the user ("log out everywhere") without disabling it. |
| `GET /v1/auth/sessions?user_id=&limit=&offset=` | Active sessions, newest first; without `user_id`, every user's. |

- **Lists** return a bare JSON array. `X-Caesium-Total-Count` carries the total
  and `X-Caesium-Next-Offset` the next page's offset while there is one.
  `limit` defaults to 100 and must be 1 to 1000; an out-of-range `limit`, a
  negative `offset`, a malformed id or a bad `disabled` value is a 400, and an
  unknown user is a 404.
- **A user** is `{"id", "issuer", "subject", "email", "email_verified",
  "display_name", "groups", "last_login_role", "created_at", "last_login_at",
  "disabled_at"}`. `last_login_role` is the cluster role resolved at the last
  login, a record only: requests resolve grants afresh. No credential material
  (the refresh grant, its lease) is ever returned.
- **A session** is `{"id", "user_id", "auth_method", "created_at",
  "last_seen_at", "idle_expires_at", "absolute_expires_at", "source_ip",
  "user_agent"}`; never the token or its CSRF token. A session is active while
  it is unrevoked and within both lifetimes. The serving node includes the
  activity it has buffered but not yet written (written every 30 seconds while
  SSO is enabled); activity buffered on another node shows once it is written.
  Revocation does not depend on this: it revokes every unrevoked session of the
  user.
- **A user-bound key** is `{"id", "key_prefix", "description", "created_at",
  "expires_at", "last_used_at"}`; never its hash. It is active while it is
  unrevoked and unexpired. `last_used_at` is written every 30 seconds, so it can
  lag. `caesium auth key list` shows all of a user's keys, revoked ones too.
- **Disable and enable** answer `{"user", "changed", "sessions_revoked"}`, plus
  `keys_revoked` when the call revoked user-bound keys.
  Repeating either is a 200 with `"changed": false` and changes nothing. After a
  disable the user's next request on any session or user-bound key is a 401, and a login is
  refused with `auth.login_denied` reason `user_disabled`. A login already in
  flight when the user is disabled cannot create a session afterwards, even if
  the user is re-enabled before it finishes (it is refused with reason
  `user_changed`; the user signs in again). An admin cannot disable the user
  behind its own request (409).
- **Revoke** answers `{"user_id", "sessions_revoked"}`, plus `keys_revoked`
  when it revoked user-bound keys. An admin may revoke its own sessions.

CLI (`--server` defaults to `http://localhost:8080`; the key comes from
`CAESIUM_API_KEY` or `--api-key`; `--json` prints the response on stdout and
nothing else; errors go to stderr and exit 1):

- `caesium auth user list [--issuer ISSUER] [--email TEXT] [--disabled true|false] [--limit N] [--offset N] [--json]`
  prints a table by default. When there is another page it prints
  `N users in total; next page: --offset M` to stderr.
- `caesium auth user get USER_ID [--json]` prints the user, its newest active
  sessions and its newest active user-bound keys.
- `caesium auth user disable USER_ID [--json]`,
  `caesium auth user enable USER_ID [--json]` and
  `caesium auth user logout USER_ID [--json]` print a one-line summary.

Find a user's id with `caesium auth user list --email`.

## The Console

The Console reads `GET /auth/whoami` (`grants`, `namespaces`, `cluster_role`; an
older server that sends only `role` is treated as holding it everywhere) and
decides which controls to offer. This is a convenience: the server enforces
every route, and a control is hidden or disabled only when the principal's
grants are known not to allow it.

- **Gating.** `roleIn(namespace)` is the higher of the `*` grant and the grant
  for that namespace. Every job action uses the job's namespace and the role its
  route requires: trigger, re-run, replay, backfill and partition retry need
  `runner`; pause, resume, queue cancel, backfill cancel and cache invalidation
  need `operator`. A mutation on a historical run or backfill also needs the role
  in that object's persisted namespace, as the server does. Until the job is
  loaded, the cluster role decides, so a confined principal sees fewer controls,
  never more. The Console's dataset hold release control still follows the
  cluster role: it does not resolve a dataset's producers, while the server
  requires `operator` in every producer namespace.
- **Access page (`/access`, sidebar entry for everyone).** "My access" shows the
  subject, kind, cluster role and per-namespace grants. Principals with a
  cluster role (and no job scope) also see the declared namespaces and their
  settings (`GET /v1/auth/namespaces`). Cluster admins additionally see the
  policy revision and last reload result and the users list with Disable,
  Enable and Log out everywhere (each behind a confirmation). Bindings are never
  edited in the Console. With authentication disabled the page only says so.

## Audit and metrics

SSO login and logout lifecycle events are written to the audit log with actions
`auth.login`, `auth.login_denied`, `auth.logout`, and
`auth.session_revoked`. New SSO users also emit `user.provisioned` once when the
shared login tail creates the local user record. Audit metadata includes the
provider and denial reason where applicable. A login whose groups match no
mapping (and with no default role) is denied with reason `no_binding`; a
disabled user is denied with reason `user_disabled`, and a login that raced a
disable is denied with reason `user_changed`.

Only the `no_binding` denial is visible to the person signing in, so the Console
can tell them to ask for a role. After an OIDC or SAML callback the browser is
redirected to `/?login_error=no_binding` (a fixed same-origin path; the login
page shows the message and strips the parameter). The LDAP login keeps its `403`
and adds `"reason": "no_binding"` to the JSON body. Every other denial
(`user_disabled`, `user_changed`) keeps the plain `403 login denied` response.

User administration writes, with `actor` the caller (API key prefix or user
email), `resource_type` `user`, `resource_id` the user id and namespace `*`:

- `user.disable` (`metadata.sessions_revoked`, plus `metadata.keys_revoked`
  when user-bound keys were revoked) and `user.enable`, only when the call
  changed the user (a repeat writes nothing);
- `auth.sessions_revoked` with `metadata.reason` (`user_disabled`, `admin` for
  `caesium auth user logout`, or `user_enabled` when an enable revoked a stray
  session or key), `metadata.count` (sessions) and, when user-bound keys were
  revoked, `metadata.keys_revoked`; only when at least one session or key was
  revoked.

Prometheus exposes SSO counters and timing:

- `caesium_sso_logins_total{provider,outcome}`
- `caesium_sso_login_duration_seconds{provider}`
- `caesium_sso_logouts_total{outcome}`

Policy reloads write `policy.reloaded` and `policy.reload_failed` rows (query
them with `caesium auth audit --action policy.reloaded`):

- `actor` is the caller (API key prefix or user email) for the API trigger, and
  `system:policy-loader` for the poller. `source_ip` is set for the API trigger.
- `resource_type` is `access_policy`; `resource_id` is the new hash on success
  or the rejected hash on failure (empty if the file was unreadable).
- `outcome` is `success` or `error`.
- `metadata` holds `trigger` (`poll` or `api`), `path`, `revision`,
  `previous_revision`, plus `changed` (success) or `error` (failure).
- A poll that finds the loaded hash writes nothing, and the poller reports a
  given bad file once. The API trigger audits every attempt.

Prometheus also exposes
`caesium_auth_policy_reload_failures_total{trigger="poll"|"api"}`, which counts
rejected reload attempts; the poller counts a given bad file once.

Audit rows carry a `namespace` column (`AuditLog.Namespace`): the target's
namespace; `*` for cluster routes, collection routes and any decision spanning
several namespaces (those are listed in `metadata.namespaces`), and for
policy-reload rows; empty for identity routes, unknown routes, failed
credentials and webhook or login rows. Filter with:

```sh
caesium auth audit --namespace marketing
```

`--namespace` (and `GET /v1/auth/audit?namespace=`) filters exactly on the
column. Omitting it applies no filter and returns every row, including identity
and older rows, so it needs the cluster admin role; a namespace admin must pass
a namespace it administers.

## Security checks

All redirect `returnTo` and SAML RelayState values are constrained to same-origin
paths before Caesium redirects the browser. Encoded scheme-relative paths such
as `/%2f%2fevil.example/...` are treated as unsafe and fall back to `/`.

Cookie-session requests use a synchronizer CSRF token stored server-side with
the session. Caesium accepts the token only from the `X-CSRF-Token` header; a
readable cookie with the same name is ignored. Bearer API-key requests do not
need a CSRF header because they are not ambient browser credentials.

The policy file is read with a size cap and as a regular file only, its raw
bytes are never returned over the API (`GET /v1/auth/policy` returns the parsed
policy to cluster admins, and validation warnings may name binding groups), and
an invalid revision can
never replace the last good one. An unresolvable target, an empty grant set or a
failed authorization lookup denies; nothing falls back to an allow.

## Migration from the env mapping

`CAESIUM_AUTH_ROLE_MAPPING` is deprecated in favour of the policy file but keeps
working. With no `CAESIUM_AUTH_POLICY_FILE`, Caesium translates it and
`CAESIUM_AUTH_DEFAULT_ROLE` at startup into cluster-wide bindings (`group=role`
becomes a binding of that group to that role at `*`) over a single `default`
namespace, and behaviour is unchanged. The translation keeps the mapping's
trimming of incoming group names; the policy file matches the stored group
string byte for byte, and OIDC group claims are stored untrimmed.
`caesium auth policy show` prints the translated policy (`source: env`).

To move to a policy file:

1. Write the file with one binding per DISTINCT group, each with
   `namespaces: ["*"]`, and a `"*"` group binding for any `*=role` catch-all. The
   env mapping drops repeated groups and keeps the LAST entry, so
   `g=admin;g=viewer` grants `viewer` today; the policy file has no such
   de-duplication (the highest matching binding wins), so writing both entries
   would grant `admin`. Use the last entry's role. Group names must equal what the
   provider stores, byte for byte: the mapping trimmed incoming groups, the file
   does not, and OIDC claims are stored untrimmed, so an IdP that sends
   `" data-eng "` needs that exact padded string in the file (the trimmed name
   would never match and the user is denied with `no_binding`).
2. Check it offline: `caesium auth policy lint --path policy.yaml`.
3. Unset `CAESIUM_AUTH_ROLE_MAPPING` and set `CAESIUM_AUTH_POLICY_FILE`.
   Setting both is a fatal startup error naming both. `CAESIUM_AUTH_DEFAULT_ROLE`
   keeps its meaning.
4. Start the server and compare `GET /v1/auth/policy` against what you expect.
   After that, narrow bindings to individual namespaces as jobs get
   `metadata.namespace` values.

Upgrading across the namespace schema changes is a coordinated upgrade: stop
every node before applying the new binary and restart only upgraded nodes.
Mixed-version rolling upgrades are unsupported.

Scoped secrets change two things on upgrade. A policy file whose
`secrets.allow` names a provider other than `env`, `k8s`, `kubernetes` or
`vault` no longer loads (startup fails; a reload keeps the last good revision),
so run `caesium auth policy lint` first; such a rule never matched anything.
And `secrets` rules start to be enforced, so jobs in a namespace with rules stop
resolving references the rules do not allow.
