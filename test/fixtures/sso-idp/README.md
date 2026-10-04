# Local SSO protocol fixture

Test-only helper (`integration` build tag). It imports no production Caesium auth,
controller, or store package. The parent collector runs the actual instrumented
Caesium server against its real database; this helper supplies an isolated IdP
and drives real HTTP login/callback/ACS requests. Hermetic fixture tests validate
crypto/protocol correctness and are not candidate live-coverage evidence.

Build in the existing container toolchain, without installing dependencies:

```sh
go build -tags=integration -o /fixture/sso-idp ./test/fixtures/sso-idp
```

The collector exclusively owns Docker resources, image identity, metadata/DB
volumes, coverage collection, restart and cleanup. The helper never invokes
Docker. Run it in a private task network with no Docker socket or host port.
The helper origin arguments require plain HTTP origins without credentials,
paths, queries or fragments. Use distinct IdP/server task hostnames on the same
network; never rewrite OIDC issuer URLs between browser and server transports.

## Serve contract

```sh
SSO_FIXTURE_CLIENT_SECRET=DISPOSABLE_PRIVATE_VALUE /fixture/sso-idp serve \
  --listen :8090 \
  --issuer http://TASK-idp:8090 \
  --sp-base http://TASK-sso:8080 \
  --metadata-file /fixture/idp.xml
```

A fresh collector-owned fixture volume is required. The helper creates metadata
exclusively (refuses an existing file), before serving `/ready`. RSA signing key
and authorization codes stay in memory. Metadata is public, containing only the
certificate. Keep the IdP alive across the server restart.

`GET /ready` returns exactly `{ready:true,version:1,issuer:string,sp_base:string}`.
OIDC discovery/JWKS/authorization/token endpoints are real HTTP endpoints;
client ID is `coverage-caesium`, with secret above. Token exchange checks client,
PKCE S256, redirect, expiry and single code redemption before signing RS256.
SAML IdP metadata advertises redirect SSO; `/saml/sso` decodes the actual SP
AuthnRequest, reads actual SP metadata, binds audience/recipient/InResponseTo and
returns a genuinely signed assertion/response via a POST binding form. `/stats`
returns only integer `issued`, `redeemed`, `rejected` counters. Responses/forms
and tokens belong only to the private protocol flow, never collector artifacts.

## Instrumented Caesium configuration

Use the collector's same immutable instrumented image for both server
processes, a fresh real DB volume and separate coverage directory. Set:

```text
CAESIUM_AUTH_MODE=api-key
CAESIUM_AUTH_REQUIRE_TLS=false
CAESIUM_AUTH_KEY_HASH_SECRET=<private random 32+ bytes, unchanged on restart>
CAESIUM_AUTH_PUBLIC_BASE_URL=http://TASK-sso:8080
CAESIUM_AUTH_ROLE_MAPPING=coverage-admins=admin;coverage-readers=viewer
CAESIUM_AUTH_DEFAULT_ROLE=
CAESIUM_AUTH_OIDC_ENABLED=true
CAESIUM_AUTH_OIDC_ISSUER_URL=http://TASK-idp:8090
CAESIUM_AUTH_OIDC_CLIENT_ID=coverage-caesium
CAESIUM_AUTH_OIDC_CLIENT_SECRET=<same disposable client secret>
CAESIUM_AUTH_SAML_ENABLED=true
CAESIUM_AUTH_SAML_IDP_METADATA_FILE=/fixture/idp.xml
CAESIUM_DATABASE_CONSOLE_ENABLED=true
```

Mount metadata read-only. Keep default cookie names and groups attributes; let
production derive OIDC callback/SAML ACS/metadata/entity URLs. No verifier,
HTTPClient, clock or replay-store injection is used. Parent must redact the
bootstrap administrator key before persisting or printing server logs. Helper
never reads or uses that key: successful SSO admin sessions provide protected
reads and read-only SQL queries with real `/auth/whoami` CSRF tokens.

## Journey and restart barrier

```sh
/fixture/sso-idp journey --base http://TASK-sso:8080 \
  --idp http://TASK-idp:8090 --candidate-sha FULL_LOWERCASE_SHA --timeout 4m
```

