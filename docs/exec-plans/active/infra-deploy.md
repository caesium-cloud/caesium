# DAG-Native Infrastructure Deployment — `cache.chain` + the Terraform pack

Last updated: 2026-08-28

This plan ships the design in
[`docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`](../../superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md):
dependency-ordered deployment of infrastructure stacks as ordinary Caesium DAGs,
with unchanged stacks skipped and a provider set warmed once. Almost everything
composes shipped primitives (content-addressed cache, `##caesium::output-ref`,
branch steps, volumes, `secret://` env, datasets/lineage). **Exactly one core
change** is required — the `cache.chain: values` key (plus `ttl: never`) that
stops a step's identity hash from chaining its predecessors' identity hashes —
and everything else is a container-image pack, example manifests, a lint
warning, a Console panel over ordinary outputs, and documentation.

The deliverable is the generic **unit-pipeline pattern** (five container roles:
materialize, warm, discover, propose, apply) and Terraform as its first
*binding*. Caesium's Go never learns what HCL is; the pack images are the only
place Terraform knowledge lives, and a second, Terraform-free binding is part of
the test gate so the contracts cannot quietly grow Terraform-shaped.

Two grounding facts from this repo that the plan bakes in and the spec did not
spell out: (1) images publish to Docker Hub as `caesiumcloud/<name>` (the CI
`publish` job and the `triage-agent` precedent) — the spec's `ghcr.io/caesium/*`
references are illustrative, so the pack ships as `caesiumcloud/git-source`,
`caesiumcloud/tf-discover`, `caesiumcloud/tf-warm`, `caesiumcloud/tf-runner`;
(2) a branch-skipped task **cascades** (`propagateSkipped` in
`internal/job/job.go:706` skips every successor whose `triggerRule` is the
default `all_success`) and a skipped task has **no outputs**, so the
hand-written *branch* form of an empty apply would strand every downstream
stack that consumes its outputs. The plan therefore makes the **container
no-op** the default for any stack with consumers (apply always runs
`terraform output -json` and emits outputs; it only *applies* when the proposal
has non-zero counts) and reserves the branch form for leaf stacks. See Open
Questions.

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

Settled by the spec (§3, §7, §12) and binding on every item:

- **Caesium declares, orders, and mounts.** No HCL parsing, no Terraform
  execution, no cloud credentials, no state storage, no HTTP-backend protocol,
  no Terraform plugin in Caesium's Go. Terraform knowledge lives only inside
  the pack images (`terraform-exec` + `terraform-json`, per spec §6.7).
- **The pack is a separate Go module** (`pack/go.mod`, no dependency on the root
  module). This keeps `hashicorp/terraform-exec` / `terraform-json` out of the
  root `go.mod`/`go.sum` (Caesium core unaffected; no `go.sum` collision with
  sibling plans) and lets the images build from plain `golang:` + a pinned
  Terraform binary rather than the CGO/dqlite builder image. The cost — the
  root `just lint`/`just unit-test` do not cover `./pack/...` — is paid by
  dedicated `pack-lint`/`pack-test` targets wired into CI (B1, H-1).
- **Fingerprint gate, drift job mandatory** (spec §3.2/§6.6): the drift job is
  part of the feature, not an optional extra, and its steps never carry a
  `cache` block.
- **Warm-once, consume read-only** (spec §3.4): `filesystem_mirror`, never
  `TF_PLUGIN_CACHE_DIR`; RWX storage on Kubernetes; RWO/node-affinity deferred.
- **Out of scope, each with an existing home**: approval gates (roadmap §3.2),
  named exclusive locks, step-group templates (roadmap §2.2), node affinity,
  matrix fan-out, forge/PR-comment callbacks, a `caesium` Terraform provider.
- **Dynamic fan-out and structured partitions are owned by
  `docs/exec-plans/active/dynamic-fanout.md`** (Streams A/C there). This plan
  ships the hand-written per-stack form, which works today; the five-step
  fan-out form is documented as forward-looking and lands as an example only
  once fan-out ships. The fanout plan in turn defers the chain break to this
  plan's Stream A — the two plans meet at `cache.chain`, and neither may invent
  a second mechanism for the other's half.

## Source-Of-Truth Note

When this plan and
`docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`
disagree, the spec wins — for intent, scope, the role contracts (§5.2), the
`cache.chain` semantics (§4.3), the Terraform binding's behaviour (§6), the
security requirements (§6.4 sensitive handling), the failure modes (§8), and
the test scenarios (§9). The two exceptions recorded above (registry naming;
the branch-form cascade) are grounding corrections, not scope changes — if the
spec is amended to address them, the amended spec wins. Where the spec touches
the YAML contract, `pkg/jobdef/definition.go` is authoritative for the
*current* shape and Stream A changes it. Dynamic fan-out and structured
partition objects are owned by `docs/exec-plans/active/dynamic-fanout.md` and
`docs/design-dynamic-fanout.md`; tracking for them continues there.

## Progress (as of 2026-08-28)

All three implementation waves shipped on the `infra-deploy` integration
branch (master `b49d631` → `c0e35c4`, 49 commits, one PR pending). Every
stream was implemented by a worktree-isolated sub-agent, reviewed per stream
(spec compliance + code quality) with scoped re-reviews of each fix round, and
gated on the merged branch with the full chain: `just lint`, `just unit-test`,
`just pack-lint` / `pack-test` / `build-pack`, `just integration-test`,
`CAESIUM_INFRA_LANE=true just integration-test-infra` (10 `TestInfra*`
scenarios), `just ui-lint` / `ui-test` / `ui-e2e`. **All Acceptance Criteria
(1–8) hold** — see the per-stream notes under `## Streams` for the deviations
each stream recorded against the item text (the spec won every time).

### Wave 1 (2026-08-27) — A, B + H-1, D1, E1 in parallel

- **W1-α — Stream A (A1–A7), Opus.** `cache.chain: {transitive|values}` and
  `cache.ttl: never` ship end to end: resolved config + `Validate()`, a
  byte-identical-by-golden-test transitive hash, the mode threaded through the
  local / worker / replay lanes and persisted on `TaskRun` + the execution
  descriptor, the exclusion rendered by `caesium why` (stdout, table + `--json`,
  fanned groups included) and the Console, regenerated schema reference and
  prose docs, and `test/cache_chain_test.go` + `test/unit_pipeline_generic_test.go`
  driving the live server and CLI (the generic binding never references a pack
  image). Review round 1 fixed group-level `why` notes, a mis-explained
  chain-mode switch, and a short-circuit-confounded A6 assertion (negative
  control verified). Also fixed `caesium cache list` writing JSON to stderr.
- **W1-β — Stream B (B1–B5) + H-1, Opus.** The nested `pack/` module
  (`protocol` emitters with `FailClosed`, `fingerprint`, `tf.ReadManifest`),
  `build/Dockerfile.pack` on checksum-verified Terraform 1.15.9 (`TF_DIST`
  switchable), `git-source` (ls-files digest, `GIT_SSH_KNOWN_HOSTS`,
  `--end-of-options` + `GIT_ALLOW_PROTOCOL`, userinfo scrubbed from errors),
  `tf-discover` (relocated `TF_DATA_DIR` so `src` stays read-only; remote
  module dirs resolved against the data dir; deterministic `(Name, Path)`
  ordering with duplicate rejection), the hermetic `pack/testdata/infra`
  fixture (incl. a `git::file://` remote-module stack), the
  `integration-test-infra` lane with `CAESIUM_CACHE_ENABLED=true`, CI wiring
  (`pack-lint`/`pack-test`, `build-and-integration-test-infra`), a golden-seam
  test that feeds pack-emitted `##caesium::partitions` bytes through
  `pkg/task`'s parser, and six `TestInfra*` scenarios (§9 #7, #8, #10, the
  git-source contract, multi-root fan-out).
- **W1-γ — D1, Sonnet.** `internal/jobdef/lint.CheckVolumeWriters`, wired into
  server lint `resp.Warnings` and a new local `caesium job lint` warnings
  block (exit 0), with local + server integration coverage. Review round 1
  added subPath containment and raw `mounts: [{type: volume}]` coverage;
  refined again in W3-β (below).
- **W1-δ — E1, Sonnet.** `ProposalPanel` + `proposal-renderers.ts` registry
  (generic key/value fallback, `terraform.plan.v1` renderer with counts,
  resource table, artifact digest/size/path and no download) mounted in
  `TaskDetailPanel`; 12 Vitest cases + 1 Playwright scenario. Established the
  wire rule the runner follows: `proposal_summary` is a JSON-encoded *string*.

### Wave 2 (2026-08-27) — C, H-2, E2

- **W2-α — Stream C (C1–C6), Opus.** `tf-warm` (content-addressed provider
  mirror, per-process staging + atomic promotion, self-healing marker,
  `terraformrc` re-asserted on the fast path) and `tf-runner`
  (`tf-plan` / `tf-apply` / `tf-drift` on terraform-exec + terraform-json;
  typed `sensitive_values` stripping, digest-verified plan artifacts, a single
  `changes` answer shared by plan and apply, always-emit apply outputs,
  step-exact `IMPORT_OUTPUTS_FROM` with collision refusal, drift going red).
  `test/infra_deploy_test.go` asserts the exact re-ran / cached / skipped
  partition for §9 #1–#6, #9, #11 (with #2 load-bearing) plus the drift
  scenario and a diamond import. Three live defects found by dry-running the
  binaries (plan / output JSON teed into the task log; refresh-only exit 2 on
  output-only changes) and two review rounds (concurrent-warm race, sentinel
  collision on diamond imports) are all fixed and regression-tested.
- **W2-β — H-2, Sonnet.** `publish` builds and pushes multi-arch manifests for
  the four pack images with the caesium tag scheme; arm64 pack images are
  build-verified only (known gap recorded in the H-2 note).
- **W2-γ — E2, Sonnet.** `RunProposalSummary` on `RunDetailPage` aggregating
  proposal counts across a run, rows opening the task panel.

### Wave 3 (2026-08-27/28) — D2, N-1, N-2, and the D1 refinement

- **W3-α — D2 + N-1 + N-2, Sonnet.** `docs/examples/infra-deploy.job.yaml`
  and `infra-drift.job.yaml`, `docs/infrastructure-deployment.md` (indexed in
  `docs/README.md`), spec banner and roadmap row flipped to Shipped,
  dynamic-fanout cross-links confirmed. Grounding corrections recorded:
  Kubernetes `pvc:` (not `claimTemplate`) for the shared `src`;
  `caesium cache invalidate --job-id`; `schemaFrom: output` needs an
  `outputSchema`; drift and warm steps need explicit `cache: false`.
