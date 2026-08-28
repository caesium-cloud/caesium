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
same `tfstate-<stack>` volume convention across jobs.

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

## Pinning the pack images to a digest

`docs/examples/infra-deploy.job.yaml` and `infra-drift.job.yaml` reference the
pack images by tag (`caesiumcloud/tf-runner:latest`). **That is a placeholder,
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
the same build, and re-pin deliberately — a pack rebuild is a change to the
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

One caveat specific to a multi-arch pack: a digest taken from a *manifest list*
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

**State isolation is the open edge in this form.** One step definition serves
every unit, so the per-stack state volumes of the hand-written form
([Volumes](#volumes)) are not expressible: the snippet mounts a single
`tfstate` volume and up to `maxParallel` partitions write it concurrently.
`subPath` cannot stand in for that — it is fixed per step definition, and
Docker ignores it entirely — and `tf-runner` takes `BACKEND_CONFIG` verbatim
from its environment with no partition substitution, so a per-unit state path
cannot be written into the manifest either. Isolation has to come from the
backend itself deriving a per-unit key. Settle that before promoting the
fan-out form to a `docs/examples/` manifest.

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
volume carved up by `subPath`. Drift steps carry **`cache: false` explicitly**, not merely an
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

## Volumes

The reference manifests use three kinds of volume, and getting each one's
sharing model right is what keeps a pipeline from being green and wrong.

| Volume | Written by | Read by | Sharing |
|---|---|---|---|
| `src` | `prepare` (clears), then `checkout` (repopulates) — DAG-ordered | every discover/plan/apply, `readOnly: true` | one per job |
| `tfcache` | `warm-cache` only | every plan/apply/drift, `readOnly: true` | shared across the deploy and drift jobs |
| `tfstate-<stack>` | that stack's `plan` then its `apply` — DAG-ordered (and the drift job's `drift-<stack>`) | — | **one volume per stack** |

### One logical volume per physical store

Give each physical store exactly one entry under `volumes:`. Declaring the
same docker volume / Kubernetes PVC twice under two names to make a pipeline
look like it has fewer writers does not make it have fewer writers — it only
hides them from the lint below, which keys on the logical name.

The per-stack state volumes are the sharp version of this. It is tempting to
declare one `tfstate` volume and carve it up with `subPath: <stack>` — do not:

> **The Docker engine does not apply `VolumeMount.SubPath`.** Kubernetes
> (`v1.VolumeMount.SubPath`) and Podman (`specgen.NamedVolume.SubPath`) both
> honour it; `internal/atom/docker`'s mount conversion never sets it. On
> Docker — the default engine, and the one `caesium dev` and `just run` use —
> every `subPath` mount of a volume resolves to the volume ROOT. Three stacks
> sharing one `tfstate` volume by `subPath` with the same
> `BACKEND_CONFIG: path=/state/terraform.tfstate` would all write the same
> file, and the last apply to finish would silently win.

With one volume per stack the backend path can stay identical across stacks
(`path=/state/terraform.tfstate`), and the isolation is real on every engine.

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

`caesium job lint` warns when a named volume is mounted read-write by two or
more steps **that can run concurrently and write overlapping regions**. It is a
warning, never an error (spec §11 Open Question 2), and it prints on both the
local path and `--server`.

What it does *not* flag, and why:

- **DAG-ordered writers.** If a path of `dependsOn`/`next` edges runs between
  the two steps (directly or transitively), they can never overlap in time and
  the volume is a handoff, not a race. `prepare` → `checkout` and
  `plan-<stack>` → `apply-<stack>` are exactly that, which is why the reference
  manifests are silent without any aliasing. A definition that declares no
  explicit edges at all is auto-linked into a sequential chain, and the lint
  reads that the same way the scheduler does.
- **Genuinely disjoint subPaths on kubernetes/podman.** `subPath: a` and
  `subPath: b` address different subtrees. Containment still counts: a mount
  with no `subPath` exposes the whole volume and overlaps everything, and
  `reports` overlaps `reports/2026`.

What it *will* flag, and should:

- Two steps on parallel branches writing the same volume.
- Two **docker** steps with different `subPath`s of one volume — because, per
  the box above, docker ignores `subPath` and both mounts cover the root. A
  step with no `engine:` is a docker step.
- Two steps sharing a raw `mounts: [{type: volume, source: …}]` volume without
  an ordering edge (that form has no `subPath` at all).

Known gaps, all of them false negatives: two `volumes:` entries resolving to
one physical store are treated as two volumes; the job-level `volumes:` form
and the raw `mounts:` form are not cross-referenced; `accessMode:
ReadOnlyMany` is not read as making step-level `readOnly: true` redundant; and
a step is never checked against itself, so a `fanOut:` step whose partitions
all write one volume is not flagged even though those instances do run
concurrently (the open edge noted in
[the fan-out form](#the-fan-out-form-forward-looking-mechanically-valid-now)).

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
| **Two read-write mounts on one volume** | `internal/jobdef/lint`'s multi-writer warning (design §8), which flags writers that can run CONCURRENTLY — a pair cooperating through a `dependsOn` edge, like this pattern's own `prepare`/`checkout` and `plan`/`apply` handoffs, is legitimate and stays silent. See [Volumes](#the-multi-writer-lint-design-8). |
| **`subPath` relied on for isolation** | Docker drops `VolumeMount.SubPath` entirely, so a "partitioned" shared volume is one shared directory there. The multi-writer lint reads docker mounts as root mounts and flags them; the manifests use one volume per stack instead ([Volumes](#one-logical-volume-per-physical-store)). |
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
- [`exec-plans/active/dynamic-fanout.md`](exec-plans/active/dynamic-fanout.md) —
  the shipped fan-out mechanism referenced above.
- `superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md` —
  the full design this guide implements.
