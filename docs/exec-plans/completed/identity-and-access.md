# Identity and Access — Namespaces, Grants, Policy-as-Code, Corporate SSO

Last updated: 2026-10-31

Caesium's authentication is shipped and native (OIDC, SAML, LDAP; `docs/identity-and-access.md`),
but authorization is a single global role ladder keyed per HTTP route, SSO users cannot be
scoped, there is no tenancy primitive anywhere, SSO admins cannot manage API keys, users have
no lifecycle, and no SSO path has end-to-end coverage. That is not usable by a corporation
that wants Active Directory, LDAP, SAML or Keycloak identity to flow through into
per-team permissions.

This plan ships the design in
`docs/design/2026-09-16-identity-and-access-design.md`: a flat **namespace**
declared on the job manifest and carried by every job-owned resource; **grants**
(`role @ namespace`, `*` for cluster-wide) resolved per request from one **policy-as-code
file** that binds IdP groups or user emails to roles and declares each namespace's
isolation settings (Kubernetes namespace and service account, secret allow-list, run
quota); enforcement at the existing middleware chokepoint plus one shared collection
filter; bounded-staleness **IdP group refresh**; **user administration**; namespaced
notification channels, policies and agent profiles; a **Keycloak** CI lane that drives
real logins; a namespace switcher and Access page in the Console; and `caesium login`
for an SSO-backed, user-attributed CLI credential. With no policy file the server
keeps the existing single-namespace behavior. The pre-alpha schema change requires
all nodes to stop and upgrade together; mixed-version rolling upgrades are unsupported.

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
[`.codex/skills/exec-plan-wave/`](../../../.codex/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.codex/skills/draft-exec-plan/`](../../../.codex/skills/draft-exec-plan/).

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

When this plan and `docs/design/2026-09-16-identity-and-access-design.md`
disagree, the spec wins. When the spec and `pkg/jobdef/definition.go` disagree about
the manifest contract, the schema wins once Stream B1 has landed. Per-namespace run
fairness was parked by `docs/exec-plans/active/window-scheduling.md` (its §"Global
vs. per-job load ceiling"); Stream D of this plan owns the v1 slice (issue #395) and
window-scheduling's parked note points here.

## Progress (as of 2026-10-31)

W1 merged on 2026-10-13: A1 (#624), B1 (#625), the separately authorized DB
repair (#626) and the W1 plan sync (#622). W2 merged on 2026-10-16: A2 (#638
`48da0880`), D3 (#640 `2646b0a9`), B3 (#639 `f733fcc0`) and the W2 plan sync
(#641 `53b375b4`). Every review comment was addressed before merge, including a
user review round on #638 and a Codex atomicity finding on #639, which was fixed
rather than documented. W3 merged on 2026-10-16: E1 (#646 `b1c8eef3`), A4 with
H-2 (#645 `831e5229`), A3 (#647 `3d065b68`) and N-1 (#648 `608fe6a3`); LDAP group
refresh was split out of E1 into the new item E1b.
W4 merged on 2026-10-17/18: the W4 base (#650 `141ac76b`), E3 (#655 `36de6e9e`), A5
(#654 `5f5fb92c`), C4 (#660 `5058dcde`), B2 with H-1 (#657 `dd4feda0`) and B4 (#661
`2a0cffef`). The W4 plan sync was not written when the wave merged; the W4 section below
records it. W5 ran on 2026-10-20/21: E4 (#665 `8071932d`), G1 with G5 (#666 `1fc4951d`),
F1 with F3 (#667 `9a8c8b93`), D4 (#673 `cd281a51`), B9 (#668 `1697ee78`) and D1 with D2
(#672 `983e78a0`) are merged; the user merged #665 and asked for admin merges of the
other five. B2 was split during W4: confined job writes and trigger tenancy moved to the
new item B9, and B4 added B7 (DAG snapshot namespaces). Every review comment, including parallel user and greptile reviews, was answered before merge.
W6 merged on 2026-10-22 by admin merge at the user's request: B7 (#682 `29d1f175`), B6 (#683
`41ec13cf`), B5 (#685 `b155ed6f`), A8 (#681 `ce68f819`), C1 with C5 (#679 `dcefc911`) and E5
(#684 `d59be1cc`). A6 moved from W6 to W7.
W7 merged from 2026-10-22 to 2026-10-28: the W6 follow-ups (#687 `1f41ca73`), A6 (#688
`ef5049e4`), C2 (#689 `1a4963df`), E1b with E2 (#691 `36dde0c3`) and F2 with F4 (#690
`94b4b62d`). The W7 plan sync was not written when the wave merged; the W7 section below
records it. W8 was dispatched on 2026-10-22, stopped that evening on the weekly usage limit
and resumed on 2026-10-31: G2 (#697 `234a1b31`), C3 (#699 `c232977a`), the unplanned
snapshot retry (#701 `b1bd3f79`), A7 (#700 `f40e7393`) and G3 with G4 (#703 `fd1fe7c4`).
N-2 closes the plan in the same change as this sync. Every acceptance criterion holds;
see the W8 section for the checks and for what the plan leaves open.

### Wave 1 — Foundations (2026-10-09)

- **W1-α / A1 — policy package:** merged [PR #624](https://github.com/caesium-cloud/caesium/pull/624)
  as `e618f921` on 2026-10-13 (reviewed head `4417f69ddc1be034674d304ed2fee03ba44b2ed3`). Independent security review
  passed after email-confusable, traversal, Vault delimiter and strict YAML fixes.
  Current-head hosted lint and both full race unit suites passed, including all
  security regressions (policy coverage 99.6%); the tested merge has the same tree.
  Current-head hosted CI is fully green, including runtime lanes, coverage and
  `ci-ok`; the primary coverage receipt reports no issues or uncovered changed
  paths. Full integration across all three AMD64 CI shards passed (267 passed,
  43 lane-specific skips). A2/C4 follow-up
  obligations are recorded below; existing review threads await human review.
  At preceding `28586d80`, local lint/race unit tests and hosted CI passed. The
  first local integration failed at queue reclaim with later DB lock/transaction
  errors; unchanged base and exact-head revalidation passed (269 passed, 41
  lane-specific skips; recheck 886.772s). Original failed evidence is preserved;
  the confirmed base pool defects do not establish its causal chain.
- **W1-β / B1 — namespace persistence and cache identity:** merged
  [PR #625](https://github.com/caesium-cloud/caesium/pull/625) as `062c5bba` on 2026-10-13
  (reviewed head `e5380a79a5a05ff307cbf5ede9d855daffc67c5d`). Independent review passed after
  correcting namespace-only CLI/REST/approval diffs and explaining unsafe replay
  refusal. Real schema CLI, namespace incident recovery, and enhanced cache/diff/
  replay journeys are required by coverage; ten selection guard tests passed.
  All coverage floors remain unchanged. Current-head hosted lint and both full
  unit suites passed; 20 live backend/auth scenario proofs passed, including
  namespace move/diff/replay on all five runtime lanes. Fresh full local integration
  passed (877.002s; 273 passed, 42 lane-specific skips); hosted CI, coverage and
  `ci-ok` passed with no uncovered changed paths. Current-head schema guardrails
  and the pinned container CLI validated all 30 example definitions.
  At preceding `41e0cb0d`, full local lint/race unit/integration
  passed (801.740s; 272 passed, 41 lane-specific skips), schema/examples passed,
  and hosted logs proved 15 namespace scenario/engine combinations without skips.
  That head's coverage failed on three changed files; the new journeys and removal
  of an unused model formatter address those gaps. A populated native-dqlite
  upgrade from the base image passed through the public API: strict constraints,
  default ownership, historical identities/event times, incident rekey/dedup,
  namespace moves and restart idempotence. The fixture used one native node and
  one DB shard; populated hot/cold-shard and PostgreSQL upgrades are unqualified.
  Owned containers/network were cleaned; cold backup and evidence are retained.
  Strict constraints and coordinated upgrade guidance travel together in this PR.
- **Separately authorized DB repair:** merged [PR #626](https://github.com/caesium-cloud/caesium/pull/626)
  as `4d7a7c98` on 2026-10-13 (reviewed head `a8e92a34f9975dc9f847722c6184f8847f709da1`). Independent review passed after
  adding a fresh-installer regression, removing unnecessary reinstallation logic,
  and hardening routing tests/fixtures/documentation. Actual SQL detects a deliberate
  Config-only wiring mutation; restored package race tests and vet passed. Full
  local lint, full race unit and fresh-image integration passed (817.924s;
  269 passed, 41 lane-specific skips). Current-head hosted CI is fully green,
  including both unit architectures, coverage and `ci-ok` (41 checks passed,
  three expected skips); the tested merge has the same tree. The preceding
  `839e8e0c` passed all hosted checks and full
  local integration (822.055s; 269 passed, 41 lane-specific skips). This repair does
  not change identity dependencies or prove the original failure's causal chain.
- **A2 and D3:** not dispatched in W1 because their prerequisites had not merged.
  Both went into W2 after A1 and B1 merged.
- **Plan sync:** merged [PR #622](https://github.com/caesium-cloud/caesium/pull/622) as `ccff3bc0`.

### Wave 2 — Grants, shared-object namespaces, git-sync allowlist (2026-10-15)

Each stream was implemented from a brief grounded at master `b796b567`. Each brief
was adversarially critiqued and revised before dispatch. Every PR had a pre-PR
Codex adversarial review, an independent three-lens review (security, data and
migration, surface and tests) in which every finding faced two or three
refuters, and Codex rounds on the PR head. Each stream agent ran lint, the full
race unit suite, `integration-test` and `integration-test-agent` under the host
lane lock. The orchestrator also ran the same chain on the combined tree
(A2 + D3 + B3 merged onto `b796b567`; see below).

- **W2-α / A2 — grants on the principal:** merged [PR #638](https://github.com/caesium-cloud/caesium/pull/638)
  as `48da0880` on 2026-10-16 (reviewed head `25a10ae0`). `Principal.Role` is replaced by `Grants` (`RoleIn`, `ClusterRole`,
  `AllowedNamespaces`; empty grants are never "unrestricted"). Grants are resolved
  per request through a new `policy.Source`. `RoleMapper` is the env adapter,
  checked against the pre-A2 `Resolve` by an oracle test. SSO login denies with
  `no_binding`. `APIKey.Namespaces` is NULL ⇒ `["*"]`, and undecodable, `null` or
  `[]` values fail closed. `User.EmailVerified` is an untagged nullable bool. No
  policy file means byte-identical RBAC, audit and metric labels.
  - **Review round (user review on #638):** `RotateKey` now copies `Namespaces`
    byte-for-byte, so rotating a confined key keeps it confined. The bootstrap
    refresh resets `namespaces` to NULL. `AdminKeyExists` counts only keys whose
    grants give admin at `*`. New live auth-lane scenarios:
    `TestAuthKeyRotationKeepsNamespaces` (a rotated `["marketing"]` key still gets
    403) and `TestAuthSessionCookieWithoutGrantsIsRefused` (a real session cookie
    for a user stored as `role=admin` with no resolvable grants gets 403 and an
    `insufficient_role` audit row).
  - **Evidence:**
    - lint, unit and `integration-test` (824s) all pass;
    - `integration-test-agent` passes 48 scenarios, including the three new
      `TestAuth…` scenarios;
    - hosted CI, `coverage-ratchets` and `ci-ok` are green on `25a10ae0`;
    - Codex (pre-PR, round 1 and round 2) and two independent multi-lens
      reviews found no open defects.
  **Decision for sign-off:** in v1 only OIDC `email_verified=true` email is
  trusted. SAML and LDAP email never satisfies `users:` bindings; groups still
  work everywhere. Obligations recorded in the PR:
  - G3: a live per-request-resolution proof.
  - A4/G1: a live rejection of an unverified email.
  - A5: minting is limited to a subset of the caller's grants; `key_create.go`
    still checks `Role`. Rotation, bootstrap and `AdminKeyExists` were fixed in
    #638.
  - Interim: whoami shows `role:""` for a confined-only principal until A3 and A5.
- **W2-β / D3 — namespaces on shared objects:** merged [PR #640](https://github.com/caesium-cloud/caesium/pull/640)
  as `2646b0a9` on 2026-10-16 (reviewed head `8fd784c8`).
  - **What changed:** `NotificationChannel`, `NotificationPolicy` and `AgentProfile`
    carry a create-only `namespace` (text, not null, default `default`, indexed; `*`
    is cluster-shared). A PATCH that changes it returns 400. A policy may reference
    only a visible channel. Server-side lint and apply (and so git sync and the
    incident patch, through `Importer.ValidateBatch`) reject a profile or escalation
    channel outside the job's namespace or `*`. The rule lives in
    `internal/jobdef/agent_profile_refs.go`, not `internal/jobdef/lint/`. A3 must
    reuse `models.NormalizeObjectNamespace` for POST bodies.
  - **Triage-only profile:** the built-in `triage-only` profile is seeded and
    migrated to `*`. Spec §10.4 and §15 are amended in the same PR, by orchestrator
    decision.
  - **Native dqlite probe:** native dqlite honours `PRAGMA foreign_keys = OFF`
    (read-back 0). SET NULL and CASCADE child rows survive the pre-existing
    parent-table rebuilds.
  - **Evidence:** lint, unit, `integration-test` and `integration-test-agent` (the
    three new scenarios pass on their lanes) all pass. Hosted CI is 41/41 green.
    Codex (pre-PR and round 1) and the three-lens review found no defects.
- **W2-γ / B3 — git-sync namespace allowlist:** merged [PR #639](https://github.com/caesium-cloud/caesium/pull/639)
  as `f733fcc0` on 2026-10-16 (reviewed head `26bfe040`).
  - **What changed:** `GitSourceConfig.Namespaces`. Fail-open spellings (`null`,
    singular `namespace`, case-variant duplicates) are rejected at decode time.
    `[]`, `*`, a missing `source_id` and a shared `source_id` are rejected at
    startup. The requested and stored namespaces are both enforced by the
    in-transaction `guardJobMutationTx` (ahead of `Force`). `PruneMissing` is
    confined, and skips are reported. A read-only `StoredNamespaces` pre-filter
    runs before `ValidateBatch`. Only allowlisted sources write a
    `jobdef.git_sync` audit row (`system:git-sync`); unrestricted sources are
    unchanged.
  - **Atomic per-commit apply:** an allowlisted source applies each commit's
    allowed manifests in ONE transaction (`ApplyBatchWithOptions`).
    - An in-transaction namespace refusal writes nothing.
    - The stored trigger and dataset graphs are validated inside the
      transaction.
    - Any failure rolls the whole commit back, so a refusal can never leave a
      partial batch or a persisted cycle.
    - Unrestricted sources keep the per-manifest flow.
    - Trade-off: the dqlite writer is held for the batch, once per new commit,
      and only for allowlisted sources.
  - **Review fixes:**
    - Guarded rows are locked (`FOR UPDATE` on Postgres). Batch and prune
      lock in one alias order, and Postgres `40P01`/`40001` are retried.
    - Only genuine graph verdicts are reported as an invalid batch; context
      cancellation and DB errors surface as themselves.
    - `CAESIUM_JOBDEF_GIT_SOURCES` is redacted from env decode errors, which
      previously leaked inline git credentials.
  - **Live scenario:** `TestJobdefGitSyncNamespaceAllowlistSkipsForeignManifests`
    runs on the coverage-ratchets `git-sync` lane. `file://` against
    `integration-up` is infeasible.
  - **Reviews:** Codex rated the atomicity finding P1 in rounds 1–3. It is now
    fixed (round 4 approves `26bfe040`). The independent three-lens review of the
    atomic batch confirmed the transaction, lock-order and compat work. Its two
    small follow-ups (a dataset-graph rollback test and graph-verdict-only error
    labelling) are in `26bfe040`.
- **Combined gate:** the final heads (A2 `25a10ae0` + D3 `8fd784c8` + B3 `26bfe040`)
  merge cleanly onto master `2615c874`, which already includes #636 (81 files).
  The full chain on that combined tree passed: lint (0 issues), the full race unit
  suite, `integration-test` and `integration-test-agent`. That is 311 scenarios
  passed and 50 lane-specific skips, with no failures.
  - All five new `TestAuth…` scenarios pass on the auth lane.
  - D3's three scenarios pass on their lanes.
  - Both git-sync scenarios skip outside the coverage `git-sync` lane by design.
    They pass in hosted `coverage-ratchets`.
  An earlier gate on `b796b567` with the first heads also passed (309 / 47).
- **Plan-doc conflict rules updated:** `internal/models/user.go` (A2 → E1),
  `pkg/env/env.go` (B3's redaction), `internal/auth/audit.go` (B3),
  `internal/jobdef/lint/` (D3 did not touch it), and the new
  `internal/models/namespace.go` line.
- **Follow-ups filed by this wave:**
  - D4:
    - reject undeclared namespaces when a shared object is created (needs the
      A4 policy holder);
    - global object names: a cross-namespace 409 discloses that the name exists
      elsewhere, and names can be squatted;
    - with A8, runtime by-name profile resolution in
      `internal/incident/playbook_resolve.go` ignores namespace.
  - B2: the git-sync coverage lane needs a policy fixture that declares
    `marketing` once undeclared namespaces are rejected.
  - Git sync: make invalid-batch failures non-fatal (a per-source degraded
    state) instead of stopping the server. This is pre-existing: an invalid
    commit still stops the server, but an allowlisted source now rolls the
    commit back first.
  - Keys:
    - `AdminKeyExists` still counts job-scoped admin keys;
    - `RotateKey` drops `ExpiresAt`, so a TTL'd key rotates to a non-expiring
      one;
    - rotating a key with malformed `namespaces` JSON copies it as-is, and the
      response then fails to serialise (reachable only through DB corruption).
    All three are pre-existing.
  - Boot cost: dqlite rebuilds default-tagged tables several times per
    AutoMigrate. This is pre-existing and safe per the D3 probe, but slow.
  - A4: warn when a policy has `users:` bindings but no provider can supply a
    trusted email. N-1: document which providers can.
- **Plan sync:** merged [PR #641](https://github.com/caesium-cloud/caesium/pull/641) as `53b375b4`.
  It landed before #639 and still described W2 as open; this follow-up records
  the merges.
- **Next wave (W3 candidates, unblocked by W2):** A3 (needs A2 + D3), A4 (A2) and
  E1 (A2), as the original W2 suggestion planned, then the W3 row of the
  suggested-waves table.

### Wave 3 — Route classes, policy loader, refresh grant, guide (2026-10-16)

Briefs were grounded at master `3194fe68` and critiqued by Codex before dispatch
(A3: 6 P1 + 6 P2, A4: 1 P1 + 5 P2, E1: 5 P1 + 4 P2, all folded in). Every PR had
pre-PR Codex rounds, Codex rounds on the PR head and an independent multi-lens
review whose findings each faced three refuters. All review threads, including a
parallel user review, were answered before merge. The four PRs were admin-merged
in the order #646 → #645 → #647 → #648.

- **W3-γ / E1 — refresh grant:** merged [PR #646](https://github.com/caesium-cloud/caesium/pull/646)
  as `b1c8eef3` (head `63342d74`). One AES-256-GCM refresh grant per user under an
  HKDF-SHA256 key from `CAESIUM_AUTH_KEY_HASH_SECRET` (AAD = user id), lease and
  fence primitives (CAS on `refresh_grant_version`), and an OIDC `GroupRefresher`
  that keeps rotated tokens, re-checks issuer and subject, validates userinfo time
  bounds, and treats a missing groups claim as unknown rather than empty.
  - **Split (decision for sign-off):** the coverage diff ratchet needs a live
    caller for every changed file, and no LDAP journey exists, so LDAP group
    refresh moved to the new item E1b. The full LDAP work is preserved on
    `iaa-e1-ldap-refresh-preserved` (`cc0cfb87`). Its refresh re-runs the exact
    login filter, rendered with the name the user typed, against the stored DN;
    re-rendering with the username attribute would have revoked every user under
    some valid configurations.
  - **E2/E4 obligations recorded in the PR:** refresh deadline below the lease,
    lease TTL at least twice the timeout, `ErrRefreshGrantInvalid` discards the
    grant, a policy for users with NULL `GroupsRefreshedAt`, an audited
    `sessions_revoked`; E4 extends `revokeUserSessionsTx` to user-bound keys.
- **W3-β / A4 + H-2 — policy loader:** merged [PR #645](https://github.com/caesium-cloud/caesium/pull/645)
  as `831e5229` (head `7cf8d548`). `CAESIUM_AUTH_POLICY_FILE` is parsed at startup
  (fatal on error, or alongside `CAESIUM_AUTH_ROLE_MAPPING`); `policy.Holder` reloads
  on a stable content-hash poll (not mtime) and on `POST /v1/auth/policy/reload`
  with serialised writers; `policy.reloaded` / `policy.reload_failed` are audited at
  namespace `*`; `GET /v1/auth/policy` and `caesium auth policy lint|show|reload`
  are new. A no-file reload answers 409, a rejected file 422. H-2's
  `runCLIJSON[T]` / `runCLIJSONFailure[T]` helpers ship here. The agent lane mounts
  a reload fixture; H-1 adds the two-namespace fixture to both servers later.
- **W3-α / A3 — route classes:** merged [PR #647](https://github.com/caesium-cloud/caesium/pull/647)
  as `3d065b68` (head `a0dfbd3b`). Every route has a role and a class (identity,
  cluster, namespaced, collection); a principal holding the role at `*` is decided
  exactly as before (an oracle test checks every route and role against master's
  decision, with no DB read). Routes whose handlers can still return another
  namespace's rows are fail-closed behind `pendingOn("<item>")` (B2, B4, B5, B6,
  D4, A5, A6, A8). New `internal/authz`, `AuditLog.Namespace` with
  `caesium auth audit --namespace`, and `run_start_idempotency.namespace` so a
  confined idempotent replay is authorised before its request is compared (D9).
  Fixed on the way: concurrency-admitted runs were persisted in `default`.
  - **Master moved under the wave:** #644 added six connector read routes. The
    five `/v1/connectors*` reads are cluster routes (their handler already answers
    only a cluster viewer); a run's external executions is a namespaced run route
    that lists links only to a cluster viewer. `TestAuthNamespacedRoutesConfineKey`
    drives a confined key through both.
  - **Accepted residual risk (decision for sign-off):** a concurrent namespace move
    of the exact target can land between the namespace check and the handler; run
    admission and replay re-check in their own transactions, and B2/B4 own
    in-transaction guards for the other mutations.
  - **Migration note:** keyed starts recorded before the upgrade carry no
    namespace, so a namespace-confined key retrying one whose start was skipped or
    dropped gets 403.
- **W3-δ / N-1 — the guide:** merged [PR #648](https://github.com/caesium-cloud/caesium/pull/648)
  as `608fe6a3`. `docs/sso-authentication.md` became `docs/identity-and-access.md`
  and grew into the operator guide (policy file, namespaces and route classes,
  keys, users and sessions, audit, migration). A 72-agent fact-check against the
  combined code kept 23 corrections; unshipped features are short **Planned**
  paragraphs naming their item. Rebased onto the merged code before merge.
- **Combined gates:** the full chain (lint, race unit suite, `integration-test`,
  `integration-test-agent`) passed three times on combined trees: all four heads on
  `3194fe68` (agent lane 58), the final heads on `3194fe68` (319 / 59), and
  master `831e5229` + A3's merge resolution (380 passed, 0 failed; agent lane 60).
- **CI flakes:** the stress smoke `terminal_oom_unconfirmed` (#645, #646);
  `pkg/dqlite` `TestFreshOrdinalZeroJoinsLiveClusterThroughBootstrapPeers` (#645);
  an early-evidence kind probe (#647); and on #647's final head a
  `No such image: debian:12-slim@sha256:…` in one integration shard. All were
  rerun green.
- **Follow-ups filed by this wave:**
  - Docker engine: the image pull reads the progress stream with `io.ReadAll` and
    ignores in-stream errors, so a failed pull is logged as pulled and surfaces as
    "No such image" at container create (`internal/atom/docker/engine.go`).
  - B4: filter `LatestRunForLogicalDate` by namespace (backfill date dedupe).
  - Clear the `pendingOn` markers as B2, B4, B5, B6, D4, A5, A6 and A8 land.
  - A policy file keeps existing SSO sessions alive after an SSO provider is
    disabled (documented in the guide; revisit with E3).
- **Next wave (W4 candidates):** the suggested W3 row's remaining items — A5, A8,
  B2, B5, C4, D1, E3, H-1 — and E1b; confirm each item's `Depends on` at dispatch.

### Wave 4 — Keys, user administration, declared namespaces, secret scope, collection filtering (2026-10-17)

Briefs were grounded at master `0fd86c24` plus the W4 base and critiqued by Codex before
dispatch. Every PR had pre-PR Codex rounds and a Codex round on the PR head. The orchestrator
ran lint, the race unit suite, `integration-test` and `integration-test-agent` on combined
trees. After a usage-limit interruption the wave ran without workflows: Codex reviews and lean
finisher agents only. The six PRs merged in the order #650 → #655 → #654 → #660 → #657 →
#661, each restacked onto the previous merge before CI.

- **W4-base — process policy holder:** merged [PR #650](https://github.com/caesium-cloud/caesium/pull/650)
  as `141ac76b` on 2026-10-17. `policy.SetProcessHolder` / `ProcessHolder` is the process-wide
  holder, installed by `initAuth` in every auth mode; `nil` behaves as the empty policy. B2,
  B4, C4, D1 and C1 read namespace settings through it. Merged by the user first.
- **W4-β / E3 — user administration:** merged [PR #655](https://github.com/caesium-cloud/caesium/pull/655)
  as `36de6e9e` on 2026-10-17. `UserAdmin` lists, disables, enables and logs users out;
  disable revokes sessions, clears the refresh grant and advances the new nullable
  `users.disable_generation` in one transaction, and a login is fenced on that generation.
  Six cluster routes, `caesium auth user list|get|disable|enable|logout`, and audit rows
  `user.disable`, `user.enable`, `auth.sessions_revoked` at namespace `*`. Live:
  `TestAuthUserAdministrationLifecycle`; PostgreSQL 16 opt-in run passed. Codex r3 approve.
  - **Decisions for sign-off:**
    - The fence is a new `disable_generation` column instead of `refresh_grant_version`
      (deviates from "no new column"): fencing on a counter every login bumps would refuse
      concurrent logins and break E1's pinned test.
    - E3-D5: self-disable answers 409; no break-glass guarantee is claimed.
    - E3-D6: the CLI addresses users by id only, because emails are not unique across issuers.
- **W4-α / A5 — namespaced keys:** merged [PR #654](https://github.com/caesium-cloud/caesium/pull/654)
  as `5f5fb92c` on 2026-10-17. `caesium auth key create --namespace` / `--jobs`;
  `POST /v1/auth/keys` accepts `namespaces`. A namespace admin mints, lists, rotates and
  revokes only inside its namespaces and cannot mint above its own role; an SSO admin's
  session can manage keys (it got 401 before). `GET /auth/whoami` adds `grants`, `namespaces`
  and `cluster_role`. Live: `TestAuthNamespacedKeyMintingCLI`, `TestAuthSSOAdminManagesKeys`.
  - **Behaviour change (all callers):** a `scope.jobs` that names no job and a `scope.agent`
    without an `incident_id` now answer 400 (master minted an unscoped key). Rotation keeps
    the old expiry, never moves it later, and answers 409 for an expired key or unreadable
    namespaces.
  - **Decisions for sign-off:** explicit `["*"]` is stored as NULL; undeclared namespaces are
    rejected only when a policy file is loaded (same rule as B2-D1); whoami `role` equals
    `cluster_role`, so it is `""` for a confined principal until F1 reads `grants`.
- **W4-ε / C4 — scoped secret resolution:** merged [PR #660](https://github.com/caesium-cloud/caesium/pull/660)
  as `5058dcde` on 2026-10-17. `secret.ScopedResolver` matches each reference against
  `namespaces.<ns>.secrets.allow` before and after the cache lookup, on the local executor,
  fan-out, the distributed worker, replay planning, the HTTP trigger, server lint and apply
  preview. A denial is terminal, redacted, classified `config`, and refuses a replay with 422.
  Live: `TestAuthSecretScope`, `TestAuthSecretScopeWebhook`. Codex r5 approve.
  - **Decisions for sign-off:**
    - Distributed residual: each worker enforces its own node's copy of the policy file until
      C2 carries the isolation context in dispatch; a node with no file fails open.
    - A relative `k8s/<x>` rule expands against the mapped Kubernetes namespace; when that
      differs from the resolver's namespace the reference is denied until C1.
    - An undeclared namespace on a file-backed node is denied.
- **W4-γ / B2 + H-1 — declared namespaces, confined lint/diff, policy fixture:** merged
  [PR #657](https://github.com/caesium-cloud/caesium/pull/657) as `dd4feda0` on 2026-10-17.
  With a policy file, server lint, apply, git sync, incident patches and legacy `POST /v1/jobs`
  reject undeclared namespaces. `POST /v1/jobdefs/lint` and `/diff` go live for confined
  callers with a baseline confined to their namespaces and foreign identities redacted; diff
  gains `namespaces`. `test/testdata/policy/two-namespaces.yaml` is mounted on
  `integration-up` and every parity-tracked recipe. Apply, `POST /v1/jobs`, `PATCH
  /v1/triggers/:id` and `GET /v1/jobs/:id/manifest` stay pending, now on B9. Codex r5 approve.
  - **Decisions for sign-off:**
    - B2-D1: undeclared namespaces are rejected only while a policy file is loaded, so podman,
      kind/helm, coverage-journey lanes and post-upgrade re-applies keep working. Design §5.1
      and §6.4 carry the amendment, marked pending maintainer sign-off.
    - B2 split: confined writes and trigger tenancy moved to B9.
    - A confined caller linting an undeclared namespace gets 403, not a lint error.
- **W4-δ / B4 — namespace filtering of collections and history:** merged
  [PR #661](https://github.com/caesium-cloud/caesium/pull/661) as `2a0cffef` on 2026-10-18.
  Jobs, triggers, incidents, stats and the event stream are filtered in SQL before paging;
  history under a job is filtered by each row's persisted namespace; ingested events show a
  delivery only when every run it started is readable. Retry of a run whose job has moved
  answers `409 namespace_moved`. `?order_by=` on the job and trigger lists is an allow-list.
  Every B4 `pendingOn` is cleared; `blame`, `topology` and `topology/history` move to B7.
  Codex pre-PR r1 to r6 fixed; the JSONB delivery SQL was checked by inspection only.
  - **Decisions for sign-off:**
    - Backfill date dedupe counts only runs in the backfill's own namespace (behaviour change
      for moved jobs, every caller).
    - `409 namespace_moved` applies to every principal, unrestricted ones included (spec §8.3).
    - Verdicts recorded before the upgrade carry no `baseline_scope`, so a confined caller
      loses their baseline evidence; new verdicts carry `baseline_scope: "namespace"`.
      Orchestrator decision after Codex P1 (thread 4303672789).
    - After a move, a job's `deltaFromBaseline` assertions start with no baseline.
- **New items B9 and B7:** B9 (confined job writes and trigger tenancy, after B2) and B7
  (namespace-aware DAG snapshots, after B4 and B9) were created by W4. B9 landed in W5 (below).
- **Combined gates:** four combined trees, each lint + race unit + `integration-test` +
  `integration-test-agent`, all green; the last (all five feature PRs) had unit 6623,
  integration 387, agent lane 74. Each PR's chain was rerun on its restacked head. No CI flakes
  were recorded.
- **Follow-ups filed by this wave:**
  - B9 (done in W5-α, with the strict prune mode still open): shared triggers, webhook-path
    ownership, one transaction for a confined apply, redaction of apply's findings.
  - B7: `dag_snapshots` namespace column, importer dedupe key, then clear the three routes.
  - A8: confine the incident `allowed_jobs` allowlist and incident detail.
  - B5/B6: reopen the withheld event types (dataset, freshness, contract, agent, approval,
    escalation) once their payloads are confined.
  - C5: `caesium why --json` surfaces the `config` class and the multi-namespace live
    fixture. C2: carry the isolation context in dispatch. C1: align relative
    `secret://k8s/<secret>/<key>` references with relative rules.
  - D1: enforce marketing's `maxConcurrentRuns: 1` and add `409 namespace_quota` (done in
    W5-δ). D4: the notification `auditActor` reads `GetAuthKey` only (done in W5-ε).
  - E4: join user-bound keys to `UserAdmin` and `revokeUserSessionsTx` (done in W5-β). E2:
    consider `disable_generation` on an IdP `invalid_grant`. G3: drive the E3 lifecycle with a
    real Keycloak user; watch the shared-IP auth failure limiter for 429s.
  - Git sync: the non-fatal invalid-batch follow-up also covers a policy reload that removes a
    namespace a source still uses.
  - No PostgreSQL lane exercises the key row locks or the delivery-ownership SQL.

### Wave 6 — Snapshots, datasets, aggregates, agent sessions, Kubernetes target, CLI login (2026-10-22)

Briefs were grounded at master `805f3252` and critiqued by Codex before dispatch; every P1
and P2 was folded in. Each PR had pre-PR Codex rounds, Codex rounds on the PR head and a
sweep of greptile threads before merge. The six PRs were merged in order onto a scratch
branch, conflicts resolved by union, and the combined tree passed lint, unit tests, both
integration lanes (402 and 113 scenarios), the scripts suite and the docs guardrails. Each
PR was then updated to that tree and admin-merged at the user's request. No Workflow
fan-outs were used.

- **W6-α / B7 — namespace-aware DAG snapshots, plus canonical aliases:** merged
  [PR #682](https://github.com/caesium-cloud/caesium/pull/682) as `29d1f175`. Snapshots carry
  the namespace they were written in and dedupe on it, so a move writes a fresh snapshot.
  Legacy NULL snapshots are cluster-only. Blame, topology and topology history are live for
  confined viewers and computed over the snapshots they may read; a commit only an
  unreadable snapshot carries answers 404. `pkg/jobdef.ValidateAlias` refuses leading or
  trailing whitespace in `metadata.alias` for every writer (400); git sync skips such a
  manifest on its own and protects its working job from prune (greptile P1, fixed).
- **W6-γ / B6 — dataset tenancy via declared producers:** merged
  [PR #683](https://github.com/caesium-cloud/caesium/pull/683) as `41ec13cf`. A dataset belongs
  to the current namespaces of its declared producers; the dataset and hold lists filter in
  SQL; single-dataset reads withhold what another namespace owns (derivations by a new
  producer-namespace stamp, metrics by run, the state reason by its author, hold runs).
  Advance and hold release need the role in every producer namespace, re-checked in the
  write transaction (advisory lock plus `FOR SHARE` on PostgreSQL). A foreign and a
  nonexistent dataset get the same audited 403. Nine pre-PR rounds and a fixer closed
  producer-move races, including a start-parameters fence at admission.
- **W6-β / B5 — aggregate redaction:** merged
  [PR #685](https://github.com/caesium-cloud/caesium/pull/685) as `b155ed6f`. Lineage impact
  and the contracts graph redact foreign nodes to `{id, kind: "external"}` (impact
  placeholders keep `depth`, a one-clause amendment to design §8.3) with per-response HMAC
  ids, keep collapsed topology, and require every merged edge contributor to be readable.
  A root the caller does not own answers like a nonexistent one. The Console renders the
  placeholder.
- **W6-δ / A8 — agent sessions confined:** merged
  [PR #681](https://github.com/caesium-cloud/caesium/pull/681) as `ce68f819`. The frozen
  allow-list keeps only the incident's namespace (foreign downstream jobs are counted);
  every agent read is confined at serve time by the run's persisted namespace; a moved-out
  job's DAG and remediation are withheld; jobdef patches cannot move a job; session content
  written before A8 is hidden from confined readers. A6 was split out to W7.
- **W6-ε / C1 + C5 — Kubernetes target per request, secret scoping end to end:** merged
  [PR #679](https://github.com/caesium-cloud/caesium/pull/679) as `dcefc911`. Kubernetes steps
  run in the namespace and service account their run namespace maps to, resolved once per
  execution before the cache identity; new pod runtime ids are `<namespace>/<pod>`; relative
  `secret://k8s` references read in the mapped namespace; replay compares targets.
  `TestSecretScopeMarketingAllowList` proves C4 end to end, and `caesium why --json` gains an
  additive `failureClass` (omitted when unknown).
- **W6-ζ / E5 — `caesium login`, `logout`, `whoami`:** merged
  [PR #684](https://github.com/caesium-cloud/caesium/pull/684) as `d59be1cc`. Durable login
  transactions, a CSRF-protected confirmation, and a PKCE-checked exchange that mints the
  user-bound key; the key is minted at exchange, not at authorize (design §13.1 amended).
  `POST /auth/logout` revokes the presented user-bound bearer; every CLI command resolves
  credentials through `cmd/cliutil` (`--api-key` > env > stored login).
- **Decisions awaiting the user's sign-off:** mint at exchange (#684, §13.1); impact
  placeholders keep `depth` (#685, §8.3); additive `failureClass` in `why --json` (#679);
  namespace-qualified Kubernetes `runtime_id` (#679).
- **CI:** three PRs first failed the hosted coverage diff ratchet (changed files reached only
  by scenarios that need a policy file); fixers added policy-free journey scenarios without
  lowering floors. `coverage-ratchets` runs 38–44 of its 45 minutes on master, so a slow
  runner can time it out. Infra flakes: a `docker.service` start failure and a Docker image
  store setup failure, both rerun green.
- **Follow-ups (owner):** W7-ε covers the lint/diff edge-id collision, receipt snapshot
  binding for confined readers, replay copying the old namespace's start view (params and
  descriptors), trimmed-alias producer identity, the PostgreSQL advance CAS and a B9 test
  race. Others: a reaper that uses the multi-namespace `List` (C1); a live kind proof of a
  mapped target (C3); Windows credential-file locking and an OS keychain (E5); the real
  policy-backed browser journey for the quota label (G2); raise or split the
  `coverage-ratchets` timeout (CI).
- **Next waves:** W7 is dispatched on the W6 tree with A6, E1b + E2, C2, F2 + F4 and the W6
  follow-ups. W8: A7, G3, G4, C3, G2, then N-2.

### Wave 5 — User-bound keys, Keycloak lane, Console gating, confined writes, quotas, shared objects (2026-10-20)

Briefs were grounded at master `e658a173` and critiqued by Codex before dispatch (every P1
and P2 folded in; the per-stream counts are in the orchestrator log). Every PR had pre-PR
Codex rounds, Codex rounds on the PR head, and a sweep of user and greptile threads before
merge. Six streams ran on worktree-isolated agents under a host lane lock. No Workflow
fan-outs were used.

- **W5-β / E4 — user-bound API keys:** merged [PR #665](https://github.com/caesium-cloud/caesium/pull/665)
  as `8071932d` on 2026-10-21 (head `b43ff9d9`). `api_keys.user_id` (nullable, indexed) marks
  a user-bound key (`csk_user_` prefix) that stores no role, namespaces or scope.
  `Service.AuthenticateKey` resolves the user's grants from the policy on every request, as
  a session does; the principal is `Kind=user` with a new `UserKeyID` (not `KeyID`, which
  `connector/reads.go` and `replay.principalIdentity` key identity on). A missing, disabled,
  expired, revoked or malformed row all answer the same 401 after the same latency floor.
  `MintUserBoundKey` (the E5 seam) fences on the user's `DisableGeneration`; expiry comes from
  `CAESIUM_AUTH_CLI_KEY_TTL` (default `12h`, at most 7 days). Disable, the enable sweep, "log
  out everywhere" and `CompleteRefreshDefinitive` revoke the user's keys in the same
  transaction as the sessions. `caesium auth user get` and the user detail list active keys.
  Live: `TestAuthUserBoundKeyGrantsFollowPolicy`, `TestAuthUserBoundKeyLifecycle` (61.5 s by
  design, to stay under the runner IP's auth-failure limiter). Codex r1 approve, 0 findings.
  - **Decisions for sign-off:**
    - Logout self-revocation is deferred to E5: E4 ships no mint surface, so no user-bound key
      exists in production, and `sso.go` / `api/api.go` are untouched.
    - "Log out everywhere" (`sessions/revoke`) also revokes user-bound keys, widening design
      §11.2 which names only sessions. Both are sign-in credentials; the guide says so.
    - Until E2, user-bound keys use the groups stored at the user's last login.
- **W5-γ / G1 + G5 — Keycloak lane:** merged [PR #666](https://github.com/caesium-cloud/caesium/pull/666)
  as `1fc4951d` on 2026-10-21 (head `0c37b1b9`; admin-merged at the user's request). Realm
  fixture, lane policy, `integration-up|test|down-keycloak` and `ui-e2e-keycloak` recipes
  (Keycloak `26.7.5`, digest-pinned, loopback-only ports), a `keycloak` matrix leg and a
  `ui-e2e-keycloak` job under `ci-ok`. Five Go scenarios drive the real code flow: admin,
  confined bob, carol denied `no_binding`, dave's unverified-email admin binding refused
  (closes the A2/A4 live-rejection obligation), erin's verified-email viewer binding. One
  Playwright admin login with traces and video off. Container logs go through
  `scripts/lane-logs.sh`, which redacts `csk_` and `css_`. The guide's Keycloak section and
  `docs/ci.md` replace the G5 placeholder. Codex r1 P1 (bind ports to loopback) fixed, r2
  approve; local runs 5/5 PASS four times. Master's push run with it and #671 was green.
  - **Decision for sign-off:** the lane has its own policy file rather than a renamed admin
    group; `requireNoBindingCallback` asserted master's 403 and moved to F1's 302 in #667.
- **W5-ζ / F1 + F3 — Console gating and Access page:** merged [PR #667](https://github.com/caesium-cloud/caesium/pull/667)
  as `9a8c8b93` on 2026-10-21 (head `cdce5525`). The Console principal gains `grants`,
  `namespaces`, `clusterRole` and `roleIn(ns)`; every job action is gated by the role its
  route needs in the job's namespace. New `/access` page (my access, declared namespaces,
  policy revision, users with disable / enable / log out everywhere) and `GET
  /v1/auth/namespaces` (cluster viewer; never bindings, subjects, groups or the reload error).
  A `no_binding` OIDC/SAML denial redirects to `/?login_error=no_binding`; LDAP keeps 403 and
  adds `reason: "no_binding"`. Live: `TestAuthNamespacesRoute`, `access.spec.ts`; `ui-test` 648.
  Codex r1 raised one P2 (live `no_binding` journey), answered by sequencing: #666 covers OIDC
  after rebase; the LDAP body is an E1b follow-up. Two greptile P2s fixed in `08f15ce0`.
- **W5-ε / D4 — shared objects in namespaces:** merged [PR #673](https://github.com/caesium-cloud/caesium/pull/673)
  as `cd281a51` on 2026-10-21 (head `767d0e58`). Channel, policy and agent-profile lists are
  live collections filtered in SQL by the new `authz.SharedObjectFilter` (own namespaces plus
  `*`); the nine create/edit/delete routes are live for confined principals. With a policy
  file, an undeclared namespace on create is 400. `RouteEvent` replaces `MatchPolicies`: an
  event is owned by its incident, hold, run or job namespace and a non-`*` destination gets a
  scoped payload (labels, contract findings and baseline evidence withheld unless provably the
  owner's). A refused job-declared or configured default profile gives the deny-all playbook.
  Policy and profile deletes rename the tombstone `<name>~deleted~<id>`, so names are released.
  The notification audit actor falls back to `principal.Subject`. Codex r3 approve; the pre-PR
  r4 fix had its own regression test and no extra Codex pass.
  - **Decisions for sign-off:**
    - Notification matching and the incident-time profile namespace rule are strict only while
      a policy file is loaded (`FileBacked()`). The orchestrator first chose strict-always
      and was overruled by spec §§1/15 after Codex PR r1.
    - With no file, `Escalate` reports `routed=false` when the channel is disabled or missing
      (it was `true`); channel eligibility is the same check in both modes.
    - A cross-namespace contract break reaches only `*` policies until placeholder collapse (B5/B6).
    - Tombstones are renamed, not deleted, because `agent_sessions.profile_id` would be lost.
- **W5-α / B9 — confined job writes and trigger tenancy:** [PR #668](https://github.com/caesium-cloud/caesium/pull/668)
  merged as `1697ee78` on 2026-10-21 (head `f2d262eb`). Clears `pendingOn("B9")` on
  apply, `POST /v1/jobs`, `PATCH /v1/triggers/:id` and the manifest. Trigger tenancy: a
  confined writer never writes a job whose trigger row or webhook path serves a live job
  outside its reach; with a policy file a webhook path and a trigger row each serve one
  namespace for every writer. `ApplyConfinedBatch` applies definitions and prune in one
  transaction (PostgreSQL: REPEATABLE READ plus session-level advisory locks taken before
  BEGIN); a late refusal rolls everything back as an audited 403. A move needs operator in both
  namespaces. Contract findings and warnings touching an unreadable job are withheld whole.
  B7 and B9 are added to Stream B. Codex PR rounds r1 to r11 each found a real defect (git-sync
  revival race, whitespace alias bypass, snapshot hiding admitted runs, running-state oracle,
  pool starvation, foreign `depends_on`, retry cache reset, trigger PATCH coverage); all were
  fixed with regressions, and r12 approved.
  - **Decisions for sign-off:**
    - A namespace-confined prune is the server-computed set intersected with the caller's
      operator namespaces; spec §9.5 is amended. Unrestricted prune is unchanged.
    - A legacy shared trigger row spanning namespaces is detached on write by a writer
      authorised over every job it serves (the job moves to a fresh row); others are refused.
    - Confined callers (namespace-confined, job-scoped or allowlisted git sync) are refused
      non-canonical (whitespace) aliases with a generic 403.
- **W5-δ / D1 + D2 — namespace quotas and queue reasons:** [PR #672](https://github.com/caesium-cloud/caesium/pull/672)
  merged as `983e78a0` on 2026-10-21 (head `05c701bd`). `namespaceCapacityTx` caps
  `quotas.maxConcurrentRuns` on `admit`, retry readmission and quarantined replay (PostgreSQL:
  transaction-scoped advisory lock per namespace). Trigger and manual starts are parked in
  `run_queue` with `held_reason: namespace_quota` (`202 queued`); retries and replays answer
  `409 namespace_quota` with `Retry-After: 5`; backfills wait with backoff. `DrainOnce`
  round-robins namespaces and jobs. `run_queue.namespace` and `held_reason` are nullable
  columns; a queued request is dropped after its job moves. `GET /v1/jobs/:id/queue` and
  `caesium job queue` show the reason; cancel needs operator in the row's namespace. New
  metrics `caesium_namespace_running_runs` and `caesium_namespace_quota_deferrals_total`. Closes
  the v1 slice of #395. Codex pre-PR r1 to r5 and PR rounds r1 to r5 (CLI retry bypass,
  claim consumed atomically, terminal outcomes consume the claim, orphaned run after promotion,
  recovery callbacks) fixed; r6's remaining P2 (callbacks lost when a committed run never reads
  back) is accepted as a recorded residual. Live: `TestNamespaceQuota*`,
  `TestAuthNamespaceQuotaRoundRobinTwoNamespaces`.
  - **Decisions for sign-off:**
    - The quota dequeuer starts whenever a policy file is configured (quota-only mode touches
      only quota-held rows), so policy-free jobs' held rows drain.
    - An undeclared namespace is refused at start when file-backed: starts, retries and
      replays get `409 namespace_undeclared`; queued rows are dropped; startup refuses
      until the configured file is installed (`run.ExpectNamespacePolicy`).
    - The coverage server caps marketing at 1 (`coverage-secret-glob.yaml`).
- **Combined gates:** the orchestrator gated combined trees of the open PRs plus merged ones
  (first four PRs: scripts 917 OK; later chains int 328 to 333, agent lane 81 to 82, distributed
  54, PostgreSQL opt-in all PASS). Each PR's own chain was rerun after every master merge.
- **Session-limit kills and finishers:** the shared usage limit killed every agent three times
  (about 22:40 EDT 10-20, 06:30 EDT 10-21, and after the #667/#673 merges). All agents had
  committed clean trees. Fresh finisher agents replaced α, δ and ε (their contexts were 475k
  to 630k) and seeded from the committed tips; β, γ, ζ were resumed by raw id. Later fixes to
  #668 and #672 ran on fresh fixers.
- **Disk and builder:** the Docker VM fell to 4.3 GB and 5.1 GB free; build-cache prunes
  recovered 4.3 GB and more. The builder tag was `iaa-w3` until #671 (dqlite query metadata
  patch, non-W5, merged `dbaa3961`) landed; patched images were then tagged `iaa-w5-full`
  and `iaa-w5` for later chains.
- **CI flakes (all rerun green or infra, none a product defect):** connectors lane Playwright
  `ECONNRESET` on #666; `agent-auth` server start failure `rename … info.yaml: invalid
  cross-device link` on #667; `helm-integration-test` `TestDataAssertionsDatasetOperatorReads`
  `VerifiedAt` drift on #667; `docker.service` start failure in `coverage-ratchets` on #672;
  `ui-test` project-list pin missing `keycloak` on #666 (a real fix, `8752f215`);
  `coverage-ratchets` uncovered `api/rest/controller/job/queue/delete.go` on #672 (a real
  fix); master's push run red once on `podman-integration-test(3)` (pre-existing).
- **Follow-ups filed by this wave (owner in brackets):**
  - PostgreSQL per-job concurrency can double-admit in `insertRunIfSlotTx` under READ
    COMMITTED (new issue; pre-existing, not C5).
  - PostgreSQL `AutoMigrate` fails on SQLite `blob` columns (`internal_ca_generations`,
    `internal_node_enrollments`, `run_checkpoints`) (issue).
  - Run admission takes no lock that conflicts with a confined prune's candidate locks, so a
    window remains on PostgreSQL (`store.go`; issue).
  - Unrestricted `PruneMissing` trims against raw stored aliases, and non-canonical aliases
    should be rejected for every writer (B-stream follow-up).
  - The agent lane prints raw container logs, which can include the bootstrap key, on failure;
    route them through `scripts/lane-logs.sh` (issue).
  - Live LDAP `no_binding` reason and body (E1b). Live OIDC is covered by #666.
  - E5 obligations from E4: `POST /auth/logout` with a user-bound bearer revokes exactly that
    key (test bearer plus cookie); wire the key service into `SSOController`; mint through
    `MintUserBoundKey` from `/auth/cli/authorize` with the session user's `DisableGeneration`.
    E2 enforces the staleness bound at the marked point in `validateUserKey`.
  - Agent-profile write audit rows (D4 follow-up Q4).
  - Placeholder-collapsed cross-namespace contract break delivery (B5/B6).
  - Scope agent-profile `secret_refs` to the incident's namespace if they are ever resolved (A8).
  - Other leader-gated loops (freshness evaluator, incident subscribers) use
    `dqlite.IsLocalLeader`, which fails on PostgreSQL (issue; only the dequeuer was fixed).
  - B4 test `TestAuthNamespaceLegacyBaselineEvidenceWithheld` asserts a bare `31337` and
    matched a nanosecond timestamp once; anchor it on `"baseline_median":31337`.
  - Mixed-grant queue-cancel 403 (viewer@marketing + operator@finance) is unit-tested only;
    the Keycloak lane can cover it (G-lane).
  - `HoldPanel` namespace gating (B6); Console `held_reason` label (F4); namespace switcher (F2).
  - A6: webhook→event-bridge deliveries across namespaces are outside trigger tenancy.
  - B7 rebases on B9's importer change.
  - No PostgreSQL server lane: quota enqueue→promotion on PostgreSQL has unit coverage only.
  - Persist callback intent when recovery finalizes a committed run, so callbacks survive a
    persistent read-back failure (D follow-up; residual accepted on #672).
- **Next waves:** the plan's suggested-waves table is out of date (it predates the W3 to
  W5 re-slicing). W6 is dispatched with B7, B5, B6, A8 + A6, C1 + C5 and E5. W7: E1b, E2,
  C2 (after C1), F2 + F4 (F2 after B5), A7 (after B5, B6 and A8) and G4 (after E5). W8: G3
  (after E2), C3 (after C2), G2 (after F2) and N-2. Confirm each item's `Depends on` at
  dispatch.

### Wave 7 — Legacy secrets, isolation context, group refresh, Console namespaces (2026-10-28)

- **W7-ε / W6 follow-ups:** merged #687 `1f41ca73`. Closes the cross-namespace leaks W6
  recorded: the lint/diff edge-id collision, receipt snapshot binding, replay's start view,
  trimmed-alias producer identity, the PostgreSQL advance CAS and the B9 test race.
- **W7-α / A6:** merged #688 `ef5049e4`. The static shared secrets are deprecated: the
  principal decides, the secret is only audited, and event ingest is namespaced.
- **W7-γ / C2:** merged #689 `1a4963df`. The isolation context travels through dispatch and
  fails closed on a worker without `isolation_v1`.
- **W7-β / E1b + E2:** merged #691 `36dde0c3`. LDAP refresh is restored from the preserved
  branch, and the asynchronous refresher serves sessions and user-bound keys.
- **W7-δ / F2 + F4:** merged #690 `94b4b62d`. The Console gains the namespace switcher, chips
  and the quota-held label.
- **W6 follow-ups left after W7:** a reaper that uses the multi-namespace `List` (C1);
  Windows credential-file locking and an OS keychain (E5); raising or splitting the
  `coverage-ratchets` timeout (CI).
- **W7 follow-ups:** contract graph identity and the trigger-chain exclusion still match by
  trimmed alias; remove the legacy secrets in the release after next; producerless arrival
  sources for confined credentials await sign-off (A6-D6); a node with
  `CAESIUM_WORKER_ENABLED=false` never renews its run leases; rows of a namespace whose
  isolation settings change while the run has no owner wait for an operator; the incidents
  widgets and the contract graph are not narrowed by the switcher.

### Wave 8 — Allow/deny matrix, Keycloak scenarios, mapped namespace proof (2026-10-31)

- **W8-δ / G2:** merged #697 `234a1b31`. The Playwright `keycloak` project signs each identity
  in through Keycloak's login page and proves the switcher, chips, `InsufficientAccess`, the
  Access page and the quota label from a live queue response.
- **W8-γ / C3:** merged #699 `c232977a`. Per-namespace Helm RBAC and a kind proof of a mapped
  namespace (`rbac`, `placement`, `graceful_restart`, `crash`). Recovery now stops the prior
  attempt's pod, only for rows the reset landed on, on all three recovery paths; greptile
  found the Postgres race that shaped that rule. `owner_takeover` is unit-tested only.
- **W8-ε / snapshot retry (not a plan item):** merged #701 `b1bd3f79`. The matrix's concurrent
  reads surfaced `checkpoint in progress` as 500s from read-only catalog snapshots.
  `db.TransactionOn` takes transaction options, and `jobdiff.ReadSnapshot` and
  `incident.readSnapshot` retry through it. Stress failures went from 147 of 10,797 (bundle)
  and 592 of 42,859 (graph) to zero. The audit fixed two closures that were unsafe to re-run;
  one, `ConfinedImpact`, would have been a confinement leak.
- **W8-α / A7:** merged #700 `f40e7393`. `TestAuthNamespaceAllowDenyMatrix` is the scenario AC1
  calls `TestNamespaceAllowDenyMatrix`: 126 routes, 21 principals, 5,859 requests on the
  agent lane. It fixed two 500s for an unknown id (`GET /v1/atoms/:id`, `GET /v1/triggers/:id`).
  Review added real methods on unmounted routes, stats numbers checked against the catalog,
  and controller audit rows. The channel, policy, reload and logout audit expectations were
  seen passing but not mutation-tested.
- **W8-β / G3 + G4:** merged #703 `fd1fe7c4`. Thirteen required Keycloak scenarios, including
  the matrix for SSO principals (104 routes, 22 unmounted, 18 principals, 4,323 requests) and
  the `caesium login --no-browser` round trip. The recipe requires a pass for every named
  scenario instead of a minimum count.
- **Acceptance check on master `fd1fe7c4`:** criteria 1 to 8 hold, and N-2 delivers 9. Two
  map to names that changed in delivery: AC3's worker "without `kube_target`" is
  `isolation_v1` in `test/distributed_isolation_test.go`, and AC7's
  `build-and-integration-test-keycloak` is the `integration (keycloak)` matrix leg, which
  `ci-ok` requires through `integration`.
- **Review record:** #697, #701: Codex approved. #699: Codex raised one P2, accepted below. #700: Codex
  raised three P2s, all fixed. #700's fix and #703 merged ahead of Codex at the user's request,
  because Codex's account limit had run out; both rounds ran after merge and approved with no
  findings.
- **Follow-ups, with no owner:**
  - No coverage journey executes the new `engine.Stop` call (C3). The Kubernetes journey runs
    `local` mode with workers and owners off. Extending it changes a shared gate and would
    reach only the "pod already gone" branch, so it belongs in its own PR.
  - In default SQL advancement, a run whose driving process died stays `running` until a cron
    tick or webhook adopts it, so a manual-only job stays stuck. Proved live on master.
  - A local-mode crash leaves an orphan pod. An in-memory single node restarting after its run
    lease expired may wedge the run (read, not reproduced).
  - On Postgres, rows a reclaim skipped are still reported as reclaimed. A stale owner's reset
    can clear a newer owner's claim, and the worker then cancels that attempt.
  - A dispatch deadline under 30s silently becomes a 5-minute claim.
  - `withSnapshot` closures swallow contended reads: `validate` shows a raw
    `checkpoint in progress`, and `readScope.resolve` fails closed. This needs a small design.
  - `DELETE /v1/atoms/:id` answers 202 for an unknown id, and a job delete leaves its
    `run_queue` row.
  - The engine namespace's `secrets get` is a hand grant.
  - `go test` in the runner dirties `go.mod` and `go.sum` through the bind mount.
- **Sign-offs still open:** mint at exchange (spec §13.1), the depth of §8.3, `failureClass`,
  namespace-qualified `runtime_id`, and A6-D6.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Policy file, grants, principal, route classes, namespace-aware middleware, keys with namespaces, SSO-admin key management | **P0** | A1 #624, A2 #638, A3 #647, A4 #645, A5 #654, A8 #681, A6 #688, A7 #700 merged — complete |
| B | `metadata.namespace` on jobs: schema, columns, importer, lint, apply/prune/move, git-sync allowlist, collection filtering, aggregate collapse | **P0** | B1 #625, B2 #657, B3 #639, B4 #661, B9 #668, B7 #682, B6 #683, B5 #685 merged — complete |
| C | Runtime isolation: per-namespace Kubernetes namespace + service account, scoped secret resolution, Helm RBAC | P1 | C4 #660, C1 + C5 #679, C2 #689, C3 #699 merged — complete |
| D | Run quota + round-robin fairness (#395 v1 slice); namespaced notification channels, policies, agent profiles | P1 | D1 + D2 #672, D3 #640, D4 #673 merged — complete |
| E | Identity lifecycle: IdP group refresh, user/session administration, user-bound keys, `caesium login` | P1 | E1 #646, E3 #655, E4 #665, E5 #684, E1b + E2 #691 merged — complete |
| F | Console: grants-aware gating, namespace switcher, namespace chips, quota label, Access page | P1 | F1 + F3 #667, F2 + F4 #690 merged — complete |
| G | Keycloak: realm fixture, CI lane, Playwright + Go scenarios, operator guidance | P1 | G1 + G5 #666, G2 #697, G3 + G4 #703 merged — complete |
| H- | Harness: integration server policy fixture, `--json` stdout discipline for new verbs | — | H-1 #657, H-2 #645 merged |
| N- | Docs: rename + grow the identity-and-access guide; roadmap, README, sovereignty | — | N-1 #648 merged; N-2 this close-out — complete |

## Streams

### Stream A — Authorization core: policy file, grants, route classes

The spine everything else hangs off. Ships the `internal/auth/policy` package (parse,
validate, resolve, glob, reload), replaces `Principal.Role` with a grants map, turns the
route policy table into classified `RoutePolicy` entries, teaches the middleware to
resolve a target namespace per route and to inject the allowed-namespace set for
collections, gives API keys namespaces, and makes every `/v1/auth/*` controller accept
any principal so SSO admins are first-class. With no policy file the env role mapping is
translated into cluster-wide bindings and behaviour is unchanged.

- [x] A1. Add the policy package: `AccessPolicy` schema (namespaces with `kubernetes`,
      `secrets.allow`, `quotas`; bindings with `subjects.{groups,users}`, `role`,
      `namespaces`), YAML parse, validation per spec §6.2 (DNS-label namespaces, `*`
      rejected as a key, declared-namespace check on bindings, role validity,
      non-empty subjects), `Resolve(groups, email) Grants` (highest role per namespace,
      `"*"` group wildcard, ASCII-case-insensitive email with non-ASCII bytes exact),
      the `provider/path` glob matcher with `*` and `**`, rejecting dot traversal;
      Vault path rules cover every field of the selected secret. Lint warnings
      cover non-`default` namespaces without secret
      rules). Pure package, table-driven unit tests, no wiring.
      Files: new `internal/auth/policy/policy.go`, new `internal/auth/policy/resolve.go`,
      new `internal/auth/policy/glob.go`, new `internal/auth/policy/policy_test.go`.
- [x] A2. Replace `Principal.Role` with `Grants` (`RoleIn`, `ClusterRole`,
      `AllowedNamespaces`); `PrincipalFromUser(user, policy)` resolves grants from the
      stored groups + email on every request; `PrincipalFromKey` maps the key's
      `Namespaces` (NULL ⇒ `["*"]`) to grants; translate `CAESIUM_AUTH_ROLE_MAPPING` +
      `CAESIUM_AUTH_DEFAULT_ROLE` into cluster-wide bindings when no policy file is set
      (`rolemap.go` becomes that translation); update the two readers of
      `Principal.Role` (middleware metrics/audit label, whoami) to the cluster role.
      Policy groups remain byte-exact; the no-policy env adapter trims incoming
      groups to preserve `RoleMapper.Resolve` behavior. Only trusted/verified
      provider email may reach email bindings (v1: OIDC `email_verified=true`
      only; SAML and LDAP email is untrusted); unverified email passes as empty,
      with the trust state preserved for per-request resolution. Tests must reject
      an unverified OIDC email claiming an email-only admin binding.
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
- [x] A3. Classify every route and make the middleware namespace-aware: `endpointPolicy`
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
      (W3-α: routes whose handlers can still return another namespace's rows carry
      `pendingOn("<item>")` and are cluster-only for confined principals until B2, B4,
      B5, B6, D4, A5, A6 or A8 clears them; resolvers live in
      `api/middleware/auth_namespace_resolvers.go`. Backfill create, read and cancel
      wait on B4, whose history filter must also cover the backfill date dedupe
      (`LatestRunForLogicalDate` reads runs by job id alone); agent-profile and
      notification channel/policy deletes wait on D4. `run_start_idempotency` records
      the namespace a keyed start was admitted in, and a retry is authorised against
      it before the request is compared. Also fixed concurrency admission persisting
      runs in `default`.)
- [x] A4. (W3-β: `policy.Holder` in `internal/auth/policy/loader.go`; content-hash poll,
      not mtime; no-file `POST` reload answers 409, a rejected file 422.)
      Wire the policy loader: `CAESIUM_AUTH_POLICY_FILE`,
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
- [x] A5. (W4-α: an explicit `["*"]` is stored as NULL; with a policy file, create
      and rotate refuse undeclared namespaces; rotation keeps the key's expiry and
      never extends the old key; `--jobs` is an alias of `--scope-jobs`; the key
      list stays JSON; whoami asserted over REST until E5's `caesium whoami`.)
      API keys with namespaces and SSO-admin key management: `Namespaces` column
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
- [x] A6. (W7-α: principal decides, the secret is only audited; ingest, fire,
      arrivals and the webhook/lifecycle bridges confined at admission.)
      Deprecate the static shared secrets: `CAESIUM_MANUAL_TRIGGER_API_KEY` and
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
- [x] A7. The namespace allow/deny matrix: `TestNamespaceAllowDenyMatrix` in `test/`
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
      (W8-α: the scenario is `TestAuthNamespaceAllowDenyMatrix`, which is what this
      item and Acceptance Criterion 1 mean by `TestNamespaceAllowDenyMatrix`: only a
      `TestAuth…` name matches the auth lane's `-run`. The lane-agnostic harness,
      `runNamespaceMatrix`, the row table and the fixture are
      `test/auth_namespace_matrix_{harness,rows,fixture}_test.go`; a route with no row
      fails it. Job-scoped keys and agent tokens stay refused on identity routes (design
      §8.2 step 4, C1). The moved job's old-run replay proves admission by local mode's
      409; `TestReplayIntoMovedNamespaceDropsOldFreshnessViews` covers the dispatched
      path on the distributed lane. The matrix found that `GET /v1/atoms/:id` and
      `GET /v1/triggers/:id` answered 500 for an unknown id: both now answer 404, and it
      holds every route that takes an id to no 5xx on unknown ids.)
- [x] A8. Confine agent-session allow-lists to the incident's namespace (W6-δ: also why baselines, a moved own job's DAG and remediation, executor refusals incl. git-synced patches, legacy-session content withheld from confined readers):
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

- [x] B1. Add `metadata.namespace` to the manifest and the columns behind it:
      `Metadata.Namespace` in the definition (syntax validation in `Validate()`,
      default `default`), `Job.Namespace` / `JobRun.Namespace` / `Backfill.Namespace`
      (text, not null, default `'default'`, indexed) and `Incident.Namespace`
      tightened from nullable to the same shape; the importer writes it on apply.
      Upgrades normalize existing NULL/empty namespace rows first. All nodes must
      stop and upgrade together; mixed-version incident writers are unsupported.
      Migration key collisions fail atomically with a diagnostic identifying both
      incident rows rather than discarding or silently closing history.
      The run/backfill/incident creators copy it from the job; JSON tags so every API
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
      Namespace-only moves also appear in the shared CLI/REST/approval metadata
      diff, including omission moving a namespaced job to `default`. Unsafe
      historical replay across a move fails closed with old/new namespace context.
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/definition_test.go`,
      `pkg/jobdef/schema.go`, `internal/jobdef/report/report.go`,
      `docs/job-schema-reference.md`, `internal/models/job.go`, `internal/models/run.go`,
      `internal/models/backfill.go`, `internal/models/incident.go`,
      `internal/jobdef/importer.go`, `internal/run/store.go`,
      `api/rest/controller/job/post.go`, `api/rest/service/job/job.go`,
      `internal/replay/replay.go` (run insert and replay hash input),
      `api/rest/controller/backfill/backfill.go` (creator), `internal/backfill/`,
      `internal/incident/{incident,store,subscriber}.go` (persisted namespace, dedupe,
      and remediation), `internal/cache/hash.go`, `internal/cache/hash_test.go`,
      `internal/job/local_run_identity.go` (local hash input),
      `internal/worker/runtime_executor.go` (worker hash input),
      `internal/run/whydiff.go` (persisted hash decoding),
      `pkg/db/{db,migrations,migrations_test}.go` (legacy namespace normalization),
      `justfile`, `scripts/test_ci.py` (distributed scenario selection and guard),
      new `test/cache_namespace_move_test.go`,
      new `test/namespace_schema_doc_test.go`,
      new `test/namespace_incident_lifecycle_test.go`,
      `internal/jobdef/diff/` (namespace projection),
      `scripts/coverage-journeys.sh`, `scripts/test_coverage_named_journeys.py`
      (required real schema, incident and cache/diff/replay journeys),
      `internal/jobdef/exporter.go`, `docs/examples/`,
      `docs/caesium-job-llm-reference.md`, `docs/job-definitions.md`.
- [x] B2. (W4-γ: confined apply/move/prune and POST /v1/jobs moved to B9.)
      Server-side namespace semantics: `POST /v1/jobdefs/lint` and apply reject an
      undeclared namespace (from the A4 policy holder) naming it; a namespace move keeps
      history (runs, receipts, incidents) on the original namespace and rewrites only
      `jobs.namespace`; the apply preview lists namespaces touched. B1 supplies the
      metadata namespace diff; B2 supplies its authorization semantics. **Diff and lint baselines are
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
- [x] B3. Git-sync per-source namespace allowlist: `GitSourceConfig.Namespaces
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
      (`file://` against `integration-up` is infeasible — no repo mount, go-git's file transport needs native `git-upload-pack`, a failed first sync stops the shared server; the live scenario is `TestJobdefGitSyncNamespaceAllowlistSkipsForeignManifests` on the coverage-ratchets `git-sync` lane in `test/jobdef_git_sync_test.go`. Only allowlisted sources are audited.)
- [x] B4. (W4-δ: history reads use `authz.WithHistoryScope`; blame, topology and topology/history stay pending under B7 because DAG snapshots record no namespace; retry and partition retry answer `409 namespace_moved` in the handler and inside the store transaction, the agent retry path included; backfill children are admitted only in the backfill's namespace; event deliveries are owned by the runs they started; the event stream sends a confined subscriber an audited allow-list of run and task types.)
      Collection filtering in job-derived handlers: job list (replacing the
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
- [x] B5. Aggregate redaction: `/v1/lineage/impact` and `/v1/contracts/graph` compute
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
      (W6-β: shipped. Ownership rides the derived row (job and persisted run
      namespaces); impact placeholders keep `depth`; collapsed topology kept;
      `?dataset=` filters after redaction; a consumer never grants an impact root.)
- [x] B6. (W6-γ: collections SQL-filtered by producer namespace; reads shaped by
      owner, incl. the state reason via a new `reason_namespace`; advance and
      release re-check producers in their transaction under the producers'
      alias advisory locks; uniform audited 403 for foreign and missing
      datasets; new derivation columns not serialised.)
      Dataset tenancy via declared producers: a dataset's namespaces are those of
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
- [x] B7. (W6-α: `dag_snapshots.namespace` is stamped on every write and deduped with the hash, so a move writes a fresh snapshot; blame, topology and topology history are live for confined viewers over the snapshots they may read, legacy snapshots unconfined-only; every writer refuses a non-canonical alias, git sync skips such a manifest per manifest, and `PruneMissing` matches aliases exactly.)
      Namespace-aware DAG snapshots: `dag_snapshots` (written by the importer,
      read by `GET /v1/jobs/:id/blame`, `/topology` and `/topology/history`) carries no
      namespace, and an apply that only moves a job writes no new snapshot because
      snapshots dedupe by content hash. Record the job's namespace on every snapshot
      write and include it in the dedupe key, so a move writes a fresh snapshot owned by
      the new namespace. Legacy snapshots, whose owner cannot be recovered, are
      cluster-only (never back-filled from the job's current namespace, which would
      hand the previous owner's commands and provenance to the new one). Then clear the
      three routes' `pendingOn("B7")` markers with history filtered by the snapshot's
      namespace. Unit tests: move without a topology change, legacy rows, two
      namespaces. Live: blame and topology of a moved job read by principals confined
      to each side.
      Files: `internal/models/dag_snapshot.go`, `internal/jobdef/importer.go`
      (snapshot write and dedupe), `internal/jobdef/snapshot_query.go`,
      `api/rest/controller/job/` (blame, topology), `internal/auth/rbac.go`, new
      `test/` scenario.
      Depends on: B4 + B9 (both edit the importer; B9 first).
- [x] B9. (W5-α: all four routes live; one tenancy check (rule 1 always, rule 2 with a policy file) on the proposed state in the importer, legacy create and trigger PATCH; a confined writer's apply is one transaction (REPEATABLE READ plus pre-BEGIN advisory locks on PostgreSQL); a namespace-confined prune is the server-computed set within its operator namespaces (spec §9.5 amended); a legacy cross-namespace trigger row is detached onto a fresh row on apply; contract findings touching a foreign job are withheld and counted; the manifest re-checks the job and withholds a shared trigger in one snapshot.)
      Confined job writes and trigger tenancy (split from B2 in W4): un-pend
      `POST /v1/jobdefs/apply`, `POST /v1/jobs`, `PATCH /v1/triggers/:id` and
      `GET /v1/jobs/:id/manifest` for namespace-confined principals. Needs:
      - **Trigger tenancy.** A trigger row may serve jobs in only one namespace; a
        webhook path may be subscribed from only one namespace. Enforce it atomically
        in the importer, the legacy create and the trigger PATCH (Postgres absent-row
        races included, not just a READ COMMITTED re-query), and refuse a confined
        caller whose write would attach to or modify a trigger serving a job outside
        its namespaces or outside its key's job-alias scope.
      - **An atomic confined batch.** One transaction for every definition plus the
        prune, so a late in-transaction refusal (a foreign job created after the
        middleware check, a concurrent move) leaves neither applied definitions nor
        pruning effects; namespace refusals inside `guardJobMutationTx` are fatal for a
        user apply (unlike git sync's skip).
      - **Move and prune authorisation** (spec §9.4, §9.5): operator in both namespaces
        for a move; operator in every namespace of the server-computed prune set.
      - **Redaction** of foreign identities in apply's 409 contract findings and its
        success `contract_warnings` (`internal/contract/enforce.go`).
      Integration: apply into `marketing`; move `marketing` → `finance` with operator in
      both, refused with one; prune refused across namespaces; a scoped key cannot
      modify a shared trigger of a job outside its scope; two confined callers racing
      for one webhook path; a late refusal rolls the whole apply back.
      Files: `internal/jobdef/importer.go`, `internal/models/trigger.go`,
      `api/rest/controller/jobdef/apply.go`, `api/rest/controller/job/post.go`,
      `api/rest/service/job/job.go`, `api/rest/controller/trigger/`,
      `api/middleware/auth_namespace_resolvers.go`, `internal/contract/enforce.go`,
      `internal/auth/rbac.go`, `test/jobdef_namespace_test.go`.
      Depends on: B2.

### Stream C — Runtime isolation: Kubernetes target and scoped secrets

Turns a namespace into a real security boundary at execution time. The Kubernetes
engine resolves the pod namespace and default service account per request from the
policy, the resolved target travels with the dispatch request so workers never read
the policy file, Helm renders RBAC for each mapped namespace, and a scoped secret
resolver refuses references outside the namespace's allow-list at lint and at run time.

- [x] C1. Kubernetes engine resolves the target per request: `KubeTarget{Namespace,
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
      (W6-ε: resolved once per execution before the cache identity and held
      across retries/fan-out; every new pod id is `<ns>/<pod>`; Docker steps
      with relative `secret://k8s` refs key on the mapped secret namespace; a
      replay re-executes a step whose recorded mapping moved; undeclared
      namespace on a file-backed node fails closed as `config`; no production
      reaper exists, so List + `KubernetesNamespaces()` ship for one.)
- [x] C2. Carry an isolation context through dispatch, fail closed on old workers:
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
      (W7-γ: the context also carries the owner's effective pod and secret-read
      namespaces; the worker's re-checks before the cache lookup, before
      resolution and in replay verification ask the owner's current rules over
      `POST /internal/isolation` (fail closed); an isolated envelope carries a
      tagged `worker_node` an old worker refuses before claiming; isolated rows
      carry the reserved node selector `caesium.io/isolation: v1` so only a
      capable, policy-serving worker can pull them; the envelope bound is
      derived from the policy size limit; `CAESIUM_WORKER_ISOLATION_V1_DISABLED`
      withdraws the capability; live proof is the three-node
      `integration-test-isolation` lane.)
- [x] C3. Helm multi-namespace RBAC and the kind-lane proof: `jobNamespaces: []` in
      values renders a `Role` + `RoleBinding` (pods, pods/log, secrets get,
      persistentvolumeclaims) per namespace; the kind CI values map one namespace and
      the k8s integration scenario asserts a job in it lands its pod, PVC and a
      `secret://k8s` read there with the mapped service account.
      Files: `helm/caesium/values.yaml`, `helm/caesium/templates/role.yaml`,
      `helm/caesium/templates/rolebinding.yaml`, `helm/caesium/ci/test-values-k8s.yaml`,
      `test/` (k8s lane scenario), `docs/kubernetes-deployment.md`.
      Depends on: C1 + C2.
      (W8-γ: the mapped release is a second release `c3` in shard 3 from
      `ci/test-values-k8s-mapped.yaml`, so the suite's release stays
      policy-free; `accessPolicy.existingConfigMap` mounts the policy
      read-only; the RBAC renders with the engine disabled too. Recovery now
      stops the previous attempt's pod through its qualified runtime id on
      every distributed re-pend path (worker reclaim, owner reap, owner
      takeover); a local resume stops nothing because a local attempt has no
      claim to fence it. `TestKubernetesMappedNamespace` proves placement and
      recovery after a graceful restart and a crash.)
- [x] C4. Scoped secret resolution: `secret.ScopedResolver{inner, namespace, rules}`
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
      `k8s/<namespace>/<secret>`), derived from the provider's effective target,
      including query overrides. Vault allow rules select the canonical secret
      path and grant every field of that object; `#` is a reserved delimiter, so
      decoded effective paths or fields containing it must be rejected before
      matching. C4 expands relative `k8s/<secret>` rules against the mapped
      Kubernetes namespace and pins `k8s/**` to that namespace in tests. Provider
      aliases are canonicalized and unknown rule providers produce a lint error.
      Returns `ErrSecretDenied` (redacted to `provider/<first segment>/…`). Wired
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
      (W4-ε: also checks `POST /v1/jobdefs/diff`; fan-out instances check
      before their own cache lookup; a denial is terminal in every retry loop;
      unsupported rule providers fail policy validation; rules are node-local
      until C2; `why` surfacing of the class is left to C5; client-side runs
      never wait for a server policy; replays check before reusing a step. Live: `TestAuthSecretScope`,
      `TestAuthSecretScopeWebhook`, `TestDevOnceSecretWithAPolicyFileConfigured`.)
- [x] C5. Secret-scoping end to end: with the H-1 fixture's `marketing` allow-list, a
      manifest referencing `env/FINANCE_*` is rejected by `caesium job lint` (server)
      naming the redacted reference; applied with lint bypassed it fails the task with
      the `config` class visible in `caesium why --json`; the allowed reference resolves
      and the task succeeds.
      Files: new `test/secret_scope_test.go`, `test/testdata/policy/two-namespaces.yaml`.
      Depends on: C4 + H-1.
      (W6-ε: `TestSecretScopeMarketingAllowList` on the no-auth lane and the
      local coverage journey; the fixture is unchanged, the servers carry
      `MARKETING_IT_TOKEN`; `caesium why` gained `failureClass`.)

### Stream D — Run quota and fairness; namespaced shared objects

Two small isolation boundaries with disjoint files. The quota lives where a queued run
becomes running; shared objects get a namespace column with a `*` sentinel for
cluster-shared instances.

- [x] D1. Per-namespace `quotas.maxConcurrentRuns` as **one transactional store
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
      (W5-δ: shipped. Also stamps `run_queue.namespace` so a queued request is
      never promoted across a job move, refuses every start in a namespace a
      loaded policy no longer declares (`409 namespace_undeclared`), and runs a
      quota-only dequeuer whenever a policy file is configured.)
- [x] D2. Surface the hold: `GET /v1/jobs/:id/queue` rows carry the `run_queue`
      row's `held_reason: namespace_quota`; `caesium job queue` prints it; the Console queue
      page label is F4. Integration (no-auth server, H-1 settings): two policy-free
      jobs in a capped namespace, the second run is queued with the reason and
      released when the first completes; a retry while held gets 409; a two-date
      backfill completes with correct accounting.
      Files: `api/rest/controller/job/queue/`, `api/rest/service/job/`,
      `cmd/job/queue.go`, `test/job_queue_cli_test.go`.
      Depends on: D1.
      (W5-δ: shipped; a confined caller sees only queued rows recorded in a
      namespace it may read.)
- [x] D3. Namespace column on shared objects: `NotificationChannel.Namespace`,
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
      (Lint rule lives in `internal/jobdef/agent_profile_refs.go`, not `internal/jobdef/lint/`; the built-in `triage-only` profile is cluster-shared `*`.)
- [x] D4. Shared-object semantics through the surface: lists are collections
      (`*` objects always included); writes to `*` objects require operator at `*`;
      notification policies match runs in their own namespace only, shared policies
      everywhere. Integration: a marketing operator cannot edit a finance channel (403),
      can read a shared one, a shared policy fires for a marketing run and a finance
      policy does not.
      Files: `api/rest/controller/notification/`, `api/rest/controller/agentprofile/`,
      `internal/notification/watcher.go`, `test/notification_routes_test.go`,
      `test/agent_profile_test.go`.
      Depends on: D3 + A3.
      (W5-ε: matching lives in `internal/notification/route.go` `RouteEvent`, keyed on the incident's, else the breaching runs' (dataset releases), else the run's persisted, else the job's namespace, only while a policy file is loaded (design §§1/15: no file, routing and profile resolution unchanged; the earlier always-strict fold was overruled); labels not provably the owner's, cross-namespace contract findings and unowned baseline evidence are withheld from namespaced destinations; undeclared namespaces refused on create; deletes release names; runtime profile resolution refuses foreign profiles (file-backed only), the deployment default included (a refused default denies all, before any job override). Follow-up: audit rows for agent-profile writes.)

### Stream E — Identity lifecycle: group refresh, user administration, CLI login

Keeps grants honest after login and gives admins and users the levers they expect.
Refresh handles on sessions, an asynchronous single-flight group refresher, the user and
session admin surface, user-bound API keys that resolve grants at use time, and the
server-brokered `caesium login` flow that works for every provider.

- [x] E1. One refresh grant per user: `User.RefreshGrant` (AES-256-GCM under an
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
      (W3-γ: lease/fence primitives in `internal/auth/users.go`, OIDC refresh in new
      `internal/auth/oidc/refresh.go`; every grant change advances the version,
      abandoned-lease takeover is uncertain, and a definitive refusal on an
      uncertain grant only clears it. LDAP refresh is deferred to E1b: an LDAP login
      stores no grant (NULL, as for SAML) and `internal/auth/ldap` is unchanged. E2
      must bound each redemption by the lease, TTL ≥ 2× timeout.)
- [x] E1b. LDAP group refresh with a live LDAP journey: re-apply the preserved branch
      `iaa-e1-ldap-refresh-preserved` (DN + login-predicate re-check, current-username
      group search, context-bounded I/O, the OpenLDAP fixture cases) and add an LDAP
      coverage journey (an OpenLDAP container in the coverage lane and the auth lane)
      so the diff ratchet covers `internal/auth/ldap`. Must land before E2 relies on
      LDAP refresh; until then LDAP users have no refresh grant and the E2 staleness
      bound forces re-login, as for SAML.
      Depends on: E1.
      (W7-β: LDAP grant = DN + typed login, `DenialOfSubject`; new
      `CAESIUM_AUTH_LDAP_CA_FILE`; OpenLDAP over LDAPS on the agent-auth lane and
      the coverage auth journey via `scripts/ldap-fixture.sh`.)
- [x] E2. The async refresher for **both** credential types: on `SessionStore.Validate`
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
      (W7-β: a failed synchronous refresh past the bound persists
      `users.stale_refused_at`, cleared only by login; live LDAP scenarios on both
      auth lanes; Keycloak assertions stay G3.)
- [x] E3. User and session administration: `GET /v1/auth/users` (filters, paging),
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
      (W4-β: `internal/auth.UserAdmin` is the cascade seam E4 extends; disable
      also clears the refresh grant, and `SessionStore.Create` is fenced on the
      user row and `refresh_grant_version`, so a login checked before a disable
      cannot mint a session. Live scenarios seed SSO users through the catalog
      on the api-key lanes; G3 still drives a real SSO user.)
- [x] E4. User-bound API keys: `APIKey.UserID`; `csk_user_` prefix; on validation the
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
      (W5-β: the mint seam is `Service.MintUserBoundKey(ctx, MintUserKeyRequest{UserID,
      DisableGeneration, …})`, fenced on the user row like `SessionStore.Create`; no
      route mints a key yet. The principal is the user (`Kind=user`, `UserKeyID` set,
      `KeyID` nil because connector reads and replay key identity on `KeyID`); a
      disabled or missing user, an expired key and a malformed row get the revoked
      key's 401. Disable, the enable sweep, `sessions/revoke` (a deliberate widening
      of the §11.2 row) and `CompleteRefreshDefinitive` revoke the user's keys in the
      same transaction; rotation is a 409. E2 hooks in at the marked point in
      `validateUserKey`. **E5 must:** make `POST /auth/logout` with a user-bound
      bearer revoke exactly that key (tested with bearer + cookie: only the bearer
      dies) and wire the key service into `SSOController`; until then logout does
      not revoke a user-bound key.)
- [x] E5. `caesium login`, `logout`, `whoami`: server routes `GET /auth/cli/start`
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
      (W6-ζ: the key is minted at `exchange`, not `authorize` — authorize records the
      user, its disable generation and a code hash; exchange mints through the shared
      `mintUserBoundKeyTx` core fenced on that generation, so no key material rests on
      the server (§13.1 step 3 amended, pending sign-off). One `cmd/login` package;
      backfill, cache, receipt get and verify now resolve credentials too; the
      resolver finds the stored login by the command's own `--server`.)

### Stream F — Console: grants, switcher, chips, quota label, Access page

The UI is the corporate user's window into all of this; it only needs to read the new
shapes. Owned separately from the Go streams so it can run on a lighter model.

- [x] F1. Grants-aware principal: `PrincipalState` gains `grants`, `namespaces`,
      `clusterRole`, `roleIn(ns)`; gating in `RunDetailPage` (replay), `HoldPanel`
      (release) and the job pages uses `roleIn(job.namespace)`; the login page shows
      the `no_binding` denial reason. Vitest on `roleIn` and the whoami parser.
      Files: `ui/src/lib/auth.ts`, `ui/src/features/auth/`, `ui/src/features/jobs/RunDetailPage.tsx`,
      `ui/src/features/datasets/HoldPanel.tsx`, `ui/src/lib/api.ts`.
      Depends on: A5.
      (W5-ζ: shipped with a typed `ErrNoBinding` redirect/credential hand-off; every job action is gated by role and namespace.)
- [x] F2. Namespace switcher and chips: a header switcher fed by `namespaces` (hidden
      with one), selection persisted in `localStorage` as a view filter, list pages send
      `?namespace=`, job list and job detail show a namespace chip, the lineage graph
      renders B5's `external` placeholder. Playwright in the existing auth project:
      switch namespace, list narrows; a deep link to a job outside the grants renders
      `InsufficientAccess`.
      Files: `ui/src/components/layout/Header.tsx`, new
      `ui/src/features/auth/NamespaceSwitcher.tsx`, `ui/src/features/jobs/`,
      `ui/src/features/jobs/LineageGraph.tsx`, `ui/src/lib/api.ts`, `ui/e2e/`.
      Depends on: F1 + B4 + B5.
      (W7-δ: shipped with an optional `?namespace=` on `GET /v1/jobs`, `/v1/incidents` and `/v1/triggers` (narrows within the allowed set; an unreadable namespace is an empty 200), always-on chips, 403 deep links rendering `InsufficientAccess`, and leak tests over the real external-node renderers.)
- [x] F3. Access page at `/access`: my grants; declared namespaces and their settings
      (cluster viewer+); users list with disable / enable / log-out-everywhere (cluster
      admin); policy revision hash and last reload result. Sidebar entry. Playwright
      coverage lands with G2.
      Files: new `ui/src/features/access/AccessPage.tsx`, `ui/src/router.tsx`,
      `ui/src/components/layout/Sidebar.tsx`, `ui/src/lib/api.ts`.
      Depends on: F1 + A4 + E3.
      (W5-ζ: shipped with `GET /v1/auth/namespaces` for cluster viewers; Playwright in the auth project.)
- [x] F4. Queue page shows "held by namespace quota" from `held_reason`.
      Files: `ui/src/features/jobs/` (queue view), `ui/src/lib/api.ts`.
      Depends on: D2 + F1.
      (W7-δ: `namespace_quota` rows are labelled in the job detail run queue. W8-δ: the real policy-backed browser journey, bob's second `marketing` start on the Keycloak lane, is `ui/e2e/keycloak-quota.spec.ts`.)

### Stream G — Keycloak: fixture, CI lane, scenarios, guidance

Gives SSO its first real end-to-end coverage and makes Keycloak a documented,
proven issuer. Every SSO-only behaviour from Streams A, C and E (SSO-admin key mint,
group refresh, user disable, CLI login) is asserted here against a live Keycloak.

- [x] G1. (W5-γ: the Go lane is the `keycloak` leg of the `integration` matrix
      and the browser smoke is job `ui-e2e-keycloak`, both under `ci-ok`, which
      has gated merges since 2026-09-28, not only `v*` publication; the lane
      serves `test/testdata/keycloak/policy.yaml` (two-namespaces' namespaces
      plus the realm bindings, guardrail-compared) because the shared fixture
      must never bind `caesium-admins`; it also proves the A2/A4 obligation,
      an unverified OIDC email denied an email-only admin binding, with a
      verified-email positive control.)
      Realm fixture and lane: `test/testdata/keycloak/caesium-realm.json` (client
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
- [x] G2. Playwright project `keycloak`: admin logs in through Keycloak's real login
      page and sees both namespaces and the switcher; bob sees only `marketing`, the
      finance job is absent and its deep link renders `InsufficientAccess`; carol is
      denied with `no_binding`; the Access page shows grants and, for admin, the users
      list.
      Files: `ui/playwright.config.ts`, new `ui/e2e/keycloak.spec.ts`,
      `ui/e2e/helpers/auth.ts`.
      Depends on: G1 + F2 + F3.
      (W8-δ: journeys in `ui/e2e/keycloak.spec.ts` and `keycloak-quota.spec.ts`; each identity signs in on its own browser context; jobs are seeded as the SSO admin over the cookie session + CSRF; `playwright.config.ts` needed no change.)
- [x] G3. Go scenarios on the lane (`CAESIUM_KEYCLOAK_LANE=true`): a cookie-jar client
      drives the OIDC code flow through Keycloak's HTML login form; the SSO
      allow/deny matrix; an SSO admin mints a namespaced key (A5's fix); group refresh
      (move bob via Keycloak's admin REST API, advance past the interval, grants change
      without re-login); `caesium auth user disable` kills bob's session within one
      request; `caesium auth user list --json` clean on stdout.
      Files: new `test/keycloak_lane_test.go`, new `test/keycloak_helpers_test.go`,
      `test/auth_lane_test.go` (lane detection).
      Depends on: G1 + E2 + E3.
      (W8-β: `TestKeycloakNamespaceAllowDenyMatrix` runs A7's harness for 17 SSO sessions
      plus the key an SSO admin mints; group refresh, rotation, `stale_groups`, disable
      and `invalid_grant` run on per-scenario realm users, so bob stays as G1 and G2
      expect. The recipe requires a top-level PASS per scenario by name.)
- [x] G4. CLI login on the lane: `caesium login --no-browser` prints the start URL, the
      test completes the SSO flow with the cookie-jar client, pastes the code with
      `caesium login --code`, `caesium whoami --json` shows bob's grants, disabling bob
      makes the next call 401, `caesium logout` cleans the credential file.
      Files: `test/keycloak_lane_test.go`.
      Depends on: G1 + E5.
      (W8-β: `TestKeycloakCLILoginRoundTrip` follows the signed-out finish page's OIDC
      link, confirms with the CSRF POST and logs out with the key revoked server side;
      the disable-kills-the-key step runs on a per-scenario realm user in
      `TestKeycloakUserDisableKillsCredentials`, so bob stays as G1 and G2 expect.)
- [x] G5. (W5-γ: the guide's Keycloak section and docs/ci.md "Keycloak lane".)
      Keycloak operator guidance in the guide (client, mapper, scopes, refresh
      tokens, issuer URL discovery) and the lane in `docs/ci.md`.
      Files: `docs/identity-and-access.md`, `docs/ci.md`.
      Depends on: N-1 + G1.

## Harness Strengthening

- [x] H-1. (W4-γ: `requireAuthLane` fails without the served fixture; the coverage
      journey's no-policy api-key lane is marked `CAESIUM_IT_AUTH_POLICY=none`;
      `mintNamespacedKey` in `test/namespaced_keys_test.go`.)
      Policy fixture on **both** integration servers: `test/testdata/policy/two-namespaces.yaml`
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
- [x] H-2. (W3-β: `runCLIJSON[T]` / `runCLIJSONFailure[T]` over `decodeCLIJSONStdout`.)
      `--json` stdout discipline for every new CLI verb (`auth policy`, `auth user`,
      `login`/`whoami`, `job queue` held reason): each scenario captures stdout with
      `runCLIStdout` and parses it; a shared assertion helper fails on any non-JSON
      byte. Lands with the first verb (A4) and is reused after.
      Files: new `test/cli_json_helpers_test.go` (`runCLIStdout` lives in
      `test/data_plane_e2e_test.go`).
      Depends on: A4.

## Navigational / Organizational Improvements

- [x] N-1. Rename `docs/sso-authentication.md` → `docs/identity-and-access.md` (`git mv`)
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
      (Done in W3-delta: the guide documents A4 (#645) and A3 (#647); merges after both.)
      Depends on: A4.
- [x] N-2. Close-out: `docs/sovereignty.md` gains a **Multi-tenancy** row; roadmap §3.1
      flips to Shipped and §3.3's "read-only vs operator role distinction" line is
      marked shipped; `docs/README.md` moves this plan to the completed list; this
      plan's Progress dashboard matches merged PRs.
      Files: `docs/sovereignty.md`, `docs/roadmap.md`, `docs/README.md`,
      `docs/exec-plans/completed/identity-and-access.md`.
      Depends on: every stream's last item.
      (W8 close-out: the plan moved to `completed/` in the same change.)

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

- `pkg/env/env.go`: B3 (W2: `CAESIUM_JOBDEF_GIT_SOURCES` redaction in `Process`), A4
  (policy file + reload interval), E4 (CLI key TTL), E2 (refresh interval, staleness,
  SAML TTL), A6 (deprecations). Additive fields; sequence
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
- `internal/auth/audit.go`: B3 (W2, `jobdef.git_sync`), A3, A4, A6, E2, E3 add action
  constants. Additive; rebase.
- `internal/metrics/metrics.go`: A4, D1, E2 add collectors (two edit sites). Additive.
- `internal/models/api_key.go`: A2 (`Namespaces`) → E4 (`UserID`). Sequential.
- `internal/models/user.go`: A2 (`EmailVerified`, W2) → E1 (`RefreshGrant*`,
  `GroupsRefreshedAt`).
- `internal/models/namespace.go`: D3 (W2) added `ClusterNamespace`,
  `NormalizeObjectNamespace`, `ObjectVisibleFrom`, `CheckNamespaceUnchanged`. A3 and D4
  reuse them; do not add a second `*` constant.
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
- `internal/worker/worker.go`: C2 only. `scripts/ci-ok.py`: G1 only.
  `scripts/test_ci.py`: B1's distributed-selector guard, then G1's Keycloak lane.
- `pkg/jobdef/definition.go`: B1 only.
- `internal/cache/hash.go`: B1 (namespace in the key + test) → C1 (hash the resolved
  Kubernetes target). Sequential.
- `internal/jobdef/lint/`: B2, C4. Different files where possible; B2 → C4. (D3's
  reference check lives in `internal/jobdef/agent_profile_refs.go`.)
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

- `docs/design/2026-09-16-identity-and-access-design.md` — the design of
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