- **W3-β — fix round, Opus.** The D1 lint now models the real §8 hazard:
  only writers that can run *concurrently* warn (DAG-ordered pairs are
  exempt via `pkg/jobdef.DeriveStepSuccessors`; unresolvable graphs fail
  closed) and it is engine-aware (the docker engine drops
  `VolumeMount.SubPath`). That let the manifests drop volume aliasing and
  fixed the bug it masked (per-stack state on one shared volume collapsed to
  one state file on docker): state is now one volume per stack, RWX
  everywhere, the drift job has its own `ARTIFACT_DIR` and a concurrency
  block, `GIT_REF` is a literal ref, and `terraform init` runs
  `-lockfile=readonly` so `src` can stay read-only.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | `cache.chain` + `ttl: never` — schema, hash semantics + golden test, wiring at all three `HashInput` sites, `caesium why` rendering, integration + generic-binding scenarios, generated schema reference | **P0** | Shipped (W1-α) |
| B | Pack scaffold (nested module, `build/Dockerfile.pack`, protocol package) + `git-source` + `tf-discover` + hermetic fixture repo + discover/materialize integration scenarios | **P0** | Shipped (W1-β) |
| C | `tf-warm` + `tf-runner` (`tf-plan` / `tf-apply` / `tf-drift`) + the Terraform end-to-end scenarios (cache-skip, module edits, output chaining, warm marker, empty plan, sensitive values, drift) | **P0** | Shipped (W2-α) |
| D | Multi-writer volume lint warning + reference manifests (`infra-deploy`, `infra-drift`) in `docs/examples/` | P1 | Shipped (W1-γ, W3-α, W3-β) |
| E | Console proposal panel — `proposal_kind` renderer registry with generic fallback, `terraform.plan.v1` renderer, optional run-level aggregate | P2 | Shipped (W1-δ, W2-γ) |
| H | Harness — `integration-test-infra` lane + CI job, `pack-lint`/`pack-test` in CI, multi-arch publish of the four pack images | P1 | Shipped (W1-β, W2-β) |
| N | Docs — `docs/infrastructure-deployment.md` user guide + README index; roadmap row + spec status close-out | P1 | Shipped (W3-α) |

### Follow-ups outside this plan (found while shipping)

- `caesium job lint --server`'s breaking-contract gate is unscoped to the
  lint target (`internal/contract/derive.go`, `cmd/job/lint.go`) — any
  unrelated breaking pair on a shared server fails every later server lint.
- The Docker engine ignores `VolumeMount.SubPath` (podman and kubernetes
  honour it); the lint is engine-aware, but the engine gap itself stands.
- No env-value interpolation exists, so a run param cannot reach `GIT_REF`;
  a `GIT_REF`-from-param mechanism (in `git-source` or the scheduler) is the
  next step for per-commit deploy triggers.
- `/cache/terraformrc` is a single slot: one provider set per `tfcache`
  volume until the manifest can address a per-key config file.
- The lossy `BuildOutputEnv` key round-trip means only snake_case Terraform
  output names survive `IMPORT_OUTPUTS_FROM` exactly.
- Fan-out partitions of one step writing one volume are not modelled as
  separate writers by the lint (documented).
- arm64 pack images are build-verified only until the infra lane has an
  arm64 twin; the infra lane needs `registry.terraform.io` egress for the
  first warm.
- `CAESIUM_CACHE_ENABLED` is unset on the default `integration-up` lane
  (scenarios there rely on `metadata.cache: true`).

## Streams` is the work
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

Settled by the spec (§3, §7, §12) and binding on every item:

- **Caesium declares, orders, and mounts.** No HCL parsing, no Terraform
  execution, no cloud credentials, no state storage, no HTTP-backend protocol,
  no Terraform plugin in Caesium's Go. Terraform knowledge lives only inside
  the pack images (`terraform-exec` + `terraform-json`, per spec §6.7).
- **The pack is a separate Go module** (`pack/go.mod`, no dependency on the root
  module). This keeps `hashicorp/terraform-exec` / `terraform-json` out of the
  root `go.mod`/`go.sum` (Caesium core unaffected; no `go.sum` collision with
  sibling plans) and lets the images build from plain `golang:` + a pinned
  Terraform binary rather than the CGO/dqlite builder image. The cost — the
  root `just lint`/`just unit-test` do not cover `./pack/...` — is paid by
  dedicated `pack-lint`/`pack-test` targets wired into CI (B1, H-1).
- **Fingerprint gate, drift job mandatory** (spec §3.2/§6.6): the drift job is
  part of the feature, not an optional extra, and its steps never carry a
  `cache` block.
- **Warm-once, consume read-only** (spec §3.4): `filesystem_mirror`, never
  `TF_PLUGIN_CACHE_DIR`; RWX storage on Kubernetes; RWO/node-affinity deferred.
- **Out of scope, each with an existing home**: approval gates (roadmap §3.2),
  named exclusive locks, step-group templates (roadmap §2.2), node affinity,
  matrix fan-out, forge/PR-comment callbacks, a `caesium` Terraform provider.
- **Dynamic fan-out and structured partitions are owned by
  `docs/exec-plans/active/dynamic-fanout.md`** (Streams A/C there). This plan
  ships the hand-written per-stack form, which works today; the five-step
  fan-out form is documented as forward-looking and lands as an example only
  once fan-out ships. The fanout plan in turn defers the chain break to this
  plan's Stream A — the two plans meet at `cache.chain`, and neither may invent
  a second mechanism for the other's half.

## Source-Of-Truth Note

When this plan and
`docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`
disagree, the spec wins — for intent, scope, the role contracts (§5.2), the
`cache.chain` semantics (§4.3), the Terraform binding's behaviour (§6), the
security requirements (§6.4 sensitive handling), the failure modes (§8), and
the test scenarios (§9). The two exceptions recorded above (registry naming;
the branch-form cascade) are grounding corrections, not scope changes — if the
spec is amended to address them, the amended spec wins. Where the spec touches
the YAML contract, `pkg/jobdef/definition.go` is authoritative for the
*current* shape and Stream A changes it. Dynamic fan-out and structured
partition objects are owned by `docs/exec-plans/active/dynamic-fanout.md` and
`docs/design-dynamic-fanout.md`; tracking for them continues there.

## Progress (as of 2026-08-26)

No implementation waves have shipped yet. The plan was drafted from the spec
merged in #343 (with the structured-partition amendment to the fan-out design
in #344 already landed, so the ownership boundary between the two plans is
settled); the first wave is the next eligible run of the `exec-plan-wave`
skill against this doc.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | `cache.chain` + `ttl: never` — schema, hash semantics + golden test, wiring at all three `HashInput` sites, `caesium why` rendering, integration + generic-binding scenarios, generated schema reference | **P0** | Not started |
| B | Pack scaffold (nested module, `build/Dockerfile.pack`, protocol package) + `git-source` + `tf-discover` + hermetic fixture repo + discover/materialize integration scenarios | **P0** | Not started |
| C | `tf-warm` + `tf-runner` (`tf-plan` / `tf-apply` / `tf-drift`) + the Terraform end-to-end scenarios (cache-skip, module edits, output chaining, warm marker, empty plan, sensitive values, drift) | **P0** | Not started |
| D | Multi-writer volume lint warning + reference manifests (`infra-deploy`, `infra-drift`) in `docs/examples/` | P1 | Not started |
| E | Console proposal panel — `proposal_kind` renderer registry with generic fallback, `terraform.plan.v1` renderer, optional run-level aggregate | P2 | Not started |
| H | Harness — `integration-test-infra` lane + CI job, `pack-lint`/`pack-test` in CI, multi-arch publish of the four pack images | P1 | Not started |
| N | Docs — `docs/infrastructure-deployment.md` user guide + README index; roadmap row + spec status close-out | P1 | Not started |

## Streams

### Stream A — `cache.chain` + `ttl: never` (the one core change)

The only change to Caesium's Go (spec §4). The identity hash is computed before
execution, so a checkout step's hash can only contain its inputs (including the
git ref, which moves every commit) and that churn propagates through
`PredecessorHashes` to every downstream stack. `chain: values` excludes
`PredecessorHashes` from the key while keeping `PredecessorOutputs` — "my key is
what I consume, not my predecessors' internal churn". `PredecessorOutputs` is
already direct-edge only (`internal/job/job.go:909-922`,
`internal/run/store.go:4409`), which is what makes this sufficient. Default
`transitive` must stay **byte-identical** to today's hash so every existing
cache entry survives. This stream is independently useful to any pipeline with
a shared upstream step, and is the prerequisite for scenario 2 (the
load-bearing "edit one stack, re-apply one stack" test).

- [x] A1. Add `chain` and `ttl: never` to the resolved cache config. Extend
      `CacheConfig` with `Chain string` (constants `CacheChainTransitive =
      "transitive"`, `CacheChainValues = "values"`; default transitive) and
      `TTLNever bool`; teach `applyCache` to read `chain` (string) and the
      literal `ttl: "never"` (other strings keep the existing
      `time.ParseDuration` path). `chain` flows through the same job-level →
      step-level layering as `pinDigests`, so `metadata.cache.chain` works as a
      job default with zero extra code (Open Question 1 — allowed, documented as
      the sharp edge). Reject unknown `chain` values in `Validate()` (the only
      new validation; unparseable `ttl` strings keep today's silent-ignore
      behaviour to avoid breaking existing manifests). Update the JSON schema
      (`cache` object gets `chain` enum + `ttl` `oneOf` duration/`never`).
      Files: `pkg/jobdef/definition.go` (`CacheConfig`, `ResolveCacheConfig`,
      `applyCache`, `Validate`), `pkg/jobdef/schema.go`, the existing cache
      config tests beside `definition.go`.
      Done (W1-α): `CacheChainTransitive`/`CacheChainValues`/`CacheTTLNever`
      constants + `NormalizeCacheChain`; `Chain`/`TTLNever` on `CacheConfig`;
      `validateCacheConfigs` rejects an unknown `chain` at job and step level
      while the resolver falls back to the inherited/transitive default so an
      unvalidated manifest fails safe. Tests in new
      `pkg/jobdef/cache_chain_test.go` (incl. a real YAML round-trip).
      **Deviation:** this repo has no machine JSON Schema for the manifest
      (`pkg/jobdef/schema.go` validates `outputSchema`/`inputSchema` only); the
      schema surface is the generated `docs/job-schema-reference.md`, updated in
      A7.
