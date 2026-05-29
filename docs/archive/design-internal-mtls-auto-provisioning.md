# Design: Auto-provisioning internal mTLS

Status: proposed (2026-05-26)
Owner: Christopher Ryan

## Goal

Make run-owner mode secure node-to-node **with zero operator certificate work**.
Today `CAESIUM_RUN_OWNER_ENABLED=true` hard-fails at startup unless the operator
has provisioned a CA + per-node cert/key and mounted all three files
(`CAESIUM_INTERNAL_MTLS_CA/CERT/KEY`). That friction is the blocker to making
owner mode (and owner+in-memory) a sensible default for distributed deployments.

This design auto-provisions the internal CA and per-node leaf certificates,
bootstrapped from a single shared secret, so that setting one env var (the
existing internal token) yields full mutual-TLS on the internal endpoints with
no `openssl`, no cert files, and no manual rotation.

## Requirements (decided)

- **Runtime-agnostic.** No dependency on Kubernetes (or cert-manager, the CSR
  API, projected service-account tokens). Must work identically on bare metal,
  docker, podman, and k8s. Trust bootstraps through Caesium itself.
- **Token-bootstrapped.** A single shared secret authenticates the trust
  bootstrap. Reuse the existing cluster token rather than introducing a new one.
- **Rotation in v1.** Leaf renewal, expiry handling, and CA roll are in scope —
  not a follow-up.
- **BYO override retained.** If the operator sets explicit
  `CAESIUM_INTERNAL_MTLS_{CA,CERT,KEY}`, use them unchanged (existing behavior).
  Auto-provisioning only engages when those are unset.
- **Ephemeral-storage-safe.** Deployments commonly run with `persistence.enabled=false`
  (ephemeral pod storage). Durable PKI state must survive node restarts and
  leader failover without requiring a PVC.

## Non-goals (v1)

- Certificate revocation (CRL/OCSP). Short leaf lifetimes + rotation cover the
  common case; revocation is a follow-up.
- External CA / PKI integration beyond the existing BYO file path (Vault,
  cert-manager). BYO remains the escape hatch.
- Securing the dqlite replication transport itself (separate concern; see
  "Security analysis" for why this design does not depend on it).
- Per-task or per-tenant identity. Identity is per-node.

## Background / current state

- Owner mode exposes internal RPC endpoints (`/internal/dispatch`,
  `/internal/complete`, `/internal/wakeup`) on a dedicated listener
  (`CAESIUM_INTERNAL_PORT`, default 8443). These let nodes drive each other's
  task execution and report completions+outputs, so they must be authenticated.
- `internal/dispatch/mtls.go` already builds the TLS configs:
  `ServerTLSConfig` (`RequireAndVerifyClientCert`) and `ClientTLSConfig`
  (`InsecureSkipVerify` + `verifyChainAgainst` — hostname is intentionally
  skipped for dynamic pod IPs; the chain is verified against the CA, and the
  peer cert must be valid for the appropriate EKU). The **same key pair is each
  node's identity in both directions** (server cert on its listener, client cert
  when POSTing to peers), so a leaf needs **both** `serverAuth` and `clientAuth`
  EKUs.
- A bearer token (`CAESIUM_INTERNAL_WAKEUP_TOKEN`) already gates every internal
  endpoint via `Handler.authorized`. It is currently *optional* (warned), while
  mTLS material is *mandatory* (fatal if missing).
- `cmd/start/start.go` builds the mTLS configs inside `if vars.RunOwnerEnabled`
  and fatally errors if `MTLSConfig.Configured()` is false.
- The **dqlite catalog** is replicated shared state every node already trusts,
  riding dqlite's own transport (separate from the HTTP endpoints we are
  securing). `pkg/dqlite` exposes `IsLocalLeader(ctx)` and `Cluster(ctx)`.

## Architecture (Approach A: catalog-mediated, leader-signed, token-encrypted CA)

