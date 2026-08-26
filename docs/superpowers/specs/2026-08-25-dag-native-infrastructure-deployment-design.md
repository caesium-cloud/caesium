# Design: DAG-Native Infrastructure Deployment (Terraform first)

**Status:** Proposed → planned — exec plan [`docs/exec-plans/active/infra-deploy.md`](../../exec-plans/active/infra-deploy.md) (drafted 2026-08-26; no implementation wave shipped yet)
**Date:** 2026-08-25
**Author:** Christopher Ryan
**Topic:** Dependency-ordered deployment of infrastructure stacks as Caesium DAGs — with a
shared provider cache and change-gated execution — delivered as **one new schema key** plus a
container-image pack. Terraform is the first consumer; nothing in Caesium's Go learns what HCL is.

---

## 1. Summary

A team running a VPC/networking stack, an account-level stack, and N application stacks wants
them deployed in dependency order, with unchanged stacks skipped and the provider set
downloaded once rather than once per stack. Today they can express roughly this in Caesium YAML,
but every run re-applies every stack, and every stack re-downloads the same providers.

Closing that gap turns out to need almost no new surface area. Caesium already ships a
content-addressed task cache, large-object reference passing, branch steps, volumes, runtime
`secret://` resolution, and a dataset/lineage layer. Composed correctly, those deliver the
plan/apply handoff, the digest-pinned artifact, the skip-when-empty branch, the shared cache
volume, and the cross-stack output wiring — with no new primitives at all.

Exactly one thing is genuinely missing, and it is irreducible (§4): the cache chains predecessor
*identity hashes* transitively, so every stack sharing a checkout ancestor invalidates on every
commit. This design adds one key — `cache.chain` — to switch a step to value-mode.

Everything else ships as four published images, example manifests, a lint warning, a Console
panel that reads ordinary step outputs, and documentation.

**The deliverable is a reusable pattern, not a Terraform feature.** §5 defines it independently of
any tool — five container roles (materialize, warm, discover, propose, apply) communicating over
existing stdout markers — and Terraform is presented as the first *binding* of it. dbt, monorepo
CI, and database migrations are sketched as further bindings, each needing only images. Caesium
gains no knowledge of any of them.

**Guiding constraint, unchanged from the volumes work:** Caesium declares, orders, and mounts.
It does not learn Terraform, does not parse HCL, does not hold cloud credentials, and does not
put itself in the data path.

## 2. Motivation & current state

### 2.1 What exists

Every item below is shipped and is load-bearing for this design:

- **Content-addressed task cache** (`internal/cache/`) — `TaskIdentityHash` over image (plus
  resolved digest when pinning is on), command, env, workdir, mounts, `PredecessorHashes`,
  `PredecessorOutputs`, run params, and `cache.version`. `cached` is a terminal-success status
  that trigger rules and indegree propagation already treat as success. Invalidation lands via
  `caesium cache invalidate`; hit-rate metrics and a pruner exist.
- **Decomposed `HashInput` + `caesium why`** — field-level causal explanation of why a task ran
  or was skipped.
- **Large-object reference passing** — `##caesium::output-ref {"key","path","digest","size"}`.
  The digest folds into the identity hash, and per `pkg/task/output.go`, *a path change with an
  unchanged digest does not change the hash: the value, not the location, is the identity.*
- **Branch steps** — `##caesium::branch <step-name>`. `executeAtom` (`internal/job/job.go`)
  parses structured outputs and branch markers from the same task's logs in a single pass, so one
  step can run a real workload *and* select its successors.
- **Volumes** (`Definition.Volumes`, `Step.VolumeMounts`) — PVC, `claimTemplate`, bind, named
  volume, tmpfs; engine-keyed sources; `readOnly` and `subPath` per mount.
- **Runtime `secret://` resolution in step `env`** — Component 0 of the volumes work, plus
  workload-identity passthrough via `serviceAccountName` (IRSA / GKE WI / Pod Identity).
- **Data contracts** — `##caesium::output` → `CAESIUM_OUTPUT_<STEP>_<KEY>`, with
  `outputSchema`/`inputSchema`, `metadata.schemaValidation`, and the contract graph + apply-time
  enforcement.
- **Datasets & lineage** — `datasets.produces/consumes`, OpenLineage emission, `/lineage/impact`.
- **Concurrency** — `metadata.concurrency: {maxRuns, strategy}`.

### 2.2 What is missing

One thing: **the ability to stop a step's cache key from chaining predecessor identity hashes.**
See §4.

Everything else in this design is composition, images, or documentation.

## 3. Design decisions

Five decisions were settled during design. Each is recorded with its rationale and its cost,
because several are deliberate trade-offs rather than free wins.

### 3.1 A stack is a unit that also declares itself a dataset

A stack is one *unit* (§5) in a per-environment job, flowing through `discover → propose → apply`,
and its apply step declares `datasets.produces: stack:<env>/<name>`. This gives one run view and
explicit ordering, while the lineage/impact/contract machinery handles blast-radius queries and
cross-job or cross-environment edges for free. "Unit" rather than "stack" is the load-bearing
noun: it is a dbt model, a monorepo package, or a migration in the other bindings.