- [x] A2. Implement the hash semantics with a golden guard. Add `Chain string`
      to `cache.HashInput`; in `Compute()` skip the `pred_hash:` lines and write
      exactly one `cache_chain:values\n` line when `Chain == values`; write
      **nothing new** in transitive mode. Add `Chain string
      json:"chain,omitempty"` to `HashInputBlob` and a
      `PredecessorHashesExcluded bool` to its summary so `why` can name the
      exclusion; `HashInputBlobVersion` stays 1 (additive, `omitempty`). Tests:
      a golden test that pins today's hash for a fixed transitive input as a
      string literal (not a recomputation) and asserts byte-identity; values
      mode: changing a predecessor hash leaves the key unchanged, changing a
      predecessor output changes it; `CanonicalJSON` round-trips `chain`.
      Files: `internal/cache/hash.go`, `internal/cache/hash_test.go`.
      Depends on: A1.
      Done (W1-α): `HashInput.Chain`; `Compute()` writes one framed
      `cache_chain:values` line in place of the `pred_hash:` lines and nothing
      new in transitive mode. The blob gains `chain,omitempty` and the oversized
      summary gains `predecessorHashesExcluded`; `HashInputBlobVersion` stays 1.
      `TestCompute_GoldenTransitiveChainUnchanged` pins the transitive digest as
      a string literal for BOTH an unset and an explicit `transitive` Chain.
- [x] A3. Wire the resolved config into all three `HashInput` construction sites
      and persist it. Add `CacheChain string` and `CacheTTLNever bool` columns to
      `TaskRun` beside `CachePinDigests` (scheduler-set in the initial snapshot at
      `internal/run/store.go:1210`; `TaskRun` is already a hot-path model so the
      columns migrate from struct tags on every shard — no `models.All` edit) and
      `Chain string` / `TTLNever bool` to `TaskExecutionCache` on the descriptor
      (`internal/models/run.go:237`) so replay and reproduce rebuild the same
      key. Pass `Chain` at `internal/job/job.go:999`,
      `internal/worker/runtime_executor.go:246`, and
      `internal/replay/replay.go:502`. Map `ttl: never` to a nil `ExpiresAt` at
      the two cache-entry writers (`internal/job/job.go:1140`,
      `internal/worker/runtime_executor.go:689`) — the store already models
      `ExpiresAt *time.Time`. Unit tests on the local and worker paths assert a
      values-mode step hits cache across a predecessor re-run whose outputs are
      unchanged.
      Files: `internal/models/run.go`, `internal/run/store.go`,
      `internal/job/job.go`, `internal/worker/runtime_executor.go`,
      `internal/replay/replay.go`, tests beside each.
      Depends on: A2.
      Done (W1-α): `TaskRun.CacheChain`/`CacheTTLNever` columns +
      `TaskExecutionCache.Chain`/`TTLNever` on the descriptor; `Chain` passed at
      all three construction sites. The THREE (not two) cache-entry writers now
      share a new `cache.EntryExpiry(createdAt, ttl, ttlNever)` helper so no lane
      can forget the `never` check.
- [x] A4. Render the exclusion in `caesium why` (spec §4.3: "or the skip becomes
      unexplainable"). `diffBlobs` emits a `predecessorHashes` `FieldChange`
      with change `excluded (chain: values)` whenever either side is
      values-mode, and `BlobDiff` carries a `Notes []string` (or equivalent)
      the summary line uses ("predecessor hashes excluded (chain: values)").
      `cmd/why` renders the note on **stdout** (never `cmd.Print*`) in both
      table and `--json` forms; the Console `TaskWhyView` renders it with a
      `data-testid`. Files: `internal/run/whydiff.go`, `internal/run/why.go`,
      `cmd/why/why.go`, `ui/src/features/jobs/TaskWhyView.tsx`,
      `ui/src/lib/api.ts` (explanation type), `ui/src/features/jobs/__tests__/`.
      Depends on: A2.
      Done (W1-α): new `fieldExcluded` kind + `FieldChange.Note`;
      `BlobDiff.Notes`, attached BEFORE the degraded early-returns so an
      oversized blob still names the exclusion. Excluded entries are filtered out
      of every "what changed" count (`discriminatingChanges`) in the summary, the
      CLI table and the Console, and out of `run diff`'s per-task change list —
      otherwise a cache HIT would report a changed field. Console testid:
      `task-why-chain-exclusion`. Fix round 1: (I-1) a FANNED step addressed
      without `--partition` carries no `Diff` at all, so `WhyGroup.Notes` was
      added (populated from the scheduler-set `cache_chain` column) and both
      `summarize` and the CLI's `renderTable` now read whichever channel the
      answer shape uses — this is the shape spec §5.5 itself uses; (I-2) a
      chain-mode SWITCH is now emitted as a real `chain` `FieldChange`, so a miss
      caused by adding `chain: values` is no longer mis-explained as "cause is
      outside the persisted hash inputs".
- [x] A5. Integration scenario for the chain break on a plain pipeline (no
      Terraform, `alpine:3.23` only): job with `upstream` (env derived from a run
      param, constant output) → `mid` (`cache: {version: 1, chain: values}`) →
      `leaf` (default chain). Run twice with a changed param: `upstream` re-runs,
      `mid` is `cached`, `leaf` under transitive re-runs; change `upstream`'s
      output → `mid` re-runs (outputs still chain). `ttl: never` on `mid` →
      `caesium cache list --json` shows a null expiry; `caesium why --json
      <run> mid` contains the exclusion note. All machine output via
      `runCLIStdout`. Files: new `test/cache_chain_test.go`.
      Depends on: A3, A4.
      Done (W1-α) with two **deviations**, both forced by shipped behaviour:
      (1) run params are hashed into EVERY step's key, so a changed param busts
      `mid` too; the churn is instead a re-applied edit to the upstream step's
      command, which is the real git-ref case. (2) an upstream that re-runs with
      byte-identical OUTPUT is neutralised by the value-verified short-circuit
      (`EquivalentPriorHash`), so the churning upstream is a `warm-cache`-shaped
      step that emits NOTHING (short-circuit guard 2). A `direct` control on the
      default chain re-runs in the same run, proving the cascade is real. Also
      fixed `caesium cache list` writing its JSON to stderr via `cmd.Print*`.
- [x] A6. The generic unit-pipeline binding (spec §9.12 — the genericity
      guarantee). A Terraform-free job over a docker named volume driving the
      exact DAG shape of the Terraform form: two per-unit `discover` steps that
      emit hand-computed `fingerprint` outputs, `propose` steps (`chain:
      values`) that write a file and emit `##caesium::output-ref` plus a
      `##caesium::branch` on change, `apply` steps that read the artifact and
      emit an output consumed by the second unit. Second run → both units
      `cached`; bump one unit's fingerprint → only that unit's propose/apply
      re-run; the consumer of a changed *output* re-runs. **Must never reference
      a pack image** — if the §5.2 contracts grow Terraform-shaped, this is what
      fails. Files: new `test/unit_pipeline_generic_test.go`.
      Depends on: A3.
      Done (W1-α): `source → discover-{a,b} → propose-{a,b} → apply-{a,b}` plus
      the §5.5 `warm` role (emits nothing, feeds every propose/apply) and a
      default-chain `control` consumer of it, over a docker/podman named volume,
      alpine:3.23 only; the test asserts the fixture contains neither
      `caesiumcloud/` nor `terraform`. Per spec §5.5 (which overrides the item
      text) BOTH propose and apply carry `chain: values`, and apply also
      `ttl: never`. Fix round 1 (review I-3): the fingerprint-bump scenario alone
      was confounded by the value-verified short-circuit, so a `warm`-churn
      scenario was added — it fails if `chain: values` is removed from any
      propose/apply step (verified by temporarily removing it).
- [x] A7. Docs + generated schema reference for the new keys. Update the job-
      and step-level `cache` rows in `internal/jobdef/report/report.go` (never
      hand-edit `docs/job-schema-reference.md` — regenerate it, or
      `TestGeneratedSchemaReferenceIsCurrent` goes red), then document the
      semantics table (spec §4.3) and the sharp edge (§4.4) in
      `docs/job-definitions.md` (Enabling Cache / Cache Behavior),
      `docs/caesium-job-llm-reference.md`, and add a `chain` section to the
      cache design of record `docs/design-incremental-execution.md`. Run
      `caesium job lint --path docs/examples/`.
      Files: `internal/jobdef/report/report.go`, `docs/job-schema-reference.md`
      (generated), `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/design-incremental-execution.md`.
      Depends on: A1, A4.
      Done (W1-α): a new "Cache Chain" section in `report.go` (semantics table +
      sharp edge) and `docs/job-schema-reference.md` regenerated from it; prose
      + example in `job-definitions.md` and the LLM reference; rationale and
      lane-threading in `design-incremental-execution.md`.
      `caesium job lint --path docs/examples/` validates 28 definitions.

### Stream B — Pack scaffold, `git-source`, `tf-discover`, hermetic fixture

The materialize and discover roles (spec §5.1, §6.1, §6.2) plus everything the
pack needs to exist at all: the nested module, the multi-stage Dockerfile, the
protocol package that emits the stdout markers, and the hermetic fake infra
repo every later scenario drives. `tf-discover` is the load-bearing image: it
owns the fingerprint, uses `terraform get` + the `.terraform/modules/modules.json`
manifest (not `terraform modules -json`, whose JSON drops the parent path), and
fails closed on anything unexpected.

- [x] B1. Scaffold the pack. Create the nested module `pack/go.mod`
      (`github.com/caesium-cloud/caesium/pack`; **no** dependency on the root
      module; add `hashicorp/terraform-exec` + `hashicorp/terraform-json` here
      up front so B and C never both add deps), `pack/internal/protocol/`
      (emitters for `##caesium::output`, `##caesium::output-ref` with sha256 +
      size, `##caesium::partitions` in the object form, `##caesium::branch`;
      buffered emission plus a `FailClosed(err)` helper so a late error exits
      non-zero with **no** marker written), `build/Dockerfile.pack` (multi-stage:
      `golang:` build stage, a Terraform-binary stage pinned by `TF_VERSION`
      build-arg with checksum verification and `TF_DIST=terraform|opentofu`, and
      `--target git-source|tf-discover|tf-warm|tf-runner` runtime stages with a
      non-root user like `build/Dockerfile.triage-agent`), and justfile targets
      `build-pack` (four tags `caesiumcloud/<name>:{{tag}}` + `:latest`, mirroring
      `build-triage-agent`), `pack-lint`, `pack-test` (run inside the build stage
      so `terraform` is on `PATH` for tests). Files: new `pack/go.mod`,
      `pack/internal/protocol/*.go` + tests, new `build/Dockerfile.pack`,
      `justfile`.
      Landed: `pack/go.mod` (terraform-exec + terraform-json pinned up front
      via `pack/tools.go`), `pack/internal/protocol` (buffered emitter +
      `FailClosed`), `build/Dockerfile.pack` (Terraform 1.15.9, checksum-
      verified, `TF_DIST` switchable), justfile `pack-toolchain` / `pack-lint`
      / `pack-test` / `build-pack`. The four role `main.go`s exist as
      fail-closed "not implemented" entrypoints so `build-pack` yields four
      images from day one; B2/B4 and Stream C replace them.

- [x] B2. `git-source` (generic materialize role, spec §6.1). Env: `GIT_URL`,
      `GIT_REF`, `GIT_SPARSE` (space-separated paths), `GIT_SSH_KEY` (already
      resolved from `secret://` by Caesium; written to a 0600 temp file and
      never logged), `DEST` (default `/src`). Sparse shallow clone at the ref,
      then `treeDigest` = sha256 over the sorted output of
      `git ls-tree -r <ref> -- <paths>`; emits `commit`, `treeDigest`, `path`.
      Supports `file://` URLs so the hermetic fixture works with no network.
      Fail closed on every git error. Go tests over a temp repo.
      Files: new `pack/cmd/git-source/main.go` + `_test.go`.
      Depends on: B1.
      Landed. Deviation from the item text: the digest is taken over
      `git ls-files -s -z -- :(glob)<pattern>` rather than `git ls-tree -r`
      — `ls-tree` supports neither pathspec magic nor globbing, so the
      documented `stacks/**` patterns match nothing there and the digest
      would silently cover an empty set. The clone is fresh into an empty
      DEST, so the index is exactly the tree at the pinned commit. A `!`
      negation in `GIT_SPARSE` is rejected (no pathspec equivalent, so
      checkout and digest would describe different trees), and
      `GIT_SSH_KNOWN_HOSTS` was added so host-key checking can be strict
      rather than disabled.

- [x] B3. Hermetic fixture repo + test helper. `pack/testdata/infra/` with three
      stacks (`stacks/network` exporting `vpc_id`, `stacks/account` depending on
      network, `stacks/app-web` consuming `vpc_id` via `TF_VAR_vpc_id`), two
      shared modules (`modules/vpc`; `modules/tags` with a nested
      `modules/tags/inner` referenced by a *relative* `source` two levels deep —
      the manifest-vs-`modules -json` guard), `local` backend, `null` +
      `random` providers, one `sensitive = true` output, an optional
      `stacks/dynamic-source` stack whose module `source` is a variable (used
      only by the fail-closed test), and a `stacks.yaml` naming `dependsOn`
      order for multi-root mode (Open Question 4). Plus a Go helper in `test/`
      that copies the fixture to a temp dir, `git init`s + commits it, exposes
      "edit file X and commit" for the edit scenarios, and returns the bind
      path the job's docker `volumes` source points at. Files: new
      `pack/testdata/infra/**`, new `test/infra_fixture_test.go` (behind
      `//go:build integration`).
      Landed as `pack/testdata/infra/**` (three stacks, `modules/vpc`,
      `modules/tags` -> `modules/tags/inner` by relative source, `local`
      backend, `null`/`random` with committed multi-arch
      `.terraform.lock.hcl`, `network.admin_token` sensitive,
      `stacks/app-web/extra.auto.tfvars.json`) plus
      `test/infra_fixture_test.go` (copy + `git init` + edit-and-commit,
      host/container bind-path mapping, lane guard). Deviation: the
      dynamic-source stack lives at `fail-closed/dynamic-source`, NOT under
      `stacks/` — `stacks.yaml` is authoritative for the multi-root set and
      must cover every stack directory it scans, so a permanently broken
      stack inside `stacks/` would make every multi-root discover red.

- [x] B4. `tf-discover` (spec §6.2). Env: `SCAN_ROOT`, `TF_WORKSPACE`. Single-root
      mode (one stack; the hand-written form): `tfexec.Get`, read
      `.terraform/modules/modules.json` with a version-pinned parser (unexpected
      shape → hard failure), hash the union of resolved module `Dir`s plus
      `*.tf`, `*.tfvars`, `*.tfvars.json`, `*.tfquery.hcl`, `.terraform.lock.hcl`
      and the workspace name — relative paths only, sorted traversal, no mtimes,
      no locale-dependent ordering — and emit `##caesium::output {"fingerprint":
      "sha256:…", "input_<name>": "sha256:…"}` (per-input digests so `why` can
      name which input moved). Multi-root mode (a directory of stacks; the
      fan-out form): emit `##caesium::partitions [{key, fingerprint, dependsOn,
      root}]` reading `dependsOn` from `stacks.yaml`. Fail closed: any error
      exits non-zero with no fingerprint. Go tests against `pack/testdata/infra`
      (run via `pack-test`): byte-identical fingerprint across two runs;
      nested-relative-module dir included; `dynamic-source` → `terraform get`
      error → non-zero and no marker; unexpected manifest shape → non-zero.
      Files: new `pack/cmd/tf-discover/main.go`, new
      `pack/internal/fingerprint/`, new `pack/internal/tf/modules.go` + tests.
      Depends on: B1, B3.
      Landed. `pack/internal/tf` parses `modules.json` with
      `DisallowUnknownFields` + structural checks (verified against
      Terraform 1.15.9); `pack/internal/fingerprint` digests each module
      directory non-recursively over `*.tf`, `*.tf.json`, `*.tfvars`,
      `*.tfvars.json`, `*.tfquery.hcl`, `.terraform.lock.hcl`. Two
      additions beyond the item: `TF_DATA_DIR` is relocated to a per-stack
      temp dir so `terraform get` works against the `readOnly: true` source
      mount §5.5 declares (verified: without it, discover needs a writable
      source); and `stacks.yaml` must name exactly the stack directories on
      disk, since a stack present but unlisted would be silently dropped.

- [x] B5. Integration scenarios for materialize + discover through the live
      server (docker engine, fixture repo bind-mounted, images from the H-1
      lane): spec §9 #7 (`discover` exit 1 → `plan`/`apply` never run and the
      run is **red**, not green-with-skips), #8 (two runs → identical
      `fingerprint` output row), #10 (dynamic module source → red, discover's
      output row has no `fingerprint`), plus the `git-source` contract (`commit`
      and `treeDigest` present; the SSH key value never appears in task logs).
      Files: new `test/infra_discover_test.go`.
      Depends on: B2, B4, H-1.

