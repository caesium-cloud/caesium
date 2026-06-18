# Design: Volumes & Workload-Identity Passthrough (BYO storage + identity)

**Status:** Proposed
**Date:** 2026-05-29
**Author:** Christopher Ryan
**Topic:** Inter-step file handoff via bring-your-own shared storage, and bring-your-own
cloud identity for steps — enabling richer data-engineering pipelines and, as a bonus,
CI and infrastructure-CD use cases.

---

## 1. Summary

Give Caesium jobs two declarative capabilities, both of which **bring the resource and
Caesium only mounts/attaches it** — Caesium never provisions storage, stores bytes, or
masters a cloud role:

1. **Volumes** — a job declares named volumes backed by user-provided storage (a CSI/PVC
   in Kubernetes; an NFS/host mount or named volume in Docker/Podman). Steps mount them by
   reference; steps that co-mount a volume share its filesystem. This is the file-analog of
   the existing `##caesium::output` data contracts: a producer writes a file and emits its
   path, a downstream step reads `$CAESIUM_OUTPUT_*` and finds the file on the shared
   volume. It unblocks `extract → transform → load` handing real files along (impossible
   today under the 64 KB output cap), and equally `checkout → build → test` (CI) and
   `plan → apply` (infra-CD).

2. **Workload-identity passthrough** — a step can run under the **platform's** identity: a
   Kubernetes `serviceAccountName` wired to IRSA / EKS Pod Identity / GKE Workload Identity,
   or `AWS_ROLE_ARN` + a token file / instance profile on a Docker host. The container's
   cloud SDK then federates against the **user's** trust relationship. No OIDC issuer, no
   JWT minting, no token-signing infrastructure in Caesium.

Both ride on a **prerequisite correctness fix**: the distributed executor currently does
not apply a step's declared `env` / `workdir` / `mounts` and does not resolve `secret://`
at run time. That fix (Component 0) is the foundation that makes everything else — and the
existing `secret://` providers — actually work in distributed mode.

The guiding principle, set by the project owner: *Caesium's job is to declare, request, and
mount storage and identity — not to be storage or an identity provider.* This preserves the
zero-dependency, self-hosted, container-native identity of the project.

## 2. Motivation & current state

### 2.1 What exists

- **Container spec** (`pkg/container/spec.go`) is minimal: `Spec{ Env map[string]string;
  WorkDir string; Mounts []Mount }`, where `Mount{ Type; Source; Target; ReadOnly }` and the
  only `MountType` is `"bind"` (host path → container path). No volumes, named volumes,
  PVCs, tmpfs, resource limits, privileged flag, or service account.
- **Data contracts** ship: a step emits `##caesium::output {json}` (capped at 64 KB,
  `pkg/task/output.go`), downstream steps receive `CAESIUM_OUTPUT_<STEP>_<KEY>` env vars via
  `pkgtask.BuildOutputEnv`; `outputSchema`/`inputSchema` + `metadata.schemaValidation`
  validate the contract. This already provides dataflow-along-edges and reference-passing —
  it just cannot carry files.
- **Secret resolution** is a clean pluggable interface — `secret.Resolver.Resolve(ctx, ref)
  (string, error)` (`internal/jobdef/secret/resolver.go`), dispatched by `MultiResolver`
  (`multi.go`) across `env` / `k8s` / `vault` providers, built from env in
  `internal/jobdef/runtime/config.go:BuildSecretResolver`. Today it is invoked at git-sync,
  HTTP-trigger, and lint time — **not** in the step execution path (see 2.2).
- **Per-step engines**: `docker` / `podman` / `kubernetes`. Docker converts bind mounts in
  `internal/atom/docker/engine.go:convertMounts` (line ~267) → `mount.TypeBind`. Kubernetes
  converts every mount to a `HostPath` volume in
  `internal/atom/kubernetes/engine.go:convertKubernetesMounts` (line ~247) — **no PVC, CSI,
  named volume, or `serviceAccountName` support.**
- **Two executors**: the in-process/local executor (`internal/job/job.go`) and the
  distributed run-owner/claimer worker executor (`internal/worker/runtime_executor.go`).