The dqlite leader is the certificate authority. CA material lives in the
replicated catalog so it survives leader failover and ephemeral node storage.
The CA private key is encrypted at rest with a key derived from the shared
token, so possession of the catalog alone does not grant signing power. Nodes
generate their own leaf key pairs locally and obtain signed certs through a
catalog-mediated CSR exchange authenticated by a token HMAC. **The token itself
is never transmitted** — it is used only locally to derive the CA-key encryption
key and to HMAC/verify CSRs; CSRs and certs flow over the dqlite transport.

### Shared secret

Reuse `CAESIUM_INTERNAL_WAKEUP_TOKEN` as the one cluster secret. Derive distinct
keys from it via labeled HKDF-SHA256 so the usages don't overlap:

- bearer auth: the raw token (unchanged).
- CA-key encryption key (KEK): `HKDF-SHA256(token, info="caesium-internal-mtls-ca-kek-v1")` → 32 bytes.
- CSR-authentication MAC key: `HKDF-SHA256(token, info="caesium-internal-mtls-csr-mac-v1")` → 32 bytes.

The token becomes **required** when owner mode is on and no explicit cert files
are provided. Emit a startup warning if it is shorter than 32 bytes (low
entropy weakens the KEK; document that a high-entropy random token is expected).
A dedicated `CAESIUM_INTERNAL_MTLS_TOKEN` may override the wakeup token if an
operator wants to separate them, but reuse is the default.

### Catalog schema (new models + migration)

`internal_ca_generations` — overlapping CA generations (multiple rows during a roll):
- `generation` INTEGER PRIMARY KEY (monotonic; highest non-expired is "active" for new issuance)
- `cert_pem` TEXT (CA certificate, public)
- `key_ciphertext` BLOB (CA private key, AES-256-GCM under the KEK)
- `key_nonce` BLOB (12-byte GCM nonce)
- `not_before`, `not_after` TIMESTAMP
- `created_at` TIMESTAMP

