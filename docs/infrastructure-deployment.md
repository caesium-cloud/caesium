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

| Role | Responsibility | Runs | Caesium cache |
|---|---|---|---|
| **Materialize** | pin and stage inputs; emit their identity | once | `cache: false` |
| **Warm** | populate a shared read-only dependency cache, idempotently | once | `cache: false` |
| **Discover** | enumerate units, fingerprint each, declare inter-unit order | once | `cache: false` |
| **Propose** | per unit: produce a reviewable artifact + summary + proceed/no-op | per unit | cached, `chain: values` |
| **Apply** | per unit: consume exactly that artifact; emit outputs | per unit | cached, `chain: values` |

**The first three roles are never Caesium-cached, and `cache: false` must be
written out.** An *omitted* `cache` block is CACHEABLE whenever the server has
`CAESIUM_CACHE_ENABLED=true` — the default this pattern's own integration lane
runs under — so "no cache block" does not mean "no caching". Each of the three
has its own reason: materialize and warm must actually look inside a volume
that may have been recreated since the last run, and discover must actually
re-resolve remote module identities, which can move while the checkout that
references them does not. Only propose and apply are cached, and the
fingerprint discover emits is what makes that safe.

The first three are cheap and run once per job. The last two fan out over the
discovered set — in the Terraform binding shipped here, "fan out" means one
hand-written `discover-<stack>` / `plan-<stack>` / `apply-<stack>` trio per
stack — the `fanOut:` form is [not yet supported](#the-fan-out-form-not-yet-supported).

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
| `GIT_REF` | branch, tag, or full commit sha (required) — a **literal** value; see the interpolation note below |
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

**Known limitation: env values are literal — there is no `${…}`
interpolation.** Caesium passes a step's `env:` values through unchanged. Run
parameters arrive as their own `CAESIUM_PARAM_<KEY>` variables inside the
container, and the reagent roles are Go binaries with no shell, so a manifest
writing `GIT_REF: "${CAESIUM_PARAM_SHA}"` — as the design spec's illustrative
§5.5 snippet does — hands `git-source` that literal string and the checkout
fails. The reference manifest therefore pins a literal `GIT_REF: "main"`, and
deploying a specific commit means editing and re-applying the manifest. **A
`GIT_REF`-from-run-param mechanism is a follow-up**, and it is the one piece of
this pattern that a per-commit deploy trigger genuinely wants.

### `tf-discover`

| Env | Meaning |
|---|---|
| `SCAN_ROOT` | a stack directory (single-root mode) or a directory of stacks (multi-root/fan-out mode) |
| `TF_WORKSPACE` | workspace name folded into the fingerprint (default `default`) |
| `ARTIFACT_DIR` | optional; only needed when the plan/apply steps have relocated `ARTIFACT_DIR` *inside* the source tree. Give discover the same value, or the plan artifacts and apply receipts accumulating there become inputs to the very stack that produced them. The default (`<root>/.caesium`) is excluded by name |
| `TF_CLI_PATH` | terraform binary to use (default: `terraform` on `PATH`) |

Runs `terraform get` (module installation *without* provider installation,
which is why discover depends only on checkout, never on warm) and hashes the
stack root and every resolved module directory **recursively**, plus the
workspace name — using the manifest `terraform get` writes
(`.terraform/modules/modules.json`), not `terraform modules -json`, because
the latter's JSON drops the parent path for a nested module call and leaves
its relative `source` unresolvable to a real directory. A module `source` that
is not a literal or const variable is a hard Terraform error at install time,
so a dynamic module source fails discover closed with no defensive heuristic
of Caesium's own.

**The fingerprint covers every regular file a module owns, not just its
`*.tf`.** A `templatefile("${path.module}/templates/userdata.tftpl", …)`, a
`file()` / `fileset()` asset, an `archive_file` `source_dir`, a policy JSON, a
cloud-init script — anything at any depth — changes deployable behaviour, so
all of it is hashed; symlinks are recorded by target. Only generated data that
moves on its own is excluded: `.terraform/`, `.caesium/`, `.git/`, `*.tfstate`,
`*.tfstate.backup`, `.terraform.tfstate.lock.info`, and a relocated
`ARTIFACT_DIR` (hence the env row above). Two consequences worth planning for:
an edit to a non-`.tf` asset now correctly re-runs its stack, and any large
unrelated file parked inside a module directory now enters that stack's cache
key.

Single-root mode emits `##caesium::output {fingerprint, input_<name>…}`.
Multi-root mode emits `##caesium::partitions [{key, fingerprint, dependsOn, root}]`
for the fan-out form. Two contract rules there are enforced by the emitter, not
merely documented: **a partition's `fingerprint` may not be empty** (an empty
one would make a cacheable instance with no unit-content identity), and a
partition's `root` must resolve **strictly inside `SCAN_ROOT`** — an embedded
`..` (`stack/../../state`) or an in-tree symlink that escapes is refused, so a
malformed discover payload cannot redirect plan/apply onto a different mounted
tree. A symlinked `SCAN_ROOT` itself is fine.

**Never given a `cache` block, ever — `cache: false` explicitly, not merely an
omitted block.** Discover's whole job is to re-resolve the unit's inputs, and
`terraform get` is the only step that observes a *remote* module's current
identity. A cached discover never runs it: if a registry version range
resolves higher, a Git branch advances, or a tag is re-pointed while this
checkout is byte-identical, the stale fingerprint is replayed, so every plan
and apply keyed on it stays cached too — a green run that deployed nothing,
this pattern's cardinal failure. The step is cheap (a `terraform get` plus a
directory hash), so the cost of always running it is a container start.

