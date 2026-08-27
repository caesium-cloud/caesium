# DAG-Native Infrastructure Deployment — `cache.chain` + the Terraform pack

Last updated: 2026-08-26

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

- [ ] A1. Add `chain` and `ttl: never` to the resolved cache config. Extend
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
- [ ] A2. Implement the hash semantics with a golden guard. Add `Chain string`
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
- [ ] A3. Wire the resolved config into all three `HashInput` construction sites
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
- [ ] A4. Render the exclusion in `caesium why` (spec §4.3: "or the skip becomes
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
- [ ] A5. Integration scenario for the chain break on a plain pipeline (no
      Terraform, `alpine:3.23` only): job with `upstream` (env derived from a run
      param, constant output) → `mid` (`cache: {version: 1, chain: values}`) →
      `leaf` (default chain). Run twice with a changed param: `upstream` re-runs,
      `mid` is `cached`, `leaf` under transitive re-runs; change `upstream`'s
      output → `mid` re-runs (outputs still chain). `ttl: never` on `mid` →
      `caesium cache list --json` shows a null expiry; `caesium why --json
      <run> mid` contains the exclusion note. All machine output via
      `runCLIStdout`. Files: new `test/cache_chain_test.go`.
      Depends on: A3, A4.
- [ ] A6. The generic unit-pipeline binding (spec §9.12 — the genericity
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
- [ ] A7. Docs + generated schema reference for the new keys. Update the job-
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

- [ ] B2. `git-source` (generic materialize role, spec §6.1). Env: `GIT_URL`,
      `GIT_REF`, `GIT_SPARSE` (space-separated paths), `GIT_SSH_KEY` (already
      resolved from `secret://` by Caesium; written to a 0600 temp file and
      never logged), `DEST` (default `/src`). Sparse shallow clone at the ref,
      then `treeDigest` = sha256 over the sorted output of
      `git ls-tree -r <ref> -- <paths>`; emits `commit`, `treeDigest`, `path`.
      Supports `file://` URLs so the hermetic fixture works with no network.
      Fail closed on every git error. Go tests over a temp repo.
      Files: new `pack/cmd/git-source/main.go` + `_test.go`.
      Depends on: B1.
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

- [ ] B4. `tf-discover` (spec §6.2). Env: `SCAN_ROOT`, `TF_WORKSPACE`. Single-root
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
- [ ] B5. Integration scenarios for materialize + discover through the live
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

- [ ] C1. `tf-warm` (spec §6.3). Read every `.terraform.lock.hcl` under
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
- [ ] C2. `tf-runner tf-plan` (propose). Env: `STACK_ROOT` (or
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
- [ ] C3. `tf-runner tf-apply`. Env: `PLAN_STEP` (locates
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
- [ ] C4. `tf-runner tf-drift` (spec §6.6). `plan -refresh-only
      -detailed-exitcode`: exit 0 → `##caesium::output {"drift":"false"}`; exit 2
      → emit `{"drift":"true", "drift_summary": <sensitive-stripped counts>}`
      then **exit non-zero** so the task and run fail and the shipped
      notification / callback / `metadata.remediation` incident paths fire;
      never writes an artifact. Files: `pack/cmd/tf-runner/main.go`, new
      `pack/internal/tf/drift.go` + tests.
      Depends on: C2.
- [ ] C5. The Terraform end-to-end gate: `test/infra_deploy_test.go` generating
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
- [ ] C6. Drift scenario: the drift job over the fixture (cron trigger fired
      manually; `checkout` + `warm-cache` reused; per-stack `drift-<s>` with
      **no** `cache` block). Clean state → green with `drift=false`; an
      out-of-band state edit (e.g. `terraform state rm` on a fixture resource)
      → run **red** with `drift=true` and a `drift_summary`; the drift step is
      never `cached` across runs.
      Files: `test/infra_deploy_test.go` (or new `test/infra_drift_test.go`).
      Depends on: C4, C5.

### Stream D — Multi-writer volume lint warning + reference manifests

The one lint rule the design calls for (spec §8 "two read-write mounts on one
volume") and the manifests operators copy. `internal/jobdef/lint/` today holds
only the secret checks; local `caesium job lint` prints only "Validated N" and
has no warnings channel, while the server lint response already carries
`Warnings []LintMessage`.

- [ ] D1. Multi-writer volume lint warning. Add
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
- [ ] D2. Reference manifests in `docs/examples/`. `infra-deploy.job.yaml` — the
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

### Stream E — Console proposal panel

Spec §5.6: a propose step emitting the three reserved keys (`proposal_kind`,
`proposal_summary`, `proposal_artifact`) is rendered as a proposal. A
convention over ordinary outputs — no new marker, no schema key, no endpoint.
The Console never fetches the artifact (Caesium is not in the data path); it
renders the summary and shows the reference.

- [ ] E1. `ProposalPanel` with a renderer registry. New
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
- [ ] E2. Run-level proposal summary (Open Question 3 — the "more useful and
      more work" half). A `RunProposalSummary` on `RunDetailPage` aggregating
      counts across every task in the run with a `proposal_kind`, linking each
      row to its task panel. Files: new
      `ui/src/features/jobs/RunProposalSummary.tsx`,
      `ui/src/features/jobs/RunDetailPage.tsx`, tests.
      Depends on: E1.

## Harness Strengthening

- [ ] H-1. `integration-test-infra` lane + CI wiring. Add `integration-up-infra`
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
- [ ] H-2. Publish the pack images. Extend the `publish` job to build and push
      multi-arch manifests for `caesiumcloud/git-source`, `tf-discover`,
      `tf-warm`, `tf-runner` with the same tag scheme as `caesiumcloud/caesium`,
      and pin `TF_VERSION` in one place (the Dockerfile ARG default, surfaced as
      a justfile variable). Files: `.github/workflows/ci.yml`, `justfile`,
      `build/Dockerfile.pack`.
      Depends on: H-1.

#### Deferred (harness)

- **Kubernetes/helm lane coverage of the pack** — needs an RWX-capable storage
  class in the kind cluster (the default local-path class is RWO). Out of scope;
  N-1 documents the RWX requirement and RWO stays unsupported until node
  affinity lands (spec §12).
- **Podman lane coverage** — the podman `volume` source exists, so the infra
  scenarios could later run under `integration-test-podman`; deferred until the
  docker lane is green.

## Navigational / Organizational Improvements

- [ ] N-1. User guide `docs/infrastructure-deployment.md`: the unit-pipeline
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
- [ ] N-2. Close-out. Flip the spec's `**Status:** Proposed` banner to Shipped
      with a link to this plan; update the Phase 4 roadmap row added at draft
      time to **Shipped**; confirm the `cache.chain` cross-links in
      `docs/exec-plans/active/dynamic-fanout.md` and
      `docs/design-dynamic-fanout.md` still describe the shipped behaviour; sync
      this plan's `## Progress` with merged PRs and move it to
      `docs/exec-plans/completed/`.
      Files: `docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`,
      `docs/roadmap.md`, `docs/exec-plans/active/dynamic-fanout.md`,
      `docs/design-dynamic-fanout.md`, this file.
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
