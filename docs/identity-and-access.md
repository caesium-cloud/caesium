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
  `*`) is what audit rows, metrics labels, and the `role` field of
  `GET /auth/whoami` report.
- **Tenancy namespace versus dataset namespace.** These are two unrelated
  axes. The tenancy namespace described in this guide decides which jobs, and
  which objects that belong to them, a principal may see and change. The
  dataset namespace is the OpenLineage namespace carried by dataset
  declarations, dataset metrics, and the `/v1/datasets/:ns/:name` path; it is
  reserved for cross-instance datasets and has nothing to do with tenancy or
  grants. Everywhere else this guide says "namespace" for the tenancy axis.
- **Job alias.** `metadata.alias` keeps its global unique index. Every
  alias-addressed surface (CLI verbs, key job scopes, lineage, receipts) is
  unchanged by namespaces.

With no policy file every principal holds its role at `*`, so authorization
behaves exactly as before namespaces existed. Namespaces themselves still take
effect without a file: a manifest may set any DNS-label `metadata.namespace`,
and the step cache key, incident deduplication, the git-sync `namespaces`
allowlist and the shared-object reference rules all use it. Nothing yet checks a
manifest's namespace against the declared ones (server-side rejection of an
undeclared namespace is planned, B2), so a typo creates a new namespace.

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
elsewhere. Today a session ends at logout or at its idle or absolute TTL; to cut
a user off sooner, remove their bindings from the policy file (grants are
resolved on every request). Operator-driven session revocation arrives with E3.

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

`GET /auth/whoami` requires a valid API key or session cookie. Cookie-session
responses include `csrf_token` alongside the principal fields (`kind`,
`subject`, `email`, and `role`). Browser clients should keep that token in
memory and send it as `X-CSRF-Token` on unsafe cookie-session requests.

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

