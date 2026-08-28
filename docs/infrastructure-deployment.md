# Infrastructure Deployment

> Status: Shipped — dependency-ordered deployment of infrastructure stacks as
> ordinary Caesium DAGs, with unchanged stacks skipped and a shared provider
> cache warmed once. Design:
> `superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`.
> Implementation: `exec-plans/completed/infra-deploy.md`.

This is a pattern, not a Terraform feature. Caesium's Go never learns what HCL
is — everything Terraform-specific lives in four container images
(`caesiumcloud/git-source`, `tf-discover`, `tf-warm`, `tf-runner`). Terraform is
the first *binding* of a generic **unit-pipeline** shape that also fits dbt,
monorepo CI, and database migrations with different images and no Caesium
changes at all.

Exactly one thing was genuinely new in Caesium's Go to make this work:
`cache.chain: values`, a key on the existing `cache` object that stops a step's
identity hash from chaining its predecessors' identity hashes. See
[Cache chain: the sharp edge](#cache-chain-the-sharp-edge) below.

## The unit-pipeline pattern

A **unit** is the thing that changes independently and is deployed
independently: a Terraform stack, a dbt model, a monorepo package, a database
migration. Five container roles implement any binding of the pattern:

| Role | Responsibility | Runs |
|---|---|---|
| **Materialize** | pin and stage inputs; emit their identity | once |
| **Warm** | populate a shared read-only dependency cache, idempotently | once |
| **Discover** | enumerate units, fingerprint each, declare inter-unit order | once |
| **Propose** | per unit: produce a reviewable artifact + summary + proceed/no-op | per unit |
| **Apply** | per unit: consume exactly that artifact; emit outputs | per unit |

The first three are cheap and run once per job. The last two fan out over the
discovered set — in the Terraform binding shipped here, "fan out" means one
hand-written `discover-<stack>` / `plan-<stack>` / `apply-<stack>` trio per
stack (see [The fan-out form](#the-fan-out-form-forward-looking) for what
changes once a manifest opts into `fanOut:`).

### Role contracts

No SDK, no Go plugin, no Caesium-side knowledge of any tool — just stdout
markers and environment variables. Any image honouring them is a drop-in.

| Role | Reads | Emits on stdout |
|---|---|---|
| Materialize | tool-specific env, `secret://` refs | `##caesium::output {commit, treeDigest, path}` |
| Warm | source path, cache dir | nothing; self-checks a marker in the cache and exits fast |
| Discover | source path, scan root | `##caesium::partitions [{key, fingerprint, dependsOn, …}]` (or, single-root, `##caesium::output {fingerprint, input_*}`) |
| Propose | `CAESIUM_PARTITION`, source, cache | `##caesium::output-ref` for the artifact + proposal outputs |
| Apply | `CAESIUM_PARTITION`, the artifact ref | `##caesium::output {…}` for downstream units |

Two rules make the pattern correct, not merely convenient:

1. **Discover owns the fingerprint.** Caesium never inspects a unit's
   contents. Whatever the discover image says a unit's inputs digest to *is*
   that unit's cache key contribution.
2. **Every role fails closed.** A discover that cannot resolve a dependency
   exits non-zero. An absent fingerprint is never read as "unchanged" — see
   [Failure modes](#failure-modes-spec-8).

### Bindings

| Role | Terraform (shipped) | dbt / ETL | Monorepo CI | DB migrations |
|---|---|---|---|---|
| Materialize | `git-source` | `git-source` | `git-source` | `git-source` |
| Warm | `providers mirror` | `dbt deps` + adapter | language dep cache | — |
| Discover | `get` + `modules.json` | `dbt ls --select state:modified+` | affected-package query (nx / turbo / bazel) | pending migration files |
| Propose | `plan -out` + `show -json` | `dbt compile` / dry-run | build + test → artifact | generate SQL + destructive-op scan |
| Apply | `apply tf.plan` | `dbt run` | publish / deploy | execute SQL |

`git-source` is the one role that is already fully generic (not
Terraform-specific) — every other binding reuses it unmodified. Building a new
binding means writing the remaining four images; Caesium requires no changes.

## The Terraform binding

Four images implement the five roles (`git-source` doubles as materialize for
every binding). All four are Go binaries on `terraform-exec` +
`terraform-json` (no shelling out to `terraform` and scraping text), pinned to
Terraform **1.15.9**. Set the Dockerfile's `TF_DIST=opentofu` build arg (with
matching `TF_VERSION`/checksum args) to ship OpenTofu binaries instead — every
image is written against the CLI surface both tools share; Caesium's Go never
names the binary.

### `git-source` (materialize — generic, not Terraform-specific)

| Env | Meaning |
|---|---|
| `GIT_URL` | repository to clone (`https://`, `ssh://`, `git@host:path`, `file://`) |
| `GIT_REF` | branch, tag, or full commit sha (required) |
| `GIT_SPARSE` | space-separated, repo-root-relative path patterns (e.g. `stacks/** modules/**`); each must contain a `/` and is used both as the sparse-checkout pattern and, with `:(glob)` magic, as the tree-digest pathspec — an unanchored pattern would stage more than it digests, so `GIT_SPARSE` rejects `!` negation, a leading `/`, and unanchored (no-`/`) patterns |
| `GIT_SSH_KEY` | private key, already resolved from a `secret://` URI by Caesium |
| `GIT_SSH_KNOWN_HOSTS` | known_hosts content for the forge — supplying it turns on strict host-key checking; the role never disables checking on its own |
| `DEST` | staging directory (default `/src`) |

Emits `##caesium::output {commit, treeDigest, path}`. `treeDigest` is a sha256
over `git ls-files -s` restricted to the sparse paths, sorted — an exact
content digest without reading a single file byte, and identical for two
commits that did not touch the sparse paths.

**Refuses a non-empty `DEST`.** A persistent volume (a docker/podman named
volume, or a Kubernetes `pvc:`, as opposed to a fresh-per-run source) still
holds the previous run's tree, so a hand-written pipeline needs an explicit
`cache: false` step ahead of checkout that clears the destination — see
`docs/examples/infra-deploy.job.yaml`'s `prepare` step.

### `tf-discover`

| Env | Meaning |
|---|---|
| `SCAN_ROOT` | a stack directory (single-root mode) or a directory of stacks (multi-root/fan-out mode) |
| `TF_WORKSPACE` | workspace name folded into the fingerprint (default `default`) |
| `TF_CLI_PATH` | terraform binary to use (default: `terraform` on `PATH`) |

Runs `terraform get` (module installation *without* provider installation,
which is why discover depends only on checkout, never on warm) and hashes the
union of the resolved module directories plus `*.tf`, `*.tfvars`,
`*.tfvars.json`, `*.tfquery.hcl`, `.terraform.lock.hcl`, and the workspace
name — using the manifest `terraform get` writes
(`.terraform/modules/modules.json`), not `terraform modules -json`, because
the latter's JSON drops the parent path for a nested module call and leaves
its relative `source` unresolvable to a real directory. A module `source` that
is not a literal or const variable is a hard Terraform error at install time,
so a dynamic module source fails discover closed with no defensive heuristic
of Caesium's own.

Single-root mode emits `##caesium::output {fingerprint, input_<name>…}`.
Multi-root mode emits `##caesium::partitions [{key, fingerprint, dependsOn, root}]`
for the fan-out form.

### `tf-warm`

| Env | Meaning |
|---|---|
| `SRC` | source tree to scan for `.terraform.lock.hcl` (default `/src`) |
| `CACHE_DIR` | the cache volume mount (default `/cache`) |
| `CACHE_MOUNT_PATH` | the cache path *consumers* see, if it differs from `CACHE_DIR` |
| `TARGET_PLATFORM` | `os_arch` to mirror for, space/comma separated (default: this container's own platform) |
| `TF_CLI_PATH` | terraform binary to use |

Reads every `.terraform.lock.hcl` under `SRC`, derives a mirror key from the
sorted provider/version/hash/**platform** union, checks `/cache/.warm/<key>`.
If present, exits in about a second. Otherwise mirrors providers into
`/cache/providers.tmp.<key>`, atomically renames to `/cache/providers/<key>`,
writes `/cache/terraformrc` (a `provider_installation { filesystem_mirror {…}
direct {exclude = ["*"]} }` block), and drops the marker. Emits no markers at
all — a role whose whole purpose is to be invisible to consumers must not make
their cache keys depend on it.

**Never given a `cache` block, ever — `cache: false` explicitly, not merely an
omitted block.** A Caesium cache hit means no container ran; if the cache
volume had been recreated (a fresh PVC, a wiped named volume) it would be
empty and every downstream `init` would fail with no warning. Always running
and self-checking the marker *inside the volume* is what makes a recreated
volume self-healing, and it costs one container start per run. On a server
with `CAESIUM_CACHE_ENABLED=true` (the shipped default for this pattern's
integration lane), an *omitted* `cache` block is cacheable, not "no cache" —
so both example manifests set `cache: false` on `warm-cache` explicitly, not
just on every drift step (see [The drift job](#the-drift-job-mandatory)).

**Known limitation: one `tfcache` volume serves one provider set.** `tf-warm`
writes a single `/cache/terraformrc` naming one `filesystem_mirror` directory,
so two jobs (or two stacks) whose `.terraform.lock.hcl` files resolve to
different provider/version unions must not share the same `tfcache` volume —
the second job's `init` would either miss the mirror for a provider the first
job never needed, or silently reuse a stale mirror. Give each independent
provider set its own `tfcache` volume (physical name), even if you reuse the
same `tfstate` volume convention across jobs.

**First warm needs outbound network; every later run is offline.**
`terraform providers mirror` needs to reach `registry.terraform.io` (or your
configured provider registry) the first time a given provider set is warmed.
`terraformrc`'s `direct { exclude = ["*"] }` block means every subsequent
`init` fails closed against the registry rather than silently reaching it, so
a provider missing from the mirror is a loud error, not a quiet egress call.
For an air-gapped runner, seed the mirror out of band at the `/cache/.warm/<key>`
marker seam before the first real run. The CI infra integration lane
(`integration-test-infra`) relies on the runner having outbound access for
this first warm — the rest of the fixture is otherwise fully hermetic
(`--network none`-verified for `pack-test`).

### `tf-runner` (`tf-plan`, `tf-apply`, `tf-drift`)

One binary, three subcommands selected by the manifest's `command:`.

Shared env:

| Env | Meaning |
|---|---|
| `STACK_ROOT` | the root module to operate on. When absent, taken from `CAESIUM_PARTITION_JSON`'s `root` attribute joined onto `SCAN_ROOT` — the fan-out form, where one step definition serves every stack |
| `SCAN_ROOT` | base for the partition's relative root (fan-out form only) |
| `TF_WORKSPACE` | workspace to select (default `default`) |
| `TF_CLI_CONFIG_FILE` | the warm step's generated `terraformrc`; point every plan/apply/drift step at it |
| `TF_DATA_DIR` | Terraform's working directory (default `<ARTIFACT_DIR>/tfdata`) |
| `ARTIFACT_DIR` | where the plan artifact is written (default `<root>/.caesium`) |
| `BACKEND_CONFIG` | comma-separated `-backend-config` key=value settings — how a pipeline keeps Terraform state on a volume that survives the source tree being re-materialized on every run |
| `TF_CLI_PATH` | terraform binary (default: `terraform` on `PATH`) |

`tf-plan` (propose) additionally reads:

| Env | Meaning |
|---|---|
| `IMPORT_OUTPUTS_FROM` | comma-separated upstream **apply** step names. Every `CAESIUM_OUTPUT_<STEP>_<KEY>` of those steps is exported as `TF_VAR_<key>` — the cross-stack wiring, deliberately not `terraform_remote_state` (which would grant every consuming stack read credentials on the producing stack's state) |
| `APPLY_STEP` | when set, emit `##caesium::branch <APPLY_STEP>` only if the plan has changes — the leaf-stack branch form |

`tf-apply` additionally reads:

| Env | Meaning |
|---|---|
| `PLAN_STEP` | the plan step whose proposal to apply; its `CAESIUM_OUTPUT_<PLAN>_PROPOSAL_*` values locate the summary and the artifact |

Exit codes (`tf-plan`, mirroring `plan -detailed-exitcode`):

| Exit | Meaning | Emits |
|---|---|---|
| 0 | no changes | `proposal_summary` with zero counts, no artifact |
| 2 (mapped to success) | changes | `output-ref` for the plan file, `proposal_*` outputs from `show -json` |
| 1 | error | task fails, no markers |

`tf-drift` reuses `plan -refresh-only -detailed-exitcode`: exit 0 emits
`{"drift":"false"}`; a refresh diff (exit 2) emits `{"drift":"true",
"drift_summary": <counts>}` and then the task **fails** (non-zero exit after
emitting) so the run goes red and the shipped notification / callback /
`metadata.remediation` paths fire. Note: `-refresh-only -detailed-exitcode`
returns 2 for an **output** change as readily as for resource drift — count
output changes in `drift_summary`, or a real drift event reads as an all-zero,
false-alarm-looking summary.

**Every phase fails closed**: an error exits non-zero having emitted no
marker at all. An absent fingerprint or proposal is never read as "unchanged."

**The plan artifact's digest changes on every re-plan, even for an identical
diff** — Terraform embeds a timestamp in the plan file, so the bytes really
are different. This is why `apply-<stack>` re-runs whenever `plan-<stack>`
re-runs (their `chain: values` cache keys are independent, but the apply
step's declared inputs include the plan artifact's digest); it is expected,
not a bug to chase.

### The two sensitive-value rules

Step outputs land in dqlite and flow onward as environment variables, so
`tf-runner` enforces both of these as **typed field access** on
`terraform-json`'s structures, not `jq` over a hand-tracked schema:

1. **`tf-apply` drops every output whose `OutputMeta.Sensitive` is set** before
   emitting `##caesium::output`. A stack with no publishable (non-sensitive)
   outputs emits nothing at all — the emitter rejects an empty output map, and
   nothing downstream can be reading a stack that publishes zero values.
2. **`tf-plan`/`tf-drift` strip `sensitive_values` from the plan JSON before
   summarizing or rendering it.** The summary is rebuilt field-by-field from
   structural facts (address, type/name/module, provider, action set) rather
   than by blanking known-sensitive fields on the original object — a field a
   future `terraform-json` release adds is dropped by default, not leaked
   until someone notices.

A third, easy-to-miss leak this closes: `terraform-exec`'s JSON-returning
calls (`ShowPlanFile`, `Output`) tee the **raw, unstripped** response to
whatever writer the caller passes alongside the typed decode. `tf-runner`
discards that writer's output for the duration of those two calls specifically
so the unsanitized plan JSON and `terraform output -json` (which prints
sensitive values in full — the CLI only masks them in human-rendered output)
never reach the task log.

## Both forms: container no-op vs. the branch form

Two grounding facts, not spelled out by the design spec, decide which form a
stack uses:

1. Caesium publishes images as `caesiumcloud/<name>` (the shipped CI `publish`
   job and the `triage-agent` precedent) — the spec's `ghcr.io/caesium/*`
   references are illustrative.
2. **A branch-skipped task cascades.** `propagateSkipped`
   (`internal/job/job.go`) skips every successor whose trigger rule is the
   default `all_success`, and a skipped task has no outputs. If an empty-plan
   `apply` were skipped via a branch marker, every downstream stack consuming
   its outputs (via `IMPORT_OUTPUTS_FROM`) would be stranded with nothing to
   read.

So:

- **Container no-op is the default** for any stack with consumers. `apply`
  always runs `terraform output -json` and always emits outputs; it only
  *applies* Terraform (invokes `apply tf.plan`) when the proposal has
  non-zero counts. The run stays green and every consumer keeps seeing fresh
  (even if unchanged) values.
- **The branch form is reserved for leaf stacks** — a stack nothing downstream
  reads from. `plan` becomes `type: branch` and emits `##caesium::branch
  <APPLY_STEP>` only when the plan has changes, so an empty plan skips `apply`
  entirely. This is the more honest rendering for a leaf: the run visibly
  shows `apply` as `skipped` rather than as a container that ran and did
  nothing.

`docs/examples/infra-deploy.job.yaml` shows both: `network` and `account` use
the container no-op form (an app stack imports network's outputs); `app-web`,
the only leaf, uses the branch form.

## The fan-out form (forward-looking, mechanically valid now)

The reference manifest's *hand-written* three-step-per-stack shape does not
scale past a handful of stacks — forty stacks means forty copy-pasted trios.
[Dynamic fan-out](exec-plans/active/dynamic-fanout.md) is the vehicle that
turns any number of units into five steps: a producer emits
`##caesium::partitions`, a consumer declares `fanOut:`, and one `Task` expands
to N `TaskRun` rows.

**Correction to the design spec's framing**: dynamic fan-out, including the
structured-partition amendment this pattern needs (`{key, fingerprint,
dependsOn}` objects, not just bare strings), **shipped** in
[#349](exec-plans/active/dynamic-fanout.md) — it is not the "designed,
unstarted" vehicle the infra-deploy design spec describes. The `fanOut` field,
`##caesium::partitions` object form, and per-partition fingerprint folding
into the cache identity hash are all real today. The snippet below is
therefore syntactically valid against the shipped schema.

It is **not** promoted to a `docs/examples/` file in this wave, for a narrower
reason than "not yet shipped": `docs/examples/` manifests are lint-validated
end to end (`internal/jobdef/examples_test.go`, `caesium job lint`), and doing
that properly for the fan-out form means a second, dedicated integration
scenario proving the per-partition fingerprint genuinely gates the cache the
way the hand-written form's `chain: values` does — that scenario was out of
scope for this wave's item (D2 scoped the shipped examples to the hand-written
three-stack form from the Terraform end-to-end gate). Treat this as illustrative
until such a scenario lands:

```yaml
steps:
  - name: discover
    image: caesiumcloud/tf-discover:latest
    dependsOn: [checkout]
    env: { SCAN_ROOT: /src/stacks }
    # multi-root mode: emits
    # ##caesium::partitions [{key, fingerprint, dependsOn, root}, …]

  - name: plan
    image: caesiumcloud/tf-runner:latest
    command: ["tf-plan"]
    cache: { version: 1, chain: values }
    fanOut: { from: discover, maxPartitions: 64, maxParallel: 10, onEmpty: skip }
    env: { SCAN_ROOT: /src/stacks, TF_CLI_CONFIG_FILE: /cache/terraformrc }
    volumeMounts:
      - {volume: src, path: /src, readOnly: true}
      - {volume: tfstate, path: /state}
      - {volume: tfcache, path: /cache, readOnly: true}

  - name: apply
    image: caesiumcloud/tf-runner:latest
    command: ["tf-apply"]
    cache: { version: 1, chain: values, ttl: never }
    fanOut: { from: discover, maxPartitions: 64, maxParallel: 5, failurePolicy: fail_fast }
    env: { SCAN_ROOT: /src/stacks, PLAN_STEP: plan, TF_CLI_CONFIG_FILE: /cache/terraformrc }
    volumeMounts:
      - {volume: src, path: /src, readOnly: true}
      - {volume: tfstate, path: /state}
      - {volume: tfcache, path: /cache, readOnly: true}
    datasets:
      produces:
        - name: "stack:prod/${CAESIUM_PARTITION}"
```

`maxPartitions` is a required field on `fanOut:` (the shipped schema has no
default) — the design spec's own snippets omit it; do not copy them literally.
In the fan-out form there is no per-unit DAG edge to suppress an empty apply,
so the branch form does not apply here at all: `apply`'s container itself
no-ops when its unit's proposal has zero counts, exactly like the container
no-op form above, for every partition, always. See
`docs/exec-plans/active/dynamic-fanout.md` for the full fan-out semantics
(`maxParallel`, `onEmpty`, `failurePolicy`, in-group `dependsOn` ordering,
retry-by-partition).

## The drift job (mandatory)

The fingerprint gate answers *"did my code change?"*; it is blind to drift by
construction — a resource edited by hand in a cloud console persists until
something else catches it. That is a deliberate cost/correctness trade-off
(§3.2), and the consequence is that **the drift job is part of the feature,
not an optional extra.**

`docs/examples/infra-drift.job.yaml` is a separate, cron-triggered job reusing
the deploy job's checkout/warm-cache shape, running `tf-drift` per stack
against the *same* `tfstate` (and ideally `tfcache`) physical volumes the
deploy job manages — otherwise it is checking drift against the wrong (or
empty) state. Drift steps carry **`cache: false` explicitly**, not merely an
omitted `cache` block: with `CAESIUM_CACHE_ENABLED=true` an omitted block is
cacheable, and a cached drift step would replay `drift: false` forever —
caching the one thing whose entire purpose is to detect out-of-band change
defeats it.

**A stack that consumes another stack's outputs needs those variables pinned
in the drift step's own env.** `tf-drift` honours `IMPORT_OUTPUTS_FROM` the
same way `tf-plan` does — but only when an `apply` step precedes it in the
*same job's* DAG, so it can read `CAESIUM_OUTPUT_<STEP>_<KEY>`. The drift
job's shape (§6.6) has no apply step at all — it only ever runs
`plan -refresh-only` — so `IMPORT_OUTPUTS_FROM` has nothing to import from
here. `docs/examples/infra-drift.job.yaml`'s `drift-app-web` step therefore
pins `TF_VAR_vpc_id` directly rather than importing it, with a comment
flagging that operators must keep the pinned value in sync with the deploy
job's actual `apply-network` output — app-web's `vpc_id` variable has no
default (by design, in the fixture stack), so a stale or missing pin fails
the drift step outright rather than silently comparing against the wrong (or
no) VPC.

**The weekly full-apply backstop.** The fingerprint gate is itself a risk: a
systematically wrong or under-reported fingerprint (see
[Failure modes](#failure-modes-spec-8)) hides silently for as long as nobody
edits the affected files. The recommended mitigation is a second, less
frequent schedule that force-applies every stack with caching disabled for
that run — the escape hatch is `caesium cache invalidate`:

```sh
caesium cache invalidate --job-id <job-id>
```

Note the real flag is `--job-id` (the job's UUID, from `caesium job list` or
`GET /v1/jobs`), **not** `--job <alias>` as an illustrative `caesium cache
invalidate --job infra-prod` might suggest — `cmd/cache/invalidate.go` takes
no alias-resolving flag. Add `--task <name>` to invalidate one step's cache
instead of the whole job.

## RWX requirement and RWO deferral

The shared provider mirror (`tfcache`) is a warm-once, read-many volume: one
`warm-cache` step holds the only read-write mount, and every other step mounts
it `readOnly: true`. On Kubernetes this needs `ReadWriteMany` storage — a
`filesystem_mirror` shared by parallel `init` calls across pods on different
nodes cannot live on `ReadWriteOnce` storage. `ReadWriteOnce` support is
**deferred** until a node-affinity / co-location primitive lands (design §12),
which would let every step in a run schedule onto the same node as the volume.
Until then, provision `tfcache` (and, for the propose/apply state-sharing
pattern below, `tfstate`) from an RWX-capable storage class.

`src` (the staged source tree) also needs to be shared across every step in
the run — nine steps in the reference manifest (materialize through every
plan/apply). **Use a pre-provisioned `pvc:`, not `claimTemplate`, for a volume
mounted by more than one step.** `claimTemplate` provisions an inline
*ephemeral* PVC scoped to one pod/step (see "Volumes And Workload Identity" in
[`job-definitions.md`](job-definitions.md)); the design spec's own §5.5
reference manifest uses `claimTemplate` for `src` and that is wrong for this
pattern's purposes — a grounding correction recorded on this plan's D2 item.

## Cache chain: the sharp edge

`cache.chain: values` (spec §4) is the one piece of new Caesium Go this
pattern needed. Full schema, hash semantics, and `caesium why` rendering are
in [`job-definitions.md`](job-definitions.md#cache-chain) and
[`caesium-job-llm-reference.md`](caesium-job-llm-reference.md#cache-chain-breaking-a-noisy-upstream).
The short version: a checkout step's identity hash must contain the git ref,
which changes on every commit, and that change propagates through
`PredecessorHashes` to every stack sharing the checkout — without `chain:
values`, editing one line in `stacks/app-web` would invalidate and re-apply
every stack in the repo. `chain: values` excludes predecessor *identity*
hashes from a step's key while still hashing predecessor **outputs**, so a
network stack's internal refactor leaves consumers cached, but a changed
`vpc_id` output still busts every consumer's plan.

**The sharp edge**: an upstream change that alters real behaviour without
altering its declared outputs leaves consumers cached. This is a real,
accepted trade-off, not a bug — `caesium why` always renders `predecessor
hashes excluded (chain: values)` on both a hit and a miss, so a skip is never
silently unexplainable, and the reference manifests put `chain: values` on
every `plan`/`apply` step so the sharp edge is visible in one place.

**Relationship to the value-verified short-circuit.** Caesium separately
short-circuits a cascade when a re-executed step is *proven* to publish
byte-identical output (`EquivalentPriorHash`). Neither mechanism subsumes the
other: the short-circuit acts *after* execution and only on proof, so it does
nothing for a step that publishes nothing at all (`warm-cache`'s own contract
— see [`design-incremental-execution.md`](design-incremental-execution.md#chain-mode-cachechain))
or for a consumer that only cares about part of what its predecessor
publishes; `chain: values` decides *before* execution and covers exactly
those two cases, at zero cost when the upstream would have re-run anyway.

## The proposal convention

A `propose` step emitting three reserved output keys is rendered as a
proposal by the Console — a documented convention over ordinary outputs, not
a new marker or schema key:

| Key | Meaning |
|---|---|
| `proposal_kind` | renderer selector, e.g. `terraform.plan.v1` |
| `proposal_summary` | small JSON — **encoded as a string value**, not a nested object (Caesium's output marker only stores scalar JSON types per key; a raw nested object is silently dropped) — that a generic renderer can display without knowing the kind |
| `proposal_artifact` | names the `##caesium::output-ref` key holding the reviewable artifact |

The Console keeps a renderer registry keyed on `proposal_kind`, with a generic
key/value fallback so an unregistered kind still renders usefully. A run-level
`RunProposalSummary` aggregates counts across every task in a run carrying a
`proposal_kind`, linking each row to its task panel — this is the run-wide
view Open Question 3 in the design spec asked for.

The Console **never fetches the artifact itself** — Caesium is not in the data
path. It renders the summary and the artifact's digest/size/path, with no
download action.

## Failure modes (spec §8)

Ordered by damage:

| Failure | Defense |
|---|---|
| **A green run that deployed nothing** (the cardinal failure) — discover under-reports its inputs, or discover fails and is read as "unchanged" | A discover failure is a *task* failure, so downstream never runs (status propagation, not hashing — fails closed). A missing `fingerprint` output is a contract violation under `schemaValidation: fail`. `caesium cache invalidate --job-id <id>` is the force-apply escape hatch. The weekly full-apply backstop catches a systematically wrong fingerprint before it hides for a month. |
| **Drift invisible to deploy runs** | Accepted by design (§3.2); mitigated only by the mandatory drift job. |
| **Fingerprint nondeterminism** — absolute paths, mtimes, unsorted traversal, or locale differences split the cache and re-apply everything | `tf-discover`'s determinism requirement: relative paths only, sorted traversal, no mtimes; regression-tested (`TestInfraDiscoverFingerprintIsDeterministic`). |
| **Sensitive values in dqlite** | The two `tf-runner` requirements above — typed field access, not `jq`. |
| **`chain: values` surprise** | Documented here and surfaced by `caesium why`. |
| **Warm volume recreated** | `tf-warm` always runs and self-checks its marker — self-healing by construction. |
| **RWX unavailable** | Runtime mount failure with a clear message; RWO remains unsupported until node affinity lands. |
| **Two read-write mounts on one volume** | `internal/jobdef/lint`'s multi-writer warning (design §8). It is a *warning*, not an error — a legitimate two-writer case (two steps that genuinely cooperate through a `dependsOn` edge, like this pattern's own `plan`/`apply` state sharing) is real and should not be blocked outright. |
| **Concurrent applies of one stack** | Terraform's backend state lock errors out; `metadata.concurrency: {maxRuns: 1, strategy: queue}` prevents the race at the job level before it reaches Terraform. |

## See also

- `docs/examples/infra-deploy.job.yaml`, `docs/examples/infra-drift.job.yaml` —
  the runnable reference manifests this guide documents.
- [`job-definitions.md`](job-definitions.md#cache-chain) — the full
  `cache.chain`/`cache.ttl` schema and volumes reference.
- [`caesium-job-llm-reference.md`](caesium-job-llm-reference.md#cache-chain-breaking-a-noisy-upstream) —
  LLM-oriented authoring reference, including the cache-chain YAML shape.
- [`design-incremental-execution.md`](design-incremental-execution.md#chain-mode-cachechain) —
  cache-chain implementation notes and its relationship to the value-verified
  short-circuit.
- [`exec-plans/active/dynamic-fanout.md`](exec-plans/active/dynamic-fanout.md) —
  the shipped fan-out mechanism referenced above.
- `superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md` —
  the full design this guide implements.