Rejected: stack-as-job (no single "deploy prod" run view; ordering moves into the event router)
and stack-as-step-group-only (no blast-radius query, no cross-job edges).

### 3.2 The source fingerprint gates the whole stack; drift is a separate job

If a stack's source, transitive modules, lockfile, tfvars, and consumed upstream outputs are all
unchanged, the entire group is skipped — no `init`, no `plan`, no `apply`.

**This is a deliberate trade-off with a real cost.** A fingerprint answers *"did my code
change?"*; it is blind to drift. A resource edited by hand in a cloud console persists until
something else catches it. The alternative — always run `plan -detailed-exitcode` and let its
exit code decide — is correct by construction but costs a warm `init`+`plan` per stack per run.

The fingerprint gate was chosen for cost. The consequence is that **the scheduled
`plan -refresh-only` drift job (§6.6) is mandatory, not optional.** It is the only thing that
closes the hole this decision opens, and it should be treated as part of the feature.

### 3.3 The fingerprint is computed by a container, not by Caesium

A `discover` step runs a Terraform-aware image, resolves the real module graph, and emits a
digest as an ordinary step output. Downstream steps consume it through the normal
predecessor-output hashing.

This keeps HCL parsing, `modules.json` handling, registry semantics, and OpenTofu variance out
of Caesium's Go, and makes the same protocol reusable for dbt (`manifest.json`), npm lockfiles,
or `go list -deps`. Cost: one cheap container per stack whenever the repo has moved at all.

Rejected: hand-maintained path globs in YAML (a forgotten module edge produces a green run that
deployed nothing — the worst failure this feature can have) and a Terraform-aware fingerprint
provider inside Caesium (breaks container-native; every new ecosystem becomes a Go PR).

### 3.4 The shared cache is a warm-once volume consumed read-only

One `warm-cache` step holds the only read-write mount; every other step mounts the volume
`readOnly: true` and points `TF_CLI_CONFIG_FILE` at a generated `.terraformrc` containing a
`provider_installation { filesystem_mirror { … } }` block.

This is not a stylistic choice. HashiCorp documents that *"the plugin cache directory is not
guaranteed to be concurrency safe. The provider installer's behavior in environments with
multiple `terraform init` calls is undefined"* — so `TF_PLUGIN_CACHE_DIR` is the wrong mechanism
for a cache shared by 40 parallel `init` calls. A filesystem mirror is read-only at consumption
time and safe under parallelism.

Cost: needs `ReadWriteMany` storage on Kubernetes. RWO stays blocked until the node-affinity /
co-location primitive lands (deferred, §12).

### 3.5 Scope: plan artifacts are in; gates, locks, and templates are out

In scope: the digest-pinned plan artifact and the Console panel that renders the resource-change
summary. Both fall out of shipped machinery.

Out of scope, each with an existing home: approval gates (roadmap §3.2), named exclusive locks,
and step-group templates (roadmap §2.2). Without templates the first delivery is verbose —
each stack is a hand-written four-step group — but fully functional.

**Named locks are not required.** Within a run the DAG serializes the warm step; across runs
`metadata.concurrency: {maxRuns: 1, strategy: queue}` does; and because the warm step writes
content-addressed under its key with an atomic rename, concurrent warms of the same key are
benign. Terraform backends additionally hold their own state locks, so a Caesium lock would buy
graceful queueing, not correctness.

## 4. The one core change: `cache.chain`

### 4.1 Why it is irreducible

The identity hash is computed **before** execution, because it decides whether to execute. A
checkout step's hash can therefore only contain its *inputs* — including the git ref, which
changes on every commit. That change propagates through `PredecessorHashes` to every stack in
the repo, so a one-line edit in `stacks/app-web` would invalidate all forty stacks and apply
them all.

There is no upstream fix. The checkout step cannot hash its own output (the tree digest) into
its own key, because the key must exist before the step runs. The chain has to be broken
downstream. `HashInput` already keeps `PredecessorHashes` and `PredecessorOutputs` as separate
fields, so the change is surgical.

### 4.2 Schema

```yaml
cache:
  version: 1
  chain: values        # transitive (default) | values
  ttl: never           # duration | never
```

`chain` is a new key on the existing `cache` object — the object users already write. Default
`transitive` preserves today's behavior exactly.

`ttl: never` maps to a nil `ExpiresAt`, which the store already models as a nullable
`*time.Time`. An apply step keyed purely on a fingerprint should not expire on a wall clock.

### 4.3 Hash semantics

```
chain: transitive (default)         chain: values
  PredecessorHashes    ✓              PredecessorHashes    ✗  excluded
  PredecessorOutputs   ✓              PredecessorOutputs   ✓  still hashed
  everything else      ✓              everything else      ✓
```

The meaning of `chain: values` is *"my key is what I consume, not my predecessors' internal
churn."* This is what makes cross-stack edges behave correctly: change the network stack's
internals and app stacks stay cached; change the network stack's `vpc_id` **output** and every
consumer re-plans.