### Stream C — `tf-warm`, `tf-runner`, and the Terraform end-to-end gate

The warm, propose, and apply roles (spec §6.3, §6.4) and the scenarios that
prove the whole pattern works: unchanged repo → everything `cached`; edit one
stack → one stack re-applies; outputs still chain under `chain: values`;
sensitive values never reach dqlite. `tf-runner` is one binary with `tf-plan`,
`tf-apply`, and `tf-drift` subcommands built on `terraform-exec` /
`terraform-json` so the two security requirements are typed field access, not
`jq`.

- [x] C1. `tf-warm` (spec §6.3). Read every `.terraform.lock.hcl` under
      `SRC` (default `/src`), derive the mirror key from the sorted
      provider/version/hash union, check `/cache/.warm/<key>` → exit fast if
      present. Otherwise `terraform providers mirror -platform=<TARGET_PLATFORM>`
      into `/cache/providers.tmp.<key>`, atomic rename to
      `/cache/providers/<key>`, write `/cache/terraformrc` with a
      `provider_installation { filesystem_mirror … }` block (and `direct`
      excluded), drop the marker. Emits nothing. Never given a `cache` block —
      the always-run + marker check is what makes a recreated volume
      self-healing. Go tests with a fake lockfile set and a fake mirror command.
      Files: new `pack/cmd/tf-warm/main.go`, new `pack/internal/tf/mirror.go` +
      tests.
      Depends on: B1.
      Done (W2-α). `pack/internal/tf/mirror.go` hand-parses `.terraform.lock.hcl`
      strictly (an unrecognised line is an error naming it) rather than pulling
      an HCL parser into the pack. `terraform providers mirror` refuses to run
      against a configuration whose modules are not installed, so the mirror is
      populated from a SYNTHETIC root module rendered from the lock file itself
      — no module resolution, and the `src` mount stays read-only. The key
      covers the target platforms as well as the provider set.
- [x] C2. `tf-runner tf-plan` (propose). Env: `STACK_ROOT` (or
      `CAESIUM_PARTITION_JSON.root` when fanned), `TF_CLI_CONFIG_FILE`,
      `TF_WORKSPACE`, `IMPORT_OUTPUTS_FROM=<step>[,<step>]` (exports every
      `CAESIUM_OUTPUT_<STEP>_<KEY>` of the named upstream apply steps as
      `TF_VAR_<key>` — the cross-stack wiring, no `terraform_remote_state`),
      `APPLY_STEP` (optional; when set, emit `##caesium::branch <APPLY_STEP>`
      only on changes — the leaf-stack branch form), `ARTIFACT_DIR` (default
      `<root>/.caesium/`). `Init(-input=false)` offline against the mirror,
      `WorkspaceSelect`, `Plan(Out=tf.plan)` → boolean; on changes `ShowPlanFile`
      → strip `sensitive_values` via `tfjson` → emit `proposal_kind =
      terraform.plan.v1`, `proposal_summary` (counts by action + a capped
      per-resource address/action list), `proposal_artifact = "plan"`, and
      `##caesium::output-ref {"key":"plan","path":…,"digest":"sha256:…","size":…}`;
      on no changes emit a zero-count summary, no artifact, no branch marker;
      on error exit non-zero with no markers. Go tests over the fixture.
      Files: new `pack/cmd/tf-runner/main.go`, new `pack/internal/tf/plan.go`,
      `pack/internal/tf/summary.go` + tests.
      Depends on: B1, B3.
      Done (W2-α), in one commit with C3 and C4: the three phases share one
      config, one prepared `tf.Runner` and one proposal type, so splitting them
      would have produced commits that do not compile. The phases are tested
      against REAL Terraform with no network at all, over a new provider-free
      root module (`pack/testdata/offline`) — the infra fixture needs
      `null`/`random`, which `just pack-test` cannot download.