Plan and apply re-resolve modules on every run (see [module
re-resolution](#modules-are-re-resolved-on-every-init) below), so they can no
longer install a *different* revision than the one discover fingerprinted. That
makes `cache: false` here the load-bearing half: it is what stops discover from
asserting an identity it never re-checked. Pinning remote modules to immutable
revisions (an exact registry `version`, a commit SHA in a `git::` ref) is still
the recommendation — it is faster, and it makes a fingerprint mean something
stable — but with the two fixes above a mutable ref is now *correct* rather
than silently stale.

### `tf-warm`

| Env | Meaning |
|---|---|
| `SRC` | source tree to scan for `.terraform.lock.hcl` (default `/src`) |
| `CACHE_DIR` | the cache volume mount (default `/cache`) |
| `CACHE_MOUNT_PATH` | the cache path *consumers* see, if it differs from `CACHE_DIR`. Set the runners' `CACHE_DIR` to this consumer-side mount path |
| `CACHE_KEY` | terraformrc slot on the cache volume. Unset keeps the historical `/cache/terraformrc`. Set (and matched on every consuming `tf-runner` step) writes `/cache/<key>/terraformrc` so two provider sets can share one volume without clobbering the CLI config. A single lower-case path element: letters, digits, `.`, `_`, `-`, starting with alphanumeric, at most 63 characters; not `providers` or `terraformrc`. Surrounding whitespace and case variants are rejected, not normalized, so two manifest values cannot alias one slot on a case-insensitive bind mount |
| `TARGET_PLATFORM` | `os_arch` to mirror for, space/comma separated (default: this container's own platform) |
| `TF_CLI_PATH` | terraform binary to use |

Reads every `.terraform.lock.hcl` under `SRC`, derives a mirror key from the
sorted provider/version/hash/**platform** union, checks `/cache/.warm/<key>`,
and verifies every provider/platform artifact promised by that union still
exists in the corresponding mirror. If both are complete, exits in about a
second. A stale marker over a missing or partially deleted mirror repairs it.
Otherwise mirrors providers into
`/cache/providers.tmp.<key>`, atomically renames to `/cache/providers/<key>`,
writes the CLI config (a `provider_installation { filesystem_mirror {…}
direct {exclude = ["*"]} }` block) at `/cache/terraformrc` or
`/cache/<CACHE_KEY>/terraformrc`, and drops the marker. Emits no markers at
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

**Sharing a `tfcache` volume across provider sets.** The provider *packages*
are already content-addressed under `/cache/providers/<mirror-key>/`. The CLI
config that *points* at one used to be a single file, so two jobs whose
`.terraform.lock.hcl` files resolve to different unions would flip
`/cache/terraformrc` between mirrors. Set `CACHE_KEY` to a distinct slot on
each job's `warm-cache` and every `tf-plan` / `tf-apply` / `tf-drift` step
that consumes it; `tf-warm` then writes `/cache/<key>/terraformrc` and
`tf-runner` exports that path as `TF_CLI_CONFIG_FILE` (omit the latter, or
point it at the same file — a mismatch fails closed). The runner also refuses
a missing config or a symlinked slot/config before invoking Terraform. Unset `CACHE_KEY`
keeps the historical `/cache/terraformrc` so existing manifests keep
working. Jobs that share a slot still last-writer-wins on that file; the
key is what keeps them from clobbering each other. Use a distinct, stable key
for every independently evolving lock-file union.

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
(`--network none`-verified for `reagents-test`).

### `tf-runner` (`tf-plan`, `tf-apply`, `tf-drift`)

One binary, three subcommands selected by the manifest's `command:`.

Shared env:

| Env | Meaning |
|---|---|
| `STACK_ROOT` | the root module to operate on. When absent, taken from `CAESIUM_PARTITION_JSON`'s `root` attribute joined onto `SCAN_ROOT` — the fan-out form, where one step definition serves every stack |
| `SCAN_ROOT` | base for the partition's relative root (fan-out form only) |
| `TF_WORKSPACE` | workspace to select (default `default`) |
| `CACHE_DIR` | this runner's cache volume mount (default `/cache`); used with `CACHE_KEY` to derive `TF_CLI_CONFIG_FILE`. If tf-warm used a different `CACHE_DIR` plus `CACHE_MOUNT_PATH`, this must equal the consumer-side `CACHE_MOUNT_PATH` |
| `CACHE_KEY` | lower-case terraformrc slot matching the warm step. Unset leaves `TF_CLI_CONFIG_FILE` to the manifest. Set exports `<CACHE_DIR>/<key>/terraformrc` unless `TF_CLI_CONFIG_FILE` is already that path |
| `TF_CLI_CONFIG_FILE` | the warm step's generated `terraformrc`; point every plan/apply/drift step at it. Optional when `CACHE_KEY` is set |
| `TF_DATA_DIR` | Terraform's working directory (default `<ARTIFACT_DIR>/tfdata`) |
| `ARTIFACT_DIR` | where the plan artifact and the apply receipt are written (default `<root>/.caesium`). **Point it at the state volume.** The [apply receipt](#a-successful-apply-is-recoverable) is what makes a failed post-apply step retryable, and a receipt on an ephemeral mount silently loses that property |
| `BACKEND_CONFIG` | comma-separated `-backend-config` key=value settings — how a pipeline keeps Terraform state on a volume that survives the source tree being re-materialized on every run |
| `TF_CLI_PATH` | terraform binary (default: `terraform` on `PATH`) |

`tf-plan` (propose) additionally reads:

| Env | Meaning |
|---|---|
| `IMPORT_OUTPUTS_FROM` | comma-separated upstream **apply** step names. Every `CAESIUM_OUTPUT_<STEP>_<KEY>` of those steps is exported as `TF_VAR_<key>` — the cross-stack wiring, deliberately not `terraform_remote_state` (which would grant every consuming stack read credentials on the producing stack's state). **Every named step must actually be an upstream `tf-apply` that published its `caesium_outputs_published` sentinel**; a typo, a non-predecessor or a skipped producer fails the phase naming the missing step, rather than importing zero variables and letting Terraform plan against variable defaults |
| `APPLY_STEP` | when set, emit `##caesium::branch <APPLY_STEP>` only if the plan has changes — the leaf-stack branch form |

`tf-apply` additionally reads:

| Env | Meaning |
|---|---|
| `PLAN_STEP` | the plan step whose proposal to apply; its `CAESIUM_OUTPUT_<PLAN>_PROPOSAL_*` values locate the summary and the artifact. `PROPOSAL_KIND` must be **exactly** `terraform.plan.v1` — an absent or empty kind is rejected just as a foreign one is, so a proposal that was never identified cannot be applied even with schema validation off |

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

**Every phase's `terraform init` runs `-lockfile=readonly`**, so a
`.terraform.lock.hcl` that no longer matches the stack's provider requirements
fails with Terraform's own diagnostic rather than being silently rewritten (or
failing with a permission error) inside the read-only `src` mount — see
[`src` is mounted read-only](#src-is-mounted-read-only-for-discover-plan-and-apply).

**The plan artifact's digest changes on every re-plan, even for an identical
diff** — Terraform embeds a timestamp in the plan file, so the bytes really
are different. This is why `apply-<stack>` re-runs whenever `plan-<stack>`
re-runs (their `chain: values` cache keys are independent, but the apply
step's declared inputs include the plan artifact's digest); it is expected,
not a bug to chase.

#### Published output names must survive the environment transport

Caesium exports a step's outputs as `CAESIUM_OUTPUT_<STEP>_<KEY>` — uppercased,
with `-` and `.` folded to `_` — and `tf-plan` lowercases the suffix again to
build `TF_VAR_<key>`. That trip is lossy, so `tf-apply` **fails closed** on any
published output name that would not come back intact:

- **Allowed**: `[a-z0-9_]+` — lowercase letters, digits, underscores.
- **Rejected**: `vpcId` (a consumer would read `TF_VAR_vpcid`, and Terraform
  would quietly fall back to `vpcId`'s default), `VPC_ID`, anything non-ASCII,
  and any *pair* that folds together — `vpc-id` and `vpc_id` in one stack both
  become `TF_VAR_vpc_id`, and which one wins depends on map order.
- **Sensitive outputs are exempt.** They are never published, so they never
  make the trip; `sensitive = true` output names are unconstrained.

This matters most for a stack consumed through `IMPORT_OUTPUTS_FROM`. The
failure it removes is the quiet one: a green apply feeding a consumer that
planned against a variable default.

#### A successful apply is recoverable

`terraform apply` mutates real infrastructure, and everything after it —
reading outputs, filtering sensitive ones, flushing markers — can still fail.
Without a durable record of the apply itself, a retry would be handed the same
cached plan file and Terraform would reject it as stale against the state it
just advanced, wedging the DAG until someone invalidated the cache by hand.

So `tf-apply` writes an **apply receipt**
(`<ARTIFACT_DIR>/applied.<plan-digest>`) immediately after a successful apply.
A retry that finds a receipt matching the proposal it was handed skips the
apply entirely and only re-reads and republishes the outputs. Receipts are
pruned to one per stack. This is why `ARTIFACT_DIR` belongs on the state
volume, as both reference manifests place it.

#### Modules are re-resolved on every `init`

`tf-runner` clears `<TF_DATA_DIR>/modules` before each `terraform init`, so
plan and apply always resolve module sources to what
[`tf-discover`](#tf-discover) fingerprinted. Without that, a persistent
`TF_DATA_DIR` on the state volume would keep an already-installed module for an
unchanged source address — so after a branch advanced or a registry range moved,
discover could fingerprint the new revision while plan silently reused the old
one and then cached the new fingerprint against it: a green run for code that
was never planned.

Providers are deliberately untouched by this — they stay pinned by
`.terraform.lock.hcl` and the warm mirror. `-upgrade` is **not** used, because
it would re-select providers and become a lock-file rewrite that
`-lockfile=readonly` refuses.

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

## Pinning the reagent images to a digest

`docs/examples/infra-deploy.job.yaml` and `infra-drift.job.yaml` reference the
reagent images by tag (`caesiumcloud/tf-runner:latest`). **That is a placeholder,
not a recommendation.** A tag is a mutable pointer: the registry can move
`:latest` — or any release tag — to different bytes at any time, and Caesium
resolves the reference when a task starts, so two runs of the "same" pipeline
can execute two different `tf-runner` builds. For a pipeline that applies
infrastructure, that is an unreviewed change to the thing doing the applying.

A digest reference is immutable — `sha256:<64 hex>` *is* the manifest's
content hash, so the registry can only serve those exact bytes or nothing:

```yaml
    image: caesiumcloud/tf-runner@sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03
```

Resolve the digest for a tag you have already pulled:

```sh
docker image inspect --format '{{index .RepoDigests 0}}' caesiumcloud/tf-runner:v1.2.3
```

or without pulling, straight from the registry:

```sh
docker buildx imagetools inspect caesiumcloud/tf-runner:v1.2.3 --format '{{.Manifest.Digest}}'
```

Pin all four roles (`git-source`, `tf-discover`, `tf-warm`, `tf-runner`) from
the same build, and re-pin deliberately — a reagent rebuild is a change to the
pipeline and should land as a reviewed commit to the manifest, exactly like a
change to a stack. Two things make re-pinning cheap rather than a chore:

- **`tf-warm` is version-coupled to `tf-runner`.** The mirror `tf-warm`
  populates is keyed on the provider set *and* the Terraform version baked
  into the image, and every `terraform init` afterwards runs offline against
  it. Bumping `tf-runner`'s Terraform version without bumping `tf-warm`'s is
  the one mismatch that produces a confusing failure rather than a clear one.
- **`warm-cache` self-heals.** It always runs and re-checks its `.warm/<key>`
  marker (`cache: false`), so a re-pin that changes the mirror key repopulates
  the volume on the next run with no manual step.

One caveat specific to a multi-arch reagent image: a digest taken from a *manifest list*
(the multi-arch index) still resolves per-architecture, which is what you want.
A digest taken from a single platform's manifest pins that platform and will
fail to pull on any other node — take the digest from the tag, not from a
platform-specific `docker manifest inspect` entry.

Until the images are published (plan item H-2 of the infra-deploy exec plan),
the reference manifests stay tag-pinned so they remain readable; treat the
tags as "fill in a digest here."

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

## The fan-out form (not yet supported)

The reference manifest's *hand-written* three-step-per-stack shape does not
scale past a handful of stacks — forty stacks means forty copy-pasted trios.
The intended shape collapses that to five steps: `discover` emits
`##caesium::partitions`, `plan` and `apply` each declare `fanOut:`, and one
`Task` expands to N `TaskRun` rows.

**That form does not work today, and this guide deliberately shows no snippet
for it.** An earlier revision presented one as "syntactically valid against
the shipped schema". It was not — and the gap it papered over is not
cosmetic, so a copy-pasteable snippet would have been worse than nothing.

Some of the machinery genuinely did ship.
[Dynamic fan-out](exec-plans/completed/dynamic-fanout.md) landed in #349 with the
structured-partition amendment this pattern needs (`{key, fingerprint,
dependsOn}` objects, not bare strings), and each instance's per-partition
fingerprint really does fold into its cache identity
(`internal/cache.HashInput.partitionIdentity`). `tf-discover`'s multi-root
mode already emits exactly that payload, and `tf-runner` already reads
`CAESIUM_PARTITION_JSON`'s `root` attribute. What is missing is the
plan → apply handoff, and two things around it.

**Gap 1 — a fanned predecessor is exposed as one group aggregate, never as the
matching partition.** `pkg/task.AggregateFanInOutputs` folds a fan-out group's
instance outputs into a *single* output set in which every key becomes a JSON
object keyed by partition value, plus synthetic
`PARTITION_COUNT`/`SUCCEEDED`/`FAILED`. So an `apply` that depends on a fanned
`plan` sees `CAESIUM_OUTPUT_PLAN_PROPOSAL_SUMMARY` as
`{"network":"{…}","account":"{…}","app-web":"{…}"}` — every partition at once
— while `tf-apply` hands that string to `tf.DecodeSummary`, which expects one
`terraform.plan.v1` summary. Every instance fails. `PROPOSAL_ARTIFACT` and the
artifact's path/`_DIGEST` pair aggregate the same way, so there would be
nothing for `verifyArtifact` to open even if the summary decoded.

And decoding would not rescue it: because every apply instance hashes the
whole aggregate, a change to ANY stack's plan would invalidate EVERY stack's
apply — precisely the cascade [`chain: values`](#cache-chain-the-sharp-edge)
exists to stop, reintroduced one level down.

Closing this needs **partition-correlated fan-in**: an instance of a fanned
consumer must see the outputs of the *same partition key* from a fanned
predecessor, projected out of the group rather than aggregated across it.
Nothing expresses that today — not the schema, not `internal/job`'s expander,
not the output plumbing.

**Gap 2 — `fanOut.from` must be a direct predecessor, and chained fan-out is
refused.** `validateFanOut` (`pkg/jobdef/definition.go`) rejects a definition
whose `fanOut.from` is not a real DAG predecessor of the fanning step, and
separately rejects a `fanOut.from` naming another `fanOut` step. So `apply`
cannot fan from `plan` at all: it must fan from `discover` and take an
ordering edge to `plan` — which is exactly the shape that lands in Gap 1.

**Gap 3 — per-unit state isolation is not expressible.** One step definition
serves every unit, so the per-stack `tfstate-<stack>` volumes of the
hand-written form ([Volumes](#volumes)) cannot be written down; a single
`tfstate` volume could have up to `maxPartitions` concurrent writers, capped
by a positive `maxParallel` when set. [The multi-writer
lint](#the-multi-writer-lint-design-8) now flags whenever that bound exceeds
one — a `fanOut:` step is checked against itself — but the lint warning is not
a fix. `subPath` cannot stand in: it is fixed per step
definition, so every fanned-out instance of that one step would resolve to
the identical value regardless of which engine runs it. And `tf-runner` takes
`BACKEND_CONFIG` verbatim from its environment with no partition
substitution, so a per-unit state path cannot be interpolated into the
manifest either. Isolation has to come from a backend that derives its own
per-unit key.

Until all three are closed — and proven by a runner end-to-end scenario for a
fanned `plan` → `apply` handoff, the way `test/infra_deploy_test.go` proves
the hand-written form — **use one hand-written trio per stack.** It is
verbose, and it is correct.

## The drift job (mandatory)

The fingerprint gate answers *"did my code change?"*; it is blind to drift by
construction — a resource edited by hand in a cloud console persists until
something else catches it. That is a deliberate cost/correctness trade-off
(§3.2), and the consequence is that **the drift job is part of the feature,
not an optional extra.**

`docs/examples/infra-drift.job.yaml` is a separate, cron-triggered job reusing
the deploy job's checkout/warm-cache shape, running `tf-drift` per stack
against the *same* per-stack `tfstate-<stack>` (and ideally `tfcache`)
physical volumes the deploy job manages — otherwise it is checking drift
against the wrong (or empty) state. The drift steps run in parallel, one per
stack, which is another reason state is one volume per stack rather than one
volume carved up by `subPath`. Sharing those stores across two job definitions
is invisible to the multi-writer lint and needs three deliberate mitigations —
a `concurrency` block, a distinct `ARTIFACT_DIR`, and a provider mirror whose
warms are idempotent. See [Sharing stores across jobs](#sharing-stores-across-jobs). Drift steps carry **`cache: false` explicitly**, not merely an
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
here, and since a named producer that published no sentinel now **fails the
phase**, setting it in a drift job is an error rather than a silent no-op. `docs/examples/infra-drift.job.yaml`'s `drift-app-web` step therefore
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

## Volumes

The reference manifests use three kinds of volume, and getting each one's
sharing model right is what keeps a pipeline from being green and wrong.

| Volume | Written by | Read by | Sharing |
|---|---|---|---|
| `src` | `prepare` (clears), then `checkout` (repopulates) — DAG-ordered | every discover/plan/apply, `readOnly: true` | one per job |
| `tfcache` | `warm-cache` only, in each job | every plan/apply/drift, `readOnly: true` | shared across the deploy and drift jobs — see [Sharing stores across jobs](#sharing-stores-across-jobs) |
| `tfstate-<stack>` | that stack's `plan` then its `apply` — DAG-ordered (and the drift job's `drift-<stack>`, under a separate `ARTIFACT_DIR`) | — | **one volume per stack**, shared with the drift job |

### Volume ownership: the reagent images run as UID 10001

Every reagent image drops to `10001:10001`, so every volume they write has to be
owned by — or group-writable to — that user. Their Dockerfile creates `/src`,
`/cache` and `/state` as `10001:10001` mode `0775`, but that baked ownership
reaches a volume in only one of the three ways a volume gets mounted, so the
rule to hold onto is:

> **The first container to mount a fresh named volume decides its ownership.**
> A reagent image mounting first owns the volume as 10001 for free. A
> `prepare`-style helper running a non-reagent image must `chown` the mount
> explicitly, because the reagent images' baked ownership never reaches a volume
> something else mounted first.

- **Docker / Podman named volumes** — the *first* container to mount a fresh
  named volume is the image its root ownership and content are copied from,
  and after that first mount it is fixed. In the reference manifests `tfcache`
  is first mounted by `warm-cache` (`caesiumcloud/tf-warm`) and each
  `tfstate-<stack>` by its own `plan-<stack>` (`caesiumcloud/tf-runner`) — reagent
  images, so those are correct with no operator action.

  `src` is the one that is not. `prepare` mounts it first, and it runs stock
  `alpine:3.23` (to `find /src -mindepth 1 -delete`) which has no `/src` in its
  image, so a brand-new volume would come up `root:root` and `checkout` —
  running as 10001 — could not write it. **`prepare` therefore hands the volume
  over explicitly**: after clearing the tree it runs

  ```sh
  chown 10001:10001 /src && chmod 0775 /src
  ```

  Both binaries ship in `alpine:3.23` and that container runs as root, so it costs
  nothing; ownership is set once and survives every later run. If you copy the
  manifest, keep the `chown` — or replace `prepare` with an image that already
  owns `/src` as 10001. (A reagent image cannot be dropped in as-is: `pkg/jobdef`'s
  `command:` supplies ARGS, not an entrypoint, so `["sh","-c",…]` against
  `caesiumcloud/git-source` would run `git-source sh -c …`. Folding the clear
  into `git-source` itself, deleting `prepare` entirely, is a possible
  follow-up.)

  The same rule applies to any volume you add: check which step mounts it first
  and, if that step is not a reagent image, chown the mount there.

- **Kubernetes PVCs** — none of the above applies. The image contributes
  nothing to a PVC; ownership comes from the volume, so it is a **cluster-side
  prerequisite**. Pre-provision each PVC already owned by 10001, or set
  `securityContext.fsGroup: 10001` on the pod. **`pkg/jobdef` exposes no pod
  security-context field at all** — no `fsGroup`, `runAsUser`, or
  `securityContext`; `VolumeSource` offers only `pvc`, `claimTemplate` and the
  free-form `volumeSource` map — so the manifest cannot express it today, and
  closing that schema gap is a follow-up. The image mode is `0775` rather than
  `0755` precisely so an `fsGroup` remap makes the mount writable.

- **Bind mounts** — ownership is the host directory's and the image contributes
  nothing at all: `chown 10001:10001 <dir> && chmod 0775 <dir>` before the
  first run. (This is what the integration lane does; it used to `chmod 0777`,
  which hid the problem rather than fixing it.)

### `subPath` sub-directories: Caesium's own helper is the first toucher

The rule above — *"the first container to mount a fresh named volume decides
its ownership"* — has one exception, and it is Caesium's own doing. When a
**docker** step mounts a named volume with `subPath` and that sub-directory
does not already exist, `internal/atom/docker`'s engine creates it itself,
before your step's container ever starts, using a short-lived helper
container (`ensureVolumeSubPath`). That helper always runs stock
`alpine:3.23` as root — it has no way to know your step's declared user — so
a **newly created** subPath sub-directory would otherwise come up
`root:root 0755`, denying any non-root step that does not already bake the
mount target with its own ownership, exactly the way a fresh `src` volume
denies `checkout` above.

To close that gap the helper `chmod 0777`s a sub-directory **only when it
creates it**; a sub-directory an earlier ordinary mount already materialized
(and possibly `chown`ed) is left completely untouched. This is a narrower
trade than the bind-mount `chmod 0777` warned against two bullets up: it
applies to exactly one Caesium-managed leaf directory the engine itself
provisions — never to an operator's own bind-mounted tree — and the engine
has no way to know the step's declared UID to `chown` to instead (`pkg/jobdef`
exposes no `runAsUser` equivalent for docker steps, the same gap the
Kubernetes PVC bullet above notes for pod security contexts). Ownership stays
`root:root`; only the permission bits open up. This caveat is specific to the
docker engine's implementation in this repo — Caesium interposes its own
helper container only there (`internal/atom/docker`); it does not run one for
kubernetes or podman steps.

### One logical volume per physical store

Give each physical store exactly one entry under `volumes:`. Declaring the
same docker volume / Kubernetes PVC twice under two names to make a pipeline
look like it has fewer writers does not make it have fewer writers — it only
hides them from the lint below, which keys on the logical name.

The per-stack state volumes are the sharp version of this. `subPath`
isolation is now real on every named-volume/PVC source used by this pattern
(below), so a single `tfstate` volume carved up by `subPath: <stack>` would no
longer silently corrupt state the way it once did on docker — but this guide
still recommends one
`tfstate-<stack>` volume per stack: it needs no Docker Engine API version
check, and it is what `test/infra_deploy_test.go` proves end-to-end. Treat
`subPath` on a shared volume as viable but unproven for this pattern, not as
the default:

> **The Docker engine now applies `VolumeMount.SubPath`** (fixed in #361).
> Kubernetes (`v1.VolumeMount.SubPath`) and Podman
> (`specgen.NamedVolume.SubPath`) already honoured it; `internal/atom/docker`'s
> mount conversion now maps `SubPath` onto `mount.VolumeOptions.Subpath` for
> named-volume mounts (creating the sub-directory on the volume first, via a
> short-lived helper container — `CAESIUM_DOCKER_SUBPATH_HELPER_IMAGE`,
> default `alpine:3.23`, controls its image for air-gapped/private-registry
> installs) and onto the joined host path for bind mounts. `subPath` values
> are validated as a relative path within the mount (`filepath.IsLocal`) —
> `subPath: ../../etc` is rejected outright, on both mount kinds, rather than
> silently resolved. This requires Docker Engine API >= 1.45 — Caesium fails
> the run loudly if the negotiated API is older, rather than silently
> mounting the whole volume. **Ownership caveat**: a *newly created* subPath
> sub-directory is `root:root` (only its permission bits are opened to
> `0777`) — see [Volume ownership](#volume-ownership-the-reagent-images-run-as-uid-10001).

With one volume per stack the backend path can stay identical across stacks
(`path=/state/terraform.tfstate`), the physical sources stay isolated on every
engine, and `caesium job lint` stays silent.

### Sharing stores across jobs

The drift job deliberately points its `tfcache` and `tfstate-<stack>` volumes
at the **deploy job's** physical stores — otherwise it would be checking drift
against empty or unrelated state, which is the whole point of the job. That is
the one case where the "one logical volume per physical store" rule is applied
*across* definitions, and **the lint cannot see it**: `caesium job lint`
examines each definition on its own, so a cross-job pair of read-write mounts
is invisible to it however either job's DAG is shaped.

Three things make it safe, and all three are load-bearing:

1. **A `metadata.concurrency` block on each job.** Admission is keyed on job
   id, so `infra-deploy-demo`'s `maxRuns: 1` constrains only itself — each job
   needs its own. The drift job uses `strategy: skip` (a drift check that
   missed its window is stale by the time a queue drains); the deploy job uses
   `queue` (a deploy that missed its slot still needs to happen). Neither
   prevents a *deploy* run and a *drift* run overlapping, which is what points
   2 and 3 are for.
2. **Distinct `ARTIFACT_DIR`s.** `tf-runner` derives `TF_DATA_DIR` as
   `<ARTIFACT_DIR>/tfdata` and every phase runs `terraform init -reconfigure`
   into it. Two concurrent inits rewriting one `.terraform/` — backend config,
   provider links, module manifest — corrupt it **silently**; Terraform's state
   lock protects the state file, not the data directory. So the drift job uses
   `/state/drift-artifacts` where the deploy job uses `/state/artifacts`.
3. **Warms of one key are idempotent.** `tf-warm` stages into its own
   `MkdirTemp` directory, promotes it with an atomic rename, adopts the winner
   if it loses the race, and writes its marker last — which is why design
   §3.5/§6.3 needs no named lock for the shared mirror. Concurrent warms of
   *different* provider sets on one volume are also safe **if each job sets a
   distinct `CACHE_KEY`**: `tf-warm` then writes `/cache/<key>/terraformrc`
   instead of flipping the shared `/cache/terraformrc` slot. Without
   `CACHE_KEY`, diverging `.terraform.lock.hcl` unions still last-writer-wins
   on that single file; give those jobs distinct keys (or distinct cache
   volumes).

### RWX requirement and RWO deferral

Declare **`accessMode: ReadWriteMany` on every volume more than one step
mounts** — `src`, `tfcache`, and the per-stack state volumes included. A
stack's `plan` and `apply` are separate pods and may be scheduled onto
different nodes, so state is not an exception. The field is documentary (the
Kubernetes engine does not translate it into anything; it tells whoever
provisions the PVC what is required), which is exactly why it has to match
reality: an operator provisioning from this manifest will follow it literally.
Name RWX PVCs with an `-rwx` suffix so the requirement is visible at the
`kubectl get pvc` level.

The shared provider mirror (`tfcache`) is the strictest case: it is a
warm-once, read-many volume — one `warm-cache` step holds the only read-write
mount, every other step mounts it `readOnly: true` — and a `filesystem_mirror`
shared by parallel `init` calls across pods on different nodes cannot live on
`ReadWriteOnce` storage. `ReadWriteOnce` support is **deferred** until a
node-affinity / co-location primitive lands (design §12), which would let every
step in a run schedule onto the same node as the volume.

`src` (the staged source tree) is shared across every step in the run — nine
steps in the reference manifest (materialize through every plan/apply). **Use
a pre-provisioned `pvc:`, not `claimTemplate`, for a volume mounted by more
than one step.** `claimTemplate` provisions an inline *ephemeral* PVC scoped to
one pod/step (see "Volumes And Workload Identity" in
[`job-definitions.md`](job-definitions.md)); the design spec's own §5.5
reference manifest uses `claimTemplate` for `src` and that is wrong for this
pattern's purposes — a grounding correction recorded on this plan's D2 item.

### `src` is mounted read-only for discover, plan, and apply

Only `prepare` and `checkout` write `src`. Everything downstream mounts it
`readOnly: true`, and both roles are built to make that hold:

- `tf-discover` scans its root without writing — it puts its own `TF_DATA_DIR`
  in a temp directory of its own making.
- `tf-runner` puts `TF_DATA_DIR` at `<ARTIFACT_DIR>/tfdata` rather than the
  stack's own `.terraform/`. `ARTIFACT_DIR` itself defaults to `<root>/.caesium`
  — inside `src` — so **set it explicitly onto the state volume**, as the
  reference manifests do (`ARTIFACT_DIR: /state/artifacts`). Leaving it at the
  default is what would put Terraform's working directory back inside the
  read-only mount.
- `tf-runner`'s `terraform init` runs with **`-lockfile=readonly`**. `init`
  rewrites `.terraform.lock.hcl` whenever the recorded provider set no longer
  matches what the configuration requires — a new provider, a pruned one, a
  hash for a platform the lock file has not seen. Under a read-only mount that
  write would surface as a bare permission error from inside Terraform;
  `-lockfile=readonly` turns it into Terraform's own *"Provider dependency
  changes detected … the lock file is read-only"* diagnostic instead.

  **What to do when you see it:** regenerate and commit the lock file, in the
  repository the pipeline checks out — never by loosening the mount:

  ```sh
  terraform providers lock -platform=linux_amd64 -platform=linux_arm64
  ```

  Lock every platform your runners can schedule onto. A lock file missing the
  running platform's hash is a lock-file update as far as `init` is concerned,
  so it fails the same way.

  An operator who sets `TF_CLI_ARGS_init` with their own `-lockfile=` value
  keeps it; anything else in that variable is preserved and appended to.

### The multi-writer lint (design §8)

`caesium job lint` warns when a named volume can have two or more concurrent
writers touching overlapping regions. Those writers may be distinct DAG
steps or multiple partition instances of one `fanOut:` step. It is a warning,
never an error (spec §11 Open Question 2), and it prints on both the local
path and `--server`.

What it does *not* flag, and why:

- **DAG-ordered writers.** If a path of `dependsOn`/`next` edges runs between
  the two steps (directly or transitively), they can never overlap in time and
  the volume is a handoff, not a race. `prepare` → `checkout` and
  `plan-<stack>` → `apply-<stack>` are exactly that, which is why the reference
  manifests are silent without any aliasing. A definition that declares no
  explicit edges at all is auto-linked into a sequential chain, and the lint
  reads that the same way the scheduler does.
- **Genuinely disjoint subPaths on a source whose runtime adapter applies
  them.** `subPath: a` and `subPath: b` address different subtrees for
  Kubernetes mounts, Docker bind/named-volume mounts, and Podman named
  volumes. Podman bind mounts currently ignore `subPath`, so lint treats each
  as exposing the whole source. Containment still counts: a mount with no
  effective `subPath` overlaps everything, and `reports` overlaps
  `reports/2026`.

What it *will* flag, and should:

- Two steps on parallel branches writing the same volume.
- Two steps sharing a raw `mounts: [{type: volume, source: …}]` volume without
  an ordering edge (that form has no `subPath` at all).
- A `fanOut:` step whose partition instances write one volume — a step *is*
  checked against itself for this case. Every fanned instance shares that
  step's `ResolvedVolumeMounts` verbatim (subPath is fixed at step-definition
  time, and a fanned instance's per-instance customization is limited to the
  injected partition env vars, never mounts), so `subPath` can never opt a
  fanned writer out for a potentially shared source. The finding is
  unconditional on subPath, but source- and concurrency-aware: tmpfs,
  `claimTemplate`, `emptyDir`, and generic ephemeral volumes are private to
  one container/pod and are omitted. A shared-source step whose own bound
  (`maxPartitions`, capped by a positive `maxParallel` when set) is 1 —
  `fanOut.maxParallel: 1`, or `fanOut.maxPartitions: 1` — also cannot have two
  instances from one run writing at once and is not flagged. Cross-run
  exclusion still requires `metadata.concurrency` with `maxRuns: 1`.

**Known limits.** Two false-negative boundaries concern what the check
compares:

- Two `volumes:` entries resolving to one physical store are treated as two
  volumes, and the job-level `volumes:` form is not cross-referenced against
  the raw `mounts:` form.
- `accessMode: ReadOnlyMany` is not read as making step-level `readOnly: true`
  redundant.
- A free-form Kubernetes `volumeSource` fails closed as potentially shared
  unless its sole top-level kind is `emptyDir` or `ephemeral`. That avoids
  missing plugin-backed shared storage, but a source-enforced read-only kind
  can still produce a conservative warning if its mount omits `readOnly: true`.

The other two are about the **scope** the ordering exemption reasons in, and
they matter more because the reference manifests depend on facts outside it:

- **Ordering and fan-out bounds hold within a single run.** These volumes are
  persistent, and a job with no `metadata.concurrency` block admits unlimited overlapping runs —
  so run 2's `prepare` can delete a tree run 1's `plan` is still reading, on a
  pair the lint is silent about by design. **`metadata.concurrency` is
  load-bearing for that silence**, not hygiene; `infra-deploy-demo` carries
  `{maxRuns: 1, strategy: queue}` and `infra-drift-demo` `{maxRuns: 1,
  strategy: skip}` for exactly this reason. Keep the block if you copy a
  manifest, especially onto a frequent cron.
- **The check runs per definition.** Two *jobs* whose volumes resolve to the
  same physical store are never compared. The shipped examples do exactly
  that on purpose — see [Sharing stores across jobs](#sharing-stores-across-jobs).

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
| `proposal_kind` | renderer selector, e.g. `terraform.plan.v1`. Mandatory for anything that will be applied: `tf-apply` requires exactly `terraform.plan.v1` and rejects an absent or empty kind, and the Console renders no proposal without one |
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
| **A cached `discover` replaying a stale remote-module identity** — the checkout is unchanged, but a registry range, Git branch, or re-pointed tag has moved, so `terraform get` would resolve different bytes | Every discover step in both reference manifests sets `cache: false` explicitly; an omitted block is cacheable under `CAESIUM_CACHE_ENABLED=true`. Pin remote modules to immutable revisions as well — that is what keeps plan/apply on the revision discover fingerprinted. |
| **A fingerprint blind to non-`.tf` assets** — a `templatefile` template, a `file()` asset or an `archive_file` source changes, the fingerprint does not, and plan/apply cache-hit | `tf-discover` hashes every regular file the stack root and its resolved modules own, at any depth, excluding only generated Terraform/state data ([`tf-discover`](#tf-discover)). |
| **Plan installing a different module revision than discover fingerprinted** | `tf-runner` clears `<TF_DATA_DIR>/modules` before every `init`, so a persistent data directory cannot pin a stale revision ([module re-resolution](#modules-are-re-resolved-on-every-init)). |
| **A published output name that does not survive the environment transport** — `vpcId` reaching a consumer as `TF_VAR_vpcid`, or `vpc-id`/`vpc_id` folding together, so Terraform uses a default and the run is green with wrong inputs | `tf-apply` fails closed on any published name outside `[a-z0-9_]+` or on any pair that folds ([Published output names](#published-output-names-must-survive-the-environment-transport)). |
| **`IMPORT_OUTPUTS_FROM` naming a producer that never ran** — a typo or a non-predecessor imports zero variables and the stack plans against defaults | Every named step must have published its `caesium_outputs_published` sentinel, or the phase fails naming the missing step. |
| **A retry re-applying a stale saved plan** after an apply succeeded but a post-apply step failed | `tf-apply` writes a durable apply receipt before its post-apply work; a matching receipt makes the retry republish outputs instead of re-applying ([A successful apply is recoverable](#a-successful-apply-is-recoverable)). Requires `ARTIFACT_DIR` on the state volume. |
| **A fresh volume unwritable by the reagent images** (they run as UID 10001) | The images own `/src`, `/cache`, `/state` as `10001:10001` 0775, which covers any named volume a reagent image mounts first — `tfcache` and every `tfstate-<stack>` in the reference manifests. `src` is mounted first by the `alpine:3.23` `prepare` step, which hands it over with `chown 10001:10001 /src && chmod 0775 /src`. A PVC needs `fsGroup: 10001` cluster-side; a bind mount needs a host `chown`. All of it in [Volume ownership](#volume-ownership-the-reagent-images-run-as-uid-10001). |
| **Drift invisible to deploy runs** | Accepted by design (§3.2); mitigated only by the mandatory drift job. |
| **Fingerprint nondeterminism** — absolute paths, mtimes, unsorted traversal, or locale differences split the cache and re-apply everything | `tf-discover`'s determinism requirement: relative paths only, sorted traversal, no mtimes; regression-tested (`TestInfraDiscoverFingerprintIsDeterministic`). |
| **Sensitive values in dqlite** | The two `tf-runner` requirements above — typed field access, not `jq`. |
| **`chain: values` surprise** | Documented here and surfaced by `caesium why`. |
| **Warm volume recreated** | `tf-warm` always runs and self-checks its marker — self-healing by construction. |
| **RWX unavailable** | Runtime mount failure with a clear message; RWO remains unsupported until node affinity lands. |
| **Two read-write mounts on one volume** | `internal/jobdef/lint`'s multi-writer warning (design §8), which flags writers that can run CONCURRENTLY — a pair cooperating through a `dependsOn` edge, like this pattern's own `prepare`/`checkout` and `plan`/`apply` handoffs, is legitimate and stays silent. See [Volumes](#the-multi-writer-lint-design-8). |
| **`subPath` relied on for isolation** | Docker now honours `VolumeMount.SubPath` (#361; before the fix a "partitioned" shared volume was actually one shared directory there), and the multi-writer lint follows each resolved source's adapter behavior. The reference named volumes/PVCs apply `subPath`; Podman binds do not and are treated as whole-source mounts. The manifests still use one volume per stack — simpler, with no Docker API-version dependency ([Volumes](#one-logical-volume-per-physical-store)). |
| **A newly created `subPath` sub-directory unwritable by a non-root docker step** | On docker only: Caesium's own root-running helper container creates the sub-directory, so — unlike the reagent-image row above — no step image's baked ownership reaches it. The helper `chmod 0777`s it, but only when it actually creates it; a pre-existing sub-directory is untouched. See [`subPath` sub-directories](#subpath-sub-directories-caesiums-own-helper-is-the-first-toucher). |
| **Two jobs sharing one physical store** | Invisible to the multi-writer lint, which runs per definition. Mitigated in the manifests: a `metadata.concurrency` block on each job, distinct `ARTIFACT_DIR`s so two `terraform init -reconfigure` data directories cannot collide, and a provider mirror whose warms are idempotent ([Sharing stores across jobs](#sharing-stores-across-jobs)). |
| **`terraform init` rewriting a read-only `src`** | `tf-runner` passes `-lockfile=readonly`, so a drifted `.terraform.lock.hcl` fails with Terraform's own dependency-change diagnostic instead of an opaque permission error ([`src` is mounted read-only](#src-is-mounted-read-only-for-discover-plan-and-apply)). |
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
- [`exec-plans/completed/dynamic-fanout.md`](exec-plans/completed/dynamic-fanout.md) —
  the fan-out mechanism itself, which shipped; the plan → apply handoff this
  pattern would need on top of it has [not](#the-fan-out-form-not-yet-supported).
- `superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md` —
  the full design this guide implements.