`caesium why` must render the exclusion explicitly — `predecessor hashes excluded (chain:
values)` — or the skip becomes unexplainable.

### 4.4 The sharp edge

`chain: values` is a sharper knife than the default. An upstream change that alters behavior
without altering outputs will leave consumers cached. That is exactly what is wanted here and
exactly what will surprise someone eventually. It needs a documentation callout and the `why`
rendering above.

## 5. The generic pattern: unit pipelines

Terraform is the first consumer of this pattern, not its definition. The shape below is the
reusable part, and the Terraform pack is one binding of it. A team wanting the same behaviour for
dbt, a monorepo CI, or database migrations implements five container contracts and gets
change-gating, per-unit caching, fingerprint-accurate invalidation, lineage, and audit for free.

### 5.1 The five roles

A **unit** is the thing that changes independently and is deployed independently: a Terraform
stack, a dbt model, a monorepo package, a migration.

| Role | Responsibility | Runs |
|---|---|---|
| **Materialize** | pin and stage inputs; emit their identity | once |
| **Warm** | populate a shared read-only dependency cache, idempotently | once |
| **Discover** | enumerate units, fingerprint each, declare inter-unit order | once |
| **Propose** | per unit: produce a reviewable artifact + summary + proceed/no-op | per unit |
| **Apply** | per unit: consume exactly that artifact; emit outputs | per unit |

The first three are cheap and run once per job. The last two fan out over the discovered set.

### 5.2 Role contracts

The contracts are stdout markers and environment variables — no SDK, no Go plugin, no
Caesium-side knowledge of any tool. Any image honouring them is a drop-in.

| Role | Reads | Emits on stdout |
|---|---|---|
| Materialize | tool-specific env, `secret://` refs | `##caesium::output {commit, treeDigest, path}` |
| Warm | source path, cache dir | nothing; self-checks a marker in the cache and exits fast |
| Discover | source path, scan root | `##caesium::partitions [{key, fingerprint, dependsOn, …}]` |
| Propose | `CAESIUM_PARTITION`, source, cache | `##caesium::output-ref` for the artifact + proposal outputs |
| Apply | `CAESIUM_PARTITION`, the artifact ref | `##caesium::output {…}` for downstream units |

Two rules bind the pattern together and are what make it correct rather than merely convenient:

1. **Discover owns the fingerprint.** Caesium never inspects a unit's contents. Whatever the
   discover image says a unit's inputs digest to *is* that unit's cache key contribution.
2. **Every role fails closed.** A discover that cannot resolve a dependency exits non-zero. An
   absent fingerprint is never read as "unchanged" — that is the one failure this pattern must
   not have, in any binding.

### 5.3 Bindings

| Role | Terraform | dbt / ETL | Monorepo CI | DB migrations |
|---|---|---|---|---|
| Materialize | `git-source` | `git-source` | `git-source` | `git-source` |
| Warm | `providers mirror` | `dbt deps` + adapter | language dep cache | — |
| Discover | `get` + `modules.json` | `dbt ls --select state:modified+` | affected-package query (nx / turbo / bazel) | pending migration files |
| Propose | `plan -out` + `show -json` | `dbt compile` / dry-run | build + test → artifact | generate SQL + destructive-op scan |
| Apply | `apply tf.plan` | `dbt run` | publish / deploy | execute SQL |

dbt is the closest sibling: `state:modified+` is the same idea as the fingerprint, computed
natively, and dbt models have inter-model dependencies exactly as stacks do.

### 5.4 Fan-out is what makes this scale — and needs one enhancement

Written out by hand, each unit is a four-step group; forty stacks is an unreadable manifest, and
three hundred dbt models is impossible. [Dynamic fan-out](../../design-dynamic-fanout.md)
(designed, **no streams started** as of 2026-07) is the right vehicle: a producer emits
`##caesium::partitions`, the consumer declares `fanOut:`, and one `Task` expands to N `TaskRun`
rows each carrying `CAESIUM_PARTITION`. That turns any number of units into **five steps**.

Two things this pattern needs that plain string partitions cannot carry:

- **A per-unit fingerprint.** The fan-out design folds the partition *value* into the cache
  identity hash, which distinguishes units but not *versions* of a unit. Unit `network` must
  cache-hit when its code is unchanged and miss when it is not, so the fingerprint has to ride
  along and enter that instance's hash.
- **Inter-unit ordering.** Fan-out gives parallelism, not a graph. Terraform needs the VPC stack
  applied before the app stacks; dbt needs dimensions before facts. Without ordering inside the
  group, the pattern cannot express either.

Both are solved by letting a partition be an object rather than a bare string:

```
##caesium::partitions [
  {"key":"network", "fingerprint":"sha256:ab…", "dependsOn":[]},
  {"key":"account", "fingerprint":"sha256:cd…", "dependsOn":["network"]},
  {"key":"app-api", "fingerprint":"sha256:ef…", "dependsOn":["account"]}
]
```

- `key` → `CAESIUM_PARTITION`, and the whole object → `CAESIUM_PARTITION_JSON`
- `fingerprint` → folded into that instance's identity hash
- `dependsOn` → ordering within the group, cycle-checked at expansion time