- [x] C3. `tf-runner tf-apply`. Env: `PLAN_STEP` (locates
      `CAESIUM_OUTPUT_<PLAN>_PROPOSAL_*`). Parse `proposal_summary`; when counts
      are non-zero, verify the artifact file's sha256 matches the ref digest
      before `Apply(planfile)`; when zero, do **not** invoke apply. In **both**
      cases run `Output()`, drop every `Sensitive` output, and emit
      `##caesium::output` — the always-emit is what keeps cross-stack wiring
      alive when this stack's plan was empty (grounding finding: a
      branch-skipped apply has no outputs and cascades). Digest mismatch or any
      Terraform error → non-zero. Go tests over the fixture.
      Files: `pack/cmd/tf-runner/main.go`, new `pack/internal/tf/apply.go` +
      tests.
      Depends on: C2.
      Done (W2-α), in `pack/internal/tf/apply.go` plus the shared `tf-runner`
      commit. `PublishableOutputs` withholds every `Sensitive` output through
      terraform-json's typed `OutputMeta`, and a non-scalar output is rendered
      as compact JSON so `pkg/task.ParseOutput`'s scalar filter does not drop
      the key. One addition: `HasChanges()` counts OUTPUT changes too — a plan
      whose only change is an output still has to be applied, because
      `terraform output` reads the state and an unapplied output change leaves
      every consuming stack on a stale value.
- [x] C4. `tf-runner tf-drift` (spec §6.6). `plan -refresh-only
      -detailed-exitcode`: exit 0 → `##caesium::output {"drift":"false"}`; exit 2
      → emit `{"drift":"true", "drift_summary": <sensitive-stripped counts>}`
      then **exit non-zero** so the task and run fail and the shipped
      notification / callback / `metadata.remediation` incident paths fire;
      never writes an artifact. Files: `pack/cmd/tf-runner/main.go`, new
      `pack/internal/tf/drift.go` + tests.
      Depends on: C2.
      Done (W2-α), in `pack/cmd/tf-runner/main.go` plus `plan.go`'s
      `RefreshOnlyPlan`. No separate `drift.go`: the phase is ~30 lines over the
      shared Runner and a file of its own would only spread the Terraform
      surface. Two corrections. The refresh-only plan is written to a SCRATCH
      file (never the artifact directory, and never referenced) so the counts
      come from typed `resource_drift` rather than scraped text. And output
      changes are counted: `plan -refresh-only -detailed-exitcode` returns 2 for
      an output change as readily as for resource drift, so without that a real
      exit 2 arrived with an all-zero summary — the shape an operator reads as a
      false alarm.
- [x] C5. The Terraform end-to-end gate: `test/infra_deploy_test.go` generating
      the hand-written three-stack job (`checkout` → `warm-cache` + per-stack
      `discover-<s>` → `plan-<s>` (`chain: values`) → `apply-<s>` (`chain:
      values, ttl: never`, `datasets.produces: stack:test/<s>`); `plan-app-web`
      also `dependsOn` `apply-network` with `IMPORT_OUTPUTS_FROM=apply-network`;
      `apply-app-web` uses the **branch form** because it is a leaf,
      `apply-network`/`apply-account` use the container no-op) over the B3
      fixture. Asserts spec §9: #1 unchanged repo, second run → every plan and
      apply `cached` and no new stack containers; #2 edit `stacks/app-web` →
      only app-web's plan/apply re-run (**load-bearing**); #3 edit `modules/vpc`
      → exactly its users re-run; #4 change network's `vpc_id` output → app-web
      re-plans though its code is untouched; #5 warm populated once, parallel
      plans read-only (`readOnly: true` on every non-warm mount), second run's
      warm step exits on the marker (assert via its logs); #6 empty plan is
      green in both forms — leaf apply `skipped` via the branch marker, non-leaf
      apply `succeeded` without invoking Terraform and still emitting outputs;
      #9 the `sensitive = true` output appears in neither the task output row,
      `GET /v1/jobs/:id/runs/:run/tasks`, nor `caesium why --json`; #11 edit
      the two-level nested module → exactly the stacks using it re-apply. All
      `--json` via `runCLIStdout`.
      Files: new `test/infra_deploy_test.go`.
      Depends on: A3, B2, B3, B4, C1, C3, H-1.
      Done (W2-α), as three scenarios (the change gate, the empty plan, the
      nested module) so a failure is attributable. Three corrections to the
      item. There is no `GET /v1/jobs/:id/runs/:run/tasks` endpoint — the run's
      task list with its outputs is `GET /v1/jobs/:id/runs/:run_id`, and #9
      scans that response body verbatim. The pipeline needs a `cache: false`
      `prepare` step that clears the workspace, because the materialize role
      refuses a non-empty destination and a cached checkout would pin the
      pipeline to the first run's tree. And Terraform state needs
      `-backend-config` onto a separate volume, because the source tree is
      re-cloned on every run and the fixture's `backend "local"` path is inside
      it — without that no plan is ever empty and #6 is untestable.
- [x] C6. Drift scenario: the drift job over the fixture (cron trigger fired
      manually; `checkout` + `warm-cache` reused; per-stack `drift-<s>` with
      **no** `cache` block). Clean state → green with `drift=false`; an
      out-of-band state edit (e.g. `terraform state rm` on a fixture resource)
      → run **red** with `drift=true` and a `drift_summary`; the drift step is
      never `cached` across runs.
      Files: `test/infra_deploy_test.go` (or new `test/infra_drift_test.go`).
      Depends on: C4, C5.
      Done (W2-α), in `test/infra_deploy_test.go`. Two corrections. The item's
      suggested `terraform state rm` does NOT produce refresh drift: `null` and
      `random` are state-only providers whose read is a no-op, so
      `plan -refresh-only` returns exit 0 for the three stacks under `stacks/`
      however their state is edited (verified against Terraform 1.15.9). The
      fixture therefore gains `drift/canary` — one `local_file` on a path
      outside the checked-out tree, whose provider read genuinely consults the
      filesystem — and the out-of-band change is deleting that file. And the
      drift steps carry `cache: false`, not merely "no cache block": the infra
      lane runs with `CAESIUM_CACHE_ENABLED=true`, under which an omitted block
      means cacheable, and the second run would then replay `drift=false`
      forever. Stream D's reference manifests should say `cache: false`
      explicitly for the same reason.

### Stream D — Multi-writer volume lint warning + reference manifests

