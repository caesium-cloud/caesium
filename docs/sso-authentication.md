# SSO Authentication

> Status: Current operator guide for native SSO configuration.

Caesium supports API keys for machines and optional browser SSO for interactive
users. SSO is additive: keep `CAESIUM_AUTH_MODE=api-key` enabled and turn on one
or more providers with `CAESIUM_AUTH_OIDC_ENABLED`,
`CAESIUM_AUTH_SAML_ENABLED`, or `CAESIUM_AUTH_LDAP_ENABLED`.

## Common Settings

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

If no group mapping matches, an empty `CAESIUM_AUTH_DEFAULT_ROLE` denies login.
Set it to `viewer`, `runner`, `operator`, or `admin` only when unmapped SSO
users should receive that fallback role.

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

### Common Operator Reference

| Env | Default | Operator note |
| --- | --- | --- |
| `CAESIUM_AUTH_PUBLIC_BASE_URL` | unset | External base URL used to derive OIDC redirect and SAML ACS/metadata URLs. |
| `CAESIUM_AUTH_DEFAULT_ROLE` | empty | Fallback role when no group mapping matches; empty denies login. |
| `CAESIUM_AUTH_SESSION_COOKIE_NAME` | `caesium_session` | Browser session cookie name for SSO callbacks, `/auth/whoami`, and logout. |
| `CAESIUM_AUTH_SESSION_IDLE_TTL` | `8h` | Sliding idle timeout refreshed by valid session use. |
| `CAESIUM_AUTH_SESSION_ABSOLUTE_TTL` | `24h` | Hard session expiry. |
| `CAESIUM_TRUSTED_PROXIES` | empty | Comma-separated trusted proxy IPs/CIDRs for forwarded-proto handling. |

## OIDC

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

## SAML

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

## LDAP

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

Filter placeholders are escaped before substitution. Supported LDAP filter
placeholders are `{username}` for the submitted username and `{dn}` for the
resolved user DN. For Active Directory, common filters are:

```sh
CAESIUM_AUTH_LDAP_USER_FILTER='(sAMAccountName={username})'
CAESIUM_AUTH_LDAP_GROUP_FILTER='(member={dn})'
CAESIUM_AUTH_LDAP_GROUP_ATTRIBUTE=dn
CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE=sAMAccountName
CAESIUM_AUTH_LDAP_EMAIL_ATTRIBUTE=userPrincipalName
CAESIUM_AUTH_LDAP_DISPLAY_NAME_ATTRIBUTE=cn
```

LDAP attribute and timeout reference:

| Env | Default | Operator note |
| --- | --- | --- |
| `CAESIUM_AUTH_LDAP_TIMEOUT` | `10s` | Dial, bind, and search timeout; keep below your load balancer timeout. |
| `CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE` | `uid` | Attribute used as the external username when present. |
| `CAESIUM_AUTH_LDAP_EMAIL_ATTRIBUTE` | `mail` | Attribute used for the Caesium user email. |
| `CAESIUM_AUTH_LDAP_DISPLAY_NAME_ATTRIBUTE` | `displayName` | Attribute used for the display name. |
| `CAESIUM_AUTH_LDAP_GROUP_ATTRIBUTE` | `cn` | Values mapped through `CAESIUM_AUTH_ROLE_MAPPING`; use `dn` for full-DN mappings. |

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

## Security Checks

All redirect `returnTo` and SAML RelayState values are constrained to same-origin
paths before Caesium redirects the browser. Encoded scheme-relative paths such
as `/%2f%2fevil.example/...` are treated as unsafe and fall back to `/`.

Cookie-session requests use a synchronizer CSRF token stored server-side with
the session. Caesium accepts the token only from the `X-CSRF-Token` header; a
readable cookie with the same name is ignored. Bearer API-key requests do not
need a CSRF header because they are not ambient browser credentials.

## Audit and Metrics

SSO login and logout lifecycle events are written to the audit log with actions
`auth.login`, `auth.login_denied`, `auth.logout`, and
`auth.session_revoked`. New SSO users also emit `user.provisioned` once when the
shared login tail creates the local user record. Audit metadata includes the
provider and denial reason where applicable.

Prometheus exposes SSO counters and timing:

- `caesium_sso_logins_total{provider,outcome}`
- `caesium_sso_login_duration_seconds{provider}`
- `caesium_sso_logouts_total{outcome}`