`internal_node_enrollments` — the CSR→cert rendezvous (rows GC'd after retrieval + TTL):
- `id` TEXT PRIMARY KEY (uuid)
- `node_id` TEXT (requesting node's `CAESIUM_NODE_ADDRESS`)
- `csr_pem` TEXT
- `csr_mac` BLOB (HMAC-SHA256(csr-mac-key, csr_der) — proves cluster membership)
- `ca_generation` INTEGER (which CA the requester wants to be signed under; the
  signer may upgrade to the newest)
- `cert_pem` TEXT NULL (filled by the signer)
- `status` TEXT ("pending" | "signed" | "rejected")
- `requested_at`, `signed_at` TIMESTAMP

These are catalog (not hot-shard) tables; migrate on the catalog connection
alongside the other models.

### Crypto parameters

- KEK derivation: HKDF-SHA256 as above; CA key sealed with AES-256-GCM (random
  12-byte nonce per seal, tag appended).
- CA cert: self-signed, `BasicConstraintsValid` + `IsCA=true`, `KeyUsage =
  CertSign|CRLSign`, CN `caesium-internal-ca`, lifetime
  `CAESIUM_INTERNAL_MTLS_CA_TTL` (default ~5y).
- Leaf cert: signed by the active CA, `ExtKeyUsage =
  {ServerAuth, ClientAuth}` (both — node is client and server), `KeyUsage =
  DigitalSignature|KeyEncipherment`, `IsCA=false`, CN/SAN = node address,
  `NotBefore = now-5m` (clock-skew backdate), lifetime
  `CAESIUM_INTERNAL_MTLS_LEAF_TTL` (default ~30d).
- The signer builds the leaf template itself and takes **only the public key**
  (and validates the claimed node identity) from the CSR — it must NOT honor
  CSR-requested extensions (no `CA:TRUE`, no arbitrary EKU). It must verify the
  CSR signature (proves the requester holds the private key) before signing.

### Enrollment sequence

On startup, when auto-provisioning is engaged, owner-mode wiring **blocks until
this node holds a signed leaf + a trust pool** (bounded by a timeout, with
retry/backoff). Steps:

1. Connect to dqlite (already done before this point).
2. **Genesis (leader only):** if `internal_ca_generations` is empty and this
   node `IsLocalLeader`, generate CA generation 1, seal its key under the KEK,
   and insert. Use the `generation` PK (and/or a conditional insert) so
   concurrent genesis attempts are idempotent — exactly one CA wins; the others
   read it. Non-leaders poll until a CA row appears.
3. **Trust pool:** read all non-expired rows from `internal_ca_generations`;
   the union of their `cert_pem` is the trust pool (`ClientCAs` / `RootCAs`).
4. **Leaf request:** generate a leaf key pair locally (private key stays in
   memory), build a CSR, compute `csr_mac = HMAC(csr-mac-key, csr_der)`, and
   insert a `pending` enrollment row (under the newest CA generation).
5. **Signing (leader signer loop):** the leader polls `pending` enrollments;
   for each, constant-time-verify `csr_mac`, verify the CSR signature, build a
   constrained leaf template, sign with the decrypted CA key, write `cert_pem` +
   `status=signed`. On MAC mismatch, set `status=rejected` (token mismatch).
6. **Retrieval:** the requester polls its enrollment row; on `signed`, load the
   leaf into the material holder. On `rejected`, fail with a clear "token
   mismatch?" error.
7. Start the internal mTLS listener + dispatch loop only after the holder has a
   valid leaf + trust pool.

The leader enrolls its own leaf through the same table (its signer loop signs
its own row), so there is no special-case path for the leader's own cert.

### Dynamic TLS material (hot reload)

Introduce a `MaterialHolder` (atomic pointer) holding the current leaf
`tls.Certificate` + trust pool. Rebuild `ServerTLSConfig`/`ClientTLSConfig` to
read from it via callbacks:
- server: `GetCertificate` (and `GetConfigForClient` for the current `ClientCAs`).
- client: `GetClientCertificate` + a `VerifyPeerCertificate` that verifies
  against the holder's current pool (preserving the hostname-skip behavior).

The BYO path wraps the static files in a holder that never changes, so both
paths share one code path.

### Rotation (v1)

A per-node rotation goroutine:
- **Leaf renewal:** when the current leaf passes `renewBefore` (default ~1/3 of
  lifetime remaining), re-run enrollment (steps 3–6) under the newest CA and
  atomically swap the holder's certificate. No restart.
- **Trust-pool refresh:** periodically re-read `internal_ca_generations` and
  update the holder's pool, so a freshly rolled CA becomes trusted clusterwide
  *before* any leaf is issued under it.
- **CA roll (leader only):** when the active CA passes `caRenewBefore`, generate
  generation N+1 and insert it alongside N. During the overlap the trust pool is
  the union of both, so leaves under either verify. New leaves are issued under
  N+1. Prune a generation only after `not_after` + grace, when no live leaf
  depends on it.

### Startup wiring changes (`cmd/start/start.go`)

Replace the `MTLSConfig.Configured()` hard-fail with:
- explicit `CAESIUM_INTERNAL_MTLS_{CA,CERT,KEY}` set → BYO path (static holder), unchanged.
- else token present → auto-provision: run enrollment (blocking, with timeout),
  build dynamic configs from the holder, start the rotation goroutine.
- else → fatal with the friendlier message: *"run-owner mode requires either
  explicit mTLS material (CAESIUM_INTERNAL_MTLS_CA/CERT/KEY) or a shared token
  (CAESIUM_INTERNAL_WAKEUP_TOKEN) for automatic provisioning."*

## Config additions (`pkg/env`)

- `CAESIUM_INTERNAL_MTLS_TOKEN` (optional; defaults to `CAESIUM_INTERNAL_WAKEUP_TOKEN`).
- `CAESIUM_INTERNAL_MTLS_CA_TTL` (default ~43800h / 5y).
- `CAESIUM_INTERNAL_MTLS_LEAF_TTL` (default ~720h / 30d).
- `CAESIUM_INTERNAL_MTLS_LEAF_RENEW_BEFORE` / `..._CA_RENEW_BEFORE` (durations or
  fractions; sensible defaults).
- Auto-provisioning engages implicitly when owner mode is on, no explicit certs
  are set, and a token is present (no separate enable flag needed).

## Suggested package / file layout

- `internal/dispatch/pki/` — new package:
  - `kek.go` (HKDF derivation, AES-GCM seal/open),
  - `ca.go` (genesis, leaf template, CSR signing with constraints),
  - `store.go` (gorm access to the two new tables),
  - `provisioner.go` (enrollment orchestration + rotation goroutine + `MaterialHolder`).
- `internal/models/` — the two new table models + register in `models.All`.
- `internal/dispatch/mtls.go` — dynamic `ServerTLSConfig`/`ClientTLSConfig` backed by the holder.
- `cmd/start/start.go` — wiring + the new fatal/branch logic.
- `pkg/env/env.go` — config fields above.

## Failure modes & edge cases

- **Token mismatch across nodes** → CSR MAC fails → `status=rejected` → the node
  fails enrollment with a clear message. (Most likely operator error.)
- **No leader / dqlite unavailable at boot** → enrollment retries with backoff
  until quorum; bounded by the enrollment timeout.
- **Genesis race** (multiple fresh leaders) → idempotent insert; one CA wins.
- **Leader failover mid-roll / mid-sign** → any new leader can decrypt the CA
  key (it has the token) and continue signing.
- **Clock skew** → `NotBefore` backdated 5m; document that gross skew breaks TLS.
- **Wrong token on a would-be signer** → cannot decrypt the CA key → logs +
  cannot sign (it should not have become a signer; covered by the same path).

## Security analysis

- The **token is the root of trust** (same as today — it was already the shared
  secret). Compromise grants CA-key decryption + bearer auth. Acceptable given
  the chosen bootstrap model.
- The token is **never transmitted**; only HKDF outputs and HMACs derived from
  it touch the catalog. CSRs/certs are not secret.
- The CA private key at rest is AES-256-GCM-sealed under the KEK, so a catalog
  reader without the token cannot sign.
- A rogue dqlite member **without** the token cannot get a leaf signed (CSR MAC
  fails) and cannot use the CA key. dqlite membership alone is insufficient.
- Leaf private keys are generated per node and never leave it.
- The design does **not** depend on the dqlite transport being encrypted: the
  only secret that transits it is the already-encrypted CA-key ciphertext.
- Use `hmac.Equal` (constant-time) for MAC verification; also fix the existing
  non-constant-time bearer-token compare (`== h.token`) while in this area.

## Test plan

Unit (`internal/dispatch/pki`):
- HKDF determinism; AES-GCM seal/open roundtrip; open fails under a wrong KEK.
- CA genesis produces a valid CA cert that signs verifiable leaves.
- Leaf template has both EKUs, correct validity, `IsCA=false`.
- Signer verifies the CSR signature and **ignores** CSR-requested extensions
  (a CSR asking for `CA:TRUE` yields a non-CA leaf).
- CSR-MAC accept/reject (constant-time).
- Trust-pool union across multiple CA generations; a leaf under an old gen still
  verifies against a pool containing both.
- Renewal/roll trigger logic (time-based, deterministic with an injected clock).

Integration (`test/`, `-tags=integration`, real store):
- End-to-end enrollment: a node obtains a signed leaf; a second node trusts it.
- Genesis race is idempotent.
- Leader failover: signing continues under a new leader.
- CA roll: both CAs trusted during overlap; new leaves under the new gen.
- Token mismatch → enrollment rejected.
- BYO override path unchanged (explicit files bypass provisioning).

3-node k8s confirmation (deploy with only the token set, no cert files; confirm
the cluster comes up mutually-authenticated and a job completes) is the final
acceptance check — run separately, not in CI.

## Rollout

Ships behind the existing owner-mode gate; no behavior change for
`RUN_OWNER_ENABLED=false`. Once verified, it removes the cert-provisioning
friction that blocks making owner+in-memory the distributed default.