A bare string array keeps its current meaning exactly, so this is backward compatible with the
fan-out design as written.

**The fingerprint is inert without §4, and the division of labour matters.** Drafting the
fan-out amendment (PR #344) established that a fingerprint in the partition object does *not*
by itself give per-unit change detection: `PredecessorHashes` still folds the producer's own
effective hash into every instance, so a changed discover step invalidates all N regardless of
what their fingerprints say. Three separate mechanisms compose here:

| Mechanism | Owner | Gives |
|---|---|---|
| `fanOut` + `partitions` | fan-out design | N instances from one step |
| `fingerprint` in the object | fan-out design | a per-instance discriminator |
| `cache.chain: values` | **§4, this spec** | the producer's churn stops dominating |

All three are required, which is why the reference manifest in §5.5 carries `chain: values` on
both fanned steps. Without the chain break the behaviour is conservative but still correct —
over-running, never a stale hit. **This enhancement belongs in `design-dynamic-fanout.md`, not here** —
it is data-engineering-first (per-file checksums and dependency-ordered dbt models want it as
much as stacks do) and this spec is one consumer. Until it lands, the fallback is the
hand-written per-unit group in §5.5, which works today and is merely verbose.

### 5.5 Reference manifest (Terraform binding)

Shown in the fan-out form. Five steps, whether the repo has three stacks or three hundred.

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: infra-prod
  concurrency: { maxRuns: 1, strategy: queue }
  schemaValidation: fail

volumes:
  - name: src
    source: { claimTemplate: { size: 5Gi, accessMode: ReadWriteMany } }
  - name: tfcache
    source: { pvc: tf-cache-rwx }

steps:
  # ── 1. MATERIALIZE ──────────────────────────────────────────
  - name: checkout
    image: ghcr.io/caesium/git-source@sha256:...
    env:
      GIT_URL:     "git@github.com:acme/infra.git"
      GIT_REF:     "${CAESIUM_PARAM_SHA}"
      GIT_SPARSE:  "stacks/** modules/**"
      GIT_SSH_KEY: "secret://env/DEPLOY_KEY"
    volumeMounts: [{ volume: src, path: /src }]
    outputSchema:
      commit:     { type: string }
      treeDigest: { type: string }

  # ── 2. WARM ─────────────────────────────────────────────────
  - name: warm-cache
    dependsOn: [checkout]
    image: ghcr.io/caesium/tf-warm@sha256:...
    volumeMounts:
      - { volume: src,     path: /src, readOnly: true }
      - { volume: tfcache, path: /cache }        # the only read-write mount

  # ── 3. DISCOVER — enumerates units AND fingerprints them ────
  - name: discover
    dependsOn: [checkout]
    image: ghcr.io/caesium/tf-discover@sha256:...
    env: { SCAN_ROOT: /src/stacks }
    volumeMounts: [{ volume: src, path: /src, readOnly: true }]
    # emits ##caesium::partitions [{key, fingerprint, dependsOn, root}, …]

  # ── 4. PROPOSE — one instance per unit ──────────────────────
  - name: plan
    dependsOn: [discover, warm-cache]
    image: ghcr.io/caesium/tf-runner@sha256:...
    command: ["tf-plan"]
    cache: { version: 1, chain: values }
    fanOut: { from: discover, maxParallel: 10, onEmpty: skip }
    volumeMounts:
      - { volume: src,     path: /src }
      - { volume: tfcache, path: /cache, readOnly: true }
    env: { TF_CLI_CONFIG_FILE: /cache/terraformrc }
    outputSchema:
      proposal_kind:     { type: string }
      proposal_summary:  { type: string }
      proposal_artifact: { type: string }

  # ── 5. APPLY — one instance per unit, in dependency order ───
  - name: apply
    dependsOn: [plan]
    image: ghcr.io/caesium/tf-runner@sha256:...
    command: ["tf-apply"]
    cache: { version: 1, chain: values, ttl: never }
    fanOut: { from: discover, maxParallel: 5, failurePolicy: fail_fast }
    volumeMounts:
      - { volume: src,     path: /src }
      - { volume: tfcache, path: /cache, readOnly: true }
    datasets:
      produces:
        - name: "stack:prod/${CAESIUM_PARTITION}"
          schemaFrom: output
```

### 5.6 Proposal rendering is a convention, not a schema

A propose step that emits three reserved output keys is rendered as a proposal by the Console:

| Key | Meaning |
|---|---|
| `proposal_kind` | renderer selector, e.g. `terraform.plan.v1`, `dbt.compile.v1` |
| `proposal_summary` | small JSON the generic renderer can display without knowing the kind |
| `proposal_artifact` | names the `##caesium::output-ref` key holding the reviewable artifact |

The Console keeps a renderer registry keyed on `proposal_kind`, with a **generic key/value
fallback** so an unknown kind still renders usefully. A team shipping `dbt.compile.v1` gets a
working panel on day one and can contribute a richer renderer later. This is a documented
convention over existing outputs — no new marker, no new schema key.

### 5.7 Every part maps to a shipped primitive

| Need | Mechanism | Status |
|---|---|---|
| source at a pinned commit | ordinary step + `secret://` in `env` + a volume | shipped |
| fingerprint transport | `##caesium::output` / partition object | shipped / §5.4 |
| artifact handoff | `##caesium::output-ref` (path + sha256, digest in the hash) | shipped |
| one writer on the cache volume | `readOnly: true` on every other mount | shipped |
| cross-unit wiring | `##caesium::output` → `CAESIUM_OUTPUT_*` | shipped |
| blast radius | `datasets.produces` → `/lineage/impact` | shipped |
| force re-run | `caesium cache invalidate` | shipped |
| per-unit expansion | `fanOut` + `##caesium::partitions` | designed, unstarted |
| per-unit fingerprint + ordering | structured partition objects | §5.4 proposal |
| **stop hash chaining** | **`cache.chain: values`** | **new, this spec** |

## 6. The Terraform binding

Four images implementing the five roles in §5.2. No Caesium Go changes beyond §4, and nothing
here is privileged — a dbt or CI binding is the same exercise with different images.

### 6.1 `git-source` (generic — not Terraform-specific)

Sparse shallow clone at a ref, then a tree digest from `git ls-tree -r <ref> -- <paths>` hashed
over sorted output. Using git's own object store means an exact content digest without reading
file bytes. Emits `commit`, `treeDigest`, `path`.

### 6.2 `tf-discover`

Resolves the module graph and emits `fingerprint` plus per-input digests (so `caesium why` can
name which input moved).

**Use Terraform's own module installation, not a hand-rolled HCL scan.** The step runs
`terraform get`, then reads the module manifest it writes at `.terraform/modules/modules.json`,
and hashes the union of the resolved module directories plus `*.tf`, `*.tfvars`, `*.tfvars.json`,
`*.tfquery.hcl`, `.terraform.lock.hcl`, and the workspace name.

`terraform get` installs modules **without** installing providers, so discover needs no provider
mirror — which is why it depends only on `checkout`, not on `warm-cache`.

All of the following was verified empirically against Terraform **v1.15.9** during design, not
inferred from documentation.

**Why the manifest and not `terraform modules -json`.** `terraform modules` (1.10+) is the
*supported* introspection surface and carries a `format_version`, but its JSON is insufficient
for fingerprinting. For a module nested one level down it emits:

```json
{"key":"inner","source":"../tags","version":""}
```

The `key` is the local call name with **no parent path**, and `source` is relative to the parent
module's directory — so the entry cannot be resolved to a real directory, and two different
parents each declaring an `inner` are indistinguishable. (The *text* output does render the
hierarchy; only `-json` loses it.) The manifest `terraform get` writes carries both missing
pieces:

```json
{"Key":"vpc.inner","Source":"../tags","Dir":"modules/tags"}
```

Fully-qualified `Key`, and `Dir` already resolved relative to the root module. That is exactly
what the fingerprint needs.

The trade-off is explicit: `modules.json` is an internal file with no documented compatibility
promise, while `terraform modules -json` has `format_version` but not enough data. The manifest
is the only one that works, so the image must pin its Terraform version and treat an unexpected
manifest shape as a hard failure. `terraform modules -json` remains the right tool if declared-
source *auditing or policy* is ever wanted — a different job from fingerprinting.

**The dynamic-source hazard is closed by Terraform itself.** A module `source` that is not a
literal or a const is a hard error at install time, so discover fails closed with no defensive
heuristic of ours:

```
Error: Unknown module source
  source = var.var_src
Only literal values and const variables can be evaluated during init.
```

`terraform get` exits non-zero, so the step fails and nothing downstream runs. Worth recording
because it is counterintuitive: a `locals` value holding a literal **does** resolve, while an
input **variable does not — even with a literal default, and even when passed explicitly via
`-var`**. So 1.15 dynamic module sources are far narrower than the name suggests, and the
"green run that deployed nothing" path is closed upstream rather than by us.

Fail-closed throughout: any error exits non-zero and emits no fingerprint.

Determinism requirement: relative paths only, sorted traversal, no mtimes, no locale-dependent
ordering. A fingerprint that differs between two workers silently re-applies everything.

### 6.3 `tf-warm`

Reads every `.terraform.lock.hcl` under the source tree, derives a mirror key from the sorted
provider/version/hash union, and checks `/cache/.warm/<key>`. If present, exits in about a
second. Otherwise runs `terraform providers mirror -platform=<target>` into
`/cache/providers.tmp.<key>`, atomically renames to `/cache/providers/<key>`, writes
`/cache/terraformrc` with a `filesystem_mirror` block, and drops the marker.

**The warm step is never Caesium-cached.** A cache hit would mean no container ran — and if the
PVC had been recreated the volume would be empty, failing every `init`. Always running and
self-checking a marker inside the volume is self-healing and removes the whole bug class. Its
cost is one container start per run.

Content-addressing plus atomic rename is what makes concurrent warms of the same key benign,
which is why no lock is required.

### 6.4 `tf-runner`

`tf-plan`: `init -input=false` (offline against the mirror), `workspace select`, then
`plan -out=tf.plan -detailed-exitcode -input=false`.

| exit | meaning | emits |
|---|---|---|
| 0 | no changes | `proposal_summary` with zero counts, no artifact |
| 2 | changes | `output-ref` for `tf.plan`, `proposal_*` outputs from `show -json` |
| 1 | error | task fails |

`tf-apply`: applies the saved plan file named by `proposal_artifact`, then `terraform output
-json`.

**How an empty plan skips the apply differs by form**, and both are supported:

- **Fan-out form (§5.5)** — `apply` is its own fan-out group, so there is no per-unit edge to
  suppress. The apply container reads `proposal_summary`, finds zero counts, and exits 0 without
  invoking Terraform. The decision lives in the image, which is where a tool-specific no-op
  belongs.
- **Hand-written form** — `plan` is a `type: branch` step and emits `##caesium::branch
  $CAESIUM_APPLY_STEP` only on exit 2, so the apply step is skipped by the DAG. This is the more
  honest rendering (the run visibly shows apply as skipped) and is why branch steps remain listed
  in §2.1.

The generic lesson for other bindings: **prefer the branch form while units are hand-written, and
let the container no-op once fanned out.**

**Two secret-handling requirements, or this leaks into dqlite.** Step outputs land in the
database and flow onward as environment variables, so the runner MUST drop every output marked
`sensitive = true` rather than emitting it, and MUST strip `sensitive_values` from `plan.json`
before the Console can render it. Sensitive values belong in a secret store, reached through the
existing `secret://` providers.

### 6.5 Terraform-native mechanisms

| Need | Mechanism |
|---|---|
| did anything change | `plan -detailed-exitcode` (0 / 1 / 2) |
| machine-readable diff | `plan -out` + `show -json` |
| cross-stack wiring | `output -json` → `TF_VAR_*` |
| shared provider cache | `provider_installation { filesystem_mirror }` + `providers mirror` |
| reproducible provider set | `.terraform.lock.hcl` + `providers lock -platform=` |
| module graph | `init -backend=false` + `.terraform/modules/modules.json` |
| pre-apply validation | `validate -json`, `terraform test` (`.tftest.hcl`) |
| state changes, declaratively | `moved` / `removed` / `import` blocks |
| post-apply health | `check` blocks |
| secrets never in state | ephemeral resources + write-only arguments |
| drift | `plan -refresh-only -detailed-exitcode` |

Verified during design (2026-08-25):

- **`terraform modules` / `terraform modules -json` exists as of Terraform 1.10** and enumerates
  declared module calls without running a plan. It is the supported surface, but its JSON drops
  the parent path and leaves relative sources unresolvable, so the fingerprint uses the
  `terraform get` manifest instead (§6.2).
- `terraform rpcapi` went GA in **1.13** but the changelog states it is *"not intended for public
  consumption"*; the pack does not build on it.
- Terraform **1.14** (Nov 2025) added list resources in `*.tfquery.hcl`, the `terraform query`
  command, and a provider-defined Actions block.
- Terraform **1.15** (Apr 2026) added variables and locals in module `source`/`version`, a
  `deprecated` attribute on variables and outputs, and `type` constraints on output blocks. The
  latter two pair naturally with `outputSchema` and the contract graph.
- The plugin cache directory is documented as **not concurrency safe**, and by default only
  serves packages matching lock-file checksums.
- Terraform's installation methods are `direct`, `filesystem_mirror`, and `network_mirror`.
  **There is no `oci_mirror` in Terraform** — OCI-registry provider mirrors are an OpenTofu
  feature. The pack therefore standardises on `filesystem_mirror`, which both tools support.

Version floors for the remaining capabilities (`moved`, `import`, `check`, `removed`,
`terraform test`, ephemeral resources, write-only arguments) should be pinned when the pack
images are built rather than asserted here.

Because Caesium never names the binary, everything works identically with OpenTofu. HCP
Terraform **Stacks** solves a similar problem, but requires HCP Terraform and is available in
neither open-source Terraform nor OpenTofu; the `terraform stacks` CLI command added in 1.13 is a
client whose availability depends on the stacks plugin implementation, and does not make Stacks
self-hostable. This design is the self-hosted equivalent. Wiring stacks through `output -json` rather than `terraform_remote_state`
also avoids granting every application stack read credentials on the network stack's state.

### 6.7 Implementation surface: libraries, not plugins

The pack images are Terraform-aware Go binaries, so the question of a "native Terraform
integration" is really a question about which upstream surface they build on.

**No plugin.** Terraform's plugin surfaces are not aimed at orchestration. Provider plugins
(resources, data sources, functions, ephemeral resources, list resources, and 1.14 Actions) run
*inside* Terraform during plan/apply, which would let Terraform call Caesium rather than Caesium
orchestrate Terraform — inverted control plus a runtime dependency from every invocation back to
the server. Backends are not pluggable; the set is compiled in. Provisioners are deprecated.
`terraform rpcapi` is the only orchestration-shaped surface and went GA in 1.13, but the
changelog states it is *"not intended for public consumption"* — no compatibility promise, for a
feature whose entire value is not silently under-reporting changes.

**Yes to `terraform-exec` + `terraform-json`.** Actively maintained (published 2026-04-29,
alongside 1.15), MPL-2.0, with typed returns and best-effort compatibility across minor versions.
The relevant surface:

```go
Plan(ctx, opts...) (bool, error)                        // diff non-empty; wraps -detailed-exitcode
ShowPlanFile(ctx, path, opts...) (*tfjson.Plan, error)
Output(ctx, opts...) (map[string]OutputMeta, error)     // OutputMeta carries Sensitive
Get / Init / ProvidersLock
```

This matters most for the two **security** requirements in §6.4: dropping `sensitive = true`
outputs and stripping `sensitive_values` from plan JSON become typed field access rather than
`jq` over a schema we would otherwise hand-track. `Plan`'s boolean is the changes signal
directly, removing exit-code handling from shell.

Two limits: tfexec exposes **no** module-manifest function, so the `modules.json` read in §6.2
stays hand-rolled and version-pinned; and this is native integration only *inside the images*,
where Terraform knowledge already belongs. Caesium core is unaffected.

### 6.6 The drift job (mandatory — see §3.2)

A separate job on a cron trigger, reusing the same checkout and warm steps, running
`plan -refresh-only -detailed-exitcode` per stack. Exit 2 emits an event that raises an incident
through the shipped path.

Drift steps carry **no `cache` block**. They must always run — caching the thing whose entire
purpose is to detect out-of-band change would defeat it.

## 7. Non-goals

- Caesium does not parse HCL, run Terraform, or hold cloud credentials.
- No Terraform state storage, no state backend, no registry. All BYO.
- No approval gates, named locks, or step-group templates in this delivery (§3.5).
- No RWO/node-affinity support for the cache volume (§12).
- Not a general CI system. This is dependency-ordered deployment of declarative stacks.
- **Caesium will not implement Terraform's HTTP backend protocol.** Backends are not pluggable,
  but the HTTP backend is a documented protocol (state GET/POST plus LOCK/UNLOCK) that any
  service could implement, which would hand Caesium native state locking and a single pane of
  glass. Rejected: it puts Caesium in the state data path, storing large sensitive blobs in
  dqlite, against the principle that Caesium never masters storage — and state is the one file a
  team cannot afford to lose or corrupt. It would also make Caesium a hard runtime dependency of
  Terraform runs it is not orchestrating.
- No Terraform plugin of any kind (§6.7).

## 8. Failure modes

Ordered by damage.

**A green run that deployed nothing.** The cardinal failure. Causes: `discover` under-reports its
inputs (see the 1.15 dynamic-source hazard in §6.2), or `discover` fails and is read as
"unchanged". Defenses: a discover failure is a *task* failure, so downstream never runs — status
propagation, not hashing, and it fails closed. A missing `fingerprint` output is a contract
violation under `schemaValidation: fail`. `caesium cache invalidate --job infra-prod` is the
force-apply escape hatch. **Additionally recommended:** a weekly full-apply run with caching
disabled, as the backstop that catches a systematically wrong fingerprint before it hides for a
month.

**Drift invisible to deploy runs.** Accepted (§3.2); mitigated only by §6.6.

**Fingerprint nondeterminism.** Absolute paths, mtimes, unsorted traversal, or locale differences
split the cache and re-apply everything. Mitigated by the determinism requirement in §6.2 and
tested explicitly.

**Sensitive values in dqlite.** Mitigated by the two runner requirements in §6.4.

**`chain: values` surprise.** Documented in §4.4 and surfaced by `caesium why`.

**Warm volume recreated.** Self-healing: the warm step always runs and checks its marker.

**RWX unavailable.** Runtime mount failure with a clear message; RWO remains unsupported (§12).

**Two read-write mounts on one volume.** Lint warning at apply time.

**Concurrent applies of one stack.** Terraform's backend state lock errors out;
`metadata.concurrency` prevents it at the job level.

## 9. Testing strategy

End-to-end is the gate. Every scenario below drives the real surface from `test/` — the CLI
binary or an HTTP endpoint against the live server — not a unit test on an internal handler.

Fixture: a hermetic fake infra repo — three stacks, two shared modules, `local` backend,
`null`/`random` providers. No cloud credentials; runs in CI.

1. Unchanged repo, second run → every plan and apply `cached`, zero stack containers.
2. Edit `stacks/app-web` only → only app-web re-applies; others stay `cached`. **Load-bearing.**
3. Edit `modules/vpc` → every stack transitively using it re-applies, and only those.
4. Change network's `vpc_id` output → `api-plan` busts although api's code is untouched, proving
   outputs still chain under `chain: values`.
5. Warm cache populated once; parallel plans consume it read-only; second run exits on the marker.
6. Empty plan → apply does not run Terraform and the run is green. Asserted in **both** forms:
   skipped via the branch marker when hand-written, and no-opped inside the container when
   fanned out.
7. Fail-closed: `discover` exits 1 → plan and apply do not run and the run is **red**, not
   green-with-skips.
8. Determinism: `discover` twice → byte-identical fingerprint.
9. A `sensitive = true` output never appears in the task output row or the API response.
10. A module block with a `source` that cannot reduce to a constant → `terraform get` errors,
    discover exits non-zero and emits no fingerprint, and the run is red. Regression-guards the
    upstream behaviour §6.2 now depends on.
11. A module nested two levels deep with a relative `source` → its resolved directory is included
    in the fingerprint, and editing a file in it re-applies exactly the stacks that use it.
    Guards the manifest-vs-`modules -json` distinction.

12. **A second, non-Terraform binding of the same pattern.** A trivial shell binding — a discover
    that emits two units with hand-computed fingerprints, a propose that writes a file, an apply
    that reads it — driven through the identical DAG shape with no Terraform involved. This is the
    genericity guarantee: if the contracts in §5.2 have quietly grown Terraform-shaped
    assumptions, this test is what fails.

Unit level, one test matters most: a golden test that default `chain: transitive` produces
**byte-identical** hashes to today, protecting every existing cache entry.

Repo-specific requirements:

- Assert `--json` output on stdout via `runCLIStdout`, not the stream-merging `runCLIRaw`.
- Set `CAESIUM_CACHE_ENABLED=true` in `just integration-up` **and** in the podman and helm lanes,
  which run their own servers and drift red silently otherwise.
- Use canonical pinned image references in fixtures or the image-pin guardrail fails.
- Regenerate `job-schema-reference.md` from `internal/jobdef/report/report.go` for the new
  `cache.chain` and `cache.ttl: never` keys. Never hand-edit that document.

## 10. Sequencing

1. `cache.chain` + `ttl: never` — schema, hash semantics, golden test, `caesium why` rendering,
   generated schema-reference update.
2. `git-source` and `tf-discover` images + the fingerprint determinism and fail-closed tests.
3. `tf-warm` + the mirror, marker, and parallel-consumption tests.
4. `tf-runner` propose/apply + output-ref, proposal-convention, and sensitive-value tests.
5. Reference manifests, the drift job, the multi-writer lint warning, and documentation.
6. Console panel rendering the plan summary from ordinary outputs.

Steps 1–4 each land with their integration scenarios; step 1 is independently useful to any
pipeline with a shared upstream step.

## 11. Open questions

1. Should `chain` also be settable at `metadata.cache` as a job-level default, matching how
   `cache` already cascades? Leaning yes for symmetry, but every step in a stack group wants it,
   so a job-level default may just hide the sharp edge.
2. Should the multi-writer volume check be a lint *warning* or an *error*? Warning is proposed;
   a legitimate two-writer case is hard to construct but should not be blocked outright.
3. Does the Console plan panel belong on the task detail view or as a run-level summary
   aggregating every stack's counts? The latter is more useful and more work.
4. Should the structured-partition enhancement (§5.4) be raised as a PR against
   `design-dynamic-fanout.md` before or after this spec is implemented? Before is tidier, but it
   couples two unshipped designs; after risks fan-out shipping with string-only partitions and
   needing a follow-up.
5. How do `fanOut` and `type: branch` interact? The fan-out form of this spec avoids the
   question (§6.4) — an unchanged unit is a cache hit and a changed-but-empty plan no-ops inside
   the container — but the hand-written form does use a branch step, the fan-out design says
   nothing about combining them, and someone will.
6. **Resolved during design** — the fast/full mode split is dropped in favour of `terraform get`
   plus the `.terraform/modules/modules.json` manifest (§6.2), and the dynamic-source question is
   settled: Terraform hard-errors on any non-const source, so discover fails closed on its own.
   The residual risk is that the manifest is an internal format; the image pins its Terraform
   version and treats an unexpected shape as a hard failure.

## 12. Deferred

- **Approval gates** (roadmap §3.2) — the natural consumer of the plan summary panel.
- **Named exclusive locks** — graceful queueing rather than correctness (§3.5).
- **Step-group templates** (roadmap §2.2) — what makes "a stack is a template" literally true and
  removes the per-stack copy-paste.
- **Node affinity / co-location** — unblocks RWO storage for the cache volume.
- **Dynamic fan-out** (`design-dynamic-fanout.md`, designed, unstarted) — the vehicle that turns
  N units into five steps (§5.4). This spec's hand-written form works without it; the pattern does
  not scale past a handful of units until it lands.
- **Structured partition objects** — the `{key, fingerprint, dependsOn}` enhancement in §5.4,
  which belongs in the fan-out design rather than here.
- **Matrix fan-out** — multi-region and multi-account apply from one stack definition.
- **Forge status / PR-comment callback** — surface the plan summary on the pull request.
- **A `caesium` Terraform provider** — `resource "caesium_job"`, `resource "caesium_trigger"`, so
  a GitOps team manages Caesium definitions *from* Terraform. This is the inverse direction of
  this spec (Terraform managing Caesium, rather than Caesium running Terraform) and is a separate
  feature, but it is the one genuinely useful plugin-shaped idea in this space.