### 2.2 The verified gaps this design fills

1. **The distributed executor drops the step spec (correctness bug / parity gap).**
   In `internal/worker/runtime_executor.go:executeTask()` (lines ~332–344) the container is
   created with `spec := container.Spec{}` and only `spec.Env = BuildOutputEnv(predOutputs)`.
   The step's real `atomSpec.Env` / `WorkDir` / `Mounts` are loaded **only** into the
   cache-hash input (lines ~143–167) and then discarded. By contrast the **local** executor
   (`internal/job/job.go:675–685`) merges and applies `spec.Env` + param env + output env.
   **Net effect:** step-level `env`, `secret://` references, `workdir`, `mounts`, **and
   `CAESIUM_PARAM_*` per-run params** work under `caesium dev` / single-node but are
   **silently ignored in distributed mode** — the distributed path never even queries params.
   This blocks Volumes (which extends `Mounts`) and any credential injection in distributed
   mode.

2. **No inter-step file handoff.** Output is capped at 64 KB and is env-only; there is no
   shared filesystem between steps. Canonical file flows (a built binary, a `.tfplan`, a
   Parquet dataset) cannot pass between steps without the user wiring external storage by
   hand.

3. **Kubernetes engine cannot mount real storage.** Only `HostPath`. No PVC/CSI/ephemeral
   claim, so cluster-managed shared storage is unreachable.

4. **No way to attach platform cloud identity.** A k8s step always runs under the pod's
   default service account; there is no `serviceAccountName` field, so IRSA / Pod Identity /
   GKE Workload Identity cannot be targeted per step.

5. **No node-affinity primitive.** `internal/dispatch/dispatch.go` round-robins a task to
   whatever worker accepts it; there is no affinity/anti-affinity. This bounds the Volumes
   design in distributed mode (see §7).

## 3. Goals / Non-goals

**Goals**

- The distributed executor applies a step's declared `env` / `workdir` / `mounts` and
  resolves `secret://` at container-create time, **identically to the local executor**.
- A job declares named **volumes** backed by user-provided storage; steps mount them; steps
  co-mounting a volume share its filesystem.
- Kubernetes steps can mount a PVC, an ephemeral CSI claim, or a generic volume source;
  Docker/Podman steps can mount a bind path, a named volume, or tmpfs.
- A step can attach the **platform's** workload identity (k8s `serviceAccountName`;
  Docker via env/mounts), so cloud SDKs in the container federate against the user's trust.
- File handoff between steps works through the **existing** data-contract path-passing — no
  new artifact/snapshot subsystem.
- Single binary, distributed sqlite, **no new mandatory external dependency.**

**Non-goals (this spec)**

- **No OIDC issuer / JWT minting / token-signing.** Caesium never masters a cloud role.
  Workload identity is the platform's; Caesium only attaches it.
- **No object-storage handoff convention** (push/pull to S3/GCS). Strong roadmap item
  (§9), designed separately.
- **No node-affinity / co-location scheduler primitive.** Documented limitation (§7);
  roadmap item.
- **No Caesium-provisioned storage.** Backing storage is always BYO; for ephemeral volumes
  Caesium manages only the *claim object* lifecycle, never the storage itself.
- **No new container privileges** (privileged, securityContext, resource limits) — out of
  scope, tracked separately.

## 4. Architecture overview

```
Job definition (YAML)
  volumes:            ── declared once, BYO-backed
  steps[].volumeMounts ── reference a volume by name + mount path
  steps[].serviceAccountName (k8s) / env+mounts (docker) ── BYO identity

         │ apply / git-sync
         ▼
  Validation (pkg/jobdef): volumes resolvable, mounts reference declared volumes,
  one source kind per engine, a source for each mounting step's *declared* engine,
  access-mode sanity, engine compatibility.

         │ stored as Atom.Spec (JSON) + Job/Trigger models
         ▼
  Execution
    Component 0 fix: BOTH executors load the full step Spec, resolve secret://
      at container-create, merge env with identical precedence.
    Engines materialize volumes + identity per runtime:
      docker/podman: bind | named volume | tmpfs ; env/mounts carry identity
      kubernetes:   PVC | ephemeral claimTemplate | generic source ; serviceAccountName
    Ephemeral-volume lifecycle: k8s via native inline ephemeral claim (kubelet GCs it);
      docker/podman via run-owner create-at-start + crash-safe reaper at run end.
```