The one lint rule the design calls for (spec §8 "two read-write mounts on one
volume") and the manifests operators copy. `internal/jobdef/lint/` today holds
only the secret checks; local `caesium job lint` prints only "Validated N" and
has no warnings channel, while the server lint response already carries
`Warnings []LintMessage`.

- [x] D1. Multi-writer volume lint warning. Add
      `internal/jobdef/lint/volumes.go` `CheckVolumeWriters(defs) []string`
      that warns when a named volume is mounted by more than one step without
      `readOnly: true` (per definition; message names the volume and the
      steps). Wire it into the server lint controller's `resp.Warnings` and
      give local `caesium job lint` a warnings block (stdout, after the
      "Validated N" line, non-zero exit **not** triggered — warning, not error,
      per Open Question 2). Integration test: `caesium job lint --path
      <two-writer fixture>` locally and against the server both print the
      warning (`runCLIStdout`), and `docs/examples/` produce none.
      Files: new `internal/jobdef/lint/volumes.go` + `_test.go`,
      `api/rest/controller/jobdef/lint.go`, `cmd/job/lint.go`, new scenario in
      `test/` (e.g. `test/lint_volumes_test.go`).
      > Shipped (W1-γ). `CheckVolumeWriters` groups write mounts (`readOnly`
      > omitted/false) by volume within a definition and clusters them by
      > subPath **containment**, not exact match: a mount with no subPath
      > exposes the whole volume and conflicts with every other write mount
      > of it, and a subPath conflicts with any subPath nested under it —
      > only genuinely disjoint siblings (`subPath: a` vs `subPath: b`) stay
      > silent (Open Question 2). Also covers the lower-level `mounts:
      > [{type: volume, source: <name>}]` mechanism (`container.Spec.Mounts`),
      > which has no subPath so any two of its write mounts of the same
      > source always conflict; the two mechanisms are checked independently
      > (not cross-referenced against each other — a documented v1 gap).
      > Fix round 1 corrected an initial exact-subPath-match version that
      > missed root-vs-subPath and nested-subPath conflicts; that also
      > required a root-vs-subPath fix in
      > `docs/examples/k8s-workload-identity-volume.job.yaml` (`plan-access`
      > moved from the volume root to a sibling `subPath: plans`, disjoint
      > from `write-cloud-report`'s `subPath: reports`).
      > Refined (W3-β) on two counts, both of which changed what the check
      > means rather than only what it catches:
      > **(1) DAG ordering.** The §8 hazard is CONCURRENT writers, so a pair
      > of write mounts whose steps are connected by a path of resolved
      > `dependsOn`/`next` edges (transitively, and including the implicit
      > sequential chain a definition with no explicit edges gets) is no
      > longer flagged — that is a handoff, not a race, and it is exactly
      > what `prepare` → `checkout` and `plan-<stack>` → `apply-<stack>` are.
      > Writers on parallel branches still warn. Edges come from
      > `pkg/jobdef.DeriveStepSuccessors`, the definition's own edge builder,
      > so the lint cannot disagree with the scheduler; an unresolvable graph
      > falls back to "no ordering proven" and still warns.
      > **(2) Engine awareness.** `internal/atom/docker`'s `convertMounts`
      > never sets `VolumeOptions.Subpath`, so on docker (and on an unset
      > engine, which pkg/jobdef defaults to docker) EVERY mount covers the
      > whole volume no matter what `subPath` says. The check now treats
      > docker mounts as root mounts and only honours `subPath` for
      > kubernetes/podman. Two parallel docker steps with `subPath: a` and
      > `subPath: b` therefore warn — which is correct, and is the class of
      > silent state-clobbering the D2 manifests had before W3-β.
      > This made the previous integration fixtures ambiguous (their steps
      > had no explicit edges, so they were implicitly ordered and would have
      > gone silent for the wrong reason): every "warns" fixture is now wired
      > explicitly parallel off a `seed` fan-out, plus new ordered-pair
      > (silent, local **and** `--server`) and docker-vs-kubernetes subPath
      > fixtures.
      > Fix round 1 (W3-β) bounds the exemption's SCOPE, after review flagged
      > that both limits were stated unconditionally: ordering is evaluated
      > within a SINGLE run (persistent volumes shared by overlapping runs of
      > one job are not covered — constrain them with `metadata.concurrency`,
      > which is therefore load-bearing for this check's silence rather than
      > hygiene), and the check runs per DEFINITION (two jobs whose volumes
      > resolve to the same physical store are never compared). Both are now
      > documented gaps in the function's doc comment and in N-1. The warning
      > text now says "not all pairwise ordered": a bridged cluster can
      > legitimately contain an ordered pair, and the old wording sent
      > operators hunting for a missing edge between them.
- [x] D2. Reference manifests in `docs/examples/`. `infra-deploy.job.yaml` — the
      hand-written three-stack form from C5 with an HTTP trigger (hydrate-safe),
      engine-keyed volume sources (docker/podman named volumes, kubernetes
      `pvc` with `ReadWriteMany` for `tfcache`, `claimTemplate` for `src`),
      `metadata.concurrency: {maxRuns: 1, strategy: queue}`, `schemaValidation:
      fail`, `outputSchema` on checkout/discover/plan, `secret://env/DEPLOY_KEY`,
      digest-pinned `caesiumcloud/*` image references, `chain: values` on
      plan/apply and `ttl: never` on apply, `datasets.produces` on apply.
      `infra-drift.job.yaml` — cron trigger, no `cache` block on drift steps.
      Both must pass `internal/jobdef/examples_test.go` and `caesium job lint
      --path docs/examples/` and stay free of the D1 warning. **The five-step
      fan-out form is not an example file** until dynamic fan-out ships
      (examples are lint-validated and `fanOut` does not exist yet) — it lives
      as a clearly-labelled snippet in N-1.
      Files: new `docs/examples/infra-deploy.job.yaml`, new
      `docs/examples/infra-drift.job.yaml`.
      Depends on: A1, C3, C4.
      > Shipped (W3-α). Grounding correction: `src` uses kubernetes `pvc:`, not
      > `claimTemplate` — `docs/job-definitions.md`'s own volumes section
      > documents `claimTemplate` as an inline PVC scoped to **one pod/step**,
      > and `src` here is mounted by all nine pipeline steps (prepare through
      > every plan/apply). `discover`/`plan`/`apply` mount `src` `readOnly:
      > true` (verified empirically against the pinned Terraform 1.15.9 that
      > `init` needs no write access to the root module directory when
      > `.terraform.lock.hcl` is already committed and `TF_DATA_DIR` is
      > relocated via `ARTIFACT_DIR`); only `prepare`+`checkout` write it.
      > `datasets.produces` on apply omits `schemaFrom: output` (an apply step
      > has no static `outputSchema` — its outputs are whatever the stack
      > exports), matching C5's own gate manifest. `warm-cache` carries
      > `cache: false` in both files (an omitted block is cacheable when
      > `CAESIUM_CACHE_ENABLED=true`, and warm must always run and self-check
      > its marker — spec §6.3). `infra-drift.job.yaml`'s three `drift-<stack>`
      > steps run in parallel (`dependsOn: [warm-cache]` each), not serialized
      > as C5's test fixture does — `propagateSkipped`
      > (`internal/job/job.go:803`) only cascades skips along DAG edges, so an
      > unrelated sibling's failure cannot skip a parallel step; C5's
      > serialization was test-determinism hygiene, not a manifest
      > requirement.
      > De-aliased (W3-β), together with D1's refinement. The volume aliasing
      > this note originally described (`src`/`src-reset`,
      > `tfstate`/`tfstate-apply` — one physical store under two logical
      > names, so the multi-writer lint saw one writer each) is **gone**: the
      > refined D1 rule exempts DAG-ordered writers directly, so both
      > manifests now declare one logical name per physical store and are
      > silent honestly rather than by hiding a real pair of writers from the
      > check. Also fixed a correctness bug the aliasing had masked: per-stack
      > Terraform state was isolated by mounting one shared `tfstate` volume
      > with `subPath: <stack>` while every stack used the identical
      > `BACKEND_CONFIG: path=/state/terraform.tfstate` — and the **Docker
      > engine drops `VolumeMount.SubPath` entirely**
      > (`internal/atom/docker/engine.go`; only kubernetes and podman apply
      > it), so on the default engine all three stacks wrote the same state
      > file and the last apply to finish silently won. Replaced with one
      > volume per stack (`tfstate-network` / `tfstate-account` /
      > `tfstate-app-web`, no `subPath`), each written only by its own
      > DAG-ordered `plan` → `apply`. `accessMode: ReadWriteMany` is now on
      > every volume more than one step mounts (state volumes included — a
      > stack's plan and apply are separate pods and may land on different
      > nodes) and the kubernetes PVC names carry the matching `-rwx` suffix.
      > Fix round 1 (W3-β): the drift job shares the deploy job's `tfcache` and
      > `tfstate-<stack>` STORES across definitions, which the per-definition
      > lint cannot see — and both jobs used `ARTIFACT_DIR: /state/artifacts`,
      > so a concurrent drift and deploy of one stack would have run two
      > `terraform init -reconfigure` into the same `<ARTIFACT_DIR>/tfdata`
      > `.terraform/` (silent corruption; Terraform's state lock does not cover
      > the data directory). `infra-drift-demo` now uses
      > `/state/drift-artifacts` and carries `metadata.concurrency: {maxRuns:
      > 1, strategy: skip}`, and `infra-deploy-demo`'s existing block is
      > re-commented as load-bearing for the lint's silence rather than
      > queueing hygiene. The shared provider mirror needed no change: `tf-warm`
      > stages into its own `MkdirTemp`, promotes by atomic rename and adopts
      > the winner on a lost race, so concurrent warms of one key are
      > idempotent — the residual (two jobs whose lock-file unions diverge would
      > flip the shared `terraformrc`) is the pre-existing one-provider-set-per-
      > `tfcache` limitation and is now written down.
      > Fix round 2 (W3-β): `checkout`'s `GIT_REF` was
      > `"${CAESIUM_PARAM_SHA}"`, copied from the spec's illustrative §5.5
      > snippet — but Caesium does not interpolate `${…}` in env values (run
      > params arrive as their own `CAESIUM_PARAM_<KEY>` variables, and
      > `git-source` is a Go binary with no shell), so the flagship example's
      > FIRST step would have failed on a literal string. Pinned to
      > `GIT_REF: "main"`, the proven fixture shape, with the limitation
      > written up in the guide's `git-source` section and its env table.

### Stream E — Console proposal panel

Spec §5.6: a propose step emitting the three reserved keys (`proposal_kind`,
`proposal_summary`, `proposal_artifact`) is rendered as a proposal. A
convention over ordinary outputs — no new marker, no schema key, no endpoint.
The Console never fetches the artifact (Caesium is not in the data path); it
renders the summary and shows the reference.

- [x] E1. `ProposalPanel` with a renderer registry. New
      `ui/src/features/jobs/ProposalPanel.tsx` mounted in `TaskDetailPanel.tsx`
      when `runTask.output` carries `proposal_kind`; `proposal-renderers.ts`
      keyed on kind with a **generic key/value fallback** (an unknown kind
      still renders) and a `terraform.plan.v1` renderer (add/change/destroy
      counts, per-resource address/action table, the artifact reference's
      digest/size/path with no download). Vitest tests for both renderers and
      the fallback; one Playwright scenario in `ui/e2e/` driven by an
      `alpine:3.23` step that echoes the three keys (no Terraform).
      Files: new `ui/src/features/jobs/ProposalPanel.tsx`, new
      `ui/src/features/jobs/proposal-renderers.ts`,
      `ui/src/features/jobs/TaskDetailPanel.tsx`,
      `ui/src/features/jobs/__tests__/ProposalPanel.test.tsx`, `ui/e2e/`.
      Note (W1-δ): `proposal_summary` is a JSON-*string* value inside the
      `##caesium::output` map (the marker's scalar-only rule drops raw nested
      objects — `pkg/task/output.go`'s `scalarOutputValue`), so the assumed
      wire shape is `{"proposal_kind":"terraform.plan.v1","proposal_summary":
      "{\"add\":2,...,\"resources\":[{\"address\":...,\"action\":...}]}",
      "proposal_artifact":"plan"}` plus a sibling `##caesium::output-ref` for
      the artifact key, encoded exactly as `OutputRef.Encode` writes it
      (`{"caesiumOutputRef":1,"path":...,"digest":"sha256:...","size":...}`).
      The renderer parses that encoding defensively (re-implemented in TS,
      not imported — the UI never imports Go); the Terraform runner (C2)
      should match this shape. `proposal-renderers.ts` stays a pure `.ts`
      module (no JSX, per the `cache-utils.ts`/`CacheView.tsx` split already
      in this file); `ProposalPanel.tsx` does the rendering. E2 (run-level
      aggregate) is out of scope here.
- [x] E2. Run-level proposal summary (Open Question 3 — the "more useful and
      more work" half). A `RunProposalSummary` on `RunDetailPage` aggregating
      counts across every task in the run with a `proposal_kind`, linking each
      row to its task panel. Files: new
      `ui/src/features/jobs/RunProposalSummary.tsx`,
      `ui/src/features/jobs/RunDetailPage.tsx`, tests.
      Depends on: E1.
      Note (W2-γ): `RunProposalSummary` iterates `run.tasks`, calls
      `parseProposal` (from `proposal-renderers.ts`, not duplicated) per task,
      and drops any task without a `proposal_kind`; renders nothing when the
      resulting list is empty. Per-action counts are summed only across
      `structured`-section proposals (currently just `terraform.plan.v1`); a
      `generic`-section proposal (unregistered kind, or an unparsable
      `proposal_summary`) still gets a row — kind badge + task name — with a
      blank-count marker instead of crashing or fabricating zero counts. Each
      row is a button wired to the same toggle handler now shared with the
      DAG's `onNodeClick` (`handleTaskSelect` in `RunDetailPage`, extracted
      from the inline lambda that previously only fed the DAG) so row-click
      and node-click open/close the same `TaskDetailPanel` identically.
      Mounted between the interactive DAG section and `CallbackRunsSection`.
      Did not touch `ProposalPanel.tsx`/`proposal-renderers.ts`; the
      count-to-badge-variant color mapping is a small local copy (not
      exported by `ProposalPanel.tsx`, and it's display-only, not the parsing
      this item was told to reuse rather than duplicate).

      Landed as `test/infra_discover_test.go`: five `TestInfra…` scenarios on
      the live server covering §9 #7 (discover exits 1 -> run red, plan and
      apply never run, no fingerprint), #8 (two checkouts of the same tree
      at DIFFERENT host paths -> byte-identical fingerprint, which also
      proves path-independence), #10 (dynamic module source -> red, no
      fingerprint, Terraform's own diagnosis in the log), and the
      `git-source` contract (`commit` matches the fixture HEAD,
      `treeDigest` present, `GIT_SSH_KEY` resolved through the real
      `secret://env` provider with its value absent from every task log and
      from the stored spec). The fixture repo and the shared workspace are
      bind-mounted from HOST paths (`CAESIUM_HOST_PROJECT_ROOT`); discover's
      source mount is `readOnly: true`.

## Harness Strengthening

- [x] H-1. `integration-test-infra` lane + CI wiring. Add `integration-up-infra`
      (`build-test` + `build-pack`; same env as `integration-up`) and
      `integration-test-infra` (runs `go test ./test/ -tags=integration -run
      'TestInfra'` against it, mirroring `integration-test-agent`'s own-server
      shape). Infra scenarios read `CAESIUM_PACK_IMAGE_TAG` (default `latest`)
      for image references and skip **with an explicit reason** unless
      `CAESIUM_INFRA_LANE=true` — the podman and helm lanes run their own
      servers without the pack images and would otherwise drift red silently.
      Add CI job `build-and-integration-test-infra` to
      `.github/workflows/ci.yml`, and add `pack-lint` / `pack-test` steps to the
      existing `lint` and `unit-test` jobs (the nested module is invisible to
      `./...`). Files: `justfile`, `.github/workflows/ci.yml`,
      `test/infra_fixture_test.go` (lane guard helper).
      Depends on: B1.
      Landed: `integration-up-infra` / `integration-test-infra` /
      `integration-down-infra` on their own container
      (`caesium-server-infra-test`, no published port so the lane coexists
      with the others), running
      `-run 'TestIntegrationTestSuite/TestInfra'` with `CAESIUM_INFRA_LANE`,
      `CAESIUM_PACK_IMAGE_TAG` and `CAESIUM_HOST_PROJECT_ROOT` set. CI gains
      `build-and-integration-test-infra` (amd64, mirroring the other
      specialised lanes, which are not matrixed), a `pack-lint` step in
      `lint`, a `pack-test` step in `unit-test`, and the new job in
      `publish`'s `needs`.

- [x] H-2. Publish the pack images. Extend the `publish` job to build and push
      multi-arch manifests for `caesiumcloud/git-source`, `tf-discover`,
      `tf-warm`, `tf-runner` with the same tag scheme as `caesiumcloud/caesium`,
      and pin `TF_VERSION` in one place (the Dockerfile ARG default, surfaced as
      a justfile variable). Files: `.github/workflows/ci.yml`, `justfile`,
      `build/Dockerfile.pack`.
      Depends on: H-1.
      > `TF_VERSION`/`TF_DIST` pinning (Dockerfile ARG default + `tf_version`/
      > `tf_dist` justfile vars) already landed with H-1 (W1-β) — no further
      > change needed there. This item adds arm64 pack-image build/save/upload
      > steps to `build-and-integration-test-arm64` (amd64 pack images already
      > come out of `build-and-integration-test-infra`'s existing
      > `integration-test-infra` → `build-pack` chain) and a "Push pack
      > multi-arch manifests" step in `publish` that mirrors the existing
      > `caesium` push/manifest step exactly, looping over the four roles.
      > Could not be exercised locally (no Docker Hub credentials / tag push in
      > this environment); validated via YAML parse + `actionlint` (zero new
      > findings) and `just build-pack` for the host arch.
      > Known gap: arm64 pack images are build-verified only until the infra
      > lane gains an arm64 twin — `build-and-integration-test-arm64` runs the
      > general `integration-test` suite, not TestInfra (that suite is
      > amd64-only per H-1's deferred-harness note above), so the arm64
      > `git-source`/`tf-discover`/`tf-warm`/`tf-runner` images pushed by this
      > job are `docker build`-verified only, never exercised by TestInfra the
      > way the amd64 images are.

#### Deferred (harness)

- **Kubernetes/helm lane coverage of the pack** — needs an RWX-capable storage
  class in the kind cluster (the default local-path class is RWO). Out of scope;
  N-1 documents the RWX requirement and RWO stays unsupported until node
  affinity lands (spec §12).
- **Podman lane coverage** — the podman `volume` source exists, so the infra
  scenarios could later run under `integration-test-podman`; deferred until the
  docker lane is green.

## Navigational / Organizational Improvements

- [x] N-1. User guide `docs/infrastructure-deployment.md`: the unit-pipeline
      pattern (five roles, the §5.2 contracts as env/marker tables, the bindings
      table for dbt / monorepo CI / migrations), the Terraform binding (each
      image's env contract, exit codes, the `IMPORT_OUTPUTS_FROM` wiring, the
      two sensitive-value rules), **both forms** with the grounding finding
      stated plainly (container no-op is the default; branch form only for leaf
      stacks), the fan-out form as a forward-looking snippet marked "requires
      dynamic fan-out — not yet shipped", the drift job as mandatory plus the
      weekly full-apply backstop (`caesium cache invalidate --job`), the RWX
      requirement and RWO deferral, the `chain: values` sharp edge, the
      proposal convention, the failure-mode table (spec §8), and OpenTofu via
      `TF_DIST`. Index it in `docs/README.md` (the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail: a real link is
      required for a top-level doc; subdirectory references stay in backtick
      form), and cross-link from `docs/caesium-job-llm-reference.md` and the
      volumes section of `docs/job-definitions.md`.
      Files: new `docs/infrastructure-deployment.md`, `docs/README.md`,
      `docs/caesium-job-llm-reference.md`, `docs/job-definitions.md`.
      Depends on: C4, D2.
      > Shipped (W3-α). Corrections to the item text, checked against the
      > working tree as instructed: dynamic fan-out (including the
      > structured-partition amendment this pattern needs) **shipped in #349**
      > — the "requires dynamic fan-out — not yet shipped" framing would now be
      > false, so the fan-out snippet is worded as "mechanically valid, not
      > promoted to a lint-validated `docs/examples/` file this wave" instead.
      > The real `caesium cache invalidate` flag is `--job-id <uuid>`, not
      > `--job <alias>`. Also documents: `tf-drift` honours
      > `IMPORT_OUTPUTS_FROM` only when an apply step precedes it in the same
      > job, which the drift job's own shape never has, so
      > `infra-drift.job.yaml`'s `drift-app-web` pins `TF_VAR_vpc_id` directly
      > instead; `tfcache` serves exactly one provider set per volume (two
      > jobs/stacks with different lock-file unions must not share one); the
      > first warm of a given provider set needs outbound network to the
      > registry (the CI infra lane relies on runner egress for it; everything
      > after is offline, verified hermetic elsewhere in this plan); and the
      > plan artifact's digest changes on every re-plan (Terraform embeds a
      > timestamp), so apply re-running whenever plan re-runs is expected, not
      > a bug.
      > Extended (W3-β): adds the **digest-pinning** section the manifests'
      > own comments cross-referenced but that did not exist
      > (`#pinning-the-pack-images-to-a-digest` — why a tag is a mutable
      > pointer for the thing doing the applying, how to resolve a digest with
      > or without pulling, the `tf-warm`/`tf-runner` version coupling that
      > makes re-pinning safe, and the manifest-list-vs-platform-manifest
      > trap). Replaces "RWX requirement and RWO deferral" with a fuller
      > **Volumes** section: one logical volume per physical store, the Docker
      > `subPath` caveat as a call-out, `accessMode: ReadWriteMany` on every
      > multi-step volume with `-rwx`-suffixed PVC names, the read-only `src`
      > contract (including `tf-runner`'s `-lockfile=readonly` and the
      > `terraform providers lock -platform=…` remedy), and the refined
      > multi-writer lint semantics (ordered writers silent, parallel writers
      > warn, docker `subPath` warns, plus the documented false negatives).
      > Two new failure-mode rows (`subPath` relied on for
      > isolation; `terraform init` rewriting a read-only `src`), and a
      > caveat on the fan-out snippet that per-unit state isolation is an
      > open edge in that form.
      > Fix round 1 (W3-β) adds a "Sharing stores across jobs" subsection (why
      > the drift job aliases the deploy job's stores on purpose, and the three
      > load-bearing mitigations: a per-job `concurrency` block, distinct
      > `ARTIFACT_DIR`s, and idempotent warms), restructures the lint's Known
      > gaps into "what it compares" vs "the scope it reasons in", and adds a
      > failure-mode row for two jobs sharing one physical store.
- [x] N-2. Close-out. Flip the spec's `**Status:** Proposed` banner to Shipped
      with a link to this plan; update the Phase 4 roadmap row added at draft
      time to **Shipped**; confirm the `cache.chain` cross-links in
      `docs/exec-plans/active/dynamic-fanout.md` and
      `docs/design-dynamic-fanout.md` still describe the shipped behaviour; sync
      this plan's `## Progress` with merged PRs and move it to
      `docs/exec-plans/completed/`.
      Files: `docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`,
      `docs/roadmap.md`, `docs/exec-plans/active/dynamic-fanout.md`,
      `docs/design-dynamic-fanout.md`, this file.
      > Shipped (W3-α), scoped per this stream's workspace rules: banner
      > flipped, roadmap row flipped, cache.chain cross-links confirmed
      > accurate (one tense fix in `design-dynamic-fanout.md`: "the knob
      > proposed in §4" → "added, and shipped (Stream A), by §4" — no
      > "deferred to" phrasing was found anywhere referencing `cache.chain`).
      > **`## Progress` sync and the move to `exec-plans/completed/` are
      > deliberately NOT done here** — common.md's workspace rules reserve
      > both to the orchestrator ("Do NOT: move the plan doc to `completed/`,
      > edit `## Progress`... the orchestrator does the Progress sync +
      > archive"), which overrides this item's literal text. Coordination
      > note: the spec's banner and the roadmap row both link to
      > `exec-plans/active/infra-deploy.md`; whoever performs the
      > `completed/` move should grep both files (and this plan's own
      > cross-references) for that path and update it in the same change,
      > matching how `dynamic-fanout.md`'s "Shipped" roadmap row currently
      > still points at its own `active/` path pending the same move there.
      Depends on: A–E, H, N-1.

## Sequencing & Dependencies

**Cross-stream order**

- Streams A, B, D1, and E1 are independent and can all start in wave 1. B1 is
  the pack's foundation; B3 (the fixture) needs no code and can land alongside.
- Stream H depends on Stream B for H-1 (`build-pack` must exist); B5, C5, C6
  depend on H-1 (the lane that has the images).
- Stream C depends on Stream B (B1 scaffold, B3 fixture) for C1/C2, and C5
  additionally depends on Stream A (A3 — scenario 2 needs the chain break).
- D2 depends on A1 (the example must lint with `chain`/`ttl: never`) and on
  C3/C4 (the env contracts it encodes).
- N-1 depends on C4 and D2; N-2 runs last.
- **Sibling plan**: `docs/exec-plans/active/dynamic-fanout.md` (unstarted) edits
  `internal/cache/hash.go`, `internal/job/job.go`,
  `internal/worker/runtime_executor.go`, `pkg/jobdef/definition.go`,
  `internal/jobdef/report/report.go`, `internal/run/why.go`/`whydiff.go`, and
  `TaskDetailPanel.tsx` — the same files as Stream A (and E1). Land Stream A
  before the fanout plan's Streams A/B/C start, or rebase the later one; the
  two designs are compatible by construction (the fanout plan defers the chain
  break to A2/A3 here).

**Within-stream order**

- A: A1 → A2 → A3 → A4 → (A5, A6, A7 in parallel).
- B: B1 → (B2, B3 in parallel) → B4 → B5.
- C: (C1 ∥ C2) → C3 → C4 → C5 → C6.
- D: D1 ∥ D2.
- E: E1 → E2.
- H: H-1 → H-2.

**Cross-stream file conflicts**

- `pack/go.sum`: B1 adds every pack dependency up front; if a later B/C item
  needs another, resolve with `go mod tidy` inside `pack/`, never a hand-merge.
  The root `go.mod`/`go.sum` are **not** touched by this plan.
- `pkg/jobdef/definition.go` / `pkg/jobdef/schema.go`: A1 only in this plan;
  sequence against `dynamic-fanout.md` Stream B and the other active plans
  (`backtesting`, `data-circuit-breaker`, `resource-right-sizing`,
  `window-scheduling` all list it).
- `internal/cache/hash.go`: A2 only; same sibling caution.
- `internal/run/whydiff.go` + `internal/run/why.go` (A4) vs
  `internal/run/store.go` (A3): different files, same package — fine in one
  wave, but A4 depends on A2 anyway.
- `pack/cmd/tf-runner/main.go`: C2, C3, C4 all edit it — sequence C2 → C3 →
  C4 (they are chained by `Depends on:` already).
- `pack/internal/tf/`: B4 (`modules.go`), C1 (`mirror.go`), C2 (`plan.go`,
  `summary.go`), C3 (`apply.go`), C4 (`drift.go`) — separate files; safe in
  parallel.
- `justfile` + `.github/workflows/ci.yml`: B1 (justfile only), H-1, H-2 —
  sequence B1 → H-1 → H-2.
- `cmd/job/lint.go`: D1 only.
- `ui/src/features/jobs/TaskDetailPanel.tsx`: E1 only in this plan;
  `data-circuit-breaker.md` and `dynamic-fanout.md` also list it.
- `docs/job-definitions.md` + `docs/caesium-job-llm-reference.md`: A7 and N-1
  — sequence A7 → N-1.
- `docs/roadmap.md`: N-2 only (the row is added at draft time).
- `test/infra_fixture_test.go`: B3 creates it, H-1 adds the lane guard —
  sequence B3 → H-1 or land the guard in B3.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:

- `pack/**` or `build/Dockerfile.pack` (Streams B, C): `just pack-lint && just
  pack-test && just build-pack`.
- Infra scenarios (B5, C5, C6, and any `test/infra_*_test.go`): `just
  integration-test-infra` (the lane with the pack images; H-1).
- `ui/**` (A4, Stream E): `just ui-lint && just ui-test && just ui-e2e`.
- Job-schema change (A1, A7, D2): `caesium job lint --path docs/examples/` and
  regenerate `docs/job-schema-reference.md` from `internal/jobdef/report`
  (`TestGeneratedSchemaReferenceIsCurrent` guards it).
- `docs/README.md` edits (N-1): `TestDocsREADMEIndexesEveryTopLevelDoc` in
  `just unit-test`.
- Machine-readable CLI output (`--json`, `why`, `cache list`): assert stdout via
  `runCLIStdout`, never `runCLIRaw`.
- Fixtures use canonical pinned image references (`alpine:3.23`, digest-pinned
  `caesiumcloud/*`) or the image-pin guardrail fails.
- This plan's checkbox ticked, the per-stream `## Progress` bullet appended for
  the active wave, and any cross-linked doc refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — `cache.chain` + `ttl: never`** is a runtime feature: the golden
   test proves default `transitive` hashes byte-identically to before; a
   values-mode step ignores predecessor identity churn but re-runs on a
   predecessor output change on both the local and distributed paths;
   `caesium why` (CLI table, `--json`, Console) renders "predecessor hashes
   excluded (chain: values)"; `test/cache_chain_test.go` and
   `test/unit_pipeline_generic_test.go` are green in CI; and
   `docs/job-schema-reference.md` is regenerated with the new keys.
2. **Stream B — pack scaffold, `git-source`, `tf-discover`** ship as
   `caesiumcloud/git-source` and `caesiumcloud/tf-discover` built by `just
   build-pack` from `build/Dockerfile.pack`; `just pack-test` proves fingerprint
   determinism, nested-relative-module inclusion, and fail-closed behaviour on
   dynamic sources and unexpected manifests; `test/infra_discover_test.go`
   (§9 #7, #8, #10) is green in the infra lane.
3. **Stream C — `tf-warm` + `tf-runner`** ship as `caesiumcloud/tf-warm` and
   `caesiumcloud/tf-runner`; `test/infra_deploy_test.go` proves §9 #1–#6, #9,
   #11 (with #2 — edit one stack, re-apply one stack — passing) and the drift
   scenario proves an out-of-band change turns the drift run red; no
   `sensitive = true` value ever reaches a task output row or API response.
4. **Stream D — lint + manifests**: `caesium job lint` (local and server) warns
   on a volume with two read-write mounts, asserted via an integration test;
   `docs/examples/infra-deploy.job.yaml` and `infra-drift.job.yaml` pass
   `examples_test.go`, `caesium job lint --path docs/examples/`, and produce no
   lint warning.
5. **Stream E — Console proposal panel** renders `terraform.plan.v1` summaries
   and falls back to key/value for unknown kinds, with vitest + one Playwright
   scenario green under `just ui-test` / `just ui-e2e` (E2 either shipped or
   explicitly recorded as deferred).
6. **Harness** — CI runs `pack-lint`/`pack-test`, the
   `build-and-integration-test-infra` job, and publishes the four pack images
   as multi-arch manifests alongside `caesiumcloud/caesium`; the podman and
   helm lanes stay green (infra scenarios skip there with an explicit reason).
7. **Docs** — `docs/infrastructure-deployment.md` exists and is indexed in
   `docs/README.md`; the spec's status banner reads Shipped; the Phase 4 roadmap
   row reads Shipped.
8. `docs/roadmap.md`, `docs/exec-plans/active/dynamic-fanout.md`,
   `docs/design-dynamic-fanout.md`, and `docs/design-incremental-execution.md`
   reflect every shipped stream; this plan's per-stream Progress entries match
   merged PRs.

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
   e.g. `Add cache.chain and ttl: never to the cache config (infra-deploy W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Open Questions

Decisions this plan makes that the user may want to confirm before wave 1:

1. **Job-level `metadata.cache.chain`** (spec §11.1): allowed, because
   `applyCache` layers job → step uniformly and blocking it costs code. The
   sharp edge is documented in A7/N-1 and always visible in `why`.
2. **Multi-writer volume check severity** (spec §11.2): a warning (D1), not an
   error.
3. **Console panel placement** (spec §11.3): task-level first (E1), run-level
   aggregate second (E2, P2).
4. **Inter-stack ordering in multi-root discover** (spec §5.5 shows `dependsOn`
   in the partitions but Terraform has no inter-stack dependency declaration):
   B3/B4 read it from a `stacks.yaml` at `SCAN_ROOT`. Only the deferred fan-out
   form consumes it; the hand-written form orders stacks in the job YAML.
5. **The branch-form cascade** (spec §6.4 / §11.5): grounding found that a
   branch-skipped apply skips every consumer (default `all_success`) and emits
   no outputs, so the plan makes the container no-op the default and limits the
   branch form to leaf stacks (C3, C5, N-1). If the spec should instead be
   amended (e.g. a branch step that still publishes cached outputs for skipped
   successors), that is a core change outside this plan.
6. **Pack as a nested Go module** (`pack/go.mod`) rather than root-module
   packages: chosen so Terraform deps never enter the root `go.sum` and the
   images build without the CGO builder. Reverse this before B1 if root-module
   packaging is preferred.
7. **Terraform vs OpenTofu in the images**: `TF_DIST`/`TF_VERSION` build-args
   with Terraform (pinned 1.15.x, the version the spec verified against) as the
   default. Distribution/licensing of the Terraform binary inside published
   images is the user's call; switching the published default to OpenTofu is a
   one-line change in H-2.

## Cross-References

- `docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`
  — the source of truth for this plan.
- `docs/roadmap.md` — Phase 4 design-wave table row added at draft time.
- `docs/exec-plans/active/dynamic-fanout.md` and `docs/design-dynamic-fanout.md`
  — own dynamic fan-out and structured partition objects; defer the chain
  break to Stream A here.
- `docs/design-incremental-execution.md` — the cache design of record; A7 adds
  `chain`.
- `docs/superpowers/specs/2026-05-29-volumes-and-workload-identity-design.md`
  — the volumes / `secret://` / workload-identity substrate this pattern rides
  on.
- `pkg/jobdef/definition.go` — the YAML contract Stream A changes.
- `internal/cache/hash.go` — `HashInput` / `Compute()` / `HashInputBlob`.
- `docs/job-definitions.md`, `docs/caesium-job-llm-reference.md`,
  `docs/job-schema-reference.md` (generated) — operator docs updated by A7 / N-1.
- `docs/infrastructure-deployment.md` — the user guide N-1 adds.