The Keycloak section arrives with the Keycloak CI lane (plan item G5). Until
then, Keycloak works as any other OIDC issuer using the OIDC settings above.

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
    kubernetes:                       # parsed and validated; not enforced yet
      namespace: caesium-marketing
      serviceAccountName: caesium-jobs
    secrets:                          # parsed and validated; not enforced yet
      allow:
        - vault/secret/data/marketing/*
        - k8s/marketing-*
        - env/MARKETING_*
    quotas:                           # parsed and validated; not enforced yet
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

The `kubernetes`, `secrets` and `quotas` blocks are validated and carried in the
loaded policy, but nothing consumes them yet; see [Namespaces and
isolation](#namespaces-and-isolation).

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
  Vault rules select the secret path and allow every field at that path; `#` is
  reserved as the field delimiter and `#field` patterns are unsupported.

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
field). It defaults to `default` and must be a DNS label. It is not yet checked
against the namespaces declared in the policy file (planned, B2).

- Runs, backfills and incidents carry a copy of the job's namespace when they
  are created, so global lists can filter without a join. Triggers, task runs,
  events and lineage rows derive it through their job. Run-scoped data
  (receipts, task descriptors and partitions, logs) is authorised on both the
  job's current namespace and the run's persisted namespace.
- Moving a job is an apply that changes `metadata.namespace`. History (runs,
  receipts, incidents) keeps its original namespace. `caesium job diff` shows the
  change as a metadata diff.
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
  on run reads, the event stream and lifecycle event history.

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
  A namespace refusal skips only the refused manifest. If the stored job graph
  (trigger chains, dataset producers) would be invalid with that job left in
  place, or any other error occurs, nothing from the commit is applied and the
  sync fails.
- With git sync enabled, startup rejects an empty list (`[]`), `""`, `"*"`,
  non-DNS labels, an allowlist without a `source_id`, and a `source_id` shared
  with a restricted source. Parsing rejects `"namespaces": null`, a singular
  `"namespace"` key and duplicate case-variant keys.

### Shared objects

Notification channels, notification policies and agent profiles carry a tenancy
`namespace`, returned on every read. A create body may set it: omitted or blank
means `default`, `*` makes the object cluster-shared, and anything else must be
a DNS label (400 otherwise). The namespace is create-only: a `PATCH` may echo
the stored value but one that changes it returns 400 and writes nothing. Moving
an object is delete and create. A notification policy may reference only a
channel in its own namespace or `*`. Server-side lint and apply reject a job
whose remediation profile or escalation channel lives outside the job's
namespace and `*`. The built-in `triage-only` agent profile lives in `*`, so
every namespace can reference it. See [rest-api.md](rest-api.md).

### Route classes and enforcement

Every REST route has a minimum role and a class. The classes decide how a
principal confined to some namespaces is treated:

| Class | Rule for a confined principal |
| --- | --- |
| identity | Allowed whatever its grants, even none: `GET /auth/whoami`, `POST /auth/logout`, `GET /v1/system/features`. |
| cluster | Always denied (403 `insufficient_role`): metrics, nodes and workers, atoms, logs, database, cache prune, `POST /v1/triggers`, the connector reads (`/v1/connectors*`), and `GET /v1/auth/policy` plus `POST /v1/auth/policy/reload`. |
| namespaced | Caesium resolves the namespace of the target the handler will act on and requires the route's role in it (in every resolved namespace; once B6 ships, dataset reads will need it in any one). A target with no owning namespace is cluster-only. |
| collection | The handler returns only rows from the namespaces where the principal holds the route's role (see [Collections](#collections-and-history-under-a-job)). |

A principal is *confined* on a route when its cluster role (its grant at `*`)
is below the route's role. An unconfined principal is authorised exactly as
before namespaces existed: no namespace lookup, the same status codes, bodies
and denial reasons. So with no policy file, where every principal holds its
role at `*`, behaviour is unchanged. A confined principal whose best grant is
below the route's role is refused before anything is parsed or looked up, and
a job-scoped key's alias allow-list is still applied afterwards.

What a confined principal can do today, when it holds the needed role in the
target's namespace:

- **Jobs:** tasks, dag and queue reads; delete, pause and unpause; run
  (admission re-checks the grant inside its transaction); backfill; queue-entry
  and cache deletion.
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
- **Triggers:** `GET /v1/triggers/:id`, its event history and
  `POST /v1/triggers/:id/fire`, requiring the role in every job the trigger
  serves.
- **Shared objects:** read and delete a notification channel, notification
  policy or agent profile by id. `*` objects are readable by viewers anywhere
  and writable only with the role at `*`.
- **Incidents:** reject an approval.
- **Audit:** `GET /v1/auth/audit` is authorised on its `?namespace=` filter (a
  namespace or `*`). An omitted filter means `*`, so a namespace admin must pass
  one.

An idempotent `POST /v1/jobs/:id/run` that would return a recorded run from a
namespace the confined caller cannot read answers an audited 403 with no run
body. The request body of a namespaced route is read (up to 8 MiB, else `413`)
before the decision. Once D4 makes shared-object creates live, a confined
caller's create body must be `application/json` up to 1 MiB, and any other body
type will be `415`; until then those creates are denied as pending.

**Pending routes.** A route whose handler can still return or touch another
namespace's rows is cluster-only for confined principals until the plan item
that makes it namespace-aware lands (unconfined principals are unaffected):

| Item | Routes |
| --- | --- |
| A5 | API key list, create, revoke, rotate (`/v1/auth/keys`) |
| A6 | `POST /v1/events` |
| A8 | `/v1/agent/incidents/*`, incident detail `GET /v1/incidents/:id`, approval approve |
| B2 | `POST /v1/jobs`, `/v1/jobdefs/apply`, `/lint`, `/diff`, `GET /v1/jobs/:id/manifest`, `PATCH /v1/triggers/:id` |
| B7 | `GET /v1/jobs/:id/blame`, `topology` and `topology/history` (DAG snapshots record no namespace yet) |
| B5 | `GET /v1/lineage/impact`, `GET /v1/contracts/graph` |
| B6 | every `/v1/datasets*` route |
| D4 | shared-object lists, and creating or editing channels, policies and agent profiles |

Every namespace denial is `403 insufficient permissions` and is audited as
`auth.denied` with reason `insufficient_role`. For a confined principal the row
also lists the namespaces checked and, for a pending route, the plan item.

One residual race is accepted for now: a concurrent move of the exact target by
a principal who is operator in both namespaces can land between the namespace
check and the handler (run admission and replay re-check inside their own
transactions); retries and backfill children re-check inside theirs (B4);
in-transaction guards for apply follow in B2.

### Collections and history under a job

For a confined principal the collection routes return only rows from the
namespaces where it holds the route's role, filtered in SQL before paging, so
limits, offsets and totals describe what it can see. An unconfined principal
gets exactly the rows it got before.

| Route | What a confined principal gets |
| --- | --- |
| `GET /v1/jobs` | Jobs in its namespaces (a job-scoped key's alias list narrows this further). Each job's recent runs and latest run hold only runs it may read. `order_by` takes only column names (`id`, `alias`, `namespace`, `trigger_id`, `priority`, `paused`, `created_at`, `updated_at`) and `asc`/`desc`, for every caller; anything else is 400. |
| `GET /v1/triggers` | Triggers that serve at least one live job, where it may read every namespace the trigger serves (the `GET /v1/triggers/:id` rule). `order_by` takes only `id`, `alias`, `type`, `created_at`, `updated_at` (400 otherwise, for every caller). |
| `GET /v1/incidents` | Incidents in its namespaces, counted in `total`, without the frozen agent allowlist (`allowed_jobs`), which can name other namespaces' jobs. |
| `GET /v1/stats`, `/v1/stats/summary` | Every total, rate, top list and trend point computed over its namespaces' jobs and runs only. |
| `GET /v1/events` | Events of runs persisted in its namespaces, of the run and task lifecycle types plus `incident_opened`. Events naming no run, and types whose payloads can name other namespaces' jobs or datasets (dataset, freshness, contract, SLA, held-upstream, agent action, approval and escalation events), are not sent. Events of a run admission refused because a consumed dataset is held are not sent either, and job labels are removed from payloads. Filtering happens on the server for the catch-up and for every live event. A `run_id` in another namespace is a 403. |
| `GET /v1/events/ingested` | Ingested events whose every delivery started runs, all in its namespaces, with no skip and no error. An event the lifecycle bridge derived from a run (source `caesium`) also needs that run to be readable, and its data loses the job labels it copied from the job. |

History under a job (`/v1/jobs/:id/...`) is authorised on the job's current
namespace and then filtered by each row's persisted namespace: runs (and the
`X-Caesium-Total-Count` header), the latest run on a job read, backfills, cache
entries (by the run that filled them), and the baseline `why` compares against
(none when the only candidate is unreadable). A run diff needs viewer in both
runs' namespaces. A trigger's event history shows only deliveries whose every
started run the caller may read (the same rule, lifecycle provenance and label removal included). A job read embeds the job's trigger only when
the caller may read every namespace that trigger serves.

### Planned isolation features

- **Planned (C1): Kubernetes target per namespace.** The `kubernetes` block of
  a namespace in the policy file will select the Kubernetes namespace and
  service account that a namespace's pods run under. Today it is validated but
  every job runs where the engine is configured.
- **Planned (C4): scoped secrets.** The `secrets.allow` rules will restrict
  which `secret://` references a namespace's jobs may resolve. Today they are
  validated but not enforced, and every namespace can resolve every secret.
- **Planned (D1): run quotas.** `quotas.maxConcurrentRuns` will cap a
  namespace's concurrently active runs. Today it is validated but not enforced.

## API keys and CLI login

API keys authenticate machines. They are presented as
`Authorization: Bearer <key>`, and a bearer header is always evaluated as an
API key before any session cookie. Key lifecycle is `caesium auth key` and the
`/v1/auth/keys` endpoints, and a key may be further narrowed to a list of job
aliases.

Keys carry a `namespaces` column. A NULL value (every key minted before
namespaces existed) means cluster-wide, so each existing key keeps its meaning.
Any other empty or undecodable value grants nothing. **Planned (A5):** minting
and rotating keys confined to namespaces (`--namespace`), the rule that a caller
can mint only keys within its own grants, and SSO-admin key management. Until
then every key is cluster-wide.

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
yet. **Planned (E2):** refreshing OIDC users' groups during a session, so group
changes apply without a new login. **Planned (E1b):** the same for LDAP, with a
live LDAP coverage journey. **Planned (E3):** user and session administration
endpoints and CLI.

## Audit and metrics

SSO login and logout lifecycle events are written to the audit log with actions
`auth.login`, `auth.login_denied`, `auth.logout`, and
`auth.session_revoked`. New SSO users also emit `user.provisioned` once when the
shared login tail creates the local user record. Audit metadata includes the
provider and denial reason where applicable. A login whose groups match no
mapping (and with no default role) is denied with reason `no_binding`; a
disabled user is denied with reason `user_disabled`.

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