The three components are independently testable increments and should land in order:
**0 → 1 → 2**. Component 0 is a prerequisite for 1 and 2 in distributed mode.

## 5. Component 0 — Run-time spec application + secret resolution (prerequisite)

**Problem.** The distributed worker discards the step spec (§2.2.1). Concretely,
`executeTask()` builds `spec := container.Spec{}` and sets only
`spec.Env = BuildOutputEnv(predOutputs)` — so **three** things the local executor applies are
absent from the distributed path:
1. the step-declared `Spec.Env` (including `secret://…` references), `WorkDir`, and `Mounts`;
2. `CAESIUM_PARAM_*` per-run params — the local executor injects these via
   `buildParamEnv(snapshot.ID, j.alias, snapshot.Params)`; the distributed path never queries
   params at all. This is a **co-equal gap** with the missing step env, not an afterthought;
3. `secret://` resolution (Component 0's headline).

**Change.**

- Extend `NewRuntimeExecutor` (`internal/worker/runtime_executor.go:46`) to accept a
  `secret.Resolver` (built once at worker startup via
  `internal/jobdef/runtime/config.go:BuildSecretResolver`).
- **Make the step spec reachable from `executeTask()`.** Today the only `atomModel.Spec` load
  lives in `Execute()` inside `if cacheCfg.Enabled { … }` (for the cache hash) and is **not
  reachable** from `executeTask()` — and when caching is disabled it does not run at all. So
  `executeTask()` must obtain the spec itself: either (preferred) refactor `executeTask` to
  accept the already-loaded `container.Spec` as a parameter threaded from the caller, or do one
  explicit `atoms` read inside `executeTask`. Whichever is chosen, the spec is **explicit**:
  there is no pre-existing load to piggyback on, and a naive "reuse the hashing load" would add
  a second per-task DB query (and break entirely with caching off).
- In `executeTask()` (line ~312), replace `spec := container.Spec{}` (line ~332) with that
  **full** atom spec, then:
  1. For every `spec.Env` value matching `secret://…`, call `resolver.Resolve(taskCtx, v)`
     and substitute the resolved value. A resolution error fails the task with a clear
     message (never silently blank).
  2. Query per-run params and build `CAESIUM_PARAM_*`, then merge them and predecessor-output
     env on top, using the **exact precedence the local executor uses**
     (`internal/job/job.go:675–685` and 812–833).
  3. Carry `WorkDir` and `Mounts` through unchanged.
- Pass the populated `spec` to `engine.Create` (line ~341).

**Resolution timing.** Per-run, at container-create — required so short-lived /
rotation-sensitive secrets are read fresh (the `env` provider already reads
`os.LookupEnv` at resolve time; `vault`/`k8s` fetch live). Run identity is **not** needed
for the consumer providers; if a future provider needs it, the existing
`dispatchMetaFrom(ctx)` pattern (`runtime_executor.go:65`) can carry it.

**Cache-hash interaction (must preserve).** The hash already incorporates the
**unresolved** `secret://` string (via `atomSpec.Env` in `hashInput.Env`). Keep it that
way: hashing the *resolved* value would (a) leak secret material into the cache key and
(b) bust the cache on every rotation. Document: `secret://` URIs are hashed **by reference,
not value**; volatile per-run values remain excluded.

**Parity contract & test.** A golden test asserts that, for the same definition, the env
(step env, `CAESIUM_PARAM_*`, and predecessor outputs) / workdir / mounts handed to
`engine.Create` are byte-identical between the local executor and the distributed executor.
Integration tests prove (a) a `secret://env/...` value declared in `step.env` and (b) a
per-run param reach the container in distributed mode (both currently absent).

**Dependency honesty.** This makes `secret://env` work with zero external dependency;
`secret://vault` still requires a running Vault and `secret://k8s` requires cluster API
access — those are external dependencies inherent to those providers and must be labeled as
such in docs.

## 6. Component 1 — Volumes (BYO shared storage)

### 6.1 YAML surface

```yaml
volumes:
  - name: work                       # referenced by steps
    # PORTABLE form — per-engine sources so the SAME definition runs under
    # `caesium dev` (docker) and in production (kubernetes) without edits.
    # The executor selects the source matching the executing step's engine.
    sources:
      kubernetes:
        pvc: ci-shared-rwx           # pre-existing PVC (mount only)
        # claimTemplate:             #   inline ephemeral, per-step (kubelet GCs with the pod)
        #   storageClass: nfs-csi    #   NOT shared across steps — use an external `pvc` for that
        #   size: 5Gi
        #   accessMode: ReadWriteOnce
        # volumeSource:              #   generic k8s VolumeSource pass-through (nfs, csi, …)
        #   nfs: { server: 10.0.0.5, path: /export/caesium }
      docker:
        bind: /mnt/nfs/caesium-work  # host path — must be an NFS/shared mount on every worker
      podman:
        bind: /mnt/nfs/caesium-work
    # SHORTHAND — single-engine jobs may use one `source:` with exactly one kind:
    #   source: { pvc: ci-shared-rwx }

steps:
  - name: plan
    image: hashicorp/terraform:1.9
    command: ["sh","-c","terraform plan -out=/work/tf.plan && echo '##caesium::output {\"plan\":\"/work/tf.plan\"}'"]
    volumeMounts:
      - { volume: work, path: /work }
    outputSchema: { plan: { type: string } }

  - name: apply
    dependsOn: [plan]
    image: hashicorp/terraform:1.9
    command: ["sh","-c","terraform apply $CAESIUM_OUTPUT_PLAN_PLAN"]
    volumeMounts:
      - { volume: work, path: /work, readOnly: true }   # apply reads the exact reviewed plan
```

`volumeMounts` entry: `{ volume, path, readOnly?, subPath? }`.

### 6.2 Schema & validation (`pkg/jobdef/definition.go`)

- New `Volume` type (job-level `Definition.Volumes []Volume`) and `VolumeMount` type
  (step-level `Step.VolumeMounts []VolumeMount`).
- Update **both** `Step.UnmarshalYAML` (line ~134) and `Step.UnmarshalJSON` (line ~197) —
  the codebase has two unmarshalers that must stay in sync.
- A volume declares **either** a single `source` (one source kind) **or** a `sources` map
  keyed by engine (`kubernetes`/`docker`/`podman`), each entry holding exactly one source
  kind. The `sources` map is what makes a definition portable across `caesium dev` (docker)
  and production (kubernetes).
- Validation in `Validate()` / `validateSteps`:
  - every `volumeMounts[].volume` references a declared job volume;
  - exactly one of `source` / `sources` is set; within it, exactly one source **kind** per
    engine entry;
  - `pvc`/`claimTemplate`/`volumeSource` are k8s-only; `bind`/`volume`/`tmpfs` are
    docker/podman-only — reject a source kind that doesn't match its engine key (or, for the
    `source` shorthand, the step's `engine`);
  - each step has exactly one **declared** `engine` (defaulting to `docker`). Validation
    checks only that a volume has a resolvable source for the *declared engine of each step
    that mounts it* — **not** for every known engine. The `sources` map enables portability
    but is never *required* to populate every engine: a single-engine job declaring only its
    one engine's source is valid. Fail fast at lint/apply only when a mounting step's declared
    engine has no source for that volume;
  - mount paths are absolute and unique per step; `accessMode` is a known value.

### 6.3 Container spec & engines

- Extend `pkg/container/spec.go`: add a resolved volume model to `Spec` (named volume refs +
  mount descriptors) alongside the existing bind `Mounts` (kept for backward compatibility).
  The whole `Spec` already serializes to `Atom.Spec` JSON and flows to distributed workers,
  so no new transport is needed.
- **Docker/Podman** (`internal/atom/docker/engine.go:convertMounts`, podman equivalent): add
  `mount.TypeVolume` (named volume) and `mount.TypeTmpfs` alongside `TypeBind`.
- **Kubernetes** (`internal/atom/kubernetes/engine.go:convertKubernetesMounts`): emit
  `PersistentVolumeClaim` volume sources, **inline `ephemeral.volumeClaimTemplate`** for
  ephemeral claims (kubelet-managed lifecycle — see §6.4), and generic pass-through
  `VolumeSource`s — not only `HostPath`. Reuse `sanitizeVolumeName`.

### 6.4 Ephemeral-volume lifecycle

- **External** sources (`pvc`, `bind`, named `volume`, generic source): mount only; Caesium
  never creates or deletes them.
- **Ephemeral on k8s (`claimTemplate`): prefer the native inline
  `volumes[].ephemeral.volumeClaimTemplate`** (pod-spec field, GA since k8s 1.23). The kubelet
  owns the generated PVC's lifecycle and garbage-collects it (via owner-reference) when the pod
  is deleted — so Caesium creates **no** separate claim object, there is **no** crash-unsafe
  window between PVC-create and pod-create, and **no orphan-reaper is needed on k8s**. Backing
  storage is still BYO (the StorageClass/CSI). **Scope:** an inline ephemeral claim is bound to
  one pod, i.e. one **step** — it is per-step scratch, not a shared volume. Ephemeral scratch
  that must be **shared across steps** is intentionally **not** offered via `claimTemplate` in
  v1 (a standalone Caesium-managed PVC would reintroduce the manual create/delete + reaper this
  bullet removes); users who need shared ephemeral scratch pre-provision an **external** RWX
  `pvc` (mount-only, §7) instead. This keeps the entire k8s path reaper-free.
- **Ephemeral on docker/podman (a per-run named volume):** there is no kubelet equivalent, so
  Caesium creates the volume at **run start** and deletes it at **run terminal state**. Cleanup
  must be **crash-safe**: tie deletion to run completion in the run-owner state machine and add
  an orphan-reaper that GCs ephemeral volumes whose run is terminal/absent (mirrors the existing
  cache pruner pattern). Name ephemeral resources deterministically by run id for reliable GC.
  (The manual-lifecycle + reaper machinery is therefore docker/podman-only, not k8s.)
- **Distributed-mode constraint on ephemeral docker/podman volumes (important):** a named
  docker/podman volume lives on a single daemon's local host. With no node-affinity (§7), a
  run-owner that creates a per-run named volume cannot share it with a co-mounting step
  dispatched to a *different* worker — that worker would silently auto-create an empty
  local volume of the same name, losing data. Therefore **ephemeral named docker/podman
  volumes are single-node-only**; validation rejects them when execution mode is distributed.
  Cross-node sharing on docker/podman requires an **external** `bind` source pointing at a
  shared network filesystem (NFS/CephFS) mounted on every worker (which Caesium mounts but
  does not manage). On k8s, inline ephemeral claims are per-pod (per-step) and so never
  cross-node-shared by construction; cross-step shared scratch uses a pre-provisioned **RWX**
  `pvc` (RWO has the single-mounter limitation — see §7).

### 6.5 File handoff via existing contracts (no new subsystem)

No change to the contract mechanism. The producer emits the file's path as a normal output
(`##caesium::output {"plan":"/work/tf.plan"}`); the consumer reads `$CAESIUM_OUTPUT_PLAN_PLAN`
and finds the file because both steps mount `work`. Because Caesium does **not** own the
bytes, it does **not** content-address volume contents — only the path string flows into the
cache key. Document this explicitly: changing a file on a shared volume does **not**
auto-invalidate a downstream cache the way a changed string output does. Mitigations: a step
may emit a content hash of its output as a normal output (which *does* fold into the cache
key); ephemeral scratch is fresh per run regardless.

## 7. Distributed-mode behavior & the RWX-only limitation

A shared volume only works where it is reachable by the worker that runs each co-mounting
step. Because the dispatcher has **no node-affinity primitive** (§2.2.5):

- **ReadWriteMany** storage (NFS, CephFS, EFS via CSI; or a host NFS mounted on every Docker
  worker) — any node can co-mount. **Fully supported; this is the recommended mode for
  fan-out/fan-in DAGs in distributed deployments.**
- **ReadWriteOnce** PVCs / per-run scratch — only one node can mount at a time. Co-locating
  the co-mounting steps **is not expressible today**. For v1 this is a **documented
  limitation**: RWO volumes are safe only when the DAG segment sharing them is effectively
  serial on one node, which Caesium cannot currently guarantee in distributed mode.
  Single-node (Docker/Podman, or a single-worker cluster) is unaffected.
- **Docker/Podman named (and ephemeral) volumes are node-local — never shareable across
  workers.** Unlike a k8s RWX PVC, there is no cross-node docker/podman volume. In a
  multi-worker docker/podman deployment, the only way to share a volume between steps that
  may land on different workers is an **external `bind` source backed by a shared network
  filesystem** (NFS/CephFS) mounted at the same path on every worker. Named/ephemeral
  docker/podman volumes are therefore validated as **single-node-only** (§6.4).

A node-affinity primitive (to make RWO co-location safe) and an object-storage handoff path
(node-agnostic, no co-location needed) are the two roadmap items that lift this limitation
(§9). The spec does **not** attempt either here; it ships the RWX-correct behavior and names
the boundary honestly.

## 8. Component 2 — Workload-identity passthrough (BYO cloud identity)

Caesium attaches the **platform's** identity to a step; the container's cloud SDK federates
against the **user's** trust relationship. Caesium mints nothing and masters no role.

### 8.1 Kubernetes

Add `serviceAccountName` (step-level, with an optional job-level default), plus optional
pod `annotations` and `automountServiceAccountToken`, to the step schema and the k8s engine
pod spec. The user creates a ServiceAccount annotated for their mechanism — IRSA
(`eks.amazonaws.com/role-arn`), EKS Pod Identity, or GKE Workload Identity — and Caesium runs
the step's pod under it. The AWS/GCP SDK in the container then resolves credentials from the
projected token automatically.

```yaml
steps:
  - name: terraform-apply
    image: hashicorp/terraform:1.9
    serviceAccountName: caesium-deployer   # user-owned SA ↔ cloud role trust
    volumeMounts: [{ volume: work, path: /work, readOnly: true }]
    command: ["terraform","apply","/work/tf.plan"]
```

### 8.2 Docker / Podman

No new field needed. Once Component 0 applies step `env`/`mounts`, the user attaches identity
the standard way: set `AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE` (or `GOOGLE_APPLICATION_
CREDENTIALS`) via `env`, mount the token file / `~/.aws` via `mounts`, or rely on the host
instance profile. Documented as a usage pattern, not a feature.

### 8.3 Security posture

**No token-issuance threat model.** Caesium does not issue tokens or hold a signing key, so
there is no cross-job subject spoofing, audience confusion, or key-rotation surface to model.
The cloud trust relationship lives entirely in the user's IAM and platform service accounts.

**Operational note — service-account *selection* (the one residual vector).** Letting a step
name a `serviceAccountName` introduces a confused-deputy risk distinct from token issuance:
if the Caesium controller may launch pods under *any* service account in its namespace, then
anyone who can apply or trigger a job could name a privileged SA (e.g. one with cloud-admin
access) and inherit it. GitOps PR review covers this only where apply access is strictly
gated — not necessarily in dev/staging. This is not a Caesium-side mechanism to build; it is
deployment guidance to document, with three layered mitigations operators should apply:

1. **Scope the controller's RBAC** — grant the Caesium controller/worker permission to use
   only a bounded set of service accounts (RoleBinding to specific SAs, not cluster-wide),
   so it physically cannot launch pods under privileged accounts it shouldn't.
2. **Optional Caesium-side allowlist** — an operator-configured allowlist of permitted
   `serviceAccountName` values (and/or namespaces); a job naming an SA outside it is rejected
   at apply. Cheap defense-in-depth that does not depend on cluster RBAC being perfect.
3. **Admission control** — document that operators should enforce SA assignment with a policy
   engine (OPA Gatekeeper / Kyverno) as the authoritative guardrail, independent of Caesium.

The same principle applies to the Docker/Podman path: the host's mounted credentials /
instance profile are whatever the operator exposes to the engine, so the blast radius is the
operator's to bound. Caesium's job is to attach identity, not to police the platform's IAM —
but the docs must make this selection risk explicit so administrators configure 1–3.

## 9. Roadmap sketch (documented, designed in later specs)

Each is dual-purpose — framed data-engineering-first, with CI/CD as the bonus:

- **Object-storage handoff convention** — node-agnostic file passing via explicit
  `aws s3 cp`/`gcloud storage cp` push/pull to a **user** bucket, with a Caesium-owned key
  convention (`<bucket>/<job_alias>/<run_id>/<step>/…`) and injected creds (Component 2).
  Caesium never touches the bytes. This is the node-agnostic alternative to RWO volumes and
  the cross-cluster handoff path. *(Reject s3fs/gcsfuse mounting — non-POSIX trap; reject a
  Caesium-run stager — puts bytes in Caesium's path.)*
- **Node-affinity / co-location primitive** — unblocks RWO volumes in distributed mode.
- **Digest-pinned cache keys** — the cache hashes the image **tag** string, not the resolved
  digest (`internal/cache/hash.go`); a mutable `:latest` can serve stale cached outputs. A
  correctness fix that benefits every workload.
- **Matrix fan-out** — backfill-style fan-out for data + test-matrix / multi-region apply.
- **Approval gates** — gate a destructive reprocess for data + a prod apply for CD.
- **Apply-only resource lock** — a dqlite-backed named mutex; serialize a warehouse load for
  data + Terraform state for CD. Zero new dependency.
- **Event-driven job chaining** — DAG-of-DAGs for data + cross-stack ordering for CD (the
  designed but unshipped event-trigger routing).
- **Forge status / PR-comment callback** — surfaces run status (and plan diffs) on PRs;
  reuses the existing callback/webhook subsystem.

## 10. Testing strategy

- **Component 0:** golden parity test (local vs distributed env/workdir/mounts identical);
  integration test that `secret://env` in `step.env` reaches a distributed container; test
  that a resolution failure fails the task with a clear error.
- **Component 1:** unit tests for schema validation (engine/source compatibility, dangling
  mount refs, dup paths); per-engine conversion tests (docker named volume + tmpfs; k8s PVC +
  ephemeral claim + generic source); an integration test where two steps share a volume and
  pass a file (the `plan → apply` example) on each engine that supports it; for k8s inline
  ephemeral, assert the pod carries `ephemeral.volumeClaimTemplate` and an owner-reference (so
  the kubelet GCs it); for docker/podman ephemeral, a create/cleanup test including
  crash/orphan-reaper.
- **Component 2:** k8s engine test asserting `serviceAccountName`/annotations land on the pod
  spec; a documented manual verification recipe for IRSA/GKE-WI (cannot be hermetic).

## 11. Risks & open items

- **RWO/distributed limitation (§7)** is a real ergonomic boundary; mitigated by clear docs
  + the RWX recommendation, lifted later by node-affinity / object-storage handoff. The
  docker/podman analogue is sharper — named/ephemeral volumes are node-local and validated
  single-node-only; distributed docker/podman sharing requires a shared-network-FS `bind`.
- **Ephemeral-volume leak risk (docker/podman only)** if cleanup is not crash-safe → the
  orphan reaper is mandatory there. k8s avoids this entirely via inline ephemeral claims, whose
  PVC the kubelet GCs with the pod (§6.4).
- **Cache correctness for file outputs (§6.5)** — files are not content-addressed; documented,
  with the emit-a-hash escape hatch. The separate digest-pinned-cache fix (§9) is orthogonal.
- **Two unmarshalers** (`UnmarshalYAML` + `UnmarshalJSON`) must both be updated for any new
  step field — a known footgun in `definition.go`.
- **Backward compatibility:** existing bind `mounts:` keep working unchanged; `volumes:` /
  `volumeMounts:` / `serviceAccountName` are additive and optional.