Whole journey deadline defaults to four minutes and cannot exceed four minutes;
HTTP requests are bounded to ten seconds. Assertion conditions last five minutes
and the state cookie ten minutes, but crewjam rejects Response or Assertion
IssueInstant older than its 90-second MaxIssueDelay before accessing the replay
store. Both replay callbacks therefore require the original signed issue instants
and monotonic elapsed time to remain strictly below 60 seconds before and after
submission. The restart barrier has that same tighter bound; an aged 401 fails
the journey instead of counting as durable replay evidence. Do not delay restart. Keep the
journey process alive with stdout/stdin pipes through the barrier. Each stdout
JSON line is written immediately, without a buffering wrapper.

Required successful OIDC login, code replay rejection, bad/missing/tampered state,
provider error, signed bad nonce/audience, fresh positive control, genuine SAML
login, same-assertion immediate replay, durable replay after restart, fresh
positive control, tampered/wrong-audience/expired signed SAML responses, bad
RelayState/state/missing cookie/response, and both providers' same-origin absolute,
escaped URI, foreign/scheme-relative/relative return targets are asserted. Each
refusal preserves exact user/session/assertion counts; successful logins retain
one user per issuer and add one session (and one accepted assertion for SAML).

The helper saves the original SAML state cookie and signed form IN MEMORY and
restores the original cookie explicitly on replay. Allowing an ordinary cookie
jar to discard it would falsely prove replay via the missing-cookie branch.
After immediate replay, stdout emits:

```json
{"event":"restart_required","phase":"saml-persisted","assertion_digest":"64 lowercase hex chars"}
```

Collector flushes candidate coverage (SIGUSR2), gracefully stops its verified
immutable server container ID, records successful exit/no OOM and starts a NEW
server process using the SAME instrumented image, DB, metadata, hash secret and
URLs. Once ready, collector writes this JSON line to helper stdin:

```json
{"event":"server_restarted","generation":2}
```

No extra acknowledgment fields are accepted. The helper checks real route
readiness, persisted session authentication and exact replay-row/count behavior.
The collector separately proves that a restart actually happened; a fabricated
acknowledgment alone is not restart evidence. A fresh signed login after durable
replay rejection is the positive control. Do not save raw stdin protocol state,
cookies, forms, JWTs, CSRF values, session hashes, assertion blobs or private keys
in artifacts.

## Strict stdout schema

All objects have only the listed keys; optional evidence keys are omitted when
not relevant. No server response body is echoed.

- Check line: `event:"check"`, `name:string`, `status:"pass"`, `evidence:object`.
- Barrier line: `event:"restart_required"`, `phase:"saml-persisted"`,
  `assertion_digest:string` (SHA-256 hex of encoded signed response).
- Final line: `event:"complete"`, `status:"pass"`, `candidate_sha:string`,
  `checks:[string]`, `evidence:[object]`, `counts:object`.
- Failure line: `event:"failed"`, `phase:string`; exit nonzero. Stderr is a
  generic diagnostic, with no raw transport/protocol error text. Argument
  errors can precede phase creation and still fail nonzero.

Every evidence object has `name:string`; optional keys are `status_code:int`,
`redirect:string`, `role:string`, `row_counts:object`. Redirects are validated
fixture return destinations; roles are validated `admin`. Both counts and
row_counts contain exactly integer `oidc_users`, `saml_users`, `oidc_sessions`,
`saml_sessions`, `assertions`. Readiness/restart barrier evidence can contain
only name. Check names are deterministic; the final checks array contains all
successful checks in order. The collector should require complete final result
and all expected checks, not just any zero exit or a partial pass list.

Final phase counts on a fresh database are two users, seven OIDC sessions, seven
SAML sessions and seven accepted assertions (one initial and one fresh control
plus five return-target logins for each provider). All negative callbacks create
no extra identity/session/replay rows.

Flush and gracefully stop the second candidate server after complete. Combine
both actual server generations' coverage with existing candidate server
provenance; fixture binary coverage never belongs to that profile. Require real
nonzero execution in OIDC/SAML state, shared ssostate, persistent replay store and
callback code. HTTP protocol/main-server qualification does not claim rendered
browser UX, external IdP compatibility, HTTPS cookie transport or cross-node
simultaneous replay.
